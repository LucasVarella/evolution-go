package chatwoot

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
)

const (
	testToken     = "cw-admin-token"
	testAccountID = 7
	testSecret    = "inbox-secret"
)

// ---------------------------------------------------------------- Chatwoot

type fakeConversation struct {
	ID        int
	ContactID int
	InboxID   int
	Status    string
}

type fakeMessage struct {
	ID             int
	ConversationID int
	Content        string
	MessageType    string
	SourceID       string
	Private        bool
	InReplyTo      int
	Filename       string
	ContentType    string
	Data           []byte
}

type statusUpdate struct {
	ConversationID, MessageID int
	Status, Error             string
}

// fakeChatwoot imitates the parts of the Chatwoot API the integration uses.
type fakeChatwoot struct {
	t      *testing.T
	server *httptest.Server

	mu            sync.Mutex
	nextID        int
	inboxes       []Inbox
	contacts      []Contact
	conversations []*fakeConversation
	messages      []*fakeMessage
	statuses      []statusUpdate
	deleted       []int
	files         map[string][]byte
	hideSecret    bool
	failContacts  bool
}

func newFakeChatwoot(t *testing.T) *fakeChatwoot {
	f := &fakeChatwoot{t: t, nextID: 100, files: map[string][]byte{}}
	mux := http.NewServeMux()
	base := "/api/v1/accounts/{account}"
	mux.HandleFunc("GET "+base+"/inboxes", f.listInboxes)
	mux.HandleFunc("POST "+base+"/inboxes", f.createInbox)
	mux.HandleFunc("PATCH "+base+"/inboxes/{id}", f.updateInbox)
	mux.HandleFunc("GET "+base+"/contacts/search", f.searchContacts)
	mux.HandleFunc("POST "+base+"/contacts", f.createContact)
	mux.HandleFunc("PUT "+base+"/contacts/{id}", f.updateContact)
	mux.HandleFunc("GET "+base+"/contacts/{id}/conversations", f.contactConversations)
	mux.HandleFunc("POST "+base+"/conversations", f.createConversation)
	mux.HandleFunc("POST "+base+"/conversations/{id}/messages", f.createMessage)
	mux.HandleFunc("PATCH "+base+"/conversations/{id}/messages/{mid}", f.updateMessage)
	mux.HandleFunc("DELETE "+base+"/conversations/{id}/messages/{mid}", f.deleteMessage)
	mux.HandleFunc("GET /rails/active_storage/blobs/redirect/{sig}/{name}", func(w http.ResponseWriter, r *http.Request) {
		// Rails redirects to the disk service using the public host.
		http.Redirect(w, r, "http://public.example:3000/rails/active_storage/disk/"+r.PathValue("name"), http.StatusFound)
	})
	mux.HandleFunc("GET /rails/active_storage/disk/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		data, ok := f.files[r.PathValue("name")]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentTypeOf(r.PathValue("name")))
		w.Header().Set("Content-Disposition", `inline; filename="`+r.PathValue("name")+`"`)
		w.Write(data)
	})

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if r.Header.Get("api_access_token") != testToken {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if r.PathValue("account") == "" && !strings.HasPrefix(r.URL.Path, fmt.Sprintf("/api/v1/accounts/%d/", testAccountID)) {
				http.Error(w, `{"error":"wrong account"}`, http.StatusNotFound)
				return
			}
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func contentTypeOf(name string) string {
	switch {
	case strings.HasSuffix(name, ".pdf"):
		return "application/pdf"
	case strings.HasSuffix(name, ".jpg"):
		return "image/jpeg"
	case strings.HasSuffix(name, ".ogg"):
		return "audio/ogg"
	}
	return "application/octet-stream"
}

func (f *fakeChatwoot) id() int {
	f.nextID++
	return f.nextID
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) {
	json.NewDecoder(r.Body).Decode(v)
}

func pathInt(r *http.Request, name string) int {
	n, _ := strconv.Atoi(r.PathValue(name))
	return n
}

func (f *fakeChatwoot) inboxJSON(in Inbox) map[string]any {
	out := map[string]any{"id": in.ID, "name": in.Name, "channel_type": in.ChannelType, "webhook_url": in.WebhookURL}
	if !f.hideSecret {
		out["secret"] = in.Secret
	}
	return out
}

func (f *fakeChatwoot) listInboxes(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	payload := []map[string]any{}
	for _, in := range f.inboxes {
		payload = append(payload, f.inboxJSON(in))
	}
	writeJSON(w, 200, map[string]any{"payload": payload})
}

func (f *fakeChatwoot) createInbox(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string `json:"name"`
		Channel struct {
			Type       string `json:"type"`
			WebhookURL string `json:"webhook_url"`
		} `json:"channel"`
	}
	decode(r, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if body.Channel.Type != "api" {
		writeJSON(w, 422, map[string]any{"error": "unsupported channel"})
		return
	}
	in := Inbox{ID: f.id(), Name: body.Name, ChannelType: "Channel::Api", WebhookURL: body.Channel.WebhookURL, Secret: testSecret}
	f.inboxes = append(f.inboxes, in)
	writeJSON(w, 200, f.inboxJSON(in))
}

func (f *fakeChatwoot) updateInbox(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channel struct {
			WebhookURL string `json:"webhook_url"`
		} `json:"channel"`
	}
	decode(r, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.inboxes {
		if f.inboxes[i].ID == pathInt(r, "id") {
			f.inboxes[i].WebhookURL = body.Channel.WebhookURL
			writeJSON(w, 200, f.inboxJSON(f.inboxes[i]))
			return
		}
	}
	http.NotFound(w, r)
}

// searchContacts matches substrings, like Chatwoot.
func (f *fakeChatwoot) searchContacts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	f.mu.Lock()
	defer f.mu.Unlock()
	payload := []Contact{}
	for _, c := range f.contacts {
		if strings.Contains(c.Name, q) || strings.Contains(c.PhoneNumber, q) || strings.Contains(c.Identifier, q) {
			payload = append(payload, c)
		}
	}
	writeJSON(w, 200, map[string]any{"payload": payload})
}

func (f *fakeChatwoot) createContact(w http.ResponseWriter, r *http.Request) {
	var body struct {
		InboxID     int    `json:"inbox_id"`
		Name        string `json:"name"`
		PhoneNumber string `json:"phone_number"`
		Identifier  string `json:"identifier"`
	}
	decode(r, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failContacts {
		writeJSON(w, 422, map[string]any{"message": "Phone number has already been taken"})
		return
	}
	for _, c := range f.contacts {
		if (body.Identifier != "" && c.Identifier == body.Identifier) || (body.PhoneNumber != "" && c.PhoneNumber == body.PhoneNumber) {
			writeJSON(w, 422, map[string]any{"message": "already taken"})
			return
		}
	}
	c := Contact{ID: f.id(), Name: body.Name, PhoneNumber: body.PhoneNumber, Identifier: body.Identifier}
	f.contacts = append(f.contacts, c)
	writeJSON(w, 200, map[string]any{"payload": map[string]any{"contact": c, "contact_inbox": map[string]any{"source_id": "abc"}}})
}

func (f *fakeChatwoot) updateContact(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	decode(r, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.contacts {
		if f.contacts[i].ID == pathInt(r, "id") {
			if v, ok := body["name"]; ok {
				f.contacts[i].Name = v
			}
			if v, ok := body["identifier"]; ok {
				f.contacts[i].Identifier = v
			}
			writeJSON(w, 200, map[string]any{"payload": f.contacts[i]})
			return
		}
	}
	http.NotFound(w, r)
}

func (f *fakeChatwoot) contactConversations(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	payload := []map[string]any{}
	for _, c := range f.conversations {
		if c.ContactID == pathInt(r, "id") {
			payload = append(payload, map[string]any{"id": c.ID, "inbox_id": c.InboxID, "status": c.Status})
		}
	}
	writeJSON(w, 200, map[string]any{"payload": payload})
}

func (f *fakeChatwoot) createConversation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ContactID int    `json:"contact_id"`
		InboxID   int    `json:"inbox_id"`
		Status    string `json:"status"`
	}
	decode(r, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if body.Status == "" {
		body.Status = "open"
	}
	c := &fakeConversation{ID: f.id(), ContactID: body.ContactID, InboxID: body.InboxID, Status: body.Status}
	f.conversations = append(f.conversations, c)
	writeJSON(w, 200, map[string]any{"id": c.ID, "inbox_id": c.InboxID, "status": c.Status})
}

func (f *fakeChatwoot) createMessage(w http.ResponseWriter, r *http.Request) {
	convID := pathInt(r, "id")
	m := &fakeMessage{ConversationID: convID}

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		m.Content = r.FormValue("content")
		m.MessageType = r.FormValue("message_type")
		m.SourceID = r.FormValue("source_id")
		m.Private = r.FormValue("private") == "true"
		if attrs := r.FormValue("content_attributes"); attrs != "" {
			var a struct {
				InReplyTo int `json:"in_reply_to"`
			}
			json.Unmarshal([]byte(attrs), &a)
			m.InReplyTo = a.InReplyTo
		}
		file, header, err := r.FormFile("attachments[]")
		if err != nil {
			http.Error(w, "missing attachment", 400)
			return
		}
		m.Data, _ = io.ReadAll(file)
		m.Filename = header.Filename
		m.ContentType = header.Header.Get("Content-Type")
	} else {
		var body struct {
			Content           string `json:"content"`
			MessageType       string `json:"message_type"`
			SourceID          string `json:"source_id"`
			Private           bool   `json:"private"`
			ContentAttributes struct {
				InReplyTo int `json:"in_reply_to"`
			} `json:"content_attributes"`
		}
		decode(r, &body)
		m.Content, m.MessageType, m.SourceID, m.Private = body.Content, body.MessageType, body.SourceID, body.Private
		m.InReplyTo = body.ContentAttributes.InReplyTo
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	for _, c := range f.conversations {
		if c.ID == convID {
			found = true
		}
	}
	if !found {
		writeJSON(w, 404, map[string]any{"error": "Resource could not be found"})
		return
	}
	m.ID = f.id()
	f.messages = append(f.messages, m)
	writeJSON(w, 200, map[string]any{"id": m.ID, "conversation_id": convID, "message_type": 0, "source_id": m.SourceID})
}

func (f *fakeChatwoot) updateMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status        string `json:"status"`
		ExternalError string `json:"external_error"`
	}
	decode(r, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, statusUpdate{pathInt(r, "id"), pathInt(r, "mid"), body.Status, body.ExternalError})
	writeJSON(w, 200, map[string]any{"id": pathInt(r, "mid")})
}

func (f *fakeChatwoot) deleteMessage(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, pathInt(r, "mid"))
	writeJSON(w, 200, map[string]any{})
}

func (f *fakeChatwoot) snapshot() (contacts []Contact, conversations []fakeConversation, messages []fakeMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	contacts = append(contacts, f.contacts...)
	for _, c := range f.conversations {
		conversations = append(conversations, *c)
	}
	for _, m := range f.messages {
		messages = append(messages, *m)
	}
	return
}

func (f *fakeChatwoot) setConversationStatus(id int, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conversations {
		if c.ID == id {
			c.Status = status
		}
	}
}

func (f *fakeChatwoot) removeConversation(id int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.conversations {
		if c.ID == id {
			f.conversations = append(f.conversations[:i], f.conversations[i+1:]...)
			return
		}
	}
}

// ---------------------------------------------------------------- WhatsApp

type sentWA struct {
	Kind   string // text, media, revoke, read
	To     types.JID
	ID     string
	Text   string
	Media  OutgoingMedia
	Quoted *Quoted
}

type fakeWhatsApp struct {
	mu       sync.Mutex
	sent     []sentWA
	nextID   int
	failSend error
	media    map[whatsmeow.DownloadableMessage][]byte
	avatar   string
	// onSend runs after a successful send, before it returns, like the real
	// send service emitting its SentMessage hook.
	onSend func(id string, to types.JID)
}

func newFakeWhatsApp() *fakeWhatsApp {
	return &fakeWhatsApp{media: map[whatsmeow.DownloadableMessage][]byte{}}
}

func (w *fakeWhatsApp) NewMessageID(string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nextID++
	return fmt.Sprintf("3EB0SENT%04d", w.nextID)
}

func (w *fakeWhatsApp) record(s sentWA) error {
	w.mu.Lock()
	if w.failSend != nil {
		err := w.failSend
		w.mu.Unlock()
		return err
	}
	w.sent = append(w.sent, s)
	hook := w.onSend
	w.mu.Unlock()
	if hook != nil && (s.Kind == "text" || s.Kind == "media") {
		hook(s.ID, s.To)
	}
	return nil
}

func (w *fakeWhatsApp) SendText(_ context.Context, _ *instance_model.Instance, to types.JID, text, id string, quoted *Quoted) error {
	return w.record(sentWA{Kind: "text", To: to, ID: id, Text: text, Quoted: quoted})
}

func (w *fakeWhatsApp) SendMedia(_ context.Context, _ *instance_model.Instance, to types.JID, media OutgoingMedia, id string, quoted *Quoted) error {
	return w.record(sentWA{Kind: "media", To: to, ID: id, Media: media, Quoted: quoted})
}

func (w *fakeWhatsApp) Revoke(_ context.Context, _ *instance_model.Instance, chat types.JID, id string) error {
	return w.record(sentWA{Kind: "revoke", To: chat, ID: id})
}

func (w *fakeWhatsApp) MarkRead(_ context.Context, _ *instance_model.Instance, chat, _ types.JID, id string) error {
	return w.record(sentWA{Kind: "read", To: chat, ID: id})
}

func (w *fakeWhatsApp) Download(_ context.Context, _ *instance_model.Instance, msg whatsmeow.DownloadableMessage) ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if data, ok := w.media[msg]; ok {
		return data, nil
	}
	return nil, errors.New("media expired")
}

func (w *fakeWhatsApp) ProfilePictureURL(context.Context, *instance_model.Instance, types.JID) string {
	return w.avatar
}

func (w *fakeWhatsApp) sentMessages() []sentWA {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]sentWA(nil), w.sent...)
}

// ---------------------------------------------------------------- repository

type memRepository struct {
	mu       sync.Mutex
	configs  map[string]Config
	messages []Message
}

func newMemRepository() *memRepository {
	return &memRepository{configs: map[string]Config{}}
}

func (r *memRepository) Migrate() error { return nil }

func (r *memRepository) GetConfig(id string) (*Config, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg, ok := r.configs[id]
	if !ok {
		return nil, nil
	}
	return &cfg, nil
}

func (r *memRepository) SaveConfig(cfg *Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.configs[cfg.InstanceID] = *cfg
	return nil
}

func (r *memRepository) SaveMessage(m *Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m.ID = uint(len(r.messages) + 1)
	r.messages = append(r.messages, *m)
	return nil
}

func (r *memRepository) FindByWAID(inst, id string) (*Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.messages {
		if m.InstanceID == inst && m.WAMessageID == id {
			m := m
			return &m, nil
		}
	}
	return nil, nil
}

func (r *memRepository) FindByCWID(inst string, id int) ([]Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Message
	for _, m := range r.messages {
		if m.InstanceID == inst && m.CWMessageID == id {
			out = append(out, m)
		}
	}
	return out, nil
}

func (r *memRepository) LastIncoming(inst, chat string) (*Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.messages) - 1; i >= 0; i-- {
		m := r.messages[i]
		if m.InstanceID == inst && m.ChatJID == chat && !m.FromMe {
			return &m, nil
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------- instances

type instanceMap map[string]*instance_model.Instance

func (m instanceMap) GetInstanceByID(id string) (*instance_model.Instance, error) {
	if inst, ok := m[id]; ok {
		return inst, nil
	}
	return nil, errors.New("record not found")
}

// signedRequest builds the headers Chatwoot sends with a webhook.
func signedHeaders(secret string, body []byte, at time.Time) http.Header {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	h := http.Header{}
	h.Set("X-Chatwoot-Timestamp", ts)
	h.Set("X-Chatwoot-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return h
}
