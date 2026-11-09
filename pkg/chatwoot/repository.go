package chatwoot

import (
	"errors"

	"gorm.io/gorm"
)

// Repository stores integration settings and the WhatsApp/Chatwoot message links.
type Repository interface {
	Migrate() error
	GetConfig(instanceID string) (*Config, error)
	SaveConfig(cfg *Config) error
	SaveMessage(msg *Message) error
	FindByWAID(instanceID, waMessageID string) (*Message, error)
	FindByCWID(instanceID string, cwMessageID int) ([]Message, error)
	LastIncoming(instanceID, chatJID string) (*Message, error)
}

type gormRepository struct {
	db *gorm.DB
}

// NewRepository returns a Repository backed by the given database.
func NewRepository(db *gorm.DB) Repository {
	return &gormRepository{db: db}
}

func (r *gormRepository) Migrate() error {
	return r.db.AutoMigrate(&Config{}, &Message{})
}

// GetConfig returns nil, nil when the instance has no configuration.
func (r *gormRepository) GetConfig(instanceID string) (*Config, error) {
	var cfg Config
	err := r.db.First(&cfg, "instance_id = ?", instanceID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (r *gormRepository) SaveConfig(cfg *Config) error {
	return r.db.Save(cfg).Error
}

func (r *gormRepository) SaveMessage(msg *Message) error {
	return r.db.Create(msg).Error
}

// FindByWAID returns nil, nil when the WhatsApp message is unknown.
func (r *gormRepository) FindByWAID(instanceID, waMessageID string) (*Message, error) {
	var msg Message
	err := r.db.Where("instance_id = ? AND wa_message_id = ?", instanceID, waMessageID).First(&msg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &msg, nil
}

func (r *gormRepository) FindByCWID(instanceID string, cwMessageID int) ([]Message, error) {
	var msgs []Message
	err := r.db.Where("instance_id = ? AND cw_message_id = ?", instanceID, cwMessageID).Order("id").Find(&msgs).Error
	return msgs, err
}

// LastIncoming returns the newest message received from the chat, or nil, nil.
func (r *gormRepository) LastIncoming(instanceID, chatJID string) (*Message, error) {
	var msg Message
	err := r.db.Where("instance_id = ? AND chat_jid = ? AND from_me = ?", instanceID, chatJID, false).
		Order("id DESC").First(&msg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &msg, nil
}
