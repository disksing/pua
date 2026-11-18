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

const serveTemplate = `---
schema-version: 2
title: API template
task-title: "{{ summary }}"
fields:
  - name: summary
    type: text
    label: Summary
    required: true
  - name: confirmed
    type: boolean
    label: Confirmed
    default: false
---
# {{ summary }}

Confirmed: {{ confirmed }}
`

func templateAPIServer(t *testing.T) (*server, *app.Workspace, string) {
	t.Helper()
	root := t.TempDir()
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := workspace.CreateProject("API templates", "api-templates")
	if err != nil {
		t.Fatal(err)
	}
	detail, err := workspace.Resource(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	templatePath := filepath.Join(root, filepath.FromSlash(detail.Path), "templates", "api.md")
	if err := os.WriteFile(templatePath, []byte(serveTemplate), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Path: root}}}); err != nil {
		t.Fatal(err)
	}
	return s, workspace, templatePath
}

func TestTemplateHTTPListRenderPreviewAndCreate(t *testing.T) {
	s, workspace, _ := templateAPIServer(t)

	recorder := httptest.NewRecorder()
	s.handleWorkspace(recorder, httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/templates?project=project1", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"valid": true`) || !strings.Contains(recorder.Body.String(), `"schemaVersion": 2`) {
		t.Fatalf("unexpected template list: %d %s", recorder.Code, recorder.Body.String())
	}

	renderBody := `{"fields":{"summary":"Rendered","confirmed":true}}`
	recorder = httptest.NewRecorder()
	s.handleWorkspace(recorder, httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-one/templates/api/render?project=project1", strings.NewReader(renderBody)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Confirmed: true") {
		t.Fatalf("unexpected template render: %d %s", recorder.Code, recorder.Body.String())
	}

	request := map[string]any{"project": "project1", "templateName": "api", "templateFields": map[string]any{"summary": "Created"}}
	data, _ := json.Marshal(request)
	recorder = httptest.NewRecorder()
	s.handleWorkspace(recorder, httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-one/tasks/preview", bytes.NewReader(data)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("preview failed: %d %s", recorder.Code, recorder.Body.String())
	}
	var preview app.TaskPreview
	if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Template == nil || preview.Title != "Created" {
		t.Fatalf("unexpected preview: %#v", preview)
	}
	request["expectedTemplateDigest"] = preview.Template.Digest
	data, _ = json.Marshal(request)
	recorder = httptest.NewRecorder()
	s.handleWorkspace(recorder, httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-one/tasks", bytes.NewReader(data)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("create failed: %d %s", recorder.Code, recorder.Body.String())
	}
	resource, err := workspace.Resource("project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := os.ReadFile(filepath.Join(workspace.Root(), filepath.FromSlash(resource.Path), app.SpecMarkdownFile))
	if err != nil || !strings.Contains(string(markdown), "# Created") {
		t.Fatalf("created task does not match preview: %v %q", err, markdown)
	}
}

func TestTemplateHTTPDigestConflictIsStructuredAndSideEffectFree(t *testing.T) {
	s, workspace, templatePath := templateAPIServer(t)
	preview, err := workspace.PreviewTask(app.CreateTaskInput{ProjectID: "project1", TemplateName: "api", TemplateFields: map[string]any{"summary": "Conflict"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(templatePath, []byte(strings.Replace(serveTemplate, "API template", "Changed template", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"project": "project1", "templateName": "api", "templateFields": map[string]any{"summary": "Conflict"}, "expectedTemplateDigest": preview.Template.Digest}
	data, _ := json.Marshal(body)
	recorder := httptest.NewRecorder()
	s.handleWorkspace(recorder, httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-one/tasks", bytes.NewReader(data)))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), `"code":"template_digest_conflict"`) || !strings.Contains(recorder.Body.String(), `"issues"`) {
		t.Fatalf("unexpected conflict response: %d %s", recorder.Code, recorder.Body.String())
	}
	if _, err := workspace.Resource("project1.task1"); err == nil {
		t.Fatal("digest conflict created a task")
	}
}

func TestTemplateHTTPValidatesUnsavedContent(t *testing.T) {
	s, _, _ := templateAPIServer(t)
	body, _ := json.Marshal(map[string]string{"name": "draft", "content": "---\nschema-version: 2\ntitle: Broken\nautorun: true\nfields: []\n---\nBody\n"})
	recorder := httptest.NewRecorder()
	s.handleWorkspace(recorder, httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-one/templates/validate", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "unknown_property") || !strings.Contains(recorder.Body.String(), `"valid": false`) {
		t.Fatalf("unexpected validation response: %d %s", recorder.Code, recorder.Body.String())
	}
}
