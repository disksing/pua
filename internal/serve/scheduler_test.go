package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func TestSchedulerHTTPAPIRoutesNaturalLanguageAndNativeChanges(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	workspace := serveWorkspace{ID: "workspace-scheduler", Name: "Scheduler", Path: root}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{workspace}}); err != nil {
		t.Fatal(err)
	}
	s.agents = newAgentManager(s)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		s.handleWorkspace(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
		return recorder
	}

	natural := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler", `{"description":"Review","condition":"tomorrow morning","target":"workspace"}`)
	if natural.Code != http.StatusAccepted {
		t.Fatalf("natural-language request = %d %s", natural.Code, natural.Body.String())
	}
	mailbox, err := loadResourceMailboxForResource(root, app.SchedulerResourceID)
	if err != nil || len(mailbox.Messages) != 1 || mailbox.Messages[0].Role != "user" || !strings.Contains(mailbox.Messages[0].Text, "IANA timezone") {
		t.Fatalf("Scheduler compilation mailbox = %#v, %v", mailbox.Messages, err)
	}

	at := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	createdResponse := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler/changes", `{"operation":"create","description":"Review","condition":"tomorrow at 09:00 UTC","target":"workspace","trigger":{"type":"at","at":"`+at+`"}}`)
	if createdResponse.Code != http.StatusOK {
		t.Fatalf("native create = %d %s", createdResponse.Code, createdResponse.Body.String())
	}
	var created app.Schedule
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil || created.Revision != 1 || created.Trigger == nil {
		t.Fatalf("created schedule = %#v, %v", created, err)
	}
	conflict := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler/changes", `{"operation":"update","id":"`+created.ID+`","expectedRevision":9,"description":"Changed"}`)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "schedule_revision_conflict") {
		t.Fatalf("revision conflict = %d %s", conflict.Code, conflict.Body.String())
	}
	paused := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler/"+created.ID+"/pause", "")
	if paused.Code != http.StatusOK || !strings.Contains(paused.Body.String(), `"state": "paused"`) {
		t.Fatalf("pause = %d %s", paused.Code, paused.Body.String())
	}
	read := request(http.MethodGet, "/api/workspaces/workspace-scheduler/scheduler", "")
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"effectiveState": "paused"`) || strings.Contains(read.Body.String(), "wakeIntervalMinutes") {
		t.Fatalf("snapshot = %d %s", read.Code, read.Body.String())
	}
	removed := request(http.MethodDelete, "/api/workspaces/workspace-scheduler/scheduler/"+created.ID, "")
	if removed.Code != http.StatusOK {
		t.Fatalf("remove = %d %s", removed.Code, removed.Body.String())
	}
}

func scheduleOccurrenceMessages(t *testing.T, workspacePath, resourceID string) []resourceMailboxMessage {
	t.Helper()
	mailbox, err := loadResourceMailboxForResource(workspacePath, resourceID)
	if err != nil {
		t.Fatal(err)
	}
	var result []resourceMailboxMessage
	for _, message := range mailbox.Messages {
		if message.Type == resourceMessageTypeScheduleOccurrence {
			result = append(result, message)
		}
	}
	return result
}

func TestNativeSchedulerCoalescesDowntimeAndUsesStableOccurrence(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
	anchor := time.Now().UTC().Add(2 * time.Minute).Truncate(time.Second)
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Check release", Condition: "every minute", Guard: "the release branch is green", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: anchor.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return anchor.Add(3*time.Minute + 10*time.Second) }
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace")
	if len(messages) != 1 || messages[0].Causation == nil {
		t.Fatalf("occurrence messages = %#v", messages)
	}
	cause := messages[0].Causation
	if cause.ScheduleID != created.ID || cause.ScheduleRevision != 1 || cause.CoalescedCount != 4 || cause.Reason != schedulerOccurrenceReasonCoalesced {
		t.Fatalf("occurrence causation = %#v\n%s", cause, messages[0].Text)
	}
	protocol, err := newNativeScheduler(manager, workspace).prepareOccurrence(created, anchor, anchor.Add(3*time.Minute), anchor.Add(4*time.Minute), 4, schedulerOccurrenceReasonCoalesced)
	if err != nil || !strings.Contains(protocol.Text, "Action: Check release") || !strings.Contains(protocol.Text, "Guard: the release branch is green") || !strings.Contains(protocol.Text, "Next occurrence: "+anchor.Add(4*time.Minute).Format(time.RFC3339Nano)) || !strings.Contains(protocol.Text, cause.OccurrenceID) {
		t.Fatalf("occurrence guard protocol is incomplete: %v\n%s", err, protocol.Text)
	}
	firstMessageID := messages[0].ID
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	messages = scheduleOccurrenceMessages(t, workspace.Path, "workspace")
	if len(messages) != 1 || messages[0].ID != firstMessageID {
		t.Fatalf("reconcile duplicated occurrence = %#v", messages)
	}
	snapshot, err := newNativeScheduler(manager, workspace).Snapshot(manager.now())
	if err != nil || len(snapshot.Schedules) != 1 || snapshot.Schedules[0].NextRunAt != anchor.Add(4*time.Minute).Format(time.RFC3339Nano) || snapshot.Schedules[0].LastOutcome != schedulerOutcomeAccepted {
		t.Fatalf("runtime snapshot = %#v, %v", snapshot, err)
	}
	if deadline := manager.nextSchedulerReconcileDeadline(manager.now()); !deadline.Equal(anchor.Add(4 * time.Minute)) {
		t.Fatalf("dynamic Scheduler deadline = %s", deadline)
	}
}

func TestNativeSchedulerSkipsBusyRepeatingTargetButQueuesOneTime(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
	anchor := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	if _, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Repeated", Condition: "each minute", Target: "project1.task1",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: anchor.Format(time.RFC3339Nano)},
	}); err != nil {
		t.Fatal(err)
	}
	at := anchor.Add(10 * time.Second)
	if _, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "One time", Condition: "once", Target: "project1.task1",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := acceptMailboxMessage(workspace.Path, "project1.task1", resourceMessageRequest{Text: "already waiting", Mode: resourceMessageModeEnqueue}); err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return at.Add(time.Second) }
	// Delivery may fail against the intentionally unavailable fake endpoint,
	// but the durable boundary still distinguishes skip from one-time queueing.
	_ = manager.reconcileSchedulerLocked(context.Background(), workspace)
	snapshot, err := newNativeScheduler(manager, workspace).Snapshot(manager.now())
	if err != nil || len(snapshot.Schedules) != 2 {
		t.Fatalf("snapshot = %#v, %v", snapshot, err)
	}
	if snapshot.Schedules[0].LastOutcome != schedulerOutcomeBusy {
		t.Fatalf("repeating target outcome = %#v", snapshot.Schedules[0])
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "project1.task1"); len(messages) != 1 || messages[0].Causation.ScheduleID != snapshot.Schedules[1].ID {
		t.Fatalf("one-time occurrence was not queued: %#v", messages)
	}
}

func TestNativeSchedulerReplaysPreparedOccurrenceWithoutDuplicate(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
	at := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Replay safely", Condition: "every minute", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: at.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	native := newNativeScheduler(manager, workspace)
	runtime, err := initialScheduleRuntime(schedule, at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	next := at.Add(time.Minute)
	prepared, err := native.prepareOccurrence(schedule, at, at, next, 1, schedulerOccurrenceReasonTime)
	if err != nil {
		t.Fatal(err)
	}
	runtime.Prepared = &prepared
	if err := native.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
		t.Fatal(err)
	}
	if err := native.deliverPrepared(context.Background(), schedule, runtime, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after target acceptance but before the source checkpoint
	// commit by restoring the exact immutable prepared value.
	if err := native.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
		t.Fatal(err)
	}
	if err := native.deliverPrepared(context.Background(), schedule, runtime, at.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace")
	if len(messages) != 1 || messages[0].ID != prepared.MessageID || messages[0].Causation.OccurrenceID != prepared.OccurrenceID {
		t.Fatalf("prepared replay messages = %#v", messages)
	}
	snapshot, err := native.Snapshot(at.Add(2 * time.Second))
	if err != nil || snapshot.Schedules[0].LastOutcome != schedulerOutcomeAccepted || snapshot.Schedules[0].NextRunAt != next.Format(time.RFC3339Nano) {
		t.Fatalf("prepared replay was mistaken for a busy skip: %#v, %v", snapshot, err)
	}
}

func TestNativeSchedulerHonorsPersistedDeliveryBackoff(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
	anchor := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Retry safely", Condition: "every minute", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: anchor.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	native := newNativeScheduler(manager, workspace)
	now := anchor.Add(time.Second)
	runtime, err := initialScheduleRuntime(schedule, now)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := native.prepareOccurrence(schedule, anchor, anchor, anchor.Add(time.Minute), 1, schedulerOccurrenceReasonTime)
	if err != nil {
		t.Fatal(err)
	}
	retryAt := now.Add(5 * time.Second)
	runtime.Prepared = &prepared
	runtime.RetryAt = retryAt.Format(time.RFC3339Nano)
	runtime.RetryCount = 1
	if err := native.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
		t.Fatal(err)
	}
	deadline, err := native.Reconcile(context.Background(), now)
	if err != nil || !deadline.Equal(retryAt) {
		t.Fatalf("persisted retry deadline = %s, %v", deadline, err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 0 {
		t.Fatalf("occurrence delivered before retry: %#v", messages)
	}
	if _, err := native.Reconcile(context.Background(), retryAt); err != nil {
		t.Fatal(err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 1 || messages[0].ID != prepared.MessageID {
		t.Fatalf("occurrence was not delivered at retry: %#v", messages)
	}
}

func TestNativeSchedulerDoesNotReplayCompletedOneTimeOnSemanticEdit(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
	at := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Run once", Condition: "once", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	current := at.Add(time.Second)
	manager.now = func() time.Time { return current }
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	description := "Clarified completed action"
	updated, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, ExpectedRevision: created.Revision, Description: &description})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 1 {
		t.Fatalf("semantic edit replayed completed one-time occurrence: %#v", messages)
	}
	snapshot, err := newNativeScheduler(manager, workspace).Snapshot(current)
	if err != nil || snapshot.Schedules[0].Revision != updated.Revision || snapshot.Schedules[0].EffectiveState != app.ScheduleStateCompleted {
		t.Fatalf("completed semantic edit snapshot = %#v, %v", snapshot, err)
	}
	newAt := at.Add(time.Hour)
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: newAt.Format(time.RFC3339Nano)}
	if _, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, ExpectedRevision: updated.Revision, Trigger: &trigger}); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	snapshot, err = newNativeScheduler(manager, workspace).Snapshot(current)
	if err != nil || snapshot.Schedules[0].EffectiveState != app.ScheduleStateActive || snapshot.Schedules[0].NextRunAt != newAt.Format(time.RFC3339Nano) {
		t.Fatalf("new one-time trigger did not reactivate schedule: %#v, %v", snapshot, err)
	}
}

func TestNativeSchedulerMigrationMessagesProgressByDigest(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
	legacy, err := acceptGeneratedMailboxMessage(workspace.Path, resourceMailboxMessage{
		ID: "msg-legacy-scheduler-tick", ResourceID: app.SchedulerResourceID, Text: "legacy tick",
		RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, ModeFrozen: true,
		Type:      resourceMessageTypeSchedulerTick,
		Causation: &resourceMessageCausation{Type: resourceMessageTypeSchedulerTick, SourceResourceID: app.SchedulerResourceID, Reason: "legacy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{Description: "First", Condition: "tomorrow", Target: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{Description: "Second", Condition: "next week", Target: "workspace"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		// The compilation message is accepted before the intentionally absent
		// AgentHub endpoint fails to wake; that wake error is recoverable.
		t.Logf("initial migration wake: %v", err)
	}
	countMigrationMessages := func() int {
		mailbox, loadErr := loadResourceMailboxForResource(workspace.Path, app.SchedulerResourceID)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		count := 0
		for _, message := range mailbox.Messages {
			if message.Type == resourceMessageTypeScheduleMigration {
				count++
			}
		}
		return count
	}
	if got := countMigrationMessages(); got != 1 {
		t.Fatalf("migration messages = %d, want 1", got)
	}
	cancelled, found, err := mailboxMessageByID(workspace.Path, legacy.ID)
	if err != nil || !found || cancelled.Status != resourceMessageUndeliverable || cancelled.LastErrorCode != "scheduler_v1_retired" {
		t.Fatalf("legacy Scheduler tick was not cancelled: %#v, found=%v, err=%v", cancelled, found, err)
	}
	trigger := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)}
	if _, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{ID: first.ID, ExpectedRevision: first.Revision, Trigger: &trigger}); err != nil {
		t.Fatal(err)
	}
	_ = manager.reconcileSchedulerLocked(context.Background(), workspace)
	if got := countMigrationMessages(); got != 2 {
		t.Fatalf("partial compilation did not advance digest: %d", got)
	}
	_ = manager.reconcileSchedulerLocked(context.Background(), workspace)
	if got := countMigrationMessages(); got != 2 {
		t.Fatalf("unchanged migration digest spun another message: %d", got)
	}
}

func TestNativeSchedulerPauseResumeSkipsPausedOccurrences(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
	anchor := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	repeating, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Repeat", Condition: "every minute", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: anchor.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.PauseSchedule(repeating.ID); err != nil {
		t.Fatal(err)
	}
	oneTime, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Once", Condition: "in the past", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: anchor.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.PauseSchedule(oneTime.ID); err != nil {
		t.Fatal(err)
	}
	current := time.Now().UTC()
	manager.now = func() time.Time { return current }
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	snapshot, err := newNativeScheduler(manager, workspace).Snapshot(current)
	if err != nil || len(snapshot.Schedules) != 2 || snapshot.Schedules[0].EffectiveState != app.ScheduleStatePaused || snapshot.Schedules[1].EffectiveState != app.ScheduleStateCompleted || snapshot.Schedules[1].LastOutcome != schedulerOutcomePaused {
		t.Fatalf("paused snapshot = %#v, %v", snapshot, err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 0 {
		t.Fatalf("paused occurrences were delivered: %#v", messages)
	}
	if _, err := newNativeScheduler(manager, workspace).Change(context.Background(), NativeSchedulerChange{Operation: "resume", ID: repeating.ID}); err != nil {
		t.Fatal(err)
	}
	current = time.Now().UTC()
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	snapshot, err = newNativeScheduler(manager, workspace).Snapshot(current)
	next := generationTime(snapshot.Schedules[0].NextRunAt)
	if err != nil || snapshot.Schedules[0].EffectiveState != app.ScheduleStateActive || !next.After(current) {
		t.Fatalf("resume caught up paused occurrences: %#v, %v", snapshot.Schedules[0], err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 0 {
		t.Fatalf("resume delivered paused backlog: %#v", messages)
	}
}

func TestNativeSchedulerArchivedTargetRequiresAttentionUntilModified(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
	at := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Archived target", Condition: "once", Target: "project1.task1",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.ArchiveResource("project1.task1"); err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return at.Add(time.Second) }
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	snapshot, err := newNativeScheduler(manager, workspace).Snapshot(manager.now())
	if err != nil || snapshot.Schedules[0].EffectiveState != schedulerOutcomeAttention || snapshot.Schedules[0].NextRunAt != "" || snapshot.NextWakeAt != "" {
		t.Fatalf("archived target snapshot = %#v, %v", snapshot, err)
	}
	description := "Still archived"
	updated, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, ExpectedRevision: created.Revision, Description: &description})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	snapshot, err = newNativeScheduler(manager, workspace).Snapshot(manager.now())
	if err != nil || snapshot.Schedules[0].EffectiveState != schedulerOutcomeAttention {
		t.Fatalf("unrelated edit cleared target attention: %#v, %v", snapshot, err)
	}
	target := "workspace"
	if _, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, ExpectedRevision: updated.Revision, Target: &target}); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 1 {
		t.Fatalf("modified attention schedule did not run once: %#v", messages)
	}
}
