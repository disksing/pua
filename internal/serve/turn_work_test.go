package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func workflowFixture(t *testing.T) (*agentManager, serveWorkspace, *runtimeFakeAgentHub, *agentHubClient) {
	t.Helper()
	fake := newRuntimeFakeAgentHub()
	fake.enforceMessageIDs, fake.materializeTurns = true, true
	hub := httptest.NewServer(fake)
	t.Cleanup(hub.Close)
	m, w, _ := newRuntimeTestManager(t, hub.URL)
	ws, _ := app.OpenWorkspace(w.Path)
	if _, err := ws.RegisterUser("reader"); err != nil {
		t.Fatal(err)
	}
	mustChatOK(t, chatCall(t, m, w, "project1.task1", "reader", "open", chatRequest{}, false))
	_, client, err := m.agentHubRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	return m, w, fake, client
}

func workFile(t *testing.T, w serveWorkspace) turnWorkFile {
	t.Helper()
	f, err := loadTurnWork(w.Path, "project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func workCommand(t *testing.T, m *agentManager, w serveWorkspace, command turnWorkCommand) {
	t.Helper()
	if err := m.withResourceController(context.Background(), w, "project1.task1", func() error {
		f := workFile(t, w)
		_, err := m.controlTurnWork(context.Background(), w, "project1.task1", &f, command)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func closeWorkflowTurn(fake *runtimeFakeAgentHub, work turnWork, status, text string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	turn := fake.turns[work.SessionID][work.TurnID]
	turn.Closed, turn.Status = true, status
	turn.Items = append(turn.Items, agentHubTurnItem{Type: "message", Role: "assistant", Text: text, StartEventID: 99, Complete: true})
	fake.turns[work.SessionID][work.TurnID] = turn
	session := fake.sessions[work.SessionID]
	session.State, session.CurrentTurnID = "ready", ""
	fake.sessions[work.SessionID] = session
}

func reconcileWorkflow(t *testing.T, m *agentManager, w serveWorkspace, client *agentHubClient) {
	t.Helper()
	if err := m.withResourceController(context.Background(), w, "project1.task1", func() error {
		if err := m.reconcileTurnWorkLocked(context.Background(), w, "project1.task1", client); err != nil {
			return err
		}
		return m.reconcileResourceMailboxLocked(context.Background(), w, "project1.task1")
	}); err != nil {
		t.Fatal(err)
	}
}

func queueWorkflowUser(t *testing.T, m *agentManager, w serveWorkspace, id, text string) resourceMailboxMessage {
	t.Helper()
	mustChatOK(t, chatCall(t, m, w, "project1.task1", "reader", "messages", chatRequest{ID: id, Text: text}, false))
	if err := m.reconcileWorkspaceChats(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	msg, ok, err := mailboxMessageByID(w.Path, "chat-"+chatID(sharedChatID("project1.task1"), "input-"+id))
	if err != nil || !ok {
		t.Fatalf("input not queued: %v", err)
	}
	return msg
}

func TestTurnWorkYieldPreviewResumeAndReplay(t *testing.T) {
	m, w, fake, client := workflowFixture(t)
	original := acceptTestResourceMessage(t, m, w, "project1.task1", "background task", resourceMessageModeSteer,
		&agentHubMessageSender{ID: "project1"})
	a := *workFile(t, w).find(original.ID)
	if a.Context.ChatVisible || a.TurnID == "" {
		t.Fatalf("invalid background work: %+v", a)
	}
	user := queueWorkflowUser(t, m, w, "question", "How far along?")
	reconcileWorkflow(t, m, w, client)
	queued, _, _ := mailboxMessageByID(w.Path, user.ID)
	if queued.Status != resourceMessageQueued || queued.ActualMode != resourceMessageModeEnqueue {
		t.Fatalf("preview consumed input: %+v", queued)
	}
	fake.mu.Lock()
	preview := fake.messageInputs["preview-"+chatID(a.ID, user.ID)]
	fake.mu.Unlock()
	if preview.ExpectedTurnID != a.TurnID || !preview.Steer || !strings.Contains(preview.Text, "still queued") {
		t.Fatalf("preview not scoped: %+v", preview)
	}
	command := turnWorkCommand{Action: "yield", WorkID: a.ID, MessageID: user.ID, Checkpoint: "file changed; test next"}
	workCommand(t, m, w, command)
	workCommand(t, m, w, command)
	reconcileWorkflow(t, m, w, client)
	if workFile(t, w).find(user.ID) != nil {
		t.Fatal("child started before parent terminal")
	}
	closeWorkflowTurn(fake, a, "completed", "internal checkpoint")
	reconcileWorkflow(t, m, w, client)
	b := *workFile(t, w).find(user.ID)
	if !b.Context.ChatVisible || b.ParentID != a.ID || b.Context.ParentHistory == "" {
		t.Fatalf("bad child: %+v", b)
	}
	// Reload the manager to verify durable recovery without in-memory frames.
	m.waitBackground()
	m = newAgentManager(m.server)
	m.server.agents = m
	closeWorkflowTurn(fake, b, "completed", "I have changed the file.")
	reconcileWorkflow(t, m, w, client)
	file := workFile(t, w)
	parent := file.find(a.ID)
	resumed := file.find(parent.ResumeID)
	if parent.State != "resuming" || resumed == nil || resumed.Context.ChatVisible || resumed.Context.OpenerSender.ID != "project1" {
		t.Fatalf("lost original audience: %+v", file)
	}
	chat, _ := loadDialogue(w.Path, dialoguePrefix+sharedChatID("project1.task1"))
	if len(chat.Messages) != 2 || chat.Messages[1].Text != "I have changed the file." {
		t.Fatalf("internal output leaked or reply lost: %+v", chat.Messages)
	}
	reconcileWorkflow(t, m, w, client)
	replayed, _ := loadDialogue(w.Path, dialoguePrefix+sharedChatID("project1.task1"))
	if len(replayed.Messages) != len(chat.Messages) || len(workFile(t, w).Works) != 3 {
		t.Fatal("replay duplicated output or resume")
	}
}

func TestTurnWorkDirectCancellationDoesNotResume(t *testing.T) {
	m, w, fake, client := workflowFixture(t)
	original := acceptTestResourceMessage(t, m, w, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	a := *workFile(t, w).find(original.ID)
	user := queueWorkflowUser(t, m, w, "cancel", "Cancel the background work.")
	reconcileWorkflow(t, m, w, client)
	workCommand(t, m, w, turnWorkCommand{Action: "finish", WorkID: a.ID, Reason: "cancelled"})
	closeWorkflowTurn(fake, a, "completed", "cancelled")
	reconcileWorkflow(t, m, w, client)
	b := *workFile(t, w).find(user.ID)
	if b.Context.PreviousResult == "" || b.ParentID != "" {
		t.Fatalf("cancellation handoff missing: %+v", b)
	}
	closeWorkflowTurn(fake, b, "completed", "Cancelled.")
	reconcileWorkflow(t, m, w, client)
	file := workFile(t, w)
	if len(file.Works) != 2 || file.find(a.ID).State != "cancelled" {
		t.Fatalf("cancelled work resumed: %+v", file)
	}
}

func TestTurnWorkChildCancelsParentAndRejectsNesting(t *testing.T) {
	m, w, fake, client := workflowFixture(t)
	original := acceptTestResourceMessage(t, m, w, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	a := *workFile(t, w).find(original.ID)
	user := queueWorkflowUser(t, m, w, "question", "Discuss the background work")
	workCommand(t, m, w, turnWorkCommand{Action: "yield", WorkID: a.ID, MessageID: user.ID, Checkpoint: "safe"})
	closeWorkflowTurn(fake, a, "completed", "")
	reconcileWorkflow(t, m, w, client)
	b := *workFile(t, w).find(user.ID)
	file := workFile(t, w)
	if _, err := m.controlTurnWork(context.Background(), w, "project1.task1", &file,
		turnWorkCommand{Action: "yield", WorkID: b.ID, MessageID: a.ID, Checkpoint: "nested"}); err == nil {
		t.Fatal("nested yield allowed")
	}
	workCommand(t, m, w, turnWorkCommand{Action: "cancel-resume", WorkID: b.ID, Reason: "user cancellation"})
	closeWorkflowTurn(fake, b, "completed", "Cancelled the suspended task.")
	reconcileWorkflow(t, m, w, client)
	if len(workFile(t, w).Works) != 2 || workFile(t, w).find(a.ID).State != "cancelled" {
		t.Fatal("parent revived")
	}
}

func TestTurnWorkTextBoundariesEpochAndRetry(t *testing.T) {
	_, w, _, _ := workflowFixture(t)
	chat, _ := loadDialogue(w.Path, dialoguePrefix+sharedChatID("project1.task1"))
	work := turnWork{SessionID: "session", TurnID: "turn", Context: providerMessageContext{ChatVisible: true, ChatEpoch: chat.Epoch}}
	turn := agentHubTurn{Items: []agentHubTurnItem{
		{Type: "activity", Text: "private reasoning", StartEventID: 1},
		{Type: "message", Role: "assistant", Text: "working", StartEventID: 2},
	}}
	if err := syncTurnChat(w.Path, "project1.task1", &work, turn); err != nil {
		t.Fatal(err)
	}
	check, _ := loadDialogue(w.Path, dialoguePrefix+sharedChatID("project1.task1"))
	if len(check.Messages) != 0 {
		t.Fatal("partial text was published")
	}
	turn.Items[1].Text, turn.Items[1].Complete = "working now", true
	for range 2 {
		if err := syncTurnChat(w.Path, "project1.task1", &work, turn); err != nil {
			t.Fatal(err)
		}
	}
	check, _ = loadDialogue(w.Path, dialoguePrefix+sharedChatID("project1.task1"))
	if len(check.Messages) != 1 || check.Messages[0].Text != "working now" {
		t.Fatal("text snapshot lost or duplicated")
	}
	chatMu.Lock()
	check.open()
	if err := saveChatLocked(w.Path, check); err != nil {
		t.Fatal(err)
	}
	chatMu.Unlock()
	turn.Items = append(turn.Items, agentHubTurnItem{Type: "message", Role: "assistant", Text: "old output", StartEventID: 3, Complete: true})
	if err := syncTurnChat(w.Path, "project1.task1", &work, turn); err != nil || work.LastError == "" {
		t.Fatalf("epoch not fenced: %v", err)
	}
	check, _ = loadDialogue(w.Path, dialoguePrefix+sharedChatID("project1.task1"))
	if len(check.Messages) != 1 {
		t.Fatal("old output entered reopened Chat")
	}
}

func TestTurnWorkNotificationAndFailedChild(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "failure"}[failed], func(t *testing.T) {
			m, w, fake, client := workflowFixture(t)
			msg := acceptTestResourceMessage(t, m, w, "project1.task1", "background", resourceMessageModeEnqueue, nil)
			a := *workFile(t, w).find(msg.ID)
			workCommand(t, m, w, turnWorkCommand{Action: "notify", WorkID: a.ID, Text: "The report is ready", Checkpoint: "next task"})
			closeWorkflowTurn(fake, a, "completed", "")
			reconcileWorkflow(t, m, w, client)
			file := workFile(t, w)
			b := *file.find(file.find(a.ID).TargetID)
			if !b.Context.ChatVisible || b.Context.ParentWork != a.ID || b.Context.ParentHistory == "" ||
				b.Context.Language != a.Context.Language || b.Context.OpenerRole != "system" {
				t.Fatal("notification invisible")
			}
			status := "completed"
			if failed {
				status = "failed"
			}
			closeWorkflowTurn(fake, b, status, "Update")
			reconcileWorkflow(t, m, w, client)
			file = workFile(t, w)
			if failed && (file.find(a.ID).State != "blocked" || len(file.Works) != 2) {
				t.Fatal("failed child resumed")
			}
			if !failed && len(file.Works) != 3 {
				t.Fatal("notification did not return")
			}
			raw, _ := json.Marshal(file)
			if strings.Contains(string(raw), "reply-to") {
				t.Fatal("stored legacy capability")
			}
		})
	}
}

func TestTurnWorkFastPollPublishesProgressBeforeCompletion(t *testing.T) {
	m, w, fake, client := workflowFixture(t)
	message := queueWorkflowUser(t, m, w, "progress", "Do some work")
	reconcileWorkflow(t, m, w, client)
	work := *workFile(t, w).find(message.ID)
	fake.mu.Lock()
	turn := fake.turns[work.SessionID][work.TurnID]
	turn.Items = append(turn.Items, agentHubTurnItem{Type: "message", Role: "assistant", Text: "Working on it.", StartEventID: 5, Complete: true})
	fake.turns[work.SessionID][work.TurnID] = turn
	fake.mu.Unlock()
	for range 2 {
		if err := m.pollFastAgentHubSessions(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	chat, _ := loadDialogue(w.Path, dialoguePrefix+sharedChatID("project1.task1"))
	if len(chat.Messages) != 2 || chat.Messages[1].Text != "Working on it." || workFile(t, w).find(work.ID).OutputDone {
		t.Fatalf("active progress missing or duplicated: %+v", chat.Messages)
	}
}

func TestTurnWorkArchivedTaskSkipsStateSettlement(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	message := queueWorkflowUser(t, manager, workspace, "archive", "Do some work")
	reconcileWorkflow(t, manager, workspace, client)
	work := *workFile(t, workspace).find(message.ID)
	closeWorkflowTurn(fake, work, "completed", "Finished before archival.")
	ws, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.ArchiveResource("project1.task1"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := manager.reconcileWorkspaceTurnWork(context.Background(), workspace, client); err != nil {
			t.Fatal(err)
		}
	}
	detail, _, err := taskDetail(workspace.Path, "project1.task1")
	if err != nil || detail.State != app.TaskStateInProgress {
		t.Fatalf("archived task was changed: %+v, %v", detail, err)
	}
	if !workFile(t, workspace).find(work.ID).OutputDone {
		t.Fatal("archived work was not settled")
	}
	chat, err := loadDialogue(workspace.Path, dialoguePrefix+sharedChatID("project1.task1"))
	if err != nil || len(chat.Messages) != 1 {
		t.Fatalf("archived output was published: %+v, %v", chat.Messages, err)
	}
}

func TestTurnWorkAuditContinuesAfterResourceFailure(t *testing.T) {
	for _, failure := range []string{"invalid-json", "missing-resource"} {
		t.Run(failure, func(t *testing.T) {
			manager, workspace, fake, client := workflowFixture(t)
			message := queueWorkflowUser(t, manager, workspace, "recover", "Do some work")
			reconcileWorkflow(t, manager, workspace, client)
			work := *workFile(t, workspace).find(message.ID)
			closeWorkflowTurn(fake, work, "completed", "Recovered final reply.")
			data := []byte("{")
			if failure == "missing-resource" {
				data = []byte(`{"version":1,"resourceId":"project999","works":[{"id":"missing"}]}`)
			}
			badPath := filepath.Join(filepath.Dir(turnWorkPath(workspace.Path, "project1.task1")), "000-broken.json")
			if err := os.WriteFile(badPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			manager.waitBackground()
			manager = newAgentManager(manager.server)
			manager.server.agents = manager
			for range 2 {
				if err := manager.reconcileWorkspaceTurnWork(context.Background(), workspace, client); err == nil {
					t.Fatal("resource failure was not reported")
				}
			}
			if !workFile(t, workspace).find(work.ID).OutputDone {
				t.Fatal("earlier resource failure blocked final reply recovery")
			}
			chat, err := loadDialogue(workspace.Path, dialoguePrefix+sharedChatID("project1.task1"))
			if err != nil || len(chat.Messages) != 2 || chat.Messages[1].Text != "Recovered final reply." {
				t.Fatalf("final reply missing or duplicated: %+v, %v", chat.Messages, err)
			}
		})
	}
}

func TestTurnWorkFastPollRetriesOutputForStableSession(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	message := queueWorkflowUser(t, manager, workspace, "stable", "Do some work")
	reconcileWorkflow(t, manager, workspace, client)
	work := *workFile(t, workspace).find(message.ID)
	closeWorkflowTurn(fake, work, "completed", "Final reply after idle.")
	fake.mu.Lock()
	session := fake.sessions[work.SessionID]
	fake.mu.Unlock()
	var runtime *agentRuntime
	for _, candidate := range manager.runtimeSnapshots() {
		if candidate.snapshotGeneration().AgentHubSessionID == work.SessionID {
			runtime = candidate
			break
		}
	}
	if runtime == nil {
		t.Fatal("test session has no runtime")
	}
	runtime.mu.Lock()
	runtime.agentHubState = "ready"
	runtime.record.Status = "idle"
	runtime.record.CurrentTurnID = ""
	runtime.record.CompletionCursor = session.LastEventID
	runtime.record.UpdatedAt = session.UpdatedAt
	runtime.record.IdleDeadlineAt = ""
	runtime.mu.Unlock()
	if generationNeedsFastReconcile(manager, runtime, runtime.snapshotGeneration(), session, true) {
		t.Fatal("test session must be stable before retrying pending output")
	}
	for range 2 {
		if err := manager.pollFastAgentHubSessions(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if !workFile(t, workspace).find(work.ID).OutputDone {
		t.Fatal("stable session left final output pending")
	}
	chat, err := loadDialogue(workspace.Path, dialoguePrefix+sharedChatID("project1.task1"))
	if err != nil || len(chat.Messages) != 2 || chat.Messages[1].Text != "Final reply after idle." {
		t.Fatalf("final reply missing or duplicated: %+v, %v", chat.Messages, err)
	}
}

func TestTurnWorkPauseCancelsQueuedResume(t *testing.T) {
	m, w, fake, client := workflowFixture(t)
	msg := acceptTestResourceMessage(t, m, w, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	a := *workFile(t, w).find(msg.ID)
	user := queueWorkflowUser(t, m, w, "pause", "Update me")
	workCommand(t, m, w, turnWorkCommand{Action: "yield", WorkID: a.ID, MessageID: user.ID, Checkpoint: "safe"})
	closeWorkflowTurn(fake, a, "completed", "")
	reconcileWorkflow(t, m, w, client)
	b := *workFile(t, w).find(user.ID)
	closeWorkflowTurn(fake, b, "completed", "Updated.")
	if err := m.withResourceController(context.Background(), w, "project1.task1", func() error {
		return m.reconcileTurnWorkLocked(context.Background(), w, "project1.task1", client)
	}); err != nil {
		t.Fatal(err)
	}
	ws, _ := app.OpenWorkspace(w.Path)
	if _, err := ws.SetTaskState("project1.task1", app.TaskStatePaused, "user paused"); err != nil {
		t.Fatal(err)
	}
	if err := m.withResourceController(context.Background(), w, "project1.task1", func() error {
		return m.reconcileTurnWorkLocked(context.Background(), w, "project1.task1", client)
	}); err != nil {
		t.Fatal(err)
	}
	parent := workFile(t, w).find(a.ID)
	resume, _, _ := mailboxMessageByID(w.Path, parent.ResumeID)
	if parent.State != "cancelled" || resume.Status != resourceMessageCancelled {
		t.Fatalf("pause kept continuation alive: %+v %+v", parent, resume)
	}
}

func TestTurnWorkUnsupportedPreviewKeepsBusinessInputQueued(t *testing.T) {
	m, w, fake, client := workflowFixture(t)
	msg := acceptTestResourceMessage(t, m, w, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	a := *workFile(t, w).find(msg.ID)
	fake.mu.Lock()
	session := fake.sessions[a.SessionID]
	session.InputCapabilities.TurnScopedSteer = false
	fake.sessions[a.SessionID] = session
	fake.mu.Unlock()
	user := queueWorkflowUser(t, m, w, "queued", "Question")
	reconcileWorkflow(t, m, w, client)
	queued, _, _ := mailboxMessageByID(w.Path, user.ID)
	if queued.Status != resourceMessageQueued || workFile(t, w).find(a.ID).Previews[user.ID] {
		t.Fatal("unsupported preview consumed the input")
	}
}

func TestTurnWorkRejectedPreviewDoesNotFailBusinessDelivery(t *testing.T) {
	m, w, fake, _ := workflowFixture(t)
	msg := acceptTestResourceMessage(t, m, w, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	a := *workFile(t, w).find(msg.ID)
	user := queueWorkflowUser(t, m, w, "preview-error", "Question")
	hub := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			rw.WriteHeader(http.StatusConflict)
			_, _ = rw.Write([]byte(`{"error":{"code":"runtime_operation_failed","message":"old turn ended"}}`))
			return
		}
		fake.ServeHTTP(rw, r)
	}))
	defer hub.Close()
	client, err := newAgentHubClient(hub.URL, hub.Client())
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	session := fake.sessions[a.SessionID]
	fake.mu.Unlock()
	if err := m.previewQueuedInput(context.Background(), w, session, client, user); err != nil {
		t.Fatal(err)
	}
	queued, _, _ := mailboxMessageByID(w.Path, user.ID)
	if queued.Status != resourceMessageQueued || queued.LastError != "" ||
		workFile(t, w).find(a.ID).PreviewErrors[user.ID] == "" {
		t.Fatalf("preview contaminated business receipt: %+v", queued)
	}
}
