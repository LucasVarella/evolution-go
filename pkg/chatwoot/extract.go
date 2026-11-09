package chatwoot

import (
	"fmt"
	"mime"
	"regexp"
	"strings"

	"github.com/gabriel-vasile/mimetype"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// parsedMessage is the part of a WhatsApp message the integration forwards.
type parsedMessage struct {
	Text     string
	QuotedID string

	Media     whatsmeow.DownloadableMessage
	MediaKind string // image, video, audio, document or sticker
	Mimetype  string
	Filename  string

	ReactionTo string // ID of the message a reaction refers to
	RevokedID  string // ID of a message deleted for everyone
	EditedID   string // ID of an edited message (Text holds the new text)

	// Skip is set for messages that carry nothing to show, such as key
	// distribution or history sync notifications.
	Skip bool
}

var vcardPhone = regexp.MustCompile(`(?m)^TEL[^:\n]*:\s*([+\d][\d\s()-]*)`)

// unwrap removes the containers WhatsApp puts around the actual content.
func unwrap(msg *waE2E.Message) *waE2E.Message {
	for i := 0; i < 5 && msg != nil; i++ {
		switch {
		case msg.GetEphemeralMessage().GetMessage() != nil:
			msg = msg.GetEphemeralMessage().GetMessage()
		case msg.GetViewOnceMessage().GetMessage() != nil:
			msg = msg.GetViewOnceMessage().GetMessage()
		case msg.GetViewOnceMessageV2().GetMessage() != nil:
			msg = msg.GetViewOnceMessageV2().GetMessage()
		case msg.GetViewOnceMessageV2Extension().GetMessage() != nil:
			msg = msg.GetViewOnceMessageV2Extension().GetMessage()
		case msg.GetDocumentWithCaptionMessage().GetMessage() != nil:
			msg = msg.GetDocumentWithCaptionMessage().GetMessage()
		default:
			return msg
		}
	}
	return msg
}

func parseMessage(raw *waE2E.Message) parsedMessage {
	msg := unwrap(raw)
	var p parsedMessage
	if msg == nil {
		p.Skip = true
		return p
	}

	if proto := msg.GetProtocolMessage(); proto != nil {
		switch proto.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			p.RevokedID = proto.GetKey().GetID()
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			edited := parseMessage(proto.GetEditedMessage())
			if edited.Text != "" {
				p.EditedID = proto.GetKey().GetID()
				p.Text = edited.Text
				return p
			}
			p.Skip = true
		default:
			p.Skip = true
		}
		return p
	}

	switch {
	case msg.Conversation != nil:
		p.Text = msg.GetConversation()
	case msg.ExtendedTextMessage != nil:
		m := msg.GetExtendedTextMessage()
		p.Text = m.GetText()
		p.QuotedID = m.GetContextInfo().GetStanzaID()
	case msg.ImageMessage != nil:
		m := msg.GetImageMessage()
		p.setMedia(m, "image", m.GetMimetype(), "")
		p.Text = m.GetCaption()
		p.QuotedID = m.GetContextInfo().GetStanzaID()
	case msg.VideoMessage != nil:
		m := msg.GetVideoMessage()
		p.setMedia(m, "video", m.GetMimetype(), "")
		p.Text = m.GetCaption()
		p.QuotedID = m.GetContextInfo().GetStanzaID()
	case msg.PtvMessage != nil:
		m := msg.GetPtvMessage()
		p.setMedia(m, "video", m.GetMimetype(), "")
		p.QuotedID = m.GetContextInfo().GetStanzaID()
	case msg.AudioMessage != nil:
		m := msg.GetAudioMessage()
		p.setMedia(m, "audio", m.GetMimetype(), "")
		p.QuotedID = m.GetContextInfo().GetStanzaID()
	case msg.DocumentMessage != nil:
		m := msg.GetDocumentMessage()
		p.setMedia(m, "document", m.GetMimetype(), m.GetFileName())
		p.Text = m.GetCaption()
		p.QuotedID = m.GetContextInfo().GetStanzaID()
	case msg.StickerMessage != nil:
		m := msg.GetStickerMessage()
		p.setMedia(m, "sticker", m.GetMimetype(), "")
		p.QuotedID = m.GetContextInfo().GetStanzaID()
	case msg.ReactionMessage != nil:
		r := msg.GetReactionMessage()
		if r.GetText() == "" { // reaction removed
			p.Skip = true
			return p
		}
		p.Text = r.GetText()
		p.ReactionTo = r.GetKey().GetID()
	case msg.LocationMessage != nil:
		l := msg.GetLocationMessage()
		place := strings.TrimSpace(strings.Join(nonEmpty(l.GetName(), l.GetAddress()), " - "))
		if place == "" {
			place = "Localização"
		}
		p.Text = fmt.Sprintf(textLocation, place, l.GetDegreesLatitude(), l.GetDegreesLongitude())
	case msg.ContactMessage != nil:
		p.Text = contactText(msg.GetContactMessage())
	case msg.ContactsArrayMessage != nil:
		var parts []string
		for _, c := range msg.GetContactsArrayMessage().GetContacts() {
			parts = append(parts, contactText(c))
		}
		p.Text = strings.Join(parts, "\n\n")
	case msg.SenderKeyDistributionMessage != nil, msg.MessageContextInfo != nil && countContent(msg) == 0:
		p.Skip = true
	default:
		p.Text = fmt.Sprintf(textUnsupported, messageTypeName(msg))
	}
	return p
}

func (p *parsedMessage) setMedia(m whatsmeow.DownloadableMessage, kind, mimeType, filename string) {
	p.Media = m
	p.MediaKind = kind
	if mediaType, _, err := mime.ParseMediaType(mimeType); err == nil {
		mimeType = mediaType
	}
	p.Mimetype = mimeType
	if filename == "" {
		filename = kind + extensionFor(mimeType)
	}
	p.Filename = filename
}

// commonExtensions overrides extensions the MIME library picks differently
// from what users expect (it maps audio/ogg to .oga).
var commonExtensions = map[string]string{
	"audio/ogg":  ".ogg",
	"audio/mpeg": ".mp3",
	"image/jpeg": ".jpg",
}

func extensionFor(mimeType string) string {
	if ext, ok := commonExtensions[mimeType]; ok {
		return ext
	}
	if m := mimetype.Lookup(mimeType); m != nil && m.Extension() != "" {
		return m.Extension()
	}
	return ".bin"
}

func contactText(c *waE2E.ContactMessage) string {
	text := fmt.Sprintf(textContact, c.GetDisplayName())
	for _, m := range vcardPhone.FindAllStringSubmatch(c.GetVcard(), -1) {
		text += "\n" + strings.TrimSpace(m[1])
	}
	return text
}

func nonEmpty(values ...string) []string {
	var out []string
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}

// countContent counts the populated message fields other than the context info.
func countContent(msg *waE2E.Message) int {
	n := 0
	msg.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		if fd.Name() != "messageContextInfo" {
			n++
		}
		return true
	})
	return n
}

// messageTypeName returns the name of the first populated message field, for
// the "unsupported message" notice.
func messageTypeName(msg *waE2E.Message) string {
	name := "desconhecido"
	msg.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		if fd.Name() == "messageContextInfo" {
			return true
		}
		name = string(fd.Name())
		return false
	})
	return name
}
