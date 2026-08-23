package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func runtimeFakeAgentHubWithCapabilities(fake *runtimeFakeAgentHub, capabilities []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/status" && r.Method == http.MethodGet {
			writeRuntimeFakeJSON(w, agentHubStatus{APIVersion: agentHubAPIVersion, Capabilities: capabilities})
			return
		}
		fake.ServeHTTP(w, r)
	})
}

func writeRuntimeServiceBindings(t *testing.T, workspace serveWorkspace, secret string) {
	t.Helper()
	t.Setenv("PUA_SECRET_RECOVERY_TOKEN", secret)
	if err := writeServiceJSON(serviceBindingsPath(workspace.Path), ServiceBindings{
		SchemaVersion: serviceSchemaVersion,
		Variables:     map[string]string{"PUBLIC_ENDPOINT": "http://service.test"},
		Secrets:       map[string]string{"SERVICE_TOKEN": "${secret.recovery-token}"},
	}, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAgentHubRecoveryReusesBoundCreateEnvironment(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	capabilities := append(append([]string(nil), requiredAgentHubCapabilities...), "session.ephemeral-environment")
	hub := httptest.NewServer(runtimeFakeAgentHubWithCapabilities(fake, capabilities))
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	const secret = "recovery-overlay-secret"
	writeRuntimeServiceBindings(t, workspace, secret)
	record := generationRecord{
		ID: "gen-overlay-recovery", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-overlay-recovery", AgentHubAgentName: "fake-agent",
		SourceInstanceID: "pua-runtime-test", SourceExternalID: "project1.task1/1",
		BindingKind: "profile", BindingName: "default", ResolvedProfile: "default",
		Title: "Recovered overlay", Cwd: workspace.Path, Status: "recovering",
		CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}
	if err := rewriteTestGenerationRecords(workspace.Path, []generationRecord{record}); err != nil {
		t.Fatal(err)
	}

	if err := manager.recoverAgentHubGenerations(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	requests := append([]agentHubCreateSessionRequest(nil), fake.createRequests...)
	fake.mu.Unlock()
	if len(requests) != 1 {
		t.Fatalf("recovery create requests = %d, want 1", len(requests))
	}
	request := requests[0]
	if request.LaunchEnvironment["PUA_WORKSPACE_ROOT"] != workspace.Path ||
		request.LaunchEnvironment["PUA_WORKSPACE_INSTANCE_ID"] != record.SourceInstanceID ||
		request.LaunchEnvironment["PUA_RESOURCE_ID"] != record.ResourceID ||
		request.LaunchEnvironment["PUBLIC_ENDPOINT"] != "http://service.test" {
		t.Fatalf("recovery launch environment = %#v", request.LaunchEnvironment)
	}
	if request.EphemeralEnvironment["SERVICE_TOKEN"] != secret {
		t.Fatalf("recovery ephemeral environment was not restored: %#v", request.EphemeralEnvironment)
	}
	data, err := json.Marshal(request.LaunchEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("secret entered durable launch environment: %s", data)
	}
}

func TestAgentHubRecoveryDoesNotBypassEphemeralCapability(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(runtimeFakeAgentHubWithCapabilities(fake, append([]string(nil), requiredAgentHubCapabilities...)))
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	writeRuntimeServiceBindings(t, workspace, "older-agenthub-secret")
	cfg, client, err := manager.agentHubRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := manager.resolveResourceAgent(workspace, "project1.task1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	record, createErr := manager.createResourceGeneration(context.Background(), workspace, "project1.task1", workspace.Path, cfg, client, resolved)
	if createErr == nil || !strings.Contains(createErr.Error(), "does not support ephemeral service secrets") {
		t.Fatalf("initial create error = %v", createErr)
	}
	if record.GenerationID == "" || record.Status != "recovering" {
		t.Fatalf("failed initial create did not retain a recoverable generation: %#v", record)
	}

	restarted := newAgentManager(manager.server)
	manager.server.agents = restarted
	recoveryErr := restarted.recoverAgentHubGenerations(context.Background())
	if recoveryErr == nil || !strings.Contains(recoveryErr.Error(), "does not support ephemeral service secrets") {
		t.Fatalf("recovery capability error = %v", recoveryErr)
	}
	fake.mu.Lock()
	createCount := len(fake.createRequests)
	fake.mu.Unlock()
	if createCount != 0 {
		t.Fatalf("older AgentHub received %d create POSTs despite required secret overlay", createCount)
	}
}

func TestAgentHubRecoveryProjectsSessionsWithoutEventsOrStreams(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	seedPollerGeneration(t, fake, workspace, generationRecord{
		ID: "gen-live", WorkspaceID: workspace.ID, ResourceID: "project1", AgentHubSessionID: "ses_live",
		SourceExternalID: workspace.ID + "/run-live",
		Status:           "running", CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}, agentHubSession{ID: "ses_live", State: "ready", UpdatedAt: "2026-08-01T00:00:10Z"})
	seedPollerGeneration(t, fake, workspace, generationRecord{
		ID: "gen-stopped", WorkspaceID: workspace.ID, ResourceID: "project1.task1", AgentHubSessionID: "ses_stopped",
		SourceExternalID: workspace.ID + "/run-stopped", Status: "stopped",
		AgentHubStoppedObserved: true,
		CreatedAt:               "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}, agentHubSession{ID: "ses_stopped", State: "stopped", UpdatedAt: "2026-08-01T00:00:11Z"})

	if err := manager.recoverAgentHubGenerations(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	listCalls, eventsCalls, streamCalls := fake.listCalls, fake.eventsCalls, fake.streamCalls
	fake.mu.Unlock()
	if listCalls != 1 {
		t.Fatalf("recovery must issue exactly one session list, got %d", listCalls)
	}
	if eventsCalls != 0 || streamCalls != 0 {
		t.Fatalf("recovery must not read event history or open streams: events=%d streams=%d", eventsCalls, streamCalls)
	}
	fake.mu.Lock()
	for _, action := range fake.actions {
		if action == "resume" {
			fake.mu.Unlock()
			t.Fatal("stopped Session without mailbox demand was resumed during startup")
		}
	}
	fake.mu.Unlock()
	live := manager.runtimeByID("gen-live")
	if live == nil {
		t.Fatal("live run was not recovered")
	}
	waitForRuntimeTest(t, func() bool {
		record := pollerGenerationState(live)
		return record.CompletionSessionID == "ses_live" && !record.CompletionPending
	})
	live.mu.Lock()
	liveGeneration, liveState := live.record, live.agentHubState
	live.mu.Unlock()
	if liveGeneration.Status != "idle" || liveState != "ready" {
		t.Fatalf("live run projection mismatch: %#v state=%q", liveGeneration, liveState)
	}
	stopped := manager.runtimeByID("gen-stopped")
	if stopped == nil {
		t.Fatal("stopped run was not recovered")
	}
	stoppedGeneration := pollerGenerationState(stopped)
	if stoppedGeneration.Status != "stopped" || !stoppedGeneration.AgentHubStoppedObserved {
		t.Fatalf("stopped run projection mismatch: %#v", stoppedGeneration)
	}
	if response := closeRuntimeTestGeneration(t, manager, workspace, "gen-live"); response.Code != http.StatusOK {
		t.Fatalf("test cleanup close failed: %d %s", response.Code, response.Body.String())
	}
}

func TestAgentHubRecoverySingleListForManyStoppedGenerations(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	now := "2026-08-01T00:00:01Z"
	const stoppedGenerations = 8
	records := make([]generationRecord, 0, stoppedGenerations)
	fake.mu.Lock()
	for index := 0; index < stoppedGenerations; index++ {
		id := fmt.Sprintf("gen-%03d", index)
		sessionID := "ses_" + id
		records = append(records, generationRecord{
			ID: id, WorkspaceID: workspace.ID, ResourceID: fmt.Sprintf("project1.task%d", index+1), AgentHubSessionID: sessionID,
			SourceExternalID: workspace.ID + "/" + id, Status: "stopped",
			AgentHubStoppedObserved: true, CreatedAt: now, UpdatedAt: now,
		})
		fake.sessions[sessionID] = agentHubSession{
			ID: sessionID, State: "stopped", UpdatedAt: now,
			Source: &agentHubSource{App: agentHubSourceApp, InstanceID: "pua-runtime-test", ExternalID: workspace.ID + "/" + id},
		}
	}
	fake.mu.Unlock()
	if err := rewriteTestGenerationRecords(workspace.Path, records); err != nil {
		t.Fatal(err)
	}

	if err := manager.recoverAgentHubGenerations(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	listCalls, eventsCalls, streamCalls := fake.listCalls, fake.eventsCalls, fake.streamCalls
	fake.mu.Unlock()
	if listCalls != 1 {
		t.Fatalf("%d stopped runs must recover with exactly one session list, got %d", stoppedGenerations, listCalls)
	}
	if eventsCalls != 0 || streamCalls != 0 {
		t.Fatalf("stopped runs must not read events or open streams: events=%d streams=%d", eventsCalls, streamCalls)
	}
	if rt := manager.runtimeByID("gen-007"); rt == nil {
		t.Fatal("stopped runs were not registered as lightweight runtimes")
	}
}

func TestAgentHubRecoveryReplacesBoundSessionWithIncompatibleSource(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	record := generationRecord{
		ID: "gen-old-source", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-old-source", AgentHubSessionID: "ses-old-source",
		AgentHubAgentName: "fake-agent", SourceInstanceID: "pua-runtime-test", SourceExternalID: "project1.task1/1",
		BindingKind: "profile", BindingName: "default", ResolvedProfile: "default",
		Status: "recovering", ReplacementPending: true, IdleSleepStopRequested: true,
		CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}
	seedPollerGeneration(t, fake, workspace, record, agentHubSession{
		ID: record.AgentHubSessionID, State: "stopped", UpdatedAt: "2026-08-01T00:00:10Z",
		Source: &agentHubSource{App: "forge", InstanceID: "pua-runtime-test", ExternalID: record.SourceExternalID},
	})
	message, err := acceptMailboxMessage(workspace.Path, record.ResourceID, resourceMessageRequest{Text: "replace old source", Mode: resourceMessageModeEnqueue})
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.recoverAgentHubGenerations(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		current, found, loadErr := currentResourceGeneration(workspace.Path, record.ResourceID)
		if loadErr != nil || !found || current.Generation <= record.Generation || current.AgentHubSessionID == record.AgentHubSessionID {
			return false
		}
		updated, messageFound, messageErr := mailboxMessageByID(workspace.Path, message.ID)
		return messageErr == nil && messageFound && updated.Status == resourceMessageDelivered && updated.GenerationID == current.GenerationID
	})
	fake.mu.Lock()
	oldSession := fake.sessions[record.AgentHubSessionID]
	var replacement agentHubSession
	for id, session := range fake.sessions {
		if id != record.AgentHubSessionID && session.Source != nil && session.Source.Metadata["resourceId"] == record.ResourceID {
			replacement = session
			break
		}
	}
	fake.mu.Unlock()
	if oldSession.State != "archived" {
		t.Fatalf("old incompatible Session was not archived: %#v", oldSession)
	}
	if replacement.ID == "" || replacement.Source == nil || replacement.Source.App != agentHubSourceApp || replacement.State != "running" {
		t.Fatalf("replacement Session mismatch: %#v", replacement)
	}
}

func TestAgentHubRecoveryRotatesGenerationAfterCreateIdempotencyConflict(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	record := generationRecord{
		ID: "gen-create-conflict", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-create-conflict", AgentHubAgentName: "fake-agent",
		SourceInstanceID: "pua-runtime-test", SourceExternalID: "project1.task1/1",
		BindingKind: "profile", BindingName: "default", ResolvedProfile: "default",
		Status: "recovering", CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}
	if err := rewriteTestGenerationRecords(workspace.Path, []generationRecord{record}); err != nil {
		t.Fatal(err)
	}
	message, err := acceptMailboxMessage(workspace.Path, record.ResourceID, resourceMessageRequest{Text: "replace conflicting create", Mode: resourceMessageModeEnqueue})
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.rejectIdempotencyKey = record.GenerationID
	fake.mu.Unlock()

	if err := manager.recoverAgentHubGenerations(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		current, found, loadErr := currentResourceGeneration(workspace.Path, record.ResourceID)
		if loadErr != nil || !found || current.Generation <= record.Generation || current.GenerationID == record.GenerationID {
			return false
		}
		updated, messageFound, messageErr := mailboxMessageByID(workspace.Path, message.ID)
		return messageErr == nil && messageFound && updated.Status == resourceMessageDelivered && updated.GenerationID == current.GenerationID
	})
	records, err := loadGenerationRecords(workspace.Path)
	if err != nil || len(records) < 2 || records[1].RetireReason == "" || !strings.Contains(records[1].RetireReason, "idempotency_conflict") {
		t.Fatalf("conflicting generation was not retired with its reason: records=%#v err=%v", records, err)
	}
}

func TestAgentHubRecoveryDoesNotRotateWhenBoundSessionReadIsTransient(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	record := generationRecord{
		ID: "gen-transient-read", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-transient-read", AgentHubSessionID: "ses-transient-read",
		AgentHubAgentName: "fake-agent", SourceInstanceID: "pua-runtime-test", SourceExternalID: "project1.task1/1",
		Status: "recovering", CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}
	seedPollerGeneration(t, fake, workspace, record, agentHubSession{
		ID: record.AgentHubSessionID, State: "stopped", UpdatedAt: "2026-08-01T00:00:10Z",
		Source: &agentHubSource{App: "forge", InstanceID: "pua-runtime-test", ExternalID: record.SourceExternalID},
	})
	fake.mu.Lock()
	fake.failGetSessionID = record.AgentHubSessionID
	fake.mu.Unlock()

	err := manager.recoverAgentHubGenerations(context.Background())
	if err == nil || !strings.Contains(err.Error(), "transient Session read failure") {
		t.Fatalf("transient bound Session read error = %v", err)
	}
	current, found, loadErr := currentResourceGeneration(workspace.Path, record.ResourceID)
	if loadErr != nil || !found || current.GenerationID != record.GenerationID || current.Retired {
		t.Fatalf("transient read rotated the generation: current=%#v found=%v err=%v", current, found, loadErr)
	}
	fake.mu.Lock()
	nextSession, stopCalls, oldState := fake.nextSession, fake.stopCalls, fake.sessions[record.AgentHubSessionID].State
	fake.mu.Unlock()
	if nextSession != 0 || stopCalls != 0 || oldState != "stopped" {
		t.Fatalf("transient read caused Session side effects: creates=%d stops=%d state=%s", nextSession, stopCalls, oldState)
	}
}

func TestAgentHubRecoveryDoesNotReplaceAfterUnknownStopOutcome(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	record := generationRecord{
		ID: "gen-ambiguous-stop", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-ambiguous-stop", AgentHubSessionID: "ses-ambiguous-stop",
		AgentHubAgentName: "fake-agent", SourceInstanceID: "pua-runtime-test", SourceExternalID: "project1.task1/1",
		BindingKind: "profile", BindingName: "default", ResolvedProfile: "default",
		Status: "recovering", ReplacementPending: true,
		CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}
	seedPollerGeneration(t, fake, workspace, record, agentHubSession{
		ID: record.AgentHubSessionID, State: "ready", UpdatedAt: "2026-08-01T00:00:10Z",
		Source: &agentHubSource{App: "forge", InstanceID: "pua-runtime-test", ExternalID: record.SourceExternalID},
	})
	message, err := acceptMailboxMessage(workspace.Path, record.ResourceID, resourceMessageRequest{Text: "wait for certain stop", Mode: resourceMessageModeEnqueue})
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.failNextStop = true
	fake.mu.Unlock()

	if err := manager.recoverAgentHubGenerations(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, found, loadErr := currentResourceGeneration(workspace.Path, record.ResourceID)
	if loadErr != nil || !found || current.GenerationID != record.GenerationID || current.Retired || !current.ReplacementPending {
		t.Fatalf("unknown Stop outcome released the generation: current=%#v found=%v err=%v", current, found, loadErr)
	}
	updated, messageFound, messageErr := mailboxMessageByID(workspace.Path, message.ID)
	if messageErr != nil || !messageFound || updated.Status != resourceMessageQueued {
		t.Fatalf("unknown Stop outcome consumed the mailbox item: message=%#v found=%v err=%v", updated, messageFound, messageErr)
	}
	fake.mu.Lock()
	nextSession, stopCalls, oldState := fake.nextSession, fake.stopCalls, fake.sessions[record.AgentHubSessionID].State
	fake.mu.Unlock()
	if nextSession != 0 || stopCalls != 1 || oldState != "ready" {
		t.Fatalf("unknown Stop outcome created a concurrent Session: creates=%d stops=%d state=%s", nextSession, stopCalls, oldState)
	}

	if err := manager.recoverAgentHubGenerations(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		current, found, loadErr := currentResourceGeneration(workspace.Path, record.ResourceID)
		if loadErr != nil || !found || current.Generation <= record.Generation || current.AgentHubSessionID == record.AgentHubSessionID {
			return false
		}
		updated, messageFound, messageErr := mailboxMessageByID(workspace.Path, message.ID)
		return messageErr == nil && messageFound && updated.Status == resourceMessageDelivered && updated.GenerationID == current.GenerationID
	})
}

func TestAgentHubRecoveryDoesNotReplayConfirmedActiveTurnAfterDaemonRestart(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	record := generationRecord{
		ID: "gen-active-restart", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		AgentHubSessionID: "ses-active-restart", SourceExternalID: workspace.ID + "/run-active-restart",
		Generation: 1, GenerationID: "gen-active-restart", AgentHubAgentName: "fake-agent",
		Status: "running", LastTurnID: "turn-active-restart", CurrentTurnID: "turn-active-restart",
		CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}
	seedPollerGeneration(t, fake, workspace, record, agentHubSession{
		ID: record.AgentHubSessionID, State: "stopped", StopReason: "daemon_recovery",
		UpdatedAt: "2026-08-01T00:00:10Z",
	})
	fake.mu.Lock()
	fake.appendLocked(record.AgentHubSessionID, "message.input", map[string]any{
		"messageId": "msg-confirmed-restart", "text": "already delivered", "role": "user",
	})
	fake.appendLocked(record.AgentHubSessionID, "turn.started", map[string]any{"text": "already delivered"})
	terminal := fake.appendLocked(record.AgentHubSessionID, "turn.cancelled", map[string]any{"reason": "daemon_recovery"})
	terminal.TurnID = record.LastTurnID
	fake.events[record.AgentHubSessionID][len(fake.events[record.AgentHubSessionID])-1] = terminal
	session := fake.sessions[record.AgentHubSessionID]
	session.State = "stopped"
	session.StopReason = "daemon_recovery"
	session.CurrentTurnID = ""
	fake.sessions[session.ID] = session
	fake.mu.Unlock()

	restarted := newAgentManager(manager.server)
	manager.server.agents = restarted
	if err := restarted.recoverAgentHubGenerations(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool {
		rt := restarted.runtimeByID(record.ID)
		if rt == nil {
			return false
		}
		current := pollerGenerationState(rt)
		return current.Status == "stopped" && !current.CompletionPending
	})
	fake.mu.Lock()
	messageIDs := append([]string(nil), fake.messageIDs...)
	actions := append([]string(nil), fake.actions...)
	fake.mu.Unlock()
	if len(messageIDs) != 0 {
		t.Fatalf("restart replayed a confirmed prompt: message ids=%#v", messageIDs)
	}
	for _, action := range actions {
		if action == "resume" {
			t.Fatalf("daemon recovery resumed a terminal stopped Session without mailbox demand: actions=%#v", actions)
		}
	}
}

func TestAgentHubRecoveryDoesNotBlockStartup(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	gate := make(chan struct{})
	blocking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sessions" && r.Method == http.MethodGet {
			<-gate
		}
		fake.ServeHTTP(w, r)
	})
	hub := httptest.NewServer(blocking)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	seedPollerGeneration(t, fake, workspace, generationRecord{
		ID: "gen-live", WorkspaceID: workspace.ID, AgentHubSessionID: "ses_live",
		SourceExternalID: workspace.ID + "/run-live",
		Status:           "running", CreatedAt: "2026-08-01T00:00:01Z", UpdatedAt: "2026-08-01T00:00:01Z",
	}, agentHubSession{ID: "ses_live", State: "ready", UpdatedAt: "2026-08-01T00:00:10Z"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	manager.startAgentRecovery(ctx)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("startup recovery blocked the caller for %s while AgentHub was unresponsive", elapsed)
	}
	close(gate)
	waitForRuntimeTest(t, func() bool {
		rt := manager.runtimeByID("gen-live")
		if rt == nil {
			return false
		}
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return rt.record.Status == "idle"
	})
	// Let the background recovery pass finish projection saves before the
	// deferred cancel and TempDir cleanup race it.
	time.Sleep(100 * time.Millisecond)
}
