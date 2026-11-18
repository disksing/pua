package serve

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/app"
)

type artifactDeleteTestFixture struct {
	server      *server
	workspace   *app.Workspace
	root        string
	projectPath string
	taskPath    string
}

func newArtifactDeleteTestFixture(t *testing.T) artifactDeleteTestFixture {
	t.Helper()
	root := t.TempDir()
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := workspace.CreateProject("Deletable project", "deletable")
	if err != nil {
		t.Fatal(err)
	}
	task, err := workspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Deletable task", Slug: "deletable-task"})
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
		filepath.Join(projectPath, "artifacts", "notes.md"):              "# Project notes\n",
		filepath.Join(taskPath, "artifacts", "report.txt"):               "plain text\n",
		filepath.Join(taskPath, "artifacts", "nested", "screenshot.png"): "fake png\n",
		filepath.Join(taskPath, "artifacts", "upload", "attachment.zip"): "fake zip\n",
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
	return artifactDeleteTestFixture{server: s, workspace: workspace, root: root, projectPath: projectPath, taskPath: taskPath}
}

func artifactDeleteRequest(t *testing.T, fixture artifactDeleteTestFixture, method, resourceID, path string) *httptest.ResponseRecorder {
	t.Helper()
	requestURL := "/api/workspaces/workspace-one/resources/" + resourceID + "/artifacts?path=" + url.QueryEscape(path)
	recorder := httptest.NewRecorder()
	fixture.server.handleWorkspace(recorder, httptest.NewRequest(method, requestURL, nil))
	return recorder
}

func TestResourceArtifactDeleteRemovesOwnedFiles(t *testing.T) {
	fixture := newArtifactDeleteTestFixture(t)
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
		{name: "project artifact", resourceID: "project1", path: project.Path + "/artifacts/notes.md", abs: filepath.Join(fixture.projectPath, "artifacts", "notes.md")},
		{name: "task text artifact", resourceID: "project1.task1", path: task.Path + "/artifacts/report.txt", abs: filepath.Join(fixture.taskPath, "artifacts", "report.txt")},
		{name: "nested task artifact", resourceID: "project1.task1", path: task.Path + "/artifacts/nested/screenshot.png", abs: filepath.Join(fixture.taskPath, "artifacts", "nested", "screenshot.png")},
		{name: "uploaded artifact", resourceID: "project1.task1", path: task.Path + "/artifacts/upload/attachment.zip", abs: filepath.Join(fixture.taskPath, "artifacts", "upload", "attachment.zip")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := artifactDeleteRequest(t, fixture, http.MethodDelete, test.resourceID, test.path)
			if recorder.Code != http.StatusOK {
				t.Fatalf("delete returned %d: %s", recorder.Code, recorder.Body.String())
			}
			if _, err := os.Lstat(test.abs); !os.IsNotExist(err) {
				t.Fatalf("artifact still exists after delete, err=%v", err)
			}
		})
	}
}

func TestResourceArtifactDeleteRejectsUnownedAndUnsafePaths(t *testing.T) {
	fixture := newArtifactDeleteTestFixture(t)
	project, _ := fixture.workspace.Resource("project1")
	task, _ := fixture.workspace.Resource("project1.task1")
	rejections := []struct {
		name       string
		resourceID string
		path       string
	}{
		{name: "task brief", resourceID: "project1.task1", path: task.Path + "/task.md"},
		{name: "resource metadata", resourceID: "project1.task1", path: task.Path + "/task.json"},
		{name: "other resource artifact", resourceID: "project1", path: task.Path + "/artifacts/report.txt"},
		{name: "project template", resourceID: "project1", path: project.Path + "/templates/review.md"},
		{name: "artifacts directory itself", resourceID: "project1.task1", path: task.Path + "/artifacts"},
		{name: "nested directory", resourceID: "project1.task1", path: task.Path + "/artifacts/nested"},
		{name: "path escape", resourceID: "project1.task1", path: task.Path + "/artifacts/../task.md"},
		{name: "workspace escape", resourceID: "project1.task1", path: "../../outside.md"},
	}
	for _, test := range rejections {
		t.Run(test.name, func(t *testing.T) {
			recorder := artifactDeleteRequest(t, fixture, http.MethodDelete, test.resourceID, test.path)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("rejected delete returned %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}

	link := filepath.Join(fixture.taskPath, "artifacts", "linked.txt")
	if err := os.Symlink(filepath.Join(fixture.taskPath, "spec.md"), link); err != nil {
		t.Fatal(err)
	}
	recorder := artifactDeleteRequest(t, fixture, http.MethodDelete, "project1.task1", task.Path+"/artifacts/linked.txt")
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "symbolic link") {
		t.Fatalf("symlink delete returned %d: %s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("symlink was removed: %v", err)
	}
}

func TestResourceArtifactDeleteRejectsMissingArchivedAndWrongMethod(t *testing.T) {
	fixture := newArtifactDeleteTestFixture(t)
	task, _ := fixture.workspace.Resource("project1.task1")

	recorder := artifactDeleteRequest(t, fixture, http.MethodDelete, "project1.task1", task.Path+"/artifacts/missing.txt")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing delete returned %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder = artifactDeleteRequest(t, fixture, http.MethodDelete, "project1.task1", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty path delete returned %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder = artifactDeleteRequest(t, fixture, http.MethodGet, "project1.task1", task.Path+"/artifacts/report.txt")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET delete route returned %d", recorder.Code)
	}

	if _, err := fixture.workspace.ArchiveResource("project1.task1"); err != nil {
		t.Fatal(err)
	}
	archived, err := fixture.workspace.Resource("project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	recorder = artifactDeleteRequest(t, fixture, http.MethodDelete, "project1.task1", archived.Path+"/artifacts/report.txt")
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "archived") {
		t.Fatalf("archived delete returned %d: %s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(fixture.root, filepath.FromSlash(archived.Path), "artifacts", "report.txt")); err != nil {
		t.Fatalf("archived artifact was removed: %v", err)
	}
}
