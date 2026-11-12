package serve

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestChatDocumentConflictSymlinkAndMissing(t *testing.T) {
	fixture := newMarkdownFileTestFixture(t)
	path := filepath.Join(fixture.root, "chat.md")
	hash := fileHash(t, path)
	if err := os.WriteFile(path, []byte("Agent has a new commitment"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := markdownSaveRequest(t, fixture, "workspace", "chat.md", "stale draft", hash); r.Code != http.StatusConflict {
		t.Fatalf("conflict: %d %s", r.Code, r.Body.String())
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if r := markdownSaveRequest(t, fixture, "workspace", "chat.md", "draft", hash); r.Code != http.StatusBadRequest {
		t.Fatalf("missing: %d", r.Code)
	}
	if err := os.Symlink(filepath.Join(fixture.taskPath, "chat.md"), path); err != nil {
		t.Fatal(err)
	}
	if r := markdownSaveRequest(t, fixture, "workspace", "chat.md", "draft", fileHash(t, path)); r.Code != http.StatusBadRequest {
		t.Fatalf("symlink: %d", r.Code)
	}
	if _, err := fixture.workspace.ArchiveResource("project1.task1"); err != nil {
		t.Fatal(err)
	}
	archived, _ := fixture.workspace.Resource("project1.task1")
	archivedPath := filepath.Join(fixture.root, archived.Path, "chat.md")
	if r := markdownSaveRequest(t, fixture, "project1.task1", archived.Path+"/chat.md", "draft", fileHash(t, archivedPath)); r.Code != http.StatusBadRequest {
		t.Fatalf("archived: %d", r.Code)
	}
}
