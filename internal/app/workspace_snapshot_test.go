package app

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFileTreeLimits(t *testing.T) {
	if maxFileTreeDepth != 3 {
		t.Fatalf("maxFileTreeDepth = %d, want 3", maxFileTreeDepth)
	}
	if maxFileTreeEntries != 10000 {
		t.Fatalf("maxFileTreeEntries = %d, want 10000", maxFileTreeEntries)
	}
}

func TestFileTreeKeepsLaterSiblingsWhenEarlierDirectoriesAreDeep(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "artifacts")
	for i := 0; i < 20; i++ {
		mustWriteFile(t, filepath.Join(dir, "preview", "nested", fmt.Sprintf("%02d.txt", i)), "x")
	}
	mustWriteFile(t, filepath.Join(dir, "video", "movie.mp4"), "mp4")

	entries, err := readFileTreeLimited(root, dir, 3, 8)
	if err != nil {
		t.Fatal(err)
	}
	if got := fileTreeNames(entries); !reflect.DeepEqual(got, []string{"preview", "video"}) {
		t.Fatalf("top-level names = %v, want [preview video]", got)
	}
	video := findFileTreeEntry(entries, "video")
	if video.Type != "directory" {
		t.Fatal("expected video directory to remain visible")
	}
	if got := fileTreeNames(video.Children); !reflect.DeepEqual(got, []string{"movie.mp4"}) {
		t.Fatalf("video children = %v truncated=%v, want [movie.mp4]", got, video.Truncated)
	}
	if video.Truncated {
		t.Fatal("video listing should be complete")
	}
	preview := findFileTreeEntry(entries, "preview")
	nested := findFileTreeEntry(preview.Children, "nested")
	if !nested.Truncated {
		t.Fatal("expected nested preview files to be truncated after later siblings are listed")
	}
}

func TestFileTreeMarksTruncatedDirectories(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "artifacts")
	for i := 0; i < 5; i++ {
		mustWriteFile(t, filepath.Join(dir, "preview", fmt.Sprintf("%02d.txt", i)), "x")
	}
	mustWriteFile(t, filepath.Join(dir, "video", "movie.mp4"), "mp4")

	entries, err := readFileTreeLimited(root, dir, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := fileTreeNames(entries); !reflect.DeepEqual(got, []string{"preview", "video"}) {
		t.Fatalf("top-level names = %v, want [preview video]", got)
	}
	preview := findFileTreeEntry(entries, "preview")
	if !preview.Truncated {
		t.Fatal("expected preview to be marked truncated")
	}
	if len(preview.Children) != 1 {
		t.Fatalf("preview children = %v, want one file before the cap", fileTreeNames(preview.Children))
	}
	video := findFileTreeEntry(entries, "video")
	if !video.Truncated {
		t.Fatal("expected video to be marked truncated when the cap is already exhausted")
	}
	if len(video.Children) != 0 {
		t.Fatalf("video children = %v, want none after the cap", fileTreeNames(video.Children))
	}
}

func TestFileTreeRespectsDepthLimit(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "artifacts")
	mustWriteFile(t, filepath.Join(dir, "a", "b", "c", "d", "hidden.txt"), "hidden")

	entries, err := readFileTreeLimited(root, dir, 3, 100)
	if err != nil {
		t.Fatal(err)
	}
	a := findFileTreeEntry(entries, "a")
	b := findFileTreeEntry(a.Children, "b")
	c := findFileTreeEntry(b.Children, "c")
	d := findFileTreeEntry(c.Children, "d")
	if d.Type != "directory" {
		t.Fatalf("expected directory d at depth 3, got %#v", d)
	}
	if len(d.Children) != 0 {
		t.Fatalf("depth 4 files should be omitted, got %v", fileTreeNames(d.Children))
	}
}

func TestFileTreeSkipsBuildDirectories(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "artifacts")
	mustWriteFile(t, filepath.Join(dir, "keep.txt"), "ok")
	mustWriteFile(t, filepath.Join(dir, "node_modules", "pkg.js"), "skip")
	mustWriteFile(t, filepath.Join(dir, ".git", "HEAD"), "skip")

	entries, err := readFileTreeLimited(root, dir, 3, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := fileTreeNames(entries); !reflect.DeepEqual(got, []string{"keep.txt"}) {
		t.Fatalf("names = %v, want [keep.txt]", got)
	}
}

func TestFileTreeMissingDirectory(t *testing.T) {
	root := t.TempDir()
	if entries := readFileTree(root, filepath.Join(root, "missing")); entries == nil || len(entries) != 0 {
		t.Fatalf("missing tree = %#v, want empty slice", entries)
	}
	if _, err := readFileTreeLimited(root, filepath.Join(root, "missing"), 3, 100); err == nil {
		t.Fatal("expected error for a missing directory")
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileTreeNames(entries []FileTreeEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names
}

func findFileTreeEntry(entries []FileTreeEntry, name string) FileTreeEntry {
	for _, entry := range entries {
		if entry.Name == name {
			return entry
		}
	}
	return FileTreeEntry{}
}
