package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/workspacepath"
)

func TestSharedChatConcurrentSendersReadCursorsAndEpoch(t *testing.T) {
	m, w := chatTestManager(t)
	resource := "project1.task1"
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
	c, err := loadDialogue(w.Path, dialoguePrefix+sharedChatID(resource))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	status := make(chan int, 4)
	for _, user := range []string{"disksing", "other"} {
		for range 2 {
			wg.Add(1)
			go func(user string) {
				defer wg.Done()
				body, _ := json.Marshal(chatRequest{ID: "same-client-id", Text: "hello from " + user})
				req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+w.ID+"/resources/"+resource+"/chat/messages", bytes.NewReader(body))
				req.Header.Set(workspaceUserHeader, user)
				result := httptest.NewRecorder()
				m.server.handleWorkspace(result, req)
				status <- result.Code
			}(user)
		}
	}
	wg.Wait()
	close(status)
	for code := range status {
		if code != http.StatusOK {
			t.Fatalf("concurrent send returned %d", code)
		}
	}
	c, err = loadDialogue(w.Path, c.address())
	if err != nil || len(c.Messages) != 2 || len(c.Outbox) != 2 {
		t.Fatalf("send/retry lost or duplicated inputs: messages=%d outbox=%d err=%v", len(c.Messages), len(c.Outbox), err)
	}
	ids, senders := map[string]bool{}, map[string]bool{}
	for _, message := range c.Messages {
		ids[message.ID], senders[message.Sender] = true, true
	}
	if len(ids) != 2 || !senders["disksing"] || !senders["other"] {
		t.Fatalf("sender or collision handling: ids=%v senders=%v", ids, senders)
	}
	if err := m.reconcileWorkspaceChats(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	mailbox, err := loadResourceMailboxForResource(w.Path, resource)
	if err != nil || len(mailbox.Messages) != 2 {
		t.Fatalf("mailbox: %d %v", len(mailbox.Messages), err)
	}
	for _, message := range mailbox.Messages {
		if message.Sender == nil || !senders[message.Sender.Name] ||
			message.Causation == nil || !message.Causation.AutomaticChat || message.Causation.ChatEpoch == 0 {
			t.Fatal("execution input lost its authenticated sender")
		}
	}
	saveTestPublicChatReply(t, w.Path, resource, "answer", "shared answer")
	other := chatCall(t, m, w, resource, "other", "", nil, false)
	mustChatOK(t, other)
	if strings.Contains(other.Body.String(), "originRoute") || strings.Contains(other.Body.String(), "clientId") ||
		strings.Contains(other.Body.String(), "legacyChatId") {
		t.Fatal("internal metadata exposed in public chat")
	}
	var page publicChat
	if err := json.Unmarshal(other.Body.Bytes(), &page); err != nil || len(page.Messages) != 3 || page.UnreadCount != 1 {
		t.Fatalf("shared page: %+v %v", page, err)
	}
	mustChatOK(t, chatCall(t, m, w, resource, "other", "read", chatRequest{ID: "answer"}, false))
	for _, test := range []struct {
		user string
		want int
	}{{"other", 0}, {"disksing", 1}} {
		response := chatCall(t, m, w, resource, test.user, "", nil, false)
		var got publicChat
		if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil || got.UnreadCount != test.want || !got.Open {
			t.Fatalf("%s unread or lifecycle: %+v %v", test.user, got, err)
		}
	}
	mustChatOK(t, chatCall(t, m, w, resource, "other", "close", chatRequest{}, false))
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
	got := chatCall(t, m, w, resource, "other", "", nil, false)
	if err := json.Unmarshal(got.Body.Bytes(), &page); err != nil || !page.Open || page.Epoch != c.Epoch+1 {
		t.Fatal("reopen not shared")
	}
}

func TestSharedChatKeepsMigratedHistoryAndCursorsOnRestart(t *testing.T) {
	m, w := chatTestManager(t)
	resource := "project1.task1"
	legacyID := chatID(resource, "disksing")
	original := resourceChat{ID: legacyID, ResourceID: resource, User: "disksing",
		Open: true, Epoch: 1, Token: "old-token",
		Messages: []chatMessage{{ID: "answer", Role: "agent", Text: "old text"}}}
	if err := saveChatLocked(w.Path, original); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(chatPath(w.Path, legacyID))
	if err != nil {
		t.Fatal(err)
	}
	migrated := resourceChat{
		ID: sharedChatID(resource), ResourceID: resource, Open: true, Epoch: 2,
		Messages: []chatMessage{
			{ID: "answer", Role: "agent", Text: "another user's reply"},
			{ID: "legacy-remapped-answer", Role: "agent", Text: "old text", LegacyChatID: legacyID, LegacyID: "answer"},
		},
		ReadCursors:   map[string]int{"disksing": 2, "other": 2},
		LegacyUnread:  map[string][]string{"disksing": {"legacy-remapped-answer"}},
		LegacySources: map[string]legacyChatSource{legacyID: {User: "disksing", Open: true, Epoch: 1, Token: "old-token", MessageIDs: []string{"legacy-remapped-answer"}}},
		MigrationEnd:  2,
	}
	if err := saveChatLocked(w.Path, migrated); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		m.waitBackground()
		m = newAgentManager(m.server)
		m.server.agents = m
		if err := m.reconcileWorkspaceChats(context.Background(), w); err != nil {
			t.Fatal(err)
		}
		response := chatCall(t, m, w, resource, "disksing", "?limit=1&before=legacy-remapped-answer", nil, false)
		if response.Code != http.StatusOK {
			t.Fatalf("mapped cursor failed after restart: %s", response.Body)
		}
		var page publicChat
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || len(page.Messages) != 1 ||
			page.Messages[0].ID != "answer" || page.UnreadCount != 1 {
			t.Fatalf("mapped history page = %#v (%v)", page, err)
		}
	}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "read", chatRequest{ID: "legacy-remapped-answer"}, false))
	current, err := loadDialogue(w.Path, migrated.address())
	if err != nil || current.snapshot("disksing").UnreadCount != 0 || current.snapshot("other").UnreadCount != 0 ||
		current.Messages[1].LegacyID != "answer" || current.LegacySources[legacyID].MessageIDs[0] != "legacy-remapped-answer" {
		t.Fatalf("read changed migrated history: %#v (%v)", current, err)
	}
	if after, err := os.ReadFile(chatPath(w.Path, legacyID)); err != nil || !bytes.Equal(after, before) {
		t.Fatal("legacy file changed")
	}
	if _, err := os.Stat(filepath.Join(workspacepath.ControlDir(w.Path), "chats-legacy-backup")); !os.IsNotExist(err) {
		t.Fatalf("obsolete backup created: %v", err)
	}
}

func TestWorkspaceChatStartsOpenWithoutImportingPerUserChat(t *testing.T) {
	m, w := chatTestManager(t)
	old := resourceChat{ID: chatID("workspace", "disksing"), ResourceID: "workspace", User: "disksing",
		Open: true, Epoch: 4, Token: "old-token"}
	if err := saveChatLocked(w.Path, old); err != nil {
		t.Fatal(err)
	}
	ws, err := app.OpenWorkspace(w.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.DeleteUser("disksing"); err != nil {
		t.Fatal(err)
	}
	response := chatCall(t, m, w, "workspace", "other", "", nil, false)
	mustChatOK(t, response)
	var chat publicChat
	if err := json.Unmarshal(response.Body.Bytes(), &chat); err != nil || !chat.Open || chat.Epoch != 1 {
		t.Fatalf("Workspace chat did not remain open: %+v %v", chat, err)
	}
}

func TestDeletingUserClearsSharedChatReadStateAndRevokesQueuedInput(t *testing.T) {
	m, w := chatTestManager(t)
	resource, user := "project1.task1", "disksing"
	legacyID := chatID(resource, user)
	shared := resourceChat{ID: sharedChatID(resource), ResourceID: resource, Open: true, Epoch: 2,
		Messages:      []chatMessage{{ID: "answer", Role: "agent", Text: "old reply", LegacyChatID: legacyID}},
		Outbox:        []chatDelivery{{ID: "input-message", Target: resource, Text: "old pending input", Epoch: 2, SourceChatID: legacyID}},
		LegacySources: map[string]legacyChatSource{legacyID: {User: user, Open: true, Token: "old-token"}},
		ReadCursors:   map[string]int{user: 1, "other": 1},
		HistoryReads:  map[string]map[string]int64{user: {"old-work": 2}},
		LegacyUnread:  map[string][]string{user: {"answer"}},
	}
	if err := saveChatLocked(w.Path, shared); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/workspaces/"+w.ID+"/users/"+user, nil)
	result := httptest.NewRecorder()
	m.server.handleWorkspace(result, req)
	if result.Code != http.StatusNoContent {
		t.Fatalf("delete user: %d %s", result.Code, result.Body.String())
	}
	ws, err := app.OpenWorkspace(w.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ws.RegisterUser(user); err != nil {
		t.Fatal(err)
	}
	current, err := loadDialogue(w.Path, shared.address())
	if err != nil || current.LegacySources[legacyID].Open || current.LegacySources[legacyID].Token != "" ||
		current.ReadCursors[user] != 0 || current.HistoryReads[user] != nil || current.LegacyUnread[user] != nil ||
		!current.Open || current.ReadCursors["other"] != 1 {
		t.Fatalf("deleted user retained shared chat state: %#v (%v)", current, err)
	}
	if err = m.reconcileWorkspaceChats(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	mailbox, err := loadResourceMailboxForResource(w.Path, resource)
	if err != nil || len(mailbox.Messages) != 0 {
		t.Fatalf("deleted user's pending input was dispatched: %d %v", len(mailbox.Messages), err)
	}
}
