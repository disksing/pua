package serve

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHistoryChatReadsBeforeReconciliationAndAfterRestart(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	const resource = "project1.task1"
	input := queueWorkflowUser(t, manager, workspace, "question", "queued user text")
	reconcileWorkflow(t, manager, workspace, client)
	manager.waitBackground()
	file := workFile(t, workspace)
	work := file.find(input.ID)
	closeWorkflowTurn(fake, *work, "completed", "history only answer")
	turnID := work.TurnID
	work.TurnID = ""
	if err := saveTurnWork(workspace.Path, resource, file); err != nil {
		t.Fatal(err)
	}
	manager = newAgentManager(manager.server)
	manager.server.agents = manager
	read := func(operation string, body any) publicChat {
		t.Helper()
		response := chatCall(t, manager, workspace, resource, "reader", operation, body, false)
		mustChatOK(t, response)
		var chat publicChat
		if err := json.Unmarshal(response.Body.Bytes(), &chat); err != nil {
			t.Fatal(err)
		}
		return chat
	}
	page := read("", nil)
	if len(page.Messages) != 2 || page.Messages[1].Text != "history only answer" || page.UnreadCount != 1 {
		t.Fatalf("unexpected public history: %+v", page)
	}
	if ids := page.Messages[0].InputMessageIDs; len(ids) != 1 || ids[0] != input.ID {
		t.Fatalf("Chat input lost its canonical mailbox ID: %v, want %s", ids, input.ID)
	}
	if answer := page.Messages[1]; answer.TurnID != turnID || answer.StartEventID <= 0 || len(answer.InputMessageIDs) != 0 {
		t.Fatalf("reconstructed answer lost its Turn event identity: %+v", answer)
	}
	if read("read", chatRequest{ID: page.Messages[1].ID, Position: 2}).UnreadCount != 0 {
		t.Fatal("could not mark reconstructed reply read")
	}
	after := read("", nil)
	if len(after.Messages) != 2 || after.UnreadCount != 0 {
		t.Fatalf("lost history or read cursor: %+v", after)
	}
	data, err := os.ReadFile(chatPath(workspace.Path, sharedChatID(resource)))
	if err != nil || strings.Contains(string(data), "history only answer") || strings.Count(string(data), "queued user text") != 1 {
		t.Fatal("Chat must store only one queued input and no automatic reply bodies", err)
	}
}

func TestHistoryChatLateTurnRemainsUnreadAndForwardsOnce(t *testing.T) {
	rt, api, state := telegramFixture(t)
	const resource = "project1.task1"
	route := telegramOpen(t, rt, &state, resource)
	file := turnWorkFile{Version: 1, ResourceID: resource}
	add := func(id, text, stamp string) {
		t.Helper()
		work := turnWork{ID: id, SessionID: "session", TurnID: id, HistoryChat: true, OutputDone: true, ChatTextCount: 1, LastPublishedAt: stamp,
			Context: providerMessageContext{ChatVisible: true, ChatEpoch: route.Epoch}}
		file.Works = append(file.Works, work)
		if err := saveTurnWork(rt.workspace.Path, resource, file); err != nil {
			t.Fatal(err)
		}
		turn := agentHubTurn{Closed: true, Items: []agentHubTurnItem{{Type: "message", Role: "assistant", StartEventID: 1, StartedAt: stamp, Text: text}}}
		rt.server.agents.chatHistoryCache.put(chatTurnKey{rt.workspace.Path, resource, work.SessionID, id, id}, historyChatTexts(work, turn, ""))
	}
	add("first", "first reply", "2026-11-19T10:01:00Z")
	telegramFlush(t, rt, &state)
	chat, err := ensureChatLocked(rt.workspace.Path, resource, "disksing")
	if err != nil {
		t.Fatal(err)
	}
	view, err := rt.server.agents.chatHistory(context.Background(), rt.workspace.Path, chat)
	if err != nil || len(view.Messages) != 1 {
		t.Fatal("missing first reply", err)
	}
	mustChatOK(t, chatCall(t, rt.server.agents, rt.workspace, resource, "disksing", "read", chatRequest{ID: view.Messages[0].ID}, false))
	add("late", "late reply", "2026-11-19T10:00:00Z")
	req := httptest.NewRequest("GET", "/api/workspaces/"+rt.workspace.ID+"/resources/"+resource+"/chat?limit=1&after="+view.Messages[0].ID+"&afterPosition=1", nil)
	req.Header.Set(workspaceUserHeader, "disksing")
	response := httptest.NewRecorder()
	rt.server.handleWorkspace(response, req)
	mustChatOK(t, response)
	var page publicChat
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || !page.Reset || page.EndPosition != 2 || page.UnreadCount != 1 || page.HasOlder == nil || !*page.HasOlder {
		t.Fatalf("late reply did not invalidate offset or count as unread: %+v %v", page, err)
	}
	for range 3 {
		state = telegramLoad(t, rt)
		rt.nextSend = map[int64]time.Time{}
		telegramFlush(t, rt, &state)
	}
	counts := map[string]int{}
	for _, call := range api.calls {
		if call.Method == "sendMessage" {
			counts[call.Body["text"].(string)]++
		}
	}
	if counts["first reply"] != 1 || counts["late reply"] != 1 {
		t.Fatalf("wrong Telegram delivery counts: %v", counts)
	}
}
