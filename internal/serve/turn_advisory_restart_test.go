package serve

import (
	"strings"
	"testing"
)

func restartWorkflowManager(t *testing.T, manager *agentManager) (*agentManager, *agentHubClient) {
	t.Helper()
	restarted := newAgentManager(manager.server)
	manager.server.agents = restarted
	_, client, err := restarted.agentHubRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	return restarted, client
}

func countSentInput(fake *runtimeFakeAgentHub, id string) int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	count := 0
	for _, sent := range fake.messageIDs {
		if sent == id {
			count++
		}
	}
	return count
}

func TestPiAdvisoryUnknownRecoversAfterManagerRestartWithoutResend(t *testing.T) {
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
	formal := queueWorkflowUser(t, manager, workspace, "restart-unknown", "Question")
	reconcileWorkflow(t, manager, workspace, client)
	file := workFile(t, workspace)
	if len(file.Advisories) != 1 || file.Advisories[0].State != "unknown" || !file.advisoryBarrier() {
		t.Fatalf("unknown outcome was not retained: %+v", file.Advisories)
	}
	advisoryID := file.Advisories[0].ID
	fake.mu.Lock()
	fake.failEvents, fake.messageHook = false, nil
	fake.mu.Unlock()
	restarted, restartedClient := restartWorkflowManager(t, manager)
	for range 2 {
		reconcileWorkflow(t, restarted, workspace, restartedClient)
	}
	file = workFile(t, workspace)
	advisory := file.Advisories[0]
	if advisory.State != "accepted" || advisory.TurnID != original.TurnID || advisory.HistoryRef == "" {
		t.Fatalf("restart lost canonical advisory identity: %+v", advisory)
	}
	if count := countSentInput(fake, advisoryID); count != 1 {
		t.Fatalf("restart resent the advisory: count=%d", count)
	}
	queued, _, _ := mailboxMessageByID(workspace.Path, formal.ID)
	if queued.Status != resourceMessageQueued || file.find(formal.ID) != nil {
		t.Fatal("formal input passed the advisory barrier after restart")
	}
	closeWorkflowTurn(fake, turnWork{SessionID: advisory.SessionID, TurnID: advisory.TurnID}, "completed", "")
	for range 2 {
		reconcileWorkflow(t, restarted, workspace, restartedClient)
	}
	if count := countSentInput(fake, advisoryID); count != 1 {
		t.Fatalf("terminal advisory was resent: count=%d", count)
	}
	if state := workFile(t, workspace).Advisories[0].State; state != "completed" {
		t.Fatalf("terminal advisory did not settle after restart: %s", state)
	}
}

func TestPiAdvisoryAcceptedSurvivesManagerRestart(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	message := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	work := *workFile(t, workspace).find(message.ID)
	setAdvisoryProvider(fake, work.SessionID, "pi")
	mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "close", chatRequest{}, false))
	reconcileWorkflow(t, manager, workspace, client)
	file := workFile(t, workspace)
	if len(file.Advisories) != 1 || file.Advisories[0].State != "accepted" || file.Advisories[0].TurnID != work.TurnID {
		t.Fatalf("advisory was not accepted in the origin Turn: %+v", file.Advisories)
	}
	advisory := file.Advisories[0]
	restarted, restartedClient := restartWorkflowManager(t, manager)
	for range 2 {
		reconcileWorkflow(t, restarted, workspace, restartedClient)
	}
	recovered := workFile(t, workspace).Advisories[0]
	if recovered.State != "accepted" || recovered.TurnID != advisory.TurnID ||
		recovered.HistoryRef != advisory.HistoryRef || recovered.Input == nil {
		t.Fatalf("restart did not recover an accepted advisory: %+v", recovered)
	}
	if count := countSentInput(fake, advisory.ID); count != 1 {
		t.Fatalf("accepted advisory was resent after restart: count=%d", count)
	}
	closeWorkflowTurn(fake, work, "completed", "")
	for range 2 {
		reconcileWorkflow(t, restarted, workspace, restartedClient)
	}
	if count := countSentInput(fake, advisory.ID); count != 1 {
		t.Fatalf("terminal accepted advisory was resent: count=%d", count)
	}
	if state := workFile(t, workspace).Advisories[0].State; state != "completed" {
		t.Fatalf("terminal advisory did not settle after restart: %s", state)
	}
}

func TestChatStatePendingNoticeSurvivesManagerRestart(t *testing.T) {
	manager, workspace, fake, _ := workflowFixture(t)
	message := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	work := *workFile(t, workspace).find(message.ID)
	setAdvisoryProvider(fake, work.SessionID, "pi")
	fake.mu.Lock()
	fake.failGetSessionID = work.SessionID
	fake.mu.Unlock()
	mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "close", chatRequest{}, false))
	stored, err := loadDialogue(workspace.Path, dialoguePrefix+sharedChatID("project1.task1"))
	if err != nil || stored.StateNotice == nil || stored.StateNotice.AllowBoundary {
		t.Fatalf("pending Chat notice was not retained: %+v %v", stored.StateNotice, err)
	}
	fake.mu.Lock()
	fake.failGetSessionID = ""
	fake.mu.Unlock()
	restarted, restartedClient := restartWorkflowManager(t, manager)
	for range 2 {
		reconcileWorkflow(t, restarted, workspace, restartedClient)
	}
	file := workFile(t, workspace)
	if len(file.Advisories) != 1 || file.Advisories[0].Type != "chat_state" {
		t.Fatalf("pending Chat notice was not delivered after restart: %+v", file.Advisories)
	}
	advisoryID := file.Advisories[0].ID
	if count := countSentInput(fake, advisoryID); count != 1 {
		t.Fatalf("Chat notice restart send count=%d", count)
	}
	if latest, err := loadDialogue(workspace.Path, dialoguePrefix+sharedChatID("project1.task1")); err != nil || latest.StateNotice != nil {
		t.Fatal("pending Chat notice was not cleared after delivery")
	}
}
