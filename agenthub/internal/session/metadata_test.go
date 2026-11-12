package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestMetadataReadsAndSubscriptionDoNotWaitForHistoryLock(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Source: &Source{App: "pua"}})
	if err != nil {
		t.Fatal(err)
	}
	state := store.sessions[created.ID]
	state.mu.Lock()
	defer state.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		if _, err := store.Get(created.ID); err != nil {
			done <- err
			return
		}
		if len(store.Filter(ListFilter{})) != 1 {
			done <- errors.New("missing metadata")
			return
		}
		sub, highWater, err := store.Subscribe(created.ID)
		if err == nil {
			sub.Cancel()
		}
		if highWater != created.LastEventID {
			err = fmt.Errorf("high water = %d, want %d", highWater, created.LastEventID)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("metadata read or Subscribe waited for the history lock")
	}
}

func TestMetadataPublicationFollowsDurableLogDespiteSnapshotFailure(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{})
	if err != nil {
		t.Fatal(err)
	}
	path := store.snapshotPath(created.ID)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, "turn.started", "turn_durable", nil); err == nil {
		t.Fatal("expected snapshot failure")
	}
	value, err := store.Get(created.ID)
	if err != nil || value.CurrentTurnID != "turn_durable" || value.LastEventID != created.LastEventID+1 {
		t.Fatalf("durable metadata = %+v, err=%v", value, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := reopened.Get(created.ID)
	if err != nil || !reflect.DeepEqual(value, replayed) {
		t.Fatalf("replay = %+v, metadata = %+v, err=%v", replayed, value, err)
	}
	// A log write failure must not publish an uncommitted update.
	if err := os.Remove(filepath.Join(store.Root(), created.ID, "events.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store.Root(), created.ID, "events.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, EventTurnCompleted, "turn_durable", nil); err == nil {
		t.Fatal("expected log failure")
	}
	after, err := store.Get(created.ID)
	if err != nil || !reflect.DeepEqual(after, value) {
		t.Fatalf("failed log changed metadata: %+v, err=%v", after, err)
	}
}

func metadataFixture(count int) *Store {
	store := &Store{sessions: make(map[string]*sessionState)}
	for index := 0; index < count; index++ {
		id := fmt.Sprintf("ses_%08d", index)
		state := &sessionState{session: Session{
			ID: id, State: StateReady, UpdatedAt: time.Unix(100, 0),
			Source:            &Source{App: "pua", Metadata: map[string]string{"key": "value"}},
			LaunchEnvironment: map[string]string{"PATH": "/bin"},
		}}
		state.publishMetadata()
		store.sessions[id] = state
	}
	return store
}

func TestMetadataQueryFiltersAndPagesBeforeCopying(t *testing.T) {
	store := metadataFixture(6)
	store.sessions["ses_00000005"].session.State = StateArchived
	store.sessions["ses_00000005"].publishMetadata()
	store.sessions["ses_00000004"].session.Source.App = "other"
	store.sessions["ses_00000004"].publishMetadata()
	app := "pua"
	filter := ListFilter{IncludeArchived: true, SourceApp: &app}
	options := ListOptions{States: []string{StateReady}, Limit: 2}
	page, err := store.Query(context.Background(), filter, options)
	if err != nil || len(page.Sessions) != 2 || !page.HasMore || page.Sessions[0].ID != "ses_00000003" {
		t.Fatalf("first page = %+v, err=%v", page, err)
	}
	last := page.Sessions[1]
	options.Cursor = &ListCursor{UpdatedAt: last.UpdatedAt, ID: last.ID}
	page, err = store.Query(context.Background(), filter, options)
	if err != nil || len(page.Sessions) != 2 || page.HasMore || page.Sessions[0].ID != "ses_00000001" {
		t.Fatalf("second page = %+v, err=%v", page, err)
	}
	page, err = store.Query(context.Background(), filter, ListOptions{ArchivedOnly: true})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].State != StateArchived {
		t.Fatalf("archive page = %+v, err=%v", page, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Query(ctx, filter, options); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled query = %v", err)
	}
}

func TestMetadataCopiesAreOwnedDuringConcurrentAppend(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(CreateInput{Source: &Source{App: "pua", Metadata: map[string]string{"key": "value"}}, LaunchEnvironment: map[string]string{"PATH": "/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, "turn.started", "turn_race", nil); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for iteration := 0; iteration < 30; iteration++ {
				page, err := store.Query(context.Background(), ListFilter{}, ListOptions{Limit: 1})
				if err != nil {
					t.Error(err)
					return
				}
				value := page.Sessions[0]
				value.Source.Metadata["key"] = "mutated"
				value.LaunchEnvironment["PATH"] = "mutated"
				if value.LastActivityAt != nil {
					*value.LastActivityAt = time.Time{}
				}
				if value.LastProgressAt != nil {
					*value.LastProgressAt = time.Time{}
				}
			}
		}()
	}
	for iteration := 0; iteration < 10; iteration++ {
		if _, err := store.Append(created.ID, "message.assistant.delta", "turn_race", []byte(`{"text":"hello"}`)); err != nil {
			t.Fatal(err)
		}
	}
	group.Wait()
	value, err := store.Get(created.ID)
	if err != nil || value.Source.Metadata["key"] != "value" || value.LaunchEnvironment["PATH"] != "/bin" || value.LastActivityAt == nil || value.LastActivityAt.IsZero() || value.LastProgressAt == nil || value.LastProgressAt.IsZero() {
		t.Fatalf("mutable metadata escaped: %+v, err=%v", value, err)
	}
}

func BenchmarkMetadataList(b *testing.B) {
	store := metadataFixture(3000)
	b.Run("unbounded", func(b *testing.B) {
		b.ReportAllocs()
		for index := 0; index < b.N; index++ {
			store.Filter(ListFilter{})
		}
	})
	b.Run("page1", func(b *testing.B) {
		b.ReportAllocs()
		for index := 0; index < b.N; index++ {
			if _, err := store.Query(context.Background(), ListFilter{}, ListOptions{Limit: 1}); err != nil {
				b.Fatal(err)
			}
		}
	})
}
