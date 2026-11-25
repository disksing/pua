package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func startChatInputTestLoop(t *testing.T, manager *agentManager) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	manager.runBackground(func() { manager.runChatInputLoop(ctx) })
	t.Cleanup(func() {
		cancel()
		manager.waitBackground()
	})
	return cancel
}

func inputTestMailbox(t *testing.T, workspace serveWorkspace, resource string) resourceMailbox {
	t.Helper()
	mailbox, err := loadResourceMailboxForResource(workspace.Path, resource)
	if err != nil {
		t.Fatal(err)
	}
	return mailbox
}

func inputTestDelivered(t *testing.T, workspace serveWorkspace, resource, input string) bool {
	t.Helper()
	message, found, err := mailboxMessageByID(workspace.Path, "chat-"+chatID(sharedChatID(resource), "input-"+input))
	if err != nil {
		t.Fatal(err)
	}
	return found && message.Status == resourceMessageDelivered
}

func TestChatPageProjectsDurableQueuedMessagesOutsideCursorPage(t *testing.T) {
	manager, workspace, _, _ := outputTestFixture(t, nil)
	const resource = "project1.task1"
	read := func(expected ...string) {
		t.Helper()
		path := "/api/workspaces/" + workspace.ID + "/resources/" + resource + "/chat?limit=1"
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set(workspaceUserHeader, "reader")
		response := httptest.NewRecorder()
		manager.server.handleWorkspace(response, request)
		var page publicChat
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil ||
			len(page.Messages) != 1 || page.QueuedMessages == nil || len(*page.QueuedMessages) != len(expected) {
			t.Fatalf("unexpected queue projection: %d %s", response.Code, response.Body)
		}
		for index, message := range *page.QueuedMessages {
			if message.Text != expected[index] || len(message.InputMessageIDs) != 1 {
				t.Fatalf("queued message %d: %+v", index, message)
			}
		}
	}
	first, second := "First [attachment](/project1/task1/files/first.md)", "Second"
	mustChatOK(t, chatCall(t, manager, workspace, resource, "reader", "messages", chatRequest{ID: "queue-first", Text: first}, false))
	mustChatOK(t, chatCall(t, manager, workspace, resource, "reader", "messages", chatRequest{ID: "queue-second", Text: second}, false))
	read(first, second)
	if _, err := manager.reconcileChatOutbox(context.Background(), workspace, sharedChatID(resource), map[string]bool{"reader": true}); err != nil {
		t.Fatal(err)
	}
	read(first, second)
	firstID := "chat-" + chatID(sharedChatID(resource), "input-queue-first")
	if _, err := mutateResourceMailboxForResource(workspace.Path, resource, func(mailbox *resourceMailbox) error {
		for index := range mailbox.Messages {
			if mailbox.Messages[index].ID == firstID {
				mailbox.Messages[index].Status = resourceMessageDelivered
				mailbox.Messages[index].TerminalAt = time.Now().UTC().Format(time.RFC3339Nano)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	read(second)
	mustChatOK(t, chatCall(t, manager, workspace, resource, "reader", "close", chatRequest{}, false))
	read()
}

func TestChatInputTransfersWhileColdAuditIsBlocked(t *testing.T) {
	var blockNextList atomic.Bool
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	manager, workspace, _, _ := outputTestFixture(t, func(writer http.ResponseWriter, request *http.Request) bool {
		if request.Method == http.MethodGet && request.URL.Path == "/v1/sessions" && blockNextList.CompareAndSwap(true, false) {
			close(reached)
			<-release
		}
		return false
	})
	manager.activePollInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		releaseOnce.Do(func() { close(release) })
		manager.waitBackground()
	})
	blockNextList.Store(true)
	manager.startAgentHubPoller(ctx)
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("cold audit did not reach the barrier")
	}
	barrier := time.NewTimer(30 * time.Second)
	defer barrier.Stop()
	started := time.Now()
	mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "messages", chatRequest{ID: "during-audit", Text: "Transfer while audit waits"}, false))
	waitForRuntimeTest(t, func() bool {
		return inputTestDelivered(t, workspace, "project1.task1", "during-audit")
	})
	if elapsed := time.Since(started); elapsed >= 5*time.Second {
		t.Fatalf("independent input took %s", elapsed)
	}
	<-barrier.C
	if mailbox := inputTestMailbox(t, workspace, "project1.task1"); len(mailbox.Messages) != 1 {
		t.Fatalf("input missing or duplicated while audit remained blocked: %+v", mailbox)
	}
}

func TestChatInputDeliversDuringArchiveGit(t *testing.T) {
	manager, workspace, _, _ := outputTestFixture(t, nil)
	root, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	task, err := root.CreateTask(app.CreateTaskInput{ProjectID: "project1", Title: "Archive"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root.Root(), task.Path, "worktree", "repo", ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	t.Setenv("PUA_TEST_GIT_DIR", bin)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(`#!/bin/sh
case " $* " in
  *" status "*)
    touch "$PUA_TEST_GIT_DIR/started"
    while [ ! -f "$PUA_TEST_GIT_DIR/release" ]; do sleep 0.05; done
    ;;
esac
`), 0755); err != nil {
		t.Fatal(err)
	}
	archived := make(chan error, 1)
	go func() {
		_, err := root.ArchiveResource(task.ID)
		archived <- err
	}()
	startChatInputTestLoop(t, manager)
	t.Cleanup(func() {
		if err := os.WriteFile(filepath.Join(bin, "release"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := <-archived; err != nil {
			t.Error(err)
		}
	})
	waitForRuntimeTest(t, func() bool {
		_, err := os.Stat(filepath.Join(bin, "started"))
		return err == nil
	})
	mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "messages", chatRequest{ID: "archive", Text: "While archive runs"}, false))
	waitForRuntimeTest(t, func() bool { return inputTestDelivered(t, workspace, "project1.task1", "archive") })
}

func TestMailboxWorkspaceLockDeadlineAndRetry(t *testing.T) {
	manager, workspace, fake, _ := outputTestFixture(t, nil)
	message := queueWorkflowUser(t, manager, workspace, "lock", "Wait for workspace")
	if _, _, _, err := manager.ensureMailboxGeneration(context.Background(), workspace, message.ResourceID); err != nil {
		t.Fatal(err)
	}
	root, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- root.WithResourceDocumentMutation(func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- manager.reconcileResourceMailboxLocked(ctx, workspace, message.ResourceID) }()
	select {
	case err = <-done:
	case <-time.After(4 * time.Second):
		close(release)
		<-lockDone
		<-done
		t.Fatal("delivery ignored its deadline while waiting for the workspace lock")
	}
	close(release)
	if lockErr := <-lockDone; lockErr != nil {
		t.Fatal(lockErr)
	}
	current, _, readErr := mailboxMessageByID(workspace.Path, message.ID)
	if !errors.Is(err, context.DeadlineExceeded) || readErr != nil || current.LastErrorCode != "deadline_exceeded" ||
		!strings.Contains(current.LastError, "application.lock") || current.Status != resourceMessageQueued || current.AttemptCount != 0 {
		t.Fatalf("unexpected delivery after lock timeout: %+v, %v, %v", current, err, readErr)
	}
	startChatInputTestLoop(t, manager)
	waitForRuntimeTest(t, func() bool { return inputTestDelivered(t, workspace, message.ResourceID, "lock") })
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.messageIDs) != 1 || fake.messageIDs[0] != message.ID {
		t.Fatalf("unexpected delivered IDs: %v", fake.messageIDs)
	}
}

func TestChatMailboxDeadlineIncludesControllerQueue(t *testing.T) {
	manager, workspace, _, _ := outputTestFixture(t, nil)
	message := queueWorkflowUser(t, manager, workspace, "queue-budget", "Wait for controller")
	controller, err := manager.controllerForResource(workspace, message.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	blocked := controller.enqueue(context.Background(), func() error { <-release; return nil })
	defer func() { close(release); <-blocked }()
	key := chatInputKey{root: workspace.Path, resource: message.ResourceID, stage: chatInputMailbox}
	manager.enqueueChatInput(context.Background(), key, workspace, time.Now().Add(100*time.Millisecond-agentHubRequestTimeout))
	waitForRuntimeTest(t, func() bool {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		return len(controller.jobs) == 0
	})
	if len(workFile(t, workspace).Works) != 0 {
		t.Fatal("expired queued job created work")
	}
}

func TestChatInputIsolatesResourcesAndWorkspacesAndCoalesces(t *testing.T) {
	manager, workspace, fake, client := outputTestFixture(t, nil)
	ws, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.CreateTask(app.CreateTaskInput{ProjectID: "project1", Title: "Independent task", Slug: "independent"}); err != nil {
		t.Fatal(err)
	}
	mustChatOK(t, chatCall(t, manager, workspace, "project1.task2", "reader", "open", chatRequest{}, false))
	otherManager, other, _ := newRuntimeTestManager(t, client.endpoint)
	other.ID = "workspace-other"
	otherWS, _ := app.OpenWorkspace(other.Path)
	if _, err := otherWS.RegisterUser("reader"); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := manager.agentHubRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspaces = append(cfg.Workspaces, other)
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.server.config, data, 0600); err != nil {
		t.Fatal(err)
	}
	_ = otherManager
	mustChatOK(t, chatCall(t, manager, other, "project1.task1", "reader", "open", chatRequest{}, false))
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	controller, err := manager.controllerForResource(workspace, "project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	blocked := controller.enqueue(context.Background(), func() error {
		close(reached)
		<-release
		return nil
	})
	<-reached
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); <-blocked })
	startChatInputTestLoop(t, manager)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "messages", chatRequest{ID: "first", Text: "First"}, false))
	waitForRuntimeTest(t, func() bool {
		return len(inputTestMailbox(t, workspace, "project1.task1").Messages) == 1
	})
	mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "messages", chatRequest{ID: "second", Text: "Second"}, false))
	mustChatOK(t, chatCall(t, manager, workspace, "project1.task2", "reader", "messages", chatRequest{ID: "same-workspace", Text: "Independent"}, false))
	mustChatOK(t, chatCall(t, manager, other, "project1.task1", "reader", "messages", chatRequest{ID: "other-workspace", Text: "Independent workspace"}, false))
	for range 100 {
		manager.requestChatInput(workspace, "project1.task1", chatInputTransfer)
		manager.requestChatInput(workspace, "project1.task1", chatInputMailbox)
	}
	waitForRuntimeTest(t, func() bool {
		return inputTestDelivered(t, workspace, "project1.task2", "same-workspace") &&
			inputTestDelivered(t, other, "project1.task1", "other-workspace") &&
			len(inputTestMailbox(t, workspace, "project1.task1").Messages) == 2
	})
	controller.mu.Lock()
	queued := len(controller.jobs)
	controller.mu.Unlock()
	if queued != 1 {
		t.Fatalf("duplicate controller jobs: %d", queued)
	}
	mailbox := inputTestMailbox(t, workspace, "project1.task1")
	if mailbox.Messages[0].Sequence >= mailbox.Messages[1].Sequence || mailbox.Messages[0].ID != "chat-"+chatID(sharedChatID("project1.task1"), "input-first") {
		t.Fatalf("input FIFO or stable ID changed: %+v", mailbox.Messages)
	}
	releaseOnce.Do(func() { close(release) })
	waitForRuntimeTest(t, func() bool { return inputTestDelivered(t, workspace, "project1.task1", "first") })
	secondID := "chat-" + chatID(sharedChatID("project1.task1"), "input-second")
	waitForRuntimeTest(t, func() bool {
		message, _, _ := mailboxMessageByID(workspace.Path, secondID)
		return message.Status == resourceMessageQueued && message.ActualMode == resourceMessageModeEnqueue
	})
	if observed := workFile(t, workspace); len(observed.Works) != 1 || !observed.Works[0].Context.ChatVisible {
		t.Fatalf("fixed audience changed: %+v", observed)
	}
	closeWorkflowTurn(fake, workFile(t, workspace).Works[0], "completed", "First done")
	manager.requestChatInput(workspace, "project1.task1", chatInputMailbox)
	waitForRuntimeTest(t, func() bool { return inputTestDelivered(t, workspace, "project1.task1", "second") })
	fake.mu.Lock()
	var delivered []string
	for _, id := range fake.messageIDs {
		if id == mailbox.Messages[0].ID || id == secondID {
			delivered = append(delivered, id)
		}
	}
	fake.mu.Unlock()
	if len(delivered) != 2 || delivered[0] != mailbox.Messages[0].ID || delivered[1] != secondID {
		t.Fatalf("provider input repeated or reordered: %v", delivered)
	}
}

func TestChatInputRecoversCrashWindows(t *testing.T) {
	for _, window := range []string{"outbox-saved", "mailbox-accepted", "accepted-flag-saved"} {
		t.Run(window, func(t *testing.T) {
			manager, workspace, fake, _ := outputTestFixture(t, nil)
			mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "messages", chatRequest{ID: "recover", Text: "Recover exactly once"}, false))
			if window != "outbox-saved" {
				if _, err := manager.reconcileChatOutbox(context.Background(), workspace, sharedChatID("project1.task1"), map[string]bool{"reader": true}); err != nil {
					t.Fatal(err)
				}
				if window == "mailbox-accepted" {
					chatMu.Lock()
					chat, err := loadChatLocked(workspace.Path, sharedChatID("project1.task1"))
					if err == nil {
						chat.Outbox[0].Accepted = false
						err = saveChatLocked(workspace.Path, chat)
					}
					chatMu.Unlock()
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			manager.waitBackground()
			manager = newAgentManager(manager.server)
			manager.server.agents = manager
			broken := chatPath(workspace.Path, "00000000000000000000000000000000")
			if err := os.WriteFile(broken, []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := manager.recoverChatInputs(context.Background()); err == nil {
				t.Fatal("malformed record was not reported")
			}
			if runtimeFakeListCalls(fake) != 0 {
				t.Fatal("durable input recovery queried AgentHub")
			}
			startChatInputTestLoop(t, manager)
			waitForRuntimeTest(t, func() bool { return inputTestDelivered(t, workspace, "project1.task1", "recover") })
			if mailbox := inputTestMailbox(t, workspace, "project1.task1"); len(mailbox.Messages) != 1 {
				t.Fatalf("recovery duplicated mailbox input: %+v", mailbox)
			}
			if err := manager.recoverChatInputs(context.Background()); err == nil {
				t.Fatal("malformed record was not reported on replay")
			}
			fake.mu.Lock()
			count := len(fake.messageIDs)
			fake.mu.Unlock()
			if count != 1 {
				t.Fatalf("recovery sent %d provider inputs", count)
			}
		})
	}
}

func TestChatInputRetriesPersistenceFailure(t *testing.T) {
	manager, workspace, _, _ := outputTestFixture(t, nil)
	mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "messages", chatRequest{ID: "retry", Text: "Persist after repair"}, false))
	directory, _, _, err := resourceMailboxDirectory(workspace.Path, "project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(directory), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(directory, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	manager.activePollInterval = 10 * time.Millisecond
	startChatInputTestLoop(t, manager)
	waitForRuntimeTest(t, func() bool {
		manager.chatInputMu.Lock()
		defer manager.chatInputMu.Unlock()
		target := manager.chatInputTargets[chatInputKey{root: workspace.Path, resource: "project1.task1", stage: chatInputTransfer}]
		return target != nil && !target.nextAt.IsZero()
	})
	chat, err := loadDialogue(workspace.Path, dialoguePrefix+sharedChatID("project1.task1"))
	if err != nil || chat.Outbox[0].Accepted {
		t.Fatalf("failed persistence marked accepted: %+v, %v", chat, err)
	}
	if err := os.Remove(directory); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool { return inputTestDelivered(t, workspace, "project1.task1", "retry") })
}

func TestChatInputPreservesClosedEpochAndArchive(t *testing.T) {
	for _, scenario := range []string{"closed", "reopened", "archived"} {
		t.Run(scenario, func(t *testing.T) {
			manager, workspace, _, _ := outputTestFixture(t, nil)
			mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "messages", chatRequest{ID: "old", Text: "Old epoch"}, false))
			if scenario == "archived" {
				ws, _ := app.OpenWorkspace(workspace.Path)
				if _, err := ws.ArchiveResource("project1.task1"); err != nil {
					t.Fatal(err)
				}
			} else {
				mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "close", chatRequest{}, false))
				if scenario == "reopened" {
					mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "open", chatRequest{}, false))
					mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "messages", chatRequest{ID: "new", Text: "New epoch"}, false))
				}
			}
			startChatInputTestLoop(t, manager)
			waitForRuntimeTest(t, func() bool {
				chat, err := loadDialogue(workspace.Path, dialoguePrefix+sharedChatID("project1.task1"))
				return err == nil && chat.Outbox[0].Accepted
			})
			mailbox := inputTestMailbox(t, workspace, "project1.task1")
			if scenario == "reopened" {
				waitForRuntimeTest(t, func() bool { return inputTestDelivered(t, workspace, "project1.task1", "new") })
				if len(mailbox.Messages) != 1 || mailbox.Messages[0].ID != "chat-"+chatID(sharedChatID("project1.task1"), "input-new") {
					t.Fatalf("old epoch leaked: %+v", mailbox)
				}
			} else if len(mailbox.Messages) != 0 {
				t.Fatalf("closed or archived input delivered: %+v", mailbox)
			}
		})
	}
}

func TestChatInputCancellationWaitsForControllerCompletion(t *testing.T) {
	manager, workspace, _, _ := outputTestFixture(t, nil)
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	controller, err := manager.controllerForResource(workspace, "project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	blocked := controller.enqueue(context.Background(), func() error {
		close(reached)
		<-release
		return nil
	})
	<-reached
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); <-blocked })
	ctx, cancel := context.WithCancel(context.Background())
	key := chatInputKey{root: workspace.Path, resource: "project1.task1", stage: chatInputMailbox}
	manager.requestChatInput(workspace, key.resource, key.stage)
	manager.enqueueChatInput(ctx, key, workspace, time.Now())
	cancel()
	shutdown := make(chan struct{})
	go func() {
		manager.waitBackground()
		close(shutdown)
	}()
	select {
	case <-shutdown:
		t.Fatal("shutdown returned while a controller job remained queued")
	case <-time.After(20 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-shutdown:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled controller did not finish")
	}
	manager.chatInputMu.Lock()
	remaining := len(manager.chatInputTargets)
	manager.chatInputMu.Unlock()
	if remaining != 1 {
		t.Fatalf("cancelled mailbox target was not removed: %d targets", remaining)
	}
	if len(workFile(t, workspace).Works) != 0 {
		t.Fatal("cancelled queued input executed")
	}
}

func TestChatInputCancellationStopsRunningRequest(t *testing.T) {
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	manager, workspace, _, _ := outputTestFixture(t, func(writer http.ResponseWriter, request *http.Request) bool {
		if request.URL.Path == "/v1/agents" {
			select {
			case <-reached:
			default:
				close(reached)
			}
			select {
			case <-request.Context().Done():
			case <-release:
			}
			return true
		}
		return false
	})
	mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "messages", chatRequest{ID: "cancel", Text: "Cancel ongoing request"}, false))
	if err := manager.reconcileWorkspaceChats(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	cancel := startChatInputTestLoop(t, manager)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("input did not reach AgentHub")
	}
	cancel()
	shutdown := make(chan struct{})
	go func() {
		manager.waitBackground()
		close(shutdown)
	}()
	select {
	case <-shutdown:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel ongoing AgentHub request")
	}
}

func TestChatInputDoesNotReplayUnknownDelivery(t *testing.T) {
	manager, workspace, fake, _ := outputTestFixture(t, nil)
	message := queueWorkflowUser(t, manager, workspace, "unknown", "Outcome is unknown")
	if _, err := updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
		current.Status = resourceMessageDelivering
		current.GenerationID = "retired-generation"
	}); err != nil {
		t.Fatal(err)
	}
	startChatInputTestLoop(t, manager)
	waitForRuntimeTest(t, func() bool {
		current, found, _ := mailboxMessageByID(workspace.Path, message.ID)
		return found && current.Status == resourceMessageDeliveryUnknown
	})
	manager.requestChatInput(workspace, "project1.task1", chatInputMailbox)
	controller, err := manager.controllerForResource(workspace, "project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.do(context.Background(), func() error {
		return manager.reconcileResourceMailboxLocked(context.Background(), workspace, "project1.task1")
	}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	sent := len(fake.messageIDs)
	fake.mu.Unlock()
	if sent != 0 {
		t.Fatalf("unknown delivery blindly resent %d inputs", sent)
	}
}

func TestTelegramChatInputWakesIndependentTransfer(t *testing.T) {
	runtime, _, state := telegramFixture(t)
	route := telegramOpen(t, runtime, &state, "project1.task1")
	startChatInputTestLoop(t, runtime.server.agents)
	telegramProcess(t, runtime, &state, telegramInput(2, 777, route.ThreadID, "Independent Telegram transfer"))
	waitForRuntimeTest(t, func() bool {
		mailbox := inputTestMailbox(t, runtime.workspace, "project1.task1")
		return len(mailbox.Messages) == 1
	})
}

func TestReconcileErrorCode(t *testing.T) {
	for _, test := range []struct {
		err  error
		code string
	}{
		{nil, "none"},
		{context.Canceled, "cancelled"},
		{context.DeadlineExceeded, "deadline_exceeded"},
		{&agentHubAPIError{Code: "session_busy"}, "session_busy"},
		{errors.New("private body"), "temporarily_undeliverable"},
	} {
		if code := reconcileErrorCode(test.err); code != test.code {
			t.Fatalf("code=%q, want %q", code, test.code)
		}
	}
}

func TestAgentHubInputTimingOmitsPrivateData(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("fail") == "1" {
			writer.WriteHeader(http.StatusConflict)
			writeRuntimeFakeJSON(writer, map[string]any{"error": map[string]any{"code": "session_busy", "message": "response-secret"}})
			return
		}
		writeRuntimeFakeJSON(writer, map[string]any{})
	}))
	defer hub.Close()
	client, err := newAgentHubClient(hub.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	var captured bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&captured)
	defer log.SetOutput(previous)
	for _, query := range []string{"?private=query-secret", "?fail=1&private=query-secret"} {
		var response map[string]any
		err := client.doJSON(context.Background(), http.MethodPost, "/v1/sessions/timing/messages"+query,
			agentHubInboundMessage{MessageID: "stable-input-id", Text: "body-secret", Payload: json.RawMessage(`{"token":"token-secret"}`)}, &response)
		if (err != nil) != strings.Contains(query, "fail=1") {
			t.Fatalf("unexpected request result: %v", err)
		}
	}
	recorded := captured.String()
	for _, required := range []string{"message=stable-input-id", "started_at=", "duration_ms=", "status=200 error_code=none", "status=409 error_code=session_busy"} {
		if !strings.Contains(recorded, required) {
			t.Fatalf("missing timing field %q: %s", required, recorded)
		}
	}
	for _, secret := range []string{"query-secret", "body-secret", "response-secret", "token-secret"} {
		if strings.Contains(recorded, secret) {
			t.Fatalf("timing log exposed %s", secret)
		}
	}
}
