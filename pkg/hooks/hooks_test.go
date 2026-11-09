package hooks

import (
	"testing"
)

func TestEmitCallsHandlersInOrder(t *testing.T) {
	t.Cleanup(reset)
	reset()

	var calls []string
	Register(func(Event) { calls = append(calls, "first") })
	Register(func(Event) { calls = append(calls, "second") })

	Emit(Event{})

	if len(calls) != 2 || calls[0] != "first" || calls[1] != "second" {
		t.Fatalf("unexpected calls: %v", calls)
	}
}

func TestEmitRecoversFromPanickingHandler(t *testing.T) {
	t.Cleanup(reset)
	reset()

	previous := OnPanic
	t.Cleanup(func() { OnPanic = previous })
	var recovered any
	OnPanic = func(r any, _ []byte) { recovered = r }

	reached := false
	Register(func(Event) { panic("boom") })
	Register(func(Event) { reached = true })

	Emit(Event{})

	if recovered != "boom" {
		t.Fatalf("panic not reported, got %v", recovered)
	}
	if !reached {
		t.Fatal("handler after the panicking one was not called")
	}
}

func TestEmitWithoutHandlers(t *testing.T) {
	t.Cleanup(reset)
	reset()
	Emit(Event{}) // must not panic
}
