package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTurnOffsetsOpenRestartAndLateDelivery(t *testing.T) {
	store, id, turnID := activeProjectionFixture(t)
	path := store.turnsPath(id)
	records, stamp, err := readTurnDiskRecords(path)
	if err != nil || stamp == nil || len(records) != 1 || records[0].Range == nil || records[0].Range.End != nil {
		t.Fatalf("open projection: %+v, %+v, %v", records, stamp, err)
	}
	initial, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	appendProjectionEvent(t, store, id, "message.assistant.delta", turnID, map[string]any{"text": " 更新"})
	afterDelta, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(initial, afterDelta) {
		t.Fatalf("delta rewrote the projection: %v", err)
	}

	reopened, err := Open(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	summary, err := reopened.Turn(id, turnID)
	if err != nil || summary.Closed || summary.FinalReplyPreview != "hello 更新" {
		t.Fatalf("reopened open turn: %+v, %v", summary, err)
	}
	records, _, err = readTurnDiskRecords(path)
	if err != nil || len(records) != 1 || records[0].Range == nil || records[0].Range.End != nil {
		t.Fatalf("refreshed open projection: %+v, %v", records, err)
	}
	terminal := appendProjectionEvent(t, reopened, id, EventTurnCompleted, turnID, TurnTerminalEventData{})
	records, _, err = readTurnDiskRecords(path)
	if err != nil || len(records) != 1 || records[0].Range == nil || records[0].Range.End == nil {
		t.Fatalf("closed projection: %+v, %v", records, err)
	}
	terminalOffset := *records[0].Range.End
	appendProjectionEvent(t, reopened, id, "provider.metadata", "", nil)
	late := appendProjectionEvent(t, reopened, id, EventMessageDelivery, turnID, MessageDeliveryEventData{
		MessageEventID: summary.FirstEventID, State: MessageDeliveryAccepted,
	})
	records, stamp, err = readTurnDiskRecords(path)
	if err != nil || len(records) != 1 || records[0].Range == nil || records[0].Range.End == nil ||
		*records[0].Range.End <= terminalOffset || records[0].EndEventID != terminal.ID || records[0].LastEventID != late.ID {
		t.Fatalf("late delivery projection: %+v, %v", records, err)
	}
	if *records[0].Range.End != stamp.Size || reopened.Diagnostics().EventIndexBuilds != 0 {
		t.Fatalf("unexpected checkpoint or full index build: %+v, %+v", stamp, reopened.Diagnostics())
	}
}

func TestTurnOffsetsRecoverDurableTerminalAndPageAfterRestart(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	appendProjectionEvent(t, store, created.ID, "turn.started", "previous", nil)
	appendProjectionEvent(t, store, created.ID, "message.assistant.delta", "previous", map[string]any{"text": strings.Repeat("x", 5<<20)})
	appendProjectionEvent(t, store, created.ID, EventTurnCompleted, "previous", nil)
	first := appendProjectionEvent(t, store, created.ID, EventMessageInput, "current", MessageInput{Text: "当前", Role: MessageRoleUser})
	appendProjectionEvent(t, store, created.ID, "turn.started", "current", nil)
	appendProjectionEvent(t, store, created.ID, "message.assistant.delta", "current", map[string]any{"text": "你好"})
	last := first.ID + 3
	appendRawEvent(t, root, created.ID, Event{ID: last, Type: EventTurnCancelled, TurnID: "current"})
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := reopened.Turn(created.ID, "current")
	if err != nil || !summary.Closed || summary.EndEventID != last || summary.FinalReplyPreview != "你好" {
		t.Fatalf("recovered turn: %+v, %v", summary, err)
	}
	secondOpen, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for cursor := first.ID - 1; cursor < last; cursor++ {
		page, err := secondOpen.EventsRangePage(created.ID, cursor, last, 1)
		if err != nil || len(page.Events) != 1 || page.Events[0].ID != cursor+1 {
			t.Fatalf("page after %d: %+v, %v", cursor, page, err)
		}
	}
	if secondOpen.Diagnostics().EventIndexBuilds != 0 {
		t.Fatal("range pagination built the whole event index")
	}
}

func TestTurnOffsetsBackfillLegacyArchive(t *testing.T) {
	store, id, turnID := activeProjectionFixture(t)
	if _, err := store.BackfillTurnOffsets(t.Context(), id); err != ErrSessionActive {
		t.Fatalf("active backfill: %v", err)
	}
	largeEvent := appendProjectionEvent(t, store, id, "provider.event", turnID, map[string]any{
		"output": strings.Repeat("x", 15<<20),
	})
	terminal := appendProjectionEvent(t, store, id, EventTurnCompleted, turnID, nil)
	appendProjectionEvent(t, store, id, "session.state", "", StateEventData{State: StateStopped})
	if _, err := store.Archive(id); err != nil {
		t.Fatal(err)
	}
	projection := filepath.Join(store.ArchiveRoot(), id, "turns.jsonl")
	summaries, err := readTurnRecordsRepairTail(projection)
	if err != nil || len(summaries) != 1 {
		t.Fatalf("projection: %+v, %v", summaries, err)
	}
	if err := writeTurnRecordsAtomic(projection, summaries); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := reopened.Turn(id, turnID)
	if err != nil || !legacy.Closed || legacy.FinalReplyPreview != "hello" {
		t.Fatalf("legacy read: %+v, %v", legacy, err)
	}
	eventPath := filepath.Join(store.ArchiveRoot(), id, "events.jsonl")
	before, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reopened.BackfillTurnOffsets(t.Context(), id)
	if err != nil || !result.Changed || result.Turns != 1 {
		t.Fatalf("backfill: %+v, %v", result, err)
	}
	result, err = reopened.BackfillTurnOffsets(t.Context(), id)
	if err != nil || result.Changed || result.Turns != 1 {
		t.Fatalf("repeated backfill: %+v, %v", result, err)
	}
	after, err := os.ReadFile(eventPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("event log changed during backfill: %v", err)
	}
	restarted, err := Open(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	page, err := restarted.EventsRangePage(id, largeEvent.ID-1, terminal.ID, 1)
	if err != nil {
		t.Fatalf("read backfilled range after restart: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].ID != largeEvent.ID || !bytes.Equal(page.Events[0].Data, largeEvent.Data) {
		t.Fatal("range page did not preserve the legacy provider event")
	}
	if restarted.Diagnostics().EventIndexBuilds != 0 {
		t.Fatal("range pagination built the whole event index")
	}
	summary, err := reopened.Turn(id, turnID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(summary)
	if err != nil || bytes.Contains(encoded, []byte("rawRange")) || bytes.Contains(encoded, []byte("rawCheckpoint")) {
		t.Fatalf("public summary includes disk fields: %s, %v", encoded, err)
	}
}
