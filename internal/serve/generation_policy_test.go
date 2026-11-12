package serve

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func TestGenerationUsageCountsCanonicalTerminalTurnsAndDuration(t *testing.T) {
	turns := []agentHubTurn{
		{TurnID: "turn-1", Status: "completed", Closed: true, DurationMS: 30_000},
		{TurnID: "turn-1", Status: "completed", Closed: true, DurationMS: 30_000},
		{TurnID: "turn-2", Status: "failed", Closed: true, StartedAt: "2026-08-01T00:00:00Z", EndedAt: "2026-08-01T00:02:00Z"},
		{TurnID: "turn-3", Status: "cancelled", Closed: true, DurationMS: 10_000},
		{TurnID: "turn-active", Status: "running", Closed: false, DurationMS: 99_000},
		{TurnID: "turn-unknown", Status: "interrupted", Closed: true, DurationMS: 99_000},
	}
	usage := generationUsageFromTurns(turns)
	if usage.CompletedTurns != 3 || usage.TurnDurationMS != 160_000 {
		t.Fatalf("generation usage = %#v", usage)
	}
}

func TestGenerationPolicyUsesIndependentOrBudgets(t *testing.T) {
	policy := app.GenerationPolicy{BudgetEnabled: true, MaxTurns: 20, MaxAccumulatedTurnMinutes: 120, MaxInactivityMinutes: 1440}
	tests := []struct {
		name  string
		usage generationUsage
		want  bool
	}{
		{name: "below both", usage: generationUsage{CompletedTurns: 19, TurnDurationMS: int64(119 * time.Minute / time.Millisecond)}},
		{name: "turn budget", usage: generationUsage{CompletedTurns: 20}, want: true},
		{name: "time budget", usage: generationUsage{CompletedTurns: 1, TurnDurationMS: int64(120 * time.Minute / time.Millisecond)}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := generationBudgetReached(policy, test.usage); got != test.want {
				t.Fatalf("generationBudgetReached() = %v, want %v", got, test.want)
			}
		})
	}
	policy.BudgetEnabled = false
	if generationBudgetReached(policy, generationUsage{CompletedTurns: 100, TurnDurationMS: int64(300 * time.Minute / time.Millisecond)}) {
		t.Fatal("disabled policy reached a budget")
	}
}

func TestGenerationInactivityUsesLastSemanticActivity(t *testing.T) {
	now := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	policy := app.GenerationPolicy{InactivityEnabled: true, MaxInactivityMinutes: 1440}
	tests := []struct {
		name       string
		activityAt string
		enabled    bool
		want       bool
	}{
		{name: "below threshold", activityAt: now.Add(-24*time.Hour + time.Nanosecond).Format(time.RFC3339Nano), enabled: true},
		{name: "at threshold", activityAt: now.Add(-24 * time.Hour).Format(time.RFC3339Nano), enabled: true, want: true},
		{name: "above threshold", activityAt: now.Add(-48 * time.Hour).Format(time.RFC3339Nano), enabled: true, want: true},
		{name: "missing activity", enabled: true},
		{name: "future activity", activityAt: now.Add(time.Minute).Format(time.RFC3339Nano), enabled: true},
		{name: "disabled", activityAt: now.Add(-48 * time.Hour).Format(time.RFC3339Nano)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy.InactivityEnabled = test.enabled
			session := agentHubSession{LastActivityAt: test.activityAt}
			if got := generationInactivityReached(policy, session, now); got != test.want {
				t.Fatalf("generationInactivityReached() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestGenerationPolicyDoesNotRotateIdleGenerationWithoutNewTurn(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetGenerationPolicy(app.GenerationPolicy{BudgetEnabled: true, MaxTurns: 2, MaxAccumulatedTurnMinutes: 120, InactivityEnabled: true, MaxInactivityMinutes: 1440}); err != nil {
		t.Fatal(err)
	}
	record := generationRecord{
		ID: "gen-policy", WorkspaceID: workspace.ID, ResourceID: "project1.task1", Generation: 1,
		GenerationID: "gen-policy", AgentHubSessionID: "ses-policy",
		SourceExternalID: workspace.ID + "/gen-policy", Status: "idle",
		CreatedAt: "2026-08-01T00:00:00Z", UpdatedAt: "2026-08-01T00:02:00Z",
	}
	session := agentHubSession{
		ID: "ses-policy", State: "ready", LastEventID: 4,
		LastActivityAt: "2026-07-30T00:00:00Z",
		CreatedAt:      "2026-08-01T00:00:00Z", UpdatedAt: "2026-08-01T00:02:00Z",
	}
	seedPollerGeneration(t, fake, workspace, record, session)
	fake.mu.Lock()
	fake.turns[session.ID] = map[string]agentHubTurn{
		"turn-1": {ID: "turn-1", TurnID: "turn-1", Status: "completed", Closed: true, DurationMS: 60_000, FirstEventID: 1, LastEventID: 2},
		"turn-2": {ID: "turn-2", TurnID: "turn-2", Status: "failed", Closed: true, DurationMS: 60_000, FirstEventID: 3, LastEventID: 4},
	}
	fake.mu.Unlock()

	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, found, err := currentResourceGeneration(workspace.Path, record.ResourceID)
	if err != nil || !found || current.GenerationID != record.GenerationID || current.ReplacementPending || current.Retired {
		t.Fatalf("idle generation rotated without input: %#v, found=%v err=%v", current, found, err)
	}
	fake.mu.Lock()
	stopCalls := fake.stopCalls
	fake.mu.Unlock()
	if stopCalls != 0 {
		t.Fatalf("idle generation received %d Stop requests without new input", stopCalls)
	}
}

func TestGenerationPolicyRotatesReadyGenerationAtNewTurnBoundary(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	fake.extraAgents = []string{"replacement-agent"}
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, configPath := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetGenerationPolicy(app.GenerationPolicy{BudgetEnabled: true, MaxTurns: 2, MaxAccumulatedTurnMinutes: 120, InactivityEnabled: true, MaxInactivityMinutes: 1440}); err != nil {
		t.Fatal(err)
	}
	setRuntimeTestProfiles(t, configPath, []agentHubProfileRoute{{Key: "default", AgentName: "replacement-agent"}})
	record := generationRecord{
		ID: "gen-policy-ready", WorkspaceID: workspace.ID, ResourceID: "project1.task1", Generation: 1,
		GenerationID: "gen-policy-ready", AgentHubSessionID: "ses-policy-ready",
		AgentHubAgentName: "fake-agent", SourceExternalID: workspace.ID + "/gen-policy-ready", Status: "idle",
		CreatedAt: "2026-08-01T00:00:00Z", UpdatedAt: "2026-08-01T00:02:00Z",
	}
	session := agentHubSession{
		ID: "ses-policy-ready", State: "ready", LastEventID: 4,
		LastActivityAt: "2026-07-30T00:00:00Z",
		CreatedAt:      "2026-08-01T00:00:00Z", UpdatedAt: "2026-08-01T00:02:00Z",
	}
	seedPollerGeneration(t, fake, workspace, record, session)
	fake.mu.Lock()
	fake.turns[session.ID] = map[string]agentHubTurn{
		"turn-1": {ID: "turn-1", TurnID: "turn-1", Status: "completed", Closed: true, DurationMS: 60_000, FirstEventID: 1, LastEventID: 2},
		"turn-2": {ID: "turn-2", TurnID: "turn-2", Status: "failed", Closed: true, DurationMS: 60_000, FirstEventID: 3, LastEventID: 4},
	}
	fake.mu.Unlock()

	message, err := manager.acceptResourceMessage(context.Background(), workspace, record.ResourceID, resourceMessageRequest{
		Text: "next Turn", Mode: resourceMessageModeEnqueue,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		current, currentFound, loadErr := currentResourceGeneration(workspace.Path, record.ResourceID)
		updated, messageFound, messageErr := mailboxMessageByID(workspace.Path, message.ID)
		return loadErr == nil && currentFound && current.Generation == 2 && current.AgentHubAgentName == "replacement-agent" &&
			messageErr == nil && messageFound && updated.Status == resourceMessageDelivered &&
			updated.GenerationID == current.GenerationID
	})
	retired, found, err := generationRecordByID(workspace.Path, record.GenerationID)
	if err != nil || !found || !retired.Retired || retired.RetireReason != generationBudgetRetireReason ||
		retired.GenerationCompletedTurns != 2 || retired.GenerationTurnDurationMS != 120_000 {
		t.Fatalf("retired ready generation = %#v, found=%v err=%v", retired, found, err)
	}
	fake.mu.Lock()
	archived := fake.sessions[session.ID]
	stopCalls := fake.stopCalls
	fake.mu.Unlock()
	if archived.State != "archived" || stopCalls != 1 {
		t.Fatalf("ready policy rotation lifecycle: state=%q stopCalls=%d", archived.State, stopCalls)
	}
}

func TestGenerationPolicyReusesGenerationBelowBudget(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetGenerationPolicy(app.GenerationPolicy{BudgetEnabled: true, MaxTurns: 2, MaxInactivityMinutes: 1440}); err != nil {
		t.Fatal(err)
	}
	record := generationRecord{
		ID: "gen-policy-below", WorkspaceID: workspace.ID, ResourceID: "project1.task1", Generation: 1,
		GenerationID: "gen-policy-below", AgentHubSessionID: "ses-policy-below", AgentHubAgentName: "fake-agent",
		SourceExternalID: workspace.ID + "/gen-policy-below", Status: "idle",
		GenerationUsageReady: true, GenerationCompletedTurns: 1,
		CreatedAt: "2026-08-01T00:00:00Z", UpdatedAt: "2026-08-01T00:02:00Z",
	}
	session := agentHubSession{
		ID: "ses-policy-below", State: "ready",
		CreatedAt: "2026-08-01T00:00:00Z", UpdatedAt: "2026-08-01T00:02:00Z",
	}
	seedPollerGeneration(t, fake, workspace, record, session)

	message, err := manager.acceptResourceMessage(context.Background(), workspace, record.ResourceID, resourceMessageRequest{
		Text: "reuse current generation", Mode: resourceMessageModeEnqueue,
	})
	if err != nil {
		t.Fatal(err)
	}
	current, found, err := currentResourceGeneration(workspace.Path, record.ResourceID)
	updated, messageFound, messageErr := mailboxMessageByID(workspace.Path, message.ID)
	if err != nil || !found || current.GenerationID != record.GenerationID || current.ReplacementPending ||
		messageErr != nil || !messageFound || updated.Status != resourceMessageDelivered || updated.GenerationID != record.GenerationID {
		t.Fatalf("below-budget Turn did not reuse generation: generation=%#v message=%#v errors=%v/%v", current, updated, err, messageErr)
	}
}

func TestGenerationPolicyRotatesStoppedGenerationBeforeResume(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetGenerationPolicy(app.GenerationPolicy{BudgetEnabled: true, MaxTurns: 2, MaxAccumulatedTurnMinutes: 120, MaxInactivityMinutes: 1440}); err != nil {
		t.Fatal(err)
	}
	record := generationRecord{
		ID: "gen-policy-stopped", WorkspaceID: workspace.ID, ResourceID: "project1.task1", Generation: 1,
		GenerationID: "gen-policy-stopped", AgentHubSessionID: "ses-policy-stopped", AgentHubAgentName: "fake-agent",
		SourceExternalID: workspace.ID + "/gen-policy-stopped", Status: "idle-suspended", IdleSleepStopRequested: true,
		GenerationUsageReady: true, GenerationCompletedTurns: 1,
		GenerationTurnDurationMS: int64(120 * time.Minute / time.Millisecond),
		CreatedAt:                "2026-08-01T00:00:00Z", UpdatedAt: "2026-08-01T00:02:00Z",
	}
	session := agentHubSession{
		ID: "ses-policy-stopped", State: "stopped",
		CreatedAt: "2026-08-01T00:00:00Z", UpdatedAt: "2026-08-01T00:02:00Z",
	}
	seedPollerGeneration(t, fake, workspace, record, session)

	message, err := manager.acceptResourceMessage(context.Background(), workspace, record.ResourceID, resourceMessageRequest{
		Text: "wake with a fresh generation", Mode: resourceMessageModeEnqueue,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		current, found, loadErr := currentResourceGeneration(workspace.Path, record.ResourceID)
		updated, messageFound, messageErr := mailboxMessageByID(workspace.Path, message.ID)
		return loadErr == nil && found && current.Generation == 2 && messageErr == nil && messageFound &&
			updated.Status == resourceMessageDelivered && updated.GenerationID == current.GenerationID
	})
	fake.mu.Lock()
	stopCalls := fake.stopCalls
	resumeCount := len(fake.resumeEnvironments)
	archived := fake.sessions[session.ID]
	fake.mu.Unlock()
	if archived.State != "archived" || stopCalls != 0 || resumeCount != 0 {
		t.Fatalf("stopped policy rotation used the wrong lifecycle: state=%q stopCalls=%d resumes=%d", archived.State, stopCalls, resumeCount)
	}
}

func TestGenerationInactivityRotatesBeforeNewTurnForReadyAndStoppedSessions(t *testing.T) {
	for _, state := range []string{"ready", "stopped"} {
		t.Run(state, func(t *testing.T) {
			fake := newRuntimeFakeAgentHub()
			hub := httptest.NewServer(fake)
			defer hub.Close()
			manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
			puaWorkspace, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := puaWorkspace.SetGenerationPolicy(app.GenerationPolicy{InactivityEnabled: true, MaxInactivityMinutes: 1440}); err != nil {
				t.Fatal(err)
			}
			status := "idle"
			idleSleepRequested := false
			if state == "stopped" {
				status = "idle-suspended"
				idleSleepRequested = true
			}
			record := generationRecord{
				ID: "gen-inactivity-" + state, WorkspaceID: workspace.ID, ResourceID: "project1.task1", Generation: 1,
				GenerationID: "gen-inactivity-" + state, AgentHubSessionID: "ses-inactivity-" + state, AgentHubAgentName: "fake-agent",
				SourceExternalID: workspace.ID + "/gen-inactivity-" + state, Status: status, IdleSleepStopRequested: idleSleepRequested,
				CreatedAt: "2026-07-30T00:00:00Z", UpdatedAt: "2026-08-01T00:00:00Z",
			}
			session := agentHubSession{
				ID: "ses-inactivity-" + state, State: state, AgentName: "fake-agent",
				LastActivityAt: "2026-07-30T00:00:00Z",
				CreatedAt:      "2026-07-30T00:00:00Z", UpdatedAt: "2026-08-01T00:00:00Z",
			}
			seedPollerGeneration(t, fake, workspace, record, session)

			message, err := manager.acceptResourceMessage(context.Background(), workspace, record.ResourceID, resourceMessageRequest{
				Text: "start after inactivity", Mode: resourceMessageModeEnqueue,
			})
			if err != nil {
				t.Fatal(err)
			}
			waitForRuntimeTest(t, func() bool {
				current, found, loadErr := currentResourceGeneration(workspace.Path, record.ResourceID)
				updated, messageFound, messageErr := mailboxMessageByID(workspace.Path, message.ID)
				return loadErr == nil && found && current.Generation == 2 && messageErr == nil && messageFound &&
					updated.Status == resourceMessageDelivered && updated.GenerationID == current.GenerationID
			})
			retired, found, err := generationRecordByID(workspace.Path, record.GenerationID)
			if err != nil || !found || !retired.Retired || retired.RetireReason != generationInactivityRetireReason {
				t.Fatalf("inactivity retirement = %#v, found=%v err=%v", retired, found, err)
			}
			fake.mu.Lock()
			resumeCount := len(fake.resumeEnvironments)
			stopCalls := fake.stopCalls
			archived := fake.sessions[session.ID]
			fake.mu.Unlock()
			wantStops := 1
			if state == "stopped" {
				wantStops = 0
			}
			if resumeCount != 0 || stopCalls != wantStops || archived.State != "archived" {
				t.Fatalf("inactivity lifecycle: resumes=%d stops=%d wantStops=%d state=%q", resumeCount, stopCalls, wantStops, archived.State)
			}
		})
	}
}

func TestGenerationPolicyKeepsActiveTurnSteerOnCurrentGeneration(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetGenerationPolicy(app.GenerationPolicy{BudgetEnabled: true, MaxTurns: 1, InactivityEnabled: true, MaxInactivityMinutes: 1440}); err != nil {
		t.Fatal(err)
	}
	record := generationRecord{
		ID: "gen-policy-active", WorkspaceID: workspace.ID, ResourceID: "project1.task1", Generation: 1,
		GenerationID: "gen-policy-active", AgentHubSessionID: "ses-policy-active", AgentHubAgentName: "fake-agent",
		SourceExternalID: workspace.ID + "/gen-policy-active", Status: "running",
		GenerationUsageReady: true, GenerationCompletedTurns: 1,
		CreatedAt: "2026-08-01T00:00:00Z", UpdatedAt: "2026-08-01T00:02:00Z",
	}
	session := agentHubSession{
		ID: "ses-policy-active", State: "running", CurrentTurnID: "turn-active",
		InputCapabilities: agentHubInputCapabilities{Steer: true},
		LastActivityAt:    "2026-07-30T00:00:00Z",
		CreatedAt:         "2026-08-01T00:00:00Z", UpdatedAt: "2026-08-01T00:02:00Z",
	}
	seedPollerGeneration(t, fake, workspace, record, session)

	message, err := manager.acceptResourceMessage(context.Background(), workspace, record.ResourceID, resourceMessageRequest{
		Text: "steer current Turn", Mode: resourceMessageModeSteer,
	})
	if err != nil {
		t.Fatal(err)
	}
	current, found, err := currentResourceGeneration(workspace.Path, record.ResourceID)
	updated, messageFound, messageErr := mailboxMessageByID(workspace.Path, message.ID)
	if err != nil || !found || current.GenerationID != record.GenerationID || current.ReplacementPending ||
		messageErr != nil || !messageFound || updated.Status != resourceMessageDelivered || updated.ActualMode != resourceMessageModeSteer {
		t.Fatalf("active steer crossed policy boundary: generation=%#v message=%#v errors=%v/%v", current, updated, err, messageErr)
	}
	fake.mu.Lock()
	stopCalls := fake.stopCalls
	fake.mu.Unlock()
	if stopCalls != 0 {
		t.Fatalf("active steer caused %d Stop requests", stopCalls)
	}
}

func TestGenerationPolicyCompletesFailedTurnBeforeContinuationRotation(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetGenerationPolicy(app.GenerationPolicy{BudgetEnabled: true, MaxAccumulatedTurnMinutes: 1, MaxInactivityMinutes: 1440}); err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetTaskState("project1.task1", app.TaskStateInProgress, ""); err != nil {
		t.Fatal(err)
	}
	record := generationRecord{
		ID: "gen-policy-completion", WorkspaceID: workspace.ID, ResourceID: "project1.task1", Generation: 1,
		GenerationID: "gen-policy-completion", AgentHubSessionID: "ses-policy-completion", AgentHubAgentName: "fake-agent",
		SourceExternalID: workspace.ID + "/gen-policy-completion", Status: "running",
		CompletionSessionID: "ses-policy-completion", CompletionCursor: 1, TaskStateChainID: "message-original",
		CreatedAt: "2026-08-01T00:00:00Z", UpdatedAt: "2026-08-01T00:02:00Z",
	}
	session := agentHubSession{
		ID: "ses-policy-completion", State: "running", CurrentTurnID: "turn-failed",
		CreatedAt: "2026-08-01T00:00:00Z", UpdatedAt: "2026-08-01T00:02:00Z",
	}
	seedPollerGeneration(t, fake, workspace, record, session)
	fake.mu.Lock()
	fake.appendLocked(session.ID, "session.created", map[string]any{"id": session.ID})
	fake.appendLocked(session.ID, "turn.started", map[string]any{"turnId": "turn-failed"})
	fake.appendLocked(session.ID, "turn.failed", map[string]any{"turnId": "turn-failed"})
	session = fake.sessions[session.ID]
	session.State = "ready"
	session.CurrentTurnID = ""
	fake.sessions[session.ID] = session
	fake.turns[session.ID] = map[string]agentHubTurn{
		"turn-failed": {
			ID: "turn-failed", TurnID: "turn-failed", Status: "failed", Closed: true,
			DurationMS: 2 * 60_000, FirstEventID: 2, LastEventID: 3,
		},
	}
	fake.mu.Unlock()

	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	marker := session.ID + ":3"
	continuationID := taskStateContinuationMessageID(record.ResourceID, record.GenerationID, record.TaskStateChainID, marker, 1)
	waitForRuntimeTest(t, func() bool {
		current, currentFound, loadErr := currentResourceGeneration(workspace.Path, record.ResourceID)
		continuation, messageFound, messageErr := mailboxMessageByID(workspace.Path, continuationID)
		return loadErr == nil && currentFound && current.Generation == 2 && messageErr == nil && messageFound &&
			continuation.Status == resourceMessageDelivered && continuation.GenerationID == current.GenerationID
	})
	retired, found, err := generationRecordByID(workspace.Path, record.GenerationID)
	if err != nil || !found || !retired.Retired || retired.RetireReason != generationBudgetRetireReason ||
		retired.CompletionMarker != marker || retired.CompletionPending ||
		retired.TaskStateCompletionMarker != marker || retired.TaskStateContinuationCount != 1 {
		t.Fatalf("failed Turn completion was not finalized before rotation: generation=%#v found=%v err=%v", retired, found, err)
	}
	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	messageIDs := append([]string(nil), fake.messageIDs...)
	fake.mu.Unlock()
	if len(messageIDs) != 1 || messageIDs[0] != continuationID {
		t.Fatalf("continuation delivery count/identity = %#v, want [%q]", messageIDs, continuationID)
	}
}

func TestGenerationInactivityReplacementIntentSurvivesRestart(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	record := generationRecord{
		ID: "gen-policy-restart", WorkspaceID: workspace.ID, ResourceID: "project1.task1", Generation: 1,
		GenerationID: "gen-policy-restart", AgentHubSessionID: "ses-policy-restart", AgentHubAgentName: "fake-agent",
		SourceExternalID: workspace.ID + "/gen-policy-restart", Status: "idle",
		ReplacementPending: true, RetireReason: generationInactivityRetireReason,
		GenerationUsageReady: true, GenerationCompletedTurns: 2,
		CreatedAt: "2026-08-01T00:00:00Z", UpdatedAt: "2026-08-01T00:02:00Z",
	}
	seedPollerGeneration(t, fake, workspace, record, agentHubSession{
		ID: "ses-policy-restart", State: "ready", AgentName: "fake-agent",
		CreatedAt: "2026-08-01T00:00:00Z", UpdatedAt: "2026-08-01T00:02:00Z",
	})
	message, err := acceptMailboxMessage(workspace.Path, record.ResourceID, resourceMessageRequest{
		Text: "deliver after restart", Mode: resourceMessageModeEnqueue,
	})
	if err != nil {
		t.Fatal(err)
	}

	restarted := newAgentManager(manager.server)
	manager.server.agents = restarted
	if err := restarted.recoverAgentHubGenerations(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		current, found, loadErr := currentResourceGeneration(workspace.Path, record.ResourceID)
		updated, messageFound, messageErr := mailboxMessageByID(workspace.Path, message.ID)
		return loadErr == nil && found && current.Generation == 2 && messageErr == nil && messageFound &&
			updated.Status == resourceMessageDelivered && updated.GenerationID == current.GenerationID
	})
	retired, found, err := generationRecordByID(workspace.Path, record.GenerationID)
	if err != nil || !found || !retired.Retired || retired.RetireReason != generationInactivityRetireReason {
		t.Fatalf("restart lost policy replacement intent: generation=%#v found=%v err=%v", retired, found, err)
	}
	fake.mu.Lock()
	messageIDs := append([]string(nil), fake.messageIDs...)
	fake.mu.Unlock()
	if len(messageIDs) != 1 || messageIDs[0] != message.ID {
		t.Fatalf("restart delivery count/identity = %#v, want [%q]", messageIDs, message.ID)
	}
}
