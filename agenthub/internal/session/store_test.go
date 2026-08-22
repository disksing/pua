package session

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStorePersistsOneContinuousEventLog(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{
		Title:     "Fix login",
		Cwd:       t.TempDir(),
		AgentName: "Codex Build",
	})
	if err != nil {
		t.Fatal(err)
	}
	turnID := "turn_test"
	if _, err := store.Append(created.ID, "turn.started", turnID, nil); err != nil {
		t.Fatal(err)
	}
	approval, _ := json.Marshal(ApprovalEventData{ApprovalID: "apr_test"})
	if _, err := store.Append(created.ID, "approval.requested", turnID, approval); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, "approval.resolved", turnID, approval); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, "turn.completed", turnID, nil); err != nil {
		t.Fatal(err)
	}

	value, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.State != StateReady || value.CurrentTurnID != "" || len(value.PendingApprovalIDs) != 0 {
		t.Fatalf("unexpected projection: %+v", value)
	}
	sessionDir := filepath.Join(root, created.ID)
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected session.json, events.jsonl, and turns.jsonl, got %d entries", len(entries))
	}
	for _, name := range []string{"session.json", "events.jsonl", "turns.jsonl"} {
		if _, err := os.Stat(filepath.Join(sessionDir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.LastEventID != 5 || reloaded.State != StateReady {
		t.Fatalf("unexpected reloaded projection: %+v", reloaded)
	}
	events, err := reopened.EventsAfter(created.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 || events[1].Type != "turn.started" || events[4].Type != "turn.completed" {
		t.Fatalf("unexpected events: %+v", events)
	}
}

func TestSessionLastActivityTracksSemanticTurnEventsOnly(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Title: "activity", Cwd: t.TempDir(), AgentName: "Codex"})
	if err != nil {
		t.Fatal(err)
	}
	turnID := "turn_activity"
	started, err := store.Append(created.ID, "turn.started", turnID, nil)
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.Get(created.ID)
	if err != nil || value.LastActivityAt == nil || !value.LastActivityAt.Equal(started.Time) || value.LastActivityTurnID != turnID {
		t.Fatalf("turn start activity = %#v, %v", value, err)
	}
	if _, err := store.Append(created.ID, "provider.stderr", "", []byte(`{"text":"still noisy"}`)); err != nil {
		t.Fatal(err)
	}
	unchanged, err := store.Get(created.ID)
	if err != nil || unchanged.LastActivityAt == nil || !unchanged.LastActivityAt.Equal(started.Time) {
		t.Fatalf("provider stderr refreshed activity: %#v, %v", unchanged, err)
	}
	delta, err := store.Append(created.ID, "message.assistant.delta", turnID, []byte(`{"text":"working"}`))
	if err != nil {
		t.Fatal(err)
	}
	value, err = store.Get(created.ID)
	if err != nil || value.LastActivityAt == nil || !value.LastActivityAt.Equal(delta.Time) {
		t.Fatalf("assistant activity = %#v, %v", value, err)
	}
	if _, err := store.Append(created.ID, "session.state", "", []byte(`{"state":"stopping"}`)); err != nil {
		t.Fatal(err)
	}
	unchanged, err = store.Get(created.ID)
	if err != nil || unchanged.LastActivityAt == nil || !unchanged.LastActivityAt.Equal(delta.Time) {
		t.Fatalf("session lifecycle refreshed activity: %#v, %v", unchanged, err)
	}
}

func TestSubscribeAllObservesEverySessionAfterPersistence(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Create(CreateInput{Title: "first", Cwd: t.TempDir(), AgentName: "Codex"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(CreateInput{Title: "second", Cwd: t.TempDir(), AgentName: "Kimi"})
	if err != nil {
		t.Fatal(err)
	}
	live := store.SubscribeAll()
	defer live.Cancel()
	if _, err := store.Append(first.ID, "provider.event", "", []byte(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(second.ID, EventTurnCompleted, "turn-1", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{first.ID, second.ID} {
		select {
		case event := <-live.Events():
			if event.SessionID != want {
				t.Fatalf("event session = %q, want %q", event.SessionID, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s", want)
		}
	}
	if live.Overflowed() {
		t.Fatal("small activity feed overflowed")
	}
}

func TestSubscribeAllOverflowIsTerminal(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	live := store.SubscribeAll()
	defer live.Cancel()
	for i := 0; i <= subscriptionBuffer; i++ {
		store.publish(Event{SessionID: "ses_busy", ID: int64(i + 1), Type: "provider.event"})
	}
	if !live.Overflowed() {
		t.Fatal("activity subscription did not report overflow")
	}
	select {
	case <-live.Overflow():
	case <-time.After(time.Second):
		t.Fatal("overflowed activity subscription did not terminate")
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if _, subscribed := store.activitySubscribers[live]; subscribed {
		t.Fatal("overflowed activity subscription remained registered")
	}
}

func TestStorePersistsLaunchEnvironmentThroughReplayWithPrivateFiles(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]string{"SESSION_CONTEXT_ID": "context-one", "EMPTY": ""}
	created, err := store.Create(CreateInput{
		Title:             "Environment",
		Cwd:               t.TempDir(),
		AgentName:         "Codex",
		LaunchEnvironment: input,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The store owns a deep copy: callers cannot change persisted session
	// state by retaining and mutating their request map.
	input["SESSION_CONTEXT_ID"] = "mutated"
	if created.LaunchEnvironment["SESSION_CONTEXT_ID"] != "context-one" {
		t.Fatalf("created environment was aliased: %+v", created.LaunchEnvironment)
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.LaunchEnvironment["SESSION_CONTEXT_ID"] != "context-one" {
		t.Fatalf("launch environment did not survive event replay: %+v", replayed)
	}
	replayed.LaunchEnvironment["SESSION_CONTEXT_ID"] = "caller-mutation"
	unchanged, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.LaunchEnvironment["SESSION_CONTEXT_ID"] != "context-one" {
		t.Fatalf("Get returned an aliased launch environment: %+v", unchanged)
	}

	for _, name := range []string{"events.jsonl", "session.json"} {
		info, err := os.Stat(filepath.Join(root, created.ID, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s permissions = %o, want 600", name, info.Mode().Perm())
		}
	}
}

func TestStoreRejectsInvalidLaunchEnvironment(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, environment := range []map[string]string{
		{"": "value"},
		{"BAD=NAME": "value"},
		{"BAD\x00NAME": "value"},
		{"NAME": "bad\x00value"},
	} {
		if _, err := store.Create(CreateInput{Cwd: t.TempDir(), LaunchEnvironment: environment}); err == nil {
			t.Fatalf("accepted invalid environment: %#v", environment)
		}
	}
}

func TestUpdateLaunchEnvironmentOverlaysAndSurvivesReplay(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{
		Cwd:               t.TempDir(),
		AgentName:         "Codex",
		LaunchEnvironment: map[string]string{"SESSION_CONTEXT_ID": "context-old", "KEEP": "original"},
	})
	if err != nil {
		t.Fatal(err)
	}
	overlay := map[string]string{"SESSION_CONTEXT_ID": "context-new", "ADDED": "yes"}
	updated, err := store.UpdateLaunchEnvironment(created.ID, overlay)
	if err != nil {
		t.Fatal(err)
	}
	// The overlay replaces same-named entries and keeps the rest; it never
	// deletes keys it does not mention.
	want := map[string]string{"SESSION_CONTEXT_ID": "context-new", "KEEP": "original", "ADDED": "yes"}
	if !maps.Equal(updated.LaunchEnvironment, want) {
		t.Fatalf("merged environment = %+v, want %+v", updated.LaunchEnvironment, want)
	}
	// The store owns a deep copy of the overlay.
	overlay["SESSION_CONTEXT_ID"] = "mutated"
	unchanged, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(unchanged.LaunchEnvironment, want) {
		t.Fatalf("overlay was aliased: %+v", unchanged.LaunchEnvironment)
	}

	// The durable event carries the full merged map; session.created keeps
	// the original environment untouched.
	events, err := store.EventsAfter(created.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Type != "session.launch-environment" {
		t.Fatalf("unexpected events: %+v", events)
	}
	var createdData struct {
		LaunchEnvironment map[string]string `json:"launchEnvironment"`
	}
	if err := json.Unmarshal(events[0].Data, &createdData); err != nil {
		t.Fatal(err)
	}
	if createdData.LaunchEnvironment["SESSION_CONTEXT_ID"] != "context-old" {
		t.Fatalf("session.created was rewritten: %+v", createdData.LaunchEnvironment)
	}
	var environmentData LaunchEnvironmentEventData
	if err := json.Unmarshal(events[1].Data, &environmentData); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(environmentData.Environment, want) {
		t.Fatalf("event payload = %+v, want %+v", environmentData.Environment, want)
	}

	// A second overlay merges onto the newest environment.
	again, err := store.UpdateLaunchEnvironment(created.ID, map[string]string{"ADDED": "updated"})
	if err != nil {
		t.Fatal(err)
	}
	want["ADDED"] = "updated"
	if !maps.Equal(again.LaunchEnvironment, want) {
		t.Fatalf("second overlay = %+v, want %+v", again.LaunchEnvironment, want)
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(replayed.LaunchEnvironment, want) {
		t.Fatalf("environment after replay = %+v, want %+v", replayed.LaunchEnvironment, want)
	}
}

func TestUpdateLaunchEnvironmentRejectsInvalidOverlayAndArchived(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{
		Cwd:               t.TempDir(),
		AgentName:         "Codex",
		LaunchEnvironment: map[string]string{"KEEP": "original"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, overlay := range []map[string]string{
		{"": "value"},
		{"BAD=NAME": "value"},
		{"NAME": "bad\x00value"},
	} {
		if _, err := store.UpdateLaunchEnvironment(created.ID, overlay); err == nil {
			t.Fatalf("accepted invalid overlay: %#v", overlay)
		}
	}
	value, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.LastEventID != 1 || !maps.Equal(value.LaunchEnvironment, map[string]string{"KEEP": "original"}) {
		t.Fatalf("invalid overlay changed the session: %+v", value)
	}
	if _, err := store.UpdateLaunchEnvironment("ses_missing", map[string]string{"A": "b"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session error = %v, want ErrNotFound", err)
	}

	// Archived sessions are read-only.
	if _, err := store.Append(created.ID, "session.state", "", []byte(`{"state":"stopped","reason":"requested"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Archive(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateLaunchEnvironment(created.ID, map[string]string{"A": "b"}); !errors.Is(err, ErrArchived) {
		t.Fatalf("archived update error = %v, want ErrArchived", err)
	}
	archived, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(archived.LaunchEnvironment, map[string]string{"KEEP": "original"}) {
		t.Fatalf("archived environment changed: %+v", archived.LaunchEnvironment)
	}
}

func TestSessionSourcePersistsAndFilters(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	create := func(title string, source *Source) Session {
		t.Helper()
		value, err := store.Create(CreateInput{
			Title: title, Cwd: t.TempDir(), AgentName: "Agent", Source: source,
		})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	puaOne := create("pua one", &Source{App: "pua", InstanceID: "mac-1", ExternalID: "task-1"})
	puaDuplicate := create("pua duplicate", &Source{App: "pua", InstanceID: "mac-1", ExternalID: "task-1"})
	puaTwo := create("pua two", &Source{App: "pua", InstanceID: "mac-2", ExternalID: "task-2"})
	other := create("other app", &Source{App: "other", InstanceID: "mac-1", ExternalID: "task-1"})
	_ = create("legacy", nil)

	if _, err := store.Append(puaTwo.ID, "session.state", "", mustJSON(t, StateEventData{State: StateStopped})); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(puaDuplicate.ID, "session.state", "", mustJSON(t, StateEventData{
		State: StateStopped, Reason: StopReasonRequested,
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Archive(puaDuplicate.ID); err != nil {
		t.Fatal(err)
	}

	app, instance, external := "pua", "mac-1", "task-1"
	assertIDs := func(name string, filter ListFilter, want ...string) {
		t.Helper()
		values := store.Filter(filter)
		got := make(map[string]bool, len(values))
		for _, value := range values {
			got[value.ID] = true
		}
		if len(got) != len(want) {
			t.Fatalf("%s: ids = %v, want %v", name, got, want)
		}
		for _, id := range want {
			if !got[id] {
				t.Fatalf("%s: missing %s in %v", name, id, got)
			}
		}
	}
	assertIDs("app", ListFilter{SourceApp: &app}, puaOne.ID, puaTwo.ID)
	assertIDs("instance", ListFilter{SourceInstanceID: &instance}, puaOne.ID, other.ID)
	assertIDs("external", ListFilter{SourceExternalID: &external}, puaOne.ID, other.ID)
	assertIDs("combination", ListFilter{
		SourceApp: &app, SourceInstanceID: &instance, SourceExternalID: &external,
	}, puaOne.ID)
	assertIDs("combination including archived", ListFilter{
		IncludeArchived: true, SourceApp: &app, SourceInstanceID: &instance, SourceExternalID: &external,
	}, puaOne.ID, puaDuplicate.ID)

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{puaOne.ID, puaDuplicate.ID} {
		value, err := reopened.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if value.Source == nil || !reflect.DeepEqual(*value.Source, Source{App: "pua", InstanceID: "mac-1", ExternalID: "task-1"}) {
			t.Fatalf("replayed source for %s = %+v", id, value.Source)
		}
	}
	events, err := reopened.EventsAfter(puaOne.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var created Session
	if err := json.Unmarshal(events[0].Data, &created); err != nil {
		t.Fatal(err)
	}
	if created.Source == nil || created.Source.App != "pua" {
		t.Fatalf("session.created source = %+v", created.Source)
	}
}

func TestStoreRepairsPartialTailAndRebuildsSnapshot(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Title: "Recover", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join(root, created.ID, "events.jsonl")
	file, err := os.OpenFile(eventPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"id":2,"type":"turn.started"`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, created.ID, "session.json")); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	value, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.LastEventID != 1 || value.State != StateReady {
		t.Fatalf("unexpected rebuilt snapshot: %+v", value)
	}
	data, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatalf("event log was not repaired: %q", data)
	}
}

func TestStoreRejectsCorruptCompleteRecord(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Title: "Corrupt", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join(root, created.ID, "events.jsonl")
	file, err := os.OpenFile(eventPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("not-json\n"); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()

	if _, err := Open(root); err == nil {
		t.Fatal("expected a corrupt complete event record to fail")
	}
}

func TestSubscribeReceivesNewEvents(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Title: "Stream", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	subscription, highWater, err := store.Subscribe(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()
	if highWater != 1 {
		t.Fatalf("high-water mark = %d, want 1", highWater)
	}
	if _, err := store.Append(created.ID, "session.state", "", mustJSON(t, StateEventData{State: StateStopped})); err != nil {
		t.Fatal(err)
	}
	event := <-subscription.Events()
	if event.ID != 2 || event.Type != "session.state" {
		t.Fatalf("unexpected live event: %+v", event)
	}
}

func TestSubscriptionOverflowIsTerminal(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Title: "Overflow", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	subscription, _, err := store.Subscribe(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Cancel()
	for i := 0; i <= subscriptionBuffer; i++ {
		store.publish(Event{SessionID: created.ID, ID: int64(i + 2), Type: "provider.test"})
	}
	select {
	case <-subscription.Overflow():
	case <-time.After(5 * time.Second):
		t.Fatal("subscription did not report overflow")
	}
	if !subscription.Overflowed() {
		t.Fatal("subscription must remain terminal after overflow")
	}
}

func TestRejectedEventDoesNotConsumeOrReuseDurableCursor(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Title: "Cursor", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, "session.state", "", []byte(`{"state":`)); err == nil {
		t.Fatal("expected invalid projection data to be rejected")
	}
	event, err := store.Append(created.ID, "provider.future", "", mustJSON(t, map[string]bool{"ok": true}))
	if err != nil {
		t.Fatal(err)
	}
	if event.ID != 2 {
		t.Fatalf("next durable id = %d, want 2", event.ID)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reopened.EventsAfter(created.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Type != "provider.future" {
		t.Fatalf("durable events = %+v", events)
	}
}

func TestProviderMappingIsProjectedAndRebuilt(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Title: "Mapped", Cwd: t.TempDir(), AgentName: "Codex Fast"})
	if err != nil {
		t.Fatal(err)
	}
	data := ProviderEventData{
		AgentName: "Codex Fast", Provider: "codex", ProviderSessionID: "native-1",
	}
	if _, err := store.Append(created.ID, "session.provider", "", mustJSON(t, data)); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	value, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.AgentName != "Codex Fast" || value.ProviderSessionID != "native-1" {
		t.Fatalf("unexpected projection: %+v", value)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func archiveTestSession(t *testing.T, store *Store) Session {
	t.Helper()
	created, err := store.Create(CreateInput{Title: "Archive me", Cwd: t.TempDir(), AgentName: "Agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, "session.state", "", mustJSON(t, StateEventData{State: StateStopped})); err != nil {
		t.Fatal(err)
	}
	return created
}

func TestArchiveMovesWholeSessionDirectory(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created := archiveTestSession(t, store)
	activeDir := filepath.Join(root, created.ID)
	before, err := os.ReadDir(activeDir)
	if err != nil {
		t.Fatal(err)
	}

	archived, err := store.Archive(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if archived.State != StateArchived {
		t.Fatalf("state = %q, want archived", archived.State)
	}
	if _, err := os.Stat(activeDir); !os.IsNotExist(err) {
		t.Fatalf("active directory still exists: %v", err)
	}
	archiveDir := filepath.Join(root, ArchiveDirName, created.ID)
	after, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("archived files = %d, want %d", len(after), len(before))
	}
	for _, name := range []string{"session.json", "events.jsonl"} {
		data, err := os.ReadFile(filepath.Join(archiveDir, name))
		if err != nil {
			t.Fatalf("missing archived %s: %v", name, err)
		}
		if len(data) == 0 {
			t.Fatalf("archived %s is empty", name)
		}
		info, err := os.Stat(filepath.Join(archiveDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("archived %s permissions = %o, want 600", name, info.Mode().Perm())
		}
	}

	// Hidden by default, visible with includeArchived, readable by ID.
	for _, value := range store.List(false) {
		if value.ID == created.ID {
			t.Fatal("archived session appears in the default list")
		}
	}
	found := false
	for _, value := range store.List(true) {
		if value.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("archived session missing from the includeArchived list")
	}
	events, err := store.EventsAfter(created.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if events[len(events)-1].Type != "session.archived" {
		t.Fatalf("last event = %q, want session.archived", events[len(events)-1].Type)
	}
}

func TestArchiveRejectsEveryNonStoppedState(t *testing.T) {
	for _, state := range []string{StateReady, StateStarting, StateRunning, StateWaitingApproval, StateStopping} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			store, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			created := archiveTestSession(t, store)
			if _, err := store.Append(created.ID, "session.state", "", mustJSON(t, StateEventData{State: state})); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Archive(created.ID); !errors.Is(err, ErrSessionActive) {
				t.Fatalf("Archive error = %v, want ErrSessionActive", err)
			}
			if _, err := os.Stat(filepath.Join(root, created.ID)); err != nil {
				t.Fatalf("session directory lost after rejected archive: %v", err)
			}
			value, err := store.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if value.State != state {
				t.Fatalf("state changed to %q after rejected archive", value.State)
			}
		})
	}

	// An open turn blocks archiving even in an otherwise quiet state.
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created := archiveTestSession(t, store)
	if _, err := store.Append(created.ID, "turn.started", "turn_open", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Archive(created.ID); !errors.Is(err, ErrSessionActive) {
		t.Fatalf("Archive error = %v, want ErrSessionActive", err)
	}
}

func TestStoppedReasonProjectsAndMissingReasonDefaults(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Cwd: t.TempDir(), AgentName: "Agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, "session.state", "", mustJSON(t, StateEventData{
		State: StateStopped, Reason: StopReasonProviderError,
	})); err != nil {
		t.Fatal(err)
	}
	value, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.State != StateStopped || value.StopReason != StopReasonProviderError {
		t.Fatalf("new stopped event did not project its reason: %+v", value)
	}
	if _, err := store.Append(created.ID, "session.state", "", mustJSON(t, map[string]any{"state": StateReady})); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, "session.state", "", mustJSON(t, map[string]any{"state": StateStopped})); err != nil {
		t.Fatal(err)
	}
	value, err = store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.State != StateStopped || value.StopReason != "" {
		t.Fatalf("stopped event without reason is incompatible: %+v", value)
	}
}

func TestOpenProviderProcessUsesStoppedAsClosureBoundary(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Cwd: t.TempDir(), AgentName: "Agent"})
	if err != nil {
		t.Fatal(err)
	}
	process := ProviderProcessEventData{PID: 123, ProcessGroupID: 123}
	if _, err := store.Append(created.ID, "provider.process.started", "", mustJSON(t, process)); err != nil {
		t.Fatal(err)
	}
	got, open, err := store.OpenProviderProcess(created.ID)
	if err != nil || !open || got != process {
		t.Fatalf("open process = %+v, %v, %v", got, open, err)
	}
	if _, err := store.Append(created.ID, "session.state", "", mustJSON(t, StateEventData{
		State: StateStopped, Reason: StopReasonCompleted,
	})); err != nil {
		t.Fatal(err)
	}
	if _, open, err := store.OpenProviderProcess(created.ID); err != nil || open {
		t.Fatalf("stopped did not close process evidence: open=%v err=%v", open, err)
	}
}

func TestArchiveUnknownAndInvalidIDs(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Archive("ses_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Archive error = %v, want ErrNotFound", err)
	}
	for _, id := range []string{"", "../etc", "ses_..", "foo", "ses_a/b", "ses_a%2f.."} {
		if _, err := store.Archive(id); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("Archive(%q) error = %v, want ErrInvalidID", id, err)
		}
	}
}

func TestArchiveIsIdempotentAndBlocksWrites(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created := archiveTestSession(t, store)
	first, err := store.Archive(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Archive(created.ID)
	if err != nil {
		t.Fatalf("repeat archive failed: %v", err)
	}
	if second.LastEventID != first.LastEventID {
		t.Fatalf("repeat archive appended events: %d -> %d", first.LastEventID, second.LastEventID)
	}
	if _, err := store.Append(created.ID, "session.state", "", mustJSON(t, StateEventData{State: StateReady})); !errors.Is(err, ErrArchived) {
		t.Fatalf("Append on archived session = %v, want ErrArchived", err)
	}
}

func TestArchiveTargetConflictKeepsDataAndRetries(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created := archiveTestSession(t, store)
	// Force the rename to fail: a file occupies the archive target path.
	conflict := filepath.Join(root, ArchiveDirName, created.ID)
	if err := os.WriteFile(conflict, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Archive(created.ID); !errors.Is(err, ErrArchiveConflict) {
		t.Fatalf("Archive error = %v, want ErrArchiveConflict", err)
	}
	// No data was lost: the session directory is still in the active area.
	activeDir := filepath.Join(root, created.ID)
	if _, err := os.Stat(filepath.Join(activeDir, "events.jsonl")); err != nil {
		t.Fatalf("session data lost after failed archive: %v", err)
	}
	// Removing the conflict lets a retry finish the move.
	if err := os.Remove(conflict); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Archive(created.ID); err != nil {
		t.Fatalf("archive retry failed: %v", err)
	}
	if _, err := os.Stat(activeDir); !os.IsNotExist(err) {
		t.Fatalf("active directory still exists after retry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ArchiveDirName, created.ID, "events.jsonl")); err != nil {
		t.Fatalf("archived events missing after retry: %v", err)
	}
}

func TestOpenCompletesInterruptedArchive(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created := archiveTestSession(t, store)
	// Simulate a crash between the archived event and the directory move.
	if _, err := store.Append(created.ID, "session.archived", "", nil); err != nil {
		t.Fatal(err)
	}
	store = nil

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, created.ID)); !os.IsNotExist(err) {
		t.Fatalf("interrupted archive was not completed: %v", err)
	}
	value, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.State != StateArchived {
		t.Fatalf("state = %q, want archived", value.State)
	}
	if _, err := os.Stat(filepath.Join(root, ArchiveDirName, created.ID, "events.jsonl")); err != nil {
		t.Fatalf("archived events missing after recovery: %v", err)
	}
	for _, item := range reopened.List(false) {
		if item.ID == created.ID {
			t.Fatal("recovered archived session appears in the default list")
		}
	}
}

func TestOpenLoadsArchivedSessionsAndIgnoresArchiveDir(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created := archiveTestSession(t, store)
	if _, err := store.Archive(created.ID); err != nil {
		t.Fatal(err)
	}
	store = nil

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	value, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.State != StateArchived || value.Title != "Archive me" {
		t.Fatalf("unexpected archived session: %+v", value)
	}
	events, err := reopened.EventsAfter(created.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if events[len(events)-1].Type != "session.archived" {
		t.Fatalf("last event = %q, want session.archived", events[len(events)-1].Type)
	}
	if len(reopened.List(false)) != 0 {
		t.Fatalf("default list is not empty: %+v", reopened.List(false))
	}
}

func TestOpenRejectsSessionDuplicatedInArchive(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created := archiveTestSession(t, store)
	if _, err := store.Archive(created.ID); err != nil {
		t.Fatal(err)
	}
	// Copy the archived directory back into the active area.
	copyDir(t, filepath.Join(root, ArchiveDirName, created.ID), filepath.Join(root, created.ID))
	store = nil
	if _, err := Open(root); err == nil {
		t.Fatal("expected Open to reject a session present in both areas")
	}
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(src, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, entry.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A session.agent event (appended when a configured agent is renamed)
// re-points the projection at the new name.
func TestAgentRenameEventUpdatesProjection(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Title: "Rename me", Cwd: t.TempDir(), AgentName: "Codex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, "session.agent", "", mustJSON(t, AgentRenameEventData{AgentName: "Codex X"})); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	value, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.AgentName != "Codex X" {
		t.Fatalf("rename event did not update the projection: %+v", value)
	}
}

// New session.created snapshots write the current agentName field.
func TestCreatedEventWritesAgentNameOnly(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Title: "Fresh", Cwd: t.TempDir(), AgentName: "Codex"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, created.ID, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"agentName":"Codex"`) || strings.Contains(string(data), "agentId") {
		t.Fatalf("created event must write agentName only: %s", data)
	}
}
