package session

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func activeProjectionFixture(t *testing.T) (*Store, string, string) {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{})
	if err != nil {
		t.Fatal(err)
	}
	turnID := "turn_cached"
	appendProjectionEvent(t, store, created.ID, EventMessageInput, turnID, MessageInput{Text: "work", Payload: []byte(`{"key":"value"}`), SchemaVersion: 2})
	appendProjectionEvent(t, store, created.ID, "turn.started", turnID, nil)
	appendProjectionEvent(t, store, created.ID, "message.assistant.delta", turnID, map[string]any{"text": "hello"})
	return store, created.ID, turnID
}

func TestActiveTurnRepeatedQueriesReuseContentRevision(t *testing.T) {
	store, id, turnID := activeProjectionFixture(t)
	turn, err := store.Turn(id, turnID)
	if err != nil {
		t.Fatal(err)
	}
	before := store.Diagnostics()
	// Mutating a response must not mutate the cached read model.
	turn.TriggerPayload[0] = ' '
	turn.Items[0].Text = "mutated"
	var group sync.WaitGroup
	for worker := 0; worker < 10; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			page, err := store.TurnsPage(id, 0, 0, true, 10)
			if err != nil || len(page.Turns) != 1 || page.Turns[0].Items[0].Text != "work" || string(page.Turns[0].TriggerPayload) != `{"key":"value"}` {
				t.Errorf("cached page = %+v, err=%v", page, err)
			}
		}()
	}
	group.Wait()
	after := store.Diagnostics()
	if after.TurnBuilds != before.TurnBuilds || after.TurnProjectionReads != before.TurnProjectionReads || after.TurnProjectionWrites != before.TurnProjectionWrites || after.TurnCacheHits-before.TurnCacheHits != 10 {
		t.Fatalf("repeated query work: before=%+v after=%+v", before, after)
	}
}

func TestTurnCacheInvalidatesForSameIDDeltaAndTerminal(t *testing.T) {
	store, id, turnID := activeProjectionFixture(t)
	before, err := store.Turn(id, turnID)
	if err != nil {
		t.Fatal(err)
	}
	oldCursor := store.sessions[id].session.LastEventID
	appendProjectionEvent(t, store, id, "message.assistant.delta", turnID, map[string]any{"text": " world"})
	if store.sessions[id].session.LastEventID != oldCursor {
		t.Fatal("fixture did not exercise a same-ID delta merge")
	}
	for _, terminal := range []bool{false, true} {
		if terminal {
			appendProjectionEvent(t, store, id, EventTurnFailed, turnID, map[string]any{"error": "failed"})
		}
		got, err := store.Turn(id, turnID)
		if err != nil {
			t.Fatal(err)
		}
		want := buildTurnSummaries(store.sessions[id].events)[0]
		if !reflect.DeepEqual(got, want) || (!terminal && got.FinalReplyPreview == before.FinalReplyPreview) {
			t.Fatalf("revision projection mismatch: got=%+v want=%+v", got, want)
		}
	}
	reopened, err := Open(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Turn(id, turnID)
	if err != nil || !got.Closed || got.Status != "failed" || got.FinalReplyPreview != "hello world" {
		t.Fatalf("reopened projection = %+v, err=%v", got, err)
	}
}

func TestOversizedTurnProjectionSkipsCacheWithoutRepeatedRebuild(t *testing.T) {
	store, id, turnID := activeProjectionFixture(t)
	for index := 0; index < 2; index++ {
		appendProjectionEvent(t, store, id, "message.assistant.delta", turnID, map[string]any{"text": strings.Repeat("x", 4<<20), "part": index})
	}
	if _, err := store.Turn(id, turnID); err != nil {
		t.Fatal(err)
	}
	before := store.Diagnostics()
	if before.TurnCacheEntries != 0 || before.TurnRebuilds != 1 {
		t.Fatalf("oversized result admitted: %+v", before)
	}
	if _, err := store.Turn(id, turnID); err != nil {
		t.Fatal(err)
	}
	after := store.Diagnostics()
	if after.TurnRebuilds != before.TurnRebuilds || after.TurnBuilds != before.TurnBuilds || after.TurnProjectionWrites != before.TurnProjectionWrites || after.TurnProjectionReads != before.TurnProjectionReads+1 {
		t.Fatalf("oversized projection rebuilt: before=%+v after=%+v", before, after)
	}
}

func TestTurnCacheBudgetAndFileIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projection")
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var cache turnCache
	for index := 0; index < 100; index++ {
		id := fmt.Sprint(index)
		cache.put(id, 1, info, []TurnSummary{{ID: id, Items: []TurnItem{{Text: strings.Repeat("x", 1<<20)}}}})
	}
	entries, bytes := cache.size()
	if bytes > turnCacheBudget || entries >= turnCacheMaxEntries || entries == 0 {
		t.Fatalf("cache entries=%d estimated bytes=%d", entries, bytes)
	}
	if _, ok := cache.get("0", 1, info); ok {
		t.Fatal("oldest entry was not evicted")
	}
	if _, ok := cache.get("99", 2, info); ok {
		t.Fatal("revision mismatch hit")
	}
	for index := 0; index < 100; index++ {
		cache.put(fmt.Sprint(index), 1, info, nil)
	}
	entries, _ = cache.size()
	if entries != turnCacheMaxEntries {
		t.Fatalf("entry bound = %d", entries)
	}
	// Replace the file while keeping size and mtime identical.
	temp := path + ".new"
	if err := os.WriteFile(temp, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(temp, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, path); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.get("99", 1, replacement); ok {
		t.Fatal("replaced projection reused cached content")
	}
}

func TestHistoryDiagnosticsDistinguishSnapshotAndLazyLoad(t *testing.T) {
	store, id, _ := activeProjectionFixture(t)
	reopened, err := Open(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	before := reopened.Diagnostics()
	if before.TrustedSnapshots != 1 || before.FallbackReplays != 0 || before.HistoryLoads != 0 || before.CachedEventPayloadBytes != 0 || before.OpenNanos <= 0 {
		t.Fatalf("snapshot diagnostics = %+v", before)
	}
	if _, err := reopened.EventsAfter(id, 0, 100); err != nil {
		t.Fatal(err)
	}
	var wantBytes int64
	for _, event := range store.sessions[id].events {
		wantBytes += int64(len(event.Data))
	}
	after := reopened.Diagnostics()
	if after.HistoryLoads != 1 || after.CachedEventPayloadBytes != wantBytes {
		t.Fatalf("history diagnostics = %+v, want payload bytes=%d", after, wantBytes)
	}
	if _, err := reopened.EventsAfter(id, 0, 100); err != nil {
		t.Fatal(err)
	}
	if reopened.Diagnostics().HistoryLoads != 1 {
		t.Fatal("loaded history was read twice")
	}
	if err := os.Remove(reopened.snapshotPath(id)); err != nil {
		t.Fatal(err)
	}
	replayed, err := Open(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	if got := replayed.Diagnostics(); got.FallbackReplays != 1 || got.TrustedSnapshots != 0 || got.HistoryLoads != 1 || got.CachedEventPayloadBytes != wantBytes {
		t.Fatalf("replay diagnostics = %+v", got)
	}
}
