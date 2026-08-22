package serve

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func TestStallWatchdogStopsAndResumesTheSameSessionOnce(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	manager.now = func() time.Time { return time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC) }
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetStallWatchdogPolicy(app.StallWatchdogPolicy{Enabled: true, TimeoutMinutes: 1}); err != nil {
		t.Fatal(err)
	}
	cfg, client, err := manager.agentHubRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	lastActivity := "2026-08-01T00:55:00Z"
	record := generationRecord{
		ID: "gen-stall-watchdog", WorkspaceID: workspace.ID, ResourceID: "project1",
		Generation: 1, GenerationID: "gen-stall-watchdog", SourceInstanceID: "pua-runtime-test",
		SourceExternalID: workspace.ID + "/stall-watchdog", AgentHubSessionID: "ses_stall_watchdog",
		AgentHubAgentName: "fake-agent", Status: "running", CreatedAt: lastActivity, UpdatedAt: lastActivity,
		CurrentTurnID: "turn-stalled", LastTurnID: "turn-stalled", TurnStartedAt: lastActivity,
	}
	session := agentHubSession{
		ID: record.AgentHubSessionID, AgentName: "fake-agent", State: "running", CurrentTurnID: record.CurrentTurnID,
		Source:         &agentHubSource{App: agentHubSourceApp, InstanceID: "pua-runtime-test", ExternalID: record.SourceExternalID},
		LastActivityAt: lastActivity, LastActivityTurnID: record.CurrentTurnID, UpdatedAt: lastActivity,
	}
	seedPollerGeneration(t, fake, workspace, record, session)
	rt := newAgentHubRuntime(manager, workspace, record, client)
	manager.registerRuntime(rt)

	if err := manager.reconcileStallWatchdogLocked(context.Background(), cfg, workspace, record, rt, session, client); err != nil {
		t.Fatal(err)
	}
	manager.waitBackground()

	fake.mu.Lock()
	actions := append([]string(nil), fake.actions...)
	messageRoles := append([]string(nil), fake.messageRoles...)
	messageIDs := append([]string(nil), fake.messageIDs...)
	stopCalls := fake.stopCalls
	fake.mu.Unlock()
	if stopCalls != 1 || strings.Count(strings.Join(actions, ","), "stop") != 1 || strings.Count(strings.Join(actions, ","), "resume") != 1 {
		t.Fatalf("stall recovery lifecycle actions = %#v, stopCalls=%d", actions, stopCalls)
	}
	if len(messageRoles) != 1 || messageRoles[0] != "system" || len(messageIDs) != 1 || messageIDs[0] == "" {
		t.Fatalf("stall recovery message was not delivered as a system message: roles=%#v ids=%#v", messageRoles, messageIDs)
	}

	stored, found, err := mailboxMessageByID(workspace.Path, messageIDs[0])
	if err != nil || !found {
		t.Fatalf("read stall recovery mailbox message: found=%v err=%v", found, err)
	}
	if stored.Type != resourceMessageTypeTurnStallRecovery || stored.Status != resourceMessageDelivered {
		t.Fatalf("stall recovery mailbox message = %#v", stored)
	}

	// A restart or a repeated poll still sees the durable Stop guard. Simulate
	// the original Turn remaining active and verify no second non-idempotent Stop
	// is sent for the same recovery chain.
	fake.mu.Lock()
	repeated := fake.sessions[record.AgentHubSessionID]
	repeated.State = "running"
	repeated.CurrentTurnID = record.CurrentTurnID
	repeated.LastActivityAt = lastActivity
	repeated.LastActivityTurnID = record.CurrentTurnID
	fake.sessions[repeated.ID] = repeated
	fake.mu.Unlock()
	if err := manager.reconcileStallWatchdogLocked(context.Background(), cfg, workspace, record, rt, repeated, client); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	stopCalls = fake.stopCalls
	fake.mu.Unlock()
	if stopCalls != 1 {
		t.Fatalf("repeated stale observation sent another Stop: %d", stopCalls)
	}
}

func TestStallWatchdogBoundsASecondStallToOneRecoveryAttempt(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	manager.now = func() time.Time { return time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC) }
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetStallWatchdogPolicy(app.StallWatchdogPolicy{Enabled: true, TimeoutMinutes: 1}); err != nil {
		t.Fatal(err)
	}
	cfg, client, err := manager.agentHubRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	lastActivity := "2026-08-01T00:55:00Z"
	record := generationRecord{
		ID: "gen-stall-bound", WorkspaceID: workspace.ID, ResourceID: app.SchedulerResourceID,
		Generation: 1, GenerationID: "gen-stall-bound", SourceInstanceID: "pua-runtime-test",
		SourceExternalID: workspace.ID + "/stall-bound", AgentHubSessionID: "ses_stall_bound",
		AgentHubAgentName: "fake-agent", Status: "running", CreatedAt: lastActivity, UpdatedAt: lastActivity,
		CurrentTurnID: "turn-recovery", LastTurnID: "turn-recovery", TurnStartedAt: lastActivity,
		StallWatchdog: &stallWatchdogState{
			GenerationID: "gen-stall-bound", SessionID: "ses_stall_bound", TurnID: "turn-original",
			RecoveryTurnID: "turn-recovery", RecoveryMessageID: "msg-recovery", DetectedAt: lastActivity,
			Attempt: 1, StopRequested: true,
		},
	}
	session := agentHubSession{
		ID: record.AgentHubSessionID, AgentName: "fake-agent", State: "running", CurrentTurnID: record.CurrentTurnID,
		Source:         &agentHubSource{App: agentHubSourceApp, InstanceID: "pua-runtime-test", ExternalID: record.SourceExternalID},
		LastActivityAt: lastActivity, LastActivityTurnID: record.CurrentTurnID, UpdatedAt: lastActivity,
	}
	seedPollerGeneration(t, fake, workspace, record, session)
	rt := newAgentHubRuntime(manager, workspace, record, client)
	manager.registerRuntime(rt)

	if err := manager.reconcileStallWatchdogLocked(context.Background(), cfg, workspace, record, rt, session, client); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	stopCalls := fake.stopCalls
	fake.mu.Unlock()
	if stopCalls != 0 {
		t.Fatalf("second stalled recovery attempted another Stop: %d", stopCalls)
	}
	state := rt.snapshotGeneration().StallWatchdog
	if state == nil || !state.RecoveryExhausted {
		t.Fatalf("second stalled recovery was not durably bounded: %#v", state)
	}
}
