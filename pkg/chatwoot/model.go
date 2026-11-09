package chatwoot

import "time"

// Config is the Chatwoot integration settings of one instance.
type Config struct {
	InstanceID string `json:"instanceId" gorm:"column:instance_id;primaryKey"`
	Enabled    bool   `json:"enabled"`

	URL       string `json:"url"`
	AccountID int    `json:"accountId" gorm:"column:account_id"`
	Token     string `json:"token"`

	InboxName string `json:"inboxName"`
	InboxID   int    `json:"inboxId" gorm:"column:inbox_id"`
	// WebhookSecret is the API inbox secret Chatwoot uses to sign webhooks.
	WebhookSecret string `json:"-" gorm:"column:webhook_secret"`

	// SignMessages prefixes agent replies with the agent name.
	SignMessages  bool   `json:"signMessages"`
	SignDelimiter string `json:"signDelimiter"`
	// ReopenConversation reuses the contact's last conversation even when it
	// is resolved, instead of starting a new one.
	ReopenConversation bool `json:"reopenConversation"`
	// ConversationPending creates new conversations as pending, so a bot can
	// handle them before agents do.
	ConversationPending bool `json:"conversationPending"`
	// MarkRead marks the contact's messages as read on WhatsApp when an agent replies.
	MarkRead bool `json:"markRead"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (Config) TableName() string { return "chatwoot_configs" }

// Message links a WhatsApp message to the Chatwoot message that represents it.
// One Chatwoot message with several attachments maps to several WhatsApp messages.
type Message struct {
	ID               uint   `gorm:"primaryKey"`
	InstanceID       string `gorm:"column:instance_id;index:idx_chatwoot_messages_wa,priority:1;index:idx_chatwoot_messages_cw,priority:1;index:idx_chatwoot_messages_chat,priority:1"`
	WAMessageID      string `gorm:"column:wa_message_id;index:idx_chatwoot_messages_wa,priority:2"`
	ChatJID          string `gorm:"column:chat_jid;index:idx_chatwoot_messages_chat,priority:2"`
	SenderJID        string `gorm:"column:sender_jid"`
	FromMe           bool   `gorm:"column:from_me"`
	CWMessageID      int    `gorm:"column:cw_message_id;index:idx_chatwoot_messages_cw,priority:2"`
	CWConversationID int    `gorm:"column:cw_conversation_id"`
	CreatedAt        time.Time
}

func (Message) TableName() string { return "chatwoot_messages" }
