package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
	invalid := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler/changes", `{"operation":"restart"}`)
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `unsupported Scheduler change \"restart\"`) {
		t.Fatalf("invalid native change = %d %s", invalid.Code, invalid.Body.String())
	}
	createdResponse := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler/changes", `{"operation":"create","description":"Review","condition":"tomorrow at 09:00 UTC","target":"workspace","trigger":{"type":"at","at":"`+at+`"}}`)
	if createdResponse.Code != http.StatusOK {
		t.Fatalf("native create = %d %s", createdResponse.Code, createdResponse.Body.String())
	}
	var created app.Schedule
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil || created.Revision != 1 || created.Trigger == nil {
		t.Fatalf("created schedule = %#v, %v", created, err)
	}
	naturalUpdate := request(http.MethodPut, "/api/workspaces/workspace-scheduler/scheduler/"+created.ID, `{"description":"Review later","condition":"next week","target":"workspace"}`)
	if naturalUpdate.Code != http.StatusAccepted {
		t.Fatalf("natural-language update = %d %s", naturalUpdate.Code, naturalUpdate.Body.String())
	}
	mailbox, err = loadResourceMailboxForResource(root, app.SchedulerResourceID)
	if err != nil || len(mailbox.Messages) != 2 || !strings.Contains(mailbox.Messages[1].Text, "Please update a native schedule for "+created.ID) {
		t.Fatalf("Scheduler update compilation mailbox = %#v, %v", mailbox.Messages, err)
	}
	conflict := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler/changes", `{"operation":"update","id":"`+created.ID+`","expectedRevision":9,"description":"Changed","trigger":{"type":"at","at":"`+at+`"}}`)
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
	resumed := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler/"+created.ID+"/resume", "")
	if resumed.Code != http.StatusOK || !strings.Contains(resumed.Body.String(), `"state": "active"`) {
		t.Fatalf("resume = %d %s", resumed.Code, resumed.Body.String())
	}
	removed := request(http.MethodDelete, "/api/workspaces/workspace-scheduler/scheduler/"+created.ID, "")
	if removed.Code != http.StatusOK {
		t.Fatalf("remove = %d %s", removed.Code, removed.Body.String())
	}
}

func TestSchedulerHTTPStructuredUpdateRequiresCompleteTrigger(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	workspace := serveWorkspace{ID: "workspace-scheduler-update", Name: "Scheduler Update", Path: root}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{workspace}}); err != nil {
		t.Fatal(err)
	}
	s.agents = newAgentManager(s)
	puaWorkspace, err := app.OpenWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	originalAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	original, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Review", Condition: "at the original time", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: originalAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-scheduler-update/scheduler/changes", strings.NewReader(body))
		s.handleWorkspace(recorder, r)
		return recorder
	}
	assertUnchanged := func(want app.Schedule) {
		t.Helper()
		config, err := puaWorkspace.Scheduler()
		if err != nil {
			t.Fatal(err)
		}
		if len(config.Schedules) != 1 || !reflect.DeepEqual(config.Schedules[0], want) {
			t.Fatalf("schedule changed after rejected update: got %#v, want %#v", config.Schedules, want)
		}
	}

	descriptionOnly := request(`{"operation":"update","id":"` + original.ID + `","expectedRevision":1,"description":"Changed"}`)
	if descriptionOnly.Code != http.StatusBadRequest {
		t.Fatalf("description-only update = %d %s", descriptionOnly.Code, descriptionOnly.Body.String())
	}
	var triggerRequired map[string]string
	if err := json.Unmarshal(descriptionOnly.Body.Bytes(), &triggerRequired); err != nil {
		t.Fatal(err)
	}
	if triggerRequired["code"] != "schedule_trigger_required" || triggerRequired["error"] != "update requires a complete trigger" {
		t.Fatalf("description-only error = %#v", triggerRequired)
	}
	assertUnchanged(original)

	compiled, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Migrated", Condition: "ambiguous legacy rule", Target: "workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	partialCompilation := request(`{"operation":"update","id":"` + compiled.ID + `","expectedRevision":1,"condition":"still ambiguous"}`)
	if partialCompilation.Code != http.StatusBadRequest || !strings.Contains(partialCompilation.Body.String(), "schedule_trigger_required") {
		t.Fatalf("partial compilation = %d %s", partialCompilation.Code, partialCompilation.Body.String())
	}
	config, err := puaWorkspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Schedules) != 2 || !reflect.DeepEqual(config.Schedules[1], compiled) {
		t.Fatalf("needs-compilation schedule changed after partial update: %#v", config.Schedules)
	}

	replacementAt := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)
	triggerOnly := request(`{"operation":"update","id":"` + original.ID + `","expectedRevision":1,"trigger":{"type":"at","at":"` + replacementAt + `"}}`)
	if triggerOnly.Code != http.StatusOK {
		t.Fatalf("trigger-only update = %d %s", triggerOnly.Code, triggerOnly.Body.String())
	}
	var updated app.Schedule
	if err := json.Unmarshal(triggerOnly.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 || updated.Description != original.Description || updated.Condition != original.Condition || updated.Target != original.Target || updated.Trigger == nil || updated.Trigger.At != replacementAt {
		t.Fatalf("trigger-only schedule = %#v", updated)
	}

	staleAt := time.Now().UTC().Add(3 * time.Hour).Format(time.RFC3339Nano)
	stale := request(`{"operation":"update","id":"` + original.ID + `","expectedRevision":1,"description":"Stale","condition":"stale rule","target":"scheduler","trigger":{"type":"at","at":"` + staleAt + `"}}`)
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "schedule_revision_conflict") {
		t.Fatalf("full stale update = %d %s", stale.Code, stale.Body.String())
	}
	config, err = puaWorkspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config.Schedules[0], updated) {
		t.Fatalf("schedule changed after stale update: got %#v, want %#v", config.Schedules[0], updated)
	}

	malformed := request(`{"operation":"update","id":"` + original.ID + `","expectedRevision":2,"trigger":{"type":"at"}}`)
	if malformed.Code != http.StatusBadRequest || !strings.Contains(malformed.Body.String(), "at trigger must contain only at") {
		t.Fatalf("malformed trigger update = %d %s", malformed.Code, malformed.Body.String())
	}
	config, err = puaWorkspace.Scheduler()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config.Schedules[0], updated) {
		t.Fatalf("schedule changed after malformed trigger: got %#v, want %#v", config.Schedules[0], updated)
	}
}

func TestNativeSchedulerStructuredUpdateRequiresCompleteTrigger(t *testing.T) {
	fixture := func(t *testing.T, trigger *app.ScheduleTrigger) (*NativeScheduler, *app.Workspace, app.Schedule) {
		t.Helper()
		root := t.TempDir()
		if _, err := app.Initialize(root, "en"); err != nil {
			t.Fatal(err)
		}
		puaWorkspace, err := app.OpenWorkspace(root)
		if err != nil {
			t.Fatal(err)
		}
		created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
			Description: "Review", Condition: "at the original time", Target: "workspace", Trigger: trigger,
		})
		if err != nil {
			t.Fatal(err)
		}
		return newNativeScheduler(nil, serveWorkspace{Path: root}), puaWorkspace, created
	}
	readSchedule := func(t *testing.T, workspace *app.Workspace) app.Schedule {
		t.Helper()
		config, err := workspace.Scheduler()
		if err != nil {
			t.Fatal(err)
		}
		if len(config.Schedules) != 1 {
			t.Fatalf("schedules = %#v", config.Schedules)
		}
		return config.Schedules[0]
	}

	t.Run("description-only current revision is rejected atomically", func(t *testing.T) {
		at := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
		native, workspace, original := fixture(t, &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at})
		description := "Changed"
		_, err := native.Change(context.Background(), NativeSchedulerChange{
			Operation: app.ScheduleChangeUpdate, ID: original.ID, ExpectedRevision: original.Revision, Description: &description,
		})
		if err == nil || !errors.Is(err, errNativeSchedulerUpdateTriggerRequired) || err.Error() != "update requires a complete trigger" {
			t.Fatalf("description-only error = %v", err)
		}
		if got := readSchedule(t, workspace); !reflect.DeepEqual(got, original) {
			t.Fatalf("schedule changed after description-only update: got %#v, want %#v", got, original)
		}
	})

	t.Run("needs-compilation partial update is rejected", func(t *testing.T) {
		native, workspace, original := fixture(t, nil)
		condition := "still ambiguous"
		_, err := native.Change(context.Background(), NativeSchedulerChange{
			Operation: app.ScheduleChangeUpdate, ID: original.ID, ExpectedRevision: original.Revision, Condition: &condition,
		})
		if !errors.Is(err, errNativeSchedulerUpdateTriggerRequired) {
			t.Fatalf("partial compilation error = %v", err)
		}
		if got := readSchedule(t, workspace); !reflect.DeepEqual(got, original) || got.State != app.ScheduleStateNeedsCompilation {
			t.Fatalf("needs-compilation schedule changed: got %#v, want %#v", got, original)
		}
	})

	t.Run("trigger-only current revision succeeds", func(t *testing.T) {
		native, _, original := fixture(t, nil)
		replacement := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)}
		updated, err := native.Change(context.Background(), NativeSchedulerChange{
			Operation: app.ScheduleChangeUpdate, ID: original.ID, ExpectedRevision: original.Revision, Trigger: &replacement,
		})
		if err != nil {
			t.Fatal(err)
		}
		if updated.Revision != original.Revision+1 || updated.State != app.ScheduleStateActive || updated.Description != original.Description || updated.Condition != original.Condition || updated.Target != original.Target || updated.Trigger == nil || *updated.Trigger != replacement {
			t.Fatalf("trigger-only schedule = %#v", updated)
		}
	})

	t.Run("valid full replacement preserves revision conflict", func(t *testing.T) {
		at := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
		native, workspace, original := fixture(t, &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at})
		description, condition, target := "Changed", "at the new time", "scheduler"
		replacement := app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)}
		_, err := native.Change(context.Background(), NativeSchedulerChange{
			Operation: app.ScheduleChangeUpdate, ID: original.ID, ExpectedRevision: original.Revision + 1,
			Description: &description, Condition: &condition, Target: &target, Trigger: &replacement,
		})
		var conflict *app.ScheduleRevisionConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("stale full replacement error = %v", err)
		}
		if got := readSchedule(t, workspace); !reflect.DeepEqual(got, original) {
			t.Fatalf("schedule changed after stale full replacement: got %#v, want %#v", got, original)
		}
	})

	t.Run("malformed replacement is rejected atomically", func(t *testing.T) {
		at := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
		native, workspace, original := fixture(t, &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at})
		malformed := app.ScheduleTrigger{Type: app.ScheduleTriggerAt}
		_, err := native.Change(context.Background(), NativeSchedulerChange{
			Operation: app.ScheduleChangeUpdate, ID: original.ID, ExpectedRevision: original.Revision, Trigger: &malformed,
		})
		if err == nil || !strings.Contains(err.Error(), "at trigger must contain only at") {
			t.Fatalf("malformed replacement error = %v", err)
		}
		if got := readSchedule(t, workspace); !reflect.DeepEqual(got, original) {
			t.Fatalf("schedule changed after malformed replacement: got %#v, want %#v", got, original)
		}
	})
}

func TestSchedulerNativeChangeValidatesTrailingData(t *testing.T) {
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

	tests := []struct {
		name      string
		body      string
		wantError string
	}{
		{name: "whitespace", body: "{\"operation\":\"restart\"} \n\t", wantError: `unsupported Scheduler change "restart"`},
		{name: "second value", body: `{"operation":"restart"} {}`, wantError: "request body must contain exactly one JSON value"},
		{name: "malformed bytes", body: `{"operation":"restart"} trailing`, wantError: "invalid character"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler/changes", strings.NewReader(test.body))
			s.handleWorkspace(recorder, request)

			if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("response = %d %q: %s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
			}
			var response map[string]string
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if !strings.Contains(response["error"], test.wantError) {
				t.Fatalf("error = %q, want substring %q", response["error"], test.wantError)
			}
		})
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
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	tests := []struct {
		name     string
		wantBusy bool
		seed     func(*resourceMailboxStore, string)
	}{
		{
			name:     "queued message",
			wantBusy: true,
			seed: func(store *resourceMailboxStore, stamp string) {
				store.Mailbox.NextSequence++
				store.Mailbox.Messages = append(store.Mailbox.Messages, resourceMailboxMessage{
					ID: "queued-message", Sequence: store.Mailbox.NextSequence, ResourceID: "project1.task1",
					Text: "already waiting", Role: "user", RequestedMode: resourceMessageModeEnqueue,
					ActualMode: resourceMessageModeEnqueue, Status: resourceMessageQueued,
					AcceptedAt: stamp, UpdatedAt: stamp,
				})
			},
		},
		{
			name:     "unresolved result subscription",
			wantBusy: true,
			seed: func(store *resourceMailboxStore, stamp string) {
				store.Mailbox.NextSequence++
				store.Mailbox.Messages = append(store.Mailbox.Messages, resourceMailboxMessage{
					ID: "pending-result", Sequence: store.Mailbox.NextSequence, ResourceID: "project1.task1",
					Text: "awaiting result", Role: "agent", Sender: &agentHubMessageSender{ID: "project1", Name: "Sender"},
					SenderWorkspaceInstanceID: "sender-instance", SubscribeResult: true,
					ResultSubscriptionStatus: resourceResultSubscriptionPending,
					RequestedMode:            resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue,
					Status: resourceMessageDelivered, AcceptedAt: stamp, UpdatedAt: stamp,
					DeliveredAt: stamp, TerminalAt: stamp, GenerationID: "generation-1", TurnID: "turn-1",
				})
			},
		},
		{
			name:     "unresolved notification",
			wantBusy: true,
			seed: func(store *resourceMailboxStore, stamp string) {
				store.Mailbox.NextSequence++
				store.Mailbox.Messages = append(store.Mailbox.Messages, resourceMailboxMessage{
					ID: "pending-notification", Sequence: store.Mailbox.NextSequence, ResourceID: "project1.task1",
					Text: "notify sender", Role: "agent", RequestedMode: resourceMessageModeEnqueue,
					ActualMode: resourceMessageModeEnqueue, Status: resourceMessageDelivered,
					AcceptedAt: stamp, UpdatedAt: stamp, DeliveredAt: stamp, TerminalAt: stamp,
					Notification: &resourceNotificationReceipt{
						ID: "notification-1", Type: resourceMessageTypeDeliveryTerminal, Status: resourceNotificationWaiting,
						TargetWorkspaceInstanceID: "sender-instance", TargetResourceID: "project1",
						CreatedAt: stamp, UpdatedAt: stamp,
					},
				})
			},
		},
		{
			name:     "pending notification outbox",
			wantBusy: true,
			seed: func(store *resourceMailboxStore, stamp string) {
				store.Mailbox.NextSequence++
				store.Mailbox.Messages = append(store.Mailbox.Messages, resourceMailboxMessage{
					ID: "outbox-source", Sequence: store.Mailbox.NextSequence, ResourceID: "project1.task1",
					Text: "completed source", Role: "agent", RequestedMode: resourceMessageModeEnqueue,
					ActualMode: resourceMessageModeEnqueue, Status: resourceMessageDelivered,
					AcceptedAt: stamp, UpdatedAt: stamp, DeliveredAt: stamp, TerminalAt: stamp,
				})
				store.Outbox.Operations = append(store.Outbox.Operations, resourceMailboxNotificationOp{
					ID: "outbox-operation", Type: resourceMessageTypeDeliveryTerminal,
					SourceMessageID: "outbox-source", SourceResourceID: "project1.task1",
					SourceWorkspaceInstanceID: "target-instance", TargetWorkspaceInstanceID: "sender-instance",
					TargetResourceID: "project1", GeneratedMessageID: "generated-notification",
					Status: resourceNotificationWaiting, UpdatedAt: stamp,
				})
			},
		},
		{
			name: "cold terminal receipt",
			seed: func(store *resourceMailboxStore, stamp string) {
				store.Mailbox.NextSequence++
				store.Mailbox.Messages = append(store.Mailbox.Messages, resourceMailboxMessage{
					ID: "cold-receipt", Sequence: store.Mailbox.NextSequence, ResourceID: "project1.task1",
					Text: "finished", Role: "user", SubscribeResult: false,
					ResultSubscriptionStatus: resourceResultSubscriptionDisabled,
					RequestedMode:            resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue,
					Status: resourceMessageDelivered, AcceptedAt: stamp, UpdatedAt: stamp,
					DeliveredAt: stamp, TerminalAt: stamp,
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			anchor := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
			repeating, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Repeated", Condition: "each minute", Target: "project1.task1",
				Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: anchor.Format(time.RFC3339Nano)},
			})
			if err != nil {
				t.Fatal(err)
			}
			at := anchor.Add(10 * time.Second)
			oneTime, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "One time", Condition: "once", Target: "project1.task1",
				Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
			})
			if err != nil {
				t.Fatal(err)
			}
			stamp := anchor.Add(-time.Minute).Format(time.RFC3339Nano)
			if _, err := mutateResourceMailboxStoreForResource(workspace.Path, "project1.task1", func(store *resourceMailboxStore) error {
				test.seed(store, stamp)
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			hasHotWork, err := resourceMailboxHasHotWork(workspace.Path, "project1.task1")
			if err != nil || hasHotWork != test.wantBusy {
				t.Fatalf("hot mailbox ownership = %v, want %v: %v", hasHotWork, test.wantBusy, err)
			}
			if test.name == "cold terminal receipt" {
				stored, found, err := mailboxMessageByID(workspace.Path, "cold-receipt")
				if err != nil || !found || !stored.receipt {
					t.Fatalf("terminal message did not leave hot storage: found=%v err=%v message=%#v", found, err, stored)
				}
			}

			manager.now = func() time.Time { return at.Add(time.Second) }
			if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
				t.Fatal(err)
			}
			snapshot, err := newNativeScheduler(manager, workspace).Snapshot(manager.now())
			if err != nil || len(snapshot.Schedules) != 2 {
				t.Fatalf("snapshot = %#v, %v", snapshot, err)
			}
			wantRepeatOutcome := schedulerOutcomeAccepted
			if test.wantBusy {
				wantRepeatOutcome = schedulerOutcomeBusy
			}
			if snapshot.Schedules[0].ID != repeating.ID || snapshot.Schedules[0].LastOutcome != wantRepeatOutcome {
				t.Fatalf("repeating target outcome = %#v, want %q", snapshot.Schedules[0], wantRepeatOutcome)
			}
			if snapshot.Schedules[1].ID != oneTime.ID || snapshot.Schedules[1].LastOutcome != schedulerOutcomeAccepted {
				t.Fatalf("one-time target outcome = %#v", snapshot.Schedules[1])
			}
			messages := scheduleOccurrenceMessages(t, workspace.Path, "project1.task1")
			wantIDs := map[string]bool{oneTime.ID: true}
			if !test.wantBusy {
				wantIDs[repeating.ID] = true
			}
			if len(messages) != len(wantIDs) {
				t.Fatalf("occurrence messages = %#v, want schedule ids %#v", messages, wantIDs)
			}
			for _, message := range messages {
				if message.Causation == nil || !wantIDs[message.Causation.ScheduleID] {
					t.Fatalf("unexpected occurrence message = %#v, want schedule ids %#v", message, wantIDs)
				}
				delete(wantIDs, message.Causation.ScheduleID)
			}
			if len(wantIDs) != 0 {
				t.Fatalf("missing occurrence schedule ids = %#v", wantIDs)
			}
		})
	}
}

func TestNativeSchedulerCrashWindowReplayMatrix(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	type crashWindow int
	const (
		crashBeforePrepared crashWindow = iota
		crashWithPrepared
		crashWithAcceptedMessage
		crashAfterCheckpoint
	)
	tests := []struct {
		name   string
		window crashWindow
	}{
		{name: "before prepared persistence", window: crashBeforePrepared},
		{name: "after prepared persistence", window: crashWithPrepared},
		// No durable write separates prepared persistence from the mailbox
		// acceptance call, so these two crash boundaries restart from the same
		// checkpoint and must have the same outcome.
		{name: "before mailbox acceptance", window: crashWithPrepared},
		{name: "after mailbox acceptance", window: crashWithAcceptedMessage},
		// Likewise, an accepted mailbox message plus the still-prepared source
		// checkpoint is the durable state immediately before checkpoint commit.
		{name: "before checkpoint commit", window: crashWithAcceptedMessage},
		{name: "after checkpoint commit", window: crashAfterCheckpoint},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
			schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Replay safely", Condition: "every minute", Target: "workspace",
				Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: at.Format(time.RFC3339Nano)},
			})
			if err != nil {
				t.Fatal(err)
			}
			now := at.Add(time.Second)
			next := at.Add(time.Minute)
			native := newNativeScheduler(manager, workspace)
			prepared, err := native.prepareOccurrence(schedule, at, at, next, 1, schedulerOccurrenceReasonTime)
			if err != nil {
				t.Fatal(err)
			}
			initial := schedulerScheduleRuntime{
				Revision: schedule.Revision, TriggerDigest: mustSchedulerTriggerDigest(t, schedule.Trigger), EffectiveState: app.ScheduleStateActive,
				NextRunAt: at.Format(time.RFC3339Nano),
			}
			if err := native.storeSchedulerRuntime(schedule.ID, initial); err != nil {
				t.Fatal(err)
			}

			switch test.window {
			case crashBeforePrepared:
				// The due cursor is durable, but no immutable occurrence has been
				// prepared. Restart must derive the same stable identifiers.
			case crashWithPrepared:
				initial.Prepared = &prepared
				if err := native.storeSchedulerRuntime(schedule.ID, initial); err != nil {
					t.Fatal(err)
				}
			case crashWithAcceptedMessage:
				initial.Prepared = &prepared
				if err := native.storeSchedulerRuntime(schedule.ID, initial); err != nil {
					t.Fatal(err)
				}
				if _, err := acceptGeneratedMailboxMessage(workspace.Path, preparedOccurrenceMessage(prepared)); err != nil {
					t.Fatal(err)
				}
			case crashAfterCheckpoint:
				initial.Prepared = &prepared
				if err := native.storeSchedulerRuntime(schedule.ID, initial); err != nil {
					t.Fatal(err)
				}
				if err := native.deliverPrepared(context.Background(), schedule, initial, now); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("unknown crash window %d", test.window)
			}

			wantBefore := 0
			if test.window >= crashWithAcceptedMessage {
				wantBefore = 1
			}
			assertSingleDurableOccurrence(t, workspace.Path, prepared, wantBefore)

			// A fresh manager has no in-memory resource controller or Scheduler
			// instance, so only the durable workspace state crosses this boundary.
			restartedManager := newAgentManager(manager.server)
			restartedManager.now = func() time.Time { return now }
			manager.server.agents = restartedManager
			restarted := newNativeScheduler(restartedManager, workspace)
			recomputed, err := restarted.prepareOccurrence(schedule, at, at, next, 1, schedulerOccurrenceReasonTime)
			if err != nil {
				t.Fatal(err)
			}
			if recomputed.OccurrenceID != prepared.OccurrenceID || recomputed.MessageID != prepared.MessageID {
				t.Fatalf("stable identifiers changed after restart: got %s/%s, want %s/%s", recomputed.OccurrenceID, recomputed.MessageID, prepared.OccurrenceID, prepared.MessageID)
			}
			if test.window == crashWithAcceptedMessage {
				busy, err := restarted.targetBusy(prepared.Target)
				if err != nil || !busy {
					t.Fatalf("accepted prepared occurrence was not hot before replay: busy=%v err=%v", busy, err)
				}
			}
			if _, err := restarted.Reconcile(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			assertSingleDurableOccurrence(t, workspace.Path, prepared, 1)

			runtime, err := restarted.schedulerRuntime(schedule.ID)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.Prepared != nil || runtime.LastOutcome != schedulerOutcomeAccepted || runtime.LastOccurrenceAt != at.Format(time.RFC3339Nano) || runtime.NextRunAt != next.Format(time.RFC3339Nano) {
				t.Fatalf("replayed checkpoint = %#v", runtime)
			}

			// A second restart proves a committed replay cannot append either a
			// hot mailbox duplicate or an additional compacted receipt.
			secondManager := newAgentManager(manager.server)
			secondManager.now = func() time.Time { return now }
			manager.server.agents = secondManager
			if _, err := newNativeScheduler(secondManager, workspace).Reconcile(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			assertSingleDurableOccurrence(t, workspace.Path, prepared, 1)
		})
	}
}

func preparedOccurrenceMessage(prepared schedulerPreparedOccurrence) resourceMailboxMessage {
	return resourceMailboxMessage{
		ID: prepared.MessageID, ResourceID: prepared.Target, Text: prepared.Text,
		RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, ModeFrozen: true,
		Type: resourceMessageTypeScheduleOccurrence, Causation: cloneMailboxCausation(prepared.Causation),
		SenderWorkspaceInstanceID: prepared.Causation.SourceWorkspaceInstanceID,
	}
}

func mustSchedulerTriggerDigest(t *testing.T, trigger *app.ScheduleTrigger) string {
	t.Helper()
	digest, err := schedulerTriggerDigest(trigger)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func assertPreparedOccurrenceEqual(t *testing.T, got *schedulerPreparedOccurrence, want schedulerPreparedOccurrence) {
	t.Helper()
	if got == nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("prepared occurrence = %#v, want %#v", got, want)
	}
}

func assertDeliveredPreparedOccurrence(t *testing.T, messages []resourceMailboxMessage, prepared schedulerPreparedOccurrence) {
	t.Helper()
	if len(messages) != 1 {
		t.Fatalf("delivered occurrence count = %d in %#v, want 1", len(messages), messages)
	}
	message := messages[0]
	if message.ID != prepared.MessageID || !reflect.DeepEqual(message.Causation, prepared.Causation) {
		t.Fatalf("delivered occurrence changed identity or causation: %#v, want %s/%#v", message, prepared.MessageID, prepared.Causation)
	}
}

func assertSingleDurableOccurrence(t *testing.T, workspacePath string, prepared schedulerPreparedOccurrence, want int) {
	t.Helper()
	messages := scheduleOccurrenceMessages(t, workspacePath, prepared.Target)
	matching := 0
	for _, message := range messages {
		if message.ID == prepared.MessageID {
			matching++
			if message.Causation == nil || message.Causation.OccurrenceID != prepared.OccurrenceID {
				t.Fatalf("occurrence causation changed: %#v", message)
			}
		}
	}
	if matching != want || len(messages) != want {
		t.Fatalf("durable occurrence copies = %d in %#v, want %d", matching, messages, want)
	}

	store, err := loadResourceMailboxStoreForRead(workspacePath, prepared.Target)
	if err != nil {
		t.Fatal(err)
	}
	hotCopies := 0
	for _, message := range store.Mailbox.Messages {
		if !message.receipt && message.ID == prepared.MessageID {
			hotCopies++
		}
	}
	receiptCopies := 0
	for _, receipt := range store.Receipts.Receipts {
		if receipt.ID == prepared.MessageID {
			receiptCopies++
		}
	}
	if hotCopies+receiptCopies != want {
		t.Fatalf("physical occurrence copies = hot %d + receipts %d, want %d", hotCopies, receiptCopies, want)
	}
}

func TestNativeSchedulerHonorsPersistedDeliveryBackoff(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
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

func TestNativeSchedulerBindingAttentionRecoversPreparedOccurrence(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, configPath := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	rewriteSchedulerTestProfiles(t, configPath, nil)
	anchor := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Recover binding", Condition: "every minute", Target: "project1.task1",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: anchor.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := anchor.Add(3*time.Minute + 10*time.Second)
	native := newNativeScheduler(manager, workspace)
	deadline, err := native.Reconcile(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if !deadline.IsZero() {
		t.Fatalf("binding attention deadline = %s, want none", deadline)
	}
	runtime, err := native.schedulerRuntime(schedule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.EffectiveState != schedulerOutcomeAttention || runtime.Prepared == nil || runtime.NextRunAt != "" || runtime.RetryAt != "" || runtime.LastOccurrenceAt != "" {
		t.Fatalf("binding attention advanced the occurrence: %#v", runtime)
	}
	prepared := *runtime.Prepared
	if prepared.ScheduledFor != anchor.Format(time.RFC3339Nano) || prepared.CoalescedThrough != anchor.Add(3*time.Minute).Format(time.RFC3339Nano) || prepared.CoalescedCount != 4 || prepared.NextRunAt != anchor.Add(4*time.Minute).Format(time.RFC3339Nano) || prepared.Reason != schedulerOccurrenceReasonCoalesced {
		t.Fatalf("binding attention did not freeze the coalesced occurrence: %#v", prepared)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 0 {
		t.Fatalf("binding attention accepted mailbox work: %#v", messages)
	}
	resumed, err := native.Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangeResume, ID: schedule.ID})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Revision != schedule.Revision || resumed.State != app.ScheduleStateActive {
		t.Fatalf("attention retry mutated the portable definition: %#v", resumed)
	}
	runtime, err = native.schedulerRuntime(schedule.ID)
	if err != nil || runtime.EffectiveState != schedulerOutcomeAttention || runtime.NextRunAt != "" || runtime.RetryAt != "" {
		t.Fatalf("resume discarded binding attention occurrence: %#v, %v", runtime, err)
	}
	assertPreparedOccurrenceEqual(t, runtime.Prepared, prepared)
	if snapshot, snapshotErr := native.Snapshot(now); snapshotErr != nil || snapshot.NextWakeAt != "" {
		t.Fatalf("binding attention retry acquired a deadline: %#v, %v", snapshot, snapshotErr)
	}

	rewriteSchedulerTestProfiles(t, configPath, []agentHubProfileRoute{{Key: "default", AgentName: "fake-agent"}})
	if _, err := native.Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangeResume, ID: schedule.ID}); err != nil {
		t.Fatal(err)
	}
	messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target)
	assertDeliveredPreparedOccurrence(t, messages, prepared)
	runtime, err = native.schedulerRuntime(schedule.ID)
	if err != nil || runtime.Prepared != nil || runtime.EffectiveState != app.ScheduleStateActive || runtime.LastOutcome != schedulerOutcomeAccepted || runtime.LastOccurrenceAt != prepared.CoalescedThrough || runtime.NextRunAt != prepared.NextRunAt {
		t.Fatalf("recovered occurrence checkpoint = %#v, %v", runtime, err)
	}
	if _, err := native.Reconcile(context.Background(), now.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 1 || messages[0].ID != prepared.MessageID {
		t.Fatalf("recovery duplicated occurrence: %#v", messages)
	}
}

func TestNativeSchedulerTransientBindingPreflightUsesBackoff(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	failCatalog := true
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failCatalog && r.Method == http.MethodGet && r.URL.Path == "/v1/agents" {
			failCatalog = false
			w.WriteHeader(http.StatusServiceUnavailable)
			writeRuntimeFakeJSON(w, map[string]any{"error": map[string]any{
				"code": "runtime_unavailable", "message": "synthetic catalog outage", "retryable": true,
			}})
			return
		}
		fake.ServeHTTP(w, r)
	}))
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Retry transiently", Condition: "once", Target: "project1.task1",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := at.Add(time.Second)
	native := newNativeScheduler(manager, workspace)
	deadline, err := native.Reconcile(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := native.schedulerRuntime(schedule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.EffectiveState == schedulerOutcomeAttention || runtime.Prepared == nil || runtime.RetryAt == "" || runtime.NextRunAt != at.Format(time.RFC3339Nano) || runtime.LastOccurrenceAt != "" {
		t.Fatalf("transient preflight classification = %#v", runtime)
	}
	retryAt := generationTime(runtime.RetryAt)
	if retryAt.IsZero() || !deadline.Equal(retryAt) {
		t.Fatalf("transient retry deadline = %s, want %s", deadline, retryAt)
	}
	prepared := *runtime.Prepared
	if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 0 {
		t.Fatalf("transient preflight accepted mailbox work: %#v", messages)
	}
	if _, err := native.Reconcile(context.Background(), retryAt); err != nil {
		t.Fatal(err)
	}
	messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target)
	if len(messages) != 1 || messages[0].ID != prepared.MessageID {
		t.Fatalf("transient retry lost stable occurrence: %#v, want %s", messages, prepared.MessageID)
	}
}

func rewriteSchedulerTestProfiles(t *testing.T, configPath string, profiles []agentHubProfileRoute) {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg agentHubServeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.AgentProfiles = profiles
	data, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNativeSchedulerDoesNotReplayCompletedOneTimeOnSemanticEdit(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
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
	if _, err := newNativeScheduler(manager, workspace).Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangeResume, ID: repeating.ID}); err != nil {
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

func TestNativeSchedulerResumeSkipsExpiredOneTimeWithoutReconcile(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
		Description: "Once", Condition: "in one hour", Target: "workspace",
		Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.PauseSchedule(created.ID); err != nil {
		t.Fatal(err)
	}

	// Model a stopped Server: the paused definition is not reconciled until a
	// resume request arrives after its one-time occurrence has elapsed.
	manager.now = func() time.Time { return at.Add(time.Minute) }
	native := newNativeScheduler(manager, workspace)
	resumed, err := native.Change(context.Background(), NativeSchedulerChange{Operation: app.ScheduleChangeResume, ID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != app.ScheduleStateActive {
		t.Fatalf("resumed schedule state = %q, want active", resumed.State)
	}
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	snapshot, err := native.Snapshot(manager.now())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Schedules) != 1 || snapshot.Schedules[0].EffectiveState != app.ScheduleStateCompleted || snapshot.Schedules[0].LastOutcome != schedulerOutcomePaused || snapshot.Schedules[0].LastOccurrenceAt != at.Format(time.RFC3339Nano) {
		t.Fatalf("resumed expired one-time snapshot = %#v", snapshot)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace"); len(messages) != 0 {
		t.Fatalf("resume delivered an occurrence skipped while paused: %#v", messages)
	}
}

func TestNativeSchedulerUnavailableTargetPreservesPreparedOccurrence(t *testing.T) {
	tests := []struct {
		name        string
		makeMissing func(*testing.T, *app.Workspace, string, string) func()
	}{
		{
			name: "archived",
			makeMissing: func(t *testing.T, workspace *app.Workspace, workspacePath, targetPath string) func() {
				archived, err := workspace.ArchiveResource("project1.task1")
				if err != nil {
					t.Fatal(err)
				}
				archivedPath := filepath.Join(workspacePath, filepath.FromSlash(archived.Path))
				return func() {
					if err := os.Rename(archivedPath, targetPath); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "missing",
			makeMissing: func(t *testing.T, _ *app.Workspace, _ string, targetPath string) func() {
				detachedPath := filepath.Join(t.TempDir(), "missing-target")
				if err := os.Rename(targetPath, detachedPath); err != nil {
					t.Fatal(err)
				}
				return func() {
					if err := os.Rename(detachedPath, targetPath); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newRuntimeFakeAgentHub()
			hub := httptest.NewServer(fake)
			defer hub.Close()
			manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			target, err := puaWorkspace.ResourceValue("project1.task1")
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
			schedule, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Recover target", Condition: "once", Target: "project1.task1",
				Trigger: &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at.Format(time.RFC3339Nano)},
			})
			if err != nil {
				t.Fatal(err)
			}
			targetPath := filepath.Join(workspace.Path, filepath.FromSlash(target.Path))
			restore := test.makeMissing(t, puaWorkspace, workspace.Path, targetPath)
			now := at.Add(time.Second)
			native := newNativeScheduler(manager, workspace)
			deadline, err := native.Reconcile(context.Background(), now)
			if err != nil || !deadline.IsZero() {
				t.Fatalf("unavailable target deadline = %s, %v", deadline, err)
			}
			runtime, err := native.schedulerRuntime(schedule.ID)
			if err != nil || runtime.EffectiveState != schedulerOutcomeAttention || runtime.Prepared == nil || runtime.NextRunAt != "" || runtime.RetryAt != "" || runtime.LastOccurrenceAt != "" {
				t.Fatalf("unavailable target advanced the occurrence: %#v, %v", runtime, err)
			}
			prepared := *runtime.Prepared
			if prepared.Target != schedule.Target || prepared.ScheduledFor != at.Format(time.RFC3339Nano) || prepared.CoalescedThrough != at.Format(time.RFC3339Nano) {
				t.Fatalf("unavailable target prepared occurrence = %#v", prepared)
			}
			if deadline, err = native.Reconcile(context.Background(), now.Add(time.Minute)); err != nil || !deadline.IsZero() {
				t.Fatalf("repeated attention deadline = %s, %v", deadline, err)
			}
			runtime, err = native.schedulerRuntime(schedule.ID)
			if err != nil {
				t.Fatal(err)
			}
			assertPreparedOccurrenceEqual(t, runtime.Prepared, prepared)
			if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 0 {
				t.Fatalf("unavailable target accepted mailbox work: %#v", messages)
			}

			restore()
			if _, err := native.Reconcile(context.Background(), now.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target)
			assertDeliveredPreparedOccurrence(t, messages, prepared)
			runtime, err = native.schedulerRuntime(schedule.ID)
			if err != nil || runtime.Prepared != nil || runtime.EffectiveState != app.ScheduleStateCompleted || runtime.LastOutcome != schedulerOutcomeAccepted || runtime.LastOccurrenceAt != prepared.CoalescedThrough {
				t.Fatalf("restored target checkpoint = %#v, %v", runtime, err)
			}
			if _, err := native.Reconcile(context.Background(), now.Add(3*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if messages := scheduleOccurrenceMessages(t, workspace.Path, schedule.Target); len(messages) != 1 || messages[0].ID != prepared.MessageID {
				t.Fatalf("restored target duplicated the occurrence: %#v", messages)
			}
		})
	}
}

func TestNativeSchedulerTargetEditReplacesPreparedOccurrence(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
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
	native := newNativeScheduler(manager, workspace)
	snapshot, err := native.Snapshot(manager.now())
	if err != nil || snapshot.Schedules[0].EffectiveState != schedulerOutcomeAttention || snapshot.Schedules[0].NextRunAt != "" || snapshot.NextWakeAt != "" {
		t.Fatalf("archived target snapshot = %#v, %v", snapshot, err)
	}
	runtime, err := native.schedulerRuntime(created.ID)
	if err != nil || runtime.Prepared == nil {
		t.Fatalf("archived target did not preserve prepared occurrence: %#v, %v", runtime, err)
	}
	prepared := *runtime.Prepared
	description := "Still archived"
	updated, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, ExpectedRevision: created.Revision, Description: &description})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	snapshot, err = native.Snapshot(manager.now())
	if err != nil || snapshot.Schedules[0].EffectiveState != schedulerOutcomeAttention {
		t.Fatalf("unrelated edit cleared target attention: %#v, %v", snapshot, err)
	}
	runtime, err = native.schedulerRuntime(created.ID)
	if err != nil || runtime.Revision != updated.Revision {
		t.Fatalf("unrelated edit runtime = %#v, %v", runtime, err)
	}
	assertPreparedOccurrenceEqual(t, runtime.Prepared, prepared)
	target := "workspace"
	retargeted, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, ExpectedRevision: updated.Revision, Target: &target})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	messages := scheduleOccurrenceMessages(t, workspace.Path, "workspace")
	if len(messages) != 1 || messages[0].ID == prepared.MessageID || messages[0].Causation == nil || messages[0].Causation.ScheduleRevision != retargeted.Revision {
		t.Fatalf("retargeted attention schedule did not prepare a new revision: %#v", messages)
	}
	if messages := scheduleOccurrenceMessages(t, workspace.Path, created.Target); len(messages) != 0 {
		t.Fatalf("retargeting delivered the discarded occurrence: %#v", messages)
	}
	runtime, err = native.schedulerRuntime(created.ID)
	if err != nil || runtime.Revision != retargeted.Revision || runtime.Prepared != nil || runtime.EffectiveState != app.ScheduleStateCompleted || runtime.LastOutcome != schedulerOutcomeAccepted {
		t.Fatalf("retargeted attention checkpoint = %#v, %v", runtime, err)
	}
}

func TestNativeSchedulerTriggerEditReplacesAttentionRuntime(t *testing.T) {
	tests := []struct {
		name         string
		oldTrigger   func(time.Time) *app.ScheduleTrigger
		overdueAt    func(app.Schedule) time.Time
		newTrigger   func(time.Time, app.Schedule) app.ScheduleTrigger
		changeNative bool
		restart      bool
	}{
		{
			name: "interval through native change",
			oldTrigger: func(base time.Time) *app.ScheduleTrigger {
				return &app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 60, AnchorAt: base.Add(-time.Hour).Format(time.RFC3339Nano)}
			},
			overdueAt: func(schedule app.Schedule) time.Time {
				return generationTime(schedule.UpdatedAt).Add(10 * time.Minute)
			},
			newTrigger: func(base time.Time, _ app.Schedule) app.ScheduleTrigger {
				return app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: 300, AnchorAt: base.Add(-time.Hour).Format(time.RFC3339Nano)}
			},
			changeNative: true,
		},
		{
			name: "cron after restart audit",
			oldTrigger: func(time.Time) *app.ScheduleTrigger {
				return &app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "0 * * * * *", TimeZone: "UTC"}
			},
			overdueAt: func(schedule app.Schedule) time.Time {
				return generationTime(schedule.UpdatedAt).Add(10 * time.Minute)
			},
			newTrigger: func(time.Time, app.Schedule) app.ScheduleTrigger {
				return app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: "0 0 * * * *", TimeZone: "UTC"}
			},
			restart: true,
		},
		{
			name: "one-time through portable revision",
			oldTrigger: func(base time.Time) *app.ScheduleTrigger {
				return &app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: base.Add(time.Minute).Format(time.RFC3339Nano)}
			},
			overdueAt: func(schedule app.Schedule) time.Time {
				return generationTime(schedule.Trigger.At).Add(time.Second)
			},
			newTrigger: func(_ time.Time, schedule app.Schedule) app.ScheduleTrigger {
				return app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: generationTime(schedule.Trigger.At).Add(2 * time.Hour).Format(time.RFC3339Nano)}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newRuntimeFakeAgentHub()
			hub := httptest.NewServer(fake)
			defer hub.Close()
			manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			const targetID = "project1.task1"
			target, err := puaWorkspace.ResourceValue(targetID)
			if err != nil {
				t.Fatal(err)
			}
			base := time.Now().UTC().Truncate(time.Second)
			created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{
				Description: "Replace frozen trigger", Condition: "old rule", Target: targetID,
				Trigger: test.oldTrigger(base),
			})
			if err != nil {
				t.Fatal(err)
			}
			archived, err := puaWorkspace.ArchiveResource(targetID)
			if err != nil {
				t.Fatal(err)
			}
			targetPath := filepath.Join(workspace.Path, filepath.FromSlash(target.Path))
			archivedPath := filepath.Join(workspace.Path, filepath.FromSlash(archived.Path))
			native := newNativeScheduler(manager, workspace)
			overdue := test.overdueAt(created)
			if _, err := native.Reconcile(context.Background(), overdue); err != nil {
				t.Fatal(err)
			}
			oldRuntime, err := native.schedulerRuntime(created.ID)
			if err != nil || oldRuntime.EffectiveState != schedulerOutcomeAttention || oldRuntime.Prepared == nil {
				t.Fatalf("overdue trigger attention runtime = %#v, %v", oldRuntime, err)
			}
			oldPrepared := *oldRuntime.Prepared

			trigger := test.newTrigger(base, created)
			var updated app.Schedule
			if test.changeNative {
				updated, err = native.Change(context.Background(), NativeSchedulerChange{
					Operation: app.ScheduleChangeUpdate, ID: created.ID, ExpectedRevision: created.Revision, Trigger: &trigger,
				})
			} else {
				// Model an audit discovering a portable definition revision that
				// was written while this Server was not processing mutations.
				updated, err = puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{
					ID: created.ID, ExpectedRevision: created.Revision, Trigger: &trigger,
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(archivedPath, targetPath); err != nil {
				t.Fatal(err)
			}
			mutationAt := generationTime(updated.UpdatedAt)
			if test.restart {
				manager = newAgentManager(manager.server)
				manager.server.agents = manager
				native = newNativeScheduler(manager, workspace)
			}
			if _, err := native.Reconcile(context.Background(), mutationAt); err != nil {
				t.Fatal(err)
			}

			runtime, err := native.schedulerRuntime(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			next := generationTime(runtime.NextRunAt)
			if runtime.Revision != updated.Revision || runtime.TriggerDigest != mustSchedulerTriggerDigest(t, updated.Trigger) || runtime.EffectiveState != app.ScheduleStateActive || runtime.Prepared != nil || runtime.AttentionTarget != "" || runtime.LastOccurrenceAt != "" || !next.After(mutationAt) {
				t.Fatalf("replacement trigger runtime = %#v; mutation = %s", runtime, mutationAt)
			}
			if messages := scheduleOccurrenceMessages(t, workspace.Path, targetID); len(messages) != 0 {
				t.Fatalf("trigger edit accepted old occurrence %s: %#v", oldPrepared.MessageID, messages)
			}
		})
	}
}
