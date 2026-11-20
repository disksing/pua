package serve

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestChatHistoryPlacesPreviewedInputsAfterWholeReply(t *testing.T) {
	manager, workspace, fake, _ := workflowFixture(t)
	const resource = "project1.task1"
	chat := resourceChat{ResourceID: resource, ID: sharedChatID(resource), Version: 4}
	stamp := func(clock string) string { return "2026-11-20T02:52:" + clock + "Z" }
	chat.Messages = []chatMessage{
		{ID: "ordinary", Role: "user", Text: "Ordinary input", CreatedAt: stamp("01")},
		{ID: "icons", Role: "user", Text: "Distinguish Turn icons", CreatedAt: stamp("02.829358")},
		{ID: "follow-up", Role: "user", Text: "Second queued input", CreatedAt: stamp("03.010")},
	}
	work := turnWork{ID: "prior-work", SessionID: "generation-34", TurnID: "prior-turn", HistoryChat: true,
		Context: providerMessageContext{ChatVisible: true}}
	if err := saveTurnWork(workspace.Path, resource, turnWorkFile{Version: 1, ResourceID: resource, Works: []turnWork{work}}); err != nil {
		t.Fatal(err)
	}
	preview := func(message chatMessage, event int64, at string) agentHubTurnItem {
		t.Helper()
		payload, err := marshalPUAMessagePayload(puaMessagePayload{Schema: puaMessagePayloadSchema, Role: "system", Type: "queue_preview",
			Causation: &resourceMessageCausation{Type: "queue_preview", MessageID: chat.executionInputIDs(message)[0]}})
		if err != nil {
			t.Fatal(err)
		}
		return agentHubTurnItem{Type: "message", Role: "system", Steer: true, Payload: payload, StartEventID: event, StartedAt: at}
	}
	first := agentHubTurnItem{Type: "message", Role: "assistant", Text: "The empty Session car",
		StartEventID: 1266, EndEventID: 1266, StartedAt: stamp("02.087398"), EndedAt: stamp("03.458138")}
	second := agentHubTurnItem{Type: "message", Role: "assistant", Text: "ds match the report.",
		StartEventID: 1268, EndEventID: 1272, StartedAt: stamp("03.495196"), EndedAt: stamp("04.310355")}
	later := agentHubTurnItem{Type: "message", Role: "assistant", Text: "I will check delivery first.",
		StartEventID: 1282, EndEventID: 1282, StartedAt: stamp("10.586962"), EndedAt: stamp("10.729023"), Complete: true}
	update := func(closed bool, items ...agentHubTurnItem) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.turns[work.SessionID] = map[string]agentHubTurn{work.TurnID: {
			TurnID: work.TurnID, TriggerMessageID: work.ID, Closed: closed, Items: items,
		}}
	}
	read := func(limit int, before string) publicChat {
		t.Helper()
		page, err := manager.historyPublicPage(context.Background(), workspace.Path, chat, limit, before, "", "reader")
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	texts := func(page publicChat) []string {
		result := make([]string, 0, len(page.Messages))
		for _, message := range page.Messages {
			result = append(result, message.Text)
		}
		return result
	}
	firstPreview := preview(chat.Messages[1], 1267, stamp("03.483332"))
	secondPreview := preview(chat.Messages[2], 1273, stamp("04.311"))
	update(false, first)
	if got := texts(read(10, "")); !reflect.DeepEqual(got, []string{"Ordinary input", "Distinguish Turn icons", "Second queued input"}) {
		t.Fatalf("unpublished fragment moved an ordinary input: %v", got)
	}
	update(false, first, firstPreview, second)
	live := read(10, "")
	if len(live.Messages) != 4 || live.Messages[2].StartEventID != 1266 || live.Messages[3].ID != "icons" {
		t.Fatalf("unexpected active projection: %+v", live)
	}
	update(true, first, firstPreview, second, secondPreview, agentHubTurnItem{Type: "activity", StartEventID: 1280}, later)
	manager = newAgentManager(manager.server)
	full := read(10, "")
	if got := texts(full); !reflect.DeepEqual(got, []string{"Ordinary input", first.Text, second.Text, "Distinguish Turn icons", "Second queued input", later.Text}) {
		t.Fatalf("wrong order after refresh: %v", got)
	}
	if full.Messages[3].CreatedAt != chat.Messages[1].CreatedAt || full.TimelineInputs["icons"] != firstPreview.StartedAt ||
		full.TimelineInputs["follow-up"] != secondPreview.StartedAt || full.TimelineInputs["ordinary"] != "" {
		t.Fatalf("unexpected send times or preview anchors: %+v", full)
	}
	tail := read(2, "")
	middle := read(2, tail.Messages[0].ID)
	older := read(2, middle.Messages[0].ID)
	continuation := middle.Messages[0]
	if group := continuation.Continuation; group == nil || group.ID != older.Messages[1].ID || group.Text != first.Text+second.Text ||
		continuation.StartEventID != 1268 || tail.Messages[1].StartEventID != 1282 || tail.EndPosition != 6 {
		t.Fatalf("page boundary lost reply text, event identity, or cursor: %+v %+v %+v", older, middle, tail)
	}
	raw, err := manager.chatHistory(context.Background(), workspace.Path, chat)
	if err != nil || raw.Messages[1].ID != "icons" || raw.Messages[3].startEventID != 1266 {
		t.Fatalf("non-Web history lost its original chronology: %+v %v", raw.Messages, err)
	}
}

func TestChatTurnProjectionKeepsCompletedRepliesSeparate(t *testing.T) {
	preview, err := marshalPUAMessagePayload(puaMessagePayload{Schema: puaMessagePayloadSchema, Role: "system", Type: "queue_preview",
		Causation: &resourceMessageCausation{Type: "queue_preview", MessageID: "input"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, boundary := range []agentHubTurnItem{
		{Type: "message", Steer: true, Payload: preview, StartEventID: 2},
		{Type: "message", Role: "user", Steer: true, StartEventID: 2},
	} {
		for _, completed := range []bool{false, true} {
			view := projectChatTurn(turnWork{}, agentHubTurn{Closed: true, Items: []agentHubTurnItem{
				{Type: "message", Role: "assistant", Text: "Before", StartEventID: 1, Complete: completed},
				boundary,
				{Type: "message", Role: "assistant", Text: "After", StartEventID: 3},
			}}, nil)
			if joined := view.messages[1].continuation != nil; joined != (!completed && len(boundary.Payload) > 0) {
				t.Fatalf("wrong continuation at boundary %+v (complete: %t): %+v", boundary, completed, view.messages)
			}
		}
	}
	_, _, fake, client := workflowFixture(t)
	work := turnWork{SessionID: "session", TurnID: "turn"}
	turn := agentHubTurn{Closed: true, Items: []agentHubTurnItem{
		{Type: "message", Role: "assistant", Text: "Before", StartEventID: 1},
		{Type: "message", Steer: true, Payload: preview, StartEventID: 2},
		{Type: "message", Role: "assistant", Text: "After", StartEventID: 4},
	}}
	for _, eventType := range []string{"message.delivery", "message.assistant.completed"} {
		fake.mu.Lock()
		fake.events[work.SessionID] = []agentHubEvent{{ID: 3, Type: eventType, SessionID: work.SessionID, TurnID: work.TurnID}}
		fake.mu.Unlock()
		view := projectChatTurn(work, turn, func(after, before int64) (bool, error) {
			return chatStreamContinues(context.Background(), client, work, after, before)
		})
		if joined := view.messages[1].continuation != nil; joined != (eventType == "message.delivery") || view.retry {
			t.Fatalf("wrong continuation around %s: %+v", eventType, view)
		}
	}
}

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
		rt.server.agents.chatHistoryCache.put(chatTurnKey{rt.workspace.Path, resource, work.SessionID, id, id}, projectChatTurn(work, turn, nil))
	}
	add("first", "first reply", "2026-11-19T10:01:00Z")
	telegramFlush(t, rt, &state)
	chat, err := ensureChatLocked(rt.workspace.Path, resource)
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
