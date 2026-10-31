package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	oldToken := c.Token
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
			!strings.Contains(message.Text, "user "+message.Sender.Name) {
			t.Fatal("execution input lost its authenticated sender")
		}
	}
	publish := func(target, token string) int {
		body := strings.NewReader(`{"id":"answer","text":"shared answer"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+w.ID+"/resources/"+target+"/chat/publish", body)
		req.Header.Set("X-PUA-Chat-Token", token)
		out := httptest.NewRecorder()
		m.server.handleWorkspace(out, req)
		return out.Code
	}
	if publish(resource, oldToken) != 200 || publish(resource, oldToken) != 200 {
		t.Fatal("token-only publication or retry failed")
	}
	mustChatOK(t, chatCall(t, m, w, "project1", "other", "open", chatRequest{}, false))
	if publish("project1", oldToken) == 200 || publish(resource, "wrong") == 200 {
		t.Fatal("cross-resource or incorrect capability accepted")
	}
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
	if publish(resource, oldToken) == 200 {
		t.Fatal("closed capability accepted")
	}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
	if publish(resource, oldToken) == 200 {
		t.Fatal("reopened capability accepted")
	}
	got := chatCall(t, m, w, resource, "other", "", nil, false)
	if err := json.Unmarshal(got.Body.Bytes(), &page); err != nil || !page.Open {
		t.Fatal("reopen not shared")
	}
}

func TestSharedChatMigrationPreservesHistoryOutboxAndLegacyCapabilities(t *testing.T) {
	m, w := chatTestManager(t)
	resource := "project1.task1"
	original := map[string][]byte{}
	for i, user := range []string{"disksing", "other"} {
		old := resourceChat{
			ID: chatID(resource, user), ResourceID: resource, User: user, Open: true,
			Epoch: uint64(i + 1), Token: "old-token-" + user, ReadThrough: i + 1,
			Messages: []chatMessage{
				{ID: "same", Role: "user", Text: "input " + user, CreatedAt: fmt.Sprintf("2026-01-01T00:00:0%dZ", i)},
				{ID: "reply", Role: "agent", Text: "answer " + user, CreatedAt: fmt.Sprintf("2026-01-01T00:00:0%dZ", i+2)},
			},
			Outbox: []chatDelivery{{ID: "input-same", Target: resource, Text: "pending " + user, Epoch: uint64(i + 1)}},
		}
		if err := saveChatLocked(w.Path, old); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(chatPath(w.Path, old.ID))
		if err != nil {
			t.Fatal(err)
		}
		original[old.ID] = data
		if i == 0 {
			_, err = acceptGeneratedMailboxMessage(w.Path, resourceMailboxMessage{
				ID: "chat-" + chatID(old.ID, "input-same"), ResourceID: resource,
				Text: old.Outbox[0].Text, Type: "user_chat_input", RequestedMode: resourceMessageModeSteer,
				Causation: &resourceMessageCausation{Type: "chat", SourceResourceID: resource, MessageID: "input-same"},
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	first := chatCall(t, m, w, resource, "disksing", "", nil, false)
	mustChatOK(t, first)
	c, err := loadDialogue(w.Path, dialoguePrefix+sharedChatID(resource))
	if err != nil || !c.Open || len(c.Messages) != 4 || c.snapshot("disksing").UnreadCount != 1 || c.snapshot("other").UnreadCount != 0 {
		t.Fatalf("migration content/unread: %#v %v", c, err)
	}
	seen := map[string]bool{}
	for _, message := range c.Messages {
		if seen[message.ID] {
			t.Fatal("migration retained duplicate public IDs")
		}
		seen[message.ID] = true
		if message.Role == "user" && message.Sender == "" {
			t.Fatal("legacy sender lost")
		}
	}
	for _, message := range c.Messages {
		if message.LegacyChatID == chatID(resource, "disksing") && message.LegacyID == "reply" {
			mustChatOK(t, chatCall(t, m, w, resource, "disksing", "read", chatRequest{ID: message.ID}, false))
		}
	}
	c, _ = loadDialogue(w.Path, c.address())
	if c.snapshot("disksing").UnreadCount != 0 || c.snapshot("other").UnreadCount != 0 {
		t.Fatal("reading migrated history did not preserve independent cursors")
	}
	for id, want := range original {
		for _, path := range []string{chatPath(w.Path, id), filepath.Join(workspacepath.ControlDir(w.Path), "chats-legacy-backup", id+".json")} {
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatal("original or backup was changed")
			}
		}
	}
	for range 2 {
		if err := m.reconcileWorkspaceChats(context.Background(), w); err != nil {
			t.Fatal(err)
		}
	}
	mailbox, err := loadResourceMailboxForResource(w.Path, resource)
	if err != nil || len(mailbox.Messages) != 2 {
		t.Fatalf("legacy outbox replay duplicated or lost a delivery: %d %v", len(mailbox.Messages), err)
	}
	legacyPublish := func(user, id, text string) int {
		data, _ := json.Marshal(chatRequest{ID: id, Text: text})
		req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+w.ID+"/resources/"+resource+"/chat/publish", bytes.NewReader(data))
		req.Header.Set("X-PUA-Chat", chatID(resource, user))
		req.Header.Set("X-PUA-Chat-Token", "old-token-"+user)
		out := httptest.NewRecorder()
		m.server.handleWorkspace(out, req)
		return out.Code
	}
	for _, user := range []string{"disksing", "other"} {
		if legacyPublish(user, "reply", "answer "+user) != 200 ||
			legacyPublish(user, "future", "new "+user) != 200 ||
			legacyPublish(user, "future", "new "+user) != 200 {
			t.Fatalf("legacy retry/ID mapping failed for %s", user)
		}
	}
	c, _ = loadDialogue(w.Path, c.address())
	if len(c.Messages) != 6 {
		t.Fatalf("legacy retries duplicated messages: %d", len(c.Messages))
	}
	ws, err := app.OpenWorkspace(w.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.DeleteUser("disksing"); err != nil {
		t.Fatal(err)
	}
	if legacyPublish("disksing", "late", "late") != 403 || legacyPublish("other", "latest", "latest") != 200 {
		t.Fatal("legacy deleted-user qualification incorrect")
	}
	mustChatOK(t, chatCall(t, m, w, resource, "other", "close", chatRequest{}, false))
	mustChatOK(t, chatCall(t, m, w, resource, "other", "open", chatRequest{}, false))
	if legacyPublish("other", "after", "after") == 200 {
		t.Fatal("legacy capability survived close/reopen")
	}
}

func TestWorkspaceChatReopensAfterMigrationFromDeletedUser(t *testing.T) {
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
	if err := json.Unmarshal(response.Body.Bytes(), &chat); err != nil || !chat.Open || chat.Epoch != 5 {
		t.Fatalf("Workspace chat did not remain open: %+v %v", chat, err)
	}
}

func TestDeletingUserRevokesLegacyCapabilityAndQueuedInput(t *testing.T) {
	m, w := chatTestManager(t)
	resource, user := "project1.task1", "disksing"
	old := resourceChat{ID: chatID(resource, user), ResourceID: resource, User: user,
		Open: true, Epoch: 1, Token: "old-token",
		Messages: []chatMessage{{ID: "message", Role: "user", Text: "old input"}},
		Outbox:   []chatDelivery{{ID: "input-message", Target: resource, Text: "old pending input", Epoch: 1}}}
	if err := saveChatLocked(w.Path, old); err != nil {
		t.Fatal(err)
	}
	mustChatOK(t, chatCall(t, m, w, resource, "other", "", nil, false))
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
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/"+w.ID+"/resources/"+resource+"/chat/publish",
		strings.NewReader(`{"id":"late","text":"late"}`))
	req.Header.Set("X-PUA-Chat", old.ID)
	req.Header.Set("X-PUA-Chat-Token", old.Token)
	result = httptest.NewRecorder()
	m.server.handleWorkspace(result, req)
	if result.Code == http.StatusOK {
		t.Fatal("recreated username revived an old capability")
	}
	if err = m.reconcileWorkspaceChats(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	mailbox, err := loadResourceMailboxForResource(w.Path, resource)
	if err != nil || len(mailbox.Messages) != 0 {
		t.Fatalf("deleted user's pending input was dispatched: %d %v", len(mailbox.Messages), err)
	}
}
