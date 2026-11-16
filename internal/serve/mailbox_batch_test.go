package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func batchSender(resource string) *agentHubMessageSender {
	return &agentHubMessageSender{ID: resource, Name: resource}
}

func acceptBatchAgent(t *testing.T, manager *agentManager, workspace serveWorkspace, resource, text, senderResource, instance string) resourceMailboxMessage {
	t.Helper()
	message, err := manager.acceptResourceMessage(context.Background(), workspace, resource, resourceMessageRequest{
		Text: text, Mode: resourceMessageModeEnqueue, Role: "agent", Sender: batchSender(senderResource), SenderWorkspaceInstanceID: instance,
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func batchList(t *testing.T, workspace serveWorkspace, resource, query string) mailboxListResponse {
	t.Helper()
	response, err := listBatch(workspace, resource, httptest.NewRequest(http.MethodGet, "/test?"+query, nil))
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func acceptBatchQueued(t *testing.T, workspace serveWorkspace, resource, text, senderResource, instance string) resourceMailboxMessage {
	t.Helper()
	message, err := acceptMailboxMessage(workspace.Path, resource, resourceMessageRequest{
		Text: text, Mode: resourceMessageModeEnqueue, Role: "agent", Sender: batchSender(senderResource), SenderWorkspaceInstanceID: instance,
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func TestMailboxBatchListUsesStableSourceAndFrozenUpper(t *testing.T) {
	_, workspace, _, _ := outputTestFixture(t, nil)
	root, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfig, err := root.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	same := acceptBatchQueued(t, workspace, "project1.task1", "same source one", "project1.task42", runtimeConfig.InstanceID)
	secondSame := acceptBatchQueued(t, workspace, "project1.task1", "same source two", "project1.task42", runtimeConfig.InstanceID)
	_ = acceptBatchQueued(t, workspace, "project1.task1", "same name elsewhere", "project1.task42", "ws-other")
	other := acceptBatchQueued(t, workspace, "project1.task1", "other", "project1.task99", runtimeConfig.InstanceID)

	first := batchList(t, workspace, "project1.task1", "fromResource=project1.task42&limit=1")
	if len(first.Items) != 1 || first.Items[0].MessageID != same.ID || first.NextCursor == "" ||
		first.UpperSequence != other.Sequence || first.Items[0].SenderWorkspaceInstanceID != runtimeConfig.InstanceID {
		t.Fatalf("source page mismatch: %#v", first)
	}
	later := acceptBatchQueued(t, workspace, "project1.task1", "new after upper", "project1.task42", runtimeConfig.InstanceID)
	second := batchList(t, workspace, "project1.task1", "cursor="+first.NextCursor)
	if len(second.Items) != 1 || second.Items[0].MessageID != secondSame.ID || second.UpperSequence != first.UpperSequence {
		t.Fatalf("frozen page admitted newer message or lost matching row: %#v", second)
	}
	if second.NextCursor != "" {
		t.Fatalf("filtered page ended but returned a cursor: %#v", second)
	}
	fresh := batchList(t, workspace, "project1.task1", "fromResource=project1.task42&limit=10")
	if len(fresh.Items) != 3 || fresh.Items[2].MessageID != later.ID {
		t.Fatalf("fresh query missed later message: %#v", fresh)
	}
	status, found, err := mailboxMessageByID(workspace.Path, same.ID)
	if err != nil || !found || status.Status != resourceMessageQueued {
		t.Fatalf("list consumed message: found=%v err=%v %#v", found, err, status)
	}
}

func TestMailboxBatchReadAckAtomicAuditAndRemainingQueue(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	opener := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "current internal report", resourceMessageModeEnqueue, batchSender("project1.task42"))
	work := *workFile(t, workspace).find(opener.ID)
	root, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfig, err := root.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	one := acceptBatchQueued(t, workspace, "project1.task1", "progress one", "project1.task42", runtimeConfig.InstanceID)
	two := acceptBatchQueued(t, workspace, "project1.task1", "progress two", "project1.task42", runtimeConfig.InstanceID)
	system := acceptBatchQueued(t, workspace, "project1.task1", "system", "project1.task42", runtimeConfig.InstanceID)
	_, err = updateMailboxMessage(workspace.Path, system.ID, func(message *resourceMailboxMessage) {
		message.Type = resourceMessageTypeSchedulerTick
	})
	if err != nil {
		t.Fatal(err)
	}

	read, err := manager.readBatch(context.Background(), workspace, "project1.task1", mailboxReadRequest{
		IDs: []string{one.ID, two.ID, system.ID, opener.ID}, WorkID: work.ID, MaxBytes: 64 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipts := map[string]string{}
	var eligible []string
	for _, item := range read.Items {
		if item.MessageID == system.ID && item.Reason != "not_ordinary_agent_message" {
			t.Fatalf("system input became batch eligible: %#v", item)
		}
		if item.MessageID == opener.ID && item.Reason != "body_unavailable_in_receipt" && item.Reason != "message_not_waiting" {
			t.Fatalf("current input unexpectedly reusable: %#v", item)
		}
		if item.CanAck {
			eligible = append(eligible, item.MessageID)
			receipts[item.MessageID] = item.ReadReceipt
		}
	}
	if len(eligible) != 2 || read.Queue.WaitingTotal != 3 || read.Queue.SameSourceWaiting != 2 || read.Queue.BatchEligibleFromSource != 2 {
		t.Fatalf("read eligibility/count mismatch: %#v", read)
	}
	bad := mailboxAckRequest{OperationID: "op-1", WorkID: work.ID, IDs: eligible, Receipts: map[string]string{one.ID: receipts[one.ID]}, Summary: "done"}
	if _, err = manager.ackBatch(context.Background(), workspace, "project1.task1", bad); err == nil {
		t.Fatal("missing receipt was accepted")
	}
	request := mailboxAckRequest{OperationID: "op-1", WorkID: work.ID, IDs: eligible, Receipts: receipts, Summary: "both reports were handled and recorded"}
	result, err := manager.ackBatch(context.Background(), workspace, "project1.task1", request)
	if err != nil || result.Handled != 2 || result.Queue.WaitingTotal != 1 || result.Queue.SameSourceWaiting != 0 {
		t.Fatalf("ack mismatch: %#v err=%v", result, err)
	}
	again, err := manager.ackBatch(context.Background(), workspace, "project1.task1", request)
	if err != nil || again.OperationID != result.OperationID || len(again.IDs) != 2 {
		t.Fatalf("operation replay failed: %#v err=%v", again, err)
	}
	other := request
	other.Summary = "different summary"
	if _, err = manager.ackBatch(context.Background(), workspace, "project1.task1", other); err == nil {
		t.Fatal("same operation accepted different input")
	}
	archived, found, err := mailboxMessageByID(workspace.Path, one.ID)
	if err != nil || !found || archived.Status != resourceMessageHandled || archived.Text != "progress one" ||
		archived.Handling == nil || archived.Handling.Summary != request.Summary || archived.Handling.History == "" {
		t.Fatalf("handled audit unavailable: found=%v err=%v %#v", found, err, archived)
	}
	if len(fake.messageIDs) != 1 || fake.messageIDs[0] != opener.ID {
		t.Fatalf("ack created provider inputs: %#v", fake.messageIDs)
	}
	closeWorkflowTurn(fake, work, "completed", "done")
	reconcileWorkflow(t, manager, workspace, client)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.messageIDs) != 2 || fake.messageIDs[0] != opener.ID || fake.messageIDs[1] != system.ID {
		t.Fatalf("handled message was rescheduled: %#v", fake.messageIDs)
	}
}

func TestMailboxBatchRejectsVisibleWorkAndContinuationTargets(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	original := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "internal", resourceMessageModeEnqueue, nil)
	a := *workFile(t, workspace).find(original.ID)
	_ = queueWorkflowUser(t, manager, workspace, "question", "chat question")
	reconcileWorkflow(t, manager, workspace, client)
	child := queueWorkflowUser(t, manager, workspace, "child", "second")
	workCommand(t, manager, workspace, turnWorkCommand{Action: "yield", WorkID: a.ID, MessageID: child.ID, Checkpoint: "checkpoint"})

	listed := batchList(t, workspace, "project1.task1", "fromResource=project1.task2&limit=10")
	for _, item := range listed.Items {
		if item.MessageID == child.ID && item.Reason != "continuation_dependency" {
			t.Fatalf("yield target is consumable: %#v", item)
		}
	}
	fake.mu.Lock()
	var providerIDs []string
	for _, id := range fake.messageIDs {
		if !strings.HasPrefix(id, "preview-") {
			providerIDs = append(providerIDs, id)
		}
	}
	fake.mu.Unlock()
	if len(providerIDs) != 1 || providerIDs[0] != original.ID {
		t.Fatalf("test consumed queued business input: %#v", fake.messageIDs)
	}
	userMessage, found, err := mailboxMessageByID(workspace.Path, "chat-"+chatID(sharedChatID("project1.task1"), "input-question"))
	if err != nil || !found {
		t.Fatal("visible chat input missing")
	}
	if _, err = manager.ackBatch(context.Background(), workspace, "project1.task1", mailboxAckRequest{
		OperationID: "op-visible", WorkID: userMessage.ID, IDs: []string{child.ID}, Receipts: map[string]string{child.ID: "x"}, Summary: "done",
	}); err == nil {
		t.Fatal("visible work could confirm queue messages")
	}
}

func TestMailboxBatchFreezesOpenerAndPreviewCounts(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	root, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfig, err := root.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	opener := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "first actual work", resourceMessageModeEnqueue, batchSender("project1.task42"))
	work := *workFile(t, workspace).find(opener.ID)
	fake.mu.Lock()
	wire := fake.messageInputs[opener.ID]
	fake.mu.Unlock()
	if !strings.Contains(wire.Text, "waiting=0") || !strings.Contains(wire.Text, "same_source_waiting=0") ||
		!strings.Contains(wire.Text, "current_excluded=true") {
		t.Fatalf("opener queue snapshot missing: %q", wire.Text)
	}
	_ = acceptBatchQueued(t, workspace, "project1.task1", "arrives after opener", "project1.task42", runtimeConfig.InstanceID)
	_ = acceptBatchQueued(t, workspace, "project1.task1", "arrives before preview", "project1.task42", runtimeConfig.InstanceID)
	reconcileWorkflow(t, manager, workspace, client)
	fake.mu.Lock()
	var previews []agentHubInboundMessage
	for _, input := range fake.messageInputs {
		if strings.HasPrefix(input.MessageID, "preview-") {
			previews = append(previews, input)
		}
	}
	fake.mu.Unlock()
	if len(previews) == 0 {
		t.Fatal("queued inputs were not previewed")
	}
	for _, preview := range previews {
		if !strings.Contains(preview.Text, "waiting=2") || !strings.Contains(preview.Text, "same_source_waiting=2") ||
			!strings.Contains(preview.Text, "includes_preview_message=true") {
			t.Fatalf("preview did not freeze queue snapshot: %#v", preview)
		}
	}
	_ = work
}
