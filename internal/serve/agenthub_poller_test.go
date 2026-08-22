package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/generation"
)

// seedPollerGeneration registers an AgentHub session in the fake and persists the
// matching local generation projection for the poller to reconcile.
func seedPollerGeneration(t *testing.T, fake *runtimeFakeAgentHub, workspace serveWorkspace, record generationRecord, session agentHubSession) {
	t.Helper()
	if session.Source == nil {
		session.Source = &agentHubSource{
			App: agentHubSourceApp, InstanceID: "pua-runtime-test", ExternalID: record.SourceExternalID,
		}
	}
	fake.mu.Lock()
	fake.sessions[session.ID] = session
	fake.mu.Unlock()
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
}

func pollerGenerationState(rt *agentRuntime) generationRecord {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.record
}

func TestAgentHubPollerReconcilesMultipleGenerationsWithSingleList(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	seedPollerGeneration(t, fake, workspace, generationRecord{
		ID: "gen-a", WorkspaceID: workspace.ID, ResourceID: "project1", AgentHubSessionID: "ses_a",
		SourceExternalID: workspace.ID + "/run-a", Status: "running",
		CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}, agentHubSession{ID: "ses_a", State: "ready", UpdatedAt: "2026-08-01T00:00:10Z"})
	seedPollerGeneration(t, fake, workspace, generationRecord{
		ID: "gen-b", WorkspaceID: workspace.ID, ResourceID: "project1.task1", AgentHubSessionID: "ses_b",
		SourceExternalID: workspace.ID + "/run-b", Status: "starting",
		CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}, agentHubSession{ID: "ses_b", State: "stopped", UpdatedAt: "2026-08-01T00:00:11Z"})
	// Give gen-a a visible durable cursor so the test can wait for the
	// completion worker's final save instead of observing its brief setup
	// window under -race.
	fake.mu.Lock()
	fake.events["ses_a"] = []agentHubEvent{{
		ID: 1, Time: "2026-08-01T00:00:10Z", Type: "session.state", SessionID: "ses_a",
		Data: []byte(`{"state":"ready"}`),
	}}
	sessionA := fake.sessions["ses_a"]
	sessionA.LastEventID = 1
	fake.sessions["ses_a"] = sessionA
	fake.mu.Unlock()

	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	listCalls := fake.listCalls
	fake.mu.Unlock()
	if listCalls != 1 {
		t.Fatalf("one poll must issue exactly one session list, got %d", listCalls)
	}
	recordA := pollerGenerationState(manager.runtimeByID("gen-a"))
	waitForRuntimeTest(t, func() bool {
		record := pollerGenerationState(manager.runtimeByID("gen-a"))
		// CompletionSessionID is established before the completion worker marks
		// the durable event cursor. Wait for that cursor too so TempDir cleanup
		// cannot race the worker's final save under -race.
		return record.CompletionSessionID == "ses_a" && record.CompletionCursor >= 1 && !record.CompletionPending
	})
	recordA = pollerGenerationState(manager.runtimeByID("gen-a"))
	if recordA.Status != "idle" || recordA.UpdatedAt != "2026-08-01T00:00:10Z" || recordA.LastOutputAt != "2026-08-01T00:00:10Z" {
		t.Fatalf("gen-a projection not reconciled: %#v", recordA)
	}
	rtA := manager.runtimeByID("gen-a")
	rtA.mu.Lock()
	stateA := rtA.agentHubState
	rtA.mu.Unlock()
	if stateA != "ready" {
		t.Fatalf("gen-a AgentHub state = %q, want ready", stateA)
	}
	recordB := pollerGenerationState(manager.runtimeByID("gen-b"))
	if recordB.Status != "stopped" || !recordB.AgentHubStoppedObserved {
		t.Fatalf("gen-b projection not reconciled: %#v", recordB)
	}
	if response := closeRuntimeTestGeneration(t, manager, workspace, "gen-a"); response.Code != http.StatusOK {
		t.Fatalf("test cleanup close failed: %d %s", response.Code, response.Body.String())
	}
}

func TestAgentHubPollerProjectsTurnStartAndClearsStaleTurnIDAtReady(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	manager.now = func() time.Time { return time.Date(2026, 8, 1, 0, 0, 20, 0, time.UTC) }
	cfg, _, err := manager.agentHubRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := manager.resolveResourceAgent(workspace, "project1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	seedPollerGeneration(t, fake, workspace, generationRecord{
		ID: "gen-activity-turn", WorkspaceID: workspace.ID, ResourceID: "project1", Generation: 1,
		GenerationID: "gen-activity-turn", AgentHubSessionID: "ses_activity_turn",
		SourceExternalID: workspace.ID + "/run-activity-turn", Status: "idle", AgentHubAgentName: resolved.AgentName,
		BindingKind: resolved.Binding.Kind, BindingName: resolved.Binding.Name,
		ProfileRevision: resolved.ProfileRevision, ResolvedProfile: resolved.ResolvedProfile,
		CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}, agentHubSession{
		ID: "ses_activity_turn", State: "running", CurrentTurnID: "turn-activity",
		UpdatedAt: "2026-08-01T00:00:10Z",
	})
	fake.mu.Lock()
	fake.turns["ses_activity_turn"] = map[string]agentHubTurn{
		"turn-activity": {ID: "turn-activity", TurnID: "turn-activity", Status: "running", StartedAt: "2026-08-01T00:00:05Z"},
	}
	fake.mu.Unlock()

	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		return pollerGenerationState(manager.runtimeByID("gen-activity-turn")).TurnStartedAt == "2026-08-01T00:00:05Z"
	})
	record := pollerGenerationState(manager.runtimeByID("gen-activity-turn"))
	if record.Status != "running" || record.CurrentTurnID != "turn-activity" || record.LastTurnID != "turn-activity" || record.TurnNumber != 1 || record.TurnStartedAt != "2026-08-01T00:00:05Z" {
		t.Fatalf("poller did not project the started Turn: %#v", record)
	}

	// AgentHub may return to ready before clearing currentTurnId from its
	// session projection. PUA must treat ready as authoritative so Activity
	// stops presenting the resource as active.
	fake.mu.Lock()
	session := fake.sessions["ses_activity_turn"]
	session.State = "ready"
	session.CurrentTurnID = "turn-activity"
	session.UpdatedAt = "2026-08-01T00:00:20Z"
	fake.sessions[session.ID] = session
	fake.mu.Unlock()

	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	record = pollerGenerationState(manager.runtimeByID("gen-activity-turn"))
	if record.Status != "idle" || record.CurrentTurnID != "" || generationHasActiveTurn(record) {
		t.Fatalf("ready session retained a stale active Turn: %#v", record)
	}
	tree, err := manager.server.treeAt(context.Background(), workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Activity.Running) != 0 || tree.Projects[0].Runtime == nil || tree.Projects[0].Runtime.ActiveTurn {
		t.Fatalf("Activity did not converge to an idle project row: %#v", tree.Projects[0])
	}
}

func TestAgentHubPollerSkipsArchiveLookupForSchedulerResource(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	seedPollerGeneration(t, fake, workspace, generationRecord{
		ID: "gen-scheduler", WorkspaceID: workspace.ID, ResourceID: app.SchedulerResourceID,
		AgentHubSessionID: "ses_scheduler", SourceExternalID: workspace.ID + "/scheduler/1", Status: "idle",
		CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}, agentHubSession{ID: "ses_scheduler", State: "ready", UpdatedAt: "2026-08-01T00:00:10Z"})

	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatalf("Scheduler poll must not inspect the special resource as a Project/Task: %v", err)
	}
	if record := pollerGenerationState(manager.runtimeByID("gen-scheduler")); record.Status != "idle" {
		t.Fatalf("Scheduler run projection changed unexpectedly: %#v", record)
	}
}

func TestAgentHubPollerStopsSessionForArchivedTask(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	seedPollerGeneration(t, fake, workspace, generationRecord{
		ID: "gen-archived-task", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		AgentHubSessionID: "ses_archived_task", SourceExternalID: workspace.ID + "/run-archived-task",
		Status:    "idle",
		CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}, agentHubSession{ID: "ses_archived_task", State: "ready", UpdatedAt: "2026-08-01T00:00:10Z"})
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.ArchiveResource("project1.task1"); err != nil {
		t.Fatal(err)
	}

	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		state := fake.sessions["ses_archived_task"].State
		return state == "stopped" || state == "archived"
	})
	fake.mu.Lock()
	actions := append([]string(nil), fake.actions...)
	fake.mu.Unlock()
	if strings.Count(strings.Join(actions, ","), "stop") != 1 {
		t.Fatalf("archived task must stop its AgentHub session exactly once: %#v", actions)
	}
	waitForRuntimeTest(t, func() bool {
		record := pollerGenerationState(manager.runtimeByID("gen-archived-task"))
		return record.Status == "stopped" && record.AgentHubStoppedObserved
	})
}

func TestAgentHubPollerKeepsSessionForOpenTask(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	seedPollerGeneration(t, fake, workspace, generationRecord{
		ID: "gen-open-task", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		AgentHubSessionID: "ses_open_task", SourceExternalID: workspace.ID + "/run-open-task", Status: "idle",
		CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}, agentHubSession{ID: "ses_open_task", State: "ready", UpdatedAt: "2026-08-01T00:00:10Z"})

	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.sessions["ses_open_task"].State != "ready" || len(fake.actions) != 0 {
		t.Fatalf("open task session must remain ready: session=%#v actions=%#v", fake.sessions["ses_open_task"], fake.actions)
	}
}

func TestArchiveResourceAllowsActiveTurnAndConvergesAsynchronously(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	seedPollerGeneration(t, fake, workspace, generationRecord{
		ID: "gen-active-archive", WorkspaceID: workspace.ID, ResourceID: "project1.task1", Generation: 1, GenerationID: "gen-active-archive",
		AgentHubSessionID: "ses_active_archive", SourceExternalID: "project1.task1/1", Status: "idle",
		CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}, agentHubSession{ID: "ses_active_archive", State: "running", CurrentTurnID: "turn-active", UpdatedAt: "2026-08-01T00:00:10Z"})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-test/archive", strings.NewReader(`{"resourceId":"project1.task1"}`))
	manager.server.archiveResource(recorder, request, workspace.ID)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "archive") {
		t.Fatalf("active Turn archive = %d %s", recorder.Code, recorder.Body.String())
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	value, err := puaWorkspace.ResourceValue("project1.task1")
	if err != nil || !value.Archived {
		t.Fatalf("active resource was not archived before runtime convergence: %#v, %v", value, err)
	}
}

func TestAgentHubPollerStopsProjectSessionWhenProjectArchived(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	seedPollerGeneration(t, fake, workspace, generationRecord{
		ID: "gen-project", WorkspaceID: workspace.ID, ResourceID: "project1",
		AgentHubSessionID: "ses_project", SourceExternalID: workspace.ID + "/run-project", Status: "idle",
		CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}, agentHubSession{ID: "ses_project", State: "ready", UpdatedAt: "2026-08-01T00:00:10Z"})
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.ArchiveResource("project1.task1"); err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.ArchiveResource("project1"); err != nil {
		t.Fatal(err)
	}

	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		state := fake.sessions["ses_project"].State
		return state == "stopped" || state == "archived"
	})
	fake.mu.Lock()
	defer fake.mu.Unlock()
	state := fake.sessions["ses_project"].State
	if (state != "stopped" && state != "archived") || strings.Count(strings.Join(fake.actions, ","), "stop") != 1 {
		t.Fatalf("archived project session was not reclaimed: session=%#v actions=%#v", fake.sessions["ses_project"], fake.actions)
	}
}

func TestAgentHubPollerDoesNotStopArchivedTaskSessionWithMismatchedSource(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	record := generationRecord{
		ID: "gen-source-mismatch", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		AgentHubSessionID: "ses_source_mismatch", SourceExternalID: workspace.ID + "/run-source-mismatch", Status: "idle",
		CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}
	seedPollerGeneration(t, fake, workspace, record, agentHubSession{
		ID: "ses_source_mismatch", State: "ready", UpdatedAt: "2026-08-01T00:00:10Z",
		Source: &agentHubSource{App: agentHubSourceApp, InstanceID: "another-pua", ExternalID: record.SourceExternalID},
	})
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.ArchiveResource("project1.task1"); err != nil {
		t.Fatal(err)
	}

	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.sessions["ses_source_mismatch"].State != "ready" || fake.stopCalls != 0 {
		t.Fatalf("source mismatch must fail closed without stop: session=%#v stopCalls=%d", fake.sessions["ses_source_mismatch"], fake.stopCalls)
	}
}

func TestAgentHubPollerDoesNotStopSessionForMissingTask(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	seedPollerGeneration(t, fake, workspace, generationRecord{
		ID: "gen-missing-task", WorkspaceID: workspace.ID, ResourceID: "project1.task99",
		AgentHubSessionID: "ses_missing_task", SourceExternalID: workspace.ID + "/run-missing-task", Status: "idle",
		CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}, agentHubSession{ID: "ses_missing_task", State: "ready", UpdatedAt: "2026-08-01T00:00:10Z"})

	if err := manager.pollAgentHubSessions(context.Background()); err == nil || !strings.Contains(err.Error(), "resource not found: project1.task99") {
		t.Fatalf("missing task inspection error = %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.sessions["ses_missing_task"].State != "ready" || fake.stopCalls != 0 {
		t.Fatalf("missing task must fail closed without stop: session=%#v stopCalls=%d", fake.sessions["ses_missing_task"], fake.stopCalls)
	}
}

func TestAgentHubPollerRetriesAmbiguousArchivedTaskStop(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	fake.failNextStop = true
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	seedPollerGeneration(t, fake, workspace, generationRecord{
		ID: "gen-stop-failure", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		AgentHubSessionID: "ses_stop_failure", SourceExternalID: workspace.ID + "/run-stop-failure",
		Status:    "idle",
		CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}, agentHubSession{ID: "ses_stop_failure", State: "ready", UpdatedAt: "2026-08-01T00:00:10Z"})
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.ArchiveResource("project1.task1"); err != nil {
		t.Fatal(err)
	}

	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		rt := manager.runtimeByID("gen-stop-failure")
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return !rt.agentHubStopRequested && rt.record.ArchivedTaskStopRequested && rt.record.Status == "recovering"
	})
	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.stopCalls == 2 && (fake.sessions["ses_stop_failure"].State == "stopped" || fake.sessions["ses_stop_failure"].State == "archived")
	})
	fake.mu.Lock()
	stopCalls := fake.stopCalls
	session := fake.sessions["ses_stop_failure"]
	fake.mu.Unlock()
	if stopCalls != 2 || (session.State != "stopped" && session.State != "archived") {
		t.Fatalf("ambiguous stop did not converge through retry: session=%#v stopCalls=%d", session, stopCalls)
	}
	record := pollerGenerationState(manager.runtimeByID("gen-stop-failure"))
	if record.Status != "stopped" || !record.AgentHubStoppedObserved {
		t.Fatalf("retried stop did not persist terminal observation: %#v", record)
	}

	// Replacing the manager simulates a PUA restart. The converged archived
	// generation remains idempotent and needs no additional Stop request.
	restarted := newAgentManager(manager.server)
	if err := restarted.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.stopCalls != 2 {
		t.Fatalf("converged stop repeated after manager restart: stopCalls=%d", fake.stopCalls)
	}
	restartedGeneration := pollerGenerationState(restarted.runtimeByID("gen-stop-failure"))
	if restartedGeneration.Status != "stopped" || !restartedGeneration.AgentHubStoppedObserved {
		t.Fatalf("restart lost terminal reconciliation: %#v", restartedGeneration)
	}
}

func TestAgentHubPollerRunningToStoppedFinishesTurn(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	seedPollerGeneration(t, fake, workspace, generationRecord{
		ID: "gen-sched", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		AgentHubSessionID: "ses_sched", SourceExternalID: workspace.ID + "/run-sched",
		Status:    "running",
		CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}, agentHubSession{ID: "ses_sched", State: "stopped", UpdatedAt: "2026-08-01T00:00:10Z"})

	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	rt := manager.runtimeByID("gen-sched")
	waitForRuntimeTest(t, func() bool {
		record := pollerGenerationState(rt)
		return record.Status == "stopped"
	})
	record := pollerGenerationState(rt)
	if !record.AgentHubStoppedObserved {
		t.Fatalf("stopped observation was not recorded: %#v", record)
	}
}

func TestAgentHubPollerWaitingApprovalToBusyDoesNotFinishTurn(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	seedPollerGeneration(t, fake, workspace, generationRecord{
		ID: "gen-sched", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		AgentHubSessionID: "ses_sched", SourceExternalID: workspace.ID + "/run-sched",
		Status:    "waiting_approval",
		CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}, agentHubSession{ID: "ses_sched", State: "running", UpdatedAt: "2026-08-01T00:00:10Z"})

	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	record := pollerGenerationState(manager.runtimeByID("gen-sched"))
	if record.Status != "running" {
		t.Fatalf("waiting_approval to running projection mismatch: %#v", record)
	}
}

func TestAgentHubPollerMissingSessionRetiresLiveGeneration(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	now := "2026-08-01T00:00:01Z"
	for _, record := range []generationRecord{
		{ID: "gen-live", GenerationID: "gen-run-live", WorkspaceID: workspace.ID, ResourceID: "project1", AgentHubSessionID: "ses_gone_live", Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "gen-stopped", GenerationID: "gen-run-stopped", WorkspaceID: workspace.ID, ResourceID: "project1.task1", AgentHubSessionID: "ses_gone_stopped", Status: "stopped", AgentHubStoppedObserved: true, CreatedAt: now, UpdatedAt: now},
		{ID: "gen-recovering", GenerationID: "gen-run-recovering", WorkspaceID: workspace.ID, ResourceID: app.SchedulerResourceID, AgentHubSessionID: "ses_gone_recovering", Status: "recovering", CreatedAt: now, UpdatedAt: now},
	} {
		if err := saveGenerationRecord(workspace.Path, record); err != nil {
			t.Fatal(err)
		}
	}

	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Missing live Sessions are terminal generation failures. With no mailbox
	// demand they retire without eagerly creating empty replacements; the
	// already-stopped generation remains dormant until a message needs it.
	current, err := loadCurrentGenerationRecords(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || current[0].ID != "gen-stopped" {
		t.Fatalf("missing live generations did not retire cleanly: %#v", current)
	}
	for _, recordID := range []string{"gen-live", "gen-recovering"} {
		record, loadErr := loadGenerationRecord(workspace.Path, recordID)
		if loadErr != nil || !record.Retired || record.Status != "stopped" || !strings.Contains(record.RetireReason, "no longer available") {
			t.Fatalf("missing generation %s was not retired: %#v err=%v", recordID, record, loadErr)
		}
	}
	if stopped := manager.runtimeByID("gen-stopped"); stopped != nil {
		if record := pollerGenerationState(stopped); record.Status != "stopped" {
			t.Fatalf("stopped generation was resurrected: %#v", record)
		}
	}
	all, err := loadGenerationRecords(workspace.Path)
	if err != nil || len(all) != 3 {
		t.Fatalf("generation history was not retained: records=%#v err=%v", all, err)
	}
}

func TestAgentHubPollerReadyClearsStoppedObserved(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	seedPollerGeneration(t, fake, workspace, generationRecord{
		ID: "gen-resumed", WorkspaceID: workspace.ID, ResourceID: "workspace", AgentHubSessionID: "ses_resumed",
		SourceExternalID: workspace.ID + "/run-resumed", Status: "stopped",
		AgentHubStoppedObserved: true,
		CreatedAt:               "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}, agentHubSession{ID: "ses_resumed", State: "ready", UpdatedAt: "2026-08-01T00:00:10Z"})

	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	record := pollerGenerationState(manager.runtimeByID("gen-resumed"))
	if record.Status != "idle" || record.AgentHubStoppedObserved {
		t.Fatalf("ready session must clear the stopped observation: %#v", record)
	}
}

func TestAgentHubApplySessionStateStartingClearsStoppedObserved(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	now := time.Now().Format(time.RFC3339)
	record := generationRecord{
		ID: "gen-starting", WorkspaceID: workspace.ID, AgentHubSessionID: "ses_starting",
		SourceExternalID: workspace.ID + "/run-starting", Status: "stopped",
		AgentHubStoppedObserved: true, Cwd: workspace.Path, CreatedAt: now, UpdatedAt: now,
	}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	rt := newAgentHubRuntime(manager, workspace, record, nil)
	manager.registerRuntime(rt)
	rt.applyAgentHubSessionState(manager, agentHubSession{ID: "ses_starting", State: "starting", UpdatedAt: now})
	updated := pollerGenerationState(rt)
	if updated.Status != "starting" || updated.AgentHubStoppedObserved {
		t.Fatalf("starting session must clear the stopped observation: %#v", updated)
	}
}

func TestAgentHubPollerSkipsSaveWhenProjectionUnchanged(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	seedPollerGeneration(t, fake, workspace, generationRecord{
		ID: "gen-idle", WorkspaceID: workspace.ID, AgentHubSessionID: "ses_idle",
		SourceExternalID: workspace.ID + "/run-idle", Status: "idle",
		CompletionSessionID:  "ses_idle",
		GenerationUsageReady: true,
		IdleSinceAt:          "2026-08-01T00:00:10Z",
		IdleDeadlineAt:       "2026-08-01T00:30:10Z",
		CreatedAt:            "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:10Z",
		LastOutputAt: "2026-08-01T00:00:10Z",
	}, agentHubSession{ID: "ses_idle", State: "ready", UpdatedAt: "2026-08-01T00:00:10Z"})
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfig, err := puaWorkspace.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	key, err := generation.ResourceKey(runtimeConfig.InstanceID, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(workspace.Path, ".pua", "runtime", "resources", key, "current.json")
	before := mustReadFile(t, indexPath)

	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := mustReadFile(t, indexPath)
	if string(before) != string(after) {
		t.Fatalf("unchanged projection must not rewrite resource generation files:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
