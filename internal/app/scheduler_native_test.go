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

func TestSchedulerV1MigrationPreservesDefinitionsForCompilation(t *testing.T) {
	workspace, err := app.Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{
		"schemaVersion":       1,
		"agentBinding":        map[string]string{"kind": "profile", "name": "default"},
		"wakeIntervalMinutes": 45,
		"schedules": []map[string]any{{
			"id": "schedule-0123456789abcdef01234567", "description": "Keep action",
			"condition": "tomorrow morning when green", "target": "workspace",
			"createdAt": "2026-08-01T00:00:00Z", "updatedAt": "2026-08-02T00:00:00Z",
		}},
	}
	data, _ := json.MarshalIndent(legacy, "", "  ")
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(workspace.Root(), "scheduler", "scheduler.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Migrate(""); err != nil {
		t.Fatal(err)
	}
	config, err := workspace.Scheduler()
	if err != nil || config.SchemaVersion != 2 || len(config.Schedules) != 1 {
		t.Fatalf("migrated config = %#v, %v", config, err)
	}
	schedule := config.Schedules[0]
	if schedule.Revision != 1 || schedule.State != app.ScheduleStateNeedsCompilation || schedule.Trigger != nil || schedule.Description != "Keep action" || schedule.Condition != "tomorrow morning when green" {
		t.Fatalf("migrated schedule = %#v", schedule)
	}
	raw, _ := os.ReadFile(filepath.Join(workspace.Root(), "scheduler", "scheduler.json"))
	if string(raw) == "" || string(raw) == string(data) {
		t.Fatal("migration did not atomically replace the v1 definition")
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
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: "2026-08-30T12:00:00Z"},
	})
	if err != nil {
		t.Fatal(err)
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
	created, err := workspace.AddSchedule(app.CreateScheduleInput{Description: "Compile me", Condition: "tomorrow", Target: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
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
