package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitMigrationRestartsAndReplacesLateSource(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, legacyDocumentStage)
	pending := filepath.Join(stage, taskMDFile)
	oldPath := filepath.Join(dir, taskMDFile)
	memory := filepath.Join(dir, MemoryMarkdownFile)
	spec := filepath.Join(dir, SpecMarkdownFile)
	initial := []byte("Background\r\n\r\n## Chat follow-ups\r\nHistory: ref-42")
	next := append(bytes.Clone(initial), []byte("\r\nStatus: published")...)
	write := func(path string, content []byte) {
		t.Helper()
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) []byte {
		t.Helper()
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return content
	}
	migrate := func() error { return ensureSplitDocuments(dir, taskMDFile, "New contract", "en") }
	write(spec, []byte("Existing approved Spec"))
	write(memory, []byte("Existing Memory"))
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	write(pending, initial)
	if err := migrate(); err != nil {
		t.Fatal(err)
	}
	merged := read(memory)
	if !bytes.HasPrefix(merged, []byte("Existing Memory")) || bytes.Count(merged, initial) != 1 {
		t.Fatalf("incomplete staged migration: %q", merged)
	}

	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	write(pending, initial)
	write(oldPath, next)
	if err := migrate(); err != nil {
		t.Fatal(err)
	}
	merged = read(memory)
	if !bytes.HasPrefix(merged, []byte("Existing Memory")) || bytes.Count(merged, initial) != 1 || !bytes.Contains(merged, next) {
		t.Fatalf("late revision should replace the previous import: %q", merged)
	}
	for _, path := range []string{stage, oldPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("legacy source survived at %s: %v", path, err)
		}
	}
	if err := migrate(); err != nil || !bytes.Equal(merged, read(memory)) {
		t.Fatalf("rerun changed Memory: %v", err)
	}
	if string(read(spec)) != "Existing approved Spec" {
		t.Fatal("migration changed existing Spec")
	}
	if info, err := os.Stat(memory); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("migration changed Memory permissions: %v %v", info, err)
	}

	edited := bytes.Replace(merged, []byte("Status: published"), []byte("User corrected the status"), 1)
	write(memory, edited)
	write(oldPath, next)
	if err := migrate(); err != nil || !bytes.Equal(edited, read(memory)) {
		t.Fatalf("replayed source modified Memory edits: %v", err)
	}
	conflicting := append(bytes.Clone(next), []byte("\r\nNew old-agent update")...)
	write(oldPath, conflicting)
	if err := migrate(); err == nil || !strings.Contains(err.Error(), "both the legacy source") {
		t.Fatalf("expected explicit conflict, got %v", err)
	}
	if !bytes.Equal(edited, read(memory)) || !bytes.Equal(conflicting, read(pending)) {
		t.Fatal("conflict lost one of the edits")
	}
	write(memory, bytes.Replace(edited, []byte("User corrected the status"), []byte("Status: published\r\nNew old-agent update"), 1))
	if err := migrate(); err != nil {
		t.Fatalf("could not resume after merging staged revision: %v", err)
	}
}

func TestSplitMigrationRecognizesTask582ChatBlock(t *testing.T) {
	legacy := []byte("Pending item\nHistory: first-user-input\n")
	dir := t.TempDir()
	primary := append([]byte("Original body\n\n"), mergedChatBlock(legacy)...)
	for name, content := range map[string][]byte{taskMDFile: primary, legacyChatMarkdownFile: legacy} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := ensureSplitDocuments(dir, taskMDFile, "Default", "en"); err != nil {
		t.Fatal(err)
	}
	memory, err := os.ReadFile(filepath.Join(dir, MemoryMarkdownFile))
	if err != nil || !bytes.Contains(memory, primary) || bytes.Count(memory, legacy) != 1 {
		t.Fatalf("prior Task 582 import not preserved exactly once: %q %v", memory, err)
	}
}
