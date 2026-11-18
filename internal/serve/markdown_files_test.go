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

func TestSpecSaveRequiresExplicitRouteAndEditorContext(t *testing.T) {
	fixture := newMarkdownFileTestFixture(t)
	if _, err := fixture.workspace.RegisterUser("editor"); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.workspace.Resource("project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	spec := filepath.Join(fixture.taskPath, "spec.md")
	memory := filepath.Join(fixture.taskPath, "memory.md")
	specPath, memoryPath := task.Path+"/spec.md", task.Path+"/memory.md"
	specHash, memoryHash := fileHash(t, spec), fileHash(t, memory)
	saveSpec := func(path, hash, origin, user, token string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]string{"content": "# Approved update\n", "expectedContentHash": hash})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPut, "/api/workspaces/workspace-one/resources/project1.task1/spec?path="+url.QueryEscape(path), bytes.NewReader(body))
		req.Header.Set("Origin", origin)
		req.Header.Set(workspaceUserHeader, user)
		req.Header.Set("X-PUA-Chat-Token", token)
		recorder := httptest.NewRecorder()
		fixture.server.handleWorkspace(recorder, req)
		return recorder
	}
	if response := markdownSaveRequest(t, fixture, task.ID, specPath, "Generic write", specHash); response.Code != http.StatusBadRequest {
		t.Fatalf("ordinary Markdown endpoint accepted Spec: %d", response.Code)
	}
	const origin = "http://example.com"
	for _, test := range []struct{ origin, user, token string }{
		{"", "editor", ""},
		{"http://outside.invalid", "editor", ""},
		{origin, "", ""},
		{origin, "unknown", ""},
		{origin, "editor", "agent-chat-token"},
	} {
		if response := saveSpec(specPath, specHash, test.origin, test.user, test.token); response.Code != http.StatusForbidden {
			t.Fatalf("unexpected editor context result: %d %s", response.Code, response.Body)
		}
	}
	if response := saveSpec(memoryPath, memoryHash, origin, "editor", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("Spec endpoint accepted Memory: %d", response.Code)
	}
	if response := saveSpec(specPath, specHash, origin, "editor", ""); response.Code != http.StatusOK {
		t.Fatalf("Spec save failed: %d %s", response.Code, response.Body)
	}
	newSpecHash := fileHash(t, spec)
	if newSpecHash == specHash || fileHash(t, memory) != memoryHash {
		t.Fatal("Spec save changed the wrong file")
	}
	if response := saveSpec(specPath, specHash, origin, "editor", ""); response.Code != http.StatusConflict {
		t.Fatalf("stale Spec save returned %d", response.Code)
	}
	if response := markdownSaveRequest(t, fixture, task.ID, memoryPath, "Memory update", memoryHash); response.Code != http.StatusOK || fileHash(t, spec) != newSpecHash {
		t.Fatalf("Memory save affected Spec or failed: %d %s", response.Code, response.Body)
	}
	if err := os.Symlink(fixture.taskPath, filepath.Join(fixture.taskPath, "artifacts", "redirected")); err != nil {
		t.Fatal(err)
	}
	if response := markdownSaveRequest(t, fixture, task.ID, task.Path+"/artifacts/redirected/spec.md", "Aliased write", newSpecHash); response.Code != http.StatusBadRequest {
		t.Fatalf("aliased ordinary write accepted: %d", response.Code)
	}
	archived, err := fixture.workspace.ArchiveResource(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if response := saveSpec(archived.Path+"/spec.md", newSpecHash, origin, "editor", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("archived Spec save returned %d", response.Code)
	}
}

func TestLegacyDocumentLinksReadMemoryAfterRetirement(t *testing.T) {
	fixture := newMarkdownFileTestFixture(t)
	body := "Historical brief\n\nHistory: source-123"
	for dir, name := range map[string]string{fixture.root: "workspace.md", fixture.projectPath: "project.md", fixture.taskPath: "task.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.workspace.Migrate("en"); err != nil {
		t.Fatal(err)
	}
	check := func(id, path, alias string) {
		t.Helper()
		for _, endpoint := range []string{"files", "files/raw"} {
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/"+endpoint+"?path="+url.QueryEscape(alias), nil)
			fixture.server.handleWorkspace(recorder, req)
			if recorder.Code != http.StatusOK {
				t.Fatalf("legacy link unavailable: %s %d %s", alias, recorder.Code, recorder.Body)
			}
			if endpoint == "files/raw" {
				if !strings.Contains(recorder.Body.String(), body) {
					t.Fatalf("raw link lost old content: %s", recorder.Body)
				}
				continue
			}
			var preview filePreview
			if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil || !preview.ReadOnly || preview.Path != path || !strings.Contains(preview.Content, body) {
				t.Fatalf("legacy preview did not resolve read-only: %+v, %v", preview, err)
			}
			if result := markdownSaveRequest(t, fixture, id, path, "Legacy edit", preview.ContentHash); result.Code != http.StatusBadRequest {
				t.Fatalf("legacy file is editable: %d", result.Code)
			}
		}
	}
	check("workspace", "workspace.md", "/workspace.md")
	check("project1", "project1-editable/project.md", "/project1/project.md")
	task, err := fixture.workspace.Resource("project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	check(task.ID, task.Path+"/task.md", "/project1/task1/task.md:7:2")
	archived, err := fixture.workspace.ArchiveResource(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	check(task.ID, archived.Path+"/task.md", "/"+archived.Path+"/task.md")
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
		{name: "workspace memory", resourceID: "workspace", path: "memory.md", abs: filepath.Join(fixture.root, "memory.md")},
		{name: "project memory", resourceID: "project1", path: project.Path + "/memory.md", abs: filepath.Join(fixture.projectPath, "memory.md")},
		{name: "task memory", resourceID: "project1.task1", path: task.Path + "/memory.md", abs: filepath.Join(fixture.taskPath, "memory.md")},
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
			reloaded := httptest.NewRecorder()
			fixture.server.handleWorkspace(reloaded, httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/files?path="+url.QueryEscape(test.path), nil))
			var preview struct {
				Content     string `json:"content"`
				ContentHash string `json:"contentHash"`
			}
			if err := json.Unmarshal(reloaded.Body.Bytes(), &preview); err != nil || reloaded.Code != http.StatusOK ||
				preview.Content != updated || preview.ContentHash != previewContentHash(data) {
				t.Fatalf("reloaded document = %s, err=%v", reloaded.Body.String(), err)
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
	taskBrief := filepath.Join(fixture.taskPath, "memory.md")
	for _, path := range []string{"chat.md", project.Path + "/chat.md", task.Path + "/chat.md"} {
		absolute := filepath.Join(fixture.root, path)
		if err := os.WriteFile(absolute, []byte("Late legacy write"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"workspace", "project1", "project1.task1"} {
			if result := markdownSaveRequest(t, fixture, id, path, "changed", fileHash(t, absolute)); result.Code != http.StatusBadRequest {
				t.Fatalf("legacy document was editable: %s %s: %d", id, path, result.Code)
			}
		}
	}
	originalHash := fileHash(t, taskBrief)
	if err := os.WriteFile(taskBrief, []byte("# External change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := markdownSaveRequest(t, fixture, "project1.task1", task.Path+"/memory.md", "# Browser draft\n", originalHash)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "changed on disk") {
		t.Fatalf("conflicting save returned %d: %s", recorder.Code, recorder.Body.String())
	}

	rejections := []struct {
		name       string
		resourceID string
		path       string
		abs        string
	}{
		{name: "workspace child memory", resourceID: "workspace", path: task.Path + "/memory.md", abs: filepath.Join(fixture.taskPath, "memory.md")},
		{name: "project child memory", resourceID: "project1", path: task.Path + "/memory.md", abs: filepath.Join(fixture.taskPath, "memory.md")},
		{name: "task parent memory", resourceID: "project1.task1", path: "memory.md", abs: filepath.Join(fixture.root, "memory.md")},
		{name: "workspace artifact", resourceID: "workspace", path: project.Path + "/artifacts/notes.md", abs: filepath.Join(fixture.projectPath, "artifacts", "notes.md")},
		{name: "other resource memory", resourceID: "project1", path: task.Path + "/memory.md", abs: taskBrief},
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
	archivedBrief := filepath.Join(fixture.root, filepath.FromSlash(archived.Path), "memory.md")
	recorder = markdownSaveRequest(t, fixture, "project1.task1", archived.Path+"/memory.md", "# Archived edit\n", fileHash(t, archivedBrief))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "archived") {
		t.Fatalf("archived save returned %d: %s", recorder.Code, recorder.Body.String())
	}

	_ = task
}
