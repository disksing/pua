package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// appendRawEvent appends one encoded event directly to the durable log,
// bypassing the snapshot update, to simulate a crash between the log write
// and the snapshot write.
func appendRawEvent(t *testing.T, root, id string, event Event) {
	t.Helper()
	appendRawEvents(t, root, id, event)
}

func appendRawEvents(t *testing.T, root, id string, events ...Event) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(root, id, "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, event := range events {
		event.SessionID = id
		if event.Time.IsZero() {
			event.Time = time.Now().UTC()
		}
		if err := encoder.Encode(event); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func reopen(t *testing.T, store *Store, root string) *Store {
	t.Helper()
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return reopened
}

func TestOpenTrustsConsistentSnapshot(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Title: "Trusted", Cwd: t.TempDir(), AgentName: "Codex Build"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, "turn.started", "turn_1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, "turn.completed", "turn_1", nil); err != nil {
		t.Fatal(err)
	}
	want, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}

	reopened := reopen(t, store, root)
	state := reopened.sessions[created.ID]
	if state == nil {
		t.Fatal("session missing after reopen")
	}
	if state.eventsLoaded {
		t.Fatal("consistent snapshot should skip the full event replay")
	}
	got, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != want.State || got.LastEventID != want.LastEventID || got.Title != want.Title || got.AgentName != want.AgentName {
		t.Fatalf("trusted snapshot mismatch: got %+v want %+v", got, want)
	}

	// History lazy-loads on first read and matches the original log.
	events, err := reopened.EventsAfter(created.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Type != "session.created" || events[2].Type != "turn.completed" {
		t.Fatalf("unexpected lazy-loaded history: %d events", len(events))
	}
	if !state.eventsLoaded {
		t.Fatal("history read should have loaded the events")
	}

	// Appending to a lazily opened session keeps the log continuous.
	if _, err := reopened.Append(created.ID, "session.state", "", []byte(`{"state":"stopped","reason":"requested"}`)); err != nil {
		t.Fatal(err)
	}
	value, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.State != StateStopped || value.LastEventID != 4 {
		t.Fatalf("unexpected projection after append: %+v", value)
	}
}

func TestOpenReplaysWhenSnapshotLagsLog(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Title: "Lagging", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the durable append but before the snapshot
	// write: the log carries one more event than session.json records.
	appendRawEvent(t, root, created.ID, Event{
		ID:   2,
		Type: "session.state",
		Data: json.RawMessage(`{"state":"stopped","reason":"requested"}`),
	})

	reopened := reopen(t, store, root)
	state := reopened.sessions[created.ID]
	if state == nil {
		t.Fatal("session missing after reopen")
	}
	if reopened.Diagnostics().FallbackReplays != 1 {
		t.Fatal("a snapshot behind the log must fall back to a full replay")
	}
	value, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.State != StateStopped || value.StopReason != StopReasonRequested || value.LastEventID != 2 {
		t.Fatalf("replay did not pick up the trailing event: %+v", value)
	}
	// The replayed projection is persisted again.
	snapshot, ok := loadTrustedSnapshot(filepath.Join(root, created.ID), created.ID)
	if !ok || snapshot.LastEventID != 2 {
		t.Fatalf("snapshot was not rebuilt after replay: %+v ok=%v", snapshot, ok)
	}
}

func TestOpenReplaysWhenSnapshotCorrupt(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Title: "CorruptMe", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, created.ID, "session.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened := reopen(t, store, root)
	value, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.Title != "CorruptMe" || value.State != StateReady || value.LastEventID != 1 {
		t.Fatalf("replay from log failed: %+v", value)
	}
}

func TestOpenRepairsTornTailDespiteSnapshot(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Title: "Torn", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// A torn tail means the snapshot cannot be trusted even when its
	// watermark matches: the crash interrupted a durable write.
	eventsPath := filepath.Join(root, created.ID, "events.jsonl")
	file, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte(`{"id":2,"type":"session.state","data":{"st`)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := reopen(t, store, root)
	state := reopened.sessions[created.ID]
	if !state.eventsLoaded {
		t.Fatal("a torn tail must take the replay-and-repair path")
	}
	value, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.State != StateReady || value.LastEventID != 1 {
		t.Fatalf("unexpected state after tail repair: %+v", value)
	}
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatal("torn tail was not truncated")
	}
}

func TestOpenProviderProcessLazyMatchesLoaded(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	processData := func(pid, pgid int) json.RawMessage {
		data, _ := json.Marshal(ProviderProcessEventData{PID: pid, ProcessGroupID: pgid})
		return data
	}

	// Open process: started and never stopped.
	running, err := store.Create(CreateInput{Title: "Running", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(running.ID, "provider.process.started", "", processData(4242, 4242)); err != nil {
		t.Fatal(err)
	}
	// A ready state record must not settle the scan; only stopped does.
	if _, err := store.Append(running.ID, "session.state", "", []byte(`{"state":"ready"}`)); err != nil {
		t.Fatal(err)
	}

	// Closed process: started, then the session stopped.
	stopped, err := store.Create(CreateInput{Title: "Stopped", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(stopped.ID, "provider.process.started", "", processData(5353, 5353)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(stopped.ID, "session.state", "", []byte(`{"state":"stopped","reason":"completed"}`)); err != nil {
		t.Fatal(err)
	}

	// Legacy log without any process metadata.
	legacy, err := store.Create(CreateInput{Title: "Legacy", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	reopened := reopen(t, store, root)
	for _, id := range []string{running.ID, stopped.ID, legacy.ID} {
		if reopened.sessions[id].eventsLoaded {
			t.Fatalf("%s unexpectedly took the replay path", id)
		}
	}

	process, open, err := reopened.OpenProviderProcess(running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !open || process.PID != 4242 || process.ProcessGroupID != 4242 {
		t.Fatalf("lazy scan: got %+v open=%v", process, open)
	}
	if process, open, err = reopened.OpenProviderProcess(stopped.ID); err != nil || open {
		t.Fatalf("lazy scan stopped: got %+v open=%v err=%v", process, open, err)
	}
	if process, open, err = reopened.OpenProviderProcess(legacy.ID); err != nil || open || process.PID != 0 {
		t.Fatalf("lazy scan legacy: got %+v open=%v err=%v", process, open, err)
	}

	// Loading the events must not change the answer.
	if _, err := reopened.EventsAfter(running.ID, 0, 100); err != nil {
		t.Fatal(err)
	}
	process, open, err = reopened.OpenProviderProcess(running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !open || process.PID != 4242 {
		t.Fatalf("loaded scan: got %+v open=%v", process, open)
	}
}

func TestOpenProviderProcessTailScanSpansChunks(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Title: "Deep", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(ProviderProcessEventData{PID: 777, ProcessGroupID: 777})
	if _, err := store.Append(created.ID, "provider.process.started", "", data); err != nil {
		t.Fatal(err)
	}
	// Bury the process record under more than two tail-scan chunks of
	// unrelated events.
	filler, _ := json.Marshal(map[string]string{"text": string(make([]byte, 4096))})
	events := make([]Event, 0, 200)
	for i := 0; i < 200; i++ {
		events = append(events, Event{ID: int64(i + 3), Type: "message.tool", Data: filler})
	}
	appendRawEvents(t, root, created.ID, events...)
	snapshot, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.LastEventID = events[len(events)-1].ID
	snapshotData, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, created.ID, "session.json"), append(snapshotData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened := reopen(t, store, root)
	if reopened.sessions[created.ID].eventsLoaded {
		t.Fatal("expected the trusted snapshot path")
	}
	process, open, err := reopened.OpenProviderProcess(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !open || process.PID != 777 {
		t.Fatalf("tail scan across chunks: got %+v open=%v", process, open)
	}
}

func TestArchivedSessionLazyLoadsEvents(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Title: "Archived", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, "turn.started", "turn_1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, "turn.completed", "turn_1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, "session.state", "", []byte(`{"state":"stopped","reason":"completed"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Archive(created.ID); err != nil {
		t.Fatal(err)
	}

	reopened := reopen(t, store, root)
	state := reopened.sessions[created.ID]
	if state == nil || !state.archived {
		t.Fatal("archived session missing after reopen")
	}
	if state.eventsLoaded {
		t.Fatal("archived session should open from its snapshot")
	}
	events, err := reopened.EventsAfter(created.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 || events[4].Type != "session.archived" {
		t.Fatalf("unexpected archived history: %d events", len(events))
	}
}

func TestTrustedSnapshotStartupUsesSnapshotsAcrossSessions(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	// Multiple independent sessions must all take the trusted-snapshot path.
	filler, _ := json.Marshal(map[string]string{"text": string(make([]byte, 32*1024))})
	const sessionCount = 4
	for i := 0; i < sessionCount; i++ {
		created, err := store.Create(CreateInput{Title: fmt.Sprintf("Bulk %d", i), Cwd: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 2; j++ {
			if _, err := store.Append(created.ID, "message.tool", "", filler); err != nil {
				t.Fatal(err)
			}
		}
	}

	reopened := reopen(t, store, root)
	if len(reopened.List(false)) != sessionCount {
		t.Fatalf("expected %d sessions, got %d", sessionCount, len(reopened.List(false)))
	}
	for id, state := range reopened.sessions {
		if state.eventsLoaded {
			t.Fatalf("%s took the replay path despite a consistent snapshot", id)
		}
	}
}
