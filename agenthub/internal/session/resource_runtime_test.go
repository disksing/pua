package session

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCreateOrGetIsDurablyIdempotent(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	input := CreateInput{
		Title: "Resource generation", Cwd: t.TempDir(), AgentName: "Codex",
		IdempotencyKey: "generation-01H", Provider: "codex",
		InputCapabilities: InputCapabilities{Steer: true},
		Source: &Source{App: "pua", InstanceID: "workspace-1", ExternalID: "project1.task2/1", Metadata: map[string]string{
			"resourceId": "project1.task2", "generationId": "generation-01H",
		}},
	}
	first, created, err := store.CreateOrGet(input)
	if err != nil || !created {
		t.Fatalf("first create = %+v, %v, %v", first, created, err)
	}
	second, created, err := store.CreateOrGet(input)
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("retry = %+v, %v, %v; want %s", second, created, err, first.ID)
	}
	conflict := input
	conflict.AgentName = "Pi"
	if _, _, err := store.CreateOrGet(conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	third, created, err := reopened.CreateOrGet(input)
	if err != nil || created || third.ID != first.ID || third.Source.Metadata["resourceId"] != "project1.task2" {
		t.Fatalf("restart retry = %+v, %v, %v", third, created, err)
	}
}

func TestTurnIndexUsesStableEventReferencesAndArchivedHistory(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Cwd: t.TempDir(), AgentName: "Agent"})
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(eventType, turnID string, value any) Event {
		t.Helper()
		var data []byte
		if value != nil {
			data, _ = json.Marshal(value)
		}
		event, err := store.Append(created.ID, eventType, turnID, data)
		if err != nil {
			t.Fatal(err)
		}
		return event
	}
	appendEvent(EventMessageInput, "turn_1", MessageInput{Text: "first request", Role: MessageRoleUser, MessageID: "msg_1"})
	firstStart := appendEvent("turn.started", "turn_1", nil)
	appendEvent("tool.event", "turn_1", map[string]any{"method": "read"})
	appendEvent("message.assistant.delta", "turn_1", map[string]any{"text": "first reply"})
	firstEnd := appendEvent(EventTurnCompleted, "turn_1", nil)
	appendEvent(EventMessageInput, "turn_2", MessageInput{Text: "second request", Role: MessageRoleAgent})
	secondStart := appendEvent("turn.started", "turn_2", nil)

	firstPage, err := store.TurnsPage(created.ID, 0, 0, false, 1)
	if err != nil || len(firstPage.Turns) != 1 || !firstPage.HasMore {
		t.Fatalf("first page = %+v, %v", firstPage, err)
	}
	first := firstPage.Turns[0]
	if first.ID != "turn_1" || first.Status != "completed" || first.TriggerEventID == 0 || first.FinalReplyEventID == 0 || first.ToolEventCount != 1 {
		t.Fatalf("first summary = %+v", first)
	}
	secondPage, err := store.TurnsPage(created.ID, firstPage.NextAfter, 0, false, 1)
	if err != nil || len(secondPage.Turns) != 1 || secondPage.Turns[0].ID != "turn_2" || secondPage.Turns[0].Status != "active" {
		t.Fatalf("second page = %+v, %v", secondPage, err)
	}
	for _, page := range []TurnPage{firstPage, secondPage} {
		totals := page.Totals
		if totals.Count != 2 || totals.ExecutionMS == nil || *totals.ExecutionMS != firstEnd.Time.Sub(firstStart.Time).Milliseconds() ||
			totals.ActiveStartedAt == nil || !totals.ActiveStartedAt.Equal(secondStart.Time) {
			t.Fatalf("partial page lost whole-session totals: %+v", totals)
		}
	}
	if accepted, err := store.HasMessageID(created.ID, "msg_1"); err != nil || !accepted {
		t.Fatalf("message id lookup = %v, %v", accepted, err)
	}
	secondEnd := appendEvent(EventTurnCancelled, "turn_2", TurnTerminalEventData{Reason: "stop"})
	appendEvent("session.state", "", StateEventData{State: StateStopped})
	if _, err := store.Archive(created.ID); err != nil {
		t.Fatal(err)
	}
	latest, err := store.TurnsPage(created.ID, 0, 0, true, 1)
	if err != nil || len(latest.Turns) != 1 || latest.Turns[0].ID != "turn_2" || latest.Turns[0].Status != "cancelled" {
		t.Fatalf("archived latest = %+v, %v", latest, err)
	}
	totals := latest.Totals
	want := firstEnd.Time.Sub(firstStart.Time).Milliseconds() + secondEnd.Time.Sub(secondStart.Time).Milliseconds()
	if totals.Count != 2 || totals.ExecutionMS == nil || *totals.ExecutionMS != want || totals.ActiveStartedAt != nil {
		t.Fatalf("archived totals = %+v, want %dms with no active interval", totals, want)
	}
}

func TestTurnTotalsExcludeQueueAndIdleTime(t *testing.T) {
	start := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	event := func(id int64, kind, turn string, offset time.Duration) Event {
		return Event{ID: id, Type: kind, TurnID: turn, Time: start.Add(offset)}
	}
	events := []Event{
		event(1, EventMessageInput, "first", 0),
		event(2, "turn.started", "first", time.Second),
		event(3, EventTurnCompleted, "first", 3*time.Second),
		event(4, "turn.started", "second", time.Hour),
		event(5, EventMessageInput, "pending", time.Hour),
	}
	totals := turnTotals(buildTurnSummaries(events), "second", 5)
	if totals.Count != 2 || totals.ExecutionMS == nil || *totals.ExecutionMS != 2000 ||
		totals.ActiveStartedAt == nil || !totals.ActiveStartedAt.Equal(start.Add(time.Hour)) {
		t.Fatalf("unexpected execution intervals: %+v", totals)
	}
	events = append(events, event(6, EventTurnFailed, "second", time.Hour+3*time.Second))
	totals = turnTotals(buildTurnSummaries(events), "", 6)
	if totals.Count != 2 || totals.ExecutionMS == nil || *totals.ExecutionMS != 5000 || totals.ActiveStartedAt != nil {
		t.Fatalf("unexpected completed intervals: %+v", totals)
	}
}
