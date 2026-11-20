package generation

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testRecord(resourceID, id, generationID string, generationNumber int, payload string) Record {
	return Record{
		ResourceID: resourceID, ID: id, Generation: generationNumber, GenerationID: generationID,
		CreatedAt: "2026-08-01T00:00:00Z", UpdatedAt: "2026-08-01T00:00:01Z",
		Payload: json.RawMessage(payload),
	}
}

func testCurrentPath(t *testing.T, store *Store, resourceID string) string {
	t.Helper()
	key, err := ResourceKey(store.instanceID, resourceID)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(store.resourceDir(key), currentFileName)
}

func writeTestFile(t *testing.T, path string, value []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, value, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResourceKeyIsUnambiguousAndPathSafe(t *testing.T) {
	left, err := ResourceKey("instance\x00a", "b")
	if err != nil {
		t.Fatal(err)
	}
	right, err := ResourceKey("instance", "a\x00b")
	if err != nil {
		t.Fatal(err)
	}
	if left == right {
		t.Fatalf("resource key collision: %q", left)
	}
	for _, key := range []string{left, right} {
		if key == "" || strings.ContainsAny(key, `/\\`) || key == "." || key == ".." {
			t.Fatalf("resource key is not path safe: %q", key)
		}
	}
	workspace, err := ResourceKey("instance", "")
	if err != nil {
		t.Fatal(err)
	}
	if workspace == "" {
		t.Fatal("empty resource must still have a stable key")
	}
}

func TestStoreEnforcesCurrentOwnershipAndImmutableRetirement(t *testing.T) {
	store, err := Open(t.TempDir(), "instance-one")
	if err != nil {
		t.Fatal(err)
	}
	first := testRecord("project1", "run-one", "gen-one", 1, `{"status":"running"}`)
	other := testRecord("project1.task1", "run-two", "gen-two", 1, `{"status":"idle"}`)
	if err := store.SaveCurrent(first); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCurrent(other); err != nil {
		t.Fatal(err)
	}
	otherBefore, err := os.ReadFile(testCurrentPath(t, store, other.ResourceID))
	if err != nil {
		t.Fatal(err)
	}
	updated := first
	updated.Payload = json.RawMessage(`{"status":"waiting_approval"}`)
	if err := store.SaveCurrent(updated); err != nil {
		t.Fatal(err)
	}
	otherAfter, err := os.ReadFile(testCurrentPath(t, store, other.ResourceID))
	if err != nil {
		t.Fatal(err)
	}
	if string(otherBefore) != string(otherAfter) {
		t.Fatal("updating one resource rewrote another resource's current file")
	}
	conflicting := testRecord("project1", "run-two", "gen-two", 2, `{"status":"running"}`)
	if err := store.SaveCurrent(conflicting); !errors.Is(err, ErrCurrentConflict) {
		t.Fatalf("cross-generation current write error = %v, want ErrCurrentConflict", err)
	}

	if err := store.RetireCurrent(updated, "replaced"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Current("project1"); err != nil || found {
		t.Fatalf("retired current still addressable: found=%v err=%v", found, err)
	}
	current, err := store.ListCurrent()
	if err != nil || len(current) != 1 || current[0].ResourceID != "project1.task1" {
		t.Fatalf("current records after retirement = %#v, err=%v", current, err)
	}
	all, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || !all[0].Retired && !all[1].Retired {
		t.Fatalf("retired history was not preserved: %#v", all)
	}
	changed := updated
	changed.Payload = json.RawMessage(`{"status":"corrupted"}`)
	if err := store.SaveRetired(changed, "tamper"); !errors.Is(err, ErrImmutable) {
		t.Fatalf("retired mutation error = %v, want ErrImmutable", err)
	}
	if err := store.SaveCurrent(changed); !errors.Is(err, ErrImmutable) {
		t.Fatalf("retired generation resurrected with error = %v", err)
	}
	if err := store.RetireCurrent(updated, "retry"); err != nil {
		t.Fatalf("repeating retirement was not idempotent: %v", err)
	}
}

func TestStoreKeepsRetiredHistoryWithoutRewritingIt(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root, "instance-scale")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureReady(); err != nil {
		t.Fatal(err)
	}
	key, err := ResourceKey(store.instanceID, "project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	retiredDir := filepath.Join(store.resourceDir(key), retiredDirName)
	if err := os.MkdirAll(retiredDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const retiredCount = 128
	for index := 0; index < retiredCount; index++ {
		generationID := fmt.Sprintf("gen-retired-%05d", index)
		name := base64.RawURLEncoding.EncodeToString([]byte(generationID)) + ".json"
		file := fileRecord{
			Version: SchemaVersion, Kind: "retired", WorkspaceInstanceID: store.instanceID,
			ResourceID: "project1.task1", ID: "run-" + generationID, Generation: index + 1,
			GenerationID: generationID, Record: json.RawMessage(fmt.Sprintf(`{"generation":%d}`, index+1)),
		}
		data, err := json.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(retiredDir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	current := testRecord("project1.task1", "run-current", "gen-current", retiredCount+1, `{"status":"idle"}`)
	if err := store.SaveCurrent(current); err != nil {
		t.Fatal(err)
	}
	if next, err := store.NextGeneration("project1.task1"); err != nil || next != retiredCount+2 {
		t.Fatalf("next generation after scaled history = %d, err=%v", next, err)
	}
	retiredPath := filepath.Join(retiredDir, base64.RawURLEncoding.EncodeToString([]byte("gen-retired-00000"))+".json")
	before := mustReadTestFile(t, retiredPath)
	current.Payload = json.RawMessage(`{"status":"running"}`)
	if err := store.SaveCurrent(current); err != nil {
		t.Fatal(err)
	}
	after := mustReadTestFile(t, retiredPath)
	if string(before) != string(after) {
		t.Fatal("current projection update rewrote retired history")
	}
	all, err := store.List()
	if err != nil || len(all) != retiredCount+1 {
		t.Fatalf("scale list count = %d, err=%v", len(all), err)
	}
	currentRecords, err := store.ListCurrent()
	if err != nil || len(currentRecords) != 1 || currentRecords[0].GenerationID != "gen-current" {
		t.Fatalf("scale current records = %#v, err=%v", currentRecords, err)
	}
}

func TestStoreRejectsRecordsWithoutGenerationID(t *testing.T) {
	store, err := Open(t.TempDir(), "instance-strict")
	if err != nil {
		t.Fatal(err)
	}
	record := testRecord("project1", "run-no-id", "", 0, `{"status":"running"}`)
	if err := store.SaveCurrent(record); !errors.Is(err, ErrGenerationIDRequired) {
		t.Fatalf("SaveCurrent without generation id = %v, want ErrGenerationIDRequired", err)
	}
	if err := store.SaveRetired(record, "cold"); !errors.Is(err, ErrGenerationIDRequired) {
		t.Fatalf("SaveRetired without generation id = %v, want ErrGenerationIDRequired", err)
	}
	if err := store.RetireCurrent(record, "replaced"); !errors.Is(err, ErrGenerationIDRequired) {
		t.Fatalf("RetireCurrent without generation id = %v, want ErrGenerationIDRequired", err)
	}
}

func TestStoreSerializesOnlyEachResourceKey(t *testing.T) {
	store, err := Open(t.TempDir(), "instance-concurrent")
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	errorsCh := make(chan error, 80)
	for _, resourceID := range []string{"project1", "project1.task1"} {
		resourceID := resourceID
		for worker := 0; worker < 40; worker++ {
			group.Add(1)
			go func(worker int) {
				defer group.Done()
				record := testRecord(resourceID, "run-"+resourceID, "gen-"+resourceID, 1, fmt.Sprintf(`{"worker":%d}`, worker))
				if err := store.SaveCurrent(record); err != nil {
					errorsCh <- err
				}
			}(worker)
		}
	}
	group.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Fatal(err)
	}
	current, err := store.ListCurrent()
	if err != nil || len(current) != 2 {
		t.Fatalf("concurrent current records = %#v err=%v", current, err)
	}
	for _, record := range current {
		if !json.Valid(record.Payload) {
			t.Fatalf("concurrent write produced invalid payload: %#v", record)
		}
	}
}

func mustReadTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
