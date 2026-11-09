package chatwoot

import (
	"os"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestGormRepository runs against a real PostgreSQL database when
// CHATWOOT_TEST_DSN is set, for example:
//
//	CHATWOOT_TEST_DSN="postgres://postgres:postgres@localhost:5432/test?sslmode=disable"
func TestGormRepository(t *testing.T) {
	dsn := os.Getenv("CHATWOOT_TEST_DSN")
	if dsn == "" {
		t.Skip("CHATWOOT_TEST_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("DROP TABLE IF EXISTS chatwoot_configs, chatwoot_messages")
	repo := NewRepository(db)
	if err := repo.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := repo.Migrate(); err != nil { // must be idempotent across restarts
		t.Fatalf("second migration: %v", err)
	}

	if cfg, err := repo.GetConfig("missing"); err != nil || cfg != nil {
		t.Fatalf("missing config: %v %v", cfg, err)
	}

	cfg := &Config{InstanceID: "inst-1", Enabled: true, URL: "http://cw", AccountID: 1, Token: "t", InboxID: 4, WebhookSecret: "s", ConversationPending: true}
	if err := repo.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Enabled = false // false must be stored, not replaced by a default
	if err := repo.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetConfig("inst-1")
	if err != nil || got == nil || got.Enabled || got.WebhookSecret != "s" || !got.ConversationPending {
		t.Fatalf("config round trip: %+v %v", got, err)
	}

	msgs := []Message{
		{InstanceID: "inst-1", WAMessageID: "A", ChatJID: "c1", SenderJID: "c1", CWMessageID: 10, CWConversationID: 1},
		{InstanceID: "inst-1", WAMessageID: "B", ChatJID: "c1", FromMe: true, CWMessageID: 11, CWConversationID: 1},
		{InstanceID: "inst-1", WAMessageID: "C", ChatJID: "c1", FromMe: true, CWMessageID: 11, CWConversationID: 1},
		{InstanceID: "inst-2", WAMessageID: "A", ChatJID: "c1", CWMessageID: 99},
	}
	for i := range msgs {
		if err := repo.SaveMessage(&msgs[i]); err != nil {
			t.Fatal(err)
		}
	}

	if m, err := repo.FindByWAID("inst-1", "A"); err != nil || m == nil || m.CWMessageID != 10 {
		t.Fatalf("FindByWAID: %+v %v", m, err)
	}
	if m, err := repo.FindByWAID("inst-1", "Z"); err != nil || m != nil {
		t.Fatalf("FindByWAID unknown: %+v %v", m, err)
	}
	if ms, err := repo.FindByCWID("inst-1", 11); err != nil || len(ms) != 2 || ms[0].WAMessageID != "B" {
		t.Fatalf("FindByCWID: %+v %v", ms, err)
	}
	if m, err := repo.LastIncoming("inst-1", "c1"); err != nil || m == nil || m.WAMessageID != "A" {
		t.Fatalf("LastIncoming: %+v %v", m, err)
	}
	if m, err := repo.LastIncoming("inst-1", "other"); err != nil || m != nil {
		t.Fatalf("LastIncoming unknown chat: %+v %v", m, err)
	}
}
