package runtime

import (
	"testing"

	"github.com/disksing/pua/agenthub/internal/provider"
	"github.com/disksing/pua/agenthub/internal/session"
)

func TestTurnScopedSteerCannotStartOrCrossTurns(t *testing.T) {
	store, _, manager := pendingTestSession(t)
	manager.cfg.AgentProviders[0].Type = "codex"
	value, err := store.Create(session.CreateInput{Cwd: t.TempDir(), AgentName: "Fast Agent",
		InputCapabilities: session.InputCapabilities{Steer: true, TurnScopedSteer: true}})
	if err != nil {
		t.Fatal(err)
	}
	manager.factory = func(options provider.Options) (provider.Session, error) {
		return &scopedFakeSession{fakeSession{hooks: options.Hooks, holdTurn: true}}, nil
	}
	preview := session.MessageInput{SchemaVersion: 2, Text: "queued preview", Steer: true, ExpectedTurnID: "old-turn", MessageID: "preview"}
	if _, err := manager.SendMessage(value.ID, preview); err == nil {
		t.Fatal("preview started a new Turn")
	}
	first, err := manager.Send(value.ID, "work", false)
	if err != nil {
		t.Fatal(err)
	}
	preview.ExpectedTurnID = first.CurrentTurnID
	// Input capabilities are normally established by the configured provider.
	if _, err := manager.SendMessage(value.ID, preview); err != nil {
		t.Fatal(err)
	}
	if err := manager.Interrupt(value.ID); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Send(value.ID, "next", false)
	if err != nil {
		t.Fatal(err)
	}
	preview.MessageID = "late-preview"
	if _, err := manager.SendMessage(value.ID, preview); err == nil {
		t.Fatal("stale preview reached another Turn")
	}
	got, _ := store.Get(value.ID)
	if got.CurrentTurnID != second.CurrentTurnID {
		t.Fatal("rejected preview changed active Turn")
	}
}

func TestPendingPreviewDoesNotReviveClosedTurn(t *testing.T) {
	store, value, manager := pendingTestSession(t)
	var starts int
	manager.factory = func(options provider.Options) (provider.Session, error) {
		starts++
		return &scopedFakeSession{fakeSession{hooks: options.Hooks, holdTurn: true}}, nil
	}
	message := session.DurableMessage{TurnID: "old", Input: session.MessageInput{
		SchemaVersion: 2, Text: "preview", Steer: true, ExpectedTurnID: "old", MessageID: "preview-old",
	}}
	event, err := store.Append(value.ID, session.EventMessageInput, message.TurnID, marshal(message.Input))
	if err != nil {
		t.Fatal(err)
	}
	message.EventID = event.ID
	if !manager.deliverMessageLocked(value.ID, message) || starts != 0 {
		t.Fatal("recovered preview started provider work")
	}
	got, _ := store.Get(value.ID)
	if got.CurrentTurnID != "" {
		t.Fatal("recovered preview revived the Turn")
	}
}

type scopedFakeSession struct{ fakeSession }

func (f *scopedFakeSession) SteerActive(text string) error { return f.Prompt(text, true) }
