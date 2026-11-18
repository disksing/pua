package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func setAdvisoryProvider(fake *runtimeFakeAgentHub, sessionID, provider string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	session := fake.sessions[sessionID]
	session.Provider = provider
	session.InputCapabilities = agentHubInputCapabilities{Steer: true, TurnScopedSteer: provider == "codex"}
	fake.sessions[sessionID] = session
}

func advisoryStatus(t *testing.T, manager *agentManager, workspace serveWorkspace) (turnCurrentControl, turnChatSnapshot) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	manager.handleTurnWork(response, request, workspace.ID, "project1.task1")
	if response.Code != http.StatusOK {
		t.Fatalf("status failed: %d %s", response.Code, response.Body.String())
	}
	var status struct {
		CurrentControl turnCurrentControl `json:"currentControl"`
		Chat           turnChatSnapshot   `json:"chat"`
		Advisories     []turnAdvisory     `json:"advisories"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	for _, advisory := range status.Advisories {
		if advisory.Input != nil {
			t.Fatal("status exposed advisory input body")
		}
	}
	return status.CurrentControl, status.Chat
}

func TestPiAdvisoryBoundarySerializesFormalInputAndHistory(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	message := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	original := *workFile(t, workspace).find(message.ID)
	setAdvisoryProvider(fake, original.SessionID, "pi")
	var boundary sync.Once
	fake.mu.Lock()
	fake.messageHook = func(_ string, input agentHubInboundMessage) {
		if strings.HasPrefix(input.MessageID, "preview-") {
			boundary.Do(func() { closeWorkflowTurn(fake, original, "completed", "") })
		}
	}
	fake.mu.Unlock()
	formal := queueWorkflowUser(t, manager, workspace, "pi-boundary", "Question")
	reconcileWorkflow(t, manager, workspace, client)
	file := workFile(t, workspace)
	if len(file.Advisories) != 1 {
		t.Fatalf("expected one advisory: %+v", file.Advisories)
	}
	advisory := file.Advisories[0]
	if !advisory.Standalone || advisory.TurnID == original.TurnID || advisory.HistoryRef == "" || advisory.State != "accepted" {
		t.Fatalf("missing standalone identity: %+v", advisory)
	}
	for range 2 {
		reconcileWorkflow(t, manager, workspace, client)
	}
	waiting, _, _ := mailboxMessageByID(workspace.Path, formal.ID)
	if waiting.Status != resourceMessageQueued || workFile(t, workspace).find(formal.ID) != nil {
		t.Fatal("formal input was merged into the advisory Turn")
	}
	control, snapshot := advisoryStatus(t, manager, workspace)
	if control.Kind != "advisory" || control.WorkID != "" || len(control.Available) != 0 || !snapshot.Open {
		t.Fatalf("advisory got business control: %+v %+v", control, snapshot)
	}
	for _, operation := range []string{"close", "open"} {
		mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", operation, chatRequest{}, false))
	}
	reconcileWorkflow(t, manager, workspace, client)
	if len(workFile(t, workspace).Advisories) != 1 {
		t.Fatal("Chat toggles during an advisory created another hint Turn")
	}
	detail, err := manager.resourceHistoryTurn(context.Background(), workspace, "project1.task1", advisory.HistoryRef)
	if err != nil || len(detail.Turn.Advisories) != 1 || !detail.Turn.Advisories[0].Standalone {
		t.Fatalf("advisory absent from resource History: %+v %v", detail, err)
	}
	if err := manager.withResourceController(context.Background(), workspace, "project1.task1", func() error {
		current := workFile(t, workspace)
		_, err := manager.controlTurnWork(context.Background(), workspace, "project1.task1", &current,
			turnWorkCommand{WorkID: original.ID, Action: "notify", Text: "stale"})
		if err == nil {
			t.Fatal("old work controlled the standalone advisory")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	closeWorkflowTurn(fake, turnWork{SessionID: advisory.SessionID, TurnID: advisory.TurnID}, "completed", "advisory only")
	reconcileWorkflow(t, manager, workspace, client)
	file = workFile(t, workspace)
	delivered, _, _ := mailboxMessageByID(workspace.Path, formal.ID)
	if delivered.Status != resourceMessageDelivered || file.find(formal.ID) == nil ||
		file.find(formal.ID).TurnID == advisory.TurnID || !file.find(formal.ID).Context.ChatVisible {
		t.Fatalf("formal input did not retain its own audience and Turn: %+v", delivered)
	}
	chat, _ := loadDialogue(workspace.Path, dialoguePrefix+sharedChatID("project1.task1"))
	for _, entry := range chat.Messages {
		if entry.Role == "agent" && entry.Text == "advisory only" {
			t.Fatal("advisory output leaked to shared Chat")
		}
	}
	control, _ = advisoryStatus(t, manager, workspace)
	if control.Kind != "work" || control.WorkID != formal.ID {
		t.Fatalf("formal control was not restored: %+v", control)
	}
}

func TestPiAdvisoryUnknownReceiptRecoversWithoutResend(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	message := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	original := *workFile(t, workspace).find(message.ID)
	setAdvisoryProvider(fake, original.SessionID, "pi")
	fake.mu.Lock()
	fake.messageHook = func(_ string, input agentHubInboundMessage) {
		if strings.HasPrefix(input.MessageID, "preview-") {
			fake.mu.Lock()
			fake.failNextMessage, fake.failEvents = true, true
			fake.mu.Unlock()
		}
	}
	fake.mu.Unlock()
	formal := queueWorkflowUser(t, manager, workspace, "unknown-pi", "Question")
	reconcileWorkflow(t, manager, workspace, client)
	file := workFile(t, workspace)
	if len(file.Advisories) != 1 || file.Advisories[0].State != "unknown" || !file.advisoryBarrier() {
		t.Fatalf("unknown outcome was not retained: %+v", file.Advisories)
	}
	fake.mu.Lock()
	inputCount := len(fake.messageIDs)
	fake.failEvents, fake.messageHook = false, nil
	fake.mu.Unlock()
	for range 2 {
		reconcileWorkflow(t, manager, workspace, client)
	}
	fake.mu.Lock()
	afterCount := len(fake.messageIDs)
	fake.mu.Unlock()
	file = workFile(t, workspace)
	if inputCount != afterCount || !file.find(original.ID).Previews[formal.ID] || file.Advisories[0].TurnID != original.TurnID {
		t.Fatal("recovery resent or lost the canonical advisory")
	}
	closeWorkflowTurn(fake, original, "completed", "")
	fake.mu.Lock()
	fake.providerBusy = true
	fake.mu.Unlock()
	reconcileWorkflow(t, manager, workspace, client)
	if workFile(t, workspace).Advisories[0].State != "completed" {
		t.Fatal("terminal advisory did not settle")
	}
	waiting, _, err := mailboxMessageByID(workspace.Path, formal.ID)
	if err != nil || waiting.Status != resourceMessageQueued || waiting.TurnID != "" || waiting.LastError != "" {
		t.Fatalf("formal input while Pi still processes the preview: %+v, %v", waiting, err)
	}
	fake.mu.Lock()
	fake.providerBusy = false
	fake.mu.Unlock()
	reconcileWorkflow(t, manager, workspace, client)
	child := workFile(t, workspace).find(formal.ID)
	if child == nil || child.TurnID == "" || child.TurnID == original.TurnID || !child.Context.ChatVisible {
		t.Fatalf("formal input did not open its own visible Turn after Pi became idle: %+v", child)
	}
}

func TestChatStateAdvisoryCoversFormalAudiencesAndProviders(t *testing.T) {
	for _, provider := range []string{"codex", "pi"} {
		for _, visible := range []bool{false, true} {
			audience := "internal"
			if visible {
				audience = "visible"
			}
			t.Run(provider+"/"+audience, func(t *testing.T) {
				manager, workspace, fake, client := workflowFixture(t)
				var message resourceMailboxMessage
				if visible {
					message = queueWorkflowUser(t, manager, workspace, "state-visible", "Work")
					reconcileWorkflow(t, manager, workspace, client)
				} else {
					message = acceptTestResourceMessage(t, manager, workspace, "project1.task1", "Work", resourceMessageModeEnqueue, nil)
				}
				work := *workFile(t, workspace).find(message.ID)
				setAdvisoryProvider(fake, work.SessionID, provider)
				for _, operation := range []string{"close", "open"} {
					mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", operation, chatRequest{}, false))
					reconcileWorkflow(t, manager, workspace, client)
				}
				file := workFile(t, workspace)
				if len(file.Advisories) != 2 || file.find(work.ID).Context.ChatVisible != visible ||
					file.find(work.ID).Context.ChatEpoch != work.Context.ChatEpoch {
					t.Fatalf("missing notices or changed audience: %+v", file)
				}
				for _, advisory := range file.Advisories {
					if advisory.Type != "chat_state" || advisory.TurnID != work.TurnID ||
						(advisory.Input.ExpectedTurnID != "") != (provider == "codex") ||
						!strings.Contains(advisory.Input.Text, "pua turn status") ||
						!strings.Contains(advisory.Input.Text, "audit only") {
						t.Fatalf("invalid Chat notice: %+v", advisory)
					}
				}
				control, snapshot := advisoryStatus(t, manager, workspace)
				if control.WorkID != work.ID || !snapshot.Open || snapshot.Epoch != 2 || snapshot.Revision != 3 || snapshot.ParentID != "project1" {
					t.Fatalf("incorrect status snapshot/control: %+v %+v", control, snapshot)
				}
				fake.mu.Lock()
				inputCount := len(fake.messageIDs)
				fake.mu.Unlock()
				mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "open", chatRequest{}, false))
				reconcileWorkflow(t, manager, workspace, client)
				fake.mu.Lock()
				afterCount := len(fake.messageIDs)
				fake.mu.Unlock()
				if inputCount != afterCount {
					t.Fatal("idempotent open generated another notice")
				}
			})
		}
	}
}

func TestChatStateIdleAndRapidChangesDoNotWakeOrReplay(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	for _, operation := range []string{"close", "open", "close"} {
		mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", operation, chatRequest{}, false))
		reconcileWorkflow(t, manager, workspace, client)
	}
	fake.mu.Lock()
	inputCount := len(fake.messageIDs)
	fake.mu.Unlock()
	if inputCount != 0 || len(workFile(t, workspace).Advisories) != 0 {
		t.Fatal("idle Chat toggles woke the Agent")
	}
	message := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	work := *workFile(t, workspace).find(message.ID)
	setAdvisoryProvider(fake, work.SessionID, "pi")
	if work.Context.Chat == nil || work.Context.Chat.Open || work.Context.Chat.Revision != 4 {
		t.Fatalf("next opener missed idle snapshot: %+v", work.Context.Chat)
	}
	for _, operation := range []string{"open", "close", "open"} {
		mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", operation, chatRequest{}, false))
	}
	reconcileWorkflow(t, manager, workspace, client)
	file := workFile(t, workspace)
	if len(file.Advisories) != 1 || file.Advisories[0].ID != "chat-state-"+chatID(work.ID, "7") ||
		!strings.Contains(file.Advisories[0].Input.Text, "Shared Chat opened") {
		t.Fatalf("rapid unsent changes did not coalesce: %+v", file.Advisories)
	}
}

func TestIdleToggleWithUnsettledWorkDoesNotStartAdvisory(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	message := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	work := *workFile(t, workspace).find(message.ID)
	setAdvisoryProvider(fake, work.SessionID, "pi")
	closeWorkflowTurn(fake, work, "completed", "")
	mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "close", chatRequest{}, false))
	reconcileWorkflow(t, manager, workspace, client)
	if len(workFile(t, workspace).Advisories) != 0 {
		t.Fatal("stale unsettled work woke an idle provider for a Chat toggle")
	}
}

func TestChatStateNoticeTransientLookupRecoversOnlyWhileActive(t *testing.T) {
	for _, ended := range []bool{false, true} {
		name := "active"
		if ended {
			name = "ended"
		}
		t.Run(name, func(t *testing.T) {
			manager, workspace, fake, client := workflowFixture(t)
			message := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "background", resourceMessageModeEnqueue, nil)
			work := *workFile(t, workspace).find(message.ID)
			setAdvisoryProvider(fake, work.SessionID, "pi")
			fake.mu.Lock()
			fake.failGetSessionID = work.SessionID
			fake.mu.Unlock()
			mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "close", chatRequest{}, false))
			chat, _ := loadDialogue(workspace.Path, dialoguePrefix+sharedChatID("project1.task1"))
			if chat.StateNotice == nil || chat.StateNotice.AllowBoundary {
				t.Fatal("transient failure lost the pending unverified notice")
			}
			fake.mu.Lock()
			fake.failGetSessionID = ""
			fake.mu.Unlock()
			if ended {
				closeWorkflowTurn(fake, work, "completed", "")
			}
			reconcileWorkflow(t, manager, workspace, client)
			count := len(workFile(t, workspace).Advisories)
			if (ended && count != 0) || (!ended && count != 1) {
				t.Fatalf("unverified notice crossed an idle boundary: ended=%t, count=%d", ended, count)
			}
		})
	}
}

func TestChatStateNoticeSurvivesTerminalProjectionRace(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	message := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	work := *workFile(t, workspace).find(message.ID)
	setAdvisoryProvider(fake, work.SessionID, "pi")
	mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "close", chatRequest{}, false))
	closeWorkflowTurn(fake, work, "completed", "")
	file := workFile(t, workspace)
	file.find(work.ID).OutputDone, file.find(work.ID).State = true, "completed"
	if err := saveTurnWork(workspace.Path, "project1.task1", file); err != nil {
		t.Fatal(err)
	}
	manager.requestTurnOutput(workspace, "project1.task1")
	key := turnOutputKey{root: workspace.Path, resource: "project1.task1"}
	manager.turnOutputMu.Lock()
	target := manager.turnOutputTargets[key]
	pending := target != nil && target.pending
	manager.turnOutputMu.Unlock()
	if !pending {
		t.Fatal("terminal output projection hid a durable Chat notice from the output loop")
	}
	reconcileWorkflow(t, manager, workspace, client)
	file = workFile(t, workspace)
	if len(file.Advisories) != 1 || !file.Advisories[0].Standalone {
		t.Fatal("verified active toggle did not survive the completion boundary")
	}
}

func TestPausedResourceDoesNotSendPendingChatNotice(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	message := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	work := *workFile(t, workspace).find(message.ID)
	setAdvisoryProvider(fake, work.SessionID, "pi")
	mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "close", chatRequest{}, false))
	ws, _ := app.OpenWorkspace(workspace.Path)
	if _, err := ws.SetTaskState("project1.task1", app.TaskStatePaused, "pause"); err != nil {
		t.Fatal(err)
	}
	reconcileWorkflow(t, manager, workspace, client)
	if len(workFile(t, workspace).Advisories) != 0 {
		t.Fatal("paused resource sent a new pending Chat notice")
	}
}

func TestCurrentControlRejectsMismatchedGenerationAndTerminalTurn(t *testing.T) {
	manager, workspace, fake, _ := workflowFixture(t)
	message := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	file := workFile(t, workspace)
	work := *file.find(message.ID)
	file.find(work.ID).GenerationID = "another-generation"
	if control := manager.currentTurnControl(context.Background(), workspace, "project1.task1", file); control.WorkID != "" {
		t.Fatal("mismatched generation got a control capability")
	}
	file.find(work.ID).GenerationID = work.GenerationID
	closeWorkflowTurn(fake, work, "completed", "")
	fake.mu.Lock()
	session := fake.sessions[work.SessionID]
	session.CurrentTurnID, session.State = work.TurnID, "running"
	fake.sessions[work.SessionID] = session
	fake.mu.Unlock()
	if control := manager.currentTurnControl(context.Background(), workspace, "project1.task1", file); control.WorkID != "" {
		t.Fatal("closed Turn got a control capability")
	}
	_, err := manager.controlTurnWork(context.Background(), workspace, "project1.task1", &file,
		turnWorkCommand{Action: "finish", WorkID: work.ID, Reason: "completed"})
	if err == nil {
		t.Fatal("terminal Turn accepted work controls")
	}
}

func TestAdvisoryCompletionDoesNotCreateBusinessContinuation(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	message := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	work := *workFile(t, workspace).find(message.ID)
	setAdvisoryProvider(fake, work.SessionID, "pi")
	var boundary sync.Once
	fake.mu.Lock()
	fake.messageHook = func(_ string, input agentHubInboundMessage) {
		if strings.HasPrefix(input.MessageID, "chat-state-") {
			boundary.Do(func() { closeWorkflowTurn(fake, work, "completed", "") })
		}
	}
	fake.mu.Unlock()
	mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "close", chatRequest{}, false))
	reconcileWorkflow(t, manager, workspace, client)
	file := workFile(t, workspace)
	if len(file.Advisories) != 1 || !file.Advisories[0].Standalone {
		t.Fatalf("no boundary status Turn: %+v", file.Advisories)
	}
	ws, _ := app.OpenWorkspace(workspace.Path)
	if _, err := ws.SetTaskState("project1.task1", app.TaskStateBlocked, "await approval"); err != nil {
		t.Fatal(err)
	}
	advisory := file.Advisories[0]
	closeWorkflowTurn(fake, turnWork{SessionID: advisory.SessionID, TurnID: advisory.TurnID}, "completed", "")
	reconcileWorkflow(t, manager, workspace, client)
	file = workFile(t, workspace)
	if len(file.Works) != 1 || file.Works[0].ResumeID != "" {
		t.Fatal("advisory created an implicit work or continuation")
	}
	detail, _, err := taskDetail(workspace.Path, "project1.task1")
	if err != nil || detail.State != app.TaskStateBlocked {
		t.Fatal("advisory completed the business task")
	}
}
