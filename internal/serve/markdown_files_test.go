package serve

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/app"
)

type markdownFileTestFixture struct {
	server      *server
	workspace   *app.Workspace
	root        string
	projectPath string
	taskPath    string
}

func newMarkdownFileTestFixture(t *testing.T) markdownFileTestFixture {
	t.Helper()
	root := t.TempDir()
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := workspace.CreateProject("Editable project", "editable")
	if err != nil {
		t.Fatal(err)
	}
	task, err := workspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Editable task", Slug: "editable-task"})
	if err != nil {
		t.Fatal(err)
	}
	projectDetail, err := workspace.Resource(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	taskDetail, err := workspace.Resource(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(root, filepath.FromSlash(projectDetail.Path))
	taskPath := filepath.Join(root, filepath.FromSlash(taskDetail.Path))
	for path, content := range map[string]string{
		filepath.Join(projectPath, "templates", "review.md"):                "---\nschema-version: 2\ntitle: Review\nfields: []\n---\n# Review\n",
		filepath.Join(projectPath, "artifacts", "notes.md"):                 "# Project notes\n",
		filepath.Join(taskPath, "artifacts", "nested", "review.markdown"):   "# Task review\n",
		filepath.Join(taskPath, "artifacts", "nested", "not-markdown.txt"):  "plain text\n",
		filepath.Join(projectPath, "templates", "nested", "unsupported.md"): "# Nested template\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Path: root}}}); err != nil {
		t.Fatal(err)
	}
	return markdownFileTestFixture{server: s, workspace: workspace, root: root, projectPath: projectPath, taskPath: taskPath}
}

func markdownSaveRequest(t *testing.T, fixture markdownFileTestFixture, resourceID, path, content, expectedHash string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"content": content, "expectedContentHash": expectedHash})
	if err != nil {
		t.Fatal(err)
	}
	requestURL := "/api/workspaces/workspace-one/resources/" + resourceID + "/documents?path=" + url.QueryEscape(path)
	recorder := httptest.NewRecorder()
	fixture.server.handleWorkspace(recorder, httptest.NewRequest(http.MethodPut, requestURL, bytes.NewReader(body)))
	return recorder
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return previewContentHash(data)
}

func TestResourceMarkdownSaveAllowsOnlyOwnedOpenMarkdown(t *testing.T) {
	fixture := newMarkdownFileTestFixture(t)
	project, err := fixture.workspace.Resource("project1")
	if err != nil {
		t.Fatal(err)
	}
	task, err := fixture.workspace.Resource("project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		resourceID string
		path       string
		abs        string
	}{
		{name: "workspace chat", resourceID: "workspace", path: "chat.md", abs: filepath.Join(fixture.root, "chat.md")},
		{name: "project chat", resourceID: "project1", path: project.Path + "/chat.md", abs: filepath.Join(fixture.projectPath, "chat.md")},
		{name: "task chat", resourceID: "project1.task1", path: task.Path + "/chat.md", abs: filepath.Join(fixture.taskPath, "chat.md")},
		{name: "project brief", resourceID: "project1", path: project.Path + "/project.md", abs: filepath.Join(fixture.projectPath, "project.md")},
		{name: "task brief", resourceID: "project1.task1", path: task.Path + "/task.md", abs: filepath.Join(fixture.taskPath, "task.md")},
		{name: "project template", resourceID: "project1", path: project.Path + "/templates/review.md", abs: filepath.Join(fixture.projectPath, "templates", "review.md")},
		{name: "project artifact", resourceID: "project1", path: project.Path + "/artifacts/notes.md", abs: filepath.Join(fixture.projectPath, "artifacts", "notes.md")},
		{name: "nested task artifact", resourceID: "project1.task1", path: task.Path + "/artifacts/nested/review.markdown", abs: filepath.Join(fixture.taskPath, "artifacts", "nested", "review.markdown")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			beforeInfo, err := os.Stat(test.abs)
			if err != nil {
				t.Fatal(err)
			}
			updated := "# Updated " + test.name + "\n"
			recorder := markdownSaveRequest(t, fixture, test.resourceID, test.path, updated, fileHash(t, test.abs))
			if recorder.Code != http.StatusOK {
				t.Fatalf("save returned %d: %s", recorder.Code, recorder.Body.String())
			}
			data, err := os.ReadFile(test.abs)
			if err != nil || string(data) != updated {
				t.Fatalf("saved content = %q, err=%v", data, err)
			}
			afterInfo, err := os.Stat(test.abs)
			if err != nil {
				t.Fatal(err)
			}
			if afterInfo.Mode().Perm() != beforeInfo.Mode().Perm() {
				t.Fatalf("save changed file mode from %v to %v", beforeInfo.Mode().Perm(), afterInfo.Mode().Perm())
			}
		})
	}
}

func TestResourceMarkdownSaveRejectsConflictsAndUnownedPaths(t *testing.T) {
	fixture := newMarkdownFileTestFixture(t)
	project, _ := fixture.workspace.Resource("project1")
	task, _ := fixture.workspace.Resource("project1.task1")
	taskBrief := filepath.Join(fixture.taskPath, "task.md")
	originalHash := fileHash(t, taskBrief)
	if err := os.WriteFile(taskBrief, []byte("# External change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := markdownSaveRequest(t, fixture, "project1.task1", task.Path+"/task.md", "# Browser draft\n", originalHash)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "changed on disk") {
		t.Fatalf("conflicting save returned %d: %s", recorder.Code, recorder.Body.String())
	}

	rejections := []struct {
		name       string
		resourceID string
		path       string
		abs        string
	}{
		{name: "workspace child chat", resourceID: "workspace", path: task.Path + "/chat.md", abs: filepath.Join(fixture.taskPath, "chat.md")},
		{name: "project child chat", resourceID: "project1", path: task.Path + "/chat.md", abs: filepath.Join(fixture.taskPath, "chat.md")},
		{name: "task parent chat", resourceID: "project1.task1", path: "chat.md", abs: filepath.Join(fixture.root, "chat.md")},
		{name: "workspace artifact", resourceID: "workspace", path: project.Path + "/artifacts/notes.md", abs: filepath.Join(fixture.projectPath, "artifacts", "notes.md")},
		{name: "other resource brief", resourceID: "project1", path: task.Path + "/task.md", abs: taskBrief},
		{name: "text artifact", resourceID: "project1.task1", path: task.Path + "/artifacts/nested/not-markdown.txt", abs: filepath.Join(fixture.taskPath, "artifacts", "nested", "not-markdown.txt")},
		{name: "nested template", resourceID: "project1", path: project.Path + "/templates/nested/unsupported.md", abs: filepath.Join(fixture.projectPath, "templates", "nested", "unsupported.md")},
		{name: "resource metadata", resourceID: "project1.task1", path: task.Path + "/task.json", abs: filepath.Join(fixture.taskPath, "task.json")},
	}
	for _, test := range rejections {
		t.Run(test.name, func(t *testing.T) {
			recorder := markdownSaveRequest(t, fixture, test.resourceID, test.path, "changed\n", fileHash(t, test.abs))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("rejected save returned %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}

	link := filepath.Join(fixture.taskPath, "artifacts", "linked.md")
	if err := os.Symlink(taskBrief, link); err != nil {
		t.Fatal(err)
	}
	recorder = markdownSaveRequest(t, fixture, "project1.task1", task.Path+"/artifacts/linked.md", "changed\n", fileHash(t, taskBrief))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "symbolic link") {
		t.Fatalf("symlink save returned %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestResourceMarkdownSaveRejectsArchivedMissingAndOversizedFiles(t *testing.T) {
	fixture := newMarkdownFileTestFixture(t)
	project, _ := fixture.workspace.Resource("project1")
	task, _ := fixture.workspace.Resource("project1.task1")

	recorder := markdownSaveRequest(t, fixture, "project1", project.Path+"/artifacts/missing.md", "# New\n", previewContentHash(nil))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "existing") {
		t.Fatalf("missing save returned %d: %s", recorder.Code, recorder.Body.String())
	}

	largePath := filepath.Join(fixture.projectPath, "artifacts", "large.md")
	if err := os.WriteFile(largePath, bytes.Repeat([]byte("x"), previewMaxBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder = markdownSaveRequest(t, fixture, "project1", project.Path+"/artifacts/large.md", "# Small\n", fileHash(t, largePath))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "larger") {
		t.Fatalf("oversized save returned %d: %s", recorder.Code, recorder.Body.String())
	}

	if _, err := fixture.workspace.ArchiveResource("project1.task1"); err != nil {
		t.Fatal(err)
	}
	archived, err := fixture.workspace.Resource("project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	archivedBrief := filepath.Join(fixture.root, filepath.FromSlash(archived.Path), "task.md")
	recorder = markdownSaveRequest(t, fixture, "project1.task1", archived.Path+"/task.md", "# Archived edit\n", fileHash(t, archivedBrief))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "archived") {
		t.Fatalf("archived save returned %d: %s", recorder.Code, recorder.Body.String())
	}

	_ = task
}
