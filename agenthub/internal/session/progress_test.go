package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProgressIgnoresIncomingWorkAndQueueNoise(t *testing.T) {
	var value Session
	start := time.Date(2026, 10, 24, 5, 0, 0, 0, time.UTC)
	events := []Event{
		{Type: "turn.started", TurnID: "turn_work", Time: start},
		{Type: EventMessageInput, TurnID: "turn_work", Time: start.Add(10 * time.Minute)},
		{Type: "message.delivery", TurnID: "turn_work", Time: start.Add(11 * time.Minute)},
		{Type: "provider.event", TurnID: "turn_work", Time: start.Add(12 * time.Minute), Data: json.RawMessage(`{"method":"queue_update"}`)},
		{Type: EventMessageInput, TurnID: "turn_work", Time: start.Add(19 * time.Minute)},
		{Type: "provider.stderr", Time: start.Add(20 * time.Minute)},
	}
	for _, event := range events {
		if err := applyEvent(&value, event); err != nil {
			t.Fatal(err)
		}
		if value.LastProgressAt == nil || !value.LastProgressAt.Equal(start) || value.LastProgressTurnID != "turn_work" {
			t.Fatalf("%s changed progress: %+v", event.Type, value)
		}
	}
	if value.LastActivityAt == nil || !value.LastActivityAt.Equal(start.Add(19*time.Minute)) {
		t.Fatalf("incoming messages must retain their UI activity: %+v", value)
	}
	for i, kind := range []string{"message.assistant.delta", "message.reasoning.delta", "tool.call", "tool.event", "approval.requested", "approval.resolved", "provider.error", EventTurnCompleted, EventTurnFailed, EventTurnCancelled, "turn.started"} {
		at := start.Add(time.Duration(21+i) * time.Minute)
		if err := applyEvent(&value, Event{Type: kind, TurnID: "turn_next", Time: at, Data: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if value.LastProgressAt == nil || !value.LastProgressAt.Equal(at) || value.LastProgressTurnID != "turn_next" {
			t.Fatalf("%s did not advance progress: %+v", kind, value)
		}
	}
}

func TestOpenRebuildsLegacyProgressWithoutUsingRecentInput(t *testing.T) {
	for _, state := range []string{StateRunning, StateWaitingApproval} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			store, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			created, err := store.Create(CreateInput{Title: "legacy progress"})
			if err != nil {
				t.Fatal(err)
			}
			progress, err := store.Append(created.ID, "turn.started", "turn_legacy", nil)
			if err != nil {
				t.Fatal(err)
			}
			if state == StateWaitingApproval {
				progress, err = store.Append(created.ID, "approval.requested", "turn_legacy", []byte(`{"approvalId":"approval_test"}`))
				if err != nil {
					t.Fatal(err)
				}
			}
			input, err := store.Append(created.ID, EventMessageInput, "turn_legacy", []byte(`{"text":"still waiting","steer":true}`))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Append(created.ID, "provider.event", "turn_legacy", []byte(`{"method":"queue_update"}`)); err != nil {
				t.Fatal(err)
			}
			legacy, err := store.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			legacy.LastProgressAt, legacy.LastProgressTurnID = nil, ""
			path := filepath.Join(root, created.ID, "session.json")
			if err := writeJSONAtomic(path, legacy); err != nil {
				t.Fatal(err)
			}
			reopened := reopen(t, store, root)
			got, err := reopened.Get(created.ID)
			if err != nil || got.State != state || got.LastProgressAt == nil || !got.LastProgressAt.Equal(progress.Time) || got.LastProgressTurnID != "turn_legacy" || got.LastActivityAt == nil || !got.LastActivityAt.Equal(input.Time) {
				t.Fatalf("legacy replay = %+v, %v", got, err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var persisted Session
			if err := json.Unmarshal(data, &persisted); err != nil {
				t.Fatal(err)
			}
			if persisted.LastProgressAt == nil || !persisted.LastProgressAt.Equal(progress.Time) {
				t.Fatalf("progress migration was not persisted: %+v", persisted)
			}
			// A subsequent open can trust the upgraded projection without replay.
			trusted, ok := loadTrustedSnapshot(filepath.Dir(path), created.ID)
			if !ok || trusted.LastProgressAt == nil || !trusted.LastProgressAt.Equal(progress.Time) {
				t.Fatalf("upgraded snapshot = %+v, trusted=%v", trusted, ok)
			}
		})
	}
}
