// Package hooks lets optional integrations observe WhatsApp activity without
// changing the code that produces it. The core calls Emit; integrations call
// Register once at startup.
package hooks

import (
	"fmt"
	"runtime/debug"
	"sync"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
)

// Event is emitted for every WhatsApp event that reaches the webhook stage and
// for every message sent through the send API.
type Event struct {
	Instance *instance_model.Instance
	Client   *whatsmeow.Client
	// Raw is the whatsmeow event (for example *events.Message) or a *SentMessage.
	Raw any
}

// SentMessage describes a message sent by this server through the send API.
// whatsmeow does not emit events for messages it sends itself.
type SentMessage struct {
	Info    types.MessageInfo
	Message *waE2E.Message
}

// Handler receives events. It runs on the caller's goroutine, so it must return
// quickly and hand slow work off to its own goroutines.
type Handler func(Event)

var (
	mu       sync.RWMutex
	handlers []Handler
	// OnPanic reports a recovered handler panic. It can be replaced in tests.
	OnPanic = func(recovered any, stack []byte) {
		fmt.Printf("[hooks] handler panic: %v\n%s\n", recovered, stack)
	}
)

// Register adds a handler that receives every emitted event.
func Register(h Handler) {
	mu.Lock()
	defer mu.Unlock()
	handlers = append(handlers, h)
}

// Emit delivers the event to every registered handler. A panicking handler is
// recovered so an integration bug can never stop WhatsApp event processing.
func Emit(e Event) {
	mu.RLock()
	current := handlers
	mu.RUnlock()

	for _, h := range current {
		call(h, e)
	}
}

func call(h Handler, e Event) {
	defer func() {
		if r := recover(); r != nil {
			OnPanic(r, debug.Stack())
		}
	}()
	h(e)
}

// reset removes all handlers. Used by tests.
func reset() {
	mu.Lock()
	defer mu.Unlock()
	handlers = nil
}
