package chatwoot

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"

	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
)

// maxWebhookAge rejects replayed webhooks.
const maxWebhookAge = 5 * time.Minute

var ErrInvalidSignature = errors.New("invalid chatwoot webhook signature")

// verifySignature checks the X-Chatwoot-Signature header, an HMAC-SHA256 of
// "<timestamp>.<body>" keyed with the inbox secret.
func verifySignature(secret string, header http.Header, body []byte, now time.Time) error {
	if secret == "" {
		return fmt.Errorf("%w: inbox secret unknown", ErrInvalidSignature)
	}
	ts := header.Get("X-Chatwoot-Timestamp")
	sig := strings.TrimPrefix(header.Get("X-Chatwoot-Signature"), "sha256=")
	if ts == "" || sig == "" {
		return fmt.Errorf("%w: missing headers", ErrInvalidSignature)
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad timestamp", ErrInvalidSignature)
	}
	if age := now.Sub(time.Unix(unix, 0)); age > maxWebhookAge || age < -maxWebhookAge {
		return fmt.Errorf("%w: timestamp outside the allowed window", ErrInvalidSignature)
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return fmt.Errorf("%w: bad signature encoding", ErrInvalidSignature)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return ErrInvalidSignature
	}
	return nil
}

type webhookSender struct {
	Identifier    string `json:"identifier"`
	PhoneNumber   string `json:"phone_number"`
	Name          string `json:"name"`
	AvailableName string `json:"available_name"`
	Type          string `json:"type"`
}

type webhookMeta struct {
	Sender webhookSender `json:"sender"`
}

type webhookPayload struct {
	Event       string `json:"event"`
	ID          int    `json:"id"`
	Content     string `json:"content"`
	MessageType string `json:"message_type"`
	Private     bool   `json:"private"`
	SourceID    string `json:"source_id"`
	// Status and Meta are top-level fields of conversation events.
	Status            string      `json:"status"`
	Meta              webhookMeta `json:"meta"`
	ContentAttributes struct {
		Deleted   bool `json:"deleted"`
		InReplyTo int  `json:"in_reply_to"`
	} `json:"content_attributes"`
	Attachments []struct {
		DataURL  string `json:"data_url"`
		FileType string `json:"file_type"`
	} `json:"attachments"`
	Sender       webhookSender `json:"sender"`
	Conversation struct {
		ID   int         `json:"id"`
		Meta webhookMeta `json:"meta"`
	} `json:"conversation"`
}

// chatFromSender returns the WhatsApp chat of a Chatwoot contact.
func chatFromSender(s webhookSender) (types.JID, bool) {
	if s.Identifier != "" {
		if jid, err := types.ParseJID(s.Identifier); err == nil && supportedChat(jid) {
			return jid, true
		}
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s.PhoneNumber)
	if digits == "" {
		return types.EmptyJID, false
	}
	return types.NewJID(digits, types.DefaultUserServer), true
}

// HandleWebhook validates a Chatwoot webhook and schedules what it asks for.
func (s *Service) HandleWebhook(instanceID string, header http.Header, body []byte) error {
	cfg, _, err := s.active(instanceID)
	if err != nil {
		return err
	}
	if cfg == nil {
		return ErrNotConfigured
	}
	if err := verifySignature(cfg.WebhookSecret, header, body, s.now()); err != nil {
		return err
	}

	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("decode webhook: %w", err)
	}

	switch {
	case p.Event == "conversation_status_changed" && p.Status == "resolved":
		if chat, ok := chatFromSender(p.Meta.Sender); ok {
			s.forgetConversation(chatKey(instanceID, chat))
		}
		return nil
	// Chatwoot builds webhook payloads asynchronously: a message deleted right
	// after being sent arrives already deleted and must not be sent.
	case p.Event == "message_created" && p.MessageType == "outgoing" && !p.Private &&
		!p.ContentAttributes.Deleted && !strings.HasPrefix(p.SourceID, sourcePrefix):
	case p.Event == "message_updated" && p.ContentAttributes.Deleted:
	default:
		return nil
	}

	chat, ok := chatFromSender(p.Conversation.Meta.Sender)
	if !ok {
		return nil
	}
	inst, err := s.instances.GetInstanceByID(instanceID)
	if err != nil {
		return fmt.Errorf("load instance: %w", err)
	}

	s.queue.Do(chatKey(instanceID, chat), func() {
		ctx, cancel := context.WithTimeout(context.Background(), taskTimeout)
		defer cancel()
		var err error
		if p.Event == "message_updated" {
			err = s.revokeInWhatsApp(ctx, inst, chat, p.ID)
		} else {
			err = s.sendToWhatsApp(ctx, inst, chat, p)
		}
		if err != nil {
			s.logf(instanceID, "error", "chatwoot message %d to %s: %v", p.ID, chat, err)
		}
	})
	return nil
}

// sendToWhatsApp delivers an agent reply. On failure the Chatwoot message is
// marked as failed with the error, so the agent sees it was not delivered.
func (s *Service) sendToWhatsApp(ctx context.Context, inst *instance_model.Instance, chat types.JID, p webhookPayload) error {
	cfg, client, err := s.active(inst.Id)
	if err != nil || cfg == nil {
		return err
	}

	text := ChatwootToWhatsApp(p.Content)
	if name := firstNonEmpty(p.Sender.AvailableName, p.Sender.Name); cfg.SignMessages && name != "" && text != "" {
		text = "*" + name + ":*" + strings.ReplaceAll(cfg.SignDelimiter, `\n`, "\n") + text
	}

	var quoted *Quoted
	if p.ContentAttributes.InReplyTo != 0 {
		if originals, err := s.repo.FindByCWID(inst.Id, p.ContentAttributes.InReplyTo); err == nil && len(originals) > 0 {
			o := originals[0]
			quoted = &Quoted{MessageID: o.WAMessageID}
			if !o.FromMe {
				quoted.Participant = o.SenderJID
			}
		}
	}

	fail := func(err error) error {
		if statusErr := client.UpdateMessageStatus(ctx, p.Conversation.ID, p.ID, "failed", err.Error()); statusErr != nil {
			s.logf(inst.Id, "warn", "mark message %d as failed: %v", p.ID, statusErr)
		}
		return err
	}

	send := func(do func(id string) error) error {
		id := s.wa.NewMessageID(inst.Id)
		s.sentByChatwoot.Add(id)
		if err := do(id); err != nil {
			s.sentByChatwoot.Remove(id)
			return err
		}
		return s.repo.SaveMessage(&Message{
			InstanceID:       inst.Id,
			WAMessageID:      id,
			ChatJID:          chat.String(),
			FromMe:           true,
			CWMessageID:      p.ID,
			CWConversationID: p.Conversation.ID,
		})
	}

	if len(p.Attachments) == 0 {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		if err := send(func(id string) error { return s.wa.SendText(ctx, inst, chat, text, id, quoted) }); err != nil {
			return fail(err)
		}
	}

	captionUsed := false
	for _, a := range p.Attachments {
		file, err := client.Download(ctx, a.DataURL)
		if err != nil {
			return fail(fmt.Errorf("download attachment: %w", err))
		}
		media := OutgoingMedia{Type: mediaType(a.FileType, file.ContentType), Filename: file.Filename, Data: file.Data}
		if !captionUsed && text != "" {
			if media.Type == "audio" {
				// Audio has no caption; send the text first.
				if err := send(func(id string) error { return s.wa.SendText(ctx, inst, chat, text, id, quoted) }); err != nil {
					return fail(err)
				}
			} else {
				media.Caption = text
			}
			captionUsed = true
		}
		if err := send(func(id string) error { return s.wa.SendMedia(ctx, inst, chat, media, id, quoted) }); err != nil {
			return fail(err)
		}
		quoted = nil // quote only the first part of a multi-part reply
	}

	if cfg.MarkRead {
		s.markRead(ctx, inst, chat)
	}
	return nil
}

// mediaType maps a Chatwoot file type to a WhatsApp media type.
func mediaType(fileType, contentType string) string {
	switch fileType {
	case "image", "video", "audio":
		if fileType == "image" && contentType == "image/gif" {
			return "document" // WhatsApp only animates GIFs sent as MP4 video
		}
		return fileType
	default:
		return "document"
	}
}

func (s *Service) markRead(ctx context.Context, inst *instance_model.Instance, chat types.JID) {
	last, err := s.repo.LastIncoming(inst.Id, chat.String())
	if err != nil || last == nil {
		return
	}
	sender, err := types.ParseJID(last.SenderJID)
	if err != nil {
		sender = chat
	}
	if err := s.wa.MarkRead(ctx, inst, chat, sender, last.WAMessageID); err != nil {
		s.logf(inst.Id, "warn", "mark %s as read: %v", chat, err)
	}
}

// revokeInWhatsApp deletes for everyone the WhatsApp messages sent for a
// Chatwoot message an agent deleted. Messages received from the contact
// cannot be deleted for everyone and are left alone.
func (s *Service) revokeInWhatsApp(ctx context.Context, inst *instance_model.Instance, chat types.JID, cwMessageID int) error {
	sent, err := s.repo.FindByCWID(inst.Id, cwMessageID)
	if err != nil {
		return err
	}
	for _, m := range sent {
		if !m.FromMe {
			continue
		}
		if err := s.wa.Revoke(ctx, inst, chat, m.WAMessageID); err != nil {
			return err
		}
	}
	return nil
}
