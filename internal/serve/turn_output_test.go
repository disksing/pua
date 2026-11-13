package serve

import (
	"context"
	"encoding/json"
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

func outputTestFixture(t *testing.T, intercept func(http.ResponseWriter, *http.Request) bool) (*agentManager, serveWorkspace, *runtimeFakeAgentHub, *agentHubClient) {
	t.Helper()
	fake := newRuntimeFakeAgentHub()
	fake.enforceMessageIDs, fake.materializeTurns = true, true
	hub := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if intercept != nil && intercept(writer, request) {
			return
		}
		fake.ServeHTTP(writer, request)
	}))
	t.Cleanup(hub.Close)
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	manager.now = time.Now
	ws, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ws.RegisterUser("reader"); err != nil {
		t.Fatal(err)
	}
	mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "open", chatRequest{}, false))
	_, client, err := manager.agentHubRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	return manager, workspace, fake, client
}

func startOutputTestLoop(t *testing.T, manager *agentManager) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	manager.runBackground(func() { manager.runTurnOutputLoop(ctx) })
	t.Cleanup(func() {
		cancel()
		manager.waitBackground()
	})
	return cancel
}

func outputTestDone(t *testing.T, workspace serveWorkspace, resource, work string) bool {
	t.Helper()
	file, err := loadTurnWork(workspace.Path, resource)
	if err != nil {
		t.Fatal(err)
	}
	return file.find(work) != nil && file.find(work).OutputDone
}

func TestTurnOutputPublishesWhileColdAuditIsBlocked(t *testing.T) {
	var blockNextList atomic.Bool
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	manager, workspace, fake, client := outputTestFixture(t, func(writer http.ResponseWriter, request *http.Request) bool {
		if request.URL.Path == "/v1/sessions" && request.Method == http.MethodGet && blockNextList.CompareAndSwap(true, false) {
			close(reached)
			<-release
		}
		return false
	})
	message := queueWorkflowUser(t, manager, workspace, "cold-output", "Do some work")
	reconcileWorkflow(t, manager, workspace, client)
	work := *workFile(t, workspace).find(message.ID)
	manager.activePollInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		cancel()
		manager.waitBackground()
	})
	blockNextList.Store(true)
	manager.startAgentHubPoller(ctx)
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("cold audit did not reach the barrier")
	}
	closeWorkflowTurn(fake, work, "completed", "Published without waiting for the audit")
	manager.requestTurnOutput(workspace, "project1.task1")
	waitForRuntimeTest(t, func() bool {
		return outputTestDone(t, workspace, "project1.task1", message.ID)
	})
	chat, err := loadDialogue(workspace.Path, dialoguePrefix+sharedChatID("project1.task1"))
	if err != nil || len(chat.Messages) != 2 || chat.Messages[1].Text != "Published without waiting for the audit" {
		t.Fatalf("final reply missing or duplicated: %+v, %v", chat.Messages, err)
	}
	if observed := workFile(t, workspace).find(message.ID); observed.TerminalObservedAt == "" || observed.LastPublishedAt == "" {
		t.Fatalf("missing output timing: %+v", observed)
	}
}

func TestTurnOutputSlowResourceDoesNotBlockAnotherAndRequestsCoalesce(t *testing.T) {
	manager, workspace, fake, client := outputTestFixture(t, nil)
	first := queueWorkflowUser(t, manager, workspace, "slow-output", "First request")
	reconcileWorkflow(t, manager, workspace, client)
	firstWork := *workFile(t, workspace).find(first.ID)
	ws, _ := app.OpenWorkspace(workspace.Path)
	if _, err := ws.CreateTask(app.CreateTaskInput{ProjectID: "project1", Title: "Another task", Slug: "another"}); err != nil {
		t.Fatal(err)
	}
	resource := "project1.task2"
	mustChatOK(t, chatCall(t, manager, workspace, resource, "reader", "open", chatRequest{}, false))
	mustChatOK(t, chatCall(t, manager, workspace, resource, "reader", "messages", chatRequest{ID: "independent", Text: "Second request"}, false))
	if err := manager.reconcileWorkspaceChats(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	if err := manager.withResourceController(context.Background(), workspace, resource, func() error {
		return manager.reconcileResourceMailboxLocked(context.Background(), workspace, resource)
	}); err != nil {
		t.Fatal(err)
	}
	secondFile, err := loadTurnWork(workspace.Path, resource)
	if err != nil || len(secondFile.Works) != 1 {
		t.Fatalf("second work missing: %+v, %v", secondFile, err)
	}
	second := secondFile.Works[0]
	closeWorkflowTurn(fake, firstWork, "completed", "Slow resource reply")
	closeWorkflowTurn(fake, second, "completed", "Independent reply")
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	controller, err := manager.controllerForResource(workspace, "project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	controller.enqueue(context.Background(), func() error {
		close(reached)
		<-release
		return nil
	})
	<-reached
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	manager.activePollInterval = 10 * time.Millisecond
	startOutputTestLoop(t, manager)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	for range 100 {
		manager.requestTurnOutput(workspace, "project1.task1")
	}
	waitForRuntimeTest(t, func() bool { return outputTestDone(t, workspace, resource, second.ID) })
	if outputTestDone(t, workspace, "project1.task1", first.ID) {
		t.Fatal("blocked resource unexpectedly completed")
	}
	controller.mu.Lock()
	queued := len(controller.jobs)
	controller.mu.Unlock()
	if queued != 1 {
		t.Fatalf("repeated output requests queued %d jobs, want one", queued)
	}
	releaseOnce.Do(func() { close(release) })
	waitForRuntimeTest(t, func() bool { return outputTestDone(t, workspace, "project1.task1", first.ID) })
	chat, err := loadDialogue(workspace.Path, dialoguePrefix+sharedChatID("project1.task1"))
	if err != nil || len(chat.Messages) != 2 {
		t.Fatalf("coalesced requests duplicated output: %+v, %v", chat.Messages, err)
	}
}

func TestTurnOutputRestartRecoversWithoutRuntimeOrColdAudit(t *testing.T) {
	manager, workspace, fake, client := outputTestFixture(t, nil)
	message := queueWorkflowUser(t, manager, workspace, "restart-output", "Do some work")
	reconcileWorkflow(t, manager, workspace, client)
	work := *workFile(t, workspace).find(message.ID)
	closeWorkflowTurn(fake, work, "completed", "Recovered after restart")
	manager.waitBackground()
	manager = newAgentManager(manager.server)
	manager.server.agents = manager
	badPath := filepath.Join(filepath.Dir(turnWorkPath(workspace.Path, "project1.task1")), "000-broken.json")
	if err := os.WriteFile(badPath, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := manager.recoverTurnOutputTargets(context.Background()); err == nil {
		t.Fatal("invalid record was not reported")
	}
	listCalls := runtimeFakeListCalls(fake)
	startOutputTestLoop(t, manager)
	waitForRuntimeTest(t, func() bool { return outputTestDone(t, workspace, "project1.task1", message.ID) })
	if len(manager.runtimeSnapshots()) != 0 || runtimeFakeListCalls(fake) != listCalls {
		t.Fatal("output recovery depended on session-list polling or a runtime")
	}
	chat, err := loadDialogue(workspace.Path, dialoguePrefix+sharedChatID("project1.task1"))
	if err != nil || len(chat.Messages) != 2 || chat.Messages[1].Text != "Recovered after restart" {
		t.Fatalf("recovered reply missing or duplicated: %+v, %v", chat.Messages, err)
	}
}

func TestTurnOutputKeepsChatAudienceAndEpoch(t *testing.T) {
	for _, scenario := range []string{"internal", "closed", "reopened", "archived"} {
		t.Run(scenario, func(t *testing.T) {
			manager, workspace, fake, client := outputTestFixture(t, nil)
			var work turnWork
			if scenario == "internal" {
				message := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "Internal request", resourceMessageModeEnqueue, nil)
				work = *workFile(t, workspace).find(message.ID)
			} else {
				message := queueWorkflowUser(t, manager, workspace, "audience-output", "User request")
				reconcileWorkflow(t, manager, workspace, client)
				work = *workFile(t, workspace).find(message.ID)
			}
			closeWorkflowTurn(fake, work, "completed", "Must stay in execution history")
			switch scenario {
			case "closed", "reopened":
				mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "close", chatRequest{}, false))
				if scenario == "reopened" {
					mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "open", chatRequest{}, false))
				}
			case "archived":
				ws, _ := app.OpenWorkspace(workspace.Path)
				if _, err := ws.ArchiveResource("project1.task1"); err != nil {
					t.Fatal(err)
				}
			}
			startOutputTestLoop(t, manager)
			waitForRuntimeTest(t, func() bool { return outputTestDone(t, workspace, "project1.task1", work.ID) })
			chat, err := loadDialogue(workspace.Path, dialoguePrefix+sharedChatID("project1.task1"))
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range chat.Messages {
				if message.Role == "agent" {
					t.Fatalf("output crossed audience or epoch: %+v", message)
				}
			}
		})
	}
}

func TestTurnOutputCompletionWakesStableLoopAndSealsProgress(t *testing.T) {
	manager, workspace, fake, client := outputTestFixture(t, nil)
	message := queueWorkflowUser(t, manager, workspace, "terminal-wake", "Do some work")
	reconcileWorkflow(t, manager, workspace, client)
	work := *workFile(t, workspace).find(message.ID)
	fake.mu.Lock()
	turn := fake.turns[work.SessionID][work.TurnID]
	turn.Items = append(turn.Items,
		agentHubTurnItem{Type: "activity", Text: "Private reasoning"},
		agentHubTurnItem{Type: "message", Role: "assistant", Text: "Progress", StartEventID: 5, Complete: true},
		agentHubTurnItem{Type: "message", Role: "assistant", Text: "Unsealed final", StartEventID: 10})
	fake.turns[work.SessionID][work.TurnID] = turn
	fake.mu.Unlock()
	manager.activePollInterval = time.Hour
	startOutputTestLoop(t, manager)
	waitForRuntimeTest(t, func() bool {
		chat, err := loadDialogue(workspace.Path, dialoguePrefix+sharedChatID("project1.task1"))
		return err == nil && len(chat.Messages) == 2
	})
	chat, _ := loadDialogue(workspace.Path, dialoguePrefix+sharedChatID("project1.task1"))
	if chat.Messages[1].Text != "Progress" {
		t.Fatalf("unsealed or private output was published: %+v", chat.Messages)
	}
	fake.mu.Lock()
	turn = fake.turns[work.SessionID][work.TurnID]
	turn.Closed, turn.Status = true, "completed"
	turn.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	turn.Items[len(turn.Items)-1].Text = "Sealed final"
	fake.turns[work.SessionID][work.TurnID] = turn
	session := fake.sessions[work.SessionID]
	session.State, session.CurrentTurnID, session.LastEventID = "ready", "", 12
	fake.sessions[work.SessionID] = session
	fake.mu.Unlock()
	var runtime *agentRuntime
	for _, candidate := range manager.runtimeSnapshots() {
		if candidate.snapshotGeneration().AgentHubSessionID == work.SessionID {
			runtime = candidate
		}
	}
	if runtime == nil {
		t.Fatal("runtime missing")
	}
	if err := manager.withResourceController(context.Background(), workspace, "project1.task1", func() error {
		runtime.recordTurnCompletionHistory(session, []agentHubEvent{
			{ID: 11, Type: "message.assistant.delta", SessionID: work.SessionID, TurnID: work.TurnID, Data: json.RawMessage(`{"text":"Sealed final"}`)},
			{ID: 12, Type: "turn.completed", SessionID: work.SessionID, TurnID: work.TurnID, Time: turn.CompletedAt},
		}, 12)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeTest(t, func() bool { return outputTestDone(t, workspace, "project1.task1", work.ID) })
	chat, _ = loadDialogue(workspace.Path, dialoguePrefix+sharedChatID("project1.task1"))
	if len(chat.Messages) != 3 || chat.Messages[2].Text != "Sealed final" {
		t.Fatalf("terminal wake lost or duplicated a text block: %+v", chat.Messages)
	}
}

func TestTurnOutputShutdownCancelsQueuedPublication(t *testing.T) {
	manager, workspace, fake, client := outputTestFixture(t, nil)
	message := queueWorkflowUser(t, manager, workspace, "shutdown-output", "Do some work")
	reconcileWorkflow(t, manager, workspace, client)
	work := *workFile(t, workspace).find(message.ID)
	closeWorkflowTurn(fake, work, "completed", "Do not publish after cancellation")
	controller, _ := manager.controllerForResource(workspace, "project1.task1")
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	controller.enqueue(context.Background(), func() error {
		close(reached)
		<-release
		return nil
	})
	<-reached
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	cancel := startOutputTestLoop(t, manager)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	waitForRuntimeTest(t, func() bool {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		return len(controller.jobs) == 1
	})
	cancel()
	releaseOnce.Do(func() { close(release) })
	manager.waitBackground()
	if outputTestDone(t, workspace, "project1.task1", message.ID) {
		t.Fatal("canceled queued output was published")
	}
}

func TestTurnOutputRetriesTransientTurnQueryFailure(t *testing.T) {
	var failNext atomic.Bool
	var queries atomic.Int64
	manager, workspace, fake, client := outputTestFixture(t, func(writer http.ResponseWriter, request *http.Request) bool {
		if strings.Contains(request.URL.Path, "/turns/") {
			queries.Add(1)
			if failNext.CompareAndSwap(true, false) {
				http.Error(writer, "temporary Turn query failure", http.StatusServiceUnavailable)
				return true
			}
		}
		return false
	})
	message := queueWorkflowUser(t, manager, workspace, "retry-output", "Do some work")
	reconcileWorkflow(t, manager, workspace, client)
	work := *workFile(t, workspace).find(message.ID)
	closeWorkflowTurn(fake, work, "completed", "Recovered from a transient failure")
	failNext.Store(true)
	initialQueries := queries.Load()
	manager.activePollInterval = 10 * time.Millisecond
	startOutputTestLoop(t, manager)
	waitForRuntimeTest(t, func() bool { return outputTestDone(t, workspace, "project1.task1", message.ID) })
	if queries.Load()-initialQueries < 2 {
		t.Fatal("transient query failure was not retried")
	}
	observed := workFile(t, workspace).find(message.ID)
	if observed.LastError != "" || len(observed.PublishedIDs) != 1 {
		t.Fatalf("query recovery left an error or duplicated output: %+v", observed)
	}
}
