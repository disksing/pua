package app_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func TestApplicationAPIProvidesTheResourceLifecycle(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := workspace.CreateProject("Application boundary", "application")
	if err != nil {
		t.Fatal(err)
	}
	task, err := workspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Exercise API", Slug: "api"})
	if err != nil {
		t.Fatal(err)
	}

	taskResult, err := workspace.ResourceValue(task.ID)
	if err != nil || taskResult.Task == nil {
		t.Fatalf("resource value = %#v, %v", taskResult, err)
	}
	taskPath := filepath.Join(root, filepath.FromSlash(taskResult.Path))
	nested := filepath.Join(taskPath, "worktree", "example")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	discovered, err := app.OpenWorkspaceFrom(nested)
	if err != nil || discovered.Root() != workspace.Root() {
		t.Fatalf("discovered Workspace = %#v, %v", discovered, err)
	}
	if got, ok, err := workspace.InferProjectID(nested); err != nil || !ok || got != project.ID {
		t.Fatalf("inferred project = %q, %v, %v", got, ok, err)
	}
	if got, ok, err := workspace.InferTaskID(nested); err != nil || !ok || got != task.ID {
		t.Fatalf("inferred task = %q, %v, %v", got, ok, err)
	}

	repoPath := filepath.Join(root, "repos", "example")
	gitDir := filepath.Join(repoPath, ".git", "worktrees", "example")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	detail, err := workspace.Resource(task.ID)
	if err != nil || len(detail.Repos) != 1 {
		t.Fatalf("resource detail = %#v, %v", detail, err)
	}
	if got := detail.Repos[0]; got.Name != "example" || got.RepoPath != "repos/example" || got.Branch != "feature" || got.WorktreePath != taskResult.Path+"/worktree/example" {
		t.Fatalf("discovered repo = %#v", got)
	}
	tree, err := workspace.Tree()
	if err != nil || len(tree.Projects) != 1 || len(tree.Projects[0].Children) != 1 {
		t.Fatalf("Workspace tree = %#v, %v", tree, err)
	}
	if err := os.RemoveAll(nested); err != nil {
		t.Fatal(err)
	}
	detail, err = workspace.Resource(task.ID)
	if err != nil || len(detail.Repos) != 0 {
		t.Fatalf("resource detail after worktree removal = %#v, %v", detail, err)
	}

	archivedTask, err := workspace.ArchiveResource(task.ID)
	if err != nil || archivedTask.Path != "project1-application/archive/task1-api" {
		t.Fatalf("archive task = %#v, %v", archivedTask, err)
	}
	tree, err = workspace.Tree()
	if err != nil || len(tree.Projects) != 1 || len(tree.Projects[0].Children) != 0 {
		t.Fatalf("Workspace tree includes archived task = %#v, %v", tree, err)
	}
	detail, err = workspace.Resource(project.ID)
	if err != nil || len(detail.Children) != 1 || !detail.Children[0].Archived {
		t.Fatalf("project detail lost archived task = %#v, %v", detail.Children, err)
	}
	archivedProject, err := workspace.ArchiveResource(project.ID)
	if err != nil || archivedProject.Path != "archive/project1-application" {
		t.Fatalf("archive project = %#v, %v", archivedProject, err)
	}
}

func TestTaskWorkflowStateLifecycle(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := workspace.CreateProject("States", "states")
	if err != nil {
		t.Fatal(err)
	}
	task, err := workspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Stateful Task"})
	if err != nil {
		t.Fatal(err)
	}
	if task.State != app.TaskStateNotStarted || task.StateUpdatedAt == "" {
		t.Fatalf("new task state = %#v", task)
	}
	if _, err := workspace.SetTaskState(task.ID, app.TaskStateWaiting, ""); err == nil {
		t.Fatal("waiting without a note succeeded")
	}
	if _, err := workspace.SetTaskState(task.ID, app.TaskStateWaiting, "line one\nline two"); err == nil {
		t.Fatal("multiline state note succeeded")
	}
	updated, err := workspace.SetTaskState(task.ID, app.TaskStateBlocked, "Need user approval")
	if err != nil || updated.State != app.TaskStateBlocked || updated.StateNote != "Need user approval" || updated.StateUpdatedAt == "" {
		t.Fatalf("updated task state = %#v, %v", updated, err)
	}
	tree, err := workspace.Tree()
	if err != nil || tree.Projects[0].Children[0].State != app.TaskStateBlocked || tree.Projects[0].Children[0].StateNote != "Need user approval" {
		t.Fatalf("tree task state = %#v, %v", tree.Projects, err)
	}
}

func TestProjectArchiveCascadesChildrenAndRepairsWorktreeReferences(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := workspace.CreateProject("Archive cascade", "cascade")
	if err != nil {
		t.Fatal(err)
	}
	first, err := workspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Open child", Slug: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := workspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Another child", Slug: "second"})
	if err != nil {
		t.Fatal(err)
	}
	repoPath := filepath.Join(root, "repos", "example")
	gitDir := filepath.Join(repoPath, ".git", "worktrees", "example")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	worktreePath := filepath.Join(root, filepath.FromSlash(first.Path), "worktree", "example")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	projectValue, err := workspace.ResourceValue(project.ID)
	if err != nil {
		t.Fatal(err)
	}

	result, err := workspace.ArchiveResource(project.ID)
	if err != nil {
		t.Fatalf("project archive failed: %v", err)
	}
	if result.Path != filepath.ToSlash(filepath.Join("archive", filepath.Base(filepath.FromSlash(projectValue.Path)))) {
		t.Fatalf("unexpected project archive path: %#v", result)
	}
	openWarnings := 0
	for _, warning := range result.Warnings {
		if warning.Code == "open_child_task" {
			openWarnings++
		}
	}
	if openWarnings != 2 {
		t.Fatalf("project archive warnings = %#v, want one open-child warning per child", result.Warnings)
	}
	firstDir := filepath.Base(filepath.FromSlash(first.Path))
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(result.Path), firstDir, "task.json")); err != nil {
		t.Fatalf("first child was not moved with its project: %v", err)
	}

	archivedFirst, err := workspace.ResourceValue(first.ID)
	if err != nil || archivedFirst.Task == nil || !archivedFirst.Archived {
		t.Fatalf("archived first child = %#v, %v", archivedFirst, err)
	}
	firstDetail, err := workspace.Resource(first.ID)
	if err != nil || len(firstDetail.Repos) != 1 {
		t.Fatalf("archived first child detail = %#v, %v", firstDetail, err)
	}
	if got := firstDetail.Repos[0].WorktreePath; !strings.HasPrefix(got, "archive/") || !strings.Contains(got, "/"+firstDir+"/worktree/example") {
		t.Fatalf("first child worktree was not discovered after archive: %q", got)
	}
	archivedSecond, err := workspace.ResourceValue(second.ID)
	if err != nil || archivedSecond.Task == nil || !archivedSecond.Archived {
		t.Fatalf("archived second child = %#v, %v", archivedSecond, err)
	}
	listed, err := workspace.Tasks(app.TaskListOptions{ProjectID: project.ID, IncludeArchived: true})
	if err != nil || len(listed.Tasks) != 2 || !listed.Tasks[0].Archived || !listed.Tasks[1].Archived {
		t.Fatalf("archived project children = %#v, %v", listed, err)
	}
	detail, err := workspace.Resource(project.ID)
	if err != nil || len(detail.Children) != 2 || !detail.Children[0].Archived || !detail.Children[1].Archived {
		t.Fatalf("archived project detail children = %#v, %v", detail.Children, err)
	}
}
func TestConcurrentResourceCreationAllocatesUniqueAtomicIDs(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	const count = 12
	projects := make(chan app.Project, count)
	errors := make(chan error, count)
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			project, createErr := workspace.CreateProject(fmt.Sprintf("Concurrent project %d", index), fmt.Sprintf("concurrent-%d", index))
			if createErr != nil {
				errors <- createErr
				return
			}
			projects <- project
		}(index)
	}
	wait.Wait()
	close(errors)
	for createErr := range errors {
		t.Fatal(createErr)
	}
	close(projects)
	seen := make(map[string]bool)
	for project := range projects {
		if seen[project.ID] {
			t.Fatalf("duplicate project id %s", project.ID)
		}
		seen[project.ID] = true
	}
	if len(seen) != count {
		t.Fatalf("created %d projects, want %d", len(seen), count)
	}
	matches, err := filepath.Glob(filepath.Join(workspace.Root(), ".pua-create-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("partial staging directories = %#v, %v", matches, err)
	}
}

func TestConcurrentWorkspaceInitializationConvergesOnOneInstance(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	type result struct {
		workspace *app.Workspace
		err       error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for index := 0; index < 2; index++ {
		go func() {
			<-start
			workspace, err := app.Initialize(root, "en")
			results <- result{workspace: workspace, err: err}
		}()
	}
	close(start)
	instanceIDs := make(map[string]bool)
	for index := 0; index < 2; index++ {
		current := <-results
		if current.err != nil {
			continue
		}
		runtime, err := current.workspace.RuntimeConfig()
		if err != nil {
			t.Fatal(err)
		}
		instanceIDs[runtime.InstanceID] = true
	}
	opened, err := app.OpenWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := opened.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	instanceIDs[runtime.InstanceID] = true
	if len(instanceIDs) != 1 {
		t.Fatalf("concurrent initialization created multiple logical instances: %#v", instanceIDs)
	}
}

func TestApplicationSelectorsAreExplicitAndCanonical(t *testing.T) {
	if got, err := app.NormalizeProjectID("12"); err != nil || got != "project12" {
		t.Fatalf("NormalizeProjectID = %q, %v", got, err)
	}
	if got, err := app.NormalizeTaskName("7"); err != nil || got != "task7" {
		t.Fatalf("NormalizeTaskName = %q, %v", got, err)
	}
	if got, err := app.NormalizeTaskID("12", "7"); err != nil || got != "project12.task7" {
		t.Fatalf("NormalizeTaskID = %q, %v", got, err)
	}
	if _, err := app.NormalizeTaskID("", "task7"); err == nil {
		t.Fatal("NormalizeTaskID inferred a project instead of requiring one")
	}
}

func TestMigrateDropsRemovedAgentCommandConfig(t *testing.T) {
	root := t.TempDir()
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "workspace.json")
	if err := os.WriteFile(configPath, []byte(`{"version":1,"language":"en","agentCommand":["legacy"]}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Migrate(""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if _, exists := config["agentCommand"]; exists {
		t.Fatalf("removed agentCommand survived migration: %s", data)
	}
}

func TestResourceDetailToleratesDeletedMarkdownAndDirectories(t *testing.T) {
	root := t.TempDir()
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := workspace.CreateProject("Missing files", "missing")
	if err != nil {
		t.Fatal(err)
	}
	task, err := workspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Deleted docs", Slug: "docs"})
	if err != nil {
		t.Fatal(err)
	}

	taskValue, err := workspace.ResourceValue(task.ID)
	if err != nil || taskValue.Task == nil {
		t.Fatalf("resource value = %#v, %v", taskValue, err)
	}
	taskPath := filepath.Join(root, filepath.FromSlash(taskValue.Path))
	if err := os.Remove(filepath.Join(taskPath, "task.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(taskPath, "artifacts")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(taskPath, "worktree")); err != nil {
		t.Fatal(err)
	}

	detail, err := workspace.Resource(task.ID)
	if err != nil {
		t.Fatalf("resource detail = %#v, %v", detail, err)
	}
	if len(detail.Files) != 2 || detail.Files[0].Name != "chat.md" || detail.Files[1].Name != "AGENTS.md" {
		t.Fatalf("expected chat.md and AGENTS.md after deleting task.md, got %#v", detail.Files)
	}
	if detail.Artifacts == nil || len(detail.Artifacts) != 0 {
		t.Fatalf("expected empty artifacts slice after deleting artifacts dir, got %#v", detail.Artifacts)
	}
	if detail.Worktrees == nil || len(detail.Worktrees) != 0 {
		t.Fatalf("expected empty worktrees slice after deleting worktree dir, got %#v", detail.Worktrees)
	}

	projectValue, err := workspace.ResourceValue(project.ID)
	if err != nil || projectValue.Project == nil {
		t.Fatalf("resource value = %#v, %v", projectValue, err)
	}
	projectPath := filepath.Join(root, filepath.FromSlash(projectValue.Path))
	if err := os.Remove(filepath.Join(projectPath, "project.md")); err != nil {
		t.Fatal(err)
	}
	projectDetail, err := workspace.Resource(project.ID)
	if err != nil {
		t.Fatalf("project detail = %#v, %v", projectDetail, err)
	}
	if len(projectDetail.Files) != 2 || projectDetail.Files[0].Name != "chat.md" || projectDetail.Files[1].Name != "AGENTS.md" {
		t.Fatalf("expected chat.md and AGENTS.md after deleting project.md, got %#v", projectDetail.Files)
	}
}
