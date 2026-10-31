package serve

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func TestAgentMessageDoesNotReturnCompletedTurn(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: "project1", Title: "Sender"}); err != nil {
		t.Fatal(err)
	}
	runtime, err := puaWorkspace.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	message, err := manager.acceptResourceMessage(context.Background(), workspace, "project1.task1", resourceMessageRequest{
		Text: "do the work", Role: "agent", Mode: resourceMessageModeEnqueue,
		Sender: &agentHubMessageSender{ID: "project1.task2"}, SenderWorkspaceInstanceID: runtime.InstanceID,
	})
	if err != nil || message.Status != resourceMessageDelivered || message.SubscribeResult != nil {
		t.Fatalf("ordinary agent delivery = %#v, %v", message, err)
	}
	record, found, err := currentResourceGeneration(workspace.Path, "project1.task1")
	if err != nil || !found {
		t.Fatalf("generation lookup: found=%v err=%v", found, err)
	}
	fake.mu.Lock()
	fake.turns[record.AgentHubSessionID] = map[string]agentHubTurn{
		"turn-completed": {TurnID: "turn-completed", Status: "completed", Closed: true, Items: []agentHubTurnItem{{Role: "assistant", Text: "done"}}},
	}
	fake.mu.Unlock()
	for i := 0; i < 2; i++ {
		if err := manager.reconcileWorkspaceNotifications(context.Background(), workspace, nil); err != nil {
			t.Fatal(err)
		}
	}
	senderMailbox, err := loadResourceMailboxForResource(workspace.Path, "project1.task2")
	if err != nil || len(senderMailbox.Messages) != 0 {
		t.Fatalf("completed Turn produced a result: %#v, %v", senderMailbox.Messages, err)
	}
	fake.mu.Lock()
	deliveries := append([]string(nil), fake.messageIDs...)
	fake.mu.Unlock()
	if len(deliveries) != 1 || deliveries[0] != message.ID {
		t.Fatalf("unexpected AgentHub deliveries: %#v", deliveries)
	}
}

func TestLegacyResultSubscriptionsAndOutboxDoNotResumeAfterRestart(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: "project1", Title: "Sender"}); err != nil {
		t.Fatal(err)
	}
	runtime, err := puaWorkspace.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}

	// Persist queued inputs first, then write the pre-upgrade hot representation.
	var sources []resourceMailboxMessage
	for _, text := range []string{"unbound", "waiting one", "waiting two", "accepted"} {
		message, err := acceptMailboxMessage(workspace.Path, "project1.task1", resourceMessageRequest{
			Text: text, Role: "agent", Mode: resourceMessageModeEnqueue,
			Sender: &agentHubMessageSender{ID: "project1.task2"}, SenderWorkspaceInstanceID: runtime.InstanceID,
		})
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, message)
	}
	directory, _, _, err := resourceMailboxDirectory(workspace.Path, "project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	var hot resourceMailboxHotDocument
	if found, err := readResourceMailboxJSON(resourceMailboxHotPath(directory), &hot); err != nil || !found {
		t.Fatalf("load old hot document: found=%v err=%v", found, err)
	}
	now := time.Now().Format(time.RFC3339Nano)
	for i := range hot.Messages {
		hot.Messages[i].Status = resourceMessageDelivered
		hot.Messages[i].SubscribeResult = boolPtr(true)
		hot.Messages[i].ResultSubscriptionStatus = "pending"
		hot.Messages[i].GenerationID = "old-generation"
		hot.Messages[i].TurnID = "completed-turn"
		hot.Messages[i].DeliveredAt = now
		hot.Messages[i].TerminalAt = now
	}
	if err := writeResourceMailboxJSON(resourceMailboxHotPath(directory), hot); err != nil {
		t.Fatal(err)
	}
	operations := []resourceMailboxNotificationOp{
		{ID: "old-waiting", Type: resourceMessageTypeTurnResult, SourceMessageIDs: []string{sources[1].ID, sources[2].ID},
			SourceResourceID: "project1.task1", TargetWorkspaceInstanceID: runtime.InstanceID, TargetResourceID: "project1.task2",
			GeneratedMessageID: "old-result-not-accepted", GeneratedText: "old result", Status: resourceNotificationWaiting},
		{ID: "old-accepted", Type: resourceMessageTypeTurnResult, SourceMessageID: sources[3].ID,
			SourceResourceID: "project1.task1", TargetWorkspaceInstanceID: runtime.InstanceID, TargetResourceID: "project1.task2",
			GeneratedMessageID: "old-result-queued", Status: resourceNotificationAccepted},
	}
	for _, operation := range operations {
		if err := upsertMailboxNotificationOperationForSources(workspace.Path, operation.SourceMessageIDs, operation); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := updateMailboxMessage(workspace.Path, sources[3].ID, func(message *resourceMailboxMessage) {
		message.Notification.Type = ""
	}); err != nil {
		t.Fatal(err)
	}
	// An orphaned operation must also be retired without creating its result.
	_, err = mutateResourceMailboxStoreForResource(workspace.Path, "project1.task1", func(store *resourceMailboxStore) error {
		store.Outbox.Operations = append(store.Outbox.Operations, resourceMailboxNotificationOp{
			ID: "old-orphan", Type: "creator_turn_result", SourceMessageID: "missing-source",
			TargetWorkspaceInstanceID: runtime.InstanceID, TargetResourceID: "project1.task2",
			GeneratedMessageID: "old-orphan-result", GeneratedText: "orphan", Status: resourceNotificationWaiting,
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ id, status string }{
		{"old-result-queued", resourceMessageQueued},
		{"old-result-delivering", resourceMessageDelivering},
		{"old-result-delivered", resourceMessageDelivered},
	} {
		messageType := resourceMessageTypeTurnResult
		if item.id == "old-result-delivering" {
			messageType = "creator_turn_result"
		}
		appendNotificationTestMessage(t, workspace.Path, resourceMailboxMessage{
			ID: item.id, ResourceID: "project1.task2", Role: "system", Text: "old result",
			Type: messageType, Status: item.status,
			RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue,
			AcceptedAt: now, UpdatedAt: now, TerminalAt: now,
		})
	}

	for i := 0; i < 2; i++ {
		manager = newAgentManager(manager.server)
		manager.server.agents = manager
		if err := manager.reconcileWorkspaceNotifications(context.Background(), workspace, nil); err != nil {
			t.Fatal(err)
		}
		if err := manager.reconcileResourceMailboxLocked(context.Background(), workspace, "project1.task2"); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := pendingResourceMailboxNotificationOperations(workspace.Path)
	if err != nil || len(pending) != 0 {
		t.Fatalf("legacy operations remain pending: %#v, %v", pending, err)
	}
	store, err := loadResourceMailboxStoreForRead(workspace.Path, "project1.task1")
	if err != nil || len(store.Outbox.Operations) != 3 {
		t.Fatalf("legacy operation audit records were lost: %#v, %v", store.Outbox.Operations, err)
	}
	for i, source := range sources {
		updated, found, err := mailboxMessageByID(workspace.Path, source.ID)
		if err != nil || !found || updated.SubscribeResult == nil || !*updated.SubscribeResult || updated.ResultSubscriptionStatus != "pending" {
			t.Fatalf("source audit fields lost: %#v found=%v err=%v", updated, found, err)
		}
		if i != 0 && (updated.Notification == nil || updated.Notification.Status != resourceNotificationTerminal ||
			updated.Notification.LastErrorCode != "result_subscription_removed") {
			t.Fatalf("legacy operation receipt not retired: %#v", updated.Notification)
		}
	}
	for _, item := range []struct{ id, status string }{
		{"old-result-queued", resourceMessageCancelled},
		{"old-result-delivering", resourceMessageDeliveryUnknown},
		{"old-result-delivered", resourceMessageDelivered},
	} {
		message, found, err := mailboxMessageByID(workspace.Path, item.id)
		if err != nil || !found || message.Status != item.status || !isLegacyTurnResultType(message.Type) {
			t.Fatalf("legacy destination %s = %#v found=%v err=%v", item.id, message, found, err)
		}
	}
	fake.mu.Lock()
	deliveries := append([]string(nil), fake.messageIDs...)
	fake.mu.Unlock()
	if len(deliveries) != 0 {
		t.Fatalf("legacy result was delivered after restart: %#v", deliveries)
	}
}
