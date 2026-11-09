package chatwoot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/evolution-foundation/evolution-go/pkg/hooks"
	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
	logger_wrapper "github.com/evolution-foundation/evolution-go/pkg/logger"
)

const (
	// sourcePrefix marks Chatwoot messages that came from WhatsApp, so their
	// webhook echo is not sent back to WhatsApp.
	sourcePrefix = "WAID:"
	// conversationTTL bounds how long a conversation id is reused without
	// asking Chatwoot, in case a status change webhook was missed.
	conversationTTL = 30 * time.Minute
	// sentTTL is how long the ids of messages sent from Chatwoot are remembered.
	sentTTL     = 10 * time.Minute
	taskTimeout = 5 * time.Minute
)

var (
	ErrNotConfigured = errors.New("chatwoot integration is not enabled for this instance")
	ErrInvalidConfig = errors.New("invalid chatwoot configuration")
)

// InstanceFinder looks up instances by id.
type InstanceFinder interface {
	GetInstanceByID(instanceId string) (*instance_model.Instance, error)
}

// Service connects WhatsApp instances to Chatwoot API inboxes.
type Service struct {
	repo           Repository
	wa             WhatsApp
	instances      InstanceFinder
	webhookBaseURL string
	logs           *logger_wrapper.LoggerManager

	queue          *serialQueue
	sentByChatwoot *expiringSet
	now            func() time.Time

	mu            sync.Mutex
	configs       map[string]*Config
	clients       map[string]*Client
	conversations map[string]cachedConversation
}

type cachedConversation struct {
	id      int
	expires time.Time
}

// NewService creates the integration. webhookBaseURL is the address Chatwoot
// uses to reach this server, for example http://evolution-go:8080.
func NewService(repo Repository, wa WhatsApp, instances InstanceFinder, webhookBaseURL string, logs *logger_wrapper.LoggerManager) *Service {
	return &Service{
		repo:           repo,
		wa:             wa,
		instances:      instances,
		webhookBaseURL: strings.TrimRight(webhookBaseURL, "/"),
		logs:           logs,
		queue:          newSerialQueue(),
		sentByChatwoot: newExpiringSet(sentTTL),
		now:            time.Now,
		configs:        make(map[string]*Config),
		clients:        make(map[string]*Client),
		conversations:  make(map[string]cachedConversation),
	}
}

// WebhookURL is the address Chatwoot calls for the instance's inbox.
func (s *Service) WebhookURL(instanceID string) string {
	return s.webhookBaseURL + "/chatwoot/webhook/" + instanceID
}

// Wait blocks until all queued work has finished.
func (s *Service) Wait() { s.queue.Wait() }

func (s *Service) logf(instanceID, level, format string, args ...any) {
	if s.logs == nil {
		return
	}
	l := s.logs.GetLogger(instanceID)
	msg := "[chatwoot] " + fmt.Sprintf(format, args...)
	switch level {
	case "error":
		l.LogError("%s", msg)
	case "warn":
		l.LogWarn("%s", msg)
	default:
		l.LogInfo("%s", msg)
	}
}

// SetRequest updates the configuration. Omitted fields keep their current value.
type SetRequest struct {
	Enabled             *bool   `json:"enabled"`
	URL                 *string `json:"url"`
	AccountID           *int    `json:"accountId"`
	Token               *string `json:"token"`
	InboxName           *string `json:"inboxName"`
	SignMessages        *bool   `json:"signMessages"`
	SignDelimiter       *string `json:"signDelimiter"`
	ReopenConversation  *bool   `json:"reopenConversation"`
	ConversationPending *bool   `json:"conversationPending"`
	MarkRead            *bool   `json:"markRead"`
}

// SetConfig saves the configuration and, when enabled, finds or creates the
// API inbox in Chatwoot and points its webhook at this server.
func (s *Service) SetConfig(ctx context.Context, inst *instance_model.Instance, req SetRequest) (*Config, error) {
	current, err := s.repo.GetConfig(inst.Id)
	if err != nil {
		return nil, err
	}
	cfg := Config{InstanceID: inst.Id, SignDelimiter: "\n"}
	if current != nil {
		cfg = *current
	}

	setBool(&cfg.Enabled, req.Enabled)
	setString(&cfg.URL, req.URL)
	if req.AccountID != nil {
		cfg.AccountID = *req.AccountID
	}
	setString(&cfg.Token, req.Token)
	setString(&cfg.InboxName, req.InboxName)
	setBool(&cfg.SignMessages, req.SignMessages)
	setString(&cfg.SignDelimiter, req.SignDelimiter)
	setBool(&cfg.ReopenConversation, req.ReopenConversation)
	setBool(&cfg.ConversationPending, req.ConversationPending)
	setBool(&cfg.MarkRead, req.MarkRead)
	cfg.URL = strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	if cfg.InboxName == "" {
		cfg.InboxName = inst.Name
	}

	if cfg.Enabled {
		if cfg.URL == "" || cfg.AccountID <= 0 || cfg.Token == "" {
			return nil, fmt.Errorf("%w: url, accountId and token are required", ErrInvalidConfig)
		}
		if s.webhookBaseURL == "" {
			return nil, fmt.Errorf("%w: the server has no webhook base URL (CHATWOOT_WEBHOOK_BASE_URL)", ErrInvalidConfig)
		}
		client, err := NewClient(cfg.URL, cfg.AccountID, cfg.Token)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
		}
		inbox, err := s.ensureInbox(ctx, client, cfg.InboxName, s.WebhookURL(inst.Id))
		if err != nil {
			return nil, err
		}
		if inbox.Secret == "" {
			return nil, fmt.Errorf("%w: Chatwoot did not return the inbox secret; the token must belong to an account administrator", ErrInvalidConfig)
		}
		cfg.InboxID = inbox.ID
		cfg.WebhookSecret = inbox.Secret
	}

	if err := s.repo.SaveConfig(&cfg); err != nil {
		return nil, err
	}
	s.forgetInstance(inst.Id)
	s.logf(inst.Id, "info", "configuration saved (enabled=%v, inbox=%d)", cfg.Enabled, cfg.InboxID)
	return &cfg, nil
}

func (s *Service) ensureInbox(ctx context.Context, client *Client, name, webhookURL string) (*Inbox, error) {
	inboxes, err := client.ListInboxes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Chatwoot inboxes: %w", err)
	}
	for i := range inboxes {
		inbox := inboxes[i]
		if inbox.Name != name {
			continue
		}
		if inbox.ChannelType != "Channel::Api" {
			return nil, fmt.Errorf("%w: inbox %q exists and is not an API inbox", ErrInvalidConfig, name)
		}
		if inbox.WebhookURL != webhookURL {
			updated, err := client.UpdateInboxWebhook(ctx, inbox.ID, webhookURL)
			if err != nil {
				return nil, fmt.Errorf("update Chatwoot inbox webhook: %w", err)
			}
			if updated.Secret == "" {
				updated.Secret = inbox.Secret
			}
			return updated, nil
		}
		return &inbox, nil
	}
	inbox, err := client.CreateInbox(ctx, name, webhookURL)
	if err != nil {
		return nil, fmt.Errorf("create Chatwoot inbox: %w", err)
	}
	return inbox, nil
}

// GetConfig returns the saved configuration, or nil when there is none.
func (s *Service) GetConfig(instanceID string) (*Config, error) {
	return s.repo.GetConfig(instanceID)
}

func setBool(dst *bool, v *bool) {
	if v != nil {
		*dst = *v
	}
}

func setString(dst *string, v *string) {
	if v != nil {
		*dst = *v
	}
}

// active returns the configuration and client of an enabled instance, or nil.
func (s *Service) active(instanceID string) (*Config, *Client, error) {
	s.mu.Lock()
	cfg, cached := s.configs[instanceID]
	client := s.clients[instanceID]
	s.mu.Unlock()

	if !cached {
		loaded, err := s.repo.GetConfig(instanceID)
		if err != nil {
			return nil, nil, err
		}
		cfg = loaded
		if cfg != nil && cfg.Enabled {
			client, err = NewClient(cfg.URL, cfg.AccountID, cfg.Token)
			if err != nil {
				return nil, nil, err
			}
		}
		s.mu.Lock()
		s.configs[instanceID] = cfg
		s.clients[instanceID] = client
		s.mu.Unlock()
	}
	if cfg == nil || !cfg.Enabled || client == nil {
		return nil, nil, nil
	}
	return cfg, client, nil
}

func (s *Service) forgetInstance(instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.configs, instanceID)
	delete(s.clients, instanceID)
	prefix := instanceID + "|"
	for k := range s.conversations {
		if strings.HasPrefix(k, prefix) {
			delete(s.conversations, k)
		}
	}
}

func chatKey(instanceID string, chat types.JID) string {
	return instanceID + "|" + chat.ToNonAD().String()
}

// supportedChat reports whether messages of the chat are forwarded. Only
// one-to-one chats are: groups, broadcasts, status and channels are ignored.
func supportedChat(chat types.JID) bool {
	return chat.Server == types.DefaultUserServer || chat.Server == types.HiddenUserServer
}

// OnEvent receives WhatsApp events from the hooks package. It only schedules
// work, so it never slows down WhatsApp event processing.
func (s *Service) OnEvent(e hooks.Event) {
	if e.Instance == nil {
		return
	}
	var info types.MessageInfo
	var msg *waE2E.Message
	switch raw := e.Raw.(type) {
	case *events.Message:
		info, msg = raw.Info, raw.Message
	case *hooks.SentMessage:
		if s.sentByChatwoot.Has(raw.Info.ID) {
			return // sent by an agent; Chatwoot already has it
		}
		info, msg = raw.Info, raw.Message
	default:
		return
	}
	if !supportedChat(info.Chat) {
		return
	}
	cfg, _, err := s.active(e.Instance.Id)
	if err != nil {
		s.logf(e.Instance.Id, "error", "load configuration: %v", err)
		return
	}
	if cfg == nil {
		return
	}
	inst := e.Instance
	s.queue.Do(chatKey(inst.Id, info.Chat), func() {
		ctx, cancel := context.WithTimeout(context.Background(), taskTimeout)
		defer cancel()
		if err := s.forwardToChatwoot(ctx, inst, info, msg); err != nil {
			s.logf(inst.Id, "error", "message %s from %s: %v", info.ID, info.Chat, err)
		}
	})
}

// forwardToChatwoot posts a WhatsApp message in the contact's conversation.
func (s *Service) forwardToChatwoot(ctx context.Context, inst *instance_model.Instance, info types.MessageInfo, msg *waE2E.Message) error {
	cfg, client, err := s.active(inst.Id)
	if err != nil || cfg == nil {
		return err
	}

	// WhatsApp can deliver the same message more than once.
	if known, err := s.repo.FindByWAID(inst.Id, info.ID); err != nil {
		return err
	} else if known != nil {
		return nil
	}

	p := parseMessage(msg)
	switch {
	case p.Skip:
		return nil
	case p.RevokedID != "":
		return s.deleteInChatwoot(ctx, inst.Id, client, p.RevokedID)
	}

	m := NewMessage{
		Content:     WhatsAppToChatwoot(p.Text),
		MessageType: "incoming",
		SourceID:    sourcePrefix + info.ID,
	}
	if info.IsFromMe {
		m.MessageType = "outgoing"
	}
	if p.EditedID != "" {
		m.Content = fmt.Sprintf(textEdited, m.Content)
	}
	if replyTo := firstNonEmpty(p.ReactionTo, p.EditedID, p.QuotedID); replyTo != "" {
		if original, err := s.repo.FindByWAID(inst.Id, replyTo); err == nil && original != nil {
			m.InReplyTo = original.CWMessageID
		}
	}
	if p.Media != nil {
		data, err := s.wa.Download(ctx, inst, p.Media)
		if err != nil {
			s.logf(inst.Id, "warn", "download media of %s: %v", info.ID, err)
			m.Content = strings.TrimSpace(m.Content + "\n" + textMediaUnavailable)
		} else {
			m.Attachment = &Attachment{Filename: p.Filename, ContentType: p.Mimetype, Data: data}
		}
	}

	convID, err := s.conversationFor(ctx, cfg, client, inst, info)
	if err != nil {
		return err
	}
	created, err := client.CreateMessage(ctx, convID, m)
	if IsNotFound(err) {
		// The conversation was deleted in Chatwoot; start a new one.
		s.forgetConversation(chatKey(inst.Id, info.Chat))
		if convID, err = s.conversationFor(ctx, cfg, client, inst, info); err != nil {
			return err
		}
		created, err = client.CreateMessage(ctx, convID, m)
	}
	if err != nil {
		return fmt.Errorf("create Chatwoot message: %w", err)
	}

	return s.repo.SaveMessage(&Message{
		InstanceID:       inst.Id,
		WAMessageID:      info.ID,
		ChatJID:          info.Chat.ToNonAD().String(),
		SenderJID:        info.Sender.ToNonAD().String(),
		FromMe:           info.IsFromMe,
		CWMessageID:      created.ID,
		CWConversationID: convID,
	})
}

func (s *Service) deleteInChatwoot(ctx context.Context, instanceID string, client *Client, waMessageID string) error {
	original, err := s.repo.FindByWAID(instanceID, waMessageID)
	if err != nil || original == nil {
		return err
	}
	err = client.DeleteMessage(ctx, original.CWConversationID, original.CWMessageID)
	if IsNotFound(err) {
		return nil
	}
	return err
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (s *Service) forgetConversation(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conversations, key)
}

// conversationFor returns the contact's current conversation in the inbox,
// creating the contact and the conversation when needed.
func (s *Service) conversationFor(ctx context.Context, cfg *Config, client *Client, inst *instance_model.Instance, info types.MessageInfo) (int, error) {
	key := chatKey(inst.Id, info.Chat)
	s.mu.Lock()
	cached, ok := s.conversations[key]
	s.mu.Unlock()
	if ok && s.now().Before(cached.expires) {
		return cached.id, nil
	}

	contact, err := s.contactFor(ctx, cfg, client, inst, info)
	if err != nil {
		return 0, err
	}

	conversations, err := client.ContactConversations(ctx, contact.ID)
	if err != nil {
		return 0, fmt.Errorf("list contact conversations: %w", err)
	}
	var current, latest *Conversation
	for i := range conversations {
		c := &conversations[i]
		if c.InboxID != cfg.InboxID {
			continue
		}
		if latest == nil || c.ID > latest.ID {
			latest = c
		}
		if c.Status != "resolved" && (current == nil || c.ID > current.ID) {
			current = c
		}
	}
	if current == nil && cfg.ReopenConversation {
		current = latest // Chatwoot reopens it when the message arrives
	}
	if current == nil {
		status := ""
		if cfg.ConversationPending {
			status = "pending"
		}
		current, err = client.CreateConversation(ctx, contact.ID, cfg.InboxID, status)
		if err != nil {
			return 0, fmt.Errorf("create conversation: %w", err)
		}
	}

	s.mu.Lock()
	s.conversations[key] = cachedConversation{id: current.ID, expires: s.now().Add(conversationTTL)}
	s.mu.Unlock()
	return current.ID, nil
}

// contactFor finds the Chatwoot contact of a chat, or creates it.
func (s *Service) contactFor(ctx context.Context, cfg *Config, client *Client, inst *instance_model.Instance, info types.MessageInfo) (*Contact, error) {
	chat := info.Chat.ToNonAD()
	identifier := chat.String()
	phone := ""
	if chat.Server == types.DefaultUserServer {
		phone = "+" + chat.User
	}
	name := phone
	if name == "" {
		name = chat.User
	}
	if !info.IsFromMe && strings.TrimSpace(info.PushName) != "" {
		name = strings.TrimSpace(info.PushName)
	}

	contact, err := s.findContact(ctx, client, chat.User, identifier, phone)
	if err != nil {
		return nil, err
	}
	if contact != nil {
		updates := map[string]any{}
		if contact.Identifier == "" {
			updates["identifier"] = identifier
		}
		if name != phone && name != chat.User && isPlaceholderName(contact, chat.User) {
			updates["name"] = name
		}
		if len(updates) > 0 {
			if err := client.UpdateContact(ctx, contact.ID, updates); err != nil {
				s.logf(inst.Id, "warn", "update contact %d: %v", contact.ID, err)
			}
		}
		return contact, nil
	}

	avatar := s.wa.ProfilePictureURL(ctx, inst, chat)
	contact, err = client.CreateContact(ctx, cfg.InboxID, name, phone, identifier, avatar)
	if err != nil {
		// Another contact may already own the phone number or identifier.
		if existing, findErr := s.findContact(ctx, client, chat.User, identifier, phone); findErr == nil && existing != nil {
			return existing, nil
		}
		return nil, fmt.Errorf("create contact: %w", err)
	}
	return contact, nil
}

func (s *Service) findContact(ctx context.Context, client *Client, query, identifier, phone string) (*Contact, error) {
	contacts, err := client.SearchContacts(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("search contacts: %w", err)
	}
	// The search matches substrings, so only exact matches are accepted;
	// the identifier wins over the phone number.
	for i := range contacts {
		if contacts[i].Identifier == identifier {
			return &contacts[i], nil
		}
	}
	if phone != "" {
		for i := range contacts {
			if contacts[i].PhoneNumber == phone {
				return &contacts[i], nil
			}
		}
	}
	return nil, nil
}

func isPlaceholderName(c *Contact, user string) bool {
	n := strings.TrimPrefix(strings.TrimSpace(c.Name), "+")
	return n == "" || n == user || n == strings.TrimPrefix(c.PhoneNumber, "+")
}
