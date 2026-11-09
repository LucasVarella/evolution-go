package chatwoot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/evolution-foundation/evolution-go/pkg/hooks"
	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
)

const webhookBase = "http://evolution-go:8080"

var (
	citizen    = types.NewJID("5511988887777", types.DefaultUserServer)
	citizenLID = types.NewJID("123456789012345", types.HiddenUserServer)
	ownJID     = types.NewJID("5511900000000", types.DefaultUserServer)
)

type harness struct {
	t    *testing.T
	svc  *Service
	cw   *fakeChatwoot
	wa   *fakeWhatsApp
	repo *memRepository
	inst *instance_model.Instance
}

func newHarness(t *testing.T, opts ...func(*SetRequest)) *harness {
	t.Helper()
	h := &harness{
		t:    t,
		cw:   newFakeChatwoot(t),
		wa:   newFakeWhatsApp(),
		repo: newMemRepository(),
		inst: &instance_model.Instance{Id: "inst-1", Name: "prefeitura"},
	}
	h.svc = NewService(h.repo, h.wa, instanceMap{h.inst.Id: h.inst}, webhookBase, nil)

	req := SetRequest{
		Enabled:   ptr(true),
		URL:       ptr(h.cw.server.URL),
		AccountID: ptr(testAccountID),
		Token:     ptr(testToken),
	}
	for _, o := range opts {
		o(&req)
	}
	if _, err := h.svc.SetConfig(context.Background(), h.inst, req); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	return h
}

func ptr[T any](v T) *T { return &v }

// incoming delivers a WhatsApp message event and waits for it to be processed.
func (h *harness) incoming(info types.MessageInfo, msg *waE2E.Message) {
	h.t.Helper()
	h.svc.OnEvent(hooks.Event{Instance: h.inst, Raw: &events.Message{Info: info, Message: msg}})
	h.svc.Wait()
}

func textFrom(chat types.JID, id, pushName, text string) (types.MessageInfo, *waE2E.Message) {
	return types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, Sender: chat},
		ID:            id,
		PushName:      pushName,
		Timestamp:     time.Now(),
	}, &waE2E.Message{Conversation: proto.String(text)}
}

// webhook sends a signed Chatwoot webhook and waits for it to be processed.
func (h *harness) webhook(payload map[string]any) error {
	h.t.Helper()
	body, _ := json.Marshal(payload)
	err := h.svc.HandleWebhook(h.inst.Id, signedHeaders(testSecret, body, time.Now()), body)
	h.svc.Wait()
	return err
}

func agentMessage(id, conversationID int, content string) map[string]any {
	return map[string]any{
		"event":        "message_created",
		"id":           id,
		"content":      content,
		"message_type": "outgoing",
		"private":      false,
		"source_id":    nil,
		"sender":       map[string]any{"name": "Maria Souza", "available_name": "Maria", "type": "user"},
		"conversation": map[string]any{
			"id":   conversationID,
			"meta": map[string]any{"sender": map[string]any{"identifier": citizen.String(), "phone_number": "+5511988887777"}},
		},
	}
}

func onlyConversation(t *testing.T, cw *fakeChatwoot) fakeConversation {
	t.Helper()
	_, convs, _ := cw.snapshot()
	if len(convs) != 1 {
		t.Fatalf("want 1 conversation, got %d: %+v", len(convs), convs)
	}
	return convs[0]
}

// ---------------------------------------------------------------- configuration

func TestSetConfigCreatesAPIInboxWithWebhook(t *testing.T) {
	h := newHarness(t)

	if len(h.cw.inboxes) != 1 {
		t.Fatalf("want 1 inbox, got %d", len(h.cw.inboxes))
	}
	in := h.cw.inboxes[0]
	if in.Name != "prefeitura" {
		t.Errorf("inbox name = %q, want the instance name", in.Name)
	}
	if want := webhookBase + "/chatwoot/webhook/inst-1"; in.WebhookURL != want {
		t.Errorf("webhook = %q, want %q", in.WebhookURL, want)
	}
	cfg, _ := h.repo.GetConfig("inst-1")
	if cfg.InboxID != in.ID || cfg.WebhookSecret != testSecret {
		t.Errorf("config not linked to inbox: %+v", cfg)
	}
}

func TestSetConfigReusesInboxAndFixesWebhook(t *testing.T) {
	cw := newFakeChatwoot(t)
	cw.inboxes = []Inbox{{ID: 55, Name: "prefeitura", ChannelType: "Channel::Api", WebhookURL: "http://old", Secret: testSecret}}
	repo := newMemRepository()
	inst := &instance_model.Instance{Id: "inst-1", Name: "prefeitura"}
	svc := NewService(repo, newFakeWhatsApp(), instanceMap{}, webhookBase, nil)

	cfg, err := svc.SetConfig(context.Background(), inst, SetRequest{
		Enabled: ptr(true), URL: ptr(cw.server.URL + "/"), AccountID: ptr(testAccountID), Token: ptr(testToken),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cw.inboxes) != 1 || cfg.InboxID != 55 {
		t.Fatalf("expected inbox 55 to be reused, got config %+v and inboxes %+v", cfg, cw.inboxes)
	}
	if cw.inboxes[0].WebhookURL != webhookBase+"/chatwoot/webhook/inst-1" {
		t.Errorf("webhook not updated: %q", cw.inboxes[0].WebhookURL)
	}
	if cfg.URL != cw.server.URL {
		t.Errorf("trailing slash not removed: %q", cfg.URL)
	}
}

func TestSetConfigValidation(t *testing.T) {
	cw := newFakeChatwoot(t)
	inst := &instance_model.Instance{Id: "inst-1", Name: "prefeitura"}

	cases := map[string]struct {
		req     SetRequest
		mutate  func()
		wantErr error
	}{
		"missing token": {
			req:     SetRequest{Enabled: ptr(true), URL: ptr(cw.server.URL), AccountID: ptr(testAccountID)},
			wantErr: ErrInvalidConfig,
		},
		"bad url": {
			req:     SetRequest{Enabled: ptr(true), URL: ptr("not a url"), AccountID: ptr(testAccountID), Token: ptr(testToken)},
			wantErr: ErrInvalidConfig,
		},
		"non admin token (no secret)": {
			req:     SetRequest{Enabled: ptr(true), URL: ptr(cw.server.URL), AccountID: ptr(testAccountID), Token: ptr(testToken)},
			mutate:  func() { cw.hideSecret = true },
			wantErr: ErrInvalidConfig,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if c.mutate != nil {
				c.mutate()
			}
			svc := NewService(newMemRepository(), newFakeWhatsApp(), instanceMap{}, webhookBase, nil)
			_, err := svc.SetConfig(context.Background(), inst, c.req)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
		})
	}

	t.Run("wrong token is reported as Chatwoot error", func(t *testing.T) {
		cw.hideSecret = false
		svc := NewService(newMemRepository(), newFakeWhatsApp(), instanceMap{}, webhookBase, nil)
		_, err := svc.SetConfig(context.Background(), inst, SetRequest{
			Enabled: ptr(true), URL: ptr(cw.server.URL), AccountID: ptr(testAccountID), Token: ptr("wrong"),
		})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
			t.Fatalf("want 401 APIError, got %v", err)
		}
	})

	t.Run("disabled config needs no Chatwoot", func(t *testing.T) {
		svc := NewService(newMemRepository(), newFakeWhatsApp(), instanceMap{}, webhookBase, nil)
		cfg, err := svc.SetConfig(context.Background(), inst, SetRequest{Enabled: ptr(false)})
		if err != nil || cfg.Enabled {
			t.Fatalf("cfg=%+v err=%v", cfg, err)
		}
	})
}

func TestSetConfigPartialUpdateKeepsToken(t *testing.T) {
	h := newHarness(t)
	cfg, err := h.svc.SetConfig(context.Background(), h.inst, SetRequest{SignMessages: ptr(true)})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != testToken || !cfg.Enabled || !cfg.SignMessages {
		t.Fatalf("partial update lost settings: %+v", cfg)
	}
}

// ---------------------------------------------------------------- WhatsApp -> Chatwoot

func TestIncomingTextCreatesContactConversationAndMessage(t *testing.T) {
	h := newHarness(t)

	h.incoming(textFrom(citizen, "MSG1", "João", "Olá, *preciso* de ajuda"))

	contacts, convs, msgs := h.cw.snapshot()
	if len(contacts) != 1 {
		t.Fatalf("want 1 contact, got %+v", contacts)
	}
	c := contacts[0]
	if c.Name != "João" || c.PhoneNumber != "+5511988887777" || c.Identifier != citizen.String() {
		t.Errorf("unexpected contact %+v", c)
	}
	if len(convs) != 1 || convs[0].ContactID != c.ID || convs[0].InboxID != h.cw.inboxes[0].ID {
		t.Fatalf("unexpected conversations %+v", convs)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %+v", msgs)
	}
	m := msgs[0]
	if m.MessageType != "incoming" || m.SourceID != "WAID:MSG1" || m.Content != "Olá, **preciso** de ajuda" {
		t.Errorf("unexpected message %+v", m)
	}
	link, _ := h.repo.FindByWAID("inst-1", "MSG1")
	if link == nil || link.CWMessageID != m.ID || link.CWConversationID != convs[0].ID {
		t.Errorf("message link not saved: %+v", link)
	}
}

func TestSecondMessageReusesConversation(t *testing.T) {
	h := newHarness(t)
	h.incoming(textFrom(citizen, "MSG1", "João", "oi"))
	h.incoming(textFrom(citizen, "MSG2", "João", "tudo bem?"))

	contacts, convs, msgs := h.cw.snapshot()
	if len(contacts) != 1 || len(convs) != 1 || len(msgs) != 2 {
		t.Fatalf("contacts=%d conversations=%d messages=%d", len(contacts), len(convs), len(msgs))
	}
}

func TestDuplicateDeliveryIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.incoming(textFrom(citizen, "MSG1", "João", "oi"))
	h.incoming(textFrom(citizen, "MSG1", "João", "oi"))

	if _, _, msgs := h.cw.snapshot(); len(msgs) != 1 {
		t.Fatalf("duplicate created %d messages", len(msgs))
	}
}

func TestConcurrentMessagesFromNewContactCreateOneConversation(t *testing.T) {
	h := newHarness(t)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			info, msg := textFrom(citizen, fmt.Sprintf("BURST%d", i), "João", "msg")
			h.svc.OnEvent(hooks.Event{Instance: h.inst, Raw: &events.Message{Info: info, Message: msg}})
		}(i)
	}
	wg.Wait()
	h.svc.Wait()

	contacts, convs, msgs := h.cw.snapshot()
	if len(contacts) != 1 || len(convs) != 1 || len(msgs) != 10 {
		t.Fatalf("contacts=%d conversations=%d messages=%d", len(contacts), len(convs), len(msgs))
	}
}

func TestMessagesKeepOrder(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 20; i++ {
		info, msg := textFrom(citizen, fmt.Sprintf("ORD%02d", i), "João", fmt.Sprintf("%02d", i))
		h.svc.OnEvent(hooks.Event{Instance: h.inst, Raw: &events.Message{Info: info, Message: msg}})
	}
	h.svc.Wait()

	_, _, msgs := h.cw.snapshot()
	for i, m := range msgs {
		if m.Content != fmt.Sprintf("%02d", i) {
			t.Fatalf("message %d has content %q", i, m.Content)
		}
	}
}

func TestPendingConversationOption(t *testing.T) {
	h := newHarness(t, func(r *SetRequest) { r.ConversationPending = ptr(true) })
	h.incoming(textFrom(citizen, "MSG1", "João", "oi"))

	if c := onlyConversation(t, h.cw); c.Status != "pending" {
		t.Fatalf("status = %q, want pending", c.Status)
	}
}

func TestResolvedConversationStartsNewOne(t *testing.T) {
	h := newHarness(t)
	h.incoming(textFrom(citizen, "MSG1", "João", "oi"))
	first := onlyConversation(t, h.cw)

	h.cw.setConversationStatus(first.ID, "resolved")
	if err := h.webhook(map[string]any{
		"event": "conversation_status_changed", "id": first.ID, "status": "resolved",
		"meta": map[string]any{"sender": map[string]any{"identifier": citizen.String()}},
	}); err != nil {
		t.Fatal(err)
	}
	h.incoming(textFrom(citizen, "MSG2", "João", "voltei"))

	_, convs, msgs := h.cw.snapshot()
	if len(convs) != 2 {
		t.Fatalf("want a new conversation, got %+v", convs)
	}
	if msgs[1].ConversationID != convs[1].ID {
		t.Errorf("second message went to conversation %d", msgs[1].ConversationID)
	}
}

func TestResolvedConversationIsReusedWithReopenOption(t *testing.T) {
	h := newHarness(t, func(r *SetRequest) { r.ReopenConversation = ptr(true) })
	h.incoming(textFrom(citizen, "MSG1", "João", "oi"))
	first := onlyConversation(t, h.cw)
	h.cw.setConversationStatus(first.ID, "resolved")
	h.svc.forgetInstance(h.inst.Id) // as after a restart: no cached conversation

	h.incoming(textFrom(citizen, "MSG2", "João", "voltei"))
	onlyConversation(t, h.cw)
}

func TestDeletedConversationIsRecreated(t *testing.T) {
	h := newHarness(t)
	h.incoming(textFrom(citizen, "MSG1", "João", "oi"))
	first := onlyConversation(t, h.cw)
	h.cw.removeConversation(first.ID)

	h.incoming(textFrom(citizen, "MSG2", "João", "ainda aí?"))

	_, convs, msgs := h.cw.snapshot()
	if len(convs) != 1 || convs[0].ID == first.ID {
		t.Fatalf("conversation not recreated: %+v", convs)
	}
	if last := msgs[len(msgs)-1]; last.ConversationID != convs[0].ID || last.Content != "ainda aí?" {
		t.Fatalf("message not delivered after recreation: %+v", last)
	}
}

func TestExistingContactIsReusedAndNamed(t *testing.T) {
	h := newHarness(t)
	h.cw.contacts = []Contact{{ID: 9, Name: "5511988887777", PhoneNumber: "+5511988887777"}}

	h.incoming(textFrom(citizen, "MSG1", "João", "oi"))

	contacts, _, _ := h.cw.snapshot()
	if len(contacts) != 1 {
		t.Fatalf("contact duplicated: %+v", contacts)
	}
	if contacts[0].Name != "João" || contacts[0].Identifier != citizen.String() {
		t.Errorf("contact not completed: %+v", contacts[0])
	}
}

func TestContactNamedByAgentIsKept(t *testing.T) {
	h := newHarness(t)
	h.cw.contacts = []Contact{{ID: 9, Name: "João da Silva (IPTU)", PhoneNumber: "+5511988887777", Identifier: citizen.String()}}

	h.incoming(textFrom(citizen, "MSG1", "joaozinho", "oi"))

	if contacts, _, _ := h.cw.snapshot(); contacts[0].Name != "João da Silva (IPTU)" {
		t.Errorf("agent-given name overwritten: %q", contacts[0].Name)
	}
}

func TestSubstringSearchMatchIsNotReused(t *testing.T) {
	h := newHarness(t)
	// Chatwoot search matches substrings: this number contains the citizen's.
	h.cw.contacts = []Contact{{ID: 9, Name: "Outro", PhoneNumber: "+155119888877771", Identifier: "155119888877771@s.whatsapp.net"}}

	h.incoming(textFrom(citizen, "MSG1", "João", "oi"))

	if contacts, _, _ := h.cw.snapshot(); len(contacts) != 2 {
		t.Fatalf("partial match reused: %+v", contacts)
	}
}

func TestLIDOnlyContact(t *testing.T) {
	h := newHarness(t)
	h.incoming(textFrom(citizenLID, "MSG1", "Ana", "oi"))

	contacts, _, _ := h.cw.snapshot()
	if len(contacts) != 1 || contacts[0].Identifier != citizenLID.String() || contacts[0].PhoneNumber != "" {
		t.Fatalf("unexpected LID contact %+v", contacts)
	}
}

func TestMessageFromOwnPhoneIsOutgoing(t *testing.T) {
	h := newHarness(t)
	info, msg := textFrom(citizen, "MSG1", "", "respondi pelo celular")
	info.IsFromMe = true
	info.Sender = ownJID
	h.incoming(info, msg)

	contacts, _, msgs := h.cw.snapshot()
	if msgs[0].MessageType != "outgoing" {
		t.Errorf("type = %q, want outgoing", msgs[0].MessageType)
	}
	if contacts[0].Name != "+5511988887777" {
		t.Errorf("own push name must not name the contact: %q", contacts[0].Name)
	}
}

func TestIgnoredChats(t *testing.T) {
	h := newHarness(t)
	for _, chat := range []types.JID{
		types.NewJID("120363000000000000", types.GroupServer),
		types.StatusBroadcastJID,
		types.NewJID("120363000000000001", types.NewsletterServer),
	} {
		h.incoming(textFrom(chat, "X"+chat.User, "Grupo", "oi"))
	}
	if contacts, _, msgs := h.cw.snapshot(); len(contacts)+len(msgs) != 0 {
		t.Fatalf("ignored chats reached Chatwoot: contacts=%+v messages=%+v", contacts, msgs)
	}
}

func TestDisabledInstanceIsIgnored(t *testing.T) {
	h := newHarness(t)
	if _, err := h.svc.SetConfig(context.Background(), h.inst, SetRequest{Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	h.incoming(textFrom(citizen, "MSG1", "João", "oi"))
	if _, _, msgs := h.cw.snapshot(); len(msgs) != 0 {
		t.Fatal("disabled instance forwarded a message")
	}
}

func TestOtherInstanceIsIgnored(t *testing.T) {
	h := newHarness(t)
	other := &instance_model.Instance{Id: "videozap", Name: "nkt"}
	info, msg := textFrom(citizen, "MSG1", "João", "oi")
	h.svc.OnEvent(hooks.Event{Instance: other, Raw: &events.Message{Info: info, Message: msg}})
	h.svc.Wait()
	if _, _, msgs := h.cw.snapshot(); len(msgs) != 0 {
		t.Fatal("instance without configuration forwarded a message")
	}
}

func TestIncomingImageIsUploaded(t *testing.T) {
	h := newHarness(t)
	img := &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg"), Caption: proto.String("_foto_ do buraco")}
	h.wa.media[img] = []byte("JPEGDATA")
	info, _ := textFrom(citizen, "IMG1", "João", "")

	h.incoming(info, &waE2E.Message{ImageMessage: img})

	_, _, msgs := h.cw.snapshot()
	m := msgs[0]
	if string(m.Data) != "JPEGDATA" || m.ContentType != "image/jpeg" || m.Filename != "image.jpg" {
		t.Errorf("attachment = %q %q %q", m.Filename, m.ContentType, m.Data)
	}
	if m.Content != "*foto* do buraco" || m.SourceID != "WAID:IMG1" {
		t.Errorf("caption or source wrong: %+v", m)
	}
}

func TestIncomingDocumentKeepsFileName(t *testing.T) {
	h := newHarness(t)
	doc := &waE2E.DocumentMessage{Mimetype: proto.String("application/pdf"), FileName: proto.String("comprovante.pdf")}
	h.wa.media[doc] = []byte("%PDF")
	info, _ := textFrom(citizen, "DOC1", "João", "")

	h.incoming(info, &waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{
		Message: &waE2E.Message{DocumentMessage: doc},
	}})

	if _, _, msgs := h.cw.snapshot(); msgs[0].Filename != "comprovante.pdf" {
		t.Errorf("file name = %q", msgs[0].Filename)
	}
}

func TestVoiceNoteWithCodecParameter(t *testing.T) {
	h := newHarness(t)
	audio := &waE2E.AudioMessage{Mimetype: proto.String("audio/ogg; codecs=opus"), PTT: proto.Bool(true)}
	h.wa.media[audio] = []byte("OGG")
	info, _ := textFrom(citizen, "AUD1", "João", "")

	h.incoming(info, &waE2E.Message{AudioMessage: audio})

	m := func() fakeMessage { _, _, msgs := h.cw.snapshot(); return msgs[0] }()
	if m.ContentType != "audio/ogg" || !strings.HasSuffix(m.Filename, ".ogg") {
		t.Errorf("audio uploaded as %q %q", m.Filename, m.ContentType)
	}
}

func TestExpiredMediaStillCreatesMessage(t *testing.T) {
	h := newHarness(t)
	info, _ := textFrom(citizen, "IMG1", "João", "")
	h.incoming(info, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg")}})

	_, _, msgs := h.cw.snapshot()
	if len(msgs) != 1 || msgs[0].Data != nil || msgs[0].Content != textMediaUnavailable {
		t.Fatalf("unexpected message %+v", msgs)
	}
}

func TestQuotedMessageBecomesReply(t *testing.T) {
	h := newHarness(t)
	h.incoming(textFrom(citizen, "MSG1", "João", "primeira"))
	_, _, msgs := h.cw.snapshot()
	firstCW := msgs[0].ID

	info, _ := textFrom(citizen, "MSG2", "João", "")
	h.incoming(info, &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text:        proto.String("respondendo"),
		ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("MSG1")},
	}})

	_, _, msgs = h.cw.snapshot()
	if msgs[1].InReplyTo != firstCW {
		t.Errorf("in_reply_to = %d, want %d", msgs[1].InReplyTo, firstCW)
	}
}

func TestReactionIsPostedAsReply(t *testing.T) {
	h := newHarness(t)
	h.incoming(textFrom(citizen, "MSG1", "João", "obrigado"))
	_, _, msgs := h.cw.snapshot()

	info, _ := textFrom(citizen, "REACT1", "João", "")
	h.incoming(info, &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key: &waCommon.MessageKey{ID: proto.String("MSG1")}, Text: proto.String("👍"),
	}})
	// A removed reaction has empty text and is ignored.
	info2, _ := textFrom(citizen, "REACT2", "João", "")
	h.incoming(info2, &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key: &waCommon.MessageKey{ID: proto.String("MSG1")}, Text: proto.String(""),
	}})

	_, _, after := h.cw.snapshot()
	if len(after) != 2 || after[1].Content != "👍" || after[1].InReplyTo != msgs[0].ID {
		t.Fatalf("unexpected messages %+v", after)
	}
}

func TestRevokedMessageIsDeletedInChatwoot(t *testing.T) {
	h := newHarness(t)
	h.incoming(textFrom(citizen, "MSG1", "João", "ops"))
	_, _, msgs := h.cw.snapshot()

	info, _ := textFrom(citizen, "REV1", "João", "")
	h.incoming(info, &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("MSG1")},
	}})

	if len(h.cw.deleted) != 1 || h.cw.deleted[0] != msgs[0].ID {
		t.Fatalf("deleted = %v, want [%d]", h.cw.deleted, msgs[0].ID)
	}
}

func TestEditedMessage(t *testing.T) {
	h := newHarness(t)
	h.incoming(textFrom(citizen, "MSG1", "João", "rua 10"))
	info, _ := textFrom(citizen, "EDIT1", "João", "")
	h.incoming(info, &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key:           &waCommon.MessageKey{ID: proto.String("MSG1")},
		EditedMessage: &waE2E.Message{Conversation: proto.String("rua 12")},
	}})

	_, _, msgs := h.cw.snapshot()
	if len(msgs) != 2 || !strings.Contains(msgs[1].Content, "rua 12") || msgs[1].InReplyTo != msgs[0].ID {
		t.Fatalf("unexpected messages %+v", msgs)
	}
}

func TestLocationAndContactAreText(t *testing.T) {
	h := newHarness(t)
	info, _ := textFrom(citizen, "LOC1", "João", "")
	h.incoming(info, &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
		DegreesLatitude: proto.Float64(-23.5), DegreesLongitude: proto.Float64(-46.6), Name: proto.String("Praça"),
	}})
	info2, _ := textFrom(citizen, "VC1", "João", "")
	h.incoming(info2, &waE2E.Message{ContactMessage: &waE2E.ContactMessage{
		DisplayName: proto.String("Pedro"),
		Vcard:       proto.String("BEGIN:VCARD\nFN:Pedro\nTEL;type=CELL;waid=5511977776666:+55 11 97777-6666\nEND:VCARD"),
	}})

	_, _, msgs := h.cw.snapshot()
	if !strings.Contains(msgs[0].Content, "Praça") || !strings.Contains(msgs[0].Content, "q=-23.500000,-46.600000") {
		t.Errorf("location: %q", msgs[0].Content)
	}
	if !strings.Contains(msgs[1].Content, "Pedro") || !strings.Contains(msgs[1].Content, "+55 11 97777-6666") {
		t.Errorf("contact: %q", msgs[1].Content)
	}
}

func TestUnsupportedMessageLeavesNotice(t *testing.T) {
	h := newHarness(t)
	info, _ := textFrom(citizen, "POLL1", "João", "")
	h.incoming(info, &waE2E.Message{PollCreationMessage: &waE2E.PollCreationMessage{Name: proto.String("?")}})

	_, _, msgs := h.cw.snapshot()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Content, "pollCreationMessage") {
		t.Fatalf("unexpected messages %+v", msgs)
	}
}

func TestEmptyProtocolMessagesAreSkipped(t *testing.T) {
	h := newHarness(t)
	info, _ := textFrom(citizen, "KEY1", "João", "")
	h.incoming(info, &waE2E.Message{SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{}})
	info2, _ := textFrom(citizen, "PROTO1", "João", "")
	h.incoming(info2, &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_EPHEMERAL_SETTING.Enum()}})

	if contacts, _, msgs := h.cw.snapshot(); len(contacts)+len(msgs) != 0 {
		t.Fatalf("empty messages reached Chatwoot: %+v %+v", contacts, msgs)
	}
}

func TestMessageSentThroughAPIAppearsAsOutgoing(t *testing.T) {
	h := newHarness(t)
	h.svc.OnEvent(hooks.Event{Instance: h.inst, Raw: &hooks.SentMessage{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: citizen, Sender: ownJID, IsFromMe: true},
			ID:            "API1",
		},
		Message: &waE2E.Message{Conversation: proto.String("Lembrete: sua consulta é amanhã")},
	}})
	h.svc.Wait()

	_, _, msgs := h.cw.snapshot()
	if len(msgs) != 1 || msgs[0].MessageType != "outgoing" || msgs[0].SourceID != "WAID:API1" {
		t.Fatalf("unexpected messages %+v", msgs)
	}
}

// ---------------------------------------------------------------- Chatwoot -> WhatsApp

func TestAgentReplyIsSentToWhatsApp(t *testing.T) {
	h := newHarness(t)
	h.incoming(textFrom(citizen, "MSG1", "João", "oi"))
	conv := onlyConversation(t, h.cw)

	if err := h.webhook(agentMessage(900, conv.ID, "Olá! **Bem-vindo** ao atendimento.")); err != nil {
		t.Fatal(err)
	}

	sent := h.wa.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("want 1 WhatsApp message, got %+v", sent)
	}
	s := sent[0]
	if s.Kind != "text" || s.To != citizen || s.Text != "Olá! *Bem-vindo* ao atendimento." {
		t.Errorf("unexpected send %+v", s)
	}
	link, _ := h.repo.FindByWAID("inst-1", s.ID)
	if link == nil || link.CWMessageID != 900 || !link.FromMe {
		t.Errorf("link not saved: %+v", link)
	}
}

func TestAgentReplyEchoIsNotForwardedBack(t *testing.T) {
	h := newHarness(t)
	// The real send service emits a SentMessage hook for every message it sends.
	h.wa.onSend = func(id string, to types.JID) {
		h.svc.OnEvent(hooks.Event{Instance: h.inst, Raw: &hooks.SentMessage{
			Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: to, Sender: ownJID, IsFromMe: true}, ID: id},
			Message: &waE2E.Message{Conversation: proto.String("echo")},
		}})
	}
	h.incoming(textFrom(citizen, "MSG1", "João", "oi"))
	conv := onlyConversation(t, h.cw)

	if err := h.webhook(agentMessage(900, conv.ID, "resposta")); err != nil {
		t.Fatal(err)
	}

	if _, _, msgs := h.cw.snapshot(); len(msgs) != 1 {
		t.Fatalf("agent reply echoed back into Chatwoot: %+v", msgs)
	}
}

func TestSignedReply(t *testing.T) {
	h := newHarness(t, func(r *SetRequest) { r.SignMessages = ptr(true) })
	if err := h.webhook(agentMessage(900, 1, "Pode me passar o CPF?")); err != nil {
		t.Fatal(err)
	}
	if got := h.wa.sentMessages()[0].Text; got != "*Maria:*\nPode me passar o CPF?" {
		t.Errorf("signed text = %q", got)
	}
}

func TestCustomSignDelimiter(t *testing.T) {
	h := newHarness(t, func(r *SetRequest) { r.SignMessages = ptr(true); r.SignDelimiter = ptr(`\n\n`) })
	h.webhook(agentMessage(900, 1, "oi"))
	if got := h.wa.sentMessages()[0].Text; got != "*Maria:*\n\noi" {
		t.Errorf("signed text = %q", got)
	}
}

func TestIgnoredWebhooks(t *testing.T) {
	h := newHarness(t)

	fromWhatsApp := agentMessage(901, 1, "veio do WhatsApp")
	fromWhatsApp["source_id"] = "WAID:MSG1"
	private := agentMessage(902, 1, "nota interna")
	private["private"] = true
	incoming := agentMessage(903, 1, "mensagem do cidadão")
	incoming["message_type"] = "incoming"
	typing := map[string]any{"event": "conversation_typing_on"}
	statusUpdated := agentMessage(904, 1, "x")
	statusUpdated["event"] = "message_updated"
	deletedBeforeDelivery := agentMessage(905, 1, "Esta mensagem foi excluída")
	deletedBeforeDelivery["content_attributes"] = map[string]any{"deleted": true}

	for _, p := range []map[string]any{fromWhatsApp, private, incoming, typing, statusUpdated, deletedBeforeDelivery} {
		if err := h.webhook(p); err != nil {
			t.Fatalf("webhook %v: %v", p["event"], err)
		}
	}
	if sent := h.wa.sentMessages(); len(sent) != 0 {
		t.Fatalf("ignored webhooks sent messages: %+v", sent)
	}
}

func TestWebhookSignatureIsRequired(t *testing.T) {
	h := newHarness(t)
	body, _ := json.Marshal(agentMessage(900, 1, "oi"))

	cases := map[string]http.Header{
		"no headers":       {},
		"wrong secret":     signedHeaders("other-secret", body, time.Now()),
		"replayed request": signedHeaders(testSecret, body, time.Now().Add(-time.Hour)),
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			err := h.svc.HandleWebhook(h.inst.Id, header, body)
			if !errors.Is(err, ErrInvalidSignature) {
				t.Fatalf("err = %v, want ErrInvalidSignature", err)
			}
		})
	}
	t.Run("tampered body", func(t *testing.T) {
		header := signedHeaders(testSecret, body, time.Now())
		tampered := []byte(strings.Replace(string(body), "oi", "pix", 1))
		if err := h.svc.HandleWebhook(h.inst.Id, header, tampered); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("err = %v", err)
		}
	})
	h.svc.Wait()
	if sent := h.wa.sentMessages(); len(sent) != 0 {
		t.Fatalf("unsigned webhook sent messages: %+v", sent)
	}
}

func TestWebhookForUnknownInstance(t *testing.T) {
	h := newHarness(t)
	body := []byte(`{}`)
	if err := h.svc.HandleWebhook("unknown", signedHeaders(testSecret, body, time.Now()), body); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}

func TestReplyQuotesOriginalMessage(t *testing.T) {
	h := newHarness(t)
	h.incoming(textFrom(citizen, "MSG1", "João", "qual o horário?"))
	_, _, msgs := h.cw.snapshot()

	reply := agentMessage(900, msgs[0].ConversationID, "Das 8h às 17h")
	reply["content_attributes"] = map[string]any{"in_reply_to": msgs[0].ID}
	h.webhook(reply)

	q := h.wa.sentMessages()[0].Quoted
	if q == nil || q.MessageID != "MSG1" || q.Participant != citizen.String() {
		t.Fatalf("quoted = %+v", q)
	}
}

func TestReplyWithAttachments(t *testing.T) {
	h := newHarness(t)
	h.cw.files["guia.pdf"] = []byte("%PDF-guia")
	h.cw.files["foto.jpg"] = []byte("JPEG")

	p := agentMessage(900, 1, "Segue a guia")
	p["attachments"] = []map[string]any{
		{"file_type": "file", "data_url": "http://public.example:3000/rails/active_storage/blobs/redirect/sig1/guia.pdf"},
		{"file_type": "image", "data_url": "http://public.example:3000/rails/active_storage/blobs/redirect/sig2/foto.jpg"},
	}
	if err := h.webhook(p); err != nil {
		t.Fatal(err)
	}

	sent := h.wa.sentMessages()
	if len(sent) != 2 {
		t.Fatalf("want 2 media messages, got %+v", sent)
	}
	doc, img := sent[0].Media, sent[1].Media
	if doc.Type != "document" || doc.Filename != "guia.pdf" || string(doc.Data) != "%PDF-guia" || doc.Caption != "Segue a guia" {
		t.Errorf("document = %+v", doc)
	}
	if img.Type != "image" || string(img.Data) != "JPEG" || img.Caption != "" {
		t.Errorf("image = %+v", img)
	}
	if links, _ := h.repo.FindByCWID("inst-1", 900); len(links) != 2 {
		t.Errorf("want 2 links for the Chatwoot message, got %d", len(links))
	}
}

func TestAudioReplySendsTextSeparately(t *testing.T) {
	h := newHarness(t)
	h.cw.files["audio.ogg"] = []byte("OGG")
	p := agentMessage(900, 1, "Ouça o recado")
	p["attachments"] = []map[string]any{
		{"file_type": "audio", "data_url": "http://public.example:3000/rails/active_storage/blobs/redirect/sig/audio.ogg"},
	}
	h.webhook(p)

	sent := h.wa.sentMessages()
	if len(sent) != 2 || sent[0].Kind != "text" || sent[0].Text != "Ouça o recado" || sent[1].Media.Type != "audio" {
		t.Fatalf("unexpected sends %+v", sent)
	}
}

func TestAttachmentFromForeignHostIsRefused(t *testing.T) {
	h := newHarness(t)
	p := agentMessage(900, 1, "")
	p["attachments"] = []map[string]any{{"file_type": "file", "data_url": "http://169.254.169.254/latest/meta-data"}}
	h.webhook(p)

	if len(h.wa.sentMessages()) != 0 {
		t.Fatal("attachment from an unexpected URL was fetched and sent")
	}
	if len(h.cw.statuses) != 1 || h.cw.statuses[0].Status != "failed" {
		t.Fatalf("message not marked as failed: %+v", h.cw.statuses)
	}
}

func TestSendFailureMarksMessageAsFailed(t *testing.T) {
	h := newHarness(t)
	h.wa.failSend = errors.New("number 5511988887777 is not registered on WhatsApp")

	h.webhook(agentMessage(900, 321, "oi"))

	if len(h.cw.statuses) != 1 {
		t.Fatalf("status updates: %+v", h.cw.statuses)
	}
	st := h.cw.statuses[0]
	if st.ConversationID != 321 || st.MessageID != 900 || st.Status != "failed" || !strings.Contains(st.Error, "not registered") {
		t.Errorf("unexpected status update %+v", st)
	}
	if links, _ := h.repo.FindByCWID("inst-1", 900); len(links) != 0 {
		t.Error("failed message was linked")
	}
}

func TestDeletedReplyIsRevoked(t *testing.T) {
	h := newHarness(t)
	h.webhook(agentMessage(900, 1, "mensagem errada"))
	sentID := h.wa.sentMessages()[0].ID

	deleted := agentMessage(900, 1, "This message was deleted")
	deleted["event"] = "message_updated"
	deleted["content_attributes"] = map[string]any{"deleted": true}
	h.webhook(deleted)

	sent := h.wa.sentMessages()
	if len(sent) != 2 || sent[1].Kind != "revoke" || sent[1].ID != sentID {
		t.Fatalf("unexpected sends %+v", sent)
	}
}

func TestMarkReadOnReply(t *testing.T) {
	h := newHarness(t, func(r *SetRequest) { r.MarkRead = ptr(true) })
	h.incoming(textFrom(citizen, "MSG1", "João", "oi"))
	h.incoming(textFrom(citizen, "MSG2", "João", "alguém?"))
	conv := onlyConversation(t, h.cw)

	h.webhook(agentMessage(900, conv.ID, "Olá!"))

	sent := h.wa.sentMessages()
	if len(sent) != 2 || sent[1].Kind != "read" || sent[1].ID != "MSG2" {
		t.Fatalf("unexpected sends %+v", sent)
	}
}

func TestReplyToLIDContact(t *testing.T) {
	h := newHarness(t)
	p := agentMessage(900, 1, "oi")
	p["conversation"] = map[string]any{"id": 1, "meta": map[string]any{"sender": map[string]any{"identifier": citizenLID.String()}}}
	h.webhook(p)
	if sent := h.wa.sentMessages(); len(sent) != 1 || sent[0].To != citizenLID {
		t.Fatalf("unexpected sends %+v", sent)
	}
}

func TestReplyToContactWithoutIdentifierUsesPhone(t *testing.T) {
	h := newHarness(t)
	p := agentMessage(900, 1, "oi")
	p["conversation"] = map[string]any{"id": 1, "meta": map[string]any{"sender": map[string]any{"phone_number": "+55 (11) 98888-7777"}}}
	h.webhook(p)
	if sent := h.wa.sentMessages(); len(sent) != 1 || sent[0].To != citizen {
		t.Fatalf("unexpected sends %+v", sent)
	}
}

func TestEmptyReplyIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.webhook(agentMessage(900, 1, "   "))
	if len(h.wa.sentMessages()) != 0 {
		t.Fatal("empty reply was sent")
	}
}
