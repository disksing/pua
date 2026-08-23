package app_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

type schedulerV1TestDefinition struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Condition   string `json:"condition"`
	Target      string `json:"target"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

func migrateSchedulerV1ForTest(t *testing.T, workspace *app.Workspace, definitions ...schedulerV1TestDefinition) ([]app.Schedule, []byte) {
	t.Helper()
	legacy := struct {
		SchemaVersion       int                         `json:"schemaVersion"`
		AgentBinding        map[string]string           `json:"agentBinding"`
		WakeIntervalMinutes int                         `json:"wakeIntervalMinutes"`
		Schedules           []schedulerV1TestDefinition `json:"schedules"`
	}{
		SchemaVersion:       1,
		AgentBinding:        map[string]string{"kind": "profile", "name": "default"},
		WakeIntervalMinutes: 45,
		Schedules:           definitions,
	}
	data, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(workspace.Root(), "scheduler", "scheduler.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Migrate(""); err != nil {
		t.Fatal(err)
	}
	config, err := workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	return config.Schedules, data
}

func TestSchedulerV1MigrationPreservesDefinitionsForCompilation(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	schedules, legacyData := migrateSchedulerV1ForTest(t, workspace, schedulerV1TestDefinition{
		ID:          "schedule-0123456789abcdef01234567",
		Description: "Keep action",
		Condition:   "tomorrow morning when green",
		Target:      "workspace",
		CreatedAt:   "2026-08-01T00:00:00Z",
		UpdatedAt:   "2026-08-02T00:00:00Z",
	})
	if len(schedules) != 1 {
		t.Fatalf("migrated schedules = %#v", schedules)
	}
	schedule := schedules[0]
	if schedule.Revision != 1 || schedule.State != app.ScheduleStateNeedsCompilation || schedule.Trigger != nil ||
		schedule.Description != "Keep action" || schedule.Condition != "tomorrow morning when green" || schedule.Target != "workspace" ||
		schedule.CreatedAt != "2026-08-01T00:00:00Z" || schedule.UpdatedAt != "2026-08-02T00:00:00Z" {
		t.Fatalf("migrated schedule = %#v", schedule)
	}
	raw, _ := os.ReadFile(filepath.Join(workspace.Root(), "scheduler", "scheduler.json"))
	if string(raw) == "" || string(raw) == string(legacyData) {
		t.Fatal("migration did not atomically replace the v1 definition")
	}
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: "9999-08-30T12:00:00Z"}
	compiled, err := workspace.UpdateSchedule(app.UpdateScheduleInput{ID: schedule.ID, ExpectedRevision: schedule.Revision, Trigger: &trigger})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Revision != 2 || compiled.State != app.ScheduleStateActive || compiled.Trigger == nil || *compiled.Trigger != trigger ||
		compiled.Description != schedule.Description || compiled.Condition != schedule.Condition || compiled.Target != schedule.Target {
		t.Fatalf("compiled migrated schedule = %#v", compiled)
	}
}

func TestAddScheduleRequiresTriggerWithoutMutation(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace.Root(), "scheduler", "scheduler.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = workspace.AddSchedule(app.CreateScheduleInput{Description: "Compile me", Condition: "tomorrow", Target: "workspace"})
	if !errors.Is(err, app.ErrScheduleTriggerRequired) || err.Error() != "add schedule: schedule trigger is required" {
		t.Fatalf("triggerless create error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || !os.SameFile(beforeInfo, afterInfo) {
		t.Fatalf("triggerless create mutated scheduler.json:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestScheduleAtMutationRequiresFutureInstant(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace.Root(), "scheduler", "scheduler.json")
	beforeCreate, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pastTrigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: "2000-01-01T00:00:00Z"}
	if _, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Too late", Condition: "in the past", Target: "workspace", Trigger: &pastTrigger,
	}); !errors.Is(err, app.ErrScheduleTriggerAtNotFuture) {
		t.Fatalf("past one-time create error = %v", err)
	}
	afterCreate, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterCreate) != string(beforeCreate) {
		t.Fatalf("failed create mutated scheduler.json:\nbefore=%s\nafter=%s", beforeCreate, afterCreate)
	}

	futureTrigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: "9999-12-31T23:59:58Z"}
	created, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "In time", Condition: "in the future", Target: "workspace", Trigger: &futureTrigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	beforeUpdate, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	description := "Must stay unchanged"
	if _, err := workspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: created.ID, ExpectedRevision: created.Revision, Description: &description, Trigger: &pastTrigger,
	}); !errors.Is(err, app.ErrScheduleTriggerAtNotFuture) {
		t.Fatalf("past one-time update error = %v", err)
	}
	afterUpdate, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterUpdate) != string(beforeUpdate) {
		t.Fatalf("failed update mutated scheduler.json:\nbefore=%s\nafter=%s", beforeUpdate, afterUpdate)
	}
	config, err := workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Schedules) != 1 || config.Schedules[0].Revision != created.Revision || config.Schedules[0].Description != created.Description || *config.Schedules[0].Trigger != futureTrigger {
		t.Fatalf("failed update changed schedule = %#v", config.Schedules)
	}

	futureUpdate := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: "9999-12-31T23:59:59Z"}
	updated, err := workspace.UpdateSchedule(app.UpdateScheduleInput{
		ID: created.ID, ExpectedRevision: created.Revision, Trigger: &futureUpdate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != created.Revision+1 || updated.Trigger == nil || *updated.Trigger != futureUpdate {
		t.Fatalf("valid future update = %#v", updated)
	}
}

func TestSchedulerLoadsCompletedPastAtTrigger(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	futureTrigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: "9999-12-31T23:59:59Z"}
	created, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Already done", Condition: "once", Target: "workspace", Trigger: &futureTrigger,
	})
	if err != nil {
		t.Fatal(err)
	}
	config, err := workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	config.Schedules[0].State = app.ScheduleStateCompleted
	config.Schedules[0].Trigger.At = "2000-01-01T00:00:00Z"
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(workspace.Root(), "scheduler", "scheduler.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := workspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Schedules) != 1 || loaded.Schedules[0].ID != created.ID || loaded.Schedules[0].State != app.ScheduleStateCompleted || loaded.Schedules[0].Trigger.At != "2000-01-01T00:00:00Z" {
		t.Fatalf("loaded completed schedule = %#v", loaded.Schedules)
	}
}

func TestScheduleTriggerValidationAndRevisionCAS(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.ValidateScheduleTrigger(app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 59, AnchorAt: "2026-08-01T00:00:00Z"}); err == nil {
		t.Fatal("sub-minute interval unexpectedly accepted")
	}
	if err := app.ValidateScheduleTrigger(app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "TZ=UTC 0 * * * * *", TimeZone: "UTC"}); err == nil {
		t.Fatal("embedded cron timezone unexpectedly accepted")
	}
	if err := app.ValidateScheduleTrigger(app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "0 * * * * *", TimeZone: "Not/AZone"}); err == nil {
		t.Fatal("invalid IANA timezone unexpectedly accepted")
	}
	if err := app.ValidateScheduleTrigger(app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "0 * * * * *", TimeZone: "Local"}); err == nil {
		t.Fatal("implicit local timezone unexpectedly accepted")
	}
	if err := app.ValidateScheduleTrigger(app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "* * * * * *", TimeZone: "UTC"}); err == nil {
		t.Fatal("sub-minute cron unexpectedly accepted")
	}
	created, err := workspace.AddSchedule(app.CreateScheduleInput{
		Description: "Run", Condition: "at noon UTC", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: "9999-08-30T12:00:00Z"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 || created.State != app.ScheduleStateActive || created.Trigger == nil || created.Trigger.At != "9999-08-30T12:00:00Z" {
		t.Fatalf("valid create = %#v", created)
	}
	description := "Changed"
	if _, err := workspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, Description: &description}); err == nil {
		t.Fatal("update without expectedRevision unexpectedly succeeded")
	}
	_, err = workspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, ExpectedRevision: 7, Description: &description})
	var conflict *app.ScheduleRevisionConflictError
	if !errors.As(err, &conflict) || conflict.Actual != 1 {
		t.Fatalf("revision conflict = %#v, %v", conflict, err)
	}
	config, _ := workspace.Scheduler()
	if config.Schedules[0].Description != created.Description || config.Schedules[0].Revision != 1 {
		t.Fatalf("failed CAS changed definition: %#v", config.Schedules[0])
	}
	paused, err := workspace.PauseSchedule(created.ID)
	if err != nil || paused.State != app.ScheduleStatePaused || paused.Revision != 2 {
		t.Fatalf("pause = %#v, %v", paused, err)
	}
	pausedAgain, err := workspace.PauseSchedule(created.ID)
	if err != nil || pausedAgain.Revision != paused.Revision {
		t.Fatalf("idempotent pause = %#v, %v", pausedAgain, err)
	}
	resumed, err := workspace.ResumeSchedule(created.ID)
	if err != nil || resumed.State != app.ScheduleStateActive || resumed.Revision != 3 {
		t.Fatalf("resume = %#v, %v", resumed, err)
	}
}

func TestScheduleChangeOperationRejectsUnknownValues(t *testing.T) {
	operation, err := app.ParseScheduleChangeOperation(" pause ")
	if err != nil || operation != app.ScheduleChangePause {
		t.Fatalf("parsed operation = %q, %v", operation, err)
	}
	if _, err := app.ParseScheduleChangeOperation("restart"); err == nil || err.Error() != `unsupported Scheduler change "restart"` {
		t.Fatalf("invalid operation error = %v", err)
	}
}

func TestCronDowntimeCoalescingIsBounded(t *testing.T) {
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "0 * * * * *", TimeZone: "UTC"}
	first := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
	now := first.Add(100_001 * time.Minute)
	last, next, count, truncated, err := app.CoalescedScheduleOccurrence(trigger, first, now)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || count != 100_000 || !last.Equal(first.Add(99_999*time.Minute)) || !next.After(now) {
		t.Fatalf("bounded coalescing = last %s next %s count %d truncated %v", last, next, count, truncated)
	}
}

func TestNeedsCompilationScheduleCannotEnterInvalidPausedState(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	schedules, _ := migrateSchedulerV1ForTest(t, workspace, schedulerV1TestDefinition{
		ID:          "schedule-111111111111111111111111",
		Description: "Compile me",
		Condition:   "tomorrow",
		Target:      "workspace",
		CreatedAt:   "2026-08-01T00:00:00Z",
		UpdatedAt:   "2026-08-01T00:00:00Z",
	})
	created := schedules[0]
	if _, err := workspace.PauseSchedule(created.ID); err == nil {
		t.Fatal("uncompiled schedule unexpectedly paused")
	}
	config, err := workspace.Scheduler()
	if err != nil || config.Schedules[0].State != app.ScheduleStateNeedsCompilation {
		t.Fatalf("failed pause corrupted Scheduler configuration: %#v, %v", config, err)
	}
}

func TestCronNextOccurrenceHonorsDSTInstants(t *testing.T) {
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "0 30 2 * * *", TimeZone: "America/New_York"}
	after := time.Date(2026, 3, 7, 8, 0, 0, 0, time.UTC)
	next, err := app.NextScheduleOccurrence(trigger, after)
	if err != nil {
		t.Fatal(err)
	}
	// 02:30 does not exist on the spring-forward day, so the next nominal
	// occurrence is the following day at 02:30 EDT (06:30 UTC).
	if want := time.Date(2026, 3, 9, 6, 30, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("spring DST next = %s, want %s", next, want)
	}

	fall := app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "0 30 1 * * *", TimeZone: "America/New_York"}
	first, err := app.NextScheduleOccurrence(fall, time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	second, err := app.NextScheduleOccurrence(fall, first)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equal(time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)) || !second.Equal(time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)) {
		t.Fatalf("fall DST occurrences = %s and %s", first, second)
	}
}
