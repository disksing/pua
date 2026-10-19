package generation

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestListCurrentSkipsUnrelatedRetiredFiles(t *testing.T) {
	for _, hasCurrent := range []bool{false, true} {
		t.Run(fmt.Sprint(hasCurrent), func(t *testing.T) {
			store, err := Open(t.TempDir(), "instance-fast")
			if err != nil {
				t.Fatal(err)
			}
			record := testRecord("project1", "run-current", "gen-current", 2, `{}`)
			if hasCurrent {
				if err := store.SaveCurrent(record); err != nil {
					t.Fatal(err)
				}
			}
			key, _ := ResourceKey(store.instanceID, record.ResourceID)
			writeTestFile(t, filepath.Join(store.resourceDir(key), retiredDirName, "unrelated.json"), []byte("broken"))
			current, err := store.ListCurrent()
			if err != nil || len(current) != map[bool]int{false: 0, true: 1}[hasCurrent] {
				t.Fatalf("current = %+v, err = %v", current, err)
			}
			if _, err := store.List(); err == nil {
				t.Fatal("full history must still validate retired files")
			}
		})
	}
}

func TestListCurrentHonorsRetirementManifestBeforeCurrentRemoval(t *testing.T) {
	store, err := Open(t.TempDir(), "instance-retired")
	if err != nil {
		t.Fatal(err)
	}
	record := testRecord("project1", "run-current", "gen-current", 2, `{}`)
	if err := store.SaveCurrent(record); err != nil {
		t.Fatal(err)
	}
	// Simulate the crash window after the retirement manifest was committed.
	if err := store.SaveRetired(record, "replaced"); err != nil {
		t.Fatal(err)
	}
	current, err := store.ListCurrent()
	if err != nil || len(current) != 0 {
		t.Fatalf("retired current = %+v, err = %v", current, err)
	}
	key, _ := ResourceKey(store.instanceID, record.ResourceID)
	name := base64.RawURLEncoding.EncodeToString([]byte(record.GenerationID)) + ".json"
	writeTestFile(t, filepath.Join(store.resourceDir(key), retiredDirName, name), []byte("broken"))
	if _, err := store.ListCurrent(); err == nil {
		t.Fatal("the matching retirement manifest must still be validated")
	}
}

func BenchmarkListCurrentRetiredHistory(b *testing.B) {
	for _, count := range []int{0, 128, 2048} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			store, err := Open(b.TempDir(), "instance-benchmark")
			if err != nil {
				b.Fatal(err)
			}
			record := testRecord("project1", "run-current", "gen-current", count+1, `{}`)
			if err := store.SaveCurrent(record); err != nil {
				b.Fatal(err)
			}
			key, _ := ResourceKey(store.instanceID, record.ResourceID)
			dir := filepath.Join(store.resourceDir(key), retiredDirName)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				b.Fatal(err)
			}
			for index := 0; index < count; index++ {
				id := fmt.Sprintf("gen-%d", index)
				data, err := json.Marshal(fileRecord{Version: SchemaVersion, Kind: "retired", ResourceID: "project1", ID: id, GenerationID: id, Record: json.RawMessage(`{}`)})
				if err != nil {
					b.Fatal(err)
				}
				name := base64.RawURLEncoding.EncodeToString([]byte(id)) + ".json"
				if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				if _, err := store.ListCurrent(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
