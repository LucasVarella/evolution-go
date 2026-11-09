//go:build chatwoot_live

package chatwoot

// Contract test against a real Chatwoot. WhatsApp is faked.
//
// The test calls the real Chatwoot API. Webhooks are not delivered over the
// network: the test writes the list it needs to CW_EXCHANGE_DIR/requests.json,
// a script runs Chatwoot's own Webhooks::Trigger with the network call
// replaced by a capture (testdata/capture_webhooks.rb), and writes the signed
// requests to CW_EXCHANGE_DIR/webhooks.json. The test replays them through the
// integration's HTTP route. This checks the real payloads and signatures
// without exposing a port.
//
// The test creates an API inbox, a contact and messages and deletes them at the end.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/evolution-foundation/evolution-go/pkg/hooks"
	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
)

func liveEnv(t *testing.T, name string) string {
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s not set", name)
	}
	return v
}

type liveMessage struct {
	ID          int    `json:"id"`
	Content     string `json:"content"`
	MessageType int    `json:"message_type"`
	SourceID    string `json:"source_id"`
	Status      string `json:"status"`
	Attachments []struct {
		FileType string `json:"file_type"`
	} `json:"attachments"`
}

type webhookRequest struct {
	URL            string `json:"url"`
	Event          string `json:"event"`
	MessageID      int    `json:"message_id,omitempty"`
	ConversationID int    `json:"conversation_id,omitempty"`
	Label          string `json:"label"`
}

type capturedWebhook struct {
	Label   string            `json:"label"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

func TestLiveChatwoot(t *testing.T) {
	cwURL := liveEnv(t, "CW_URL")
	account, _ := strconv.Atoi(liveEnv(t, "CW_ACCOUNT"))
	token := liveEnv(t, "CW_TOKEN")
	exchange := liveEnv(t, "CW_EXCHANGE_DIR")
	ctx := context.Background()

	suffix := make([]byte, 3)
	rand.Read(suffix)
	tag := hex.EncodeToString(suffix)
	inst := &instance_model.Instance{Id: "live-" + tag, Name: "tmp-integracao-" + tag}
	// Area code 00 does not exist in Brazil, so no real person is involved.
	citizen := types.NewJID(fmt.Sprintf("5500%08d", int(suffix[0])<<16|int(suffix[1])<<8|int(suffix[2])), types.DefaultUserServer)

	wa := newFakeWhatsApp()
	repo := newMemRepository()
	svc := NewService(repo, wa, instanceMap{inst.Id: inst}, "http://evolution-go:8080", nil)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router, func(c *gin.Context) { c.Set("instance", inst); c.Next() }, svc)

	client, err := NewClient(cwURL, account, token)
	if err != nil {
		t.Fatal(err)
	}
	api := func(method, path string, body, out any) {
		t.Helper()
		if err := client.doJSON(ctx, method, client.accountPath("%s", path), body, out); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
	}
	messages := func(conv int) map[int]liveMessage {
		var out struct {
			Payload []liveMessage `json:"payload"`
		}
		api(http.MethodGet, fmt.Sprintf("/conversations/%d/messages", conv), nil, &out)
		byID := map[int]liveMessage{}
		for _, m := range out.Payload {
			byID[m.ID] = m
		}
		return byID
	}

	// ------------------------------------------------------------ configure
	cfg, err := svc.SetConfig(ctx, inst, SetRequest{Enabled: ptr(true), URL: ptr(cwURL), AccountID: ptr(account), Token: ptr(token)})
	if err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	var contactIDs []int
	t.Cleanup(func() {
		svc.Wait()
		for _, id := range contactIDs {
			client.doJSON(ctx, http.MethodDelete, client.accountPath("/contacts/%d", id), nil, nil)
		}
		if err := client.doJSON(ctx, http.MethodDelete, client.accountPath("/inboxes/%d", cfg.InboxID), nil, nil); err != nil {
			t.Logf("cleanup inbox: %v", err)
		}
	})
	if again, err := svc.SetConfig(ctx, inst, SetRequest{Enabled: ptr(true)}); err != nil || again.InboxID != cfg.InboxID {
		t.Fatalf("second SetConfig must reuse the inbox: %+v %v", again, err)
	}
	// Chatwoot cannot reach this test, and a failed webhook delivery marks the
	// message as failed. Without a URL, Chatwoot does not try to deliver.
	if _, err := client.UpdateInboxWebhook(ctx, cfg.InboxID, ""); err != nil {
		t.Fatal(err)
	}

	deliver := func(id string, msg *waE2E.Message, fromMe bool) *Message {
		t.Helper()
		info := types.MessageInfo{
			MessageSource: types.MessageSource{Chat: citizen, Sender: citizen, IsFromMe: fromMe},
			ID:            id, PushName: "Cidadão Teste", Timestamp: time.Now(),
		}
		svc.OnEvent(hooks.Event{Instance: inst, Raw: &events.Message{Info: info, Message: msg}})
		svc.Wait()
		link, _ := repo.FindByWAID(inst.Id, id)
		if link == nil {
			t.Fatalf("WhatsApp message %s did not reach Chatwoot", id)
		}
		return link
	}

	// ------------------------------------------------------------ WhatsApp -> Chatwoot
	in1 := deliver("LIVE1", &waE2E.Message{Conversation: proto.String("Olá, *preciso* de ajuda")}, false)
	conv := in1.CWConversationID

	contacts, err := client.SearchContacts(ctx, citizen.User)
	if err != nil || len(contacts) != 1 || contacts[0].Identifier != citizen.String() || contacts[0].Name != "Cidadão Teste" || contacts[0].PhoneNumber != "+"+citizen.User {
		t.Fatalf("contact in Chatwoot: %+v %v", contacts, err)
	}
	contactIDs = append(contactIDs, contacts[0].ID)

	img := &waE2E.ImageMessage{Mimetype: proto.String("image/png"), Caption: proto.String("foto")}
	wa.media[img] = tinyPNG
	in2 := deliver("LIVE2", &waE2E.Message{ImageMessage: img}, false)
	in3 := deliver("LIVE3", &waE2E.Message{Conversation: proto.String("respondi pelo celular")}, true)

	msgs := messages(conv)
	if m := msgs[in1.CWMessageID]; m.Content != "Olá, **preciso** de ajuda" || m.SourceID != "WAID:LIVE1" || m.MessageType != 0 {
		t.Errorf("incoming text: %+v", m)
	}
	if m := msgs[in2.CWMessageID]; len(m.Attachments) != 1 || m.Attachments[0].FileType != "image" || m.Content != "foto" {
		t.Errorf("incoming image: %+v", m)
	}
	if m := msgs[in3.CWMessageID]; m.MessageType != 1 || m.SourceID != "WAID:LIVE3" {
		t.Errorf("message typed on the phone: %+v", m)
	}

	// ------------------------------------------------------------ agent activity
	create := func(m NewMessage) int {
		t.Helper()
		created, err := client.CreateMessage(ctx, conv, m)
		if err != nil {
			t.Fatal(err)
		}
		return created.ID
	}
	reply := create(NewMessage{Content: "Olá! **Bem-vindo**", MessageType: "outgoing"})
	quoting := create(NewMessage{Content: "respondendo", MessageType: "outgoing", InReplyTo: in1.CWMessageID})
	pdf := []byte("%PDF-1.4 teste " + tag)
	withFile := create(NewMessage{Content: "Segue o documento", MessageType: "outgoing",
		Attachment: &Attachment{Filename: "guia-" + tag + ".pdf", ContentType: "application/pdf", Data: pdf}})
	note := create(NewMessage{Content: "nota interna", MessageType: "outgoing", Private: true})
	failing := create(NewMessage{Content: "esta vai falhar", MessageType: "outgoing"})

	// ------------------------------------------------------------ webhooks generated by Chatwoot
	round := 0
	capture := func(requests []webhookRequest) []capturedWebhook {
		t.Helper()
		round++
		for i := range requests {
			requests[i].URL = svc.WebhookURL(inst.Id)
		}
		data, _ := json.Marshal(requests)
		if err := os.WriteFile(filepath.Join(exchange, fmt.Sprintf("requests-%d.json", round)), data, 0o644); err != nil {
			t.Fatal(err)
		}
		var captured []capturedWebhook
		deadline := time.Now().Add(3 * time.Minute)
		for {
			if data, err := os.ReadFile(filepath.Join(exchange, fmt.Sprintf("webhooks-%d.json", round))); err == nil && json.Unmarshal(data, &captured) == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for webhooks-%d.json", round)
			}
			time.Sleep(500 * time.Millisecond)
		}
		if len(captured) != len(requests) {
			t.Fatalf("got %d captured webhooks, want %d", len(captured), len(requests))
		}
		return captured
	}

	replay := func(c capturedWebhook) int {
		t.Helper()
		if want := svc.WebhookURL(inst.Id); c.URL != want {
			t.Errorf("%s: Chatwoot would call %q, want %q", c.Label, c.URL, want)
		}
		req := httptest.NewRequest(http.MethodPost, "/chatwoot/webhook/"+inst.Id, bytes.NewReader([]byte(c.Body)))
		for k, v := range c.Headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		svc.Wait()
		return w.Code
	}

	check := func(captured []capturedWebhook) {
		t.Helper()
		for _, c := range captured {
			failedBefore := t.Failed()
			before := len(wa.sentMessages())
			if c.Label == "failing" {
				wa.mu.Lock()
				wa.failSend = fmt.Errorf("number is not registered on WhatsApp")
				wa.mu.Unlock()
			}
			if code := replay(c); code != http.StatusOK {
				t.Fatalf("%s: status %d (signature or payload rejected)", c.Label, code)
			}
			wa.mu.Lock()
			wa.failSend = nil
			wa.mu.Unlock()
			sent := wa.sentMessages()[before:]

			switch c.Label {
			case "incoming-from-whatsapp", "phone-echo", "private-note", "failing", "resolved", "deleted-before-delivery":
				if len(sent) != 0 {
					t.Errorf("%s: must not send to WhatsApp, sent %+v", c.Label, sent)
				}
			case "reply":
				if len(sent) != 1 || sent[0].Kind != "text" || sent[0].Text != "Olá! *Bem-vindo*" || sent[0].To != citizen {
					t.Errorf("reply: %+v", sent)
				}
			case "quoting":
				if len(sent) != 1 || sent[0].Quoted == nil || sent[0].Quoted.MessageID != "LIVE1" || sent[0].Quoted.Participant != citizen.String() {
					t.Errorf("quoting: %+v", sent)
				}
			case "attachment":
				if len(sent) != 1 || sent[0].Kind != "media" || sent[0].Media.Type != "document" ||
					!bytes.Equal(sent[0].Media.Data, pdf) || sent[0].Media.Caption != "Segue o documento" ||
					!strings.HasPrefix(sent[0].Media.Filename, "guia-") {
					t.Errorf("attachment (downloaded through the internal URL): %+v", sent)
				}
			case "deleted":
				if len(sent) != 1 || sent[0].Kind != "revoke" {
					t.Errorf("deleted: %+v", sent)
				}
			}
			if !t.Failed() || failedBefore {
				t.Logf("%-24s ok (%d sent to WhatsApp)", c.Label, len(sent))
			}
		}
	}

	// Round 1: messages as Chatwoot sends them right after creation.
	check(capture([]webhookRequest{
		{Label: "incoming-from-whatsapp", Event: "message_created", MessageID: in1.CWMessageID},
		{Label: "phone-echo", Event: "message_created", MessageID: in3.CWMessageID},
		{Label: "reply", Event: "message_created", MessageID: reply},
		{Label: "quoting", Event: "message_created", MessageID: quoting},
		{Label: "attachment", Event: "message_created", MessageID: withFile},
		{Label: "private-note", Event: "message_created", MessageID: note},
		{Label: "failing", Event: "message_created", MessageID: failing},
	}))

	// Round 2: the agent deletes the reply, a message is deleted before its
	// webhook is built, and the conversation is resolved.
	if err := client.DeleteMessage(ctx, conv, reply); err != nil {
		t.Fatal(err)
	}
	fast := create(NewMessage{Content: "apagada logo em seguida", MessageType: "outgoing"})
	if err := client.DeleteMessage(ctx, conv, fast); err != nil {
		t.Fatal(err)
	}
	api(http.MethodPost, fmt.Sprintf("/conversations/%d/toggle_status", conv), map[string]any{"status": "resolved"}, nil)
	captured := capture([]webhookRequest{
		{Label: "deleted", Event: "message_updated", MessageID: reply},
		{Label: "deleted-before-delivery", Event: "message_created", MessageID: fast},
		{Label: "resolved", Event: "conversation_status_changed", ConversationID: conv},
	})
	check(captured)

	final := messages(conv)
	if m := final[failing]; m.Status != "failed" {
		t.Errorf("failed send not shown in Chatwoot: status %q", m.Status)
	}
	if m := final[quoting]; m.Status == "failed" {
		t.Errorf("delivered reply shown as failed")
	}

	// After the conversation was resolved, the next message opens a new one.
	in4 := deliver("LIVE4", &waE2E.Message{Conversation: proto.String("voltei")}, false)
	if in4.CWConversationID == conv {
		t.Errorf("message after resolving went to the resolved conversation %d", conv)
	}

	// A wrong signature is rejected even with a real payload.
	bad := captured[0]
	bad.Headers["X-Chatwoot-Signature"] = "sha256=" + strings.Repeat("0", 64)
	if code := replay(bad); code != http.StatusUnauthorized {
		t.Errorf("tampered signature accepted: status %d", code)
	}
}

// tinyPNG is a 1x1 transparent PNG.
var tinyPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}
