package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func TestStartupRefreshesOwnedWorkspaceGuidance(t *testing.T) {
	root := t.TempDir()
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.CreateProject("Project", ""); err != nil {
		t.Fatal(err)
	}
	templates := filepath.Join(root, "project1", "templates")
	if err := os.Mkdir(templates, 0o755); err != nil {
		t.Fatal(err)
	}
	guidance := filepath.Join(root, "AGENTS.md")
	const before = "Local instructions\n\n<!-- managed by pua cli -->\nStale guidance.\n<!-- end of pua cli prompt -->\n\nLocal postscript\n"
	if err := os.WriteFile(guidance, []byte(before), 0o640); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "serve.json")
	s := &server{config: configPath, locks: newWorkspaceLockManager("", configPath)}
	t.Cleanup(s.locks.closeAll)
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "test", Path: root}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureConfiguredResourceRuntimes(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(guidance); err != nil || string(data) != before {
		t.Fatalf("startup changed unowned Workspace guidance: %q (%v)", data, err)
	}
	if err := s.acquireConfiguredWorkspaceLocks(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.ensureConfiguredResourceRuntimes(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(templates); err != nil {
		t.Fatalf("startup changed retired template storage: %v", err)
	}
	data, err := os.ReadFile(guidance)
	if err != nil {
		t.Fatal(err)
	}
	after := string(data)
	if !strings.HasPrefix(after, "Local instructions\n\n") || !strings.HasSuffix(after, "\n\nLocal postscript\n") ||
		!strings.Contains(after, "read the Project's spec.md") || strings.Contains(after, "Stale guidance.") {
		t.Fatalf("managed guidance was not updated independently: %s", after)
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
				t.Fatalf("HTTP %d, expected %d: %s", recorder.Code, tc.status, recorder.Body)
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
