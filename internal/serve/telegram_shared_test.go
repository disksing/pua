package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func TestTelegramSharedChatForwardsToBothTopicsWithoutEcho(t *testing.T) {
	rt, api, st := telegramFixture(t)
	resource := "project1.task1"
	first := telegramOpen(t, rt, &st, resource)
	st.Accounts[888] = "other"
	telegramProcess(t, rt, &st, telegramInput(2, 888, 0, "/open "+resource))
	var second telegramRoute
	for _, route := range st.Routes {
		if route.User == "other" {
			second = route
		}
	}
	if second.ThreadID == 0 || second.ThreadID == first.ThreadID {
		t.Fatal("second user did not receive a separate topic")
	}
	start := len(api.calls)
	telegramProcess(t, rt, &st, telegramInput(10, 777, first.ThreadID, "first telegram"))
	telegramProcess(t, rt, &st, telegramInput(11, 888, second.ThreadID, "second telegram"))
	telegramPublish(t, rt, resource, "shared-answer", "shared answer")
	for range 5 {
		rt.nextSend = map[int64]time.Time{}
		telegramFlush(t, rt, &st)
		st = telegramLoad(t, rt)
	}
	delivered := map[int64][]string{}
	for _, call := range api.calls[start:] {
		if call.Method == "sendMessage" {
			thread := int64(call.Body["message_thread_id"].(float64))
			delivered[thread] = append(delivered[thread], call.Body["text"].(string))
		}
	}
	firstText := strings.Join(delivered[first.ThreadID], "\n")
	secondText := strings.Join(delivered[second.ThreadID], "\n")
	if strings.Contains(firstText, "first telegram") || !strings.Contains(firstText, "other (Telegram):\nsecond telegram") ||
		strings.Contains(secondText, "second telegram") || !strings.Contains(secondText, "disksing (Telegram):\nfirst telegram") ||
		strings.Count(firstText, "shared answer") != 1 || strings.Count(secondText, "shared answer") != 1 {
		t.Fatalf("incorrect shared delivery: %v", delivered)
	}
}

func TestTelegramLegacyPendingMigrationAndRecovery(t *testing.T) {
	rt, _, st := telegramFixture(t)
	resource, user := "project1.task1", "disksing"
	old := resourceChat{ID: chatID(resource, user), ResourceID: resource, User: user, Open: true, Epoch: 3, Token: "old-token",
		Messages: []chatMessage{{ID: "reply", Role: "agent", Text: "old reply"}}}
	if err := saveChatLocked(rt.workspace.Path, old); err != nil {
		t.Fatal(err)
	}
	st.Accounts[777] = user
	st.Routes = append(st.Routes, telegramRoute{User: user, ChatID: 777, ThreadID: 41, Resource: resource, Epoch: 3,
		Pending: &telegramDelivery{Text: "old reply", Chunks: []string{"old reply"}, NextCursor: 1, Status: "unknown"}})
	ws, err := app.OpenWorkspace(rt.workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, ready, changed := rt.checkRoute(&st, &st.Routes[0], ws, false)
	route := &st.Routes[0]
	if !ready || !changed || !route.SharedChat || route.Pending == nil || route.Pending.Status != "unknown" || route.Pending.NextCursor != 1 {
		t.Fatalf("unknown pending delivery was not preserved: %+v", route)
	}
	if err := rt.save(st); err != nil {
		t.Fatal(err)
	}
	telegramFlush(t, rt, &st)
	if route.Pending == nil || route.Pending.Status != "unknown" || route.Cursor != 0 {
		t.Fatal("unknown delivery was retried automatically")
	}

	// A legacy topic that missed a close/reopen requires explicit recovery;
	// its unconfirmed delivery is retained for review rather than replayed.
	route.SharedChat = false
	route.Epoch = old.Epoch
	route.Cursor = 0
	route.Pending = &telegramDelivery{Text: "old reply", Chunks: []string{"old reply"}, NextCursor: 1, Status: "unknown"}
	mustChatOK(t, chatCall(t, rt.server.agents, rt.workspace, resource, user, "close", chatRequest{}, false))
	mustChatOK(t, chatCall(t, rt.server.agents, rt.workspace, resource, user, "open", chatRequest{}, false))
	_, _, ready, _ = rt.checkRoute(&st, route, ws, false)
	if ready {
		t.Fatal("legacy route resumed across an unverified epoch")
	}
	if err := rt.openTopic(context.Background(), &st, user, 777, 41, resource, true, true, func(string, [][]telegramButton) {}); err != nil {
		t.Fatal(err)
	}
	route = &st.Routes[0]
	if !route.SharedChat || route.Pending != nil || len(route.Held) != 1 ||
		route.Held[0].Pending == nil || route.Held[0].Pending.Status != "unknown" {
		t.Fatalf("confirmed recovery discarded the old pending delivery: %+v", route)
	}
}

func TestTelegramLegacyCursorSurvivesInterleavedAndRemappedMessages(t *testing.T) {
	resource, user := "project1.task1", "disksing"
	legacyID := chatID(resource, user)
	otherID := chatID(resource, "other")
	chat := resourceChat{
		Epoch: 5, MigrationEnd: 4,
		LegacySources: map[string]legacyChatSource{
			legacyID: {User: user, Open: true, Epoch: 2, MessageIDs: []string{"remapped", "own-answer"}},
		},
		Messages: []chatMessage{
			{ID: "other", Role: "agent", Text: "other", LegacyChatID: otherID},
			{ID: "remapped", Role: "user", Text: "mine", Sender: user, LegacyChatID: legacyID, LegacyID: "telegram-99-1"},
			{ID: "other-answer", Role: "agent", Text: "other answer", LegacyChatID: otherID, LegacyIndex: 1},
			{ID: "own-answer", Role: "agent", Text: "answer", LegacyChatID: legacyID, LegacyIndex: 1},
		},
	}
	route := telegramRoute{Resource: resource, User: user, Epoch: 2, Cursor: 0,
		Pending: &telegramDelivery{Status: "unknown", NextCursor: 1}}
	if !route.migrateSharedChat(chat) || route.Cursor != 1 || route.Pending.NextCursor != 2 || route.Pending.Status != "unknown" {
		t.Fatalf("interleaved cursor or pending state changed: %+v", route)
	}
	if telegramForwardable(chat.Messages[0], &route) || telegramForwardable(chat.Messages[1], &route) ||
		telegramForwardable(chat.Messages[2], &route) || !telegramForwardable(chat.Messages[3], &route) {
		t.Fatal("legacy topic would replay another user's history or echo its remapped input")
	}
}

func TestTelegramUserDeletionRevokesPairingAndHoldsDelivery(t *testing.T) {
	rt, _, st := telegramFixture(t)
	opened := telegramOpen(t, rt, &st, "project1.task1")
	st.Routes[0].Pending = &telegramDelivery{Text: "unconfirmed", Chunks: []string{"unconfirmed"}, NextCursor: 1, Status: "unknown"}
	if err := rt.save(st); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/workspaces/"+rt.workspace.ID+"/users/disksing", nil)
	out := httptest.NewRecorder()
	rt.server.handleWorkspace(out, req)
	if out.Code != http.StatusNoContent {
		t.Fatalf("delete user: %d", out.Code)
	}
	st = telegramLoad(t, rt)
	if _, paired := st.Accounts[777]; paired || len(st.Routes) != 1 ||
		st.Routes[0].PauseReason != telegramPauseDeleted || st.Routes[0].Pending == nil {
		t.Fatal("deleting user did not revoke pairing and retain delivery")
	}
	ws, err := app.OpenWorkspace(rt.workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ws.RegisterUser("disksing"); err != nil {
		t.Fatal(err)
	}
	st.Accounts[777] = "disksing" // A later explicit pairing.
	_, _, ready, _ := rt.checkRoute(&st, &st.Routes[0], ws, false)
	if ready {
		t.Fatal("recreated username resumed old deliveries without review")
	}
	// A valid cursor is required for confirmed recovery; here the pending
	// delivery's next cursor belongs to the message retained in shared Chat.
	chatMu.Lock()
	c, err := loadChatLocked(rt.workspace.Path, sharedChatID(opened.Resource))
	if err == nil {
		c.Messages = append(c.Messages, chatMessage{ID: "reply", Role: "agent", Text: "unconfirmed"})
		err = saveChatLocked(rt.workspace.Path, c)
	}
	chatMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = rt.openTopic(context.Background(), &st, "disksing", 777, opened.ThreadID, opened.Resource, true, true, func(string, [][]telegramButton) {}); err != nil {
		t.Fatal(err)
	}
	if st.Routes[0].Pending != nil || len(st.Routes[0].Held) != 1 ||
		st.Routes[0].Held[0].Pending == nil || st.Routes[0].Held[0].Pending.Status != "unknown" {
		t.Fatal("confirmed recovery lost or retried the unconfirmed delivery")
	}
}
