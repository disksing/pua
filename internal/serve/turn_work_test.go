package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func workflowChat(m *agentManager, workspace serveWorkspace) (resourceChat, error) {
	chat, err := loadDialogue(workspace.Path, dialoguePrefix+sharedChatID("project1.task1"))
	if err != nil {
		return chat, err
	}
	return m.chatHistory(context.Background(), workspace.Path, chat)
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

func TestTurnWorkSwitchPreviewResumeAndReplay(t *testing.T) {
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
	if preview.ExpectedTurnID != a.TurnID || !preview.Steer || !strings.Contains(preview.Text, "pua turn switch --message="+user.ID) {
		t.Fatalf("preview not scoped: %+v", preview)
	}
	command := turnWorkCommand{Action: "switch", WorkID: a.ID, MessageID: user.ID}
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
	chat, _ := workflowChat(m, w)
	if len(chat.Messages) != 2 || chat.Messages[1].Text != "I have changed the file." {
		t.Fatalf("internal output leaked or reply lost: %+v", chat.Messages)
	}
	reconcileWorkflow(t, m, w, client)
	replayed, _ := workflowChat(m, w)
	if len(replayed.Messages) != len(chat.Messages) || len(workFile(t, w).Works) != 3 {
		t.Fatal("replay duplicated output or resume")
	}
}

func TestTurnResumeDeliveryReconcilesCanonicalInputAfterRestart(t *testing.T) {
	tests := []struct {
		name           string
		unaccepted     bool
		conflict       bool
		newerTurn      bool
		eventError     bool
		changedSession bool
		wantStatus     string
		wantCode       string
	}{
		{name: "accepted active", wantStatus: resourceMessageDelivered},
		{name: "accepted before different active turn", newerTurn: true, wantStatus: resourceMessageDelivered},
		{name: "unaccepted waits for turn boundary", unaccepted: true, wantStatus: resourceMessageDelivering, wantCode: "temporarily_undeliverable"},
		{name: "same ID with different content", conflict: true, wantStatus: resourceMessageUndeliverable, wantCode: "message_conflict"},
		{name: "history read fails", eventError: true, wantStatus: resourceMessageDelivering, wantCode: "temporarily_undeliverable"},
		{name: "different delivery Session", changedSession: true, wantStatus: resourceMessageDeliveryUnknown, wantCode: "delivery_session_changed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const resource = "project1.task1"
			const resumeID = "resume-restart-fixture"
			ctx := context.Background()
			fake := newRuntimeFakeAgentHub()
			fake.enforceMessageIDs, fake.materializeTurns = true, true
			fake.failNextMessage = !test.unaccepted
			var resumePosts atomic.Int32
			hub := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/messages") {
					var input agentHubInboundMessage
					if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
						t.Error(err)
						writer.WriteHeader(http.StatusBadRequest)
						return
					}
					if input.MessageID == resumeID && resumePosts.Add(1) == 1 {
						if test.unaccepted {
							writer.WriteHeader(http.StatusBadGateway)
							return
						}
						if test.conflict {
							input.Text += " Different content."
						}
					}
					sessionID := strings.Split(request.URL.Path, "/")[3]
					fake.mu.Lock()
					active := fake.sessions[sessionID].CurrentTurnID != ""
					fake.mu.Unlock()
					if active && !input.Steer {
						writer.WriteHeader(http.StatusConflict)
						writeRuntimeFakeJSON(writer, map[string]any{"error": map[string]any{"code": "turn_active"}})
						return
					}
					wire, err := json.Marshal(input)
					if err != nil {
						t.Error(err)
						writer.WriteHeader(http.StatusBadRequest)
						return
					}
					request.Body = io.NopCloser(bytes.NewReader(wire))
				}
				fake.ServeHTTP(writer, request)
			}))
			t.Cleanup(hub.Close)
			manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
			_, client, err := manager.agentHubRuntimeConfig()
			if err != nil {
				t.Fatal(err)
			}
			_, err = acceptGeneratedMailboxMessage(workspace.Path, resourceMailboxMessage{
				ID: resumeID, ResourceID: resource, Type: "turn_resume", Text: "Resume isolated work",
				RequestedMode:   resourceMessageModeEnqueue,
				ProviderContext: &providerMessageContext{Protocol: 1, WorkID: resumeID, OpenerRole: "agent"},
				Causation:       &resourceMessageCausation{Type: "turn_resume", SourceResourceID: resource, MessageID: "isolated-parent"},
			})
			if err != nil {
				t.Fatal(err)
			}
			reconcile := func(manager *agentManager) error {
				return manager.withResourceController(ctx, workspace, resource, func() error {
					return manager.reconcileResourceMailboxLocked(ctx, workspace, resource)
				})
			}
			if err := reconcile(manager); err == nil {
				t.Fatal("first response should leave the delivery outcome unconfirmed")
			}
			resume, found, err := mailboxMessageByID(workspace.Path, resumeID)
			if err != nil || !found || resume.Status != resourceMessageDelivering || resume.AttemptCount != 1 {
				t.Fatalf("first attempt was not persisted: %+v found=%v err=%v", resume, found, err)
			}
			session, err := client.GetSession(ctx, resume.AgentHubSessionID)
			if err != nil {
				t.Fatal(err)
			}
			originalTurn := session.CurrentTurnID
			if test.newerTurn {
				closeWorkflowTurn(fake, turnWork{SessionID: session.ID, TurnID: originalTurn}, "completed", "first Turn done")
			}
			if test.newerTurn || test.unaccepted {
				session, err = client.Message(ctx, session.ID, agentHubInboundMessage{MessageID: "independent-input", Text: "Independent active Turn"})
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.changedSession {
				_, err := updateMailboxMessage(workspace.Path, resumeID, func(message *resourceMailboxMessage) {
					message.AgentHubSessionID = "different-session"
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			fake.mu.Lock()
			fake.failEvents = test.eventError
			fake.mu.Unlock()

			manager.waitBackground()
			restarted := newAgentManager(manager.server)
			manager.server.agents = restarted
			neighbor := acceptTestResourceMessage(t, restarted, workspace, resource, "Independent queued message", resourceMessageModeEnqueue, nil)
			if neighbor.Status != resourceMessageQueued || neighbor.LastError != "" || neighbor.LastErrorCode != "" {
				t.Fatalf("the adjacent input inherited a different delivery failure: %+v", neighbor)
			}
			resume, found, err = mailboxMessageByID(workspace.Path, resumeID)
			if err != nil || !found || resume.Status != test.wantStatus || resume.LastErrorCode != test.wantCode {
				t.Fatalf("unexpected recovered receipt: %+v found=%v err=%v", resume, found, err)
			}
			if resumePosts.Load() != 1 {
				t.Fatalf("resume was reposted while a Turn was active: posts=%d", resumePosts.Load())
			}
			work := workFile(t, workspace).find(resumeID)
			if test.wantStatus == resourceMessageDelivered {
				if originalTurn == "" || resume.TurnID != originalTurn || work == nil || work.TurnID != originalTurn {
					t.Fatalf("resume was not bound to its canonical Turn: receipt=%+v work=%+v", resume, work)
				}
			} else if resume.TurnID != "" || work == nil || work.TurnID != "" {
				t.Fatalf("unconfirmed resume was bound to a Turn: receipt=%+v work=%+v", resume, work)
			}
			if test.eventError && !strings.Contains(resume.LastError, "events_unavailable") {
				t.Fatalf("failed history read was not recorded on the resume: %q", resume.LastError)
			}
			if test.unaccepted {
				closeWorkflowTurn(fake, turnWork{SessionID: session.ID, TurnID: session.CurrentTurnID}, "completed", "independent Turn done")
				if err := reconcile(restarted); err != nil {
					t.Fatal(err)
				}
				resume, found, err = mailboxMessageByID(workspace.Path, resumeID)
				if err != nil || !found || resume.Status != resourceMessageDelivered ||
					resume.TurnID == "" || resume.TurnID == session.CurrentTurnID || resumePosts.Load() != 2 {
					t.Fatalf("unaccepted resume did not deliver once the Session became ready: %+v found=%v err=%v", resume, found, err)
				}
			}
			fake.mu.Lock()
			var canonicalCount int
			for _, event := range fake.events[session.ID] {
				var input agentHubInboundMessage
				if event.Type == "message.input" && json.Unmarshal(event.Data, &input) == nil && input.MessageID == resumeID {
					canonicalCount++
				}
			}
			fake.mu.Unlock()
			if canonicalCount != 1 {
				t.Fatalf("canonical input count=%d, want 1", canonicalCount)
			}
		})
	}
}

func TestTurnWorkExpiredCancelledResumeUnblocksMailbox(t *testing.T) {
	manager, workspace, fake, _ := workflowFixture(t)
	ctx := context.Background()
	const resource = "project1.task1"
	const parentID = "old-parent"
	resumeID := "resume-" + chatID(parentID, "old-child")
	record := idleTestGeneration(workspace, resource, "gen-old-resume", "ses-old-resume", manager.resourceNow())
	record.Status, record.IdleSinceAt, record.IdleDeadlineAt = "stopped", "", ""
	record.AgentHubStoppedObserved = true
	seedIdleTestGeneration(t, fake, workspace, record, "stopped")

	file := workFile(t, workspace)
	file.Works = append(file.Works, turnWork{
		ID: parentID, State: "resuming", OutputDone: true, ResumeID: resumeID,
	})
	if err := saveTurnWork(workspace.Path, resource, file); err != nil {
		t.Fatal(err)
	}
	resume, err := acceptGeneratedMailboxMessage(workspace.Path, resourceMailboxMessage{
		ID: resumeID, ResourceID: resource, Type: "turn_resume", Text: "Resume old work",
		RequestedMode:   resourceMessageModeEnqueue,
		ProviderContext: &providerMessageContext{Protocol: 1, WorkID: resumeID, OpenerRole: "agent"},
		Causation:       &resourceMessageCausation{Type: "turn_resume", SourceResourceID: resource, MessageID: parentID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareTurnWork(workspace.Path, resource, record.GenerationID, record.AgentHubSessionID, resume); err != nil {
		t.Fatal(err)
	}
	if _, err := pauseTaskAfterManualTurnStop(workspace, resource); err != nil {
		t.Fatal(err)
	}
	expireTurnWorkReceipt(t, workspace, resumeID)
	first := queueWorkflowUser(t, manager, workspace, "first-waiting", "First waiting input")
	second := queueWorkflowUser(t, manager, workspace, "second-waiting", "Second waiting input")
	if first.Status != resourceMessageQueued || second.Status != resourceMessageQueued {
		t.Fatalf("fixture needs two waiting inputs: first=%+v second=%+v", first, second)
	}
	stale := workFile(t, workspace).find(resumeID)
	if stale.State != "running" || stale.TurnID != "" || stale.OutputDone {
		t.Fatalf("fixture needs unresolved resume work: %+v", stale)
	}

	manager.waitBackground()
	manager = newAgentManager(manager.server)
	manager.server.agents = manager
	if err := manager.reconcileWorkspaceMailboxes(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	file = workFile(t, workspace)
	parent, stale := file.find(parentID), file.find(resumeID)
	if parent.State != "cancelled" || parent.Outcome != "manual stop" || !stale.OutputDone || stale.State != "blocked" ||
		stale.TurnID != "" || !strings.Contains(stale.LastError, "receipt expired") {
		t.Fatalf("old work did not settle: parent=%+v resume=%+v", parent, stale)
	}
	assertDelivered := func(id string) turnWork {
		t.Helper()
		work := workFile(t, workspace).find(id)
		delivery, found, err := mailboxMessageByID(workspace.Path, id)
		if err != nil || !found || delivery.Status != resourceMessageDelivered || delivery.DeliveredAt == "" ||
			work == nil || work.TurnID == "" || delivery.TurnID != work.TurnID || work.SessionID != record.AgentHubSessionID ||
			!work.Context.ChatVisible || work.Context.OpenerRole != "user" {
			t.Fatalf("waiting input did not open a formal Chat Turn: work=%+v receipt=%+v found=%v err=%v", work, delivery, found, err)
		}
		return *work
	}
	firstWork := assertDelivered(first.ID)
	if err := manager.reconcileWorkspaceMailboxes(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	waiting, found, err := mailboxMessageByID(workspace.Path, second.ID)
	if err != nil || !found || waiting.Status != resourceMessageQueued || workFile(t, workspace).find(second.ID) != nil {
		t.Fatalf("second input did not wait for first Turn: %+v, found=%v, err=%v", waiting, found, err)
	}
	closeWorkflowTurn(fake, firstWork, "completed", "First input handled")
	manager.waitBackground()
	manager = newAgentManager(manager.server)
	manager.server.agents = manager
	if err := manager.reconcileWorkspaceMailboxes(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	secondWork := assertDelivered(second.ID)
	if secondWork.TurnID == firstWork.TurnID || len(workFile(t, workspace).Works) != 4 {
		t.Fatalf("mailbox did not advance to a distinct Turn: %+v", workFile(t, workspace))
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	_, replayed := fake.messageInputs[resumeID]
	if replayed || len(fake.resumeEnvironments) != 1 || len(fake.turns[record.AgentHubSessionID]) != 2 {
		t.Fatalf("unexpected resume or Turn replay: old input sent=%v actions=%v turns=%d", replayed, fake.actions, len(fake.turns[record.AgentHubSessionID]))
	}
}

func TestTurnWorkExpiredContinuationReferencesSettle(t *testing.T) {
	for _, state := range []string{"suspended", "resuming", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			manager, workspace, _, client := workflowFixture(t)
			const resource = "project1.task1"
			input, err := acceptMailboxMessage(workspace.Path, resource, resourceMessageRequest{Text: "Old continuation input"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := updateMailboxMessage(workspace.Path, input.ID, func(message *resourceMailboxMessage) {
				message.Status = resourceMessageCancelled
			}); err != nil {
				t.Fatal(err)
			}
			expireTurnWorkReceipt(t, workspace, input.ID)
			file := workFile(t, workspace)
			parent := turnWork{ID: "parent", OutputDone: true, State: "resuming"}
			if state == "suspended" {
				parent.State, parent.TargetID = state, input.ID
			} else {
				parent.ResumeID = input.ID
			}
			file.Works = append(file.Works, parent)
			if err := saveTurnWork(workspace.Path, resource, file); err != nil {
				t.Fatal(err)
			}
			wantState, wantOutcome := "blocked", "mailbox receipt expired"
			if state == "cancelled" {
				if err := cancelSuspendedTurnWork(workspace.Path, resource, "manual stop"); err != nil {
					t.Fatal(err)
				}
				wantState, wantOutcome = state, "manual stop"
			}
			if err := manager.reconcileWorkspaceTurnWork(context.Background(), workspace, client); err != nil {
				t.Fatal(err)
			}
			file = workFile(t, workspace)
			if got := file.find(parent.ID); len(file.Works) != 1 || got.State != wantState || got.Outcome != wantOutcome {
				t.Fatalf("continuation did not settle: %+v", file.Works)
			}
		})
	}
}

func TestTurnWorkRecoversDeliveredChatWithoutTurnID(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	const resource = "project1.task1"
	input := queueWorkflowUser(t, manager, workspace, "lost-binding", "What is K?")
	reconcileWorkflow(t, manager, workspace, client)
	manager.waitBackground()
	file := workFile(t, workspace)
	work := file.find(input.ID)
	turnID := work.TurnID
	closeWorkflowTurn(fake, *work, "completed", "Answer about K")
	work.TurnID = ""
	if err := saveTurnWork(workspace.Path, resource, file); err != nil {
		t.Fatal(err)
	}
	if _, err := updateMailboxMessage(workspace.Path, input.ID, func(message *resourceMailboxMessage) {
		message.TurnID = ""
	}); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		reconcileWorkflow(t, manager, workspace, client)
	}
	recovered := workFile(t, workspace).find(input.ID)
	receipt, _, err := mailboxMessageByID(workspace.Path, input.ID)
	if err != nil || receipt.TurnID != turnID || recovered.TurnID != turnID || recovered.State != "completed" {
		t.Fatalf("failed to restore delivery: work=%+v receipt=%+v err=%v", recovered, receipt, err)
	}
	chat, err := workflowChat(manager, workspace)
	if err != nil || len(chat.Messages) != 2 || chat.Messages[1].Text != "Answer about K" {
		t.Fatalf("expected exactly one published answer: %+v, %v", chat.Messages, err)
	}
}

func TestTurnWorkInputLookupRetriesAndKeepsBoundTurn(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	ctx := context.Background()
	const resource = "project1.task1"
	input := queueWorkflowUser(t, manager, workspace, "recoverable", "Recover my Turn")
	reconcileWorkflow(t, manager, workspace, client)
	next := queueWorkflowUser(t, manager, workspace, "after-recovery", "Handle this after recovery")
	manager.waitBackground()
	file := workFile(t, workspace)
	work := file.find(input.ID)
	turnID := work.TurnID
	work.TurnID = "" // Simulate a crash after persisting the receipt, before binding the Turn.
	file.Works = append([]turnWork{{ID: "parent", State: "resuming", OutputDone: true, ResumeID: input.ID}}, file.Works...)
	if err := saveTurnWork(workspace.Path, resource, file); err != nil {
		t.Fatal(err)
	}
	directory, _, _, err := resourceMailboxDirectory(workspace.Path, resource)
	if err != nil {
		t.Fatal(err)
	}
	path := resourceMailboxReceiptPath(directory)
	receipts, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileWorkspaceMailboxes(ctx, workspace); err == nil || !strings.Contains(err.Error(), "receipts.json") {
		t.Fatalf("temporary receipt lookup failure not reported: %v", err)
	}
	if got := workFile(t, workspace).find(input.ID); got.State != "running" || got.OutputDone || got.TurnID != "" {
		t.Fatalf("temporary lookup failure settled recoverable work: %+v", got)
	}
	if err := os.WriteFile(path, receipts, 0600); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileWorkspaceMailboxes(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if got := workFile(t, workspace).find(input.ID); got.TurnID != turnID || got.OutputDone {
		t.Fatalf("successful retry did not bind the original Turn: %+v", got)
	}
	expireTurnWorkReceipt(t, workspace, input.ID)
	if err := manager.reconcileWorkspaceMailboxes(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	file = workFile(t, workspace)
	if got := file.find(input.ID); got.OutputDone || got.State != "running" || file.find("parent").State != "resuming" {
		t.Fatalf("expiry settled work whose Turn is still available: %+v", file.Works)
	}
	closeWorkflowTurn(fake, *file.find(input.ID), "completed", "Recovered")
	if err := manager.reconcileWorkspaceMailboxes(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	file = workFile(t, workspace)
	following := file.find(next.ID)
	if file.find(input.ID).State != "completed" || file.find("parent").State != "completed" || following == nil || following.TurnID == "" {
		t.Fatalf("recovered Turn did not complete and advance the mailbox: %+v", file.Works)
	}
}

func expireTurnWorkReceipt(t *testing.T, workspace serveWorkspace, id string) {
	t.Helper()
	if _, err := updateMailboxMessage(workspace.Path, id, func(message *resourceMailboxMessage) {
		message.TerminalAt = time.Now().Add(-resourceMailboxReceiptRetentionWindow - time.Hour).Format(time.RFC3339Nano)
	}); err != nil {
		t.Fatal(err)
	}
	_, found, err := mailboxMessageByID(workspace.Path, id)
	var apiErr *resourceAPIError
	if found || !errors.As(err, &apiErr) || apiErr.Code != "message_receipt_expired" {
		t.Fatalf("fixture receipt did not expire: found=%v, err=%v", found, err)
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
			command := turnWorkCommand{Action: "switch", WorkID: a.ID, Text: "The report is ready"}
			workCommand(t, m, w, command)
			workCommand(t, m, w, command)
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
	chat, _ := workflowChat(m, w)
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
			chat, err := workflowChat(manager, workspace)
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
	chat, err := workflowChat(manager, workspace)
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
