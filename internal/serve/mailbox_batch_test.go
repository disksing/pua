package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

func TestMailboxBatchWorkspaceRoutes(t *testing.T) {
	manager, workspace, fake, _ := workflowFixture(t)
	const resource = "workspace"
	const source = "project1"
	instance, err := workspaceInstanceID(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	opener := acceptBatchAgent(t, manager, workspace, resource, "current report", source, instance)
	works, err := loadTurnWork(workspace.Path, resource)
	if err != nil {
		t.Fatal(err)
	}
	work := works.find(opener.ID)
	if work == nil || work.TurnID == "" {
		t.Fatalf("Workspace opener did not start a Turn: %#v", work)
	}
	explicit := acceptBatchQueued(t, workspace, resource, "explicit report", "project1.task1", instance)
	filtered := acceptBatchQueued(t, workspace, resource, "source report", source, instance)
	waiting := acceptBatchQueued(t, workspace, resource, "still waiting", "project1.task1", instance)

	call := func(method, path string, input, output any, status int) {
		t.Helper()
		body, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(method, "/api/workspaces/"+workspace.ID+"/resources/"+resource+path, bytes.NewReader(body))
		recorder := httptest.NewRecorder()
		manager.server.handleWorkspace(recorder, request)
		if recorder.Code != status {
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, recorder.Code, status, recorder.Body)
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), output); err != nil {
			t.Fatal(err)
		}
	}
	read := func(id, text string) {
		t.Helper()
		var response mailboxReadResponse
		call(http.MethodPost, "/messages/read", mailboxReadRequest{WorkID: work.ID, IDs: []string{id}}, &response, http.StatusOK)
		if len(response.Items) != 1 {
			t.Fatalf("expected one read item: %#v", response.Items)
		}
		item := response.Items[0]
		if item.MessageID != id || item.Text != text || !item.Complete || !item.CanAck || item.ReadReceipt == "" {
			t.Fatalf("Workspace read did not return full, eligible body: %#v", item)
		}
	}
	read(explicit.ID, explicit.Text)
	var listed mailboxListResponse
	call(http.MethodGet, "/messages?status=waiting&role=agent&fromResource="+source, nil, &listed, http.StatusOK)
	if len(listed.Items) != 1 || listed.Items[0].MessageID != filtered.ID || !listed.Items[0].CanAck {
		t.Fatalf("source filter returned unexpected messages: %#v", listed.Items)
	}
	read(listed.Items[0].MessageID, filtered.Text)
	read(waiting.ID, waiting.Text)

	request := mailboxAckRequest{OperationID: "op-workspace", WorkID: work.ID, IDs: []string{explicit.ID, filtered.ID}, Summary: "reports handled"}
	var committed mailboxAckResult
	call(http.MethodPost, "/messages/ack", request, &committed, http.StatusOK)
	if committed.Handled != 2 || committed.History == "" || committed.Queue.WaitingTotal != 1 {
		t.Fatalf("Workspace ack result: %#v", committed)
	}
	for _, id := range request.IDs {
		message, found, err := mailboxMessageByID(workspace.Path, id)
		if err != nil || !found || message.Status != resourceMessageHandled || message.Handling == nil || message.Handling.OperationID != request.OperationID {
			t.Fatalf("message %s missing durable handling: found=%t err=%v message=%#v", id, found, err, message)
		}
	}

	stale := mailboxAckRequest{OperationID: "op-workspace-stale", WorkID: work.ID, IDs: []string{waiting.ID}, Summary: "report handled"}
	closeWorkflowTurn(fake, *work, "completed", "done")
	var replay, lookedUp mailboxAckResult
	call(http.MethodPost, "/messages/ack", request, &replay, http.StatusOK)
	call(http.MethodGet, "/message-operations/"+request.OperationID, nil, &lookedUp, http.StatusOK)
	if !reflect.DeepEqual(replay, committed) || !reflect.DeepEqual(lookedUp, committed) {
		t.Fatalf("replay or lookup changed the committed result: replay=%#v lookup=%#v", replay, lookedUp)
	}

	var failure struct {
		Code string
	}
	call(http.MethodPost, "/messages/read", mailboxReadRequest{WorkID: work.ID, IDs: stale.IDs}, &failure, http.StatusConflict)
	if failure.Code != "batch_work_unavailable" {
		t.Fatalf("unexpected stale read error: %#v", failure)
	}
	failure.Code = ""
	call(http.MethodPost, "/messages/ack", stale, &failure, http.StatusConflict)
	if failure.Code != "batch_work_unavailable" {
		t.Fatalf("unexpected stale work error: %#v", failure)
	}
	call(http.MethodGet, "/message-operations/"+stale.OperationID, nil, &failure, http.StatusNotFound)
	if failure.Code != "message_not_found" {
		t.Fatalf("unexpected uncommitted operation lookup: %#v", failure)
	}
	listed = mailboxListResponse{}
	call(http.MethodGet, "/messages?status=waiting&role=agent", nil, &listed, http.StatusOK)
	if len(listed.Items) != 1 || listed.Items[0].MessageID != waiting.ID {
		t.Fatalf("closed work changed remaining queue: %#v", listed.Items)
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
			!strings.Contains(preview.Text, "including the previewed message") {
			t.Fatalf("preview did not freeze queue snapshot: %#v", preview)
		}
	}
	_ = work
}

func TestMailboxBatchAccumulatesReadClaimsAcrossPagesAndRestart(t *testing.T) {
	manager, workspace, _, _ := workflowFixture(t)
	opener := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "current internal report", resourceMessageModeEnqueue, batchSender("project1.task42"))
	work := *workFile(t, workspace).find(opener.ID)
	one := acceptBatchQueued(t, workspace, "project1.task1", "page one", "project1.task42", "ws-a")
	two := acceptBatchQueued(t, workspace, "project1.task1", "page two", "project1.task42", "ws-a")

	for _, id := range []string{one.ID, two.ID, one.ID} {
		read, err := manager.readBatch(context.Background(), workspace, "project1.task1", mailboxReadRequest{
			IDs: []string{id}, WorkID: work.ID, MaxBytes: 1024,
		})
		if err != nil || len(read.Items) != 1 || !read.Items[0].CanAck {
			t.Fatalf("read claim was not issued: %#v err=%v", read, err)
		}
	}
	manager.waitBackground()
	manager = newAgentManager(manager.server)
	manager.server.agents = manager
	store, err := loadResourceMailboxStoreForRead(workspace.Path, "project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := readMailboxClaims(store.Directory, work.ID)
	if err != nil || len(claims.Messages) != 2 || len(claims.Receipts) != 2 {
		t.Fatalf("restarted claims lost earlier page or duplicated a read: %#v err=%v", claims, err)
	}
	result, err := manager.ackBatch(context.Background(), workspace, "project1.task1", mailboxAckRequest{
		OperationID: "op-cross-page", WorkID: work.ID, IDs: []string{one.ID, two.ID}, Summary: "handled across read pages",
	})
	if err != nil || result.Handled != 2 {
		t.Fatalf("cross-page server claims were not accepted: %#v err=%v", result, err)
	}
	if _, err := os.Stat(mailboxReadClaimsPath(defaultResourceMailboxStore(workspace.Path, "project1.task1").Directory, work.ID)); err != nil {
		t.Fatalf("read claims were not persisted: %v", err)
	}
	if _, found, err := readMailboxOperation(defaultResourceMailboxStore(workspace.Path, "project1.task1").Directory, "op-cross-page"); err != nil || !found {
		t.Fatalf("operation audit missing after store reload: found=%v err=%v", found, err)
	}
	reloaded, err := loadResourceMailboxStoreForRead(workspace.Path, "project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	claims, err = readMailboxClaims(reloaded.Directory, work.ID)
	if err != nil || len(claims.Messages) != 2 || len(claims.Receipts) != 2 {
		t.Fatalf("restarted claims lost earlier page: %#v err=%v", claims, err)
	}
}

func TestMailboxBatchRejectsStaleSessionAfterClosedCanonicalTurn(t *testing.T) {
	manager, workspace, fake, _ := workflowFixture(t)
	opener := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "current internal report", resourceMessageModeEnqueue, batchSender("project1.task42"))
	work := *workFile(t, workspace).find(opener.ID)
	message := acceptBatchQueued(t, workspace, "project1.task1", "queued report", "project1.task42", "ws-a")

	fake.mu.Lock()
	turn := fake.turns[work.SessionID][work.TurnID]
	turn.Closed, turn.Status = true, "completed"
	fake.turns[work.SessionID][work.TurnID] = turn
	session := fake.sessions[work.SessionID]
	session.State, session.CurrentTurnID = "running", work.TurnID
	fake.sessions[work.SessionID] = session
	fake.mu.Unlock()

	if _, err := manager.readBatch(context.Background(), workspace, "project1.task1", mailboxReadRequest{
		IDs: []string{message.ID}, WorkID: work.ID, MaxBytes: 1024,
	}); err == nil {
		t.Fatal("stale Session projection allowed a read claim")
	}
	if _, err := manager.ackBatch(context.Background(), workspace, "project1.task1", mailboxAckRequest{
		OperationID: "op-stale", WorkID: work.ID, IDs: []string{message.ID}, Summary: "stale turn",
	}); err == nil {
		t.Fatal("stale Session projection allowed ack")
	}
	queued, found, err := mailboxMessageByID(workspace.Path, message.ID)
	if err != nil || !found || queued.Status != resourceMessageQueued {
		t.Fatalf("closed Turn ack mutated queue: found=%v err=%v %#v", found, err, queued)
	}
}

func TestMailboxBatchTransactionVersionProtectsExtraFiles(t *testing.T) {
	_, workspace, _, _ := outputTestFixture(t, nil)
	message := acceptBatchQueued(t, workspace, "project1.task1", "queued report", "project1.task42", "ws-a")
	store, err := loadResourceMailboxStoreForRead(workspace.Path, "project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	claimsPath := mailboxReadClaimsPath(store.Directory, "work-1")
	extra := resourceMailboxDocument{value: mailboxReadClaims{
		Version: 1, WorkID: "work-1", Receipts: map[string]string{}, Messages: []resourceMailboxMessage{message},
	}, path: claimsPath}
	if err := writeResourceMailboxStoreDocuments(store.Directory, mailboxStoreMeta(store), resourceMailboxHotDocument{
		Version: resourceMailboxStoreVersion, ResourceID: store.ResourceID, NextSequence: store.Mailbox.NextSequence, Messages: store.Mailbox.Messages,
	}, store.Receipts, store.Outbox, store.Scheduler, extra); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(claimsPath); err != nil {
		t.Fatalf("extra file was not committed: %v", err)
	}

	data, err := os.ReadFile(resourceMailboxTxnPath(store.Directory))
	if err == nil {
		t.Fatalf("committed transaction should be removed: %s", data)
	}
	txnPath := resourceMailboxTxnPath(store.Directory)
	fakeTxn := resourceMailboxTxn{Version: resourceMailboxTxnBatchFilesVersion,
		Files: []resourceMailboxTxnFile{{Temp: claimsPath + ".crash.tmp", Final: claimsPath}}}
	crashTemp := claimsPath + ".crash.tmp"
	if err := os.Rename(claimsPath, crashTemp); err != nil {
		t.Fatal(err)
	}
	if err := writeResourceMailboxJSON(txnPath, fakeTxn); err != nil {
		t.Fatal(err)
	}
	if err := recoverResourceMailboxTransaction(store.Directory); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(claimsPath); err != nil {
		t.Fatalf("crash recovery did not restore extra file: %v", err)
	}
	if err := writeResourceMailboxJSON(txnPath, resourceMailboxTxn{Version: "mailbox-future-v3"}); err != nil {
		t.Fatal(err)
	}
	if err := recoverResourceMailboxTransaction(store.Directory); err == nil {
		t.Fatal("future transaction version was silently removed")
	}
	if _, err := os.Stat(txnPath); err != nil {
		t.Fatalf("unsupported transaction journal was deleted: %v", err)
	}

	legacyFinal := filepath.Join(store.Directory, "claims-legacy.json")
	legacyTemp := legacyFinal + ".tmp"
	if err := os.WriteFile(legacyFinal, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyTemp, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyJournal := fmt.Sprintf(`{"version":1,"files":[{"temp":%q,"final":%q}]}`, legacyTemp, legacyFinal)
	if err := os.WriteFile(txnPath, []byte(legacyJournal), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverResourceMailboxTransaction(store.Directory); err == nil ||
		!strings.Contains(err.Error(), "mailbox transaction Files require mailbox-batch-v2") {
		t.Fatalf("legacy version=1 journal did not reject Files: %v", err)
	}
	if _, err := os.Stat(txnPath); err != nil {
		t.Fatalf("rejected legacy journal was deleted: %v", err)
	}
	content, err := os.ReadFile(legacyFinal)
	if err != nil || string(content) != "original" {
		t.Fatalf("rejected legacy recovery replaced the final file: content=%q err=%v", content, err)
	}
	if _, err := os.Stat(legacyTemp); err != nil {
		t.Fatalf("rejected legacy recovery moved the temp file: %v", err)
	}
}

func TestMailboxBatchAckTransactionCrashWindows(t *testing.T) {
	for window := 0; window <= 7; window++ {
		t.Run(fmt.Sprintf("renamed-%d", window), func(t *testing.T) {
			_, workspace, _, _ := outputTestFixture(t, nil)
			message := acceptBatchQueued(t, workspace, "project1.task1", "complete audited body", "project1.task42", "ws-a")
			store, err := loadResourceMailboxStoreForRead(workspace.Path, "project1.task1")
			if err != nil {
				t.Fatal(err)
			}
			handled := cloneMailboxMessage(message)
			handled.Status = resourceMessageHandled
			handled.Handling = &mailboxHandling{OperationID: "crash-ack", WorkID: "work-a", Summary: "done", BodyDigest: batchHash(message.Text), History: "history-a"}
			for index := range store.Mailbox.Messages {
				if store.Mailbox.Messages[index].ID == message.ID {
					store.Mailbox.Messages[index] = handled
				}
			}
			store.BatchCommit = &mailboxHandledOperation{
				Version: 1, RequestDigest: "frozen-request",
				Result:   mailboxAckResult{OperationID: "crash-ack", WorkID: "work-a", IDs: []string{message.ID}, Handled: 1},
				Messages: []resourceMailboxMessage{handled},
			}
			store.BatchExtra = []resourceMailboxDocument{{
				path:  mailboxReadClaimsPath(store.Directory, "work-a"),
				value: mailboxReadClaims{Version: 1, WorkID: "work-a", Receipts: map[string]string{message.ID: "receipt"}, Messages: []resourceMailboxMessage{message}},
			}}
			hot, receipts, outbox, scheduler := prepareResourceMailboxDocuments(store)
			extra, err := mailboxHandledDocuments(store)
			if err != nil {
				t.Fatal(err)
			}
			documents := 4 + len(extra)
			if documents != 7 {
				t.Fatalf("update crash windows for %d transaction documents", documents)
			}
			renamed := 0
			crash := errors.New("simulated process loss")
			err = commitResourceMailboxDocuments(store.Directory, mailboxStoreMeta(store), hot, receipts, outbox, scheduler, func(temp, final string) error {
				if renamed == window {
					return crash
				}
				if err := os.Rename(temp, final); err != nil {
					return err
				}
				renamed++
				if window == documents && renamed == documents {
					return crash
				}
				return nil
			}, extra...)
			if !errors.Is(err, crash) || renamed != window {
				t.Fatalf("fault did not stop at requested window: renamed=%d err=%v", renamed, err)
			}
			journal, err := os.ReadFile(resourceMailboxTxnPath(store.Directory))
			if err != nil {
				t.Fatal(err)
			}
			var legacy struct {
				Version int `json:"version"`
			}
			if err := json.Unmarshal(journal, &legacy); err == nil {
				t.Fatal("legacy int-version recovery could consume a Files transaction")
			}
			reloaded, err := loadResourceMailboxStoreForRead(workspace.Path, "project1.task1")
			if err != nil {
				t.Fatal(err)
			}
			operation, found, err := readMailboxOperation(reloaded.Directory, "crash-ack")
			if err != nil || !found || operation.Result.Handled != 1 || len(operation.Messages) != 1 || operation.Messages[0].Text != message.Text {
				t.Fatalf("operation/body lost after recovery: %#v found=%v err=%v", operation, found, err)
			}
			audit, found, err := mailboxHandledByID(reloaded.Directory, message.ID)
			if err != nil || !found || audit.Status != resourceMessageHandled || audit.Handling.History != "history-a" {
				t.Fatalf("handled index lost after recovery: %#v found=%v err=%v", audit, found, err)
			}
			claims, err := readMailboxClaims(reloaded.Directory, "work-a")
			if err != nil || claims.Receipts[message.ID] != "receipt" {
				t.Fatalf("claims lost after recovery: %#v err=%v", claims, err)
			}
			if err := recoverResourceMailboxTransaction(reloaded.Directory); err != nil {
				t.Fatalf("second recovery failed: %v", err)
			}
			if _, err := os.Stat(resourceMailboxTxnPath(reloaded.Directory)); !os.IsNotExist(err) {
				t.Fatalf("recovered journal remains: %v", err)
			}
			if _, err := mutateResourceMailboxStoreForResource(workspace.Path, "project1.task1", func(*resourceMailboxStore) error { return nil }); err != nil {
				t.Fatal(err)
			}
			var meta resourceMailboxMeta
			if found, err := readResourceMailboxJSON(resourceMailboxMetaPath(reloaded.Directory), &meta); err != nil || !found || meta.Version != resourceMailboxBatchStoreVersion {
				t.Fatalf("ordinary write downgraded batch metadata: %#v found=%v err=%v", meta, found, err)
			}
		})
	}
}

func TestMailboxBatchFailureBeforeJournalKeepsWaitingState(t *testing.T) {
	_, workspace, _, _ := outputTestFixture(t, nil)
	message := acceptBatchQueued(t, workspace, "project1.task1", "still waiting", "project1.task42", "ws-a")
	store, err := loadResourceMailboxStoreForRead(workspace.Path, "project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	hot, receipts, outbox, scheduler := prepareResourceMailboxDocuments(store)
	err = writeResourceMailboxStoreDocuments(store.Directory, mailboxStoreMeta(store), hot, receipts, outbox, scheduler, resourceMailboxDocument{
		path: mailboxOperationPath(store.Directory, "not-committed"), value: make(chan string),
	})
	if err == nil {
		t.Fatal("unsupported document did not stop before journal")
	}
	if _, err := os.Stat(resourceMailboxTxnPath(store.Directory)); !os.IsNotExist(err) {
		t.Fatalf("precommit failure wrote a journal: %v", err)
	}
	queued, found, err := mailboxMessageByID(workspace.Path, message.ID)
	if err != nil || !found || queued.Status != resourceMessageQueued || queued.Text != message.Text {
		t.Fatalf("precommit failure changed queue: %#v found=%v err=%v", queued, found, err)
	}
	if _, found, err := readMailboxOperation(store.Directory, "not-committed"); err != nil || found {
		t.Fatalf("precommit operation exists: found=%v err=%v", found, err)
	}
}

func TestMailboxBatchHundredReportsTenSourceTurnsModel(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	for source := 0; source < 10; source++ {
		for report := 0; report < 10; report++ {
			acceptBatchQueued(t, workspace, "project1.task1", fmt.Sprintf("report-%d-%d", source, report), fmt.Sprintf("project1.task%d", source+10), "ws-source")
		}
	}
	turns, handled := 0, 0
	for iteration := 0; iteration < 11; iteration++ {
		reconcileWorkflow(t, manager, workspace, client)
		file := workFile(t, workspace)
		var active *turnWork
		for index := range file.Works {
			if file.Works[index].State == "running" && !file.Works[index].OutputDone {
				active = &file.Works[index]
				break
			}
		}
		if active == nil {
			break
		}
		turns++
		opener, found, err := mailboxMessageByID(workspace.Path, active.ID)
		if err != nil || !found || opener.Sender == nil {
			t.Fatalf("opener missing: %#v err=%v", opener, err)
		}
		page := batchList(t, workspace, "project1.task1", "fromResource="+opener.Sender.ID+"&fromWorkspaceInstanceId=ws-source&limit=100")
		ids := []string{}
		for _, item := range page.Items {
			if item.CanAck {
				ids = append(ids, item.MessageID)
			}
		}
		if len(ids) != 9 {
			t.Fatalf("expected nine sibling reports: %#v", page)
		}
		if _, err := manager.readBatch(context.Background(), workspace, "project1.task1", mailboxReadRequest{WorkID: active.ID, IDs: ids}); err != nil {
			t.Fatal(err)
		}
		result, err := manager.ackBatch(context.Background(), workspace, "project1.task1", mailboxAckRequest{
			OperationID: fmt.Sprintf("model-%d", turns), WorkID: active.ID, IDs: ids, Summary: "all ten source reports checked",
		})
		if err != nil {
			t.Fatal(err)
		}
		handled += result.Handled
		closeWorkflowTurn(fake, *active, "completed", "done")
	}
	actual := 0
	fake.mu.Lock()
	for _, id := range fake.messageIDs {
		if !strings.HasPrefix(id, "preview-") && !strings.HasPrefix(id, "advisory-") {
			actual++
		}
	}
	fake.mu.Unlock()
	if turns != 10 || handled != 90 || actual != 10 {
		t.Fatalf("batch model mismatch: turns=%d providerInputs=%d handled=%d", turns, actual, handled)
	}
	t.Logf("reports=100 sourceGroups=10 batchedTurns=%d handledByBatch=%d providerBusinessInputs=%d independentTurnStartsWithoutBatch=%d",
		turns, handled, actual, turns+handled)
}
