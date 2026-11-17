package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func largeHistoryStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	id := memoryHistoryFixture(t, root, 1152, 4096)
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return store, id
}

func TestIndexedHistoryPreservesCursorsAndOwnership(t *testing.T) {
	store, id := largeHistoryStore(t)
	for cursor := int64(0); cursor < 1153; {
		page, err := store.EventsPage(id, cursor, 127)
		if err != nil {
			t.Fatal(err)
		}
		for index, event := range page.Events {
			if event.ID != cursor+int64(index)+1 {
				t.Fatalf("cursor %d: event %d = %d", cursor, index, event.ID)
			}
		}
		if page.LatestCursor != 1153 || page.HasMore != (page.NextAfter < 1153) || len(page.Events) == 0 {
			t.Fatalf("page = %+v", page)
		}
		cursor = page.NextAfter
	}
	page, err := store.EventsPageBefore(id, 999999, 129)
	if err != nil {
		t.Fatal(err)
	}
	if page.Before != 1154 || page.NextBefore != 1025 || page.NextAfter != 1153 || !page.HasMoreBefore {
		t.Fatalf("backward page = %+v", page)
	}
	events, err := store.EventsThrough(id, 126, 130, 100)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, events, 127, 128, 129, 130)
	events[0].Data[0] = '!'
	events, err = store.EventsThrough(id, 126, 130, 100)
	if err != nil || !json.Valid(events[0].Data) {
		t.Fatalf("caller changed cached history: %v", err)
	}
	empty, err := store.EventsPageBefore(id, 1, 100)
	if err != nil || len(empty.Events) != 0 || empty.NextAfter != 0 {
		t.Fatalf("empty page = %+v, %v", empty, err)
	}
	if _, err := store.EventsPage(id, 1154, 1); !errors.Is(err, ErrEventCursorAhead) {
		t.Fatalf("ahead cursor: %v", err)
	}
	state := store.sessions[id]
	if state.eventsLoaded || len(state.events) > 1 || state.eventIndex == nil || len(state.eventIndex.offsets) != 10 {
		t.Fatalf("unbounded history retained: loaded=%t events=%d index=%+v", state.eventsLoaded, len(state.events), state.eventIndex)
	}
}

func TestLargeHistoryAppendMergeProjectionAndArchive(t *testing.T) {
	store, id := largeHistoryStore(t)
	if _, err := store.EventsAfter(id, 0, 1); err != nil {
		t.Fatal(err)
	}
	before := store.Diagnostics().HistoryLoads
	appendProjectionEvent(t, store, id, EventMessageInput, "turn_large", MessageInput{
		Text: "complete history", Role: MessageRoleUser, MessageID: "large-input",
		Payload: json.RawMessage(`{"opaque":"retained"}`),
	})
	if store.Diagnostics().HistoryLoads != before {
		t.Fatal("ordinary append materialized complete history")
	}
	appendProjectionEvent(t, store, id, "turn.started", "turn_large", nil)
	first := appendProjectionEvent(t, store, id, "message.assistant.delta", "turn_large", map[string]string{"text": "first "})
	merged := appendProjectionEvent(t, store, id, "message.assistant.delta", "turn_large", map[string]string{"text": "second"})
	if first.ID != merged.ID {
		t.Fatal("tail eviction split a mergeable event")
	}
	for index := 0; index < 130; index++ {
		appendProjectionEvent(t, store, id, "provider.metadata", "turn_large", map[string]string{"text": strings.Repeat("v", index+1)})
	}
	appendProjectionEvent(t, store, id, EventTurnCompleted, "turn_large", nil)
	turn, err := store.Turn(id, "turn_large")
	if err != nil || turn.FinalReplyPreview != "first second" || turn.TriggerMessageID != "large-input" || !turn.Closed {
		t.Fatalf("turn = %+v, %v", turn, err)
	}
	messages, err := store.DurableMessages(id)
	if err != nil || len(messages) != 1 || messages[0].Input.Text != "complete history" || !messages[0].Terminal {
		t.Fatalf("messages = %+v, %v", messages, err)
	}
	if store.sessions[id].eventsLoaded || len(store.sessions[id].events) != 1 {
		t.Fatal("projection or message query retained a large history")
	}
	if store.Diagnostics().HistoryLoads != before {
		t.Fatal("projection or message query materialized old history")
	}
	value, _ := store.Get(id)
	page, err := store.EventsPageBefore(id, value.LastEventID+1, 140)
	if err != nil || page.Events[len(page.Events)-1].ID != value.LastEventID {
		t.Fatalf("index after append/merge = %+v, %v", page, err)
	}
	appendProjectionEvent(t, store, id, "session.state", "", StateEventData{State: StateStopped})
	if _, err := store.Archive(id); err != nil {
		t.Fatal(err)
	}
	archivedEvents, err := store.EventsAfter(id, merged.ID-1, 1)
	if err != nil || len(archivedEvents) != 1 || !bytes.Contains(archivedEvents[0].Data, []byte("first second")) {
		t.Fatalf("archived events = %+v, %v", archivedEvents, err)
	}
	reopened, err := Open(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := reopened.Turn(id, "turn_large")
	if err != nil || recovered.FinalReplyPreview != turn.FinalReplyPreview || recovered.TriggerMessageID != turn.TriggerMessageID {
		t.Fatalf("recovered = %+v, %v", recovered, err)
	}
}

func TestAppendToTrustedLargeHistoryOnlyLoadsTail(t *testing.T) {
	store, id := largeHistoryStore(t)
	appendProjectionEvent(t, store, id, "provider.metadata", "", map[string]string{"text": "tail"})
	if store.Diagnostics().HistoryLoads != 0 || store.sessions[id].eventsLoaded || len(store.sessions[id].events) != 1 {
		t.Fatal("first append loaded old history")
	}
	events, err := store.EventsAfter(id, 1152, 10)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs(t, events, 1153, 1154)
}

func TestStreamingHistoryRepairsTailAndRejectsInteriorCorruption(t *testing.T) {
	for _, suffix := range []string{"partial", strings.Repeat("x", 131072)} {
		t.Run("torn-tail", func(t *testing.T) {
			store, id := largeHistoryStore(t)
			path := store.eventsPath(id)
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString(suffix); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			events, err := readEventsRepairTail(path)
			if err != nil || len(events) != 1153 {
				t.Fatalf("tail repair: %d events, %v", len(events), err)
			}
			page, err := store.EventsPageBefore(id, 1154, 2)
			if err != nil {
				t.Fatal(err)
			}
			assertIDs(t, page.Events, 1152, 1153)
		})
	}
	store, id := largeHistoryStore(t)
	path := filepath.Join(store.Root(), id, "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"id":500,`), []byte(`"id":501,`), 1)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EventsAfter(id, 0, 1); err == nil || !strings.Contains(err.Error(), "non-contiguous") {
		t.Fatalf("interior corruption was hidden: %v", err)
	}
}

func TestSmallHistoryCacheReleasesOnStop(t *testing.T) {
	root := t.TempDir()
	id := memoryHistoryFixture(t, root, 32, 4096)
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EventsAfter(id, 0, 100); err != nil {
		t.Fatal(err)
	}
	if !store.sessions[id].eventsLoaded || store.Diagnostics().CachedEventPayloadBytes < 32*4096 {
		t.Fatal("small active history was not cached")
	}
	appendProjectionEvent(t, store, id, "session.state", "", StateEventData{State: StateStopped})
	if store.sessions[id].eventsLoaded || store.Diagnostics().CachedEventPayloadBytes > 100 {
		t.Fatalf("stopped history retained: %+v", store.Diagnostics())
	}
}

func TestLargeHistoryReplayKeepsOnlyBoundedCache(t *testing.T) {
	store, id := largeHistoryStore(t)
	if err := os.Remove(store.snapshotPath(id)); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	state := reopened.sessions[id]
	value, err := reopened.Get(id)
	if err != nil || value.LastEventID != 1153 || value.Title != "Synthetic memory workload" {
		t.Fatalf("replayed metadata = %+v, %v", value, err)
	}
	if state.eventsLoaded || len(state.events) != 1 || state.eventCacheBytes > eventHistorySessionBudget {
		t.Fatal("fallback replay retained all events")
	}
	if reopened.Diagnostics().FallbackReplays != 1 || reopened.Diagnostics().HistoryLoads != 1 {
		t.Fatalf("replay diagnostics = %+v", reopened.Diagnostics())
	}
}

func TestHistoryCacheStoreBudget(t *testing.T) {
	root := t.TempDir()
	ids := make([]string, 32)
	for index := range ids {
		ids[index] = memoryHistoryFixture(t, root, 80, 16384)
	}
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := store.EventsAfter(id, 0, 1); err != nil {
			t.Fatal(err)
		}
		if bytes := store.Diagnostics().CachedEventBytes; bytes > eventHistoryStoreBudget {
			t.Fatalf("cache budget exceeded: %d", bytes)
		}
	}
	for _, id := range ids {
		events, err := store.EventsAfter(id, 79, 10)
		if err != nil {
			t.Fatal(err)
		}
		assertIDs(t, events, 80, 81)
	}
}

func TestEventIndexRebuildsAfterReplacementAndRejectsTruncation(t *testing.T) {
	store, id := largeHistoryStore(t)
	if _, err := store.EventsAfter(id, 0, 1); err != nil {
		t.Fatal(err)
	}
	before := store.Diagnostics().EventIndexBuilds
	path := store.eventsPath(id)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(strings.Repeat("x", 4096)), []byte(strings.Repeat("y", 4096)), 1)
	replacement := filepath.Join(t.TempDir(), "replacement.jsonl")
	if err := os.WriteFile(replacement, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	events, err := store.EventsAfter(id, 1, 1)
	if err != nil || len(events) != 1 || !bytes.Contains(events[0].Data, []byte("yyyy")) {
		t.Fatalf("replaced history = %+v, %v", events, err)
	}
	if store.Diagnostics().EventIndexBuilds != before+1 {
		t.Fatal("replaced file reused its old index")
	}
	if err := os.Truncate(path, int64(bytes.IndexByte(data, '\n')+1)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EventsAfter(id, 0, 1); err == nil {
		t.Fatal("truncated log reused old offsets")
	}
}

func TestTurnOffsetRangeFiltersInterleavedEvents(t *testing.T) {
	store, id := largeHistoryStore(t)
	appendProjectionEvent(t, store, id, EventMessageInput, "turn_first", MessageInput{Text: "first"})
	appendProjectionEvent(t, store, id, "turn.started", "turn_first", nil)
	appendProjectionEvent(t, store, id, EventMessageInput, "turn_other", MessageInput{Text: "other"})
	appendProjectionEvent(t, store, id, "provider.metadata", "", map[string]string{"text": "unassigned"})
	appendProjectionEvent(t, store, id, "message.assistant.delta", "turn_first", map[string]string{"text": "first answer"})
	appendProjectionEvent(t, store, id, EventTurnCompleted, "turn_first", nil)
	turns, err := store.buildStoredTurnsLocked(store.sessions[id], id, "turn_first")
	if err != nil || len(turns) != 1 || turns[0].TriggerPreview != "first" || turns[0].FinalReplyPreview != "first answer" {
		t.Fatalf("interleaved turn = %+v, %v", turns, err)
	}
	for _, item := range turns[0].Items {
		if item.Text == "other" || item.Text == "unassigned" {
			t.Fatal("offset range included a different turn")
		}
	}
}

func TestIndexedHistoryConcurrentAppendAndReplay(t *testing.T) {
	store, id := largeHistoryStore(t)
	subscription, highWater, err := store.Subscribe(id)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for index := 0; index < 32; index++ {
			if _, err := store.Append(id, "provider.metadata", "", []byte(`{"text":"concurrent"}`)); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for index := 0; index < 32; index++ {
			events, err := store.EventsThrough(id, highWater-2, highWater, 10)
			if err != nil || len(events) != 2 || events[1].ID != highWater {
				t.Errorf("high-water replay: %v, %v", events, err)
				return
			}
		}
	}()
	workers.Wait()
	for index := 0; index < 32; index++ {
		select {
		case event := <-subscription.Events():
			if event.ID != highWater+int64(index)+1 {
				t.Fatalf("live cursor %d: %d", index, event.ID)
			}
		default:
			t.Fatal("live append was lost")
		}
	}
	page, err := store.EventsPageBefore(id, highWater+33, 32)
	if err != nil || len(page.Events) != 32 || page.Events[0].ID != highWater+1 {
		t.Fatalf("durable catch-up: %+v, %v", page, err)
	}
}
