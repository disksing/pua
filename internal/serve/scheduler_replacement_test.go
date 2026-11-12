package serve

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func TestSchedulerFailedPendingTurnAllowsBindingReplacementAndRecoveryTick(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	fake.extraAgents = []string{"replacement-agent"}
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	client, err := newAgentHubClient(hub.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.AddSchedule(app.CreateScheduleInput{Description: "Check status", Condition: "when appropriate", Target: "workspace"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace, client); err != nil {
		t.Fatal(err)
	}
	firstTick := schedulerMessages(t, workspace.Path)[0]
	first, found, err := generationRecordByID(workspace.Path, firstTick.GenerationID)
	if err != nil || !found {
		t.Fatalf("generation = %+v, found=%v, err=%v", first, found, err)
	}
	_, err = updateMailboxMessage(workspace.Path, firstTick.ID, func(message *resourceMailboxMessage) { message.TurnID = "turn-pending-tick" })
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	old := fake.sessions[first.AgentHubSessionID]
	old.State, old.CurrentTurnID = "starting", "turn-pending-tick"
	fake.sessions[old.ID] = old
	fake.turns[old.ID] = map[string]agentHubTurn{"turn-pending-tick": {TurnID: "turn-pending-tick", Status: "active"}}
	fake.mu.Unlock()
	binding := app.AgentBinding{Kind: "agent", Name: "replacement-agent"}
	if _, err := puaWorkspace.SetSchedulerSettings(app.SchedulerSettingsInput{AgentBinding: binding, WakeIntervalMinutes: 10}); err != nil {
		t.Fatal(err)
	}
	if err := manager.resourceBindingChanged(context.Background(), workspace, app.SchedulerResourceID, binding); err != nil {
		t.Fatal(err)
	}
	message, err := manager.acceptResourceMessage(context.Background(), workspace, app.SchedulerResourceID, resourceMessageRequest{Text: "check again", Mode: resourceMessageModeSteer})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			stored, _, _ := mailboxMessageByID(workspace.Path, message.ID)
			records, _ := loadGenerationRecords(workspace.Path)
			t.Logf("message=%+v generations=%+v", stored, records)
			fake.mu.Lock()
			t.Logf("sessions=%+v actions=%+v", fake.sessions, fake.actions)
			fake.mu.Unlock()
		}
	})
	if err := manager.withResourceController(context.Background(), workspace, app.SchedulerResourceID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	stored, found, err := mailboxMessageByID(workspace.Path, message.ID)
	if err != nil || !found || stored.Status != resourceMessageQueued || stored.DowngradeReason != resourceMessageReasonGenerationReplacing {
		t.Fatalf("replacement queue = %+v, err=%v", stored, err)
	}
	if fakeStopCalls(fake) != 0 {
		t.Fatal("binding change interrupted the active old provider")
	}
	// AgentHub now closes its pending input after a permanent startup failure.
	fake.mu.Lock()
	old = fake.sessions[first.AgentHubSessionID]
	failure := fake.appendLocked(old.ID, "turn.failed", map[string]any{"error": "native provider session is missing"})
	fake.events[old.ID][len(fake.events[old.ID])-1].TurnID = "turn-pending-tick"
	old = fake.sessions[old.ID]
	old.State, old.StopReason, old.CurrentTurnID = "stopped", "startup_error", ""
	fake.sessions[old.ID] = old
	fake.turns[old.ID]["turn-pending-tick"] = agentHubTurn{TurnID: "turn-pending-tick", Status: "failed", Closed: true, EndedAt: failure.Time, LastEventID: failure.ID}
	fake.mu.Unlock()
	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.withResourceController(context.Background(), workspace, app.SchedulerResourceID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	var replacement generationRecord
	waitForRuntimeTest(t, func() bool {
		stored, found, _ := mailboxMessageByID(workspace.Path, message.ID)
		current, currentFound, _ := currentResourceGeneration(workspace.Path, app.SchedulerResourceID)
		if found && stored.Status == resourceMessageDelivered && currentFound && current.AgentHubAgentName == "replacement-agent" {
			replacement = current
			return true
		}
		return false
	})
	fake.mu.Lock()
	old = fake.sessions[first.AgentHubSessionID]
	current := fake.sessions[replacement.AgentHubSessionID]
	current.State, current.CurrentTurnID = "ready", ""
	fake.sessions[current.ID] = current
	fake.mu.Unlock()
	if old.State != "archived" {
		t.Fatalf("old pending generation not archived: %+v", old)
	}
	if err := manager.pollAgentHubSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace, client); err != nil {
		t.Fatal(err)
	}
	ticks := schedulerMessages(t, workspace.Path)
	if len(ticks) != 2 || ticks[1].Causation.Reason != schedulerTickReasonRecovery {
		t.Fatalf("failed tick did not recover: %+v", ticks)
	}
	waitForRuntimeTest(t, func() bool {
		stored, found, _ := mailboxMessageByID(workspace.Path, ticks[1].ID)
		return found && stored.Status == resourceMessageDelivered && stored.GenerationID == replacement.GenerationID
	})
	// A repeated reconciliation must not create another outstanding tick.
	manager.now = func() time.Time { return time.Now() }
	if err := manager.reconcileSchedulerLocked(context.Background(), workspace, client); err != nil {
		t.Fatal(err)
	}
	if len(schedulerMessages(t, workspace.Path)) != 2 {
		t.Fatal("recovery duplicated an outstanding tick")
	}
}
