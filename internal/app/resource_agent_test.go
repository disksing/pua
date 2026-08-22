package app_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func TestResourceAgentBindingsAreExplicitAndStable(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := workspace.CreateProject("Project", "project")
	if err != nil {
		t.Fatal(err)
	}
	task, err := workspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Task", Slug: "task"})
	if err != nil {
		t.Fatal(err)
	}

	runtime, err := workspace.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.InstanceID == "" || runtime.AgentBinding != (app.AgentBinding{Kind: "profile", Name: "default"}) {
		t.Fatalf("unexpected Workspace runtime: %#v", runtime)
	}
	for _, id := range []string{project.ID, task.ID} {
		binding, err := workspace.ResourceAgentBinding(id)
		if err != nil || binding != (app.AgentBinding{Kind: "profile", Name: "default"}) {
			t.Fatalf("resource %s binding=%#v err=%v", id, binding, err)
		}
	}

	direct := app.AgentBinding{Kind: "agent", Name: "gpt-test"}
	if _, err := workspace.SetResourceAgentBinding(project.ID, direct); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.SetResourceAgentDefaults(app.ResourceAgentDefaults{
		Project: app.AgentBinding{Kind: "profile", Name: "fast"},
		Task:    app.AgentBinding{Kind: "profile", Name: "reasoning"},
	}); err != nil {
		t.Fatal(err)
	}
	after, err := workspace.EnsureResourceRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if after.InstanceID != runtime.InstanceID || after.AgentBinding != runtime.AgentBinding {
		t.Fatalf("runtime migration overwrote stable Workspace identity or binding: before=%#v after=%#v", runtime, after)
	}
	if got, err := workspace.ResourceAgentBinding(project.ID); err != nil || got != direct {
		t.Fatalf("migration overwrote direct Agent binding: got=%#v err=%v", got, err)
	}
	newProject, err := workspace.CreateProject("Typed default project", "typed-default")
	if err != nil {
		t.Fatal(err)
	}
	newTask, err := workspace.CreateTask(app.CreateTaskInput{ProjectID: newProject.ID, Title: "Typed default task", Slug: "typed-default"})
	if err != nil {
		t.Fatal(err)
	}
	if newProject.AgentBinding != (app.AgentBinding{Kind: "profile", Name: "fast"}) {
		t.Fatalf("new Project did not use persisted Project default: %#v", newProject.AgentBinding)
	}
	if newTask.AgentBinding != (app.AgentBinding{Kind: "profile", Name: "reasoning"}) {
		t.Fatalf("new Task did not use persisted Task default: %#v", newTask.AgentBinding)
	}
	info, err := os.Stat(filepath.Join(workspace.Root(), ".pua", "runtime"))
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("runtime directory permissions mismatch: info=%v err=%v", info, err)
	}
}

func TestGenerationPolicyDefaultsAndPersistsWorkspaceOverride(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := workspace.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	wantDefault := app.GenerationPolicy{
		Enabled: true, MaxTurns: app.DefaultGenerationMaxTurns,
		MaxAccumulatedTurnMinutes: app.DefaultGenerationMaxAccumulatedTurnMinutes,
	}
	if runtime.GenerationPolicy != wantDefault {
		t.Fatalf("default generation policy = %#v, want %#v", runtime.GenerationPolicy, wantDefault)
	}

	wantDisabled := app.GenerationPolicy{Enabled: false, MaxTurns: 30, MaxAccumulatedTurnMinutes: 180}
	if saved, err := workspace.SetGenerationPolicy(wantDisabled); err != nil || saved != wantDisabled {
		t.Fatalf("save generation policy = %#v, %v", saved, err)
	}
	reopened, err := app.OpenWorkspace(workspace.Root())
	if err != nil {
		t.Fatal(err)
	}
	runtime, err = reopened.RuntimeConfig()
	if err != nil || runtime.GenerationPolicy != wantDisabled {
		t.Fatalf("reopened generation policy = %#v, %v", runtime.GenerationPolicy, err)
	}

	if _, err := workspace.SetGenerationPolicy(app.GenerationPolicy{Enabled: true}); err == nil {
		t.Fatal("enabled generation policy accepted two disabled budget dimensions")
	}
}

func TestStallWatchdogPolicyDefaultsAndPersistsWorkspaceOverride(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := workspace.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	wantDefault := app.StallWatchdogPolicy{Enabled: true, TimeoutMinutes: app.DefaultStallWatchdogTimeoutMinutes}
	if runtime.StallWatchdogPolicy != wantDefault {
		t.Fatalf("default stall watchdog policy = %#v, want %#v", runtime.StallWatchdogPolicy, wantDefault)
	}

	wantDisabled := app.StallWatchdogPolicy{Enabled: false, TimeoutMinutes: 45}
	if saved, err := workspace.SetStallWatchdogPolicy(wantDisabled); err != nil || saved != wantDisabled {
		t.Fatalf("save stall watchdog policy = %#v, %v", saved, err)
	}
	reopened, err := app.OpenWorkspace(workspace.Root())
	if err != nil {
		t.Fatal(err)
	}
	runtime, err = reopened.RuntimeConfig()
	if err != nil || runtime.StallWatchdogPolicy != wantDisabled {
		t.Fatalf("reopened stall watchdog policy = %#v, %v", runtime.StallWatchdogPolicy, err)
	}
	if _, err := workspace.SetStallWatchdogPolicy(app.StallWatchdogPolicy{Enabled: true, TimeoutMinutes: 0}); err == nil {
		t.Fatal("invalid stall watchdog timeout was accepted")
	}
}

func TestResourceDefaultsAcceptDirectAgentBindings(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.EnsureResourceRuntime(); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.SetResourceAgentDefaults(app.ResourceAgentDefaults{
		Project: app.AgentBinding{Kind: "agent", Name: "Kimi-K3"},
		Task:    app.AgentBinding{Kind: "agent", Name: "Kimi-K3"},
	}); err != nil {
		t.Fatal(err)
	}
	project, err := workspace.CreateProject("Agent default project", "agent-default")
	if err != nil {
		t.Fatal(err)
	}
	if project.AgentBinding != (app.AgentBinding{Kind: "agent", Name: "Kimi-K3"}) {
		t.Fatalf("new Project did not use the direct Agent default: %#v", project.AgentBinding)
	}
	task, err := workspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Agent default task", Slug: "agent-default"})
	if err != nil {
		t.Fatal(err)
	}
	if task.AgentBinding != (app.AgentBinding{Kind: "agent", Name: "Kimi-K3"}) {
		t.Fatalf("new Task did not use the direct Agent default: %#v", task.AgentBinding)
	}
}

func TestLegacyStringResourceDefaultsDecodeAsProfiles(t *testing.T) {
	data := []byte(`{"version":1,"language":"en","resourceDefaults":{"workspace":"Fast","project":{"kind":"agent","name":"Kimi-K3"},"task":"default"}}`)
	var cfg app.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ResourceDefaults.Project != (app.AgentBinding{Kind: "agent", Name: "Kimi-K3"}) {
		t.Fatalf("structured Project default decoded as %#v", cfg.ResourceDefaults.Project)
	}
	if cfg.ResourceDefaults.Task != (app.AgentBinding{Kind: "profile", Name: "default"}) {
		t.Fatalf("legacy Task default decoded as %#v", cfg.ResourceDefaults.Task)
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "\"workspace\"") {
		t.Fatalf("removed Workspace default survived re-encoding: %s", out)
	}
}

func TestCreateTaskInheritsWorkspaceDefaultUnlessProjectOverrides(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.EnsureResourceRuntime(); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.SetResourceAgentDefaults(app.ResourceAgentDefaults{
		Project: app.AgentBinding{Kind: "profile", Name: "default"},
		Task:    app.AgentBinding{Kind: "profile", Name: "workspace-task"},
	}); err != nil {
		t.Fatal(err)
	}
	inheriting, err := workspace.CreateProject("Inheriting project", "inheriting")
	if err != nil {
		t.Fatal(err)
	}
	overriding, err := workspace.CreateProject("Overriding project", "overriding")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.SetProjectTaskDefault(overriding.ID, app.AgentBinding{Kind: "agent", Name: "project-agent"}); err != nil {
		t.Fatal(err)
	}
	inheritedTask, err := workspace.CreateTask(app.CreateTaskInput{ProjectID: inheriting.ID, Title: "Inherited task"})
	if err != nil {
		t.Fatal(err)
	}
	if inheritedTask.AgentBinding != (app.AgentBinding{Kind: "profile", Name: "workspace-task"}) {
		t.Fatalf("Task did not inherit the Workspace default: %#v", inheritedTask.AgentBinding)
	}
	overriddenTask, err := workspace.CreateTask(app.CreateTaskInput{ProjectID: overriding.ID, Title: "Overridden task"})
	if err != nil {
		t.Fatal(err)
	}
	if overriddenTask.AgentBinding != (app.AgentBinding{Kind: "agent", Name: "project-agent"}) {
		t.Fatalf("Task did not use the Project default: %#v", overriddenTask.AgentBinding)
	}
	cleared, err := workspace.SetProjectTaskDefault(overriding.ID, app.AgentBinding{})
	if err != nil {
		t.Fatal(err)
	}
	if cleared != (app.AgentBinding{}) {
		t.Fatalf("cleared Project Task default = %#v", cleared)
	}
	detail, err := workspace.Resource(inheriting.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.TaskDefault != (app.AgentBinding{}) {
		t.Fatalf("inheriting Project detail exposed a Task default: %#v", detail.TaskDefault)
	}
}

func TestEnsureResourceRuntimeNeverOverwritesWorkspaceDefaults(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := workspace.EnsureResourceRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.ResourceDefaults.Project != (app.AgentBinding{Kind: "profile", Name: "default"}) || runtime.ResourceDefaults.Task != (app.AgentBinding{Kind: "profile", Name: "default"}) {
		t.Fatalf("fresh Workspace defaults = %#v", runtime.ResourceDefaults)
	}
	custom := app.ResourceAgentDefaults{
		Project: app.AgentBinding{Kind: "profile", Name: "custom-project"},
		Task:    app.AgentBinding{Kind: "agent", Name: "custom-task"},
	}
	if _, err := workspace.SetResourceAgentDefaults(custom); err != nil {
		t.Fatal(err)
	}
	again, err := workspace.EnsureResourceRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if again.ResourceDefaults != custom {
		t.Fatalf("EnsureResourceRuntime overwrote Workspace defaults: %#v", again.ResourceDefaults)
	}
}
