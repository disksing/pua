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

func TestInitializeCreatesSchedulerResource(t *testing.T) {
	root := t.TempDir()
	workspace, err := app.Initialize(root, "zh-CN")
	if err != nil {
		t.Fatal(err)
	}
	config, err := workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if config.SchemaVersion != 1 || config.AgentBinding.Kind != "profile" || config.AgentBinding.Name != "default" || config.WakeIntervalMinutes != 30 || len(config.Schedules) != 0 {
		t.Fatalf("default Scheduler configuration = %#v", config)
	}
	tree, err := workspace.Tree()
	if err != nil || tree.Scheduler.ID != app.SchedulerResourceID || tree.Scheduler.Type != "scheduler" || tree.Scheduler.Path != "scheduler" || tree.Scheduler.AgentBinding != config.AgentBinding {
		t.Fatalf("Scheduler tree resource = %#v, %v", tree.Scheduler, err)
	}
	detail, err := workspace.Resource(app.SchedulerResourceID)
	if err != nil || detail.Scheduler == nil || detail.Type != "scheduler" || detail.Path != "scheduler" || len(detail.Files) != 2 {
		t.Fatalf("Scheduler detail = %#v, %v", detail, err)
	}
	for _, name := range []string{"scheduler.json", "scheduler.md", "AGENTS.md"} {
		info, statErr := os.Stat(filepath.Join(workspace.Root(), "scheduler", name))
		if statErr != nil || !info.Mode().IsRegular() {
			t.Fatalf("Scheduler file %s = %#v, %v", name, info, statErr)
		}
	}
	agents, err := os.ReadFile(filepath.Join(workspace.Root(), "scheduler", "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agents), "../AGENTS.md") || !strings.Contains(string(agents), "schedule ID") || !strings.Contains(string(agents), "不要把它当作长期历史") {
		t.Fatalf("Scheduler guidance is incomplete:\n%s", agents)
	}
	schedulerMarkdown, err := os.ReadFile(filepath.Join(workspace.Root(), "scheduler", "scheduler.md"))
	if err != nil || !strings.Contains(string(schedulerMarkdown), "后续调度判断所需的最小上下文") {
		t.Fatalf("Scheduler context guidance is incomplete: %v\n%s", err, schedulerMarkdown)
	}
	inside, err := workspace.IsSchedulerPath(filepath.Join(workspace.Root(), "scheduler", "nested"))
	if err == nil || inside {
		t.Fatal("a nonexistent nested path unexpectedly matched")
	}
	if err := os.Mkdir(filepath.Join(workspace.Root(), "scheduler", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	inside, err = workspace.IsSchedulerPath(filepath.Join(workspace.Root(), "scheduler", "nested"))
	if err != nil || !inside {
		t.Fatalf("Scheduler path match = %v, %v", inside, err)
	}
}

func TestResourceGuidanceIsLocalizedAndInherited(t *testing.T) {
	root := t.TempDir()
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := workspace.CreateProject("Guidance project", "guidance")
	if err != nil {
		t.Fatal(err)
	}
	task, err := workspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Guidance task", Slug: "guidance"})
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
	paths := []string{
		filepath.Join(root, "AGENTS.md"),
		filepath.Join(workspace.Root(), "scheduler", "AGENTS.md"),
		filepath.Join(root, projectDetail.Path, "AGENTS.md"),
		filepath.Join(root, taskDetail.Path, "AGENTS.md"),
	}
	rootAgents, err := os.ReadFile(paths[0])
	if err != nil || !strings.Contains(string(rootAgents), "## 6. Agent collaboration") || !strings.Contains(string(rootAgents), "[[project1.task2]]") {
		t.Fatalf("English Workspace guidance is incomplete: %v\n%s", err, rootAgents)
	}
	if err := workspace.Migrate("zh-CN"); err != nil {
		t.Fatal(err)
	}
	rootAgents, err = os.ReadFile(paths[0])
	if err != nil || !strings.Contains(string(rootAgents), "## 6. Agent 协作") || !strings.Contains(string(rootAgents), "[[project1.task2]]") {
		t.Fatalf("Chinese Workspace guidance is incomplete: %v\n%s", err, rootAgents)
	}
	projectAgents, err := os.ReadFile(paths[2])
	if err != nil {
		t.Fatal(err)
	}
	taskAgents, err := os.ReadFile(paths[3])
	if err != nil {
		t.Fatal(err)
	}
	schedulerAgents, err := os.ReadFile(paths[1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(projectAgents), project.ID) || !strings.Contains(string(projectAgents), "../AGENTS.md") ||
		!strings.Contains(string(taskAgents), task.ID) || !strings.Contains(string(taskAgents), "../../AGENTS.md") ||
		!strings.Contains(string(schedulerAgents), "../AGENTS.md") {
		t.Fatal("resource prompts no longer inherit the Workspace root guidance")
	}
}

func TestScheduleLifecycleValidatesTargets(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := workspace.CreateProject("Scheduled project", "scheduled")
	if err != nil {
		t.Fatal(err)
	}
	task, err := workspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Target", Slug: "target"})
	if err != nil {
		t.Fatal(err)
	}
	created, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "  Remind the target  ",
		Condition:   "  tomorrow morning  ",
		Target:      task.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Description != "Remind the target" || created.Condition != "tomorrow morning" || created.Target != task.ID || created.CreatedAt == "" || created.UpdatedAt != created.CreatedAt {
		t.Fatalf("created schedule = %#v", created)
	}
	condition := "when the build is green"
	target := app.SchedulerResourceID
	updated, err := workspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, Condition: &condition, Target: &target})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Condition != condition || updated.Target != target || updated.CreatedAt != created.CreatedAt {
		t.Fatalf("updated schedule = %#v", updated)
	}
	if _, err := workspace.AddSchedule(app.CreateScheduleInput{Description: "Bad", Condition: "now", Target: "project999.task999"}); err == nil {
		t.Fatal("missing cross-resource target unexpectedly accepted")
	}
	if _, err := workspace.ArchiveResource(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.AddSchedule(app.CreateScheduleInput{Description: "Archived", Condition: "now", Target: task.ID}); err == nil {
		t.Fatal("archived target unexpectedly accepted")
	}
	removed, err := workspace.RemoveSchedule(created.ID)
	if err != nil || removed.ID != created.ID {
		t.Fatalf("removed schedule = %#v, %v", removed, err)
	}
	config, err := workspace.Scheduler()
	if err != nil || len(config.Schedules) != 0 {
		t.Fatalf("Scheduler after removal = %#v, %v", config, err)
	}
}

func TestSchedulerSettingsAndConcurrentScheduleWrites(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	config, err := workspace.SetSchedulerSettings(app.SchedulerSettingsInput{
		AgentBinding:        app.AgentBinding{Kind: "agent", Name: "reviewer"},
		WakeIntervalMinutes: 45,
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.AgentBinding.Kind != "agent" || config.AgentBinding.Name != "reviewer" || config.WakeIntervalMinutes != 45 {
		t.Fatalf("Scheduler settings = %#v", config)
	}
	binding := app.AgentBinding{Kind: "profile", Name: "fast"}
	if got, err := workspace.SetResourceAgentBinding(app.SchedulerResourceID, binding); err != nil || got != binding {
		t.Fatalf("set Scheduler resource binding = %#v, %v", got, err)
	}
	config, err = workspace.Scheduler()
	if err != nil || config.AgentBinding != binding || config.WakeIntervalMinutes != 45 {
		t.Fatalf("resource binding update drifted Scheduler settings = %#v, %v", config, err)
	}
	const count = 16
	var wait sync.WaitGroup
	errors := make(chan error, count)
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, addErr := workspace.AddSchedule(app.CreateScheduleInput{
				Description: fmt.Sprintf("Concurrent schedule %d", index),
				Condition:   "when appropriate",
				Target:      "workspace",
			})
			if addErr != nil {
				errors <- addErr
			}
		}(index)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	config, err = workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Schedules) != count {
		t.Fatalf("got %d schedules, want %d", len(config.Schedules), count)
	}
	seen := make(map[string]bool, count)
	for _, schedule := range config.Schedules {
		if seen[schedule.ID] {
			t.Fatalf("duplicate schedule id %q", schedule.ID)
		}
		seen[schedule.ID] = true
	}
	data, err := os.ReadFile(filepath.Join(workspace.Root(), "scheduler", "scheduler.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if json.Unmarshal(data, &raw) != nil || !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("scheduler.json is not formatted valid JSON:\n%s", data)
	}
}

func TestMigratePreservesSchedulerContentAndRejectsUnsafeConflicts(t *testing.T) {
	root := t.TempDir()
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	created, err := workspace.AddSchedule(app.CreateScheduleInput{Description: "Keep me", Condition: "next week", Target: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	markdownPath := filepath.Join(workspace.Root(), "scheduler", "scheduler.md")
	if err := os.WriteFile(markdownPath, []byte("# Durable scheduler notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentsPath := filepath.Join(workspace.Root(), "scheduler", "AGENTS.md")
	agents, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentsPath, append([]byte("Manual preface\n\n"), agents...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Migrate("zh-CN"); err != nil {
		t.Fatal(err)
	}
	config, err := workspace.Scheduler()
	if err != nil || len(config.Schedules) != 1 || config.Schedules[0].ID != created.ID {
		t.Fatalf("migrated schedules = %#v, %v", config.Schedules, err)
	}
	markdown, _ := os.ReadFile(markdownPath)
	agents, _ = os.ReadFile(agentsPath)
	if string(markdown) != "# Durable scheduler notes\n" || !strings.HasPrefix(string(agents), "Manual preface") || !strings.Contains(string(agents), "Scheduler Agent 指引") {
		t.Fatalf("migration did not preserve unmanaged content:\nmarkdown=%s\nagents=%s", markdown, agents)
	}

	unsafeRoot := t.TempDir()
	unsafeWorkspace, err := app.Initialize(unsafeRoot, "en")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(unsafeRoot, "scheduler", "scheduler.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(unsafeRoot, "workspace.json"), filepath.Join(unsafeRoot, "scheduler", "scheduler.json")); err != nil {
		t.Fatal(err)
	}
	if err := unsafeWorkspace.Migrate(""); err == nil || !app.IsKind(err, "scheduler") {
		t.Fatalf("unsafe Scheduler conflict = %v", err)
	}
}

func TestScheduleConditionCharacterLimit(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	config, err := workspace.Scheduler()
	if err != nil || config.ConditionMaxChars != 128 {
		t.Fatalf("default limit = %#v, %v", config, err)
	}
	for _, character := range []string{"a", "\u4e2d", "😀"} {
		t.Run(character, func(t *testing.T) {
			condition := strings.Repeat(character, 128)
			created, err := workspace.AddSchedule(app.CreateScheduleInput{Description: "Boundary", Condition: " \n" + condition + "\t ", Target: "workspace"})
			if err != nil || created.Condition != condition {
				t.Fatalf("128-character condition = %#v, %v", created, err)
			}
			before, err := os.ReadFile(filepath.Join(workspace.Root(), "scheduler", "scheduler.json"))
			if err != nil {
				t.Fatal(err)
			}
			tooLong := condition + character
			if _, err := workspace.AddSchedule(app.CreateScheduleInput{Description: "Too long", Condition: tooLong, Target: "workspace"}); err == nil || !strings.Contains(err.Error(), "at most 128 characters (got 129)") {
				t.Fatalf("oversized add error = %v", err)
			}
			if _, err := workspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, Condition: &tooLong}); err == nil || !strings.Contains(err.Error(), "at most 128 characters (got 129)") {
				t.Fatalf("oversized update error = %v", err)
			}
			after, err := os.ReadFile(filepath.Join(workspace.Root(), "scheduler", "scheduler.json"))
			if err != nil || string(before) != string(after) {
				t.Fatalf("rejected writes changed scheduler.json: %v", err)
			}
		})
	}
}

func TestSchedulerConditionLimitSettingsAndLegacySchedules(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	setLimit := func(limit int) (app.SchedulerConfig, error) {
		return workspace.SetSchedulerSettings(app.SchedulerSettingsInput{
			AgentBinding: app.AgentBinding{Kind: "profile", Name: "default"}, WakeIntervalMinutes: 30, ConditionMaxChars: &limit,
		})
	}
	for _, limit := range []int{0, -1, 65537} {
		if _, err := setLimit(limit); err == nil {
			t.Fatalf("invalid limit %d accepted", limit)
		}
	}
	if config, err := setLimit(256); err != nil || config.ConditionMaxChars != 256 {
		t.Fatalf("configured limit = %#v, %v", config, err)
	}
	created, err := workspace.AddSchedule(app.CreateScheduleInput{Description: "Legacy", Condition: strings.Repeat("\u4e2d", 256), Target: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setLimit(1); err != nil {
		t.Fatal(err)
	}
	description := "Updated description"
	if _, err := workspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, Description: &description}); err != nil {
		t.Fatalf("unrelated update of long condition failed: %v", err)
	}
	short := "😀"
	if _, err := workspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, Condition: &short}); err != nil {
		t.Fatalf("single character update failed: %v", err)
	}
	tooLong := "😀😀"
	if _, err := workspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, Condition: &tooLong}); err == nil {
		t.Fatal("configured limit was ignored")
	}
	config, err := workspace.SetSchedulerSettings(app.SchedulerSettingsInput{
		AgentBinding: app.AgentBinding{Kind: "profile", Name: "fast"}, WakeIntervalMinutes: 45,
	})
	if err != nil || config.ConditionMaxChars != 1 {
		t.Fatalf("omitted limit did not preserve setting: %#v, %v", config, err)
	}
	if _, err := setLimit(65536); err != nil {
		t.Fatal(err)
	}
	created, err = workspace.AddSchedule(app.CreateScheduleInput{Description: "Maximum", Condition: strings.Repeat("😀", 65536), Target: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-upgrade file without the new setting and with a long condition.
	path := filepath.Join(workspace.Root(), "scheduler", "scheduler.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "conditionMaxChars")
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Migrate(""); err != nil {
		t.Fatal(err)
	}
	config, err = workspace.Scheduler()
	if err != nil || config.ConditionMaxChars != 128 || len(config.Schedules) != 2 {
		t.Fatalf("legacy read = %#v, %v", config, err)
	}
	if _, err := workspace.RemoveSchedule(created.ID); err != nil {
		t.Fatalf("legacy oversized schedule removal failed: %v", err)
	}
	config, err = workspace.Scheduler()
	if err != nil || config.ConditionMaxChars != 128 || len(config.Schedules) != 1 {
		t.Fatalf("legacy removal = %#v, %v", config, err)
	}
}
