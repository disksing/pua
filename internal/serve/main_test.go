package serve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func TestFaviconIsLinkedAndEmbedded(t *testing.T) {
	indexData, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(indexData), `<link rel="icon" type="image/png" href="/workspace-icons/pua-yellow-opaque.png" />`) {
		t.Fatal("opaque PNG favicon link is missing from the page head")
	}

	staticRoot, err := fs.Sub(staticFiles, "static")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/workspace-icons/pua-yellow-opaque.png", nil)
	rec := httptest.NewRecorder()
	serveStatic(staticRoot, rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("favicon request returned %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("favicon content type is %q, want image/png", got)
	}

	for _, name := range []string{"pua-yellow", "pua-red", "pua-green", "pua-blue", "pua-purple"} {
		for _, variant := range []string{"", "-opaque"} {
			data, err := staticFiles.ReadFile("static/workspace-icons/" + name + variant + ".png")
			if err != nil {
				t.Fatalf("workspace icon %s%s is not embedded: %v", name, variant, err)
			}
			if len(data) == 0 {
				t.Fatalf("workspace icon %s%s is empty", name, variant)
			}
		}
	}
}

func TestCanonicalFrontendAssetsAndHistoryFallback(t *testing.T) {
	indexData, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	index := string(indexData)
	if count := strings.Count(index, `type="module"`); count != 1 {
		t.Fatalf("index module entry count = %d, want 1", count)
	}
	for _, want := range []string{`src="/assets/pua-app.js"`, `href="/assets/pua-app.css"`} {
		if !strings.Contains(index, want) {
			t.Fatalf("canonical frontend asset is missing %s", want)
		}
	}
	if strings.Contains(index, `src="/app.js"`) {
		t.Fatal("removed application entry is still loaded")
	}
	if _, err := staticFiles.ReadFile("static/app.js"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed application entry is still embedded: %v", err)
	}

	staticRoot, err := fs.Sub(staticFiles, "static")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path        string
		contentType string
	}{
		{path: "/assets/pua-app.js", contentType: "text/javascript; charset=utf-8"},
		{path: "/assets/pua-app.css", contentType: "text/css; charset=utf-8"},
	} {
		recorder := httptest.NewRecorder()
		serveStatic(staticRoot, recorder, httptest.NewRequest(http.MethodGet, test.path, nil))
		if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != test.contentType || recorder.Body.Len() == 0 {
			t.Fatalf("asset %s response = %d %q (%d bytes)", test.path, recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.Len())
		}
	}

	deepLink := httptest.NewRecorder()
	serveStatic(staticRoot, deepLink, httptest.NewRequest(http.MethodGet, "/w/workspace-one/r/project1.task1", nil))
	if deepLink.Code != http.StatusOK || !bytes.Equal(deepLink.Body.Bytes(), indexData) {
		t.Fatalf("History fallback response = %d (%d bytes), want index", deepLink.Code, deepLink.Body.Len())
	}
}

func TestWorkspaceIconCanBeUpdatedAndReset(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "serve.json")
	s := &server{config: configPath}
	if err := s.saveConfig(config{
		Version:    agentHubConfigVersion,
		Workspaces: []serveWorkspace{{ID: "workspace-one", Name: "One", Path: t.TempDir()}},
	}); err != nil {
		t.Fatal(err)
	}

	update := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/workspaces/workspace-one", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleWorkspace(rec, req)
		return rec
	}

	var rec *httptest.ResponseRecorder
	for _, icon := range []string{"pua-red", "pua-green", "pua-blue", "pua-purple", "software-engineering"} {
		rec = update(`{"icon":"` + icon + `"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("workspace icon %s update returned %d: %s", icon, rec.Code, rec.Body.String())
		}
		var workspace serveWorkspace
		if err := json.Unmarshal(rec.Body.Bytes(), &workspace); err != nil {
			t.Fatal(err)
		}
		if workspace.Icon != icon {
			t.Fatalf("updated workspace icon is %q, want %q", workspace.Icon, icon)
		}
	}
	cfg, err := s.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Workspaces[0].Icon; got != "software-engineering" {
		t.Fatalf("persisted workspace icon is %q", got)
	}

	rec = update(`{"icon":"not-a-workspace-icon"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown workspace icon returned %d, want 400", rec.Code)
	}
	cfg, err = s.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Workspaces[0].Icon; got != "software-engineering" {
		t.Fatalf("invalid update changed workspace icon to %q", got)
	}

	rec = update(`{"icon":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("workspace icon reset returned %d: %s", rec.Code, rec.Body.String())
	}
	cfg, err = s.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Workspaces[0].Icon; got != "" {
		t.Fatalf("reset workspace icon is %q, want empty", got)
	}
}

func TestWorkspaceNameCanBeUpdatedAndCleared(t *testing.T) {
	workspacePath := filepath.Join(t.TempDir(), "name-workspace")
	if _, err := app.Initialize(workspacePath, "en"); err != nil {
		t.Fatal(err)
	}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{
		Version:    agentHubConfigVersion,
		Workspaces: []serveWorkspace{{ID: "workspace-one", Name: "name-workspace", Path: workspacePath}},
	}); err != nil {
		t.Fatal(err)
	}

	update := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/workspaces/workspace-one", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleWorkspace(rec, req)
		return rec
	}

	rec := update(`{"name":"  Named Workspace  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("workspace name update returned %d: %s", rec.Code, rec.Body.String())
	}
	var workspace serveWorkspace
	if err := json.Unmarshal(rec.Body.Bytes(), &workspace); err != nil {
		t.Fatal(err)
	}
	if workspace.Name != "Named Workspace" {
		t.Fatalf("updated workspace name is %q", workspace.Name)
	}
	cfg, err := s.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Workspaces[0].Name; got != "Named Workspace" {
		t.Fatalf("persisted workspace name is %q", got)
	}
	if got := app.WorkspaceName(workspacePath); got != "Named Workspace" {
		t.Fatalf("workspace.json name is %q", got)
	}

	// The name survives an icon-only update.
	rec = update(`{"icon":"research-lab"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("workspace icon update returned %d: %s", rec.Code, rec.Body.String())
	}
	cfg, err = s.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Workspaces[0].Name; got != "Named Workspace" {
		t.Fatalf("icon update changed workspace name to %q", got)
	}

	rec = update(`{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty workspace update returned %d, want 400", rec.Code)
	}

	// Clearing the name falls back to the directory base name.
	rec = update(`{"name":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("workspace name reset returned %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &workspace); err != nil {
		t.Fatal(err)
	}
	if workspace.Name != "name-workspace" {
		t.Fatalf("reset workspace name is %q, want directory base name", workspace.Name)
	}
}

func TestWorkspaceListResolvesConfiguredNames(t *testing.T) {
	workspacePath := filepath.Join(t.TempDir(), "listed-workspace")
	puaWorkspace, err := app.Initialize(workspacePath, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetName("Configured Name"); err != nil {
		t.Fatal(err)
	}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	// Seed a stale cached name; reads must resolve the configured name live.
	if err := s.saveConfig(config{
		Version:    agentHubConfigVersion,
		ActiveID:   "workspace-one",
		Workspaces: []serveWorkspace{{ID: "workspace-one", Name: "Stale", Path: workspacePath}},
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	s.handleWorkspaces(rec, httptest.NewRequest(http.MethodGet, "/api/workspaces", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("workspace list returned %d: %s", rec.Code, rec.Body.String())
	}
	var listed config
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Workspaces) != 1 || listed.Workspaces[0].Name != "Configured Name" {
		t.Fatalf("workspace list name = %#v", listed.Workspaces)
	}

	rec = httptest.NewRecorder()
	s.writeSettings(rec)
	var settings settingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	if len(settings.Workspaces) != 1 || settings.Workspaces[0].Name != "Configured Name" {
		t.Fatalf("settings workspace name = %#v", settings.Workspaces)
	}
}

func TestAddingExistingWorkspacePreservesIcon(t *testing.T) {
	workspacePath := t.TempDir()
	if _, err := app.Initialize(workspacePath, "en"); err != nil {
		t.Fatal(err)
	}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	workspace, err := s.addWorkspace(context.Background(), workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.updateWorkspaceIcon(workspace.ID, "research-lab"); err != nil {
		t.Fatal(err)
	}
	readded, err := s.addWorkspace(context.Background(), workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	if readded.Icon != "research-lab" {
		t.Fatalf("re-adding workspace changed icon to %q", readded.Icon)
	}
}

func TestCreateWorkspaceUsesRequestedContentLanguage(t *testing.T) {
	workspacePath := filepath.Join(t.TempDir(), "created-workspace")
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	body := strings.NewReader(fmt.Sprintf(`{"path":%q,"create":true,"language":"zh-CN","initialUserName":"Alice"}`, workspacePath))
	recorder := httptest.NewRecorder()
	s.handleWorkspaces(recorder, httptest.NewRequest(http.MethodPost, "/api/workspaces", body))
	if recorder.Code != http.StatusOK {
		t.Fatalf("create Workspace returned %d: %s", recorder.Code, recorder.Body.String())
	}
	workspace, err := app.OpenWorkspace(workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	language, err := workspace.Language()
	if err != nil {
		t.Fatal(err)
	}
	if language != "zh-CN" {
		t.Fatalf("created Workspace language = %q", language)
	}
	users, err := workspace.Users()
	if err != nil || len(users) != 1 || users[0].Name != "Alice" {
		t.Fatalf("created Workspace users = %#v, %v", users, err)
	}
	if _, err := os.Stat(filepath.Join(workspacePath, ".pua", "users", app.LegacyDefaultUserName)); !os.IsNotExist(err) {
		t.Fatalf("created Workspace contains legacy default User: %v", err)
	}
	agents, err := os.ReadFile(filepath.Join(workspacePath, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agents), "Agent 工作指引") {
		t.Fatalf("created Workspace instructions were not localized:\n%s", agents)
	}
}

func TestCreateWorkspaceRejectsMissingInitialUserBeforeCreatingDirectory(t *testing.T) {
	workspacePath := filepath.Join(t.TempDir(), "must-not-exist")
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	body := strings.NewReader(fmt.Sprintf(`{"path":%q,"create":true,"language":"en"}`, workspacePath))
	recorder := httptest.NewRecorder()
	s.handleWorkspaces(recorder, httptest.NewRequest(http.MethodPost, "/api/workspaces", body))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing initial user returned %d: %s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(workspacePath); !os.IsNotExist(err) {
		t.Fatalf("missing initial user created the directory: %v", err)
	}
}

func TestCreateWorkspaceRejectsInvalidLanguageBeforeCreatingDirectory(t *testing.T) {
	workspacePath := filepath.Join(t.TempDir(), "must-not-exist")
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	body := strings.NewReader(fmt.Sprintf(`{"path":%q,"create":true,"language":"fr","initialUserName":"Alice"}`, workspacePath))
	recorder := httptest.NewRecorder()
	s.handleWorkspaces(recorder, httptest.NewRequest(http.MethodPost, "/api/workspaces", body))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid language returned %d: %s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(workspacePath); !os.IsNotExist(err) {
		t.Fatalf("invalid language created the directory: %v", err)
	}
}

func TestWorkspaceWikiPreviewIsScopedAndReadable(t *testing.T) {
	workspace := t.TempDir()
	wikiDir := filepath.Join(workspace, "wiki")
	if err := os.MkdirAll(filepath.Join(wikiDir, "guides"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wikiDir, "guides", "notes.md"), []byte("# Notes\n\nSafe content.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Path: workspace}}}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/files?path=wiki%2Fguides%2Fnotes.md", nil)
	rec := httptest.NewRecorder()
	s.handleWorkspace(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected Wiki Markdown preview, got %d: %s", rec.Code, rec.Body.String())
	}
	var preview filePreview
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	expectedDigest := sha256.Sum256([]byte("# Notes\n\nSafe content.\n"))
	if preview.Path != "wiki/guides/notes.md" || preview.Binary || !strings.Contains(preview.Content, "Safe content") || preview.ContentHash != hex.EncodeToString(expectedDigest[:]) {
		t.Fatalf("unexpected Wiki preview: %+v", preview)
	}

	for _, path := range []string{"../outside.txt", "wiki/guides/../../../outside.txt", "/etc/passwd"} {
		req := httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/files?path="+path, nil)
		rec := httptest.NewRecorder()
		s.handleWorkspace(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected Wiki traversal %q to be rejected, got %d: %s", path, rec.Code, rec.Body.String())
		}
	}

	if err := os.Symlink(outside, filepath.Join(wikiDir, "outside-link.txt")); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"files?path=wiki/outside-link.txt", "files/raw?path=wiki/outside-link.txt"} {
		req := httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/"+suffix, nil)
		rec := httptest.NewRecorder()
		s.handleWorkspace(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected external Wiki symlink to be rejected, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/files?path=wiki/missing.md", nil)
	rec = httptest.NewRecorder()
	s.handleWorkspace(rec, req)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no such file") {
		t.Fatalf("expected a clear missing Wiki file response, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestWorkspaceFileLinkResolvesSluggedDirectories(t *testing.T) {
	workspace := t.TempDir()
	puaWorkspace, err := app.Initialize(workspace, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.CreateProject("API project", "pua"); err != nil {
		t.Fatal(err)
	}
	task, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: "project1", Title: "First task", Slug: "fix"})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := puaWorkspace.Resource(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Join(workspace, filepath.FromSlash(detail.Path), "artifacts")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("# Attachment\n\nHello link.\n")
	if err := os.WriteFile(filepath.Join(artifactDir, "foobar.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Path: workspace}}}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/files?path=project1%2Ftask1%2Fartifacts%2Ffoobar.md", nil)
	rec := httptest.NewRecorder()
	s.handleWorkspace(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected slug-resolved preview, got %d: %s", rec.Code, rec.Body.String())
	}
	var preview filePreview
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Path != "project1-pua/task1-fix/artifacts/foobar.md" || preview.Binary || !strings.Contains(preview.Content, "Hello link") {
		t.Fatalf("unexpected preview: %+v", preview)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/files/raw?path=project1/task1/artifacts/foobar.md", nil)
	rec = httptest.NewRecorder()
	s.handleWorkspace(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Hello link") {
		t.Fatalf("expected slug-resolved raw preview, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestWorkspaceFileLinkResolvesAbsolutePathsAndLineSuffixes(t *testing.T) {
	workspace := t.TempDir()
	projectDir := filepath.Join(workspace, "project1-pua")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(projectDir, "source.go")
	if err := os.WriteFile(sourcePath, []byte("package example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	colonPath := filepath.Join(projectDir, "source.go:42")
	if err := os.WriteFile(colonPath, []byte("literal colon file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		target  string
		wantAbs string
		wantRel string
	}{
		{name: "workspace root path", target: "/project1/artifacts/missing.md", wantAbs: filepath.Join(projectDir, "artifacts", "missing.md"), wantRel: "project1-pua/artifacts/missing.md"},
		{name: "absolute path", target: sourcePath, wantAbs: sourcePath, wantRel: "project1-pua/source.go"},
		{name: "line suffix", target: sourcePath + ":73", wantAbs: sourcePath, wantRel: "project1-pua/source.go"},
		{name: "line and column suffix", target: sourcePath + ":73:9", wantAbs: sourcePath, wantRel: "project1-pua/source.go"},
		{name: "existing colon filename wins", target: colonPath, wantAbs: colonPath, wantRel: "project1-pua/source.go:42"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotAbs, gotRel, err := resolveWorkspaceFileLink(workspace, test.target)
			if err != nil {
				t.Fatal(err)
			}
			if gotAbs != test.wantAbs || gotRel != test.wantRel {
				t.Fatalf("resolveWorkspaceFileLink(%q) = (%q, %q), want (%q, %q)", test.target, gotAbs, gotRel, test.wantAbs, test.wantRel)
			}
		})
	}
}

func TestWorkspaceFileLinkAbsolutePathCannotEscapeWorkspace(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	outsideRoot := filepath.Join(root, "workspace-other")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(outsideRoot, "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := resolveWorkspaceFileLink(workspace, outside); err == nil || !strings.Contains(err.Error(), "must be inside the workspace") {
		t.Fatalf("expected outside absolute path to be rejected, got %v", err)
	}

	escaped := workspace + string(filepath.Separator) + filepath.Join("nested", "..", "..", filepath.Base(outsideRoot), "secret.txt")
	if _, _, err := resolveWorkspaceFileLink(workspace, escaped); err == nil || !strings.Contains(err.Error(), "escapes the workspace") {
		t.Fatalf("expected Workspace-prefixed traversal to be rejected, got %v", err)
	}

	link := filepath.Join(workspace, "outside-link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveWorkspaceFileLink(workspace, link); err == nil || !strings.Contains(err.Error(), "escapes the workspace") {
		t.Fatalf("expected external symlink to be rejected, got %v", err)
	}
}

func TestCreateTaskMapsTemplateBodyAsCompleteMarkdown(t *testing.T) {
	workspace := t.TempDir()
	puaWorkspace, err := app.Initialize(workspace, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.CreateProject("API project", "api"); err != nil {
		t.Fatal(err)
	}

	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Path: workspace}}}); err != nil {
		t.Fatal(err)
	}

	body := `{"project":"project1","title":"Template task","taskMarkdown":"# Template task"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-one/tasks", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	s.createTask(rec, req, "workspace-one")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected OK, got %d: %s", rec.Code, rec.Body.String())
	}
	resource, err := puaWorkspace.Resource("project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := os.ReadFile(filepath.Join(workspace, filepath.FromSlash(resource.Path), "task.md"))
	if err != nil || string(markdown) != "# Template task" {
		t.Fatalf("template body was not written as complete markdown: err=%v content=%q", err, markdown)
	}
}

func TestRemovedAutomationHTTPInputsAreRejected(t *testing.T) {
	workspace := t.TempDir()
	puaWorkspace, err := app.Initialize(workspace, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.CreateProject("API project", "api"); err != nil {
		t.Fatal(err)
	}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Path: workspace}}}); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	s.createTask(recorder, httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-one/tasks", strings.NewReader(`{"project":"project1","title":"Task","selfDriving":true}`)), "workspace-one")
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "unknown field") {
		t.Fatalf("removed create field was accepted: %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	s.handleWorkspace(recorder, httptest.NewRequest(http.MethodPut, "/api/workspaces/workspace-one/self-driving", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("removed endpoint returned %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestLegacyAgentRunControlRouteIsGone(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/agent/runs", nil)
	(&server{}).handleWorkspace(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("legacy agent run route returned %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestArchiveResourceUsesUnifiedResourceCommand(t *testing.T) {
	workspace := t.TempDir()
	puaWorkspace, err := app.Initialize(workspace, "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := puaWorkspace.CreateProject("API project", "api")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Archive task", Slug: "archive"}); err != nil {
		t.Fatal(err)
	}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Path: workspace}}}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-one/archive", strings.NewReader(`{"resourceId":"project1.task1"}`))
	rec := httptest.NewRecorder()
	s.archiveResource(rec, req, "workspace-one")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected OK, got %d: %s", rec.Code, rec.Body.String())
	}
	var archived map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &archived); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(archived["path"], "archive") || len(archived) != 1 {
		t.Fatalf("unexpected archive response: %#v", archived)
	}
}

func TestArchiveResourceReturnsNonBlockingWarningsAfterMove(t *testing.T) {
	workspace := t.TempDir()
	puaWorkspace, err := app.Initialize(workspace, "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := puaWorkspace.CreateProject("Warning project", "warning")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Open child", Slug: "child"}); err != nil {
		t.Fatal(err)
	}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Path: workspace}}}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-one/archive", strings.NewReader(`{"resourceId":"project1"}`))
	rec := httptest.NewRecorder()
	s.archiveResource(rec, req, "workspace-one")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected OK with warnings, got %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Path     string               `json:"path"`
		Warnings []app.ArchiveWarning `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Path, "archive") || len(response.Warnings) == 0 || response.Warnings[0].Severity != "warning" {
		t.Fatalf("archive response did not expose structured warning: %#v", response)
	}
	if _, err := puaWorkspace.ResourceValue("project1.task1"); err != nil {
		t.Fatalf("archived child should remain addressable after HTTP archive: %v", err)
	}
}

func TestArchiveResourcePrunesPersistedUIState(t *testing.T) {
	workspace := t.TempDir()
	puaWorkspace, err := app.Initialize(workspace, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.RegisterUser(app.LegacyDefaultUserName); err != nil {
		t.Fatal(err)
	}
	project, err := puaWorkspace.CreateProject("Prune project", "prune")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Prune A", Slug: "prune-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Prune B", Slug: "prune-b"}); err != nil {
		t.Fatal(err)
	}
	keep, err := puaWorkspace.CreateProject("Keep project", "keep")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: keep.ID, Title: "Keep A", Slug: "keep-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: keep.ID, Title: "Keep B", Slug: "keep-b"}); err != nil {
		t.Fatal(err)
	}
	seed := uiState{
		Version:          1,
		ExpandedProjects: []string{"project1", "project2"},
		LastResourceID:   "project1.task1",
		ProjectOrder:     []string{"project1", "project2"},
		TaskOrder: map[string][]string{
			"project1": {"project1.task2", "project1.task1"},
			"project2": {"project2.task1", "project2.task2"},
		},
		ResourceStates: map[string]resourceUserState{
			"project1":       {ReadTurnNumber: intPointer(7)},
			"project1.task2": {ReadTurnNumber: intPointer(7)},
			"project2":       {ReadTurnNumber: intPointer(4)},
			"project2.task1": {ReadTurnNumber: intPointer(4)},
			"project2.task2": {ReadTurnNumber: intPointer(4)},
		},
	}
	if err := saveUIStateFile(userUIStatePath(workspace, app.DefaultUserName), seed); err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.RegisterUser("Alice"); err != nil {
		t.Fatal(err)
	}
	if err := saveUIStateFile(userUIStatePath(workspace, "Alice"), seed); err != nil {
		t.Fatal(err)
	}
	if err := saveResourceStateFile(resourceStatePath(workspace), resourceState{Version: 1, TurnNumbers: map[string]int{"project1": 7, "project2": 4}}); err != nil {
		t.Fatal(err)
	}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Path: workspace}}}); err != nil {
		t.Fatal(err)
	}

	archive := func(resourceID string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-one/archive", strings.NewReader(`{"resourceId":"`+resourceID+`"}`))
		rec := httptest.NewRecorder()
		s.archiveResource(rec, req, "workspace-one")
		if rec.Code != http.StatusOK {
			t.Fatalf("archive %s: expected OK, got %d: %s", resourceID, rec.Code, rec.Body.String())
		}
	}

	// Archiving a project prunes the project and every descendant task.
	archive("project1")
	state, err := loadUIStateFile(userUIStatePath(workspace, app.DefaultUserName))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(state.ExpandedProjects, ","); got != "project2" {
		t.Fatalf("expandedProjects not pruned: %v", state.ExpandedProjects)
	}
	if state.LastResourceID != "" {
		t.Fatalf("lastResourceId not cleared: %q", state.LastResourceID)
	}
	if got := strings.Join(state.ProjectOrder, ","); got != "project2" {
		t.Fatalf("projectOrder not pruned: %v", state.ProjectOrder)
	}
	if _, ok := state.TaskOrder["project1"]; ok {
		t.Fatalf("taskOrder for archived project retained: %v", state.TaskOrder)
	}
	for id := range state.ResourceStates {
		if id == "project1" || strings.HasPrefix(id, "project1.") {
			t.Fatalf("resource state for archived resource retained: %v", state.ResourceStates)
		}
	}
	if state.ResourceStates["project2"].ReadTurnNumber == nil {
		t.Fatalf("read cursor for unrelated project lost: %v", state.ResourceStates)
	}
	aliceState, err := loadUIStateFile(userUIStatePath(workspace, "Alice"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := aliceState.ResourceStates["project1"]; ok || aliceState.LastResourceID != "" {
		t.Fatalf("archived resource retained in second user's state: %#v", aliceState)
	}
	shared, err := loadResourceStateFile(resourceStatePath(workspace))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := shared.TurnNumbers["project1"]; ok || shared.TurnNumbers["project2"] != 4 {
		t.Fatalf("resource turn high-water marks not pruned selectively: %#v", shared.TurnNumbers)
	}

	// Archiving a single task prunes only that task and keeps its siblings.
	archive("project2.task1")
	state, err = loadUIStateFile(userUIStatePath(workspace, app.DefaultUserName))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.ResourceStates["project2.task1"]; ok {
		t.Fatalf("resource state for archived task retained: %v", state.ResourceStates)
	}
	if state.ResourceStates["project2"].ReadTurnNumber == nil || state.ResourceStates["project2.task2"].ReadTurnNumber == nil {
		t.Fatalf("read cursors for surviving resources lost: %v", state.ResourceStates)
	}
	if got := strings.Join(state.TaskOrder["project2"], ","); got != "project2.task2" {
		t.Fatalf("taskOrder for surviving project not pruned: %v", state.TaskOrder)
	}
	if got := strings.Join(state.ExpandedProjects, ","); got != "project2" {
		t.Fatalf("expandedProjects changed by task archive: %v", state.ExpandedProjects)
	}
}

func TestWorkspaceAgentsSaveRejectsChangedContentHash(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "serve.json")
	s := &server{config: configPath}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Name: "Test", Path: workspace.Root()}}}); err != nil {
		t.Fatal(err)
	}

	get := httptest.NewRecorder()
	s.handleWorkspace(get, httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/files?path=AGENTS.md", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("initial AGENTS.md preview returned %d: %s", get.Code, get.Body.String())
	}
	var preview filePreview
	if err := json.Unmarshal(get.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace.Root(), "AGENTS.md")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	external := append(append([]byte(nil), before...), []byte("\nExternal change.\n")...)
	if err := os.WriteFile(path, external, 0o644); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"content": "Unsaved browser draft", "expectedContentHash": preview.ContentHash})
	put := httptest.NewRecorder()
	s.handleWorkspace(put, httptest.NewRequest(http.MethodPut, "/api/workspaces/workspace-one/files?path=AGENTS.md", bytes.NewReader(body)))
	if put.Code != http.StatusConflict || !strings.Contains(put.Body.String(), "changed on disk") {
		t.Fatalf("stale AGENTS.md save returned %d %q, want conflict", put.Code, put.Body.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, external) {
		t.Fatal("stale AGENTS.md save overwrote the external change")
	}
}

func TestWorkspaceAgentsSaveWritesFullContent(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "serve.json")
	s := &server{config: configPath}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Name: "Test", Path: workspace.Root()}}}); err != nil {
		t.Fatal(err)
	}

	get := httptest.NewRecorder()
	s.handleWorkspace(get, httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/files?path=AGENTS.md", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("initial AGENTS.md preview returned %d: %s", get.Code, get.Body.String())
	}
	var preview filePreview
	if err := json.Unmarshal(get.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}

	fullContent := "# User notes\n\n<!-- managed by pua cli -->\nsystem\n<!-- end of pua cli prompt -->\n"
	body, _ := json.Marshal(map[string]string{"content": fullContent, "expectedContentHash": preview.ContentHash})
	put := httptest.NewRecorder()
	s.handleWorkspace(put, httptest.NewRequest(http.MethodPut, "/api/workspaces/workspace-one/files?path=AGENTS.md", bytes.NewReader(body)))
	if put.Code != http.StatusOK {
		t.Fatalf("AGENTS.md full save returned %d: %s", put.Code, put.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(workspace.Root(), "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != fullContent {
		t.Fatalf("AGENTS.md full save wrote unexpected content\nwant:\n%s\ngot:\n%s", fullContent, got)
	}
}

func TestRawFileDownloadServesAttachment(t *testing.T) {
	workspace := t.TempDir()
	textContent := []byte("hello artifact\n")
	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), textContent, 0o644); err != nil {
		t.Fatal(err)
	}
	binaryContent := []byte{'P', 'K', 0x00, 0x01, 0xff, 0x02}
	if err := os.WriteFile(filepath.Join(workspace, "bundle.zip"), binaryContent, 0o644); err != nil {
		t.Fatal(err)
	}

	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Path: workspace}}}); err != nil {
		t.Fatal(err)
	}

	// Binary files are rejected for inline raw preview.
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/files/raw?path=bundle.zip", nil)
	rec := httptest.NewRecorder()
	s.handleWorkspace(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected binary inline preview to be rejected, got %d: %s", rec.Code, rec.Body.String())
	}

	// The same binary file downloads as an attachment.
	req = httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/files/raw?path=bundle.zip&download=1", nil)
	rec = httptest.NewRecorder()
	s.handleWorkspace(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected binary download to succeed, got %d: %s", rec.Code, rec.Body.String())
	}
	if disposition := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(disposition, "attachment") || !strings.Contains(disposition, "bundle.zip") {
		t.Fatalf("unexpected Content-Disposition for download: %q", disposition)
	}
	if !bytes.Equal(rec.Body.Bytes(), binaryContent) {
		t.Fatalf("unexpected download body: %v", rec.Body.Bytes())
	}

	// Text files also download as attachments.
	req = httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/files/raw?path=notes.txt&download=1", nil)
	rec = httptest.NewRecorder()
	s.handleWorkspace(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected text download to succeed, got %d: %s", rec.Code, rec.Body.String())
	}
	if disposition := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(disposition, "attachment") || !strings.Contains(disposition, "notes.txt") {
		t.Fatalf("unexpected Content-Disposition for text download: %q", disposition)
	}
	if !bytes.Equal(rec.Body.Bytes(), textContent) {
		t.Fatalf("unexpected text download body: %q", rec.Body.String())
	}
}

func TestFileMimeTypeMarkdown(t *testing.T) {
	for _, name := range []string{"task.md", "README.markdown", "notes.mdown", "brief.mkdn"} {
		if got := fileMimeType(name, []byte("# Title\n")); got != "text/markdown" {
			t.Fatalf("fileMimeType(%q) = %q, want text/markdown", name, got)
		}
	}
}

func TestContentTypeWithCharset(t *testing.T) {
	if got := contentTypeWithCharset("text/markdown"); got != "text/markdown; charset=utf-8" {
		t.Fatalf("contentTypeWithCharset(text/markdown) = %q", got)
	}
	if got := contentTypeWithCharset("text/plain; charset=utf-8"); got != "text/plain; charset=utf-8" {
		t.Fatalf("contentTypeWithCharset should keep an existing charset, got %q", got)
	}
	if got := contentTypeWithCharset("image/png"); got != "image/png" {
		t.Fatalf("contentTypeWithCharset should leave non-text types alone, got %q", got)
	}
}

func TestRawFileServesUTF8Charset(t *testing.T) {
	workspace := t.TempDir()
	content := []byte("# 标题\n\n中文内容。\n")
	if err := os.WriteFile(filepath.Join(workspace, "notes.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "wiki", "notes.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Path: workspace}}}); err != nil {
		t.Fatal(err)
	}

	for _, suffix := range []string{"files/raw?path=notes.md", "files/raw?path=wiki/notes.md"} {
		req := httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/"+suffix, nil)
		rec := httptest.NewRecorder()
		s.handleWorkspace(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected raw preview for %q, got %d: %s", suffix, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "text/markdown; charset=utf-8" {
			t.Fatalf("raw preview for %q should declare UTF-8, got Content-Type %q", suffix, got)
		}
		if !strings.Contains(rec.Body.String(), "中文内容") {
			t.Fatalf("raw preview for %q lost UTF-8 content: %q", suffix, rec.Body.String())
		}
	}
}

func TestUIStateRoundTripsLastResource(t *testing.T) {
	workspace := t.TempDir()
	puaWorkspace, err := app.Initialize(workspace, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.RegisterUser(app.LegacyDefaultUserName); err != nil {
		t.Fatal(err)
	}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Path: workspace}}}); err != nil {
		t.Fatal(err)
	}

	put := httptest.NewRequest(http.MethodPut, "/api/workspaces/workspace-one/ui-state", strings.NewReader(`{"version":1,"expandedProjects":["project1"],"lastResourceId":"project1.task2"}`))
	put.Header.Set(workspaceUserHeader, app.LegacyDefaultUserName)
	rec := httptest.NewRecorder()
	s.handleWorkspace(rec, put)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected ui-state PUT to succeed, got %d: %s", rec.Code, rec.Body.String())
	}
	var saved uiState
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.LastResourceID != "project1.task2" {
		t.Fatalf("expected PUT response to echo lastResourceId, got %+v", saved)
	}

	get := httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/ui-state", nil)
	get.Header.Set(workspaceUserHeader, app.LegacyDefaultUserName)
	rec = httptest.NewRecorder()
	s.handleWorkspace(rec, get)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected ui-state GET to succeed, got %d: %s", rec.Code, rec.Body.String())
	}
	var loaded uiState
	if err := json.Unmarshal(rec.Body.Bytes(), &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.LastResourceID != "project1.task2" || len(loaded.ExpandedProjects) != 1 || loaded.ExpandedProjects[0] != "project1" {
		t.Fatalf("expected persisted ui-state, got %+v", loaded)
	}

	data, err := os.ReadFile(filepath.Join(workspace, ".pua", "users", "User", "ui-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"lastResourceId": "project1.task2"`) {
		t.Fatalf("expected ui-state.json to persist lastResourceId, got %s", data)
	}
}

func TestUIStateRoundTripsCustomOrder(t *testing.T) {
	workspace := t.TempDir()
	puaWorkspace, err := app.Initialize(workspace, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.RegisterUser(app.LegacyDefaultUserName); err != nil {
		t.Fatal(err)
	}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Path: workspace}}}); err != nil {
		t.Fatal(err)
	}

	put := httptest.NewRequest(http.MethodPut, "/api/workspaces/workspace-one/ui-state", strings.NewReader(`{"version":1,"expandedProjects":[],"projectOrder":["project2","project1"],"taskOrder":{"project1":["project1.task3","project1.task1"]}}`))
	put.Header.Set(workspaceUserHeader, app.LegacyDefaultUserName)
	rec := httptest.NewRecorder()
	s.handleWorkspace(rec, put)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected ui-state PUT to succeed, got %d: %s", rec.Code, rec.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/ui-state", nil)
	get.Header.Set(workspaceUserHeader, app.LegacyDefaultUserName)
	rec = httptest.NewRecorder()
	s.handleWorkspace(rec, get)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected ui-state GET to succeed, got %d: %s", rec.Code, rec.Body.String())
	}
	var loaded uiState
	if err := json.Unmarshal(rec.Body.Bytes(), &loaded); err != nil {
		t.Fatal(err)
	}
	if len(loaded.ProjectOrder) != 2 || loaded.ProjectOrder[0] != "project2" || loaded.ProjectOrder[1] != "project1" {
		t.Fatalf("expected persisted project order, got %+v", loaded.ProjectOrder)
	}
	if len(loaded.TaskOrder["project1"]) != 2 || loaded.TaskOrder["project1"][0] != "project1.task3" {
		t.Fatalf("expected persisted task order, got %+v", loaded.TaskOrder)
	}

	data, err := os.ReadFile(filepath.Join(workspace, ".pua", "users", "User", "ui-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"projectOrder"`) || !strings.Contains(string(data), `"taskOrder"`) || strings.Contains(string(data), `"sessionOrder"`) {
		t.Fatalf("expected ui-state.json to persist custom order fields, got %s", data)
	}
}

func TestUIStateRoundTripsFolders(t *testing.T) {
	workspace := t.TempDir()
	puaWorkspace, err := app.Initialize(workspace, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.RegisterUser(app.LegacyDefaultUserName); err != nil {
		t.Fatal(err)
	}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Path: workspace}}}); err != nil {
		t.Fatal(err)
	}

	put := httptest.NewRequest(http.MethodPut, "/api/workspaces/workspace-one/ui-state", strings.NewReader(`{"version":1,"expandedProjects":[],"taskOrder":{"project1":["project1.task1","vf-one"]},"folders":[{"id":"vf-one","projectId":"project1","name":"  Grouped  ","expanded":true},{"id":"vf-one","projectId":"project1","name":"duplicate"},{"id":"","projectId":"project1","name":"no id"}],"folderOrder":{"vf-one":["project1.task2"],"vf-missing":["project1.task3"]}}`))
	put.Header.Set(workspaceUserHeader, app.LegacyDefaultUserName)
	rec := httptest.NewRecorder()
	s.handleWorkspace(rec, put)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected ui-state PUT to succeed, got %d: %s", rec.Code, rec.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "/api/workspaces/workspace-one/ui-state", nil)
	get.Header.Set(workspaceUserHeader, app.LegacyDefaultUserName)
	rec = httptest.NewRecorder()
	s.handleWorkspace(rec, get)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected ui-state GET to succeed, got %d: %s", rec.Code, rec.Body.String())
	}
	var loaded uiState
	if err := json.Unmarshal(rec.Body.Bytes(), &loaded); err != nil {
		t.Fatal(err)
	}
	if len(loaded.Folders) != 1 {
		t.Fatalf("expected only the valid folder to persist, got %+v", loaded.Folders)
	}
	folder := loaded.Folders[0]
	if folder.ID != "vf-one" || folder.ProjectID != "project1" || folder.Name != "Grouped" || !folder.Expanded {
		t.Fatalf("expected normalized folder, got %+v", folder)
	}
	if _, ok := loaded.FolderOrder["vf-missing"]; ok {
		t.Fatalf("expected folderOrder for unknown folder to be dropped, got %+v", loaded.FolderOrder)
	}
	if got := strings.Join(loaded.FolderOrder["vf-one"], ","); got != "project1.task2" {
		t.Fatalf("expected persisted folder order, got %+v", loaded.FolderOrder)
	}
	if got := strings.Join(loaded.TaskOrder["project1"], ","); got != "project1.task1,vf-one" {
		t.Fatalf("expected task order with folder id, got %+v", loaded.TaskOrder)
	}
}

func TestArchiveResourcePrunesPersistedFolders(t *testing.T) {
	workspace := t.TempDir()
	puaWorkspace, err := app.Initialize(workspace, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.RegisterUser(app.LegacyDefaultUserName); err != nil {
		t.Fatal(err)
	}
	project, err := puaWorkspace.CreateProject("Folder project", "folder")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Folder A", Slug: "folder-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Folder B", Slug: "folder-b"}); err != nil {
		t.Fatal(err)
	}
	keep, err := puaWorkspace.CreateProject("Keep project", "keep")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: keep.ID, Title: "Keep A", Slug: "keep-a"}); err != nil {
		t.Fatal(err)
	}
	seed := uiState{
		Version:          2,
		ExpandedProjects: []string{"project1", "project2"},
		TaskOrder: map[string][]string{
			"project1": {"project1.task1", "vf-one"},
			"project2": {"project2.task1", "vf-two"},
		},
		Folders: []uiStateFolder{
			{ID: "vf-one", ProjectID: "project1", Name: "One", Expanded: true},
			{ID: "vf-two", ProjectID: "project2", Name: "Two"},
		},
		FolderOrder: map[string][]string{
			"vf-one": {"project1.task2", "project1.task1"},
			"vf-two": {"project2.task1"},
		},
		ResourceStates: map[string]resourceUserState{},
	}
	if err := saveUIStateFile(userUIStatePath(workspace, app.DefaultUserName), seed); err != nil {
		t.Fatal(err)
	}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Path: workspace}}}); err != nil {
		t.Fatal(err)
	}

	archive := func(resourceID string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-one/archive", strings.NewReader(`{"resourceId":"`+resourceID+`"}`))
		rec := httptest.NewRecorder()
		s.archiveResource(rec, req, "workspace-one")
		if rec.Code != http.StatusOK {
			t.Fatalf("archive %s: expected OK, got %d: %s", resourceID, rec.Code, rec.Body.String())
		}
	}

	// Archiving a single task removes it from its folder but keeps the folder.
	archive("project1.task1")
	state, err := loadUIStateFile(userUIStatePath(workspace, app.DefaultUserName))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(state.FolderOrder["vf-one"], ","); got != "project1.task2" {
		t.Fatalf("archived task retained in folder order: %v", state.FolderOrder)
	}
	if got := strings.Join(state.TaskOrder["project1"], ","); got != "vf-one" {
		t.Fatalf("archived task retained in task order: %v", state.TaskOrder)
	}
	if len(state.Folders) != 2 {
		t.Fatalf("folders lost on task archive: %+v", state.Folders)
	}

	// Archiving a project removes its folders and their order entries.
	archive("project1")
	state, err = loadUIStateFile(userUIStatePath(workspace, app.DefaultUserName))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Folders) != 1 || state.Folders[0].ID != "vf-two" {
		t.Fatalf("folders for archived project retained: %+v", state.Folders)
	}
	if _, ok := state.FolderOrder["vf-one"]; ok {
		t.Fatalf("folder order for archived project retained: %v", state.FolderOrder)
	}
	if got := strings.Join(state.FolderOrder["vf-two"], ","); got != "project2.task1" {
		t.Fatalf("folder order for surviving project lost: %v", state.FolderOrder)
	}
}
