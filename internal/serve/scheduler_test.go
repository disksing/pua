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

func TestSchedulerHTTPAPIUsesApplicationStore(t *testing.T) {
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
	createdResponse := request(http.MethodPost, "/api/workspaces/workspace-scheduler/scheduler", `{"description":"Review","condition":"tomorrow","target":"workspace"}`)
	if createdResponse.Code != http.StatusOK {
		t.Fatalf("create schedule = %d %s", createdResponse.Code, createdResponse.Body.String())
	}
	var created app.Schedule
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("created schedule = %#v, %v", created, err)
	}
	if request := s.agents.takeReconcileRequests(); request&reconcileScheduler == 0 {
		t.Fatalf("create schedule did not wake Scheduler reconciliation: %08b", request)
	}
	updatedResponse := request(http.MethodPut, "/api/workspaces/workspace-scheduler/scheduler/"+created.ID, `{"condition":"next week","target":"scheduler"}`)
	var updated app.Schedule
	updatedErr := json.Unmarshal(updatedResponse.Body.Bytes(), &updated)
	if updatedResponse.Code != http.StatusOK || updatedErr != nil || updated.Target != app.SchedulerResourceID {
		t.Fatalf("update schedule = %d %s", updatedResponse.Code, updatedResponse.Body.String())
	}
	settingsResponse := request(http.MethodPut, "/api/workspaces/workspace-scheduler/scheduler/settings", `{"agentBinding":{"kind":"agent","name":"cheap-scheduler"},"wakeIntervalMinutes":60,"conditionMaxChars":16,"descriptionMaxChars":8}`)
	var settings app.SchedulerConfig
	settingsErr := json.Unmarshal(settingsResponse.Body.Bytes(), &settings)
	if settingsResponse.Code != http.StatusOK || settingsErr != nil || settings.WakeIntervalMinutes != 60 || settings.ConditionMaxChars != 16 || settings.DescriptionMaxChars != 8 {
		t.Fatalf("update Scheduler settings = %d %s", settingsResponse.Code, settingsResponse.Body.String())
	}
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		path := "/api/workspaces/workspace-scheduler/scheduler"
		if method == http.MethodPut {
			path += "/" + created.ID
		}
		response := request(method, path, `{"description":"Too long","condition":"`+strings.Repeat("\u4e2d", 17)+`","target":"workspace"}`)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "at most 16 characters") {
			t.Fatalf("oversized %s = %d %s", method, response.Code, response.Body.String())
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		path := "/api/workspaces/workspace-scheduler/scheduler"
		if method == http.MethodPut {
			path += "/" + created.ID
		}
		response := request(method, path, `{"description":"`+strings.Repeat("😀", 9)+`","condition":"now","target":"workspace"}`)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "description must be at most 8 characters (got 9)") {
			t.Fatalf("oversized description %s = %d %s", method, response.Code, response.Body.String())
		}
	}
	// Raise the description limit, store an oversized value, then lower it again.
	if response := request(http.MethodPut, "/api/workspaces/workspace-scheduler/scheduler/settings", `{"agentBinding":{"kind":"agent","name":"cheap-scheduler"},"wakeIntervalMinutes":60,"descriptionMaxChars":12}`); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"conditionMaxChars": 16`) {
		t.Fatalf("omitted condition limit = %d %s", response.Code, response.Body.String())
	}
	longDescription := strings.Repeat("\u4e2d", 12)
	if response := request(http.MethodPut, "/api/workspaces/workspace-scheduler/scheduler/"+created.ID, `{"description":"`+longDescription+`"}`); response.Code != http.StatusOK {
		t.Fatalf("raise description = %d %s", response.Code, response.Body.String())
	}
	if response := request(http.MethodPut, "/api/workspaces/workspace-scheduler/scheduler/settings", `{"agentBinding":{"kind":"agent","name":"cheap-scheduler"},"wakeIntervalMinutes":60,"descriptionMaxChars":4}`); response.Code != http.StatusOK {
		t.Fatalf("lower description limit = %d %s", response.Code, response.Body.String())
	}
	otherFields := request(http.MethodPut, "/api/workspaces/workspace-scheduler/scheduler/"+created.ID, `{"condition":"next month","target":"workspace"}`)
	var preserved app.Schedule
	if err := json.Unmarshal(otherFields.Body.Bytes(), &preserved); err != nil || otherFields.Code != http.StatusOK || preserved.Description != longDescription || preserved.Condition != "next month" {
		t.Fatalf("update other fields with long description = %d %s", otherFields.Code, otherFields.Body.String())
	}
	invalidSettings := request(http.MethodPut, "/api/workspaces/workspace-scheduler/scheduler/settings", `{"agentBinding":{"kind":"profile","name":"default"},"wakeIntervalMinutes":30,"conditionMaxChars":0}`)
	if invalidSettings.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit = %d %s", invalidSettings.Code, invalidSettings.Body.String())
	}
	invalidDescriptionLimit := request(http.MethodPut, "/api/workspaces/workspace-scheduler/scheduler/settings", `{"agentBinding":{"kind":"profile","name":"default"},"wakeIntervalMinutes":30,"descriptionMaxChars":0}`)
	if invalidDescriptionLimit.Code != http.StatusBadRequest {
		t.Fatalf("invalid description limit = %d %s", invalidDescriptionLimit.Code, invalidDescriptionLimit.Body.String())
	}
	legacySettings := request(http.MethodPut, "/api/workspaces/workspace-scheduler/scheduler/settings", `{"agentBinding":{"kind":"profile","name":"default"},"wakeIntervalMinutes":45}`)
	if legacySettings.Code != http.StatusOK || !strings.Contains(legacySettings.Body.String(), `"conditionMaxChars": 16`) || !strings.Contains(legacySettings.Body.String(), `"descriptionMaxChars": 4`) {
		t.Fatalf("omitted limit = %d %s", legacySettings.Code, legacySettings.Body.String())
	}
	readResponse := request(http.MethodGet, "/api/workspaces/workspace-scheduler/scheduler", "")
	if readResponse.Code != http.StatusOK || !strings.Contains(readResponse.Body.String(), created.ID) {
		t.Fatalf("read Scheduler = %d %s", readResponse.Code, readResponse.Body.String())
	}
	removedResponse := request(http.MethodDelete, "/api/workspaces/workspace-scheduler/scheduler/"+created.ID, "")
	if removedResponse.Code != http.StatusOK {
		t.Fatalf("remove schedule = %d %s", removedResponse.Code, removedResponse.Body.String())
	}
}

func schedulerMessages(t *testing.T, workspacePath string) []resourceMailboxMessage {
	t.Helper()
	mailbox, err := loadResourceMailbox(workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	result := make([]resourceMailboxMessage, 0)
	for _, message := range mailbox.Messages {
		if message.ResourceID == app.SchedulerResourceID && message.Type == resourceMessageTypeSchedulerTick {
			result = append(result, message)
		}
	}
	return result
}

func TestSchedulerReconcileSkipsEmptyUsesCompletedTickIntervalAndCoalescesChanges(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	client, err := newAgentHubClient(hub.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	err = manager.reconcileSchedulerLocked(context.Background(), workspace, client)
	if err != nil || len(schedulerMessages(t, workspace.Path)) != 0 {
		t.Fatalf("empty Scheduler reconcile = %v, messages=%#v", err, schedulerMessages(t, workspace.Path))
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{Description: "Check status", Condition: "when appropriate", Target: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	err = manager.reconcileSchedulerLocked(context.Background(), workspace, client)
	if err != nil {
		t.Fatal(err)
	}
	ticks := schedulerMessages(t, workspace.Path)
	if len(ticks) != 1 || ticks[0].Status != resourceMessageDelivered || ticks[0].Causation == nil || ticks[0].Causation.Reason != schedulerTickReasonChanged {
		t.Fatalf("first Scheduler tick = %#v", ticks)
	}
	first := ticks[0]
	endedAt := time.Date(2026, 8, 13, 8, 0, 0, 0, time.UTC)
	first, err = updateMailboxMessage(workspace.Path, first.ID, func(message *resourceMailboxMessage) {
		message.TurnID = "turn-scheduler-1"
	})
	if err != nil {
		t.Fatal(err)
	}
	record, found, err := generationRecordByID(workspace.Path, first.GenerationID)
	if err != nil || !found {
		t.Fatalf("Scheduler generation = %#v, found=%v err=%v", record, found, err)
	}
	fake.mu.Lock()
	if fake.turns[record.AgentHubSessionID] == nil {
		fake.turns[record.AgentHubSessionID] = make(map[string]agentHubTurn)
	}
	fake.turns[record.AgentHubSessionID][first.TurnID] = agentHubTurn{TurnID: first.TurnID, Status: "completed", Closed: true, EndedAt: endedAt.Format(time.RFC3339Nano)}
	session := fake.sessions[record.AgentHubSessionID]
	session.State = "ready"
	session.CurrentTurnID = ""
	fake.sessions[record.AgentHubSessionID] = session
	fake.mu.Unlock()

	manager.now = func() time.Time { return endedAt.Add(29 * time.Minute) }
	err = manager.reconcileSchedulerLocked(context.Background(), workspace, client)
	if err != nil || len(schedulerMessages(t, workspace.Path)) != 1 {
		t.Fatalf("early interval reconcile = %v, messages=%#v", err, schedulerMessages(t, workspace.Path))
	}
	if next := manager.nextSchedulerReconcileDeadline(endedAt.Add(29 * time.Minute)); !next.Equal(endedAt.Add(30 * time.Minute)) {
		t.Fatalf("next Scheduler deadline = %s, want %s", next, endedAt.Add(30*time.Minute))
	}
	// A later ordinary user message does not participate in Scheduler timing.
	appendNotificationTestMessage(t, workspace.Path, resourceMailboxMessage{
		ID: "msg-user-after-tick", ResourceID: app.SchedulerResourceID, Text: "User chat", Role: "user",
		RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue,
		Status: resourceMessageDelivered, AcceptedAt: endedAt.Add(time.Minute).Format(time.RFC3339Nano), UpdatedAt: endedAt.Add(time.Minute).Format(time.RFC3339Nano),
	})
	manager.now = func() time.Time { return endedAt.Add(30 * time.Minute) }
	err = manager.reconcileSchedulerLocked(context.Background(), workspace, client)
	if err != nil {
		t.Fatal(err)
	}
	ticks = schedulerMessages(t, workspace.Path)
	if len(ticks) != 2 || ticks[1].Causation == nil || ticks[1].Causation.Reason != schedulerTickReasonInterval {
		t.Fatalf("due interval tick = %#v", ticks)
	}

	description := "Check status with new context"
	if _, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, Description: &description}); err != nil {
		t.Fatal(err)
	}
	err = manager.reconcileSchedulerLocked(context.Background(), workspace, client)
	if err != nil {
		t.Fatal(err)
	}
	ticks = schedulerMessages(t, workspace.Path)
	if len(ticks) != 3 || ticks[2].Status != resourceMessageQueued || ticks[2].Causation.Reason != schedulerTickReasonChanged {
		t.Fatalf("queued change tick = %#v", ticks)
	}
	condition := "when the updated condition is satisfied"
	if _, err := puaWorkspace.UpdateSchedule(app.UpdateScheduleInput{ID: created.ID, Condition: &condition}); err != nil {
		t.Fatal(err)
	}
	err = manager.reconcileSchedulerLocked(context.Background(), workspace, client)
	if err != nil || len(schedulerMessages(t, workspace.Path)) != 3 {
		t.Fatalf("coalesced change reconcile = %v, messages=%#v", err, schedulerMessages(t, workspace.Path))
	}
	if _, err := puaWorkspace.RemoveSchedule(created.ID); err != nil {
		t.Fatal(err)
	}
	err = manager.reconcileSchedulerLocked(context.Background(), workspace, client)
	ticks = schedulerMessages(t, workspace.Path)
	if err != nil || ticks[2].Status != resourceMessageUndeliverable || ticks[2].LastErrorCode != "scheduler_empty" {
		t.Fatalf("empty-list cancellation = %v, messages=%#v", err, ticks)
	}
}

func TestFailedSchedulerTickDoesNotResetInterval(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	client, _ := newAgentHubClient(hub.URL, nil)
	puaWorkspace, _ := app.OpenWorkspace(workspace.Path)
	if _, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{Description: "Retry", Condition: "always inspect", Target: "workspace"}); err != nil {
		t.Fatal(err)
	}
	err := manager.reconcileSchedulerLocked(context.Background(), workspace, client)
	if err != nil {
		t.Fatal(err)
	}
	tick := schedulerMessages(t, workspace.Path)[0]
	tick, err = updateMailboxMessage(workspace.Path, tick.ID, func(message *resourceMailboxMessage) { message.TurnID = "turn-failed" })
	if err != nil {
		t.Fatal(err)
	}
	record, _, _ := generationRecordByID(workspace.Path, tick.GenerationID)
	fake.mu.Lock()
	if fake.turns[record.AgentHubSessionID] == nil {
		fake.turns[record.AgentHubSessionID] = make(map[string]agentHubTurn)
	}
	fake.turns[record.AgentHubSessionID][tick.TurnID] = agentHubTurn{TurnID: tick.TurnID, Status: "failed", Closed: true, EndedAt: time.Now().Format(time.RFC3339Nano)}
	session := fake.sessions[record.AgentHubSessionID]
	session.State = "ready"
	fake.sessions[record.AgentHubSessionID] = session
	fake.mu.Unlock()
	err = manager.reconcileSchedulerLocked(context.Background(), workspace, client)
	ticks := schedulerMessages(t, workspace.Path)
	if err != nil || len(ticks) != 2 || ticks[1].Causation.Reason != schedulerTickReasonRecovery {
		t.Fatalf("failed tick recovery = %v, messages=%#v", err, ticks)
	}
}
