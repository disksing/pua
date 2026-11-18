package serve

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func TestStartupRemovesLegacyTaskTemplates(t *testing.T) {
	root := t.TempDir()
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"open", "archived", "linked"} {
		if _, err := workspace.CreateProject("Template migration project", slug); err != nil {
			t.Fatal(err)
		}
	}
	for _, input := range []app.CreateTaskInput{
		{ProjectID: "project1", Title: "Open task"},
		{ProjectID: "project1", Title: "Archived task"},
		{ProjectID: "project1", Title: "Ordinary task"},
		{ProjectID: "project2", Title: "Task moved with its project"},
		{ProjectID: "project2", Title: "Task archived before its project"},
	} {
		if _, err := workspace.CreateTask(input); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"project1.task2", "project2.task2", "project2"} {
		if _, err := workspace.ArchiveResource(id); err != nil {
			t.Fatal(err)
		}
	}

	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	openProject := filepath.Join(root, "project1-open")
	archivedProject := filepath.Join(root, "archive", "project2-archived")
	linkedProject := filepath.Join(root, "project3-linked")
	for _, project := range []string{openProject, archivedProject} {
		write(filepath.Join(project, "templates", "review.md"), "# Old review instructions\n")
	}
	outside := t.TempDir()
	write(filepath.Join(outside, "keep.md"), "External file\n")
	if err := os.Symlink(outside, filepath.Join(linkedProject, "templates")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(openProject, "templates", "linked")); err != nil {
		t.Fatal(err)
	}
	untouched := filepath.Join(openProject, "artifacts", "templates", "keep.md")
	write(untouched, "Project output\n")

	taskDirs := []string{
		filepath.Join(openProject, "task1"),
		filepath.Join(openProject, "archive", "task2"),
		filepath.Join(archivedProject, "task1"),
		filepath.Join(archivedProject, "archive", "task2"),
	}
	wantMetadata := make(map[string][]byte)
	for i, dir := range taskDirs {
		metadata := filepath.Join(dir, "task.json")
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(read(metadata)), &fields); err != nil {
			t.Fatal(err)
		}
		fields["extra"] = json.RawMessage(`{"count":9007199254740993}`)
		fields["template"] = json.RawMessage(`{"name":"review","schemaVersion":2,"digest":"sha256:old"}`)
		if i == 1 {
			fields["template"] = json.RawMessage(`{"name":"missing","schemaVersion":1}`)
		}
		data, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		write(metadata, string(data))
		delete(fields, "template")
		expected, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		wantMetadata[metadata] = expected
		write(filepath.Join(dir, app.SpecMarkdownFile), "# Materialized task specification\n")
	}
	ordinaryMetadata := filepath.Join(openProject, "task3", "task.json")
	ordinary := read(ordinaryMetadata)
	guidance := filepath.Join(root, "AGENTS.md")
	write(guidance, "Local instructions\n\n<!-- managed by pua cli -->\nUse pua template list.\n<!-- end of pua cli prompt -->\n\nLocal postscript\n")

	configPath := filepath.Join(t.TempDir(), "serve.json")
	s := &server{config: configPath, locks: newWorkspaceLockManager("", configPath)}
	t.Cleanup(s.locks.closeAll)
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "legacy", Path: root}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureConfiguredResourceRuntimes(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(openProject, "templates", "review.md")); err != nil {
		t.Fatalf("unowned Workspace was modified: %v", err)
	}
	if err := s.acquireConfiguredWorkspaceLocks(); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := s.ensureConfiguredResourceRuntimes(); err != nil {
			t.Fatal(err)
		}
		for _, project := range []string{openProject, archivedProject, linkedProject} {
			if _, err := os.Lstat(filepath.Join(project, "templates")); !os.IsNotExist(err) {
				t.Fatalf("legacy template directory still exists in %s: %v", project, err)
			}
		}
		for metadata, expected := range wantMetadata {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(read(metadata)), &fields); err != nil {
				t.Fatal(err)
			}
			actual, err := json.Marshal(fields)
			if err != nil || !bytes.Equal(actual, expected) {
				t.Fatalf("unexpected migration of %s: %s (%v)", metadata, actual, err)
			}
			if spec := read(filepath.Join(filepath.Dir(metadata), app.SpecMarkdownFile)); spec != "# Materialized task specification\n" {
				t.Fatalf("existing task spec changed: %q", spec)
			}
		}
	}
	if read(ordinaryMetadata) != ordinary || read(untouched) != "Project output\n" || read(filepath.Join(outside, "keep.md")) != "External file\n" {
		t.Fatal("migration changed data outside the old template storage or rewrote ordinary task metadata")
	}
	agents := read(guidance)
	if !strings.HasPrefix(agents, "Local instructions\n\n") || !strings.HasSuffix(agents, "\n\nLocal postscript\n") || !strings.Contains(agents, "read the Project's spec.md") || strings.Contains(agents, "pua template list") {
		t.Fatalf("managed guidance was not updated independently: %s", agents)
	}
}

func TestRetiredTaskTemplateAPI(t *testing.T) {
	root := t.TempDir()
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.CreateProject("Project", ""); err != nil {
		t.Fatal(err)
	}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "test", Path: root}}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method string
		path   string
		body   string
		status int
	}{
		{http.MethodGet, "/templates", "", http.StatusNotFound},
		{http.MethodPost, "/templates/validate", `{}`, http.StatusNotFound},
		{http.MethodPost, "/tasks", `{"project":"project1","title":"Task","templateName":"review"}`, http.StatusBadRequest},
		{http.MethodPost, "/tasks/preview", `{"project":"project1","title":"Task","templateFields":{}}`, http.StatusBadRequest},
		{http.MethodPost, "/tasks/preview", `{"project":"project1","title":"Task","taskMarkdown":"# Exact\n"}`, http.StatusOK},
	} {
		t.Run(tc.path+" "+tc.body, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			s.handleWorkspace(recorder, httptest.NewRequest(tc.method, "/api/workspaces/test"+tc.path, strings.NewReader(tc.body)))
			if recorder.Code != tc.status {
				t.Fatalf("HTTP %d, expected %d: %s", recorder.Code, tc.status, recorder.Body.String())
			}
			if tc.status == http.StatusOK {
				var preview app.TaskPreview
				if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil || preview.Title != "Task" || preview.Markdown != "# Exact\n" {
					t.Fatalf("unexpected manual preview: %s (%v)", recorder.Body, err)
				}
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, "project1", "task1")); !os.IsNotExist(err) {
		t.Fatalf("preview or rejected request created a task: %v", err)
	}
}
