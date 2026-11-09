package chatwoot

import (
	"context"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
	send_service "github.com/evolution-foundation/evolution-go/pkg/sendMessage/service"
)

// Quoted identifies the WhatsApp message a reply refers to.
type Quoted struct {
	MessageID   string
	Participant string
}

// OutgoingMedia is a file to send to WhatsApp.
type OutgoingMedia struct {
	Type     string // image, video, audio or document
	Filename string
	Caption  string
	Data     []byte
}

// WhatsApp is what the integration needs from a connected instance. It is an
// interface so the integration can be tested without a WhatsApp connection.
type WhatsApp interface {
	NewMessageID(instanceID string) string
	SendText(ctx context.Context, inst *instance_model.Instance, to types.JID, text, id string, quoted *Quoted) error
	SendMedia(ctx context.Context, inst *instance_model.Instance, to types.JID, media OutgoingMedia, id string, quoted *Quoted) error
	Revoke(ctx context.Context, inst *instance_model.Instance, chat types.JID, messageID string) error
	MarkRead(ctx context.Context, inst *instance_model.Instance, chat, sender types.JID, messageID string) error
	Download(ctx context.Context, inst *instance_model.Instance, msg whatsmeow.DownloadableMessage) ([]byte, error)
	ProfilePictureURL(ctx context.Context, inst *instance_model.Instance, jid types.JID) string
}

type whatsApp struct {
	send    send_service.SendService
	clients map[string]*whatsmeow.Client
}

// NewWhatsApp adapts the server's send service and WhatsApp clients.
func NewWhatsApp(send send_service.SendService, clients map[string]*whatsmeow.Client) WhatsApp {
	return &whatsApp{send: send, clients: clients}
}

func (w *whatsApp) client(instanceID string) (*whatsmeow.Client, error) {
	c := w.clients[instanceID]
	if c == nil {
		return nil, fmt.Errorf("instance %s is not connected", instanceID)
	}
	return c, nil
}

func (w *whatsApp) NewMessageID(instanceID string) string {
	if c := w.clients[instanceID]; c != nil {
		return c.GenerateMessageID()
	}
	return whatsmeow.GenerateMessageID()
}

// recipient formats a chat JID the way the send service expects it: digits for
// phone numbers, the full JID for LIDs.
func recipient(chat types.JID) string {
	if chat.Server == types.DefaultUserServer {
		return chat.User
	}
	return chat.String()
}

func quotedStruct(q *Quoted) send_service.QuotedStruct {
	if q == nil {
		return send_service.QuotedStruct{}
	}
	return send_service.QuotedStruct{MessageID: q.MessageID, Participant: q.Participant}
}

func (w *whatsApp) SendText(_ context.Context, inst *instance_model.Instance, to types.JID, text, id string, quoted *Quoted) error {
	_, err := w.send.SendText(&send_service.TextStruct{
		Number: recipient(to),
		Text:   text,
		Id:     id,
		Quoted: quotedStruct(quoted),
	}, inst)
	return err
}

func (w *whatsApp) SendMedia(_ context.Context, inst *instance_model.Instance, to types.JID, media OutgoingMedia, id string, quoted *Quoted) error {
	_, err := w.send.SendMediaFile(&send_service.MediaStruct{
		Number:   recipient(to),
		Type:     media.Type,
		Caption:  media.Caption,
		Filename: media.Filename,
		Id:       id,
		Quoted:   quotedStruct(quoted),
	}, media.Data, inst)
	return err
}

func (w *whatsApp) Revoke(ctx context.Context, inst *instance_model.Instance, chat types.JID, messageID string) error {
	c, err := w.client(inst.Id)
	if err != nil {
		return err
	}
	_, err = c.SendMessage(ctx, chat, c.BuildRevoke(chat, types.EmptyJID, messageID))
	return err
}

func (w *whatsApp) MarkRead(ctx context.Context, inst *instance_model.Instance, chat, sender types.JID, messageID string) error {
	c, err := w.client(inst.Id)
	if err != nil {
		return err
	}
	return c.MarkRead(ctx, []types.MessageID{messageID}, time.Now(), chat, sender)
}

func (w *whatsApp) Download(ctx context.Context, inst *instance_model.Instance, msg whatsmeow.DownloadableMessage) ([]byte, error) {
	c, err := w.client(inst.Id)
	if err != nil {
		return nil, err
	}
	return c.Download(ctx, msg)
}

func (w *whatsApp) ProfilePictureURL(ctx context.Context, inst *instance_model.Instance, jid types.JID) string {
	c, err := w.client(inst.Id)
	if err != nil {
		return ""
	}
	info, err := c.GetProfilePictureInfo(ctx, jid, &whatsmeow.GetProfilePictureParams{})
	if err != nil || info == nil {
		return ""
	}
	return info.URL
}
