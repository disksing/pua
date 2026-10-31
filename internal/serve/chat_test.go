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
	"testing"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/workspacepath"
)

func chatTestManager(t *testing.T) (*agentManager, serveWorkspace) {
	t.Helper()
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	t.Cleanup(hub.Close)
	m, w, _ := newRuntimeTestManager(t, hub.URL)
	ws, err := app.OpenWorkspace(w.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"disksing", "other"} {
		if _, err = ws.RegisterUser(user); err != nil {
			t.Fatal(err)
		}
	}
	return m, w
}
func chatCall(t *testing.T, m *agentManager, w serveWorkspace, resource, user, op string, body any, agent bool) *httptest.ResponseRecorder {
	t.Helper()
	data, _ := json.Marshal(body)
	method := http.MethodPost
	if body == nil {
		method = http.MethodGet
	}
	path := "/api/workspaces/" + w.ID + "/resources/" + resource + "/chat"
	if op != "" {
		path += "/" + op
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	req.Header.Set(workspaceUserHeader, user)
	if agent {
		c, err := loadDialogue(w.Path, dialoguePrefix+chatID(resource, user))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-PUA-Chat", c.ID)
		req.Header.Set("X-PUA-Chat-Token", c.Token)
	}
	out := httptest.NewRecorder()
	m.server.handleWorkspace(out, req)
	return out
}
func mustChatOK(t *testing.T, r *httptest.ResponseRecorder) {
	t.Helper()
	if r.Code != 200 {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
}

func TestChatMessagePaginationAndLegacyResponses(t *testing.T) {
	m, w := chatTestManager(t)
	resource, user := "project1.task1", "disksing"
	mustChatOK(t, chatCall(t, m, w, resource, user, "open", chatRequest{}, false))
	chatMu.Lock()
	c, err := loadChatLocked(w.Path, chatID(resource, user))
	if err != nil {
		chatMu.Unlock()
		t.Fatal(err)
	}
	for i := range 8 {
		c.Messages = append(c.Messages, chatMessage{ID: fmt.Sprintf("m%d", i), Role: "agent", Text: fmt.Sprintf("message %d", i)})
	}
	err = saveChatLocked(w.Path, c)
	chatMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, operation, query string, body any) *httptest.ResponseRecorder {
		t.Helper()
		data, _ := json.Marshal(body)
		req := httptest.NewRequest(method, "/api/workspaces/"+w.ID+"/resources/"+resource+"/chat"+operation+query, bytes.NewReader(data))
		req.Header.Set(workspaceUserHeader, user)
		out := httptest.NewRecorder()
		m.server.handleWorkspace(out, req)
		return out
	}
	decode := func(r *httptest.ResponseRecorder) publicChat {
		t.Helper()
		mustChatOK(t, r)
		var page publicChat
		if err := json.Unmarshal(r.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	ids := func(messages []chatMessage) string {
		var result []string
		for _, message := range messages {
			result = append(result, message.ID)
		}
		return strings.Join(result, ",")
	}
	latest := decode(call("GET", "", "?limit=3", nil))
	if ids(latest.Messages) != "m5,m6,m7" || latest.HasOlder == nil || !*latest.HasOlder || latest.HasNewer == nil || *latest.HasNewer || latest.UnreadCount != 8 {
		t.Fatalf("unexpected latest page: %+v", latest)
	}
	older := decode(call("GET", "", "?limit=3&before=m5", nil))
	if ids(older.Messages) != "m2,m3,m4" || !*older.HasOlder || !*older.HasNewer {
		t.Fatalf("unexpected older page: %+v", older)
	}
	first := decode(call("GET", "", "?limit=3&before=m2", nil))
	if ids(first.Messages) != "m0,m1" || *first.HasOlder {
		t.Fatalf("unexpected first page: %+v", first)
	}
	mustChatOK(t, chatCall(t, m, w, resource, user, "publish", chatRequest{ID: "new", Text: "new message"}, true))
	after := decode(call("GET", "", "?limit=1&after=m6", nil))
	if ids(after.Messages) != "m7" || !*after.HasNewer {
		t.Fatalf("unexpected forward page: %+v", after)
	}
	after = decode(call("GET", "", "?limit=1&after=m7", nil))
	if ids(after.Messages) != "new" || *after.HasNewer {
		t.Fatalf("unexpected last forward page: %+v", after)
	}
	empty := decode(call("GET", "", "?limit=3&after=new", nil))
	if empty.Messages == nil || len(empty.Messages) != 0 || !*empty.HasOlder || *empty.HasNewer {
		t.Fatalf("unexpected empty forward page: %+v", empty)
	}
	// A later append must not shift an older page selected by message ID.
	if got := decode(call("GET", "", "?limit=3&before=m5", nil)); ids(got.Messages) != "m2,m3,m4" {
		t.Fatalf("append shifted older page: %+v", got)
	}
	read := decode(call("POST", "/read", "?limit=1", chatRequest{ID: "m7"}))
	if ids(read.Messages) != "new" || read.UnreadCount != 1 {
		t.Fatalf("read response was not bounded or did not preserve unread count: %+v", read)
	}
	legacy := decode(call("GET", "", "", nil))
	if len(legacy.Messages) != 9 || legacy.HasOlder != nil || legacy.HasNewer != nil {
		t.Fatalf("legacy response changed: %+v", legacy)
	}
	for _, query := range []string{"?limit=0", "?limit=101", "?limit=bad", "?before=m5", "?limit=3&before=m5&after=m6", "?limit=3&before=missing"} {
		if got := call("GET", "", query, nil); got.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", query, got.Code, got.Body.String())
		}
	}
}

func TestChatLifecycleIsolationAndOutboxReplay(t *testing.T) {
	m, w := chatTestManager(t)
	resource := "project1.task1"
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
	body := chatRequest{ID: "request-1", Text: "Please work in the background"}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "messages", body, false))
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "messages", body, false))
	c, _ := loadDialogue(w.Path, dialoguePrefix+chatID(resource, "disksing"))
	if len(c.Messages) != 1 || len(c.Outbox) != 1 {
		t.Fatalf("duplicate input: %#v", c)
	}
	for range 2 {
		if err := m.reconcileWorkspaceChats(context.Background(), w); err != nil {
			t.Fatal(err)
		}
	}
	mailbox, err := loadResourceMailboxForResource(w.Path, resource)
	if err != nil {
		t.Fatal(err)
	}
	if len(mailbox.Messages) != 1 {
		t.Fatalf("outbox replay created %d messages", len(mailbox.Messages))
	}
	internal, err := loadResourceMailboxForResource(w.Path, c.address())
	if err != nil {
		t.Fatal(err)
	}
	if len(internal.Messages) != 0 {
		t.Fatal("user input created a dialogue mailbox")
	}
	addresses, err := listResourceMailboxResourceIDs(w.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		if strings.HasPrefix(address, dialoguePrefix) {
			t.Fatal("chat polling created legacy transport state")
		}
	}
	other := chatCall(t, m, w, resource, "other", "", nil, false)
	mustChatOK(t, other)
	if strings.Contains(other.Body.String(), body.Text) {
		t.Fatal("chat leaked across users")
	}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "close", chatRequest{}, false))
	if got := chatCall(t, m, w, resource, "disksing", "messages", chatRequest{ID: "late", Text: "late"}, false); got.Code != 409 {
		t.Fatal(got.Code)
	}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
	reopened, _ := loadDialogue(w.Path, c.address())
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "messages", body, false))
	if reopened.Epoch != c.Epoch+1 || reopened.Token == c.Token || len(reopened.Messages) != 1 {
		t.Fatal("reopen lost history or reused authority")
	}
}
func TestChatAgentReplyCapabilityAndIsolation(t *testing.T) {
	m, w := chatTestManager(t)
	resource := "project1.task1"
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
	c, _ := loadDialogue(w.Path, dialoguePrefix+chatID(resource, "disksing"))
	publish := chatRequest{ID: "reply-1", Text: "Work completed."}
	if r := chatCall(t, m, w, resource, "disksing", "publish", publish, false); r.Code != 403 {
		t.Fatal(r.Code)
	}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "publish", publish, true))
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "publish", publish, true))
	call := func(target, token, generation string) int {
		data, _ := json.Marshal(chatRequest{ID: "new", Text: "answer"})
		req := httptest.NewRequest("POST", "/api/workspaces/"+w.ID+"/resources/"+target+"/chat/publish", bytes.NewReader(data))
		req.Header.Set("X-PUA-Chat", c.ID)
		req.Header.Set("X-PUA-Chat-Token", token)
		req.Header.Set("X-PUA-Chat-Generation", generation)
		out := httptest.NewRecorder()
		m.server.handleWorkspace(out, req)
		return out.Code
	}
	if call("project1", c.Token, "") != 403 {
		t.Fatal("cross-resource capability accepted")
	}
	if call(resource, "wrong", "") != 409 {
		t.Fatal("wrong capability accepted")
	}
	if call(resource, c.Token, "legacy-dialogue-generation") != 409 {
		t.Fatal("legacy dialogue published")
	}
	mustChatOK(t, chatCall(t, m, w, resource, "other", "open", chatRequest{}, false))
	other := chatCall(t, m, w, resource, "other", "", nil, false)
	if strings.Contains(other.Body.String(), publish.Text) {
		t.Fatal("reply leaked across users")
	}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "close", chatRequest{}, false))
	if call(resource, c.Token, "") != 409 {
		t.Fatal("closed publication")
	}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
	if call(resource, c.Token, "") != 409 {
		t.Fatal("stale publication after reopen")
	}
	current, _ := loadDialogue(w.Path, c.address())
	if len(current.Messages) != 1 {
		t.Fatal("reply duplicated or history lost")
	}
	for _, op := range []string{"dispatch", "timer", "binding"} {
		if got := chatCall(t, m, w, resource, "disksing", op, chatRequest{ID: "legacy", Text: "do work"}, op != "binding"); got.Code != 409 {
			t.Fatalf("%s: %d", op, got.Code)
		}
	}
}

func TestChatBindingDoesNotOpenAndInboxDiscarded(t *testing.T) {
	m, w := chatTestManager(t)
	if got := chatCall(t, m, w, "project1.task1", "disksing", "binding", chatRequest{Binding: app.AgentBinding{Kind: "agent", Name: "example"}}, false); got.Code != 409 {
		t.Fatal(got.Code)
	}
	c, _ := loadDialogue(w.Path, dialoguePrefix+chatID("project1.task1", "disksing"))
	if c.Open || c.Epoch != 0 {
		t.Fatal("model selection opened chat")
	}
	path := filepath.Join(workspacepath.ControlDir(w.Path), "users", "disksing", "inbox.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"messages":[{"text":"legacy"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.reconcileWorkspaceChats(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("Inbox not discarded")
	}
	req := httptest.NewRequest("GET", "/api/workspaces/"+w.ID+"/users/disksing/messages", nil)
	out := httptest.NewRecorder()
	m.server.handleWorkspace(out, req)
	if out.Code != 410 {
		t.Fatal(out.Code)
	}
	mustChatOK(t, chatCall(t, m, w, "workspace", "disksing", "", nil, false))
	if r := chatCall(t, m, w, "workspace", "disksing", "close", chatRequest{}, false); r.Code != 409 {
		t.Fatal("Workspace closed")
	}
}

func TestChatUsesExecutionRuntimeWhileBusyAndAfterReopen(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	fake.enforceMessageIDs = true
	hub := httptest.NewServer(fake)
	defer hub.Close()
	m, w, _ := newRuntimeTestManager(t, hub.URL)
	ws, _ := app.OpenWorkspace(w.Path)
	_, _ = ws.RegisterUser("disksing")
	resource := "project1.task1"
	_ = acceptTestResourceMessage(t, m, w, resource, "internal work", resourceMessageModeSteer, nil)
	original, found, err := currentResourceGeneration(w.Path, resource)
	if err != nil || !found {
		t.Fatalf("execution missing: %v", err)
	}
	fake.mu.Lock()
	session := fake.sessions[original.AgentHubSessionID]
	session.State = "running"
	session.CurrentTurnID = "active-work"
	session.InputCapabilities.Steer = true
	fake.sessions[session.ID] = session
	fake.appendLocked(session.ID, "message.assistant.completed", map[string]any{"text": "private internal final"})
	fake.appendLocked(session.ID, "tool.call", map[string]any{"summary": "private tool output"})
	fake.mu.Unlock()
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
	for _, id := range []string{"first", "second"} {
		mustChatOK(t, chatCall(t, m, w, resource, "disksing", "messages", chatRequest{ID: id, Text: "User question " + id}, false))
		if err = m.reconcileWorkspaceChats(context.Background(), w); err != nil {
			t.Fatal(err)
		}
		if err = m.withResourceController(context.Background(), w, resource, func() error { return m.reconcileResourceMailboxLocked(context.Background(), w, resource) }); err != nil {
			t.Fatal(err)
		}
		current, _, _ := currentResourceGeneration(w.Path, resource)
		if current.GenerationID != original.GenerationID {
			t.Fatal("chat replaced running execution")
		}
		c, _ := loadDialogue(w.Path, dialoguePrefix+chatID(resource, "disksing"))
		if _, found, _ := currentResourceGeneration(w.Path, c.address()); found {
			t.Fatal("dialogue generation created")
		}
		mailbox, _ := loadResourceMailboxForResource(w.Path, resource)
		delivered := false
		for _, message := range mailbox.Messages {
			if message.Causation != nil && message.Causation.MessageID == "input-"+id {
				delivered = message.Status == resourceMessageDelivered
				fake.mu.Lock()
				input := fake.messageInputs[message.ID]
				fake.mu.Unlock()
				if !strings.Contains(input.Text, "pua chat publish --reply-to="+c.ID+"."+c.Token) || !strings.Contains(input.Text, "NEVER automatic public replies") {
					t.Fatal("reply instructions missing")
				}
				if message.ActualMode != resourceMessageModeSteer {
					t.Fatalf("busy input not steered: %s", message.ActualMode)
				}
			}
		}
		if !delivered {
			t.Fatal("user input not delivered")
		}
		mustChatOK(t, chatCall(t, m, w, resource, "disksing", "publish", chatRequest{ID: "answer-" + id, Text: "Answer " + id}, true))
		mustChatOK(t, chatCall(t, m, w, resource, "disksing", "close", chatRequest{}, false))
		if err = m.reconcileWorkspaceChats(context.Background(), w); err != nil {
			t.Fatal(err)
		}
		fake.mu.Lock()
		state := fake.sessions[original.AgentHubSessionID].State
		fake.mu.Unlock()
		if state != "running" {
			t.Fatalf("closure interrupted execution: %s", state)
		}
		mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
	}
	c, _ := loadDialogue(w.Path, dialoguePrefix+chatID(resource, "disksing"))
	if len(c.Messages) != 4 {
		t.Fatalf("internal work leaked into Chat: %d", len(c.Messages))
	}
}

func TestChatNotesAndReadCursorWithoutTimer(t *testing.T) {
	m, w := chatTestManager(t)
	resource := "project1.task1"
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
	c, _ := loadDialogue(w.Path, dialoguePrefix+chatID(resource, "disksing"))
	rec := generationRecord{ID: "timer-gen", GenerationID: "timer-gen", Generation: 1, WorkspaceID: w.ID, ResourceID: c.address(), OwnerResourceID: resource, ChatEpoch: c.Epoch, Status: "idle"}
	if err := saveGenerationRecord(w.Path, rec); err != nil {
		t.Fatal(err)
	}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "notes", chatRequest{Text: "Follow project1.task1; nothing published yet."}, true))
	if got := chatCall(t, m, w, resource, "disksing", "timer", chatRequest{Seconds: 60}, true); got.Code != 409 {
		t.Fatal(got.Code)
	}

	for _, id := range []string{"first", "second"} {
		mustChatOK(t, chatCall(t, m, w, resource, "disksing", "publish", chatRequest{ID: id, Text: id}, true))
	}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "read", chatRequest{ID: "first"}, false))
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/"+w.ID+"/chats", nil)
	req.Header.Set(workspaceUserHeader, "disksing")
	out := httptest.NewRecorder()
	m.server.handleWorkspace(out, req)
	var listing struct {
		Chats []struct {
			ResourceID string `json:"resourceId"`
			Unread     int    `json:"unread"`
		} `json:"chats"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &listing); err != nil || out.Code != 200 {
		t.Fatalf("list: %d %v", out.Code, err)
	}
	unread := -1
	for _, entry := range listing.Chats {
		if entry.ResourceID == resource {
			unread = entry.Unread
		}
	}
	if unread != 1 {
		t.Fatalf("read swallowed unseen message: %d", unread)
	}

	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "close", chatRequest{}, false))
	current, _ := loadDialogue(w.Path, c.address())
	if current.IntervalSeconds != 0 || current.Notes == "" || current.ReadThrough != 1 {
		t.Fatal("closure lost notes/read state or retained timer")
	}
}

func TestDeletingUserDisablesTheirDialogue(t *testing.T) {
	m, w := chatTestManager(t)
	mustChatOK(t, chatCall(t, m, w, "project1.task1", "disksing", "open", chatRequest{}, false))
	ws, err := app.OpenWorkspace(w.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err = ws.DeleteUser("disksing"); err != nil {
		t.Fatal(err)
	}
	denied := chatCall(t, m, w, "project1.task1", "disksing", "publish", chatRequest{ID: "late", Text: "late"}, true)
	if denied.Code != 403 {
		t.Fatalf("deleted user publication: %d", denied.Code)
	}
	if err = m.reconcileWorkspaceChats(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	c, err := loadDialogue(w.Path, dialoguePrefix+chatID("project1.task1", "disksing"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Open || c.Token != "" || c.IntervalSeconds != 0 {
		t.Fatal("deleted user retains active dialogue")
	}
}

func TestChatLegacyMigrationRetiresTimerAndPreservesDispatchedWork(t *testing.T) {
	m, w := chatTestManager(t)
	resource := "project1.task1"
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
	c, _ := loadDialogue(w.Path, dialoguePrefix+chatID(resource, "disksing"))
	c.Version = 2
	c.IntervalSeconds = 60
	c.NextCheckAt = "2020-01-01T00:00:00Z"
	c.Notes = "legacy audit notes"
	for _, id := range []string{"unsent", "queued", "delivered", "uncertain"} {
		_ = c.appendMessage(id, "user", id)
		_ = c.queue("input-"+id, c.address(), "legacy dialogue prompt "+id)
		if id != "unsent" {
			c.Outbox[len(c.Outbox)-1].Accepted = true
			_, err := acceptGeneratedMailboxMessage(w.Path, resourceMailboxMessage{ID: "chat-" + chatID(c.ID, "input-"+id), ResourceID: c.address(), Text: "legacy dialogue prompt " + id, Type: "chat_input", RequestedMode: "steer", Causation: &resourceMessageCausation{Type: "chat", MessageID: id}})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	_, err := mutateResourceMailboxForResource(w.Path, c.address(), func(box *resourceMailbox) error {
		for i := range box.Messages {
			if strings.Contains(box.Messages[i].Text, "delivered") {
				box.Messages[i].Status = resourceMessageDelivered
				box.Messages[i].AttemptCount = 1
			}
			if strings.Contains(box.Messages[i].Text, "uncertain") {
				box.Messages[i].Status = resourceMessageDelivering
				box.Messages[i].AttemptCount = 1
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.queue("tick-old", c.address(), "old timer")
	_ = c.queue("dispatch-existing", resource, "already delegated work")
	_ = c.appendMessage("reply", "agent", "preserved public reply")
	c.ReadThrough = 2
	data, _ := json.Marshal(c)
	if err = os.WriteFile(chatPath(w.Path, c.ID), data, 0600); err != nil {
		t.Fatal(err)
	}
	legacyRecord := generationRecord{ID: "legacy-missing", GenerationID: "legacy-missing", Generation: 1, ResourceID: c.address(), WorkspaceID: w.ID, RuntimeChannel: "dialogue", ChatEpoch: c.Epoch, Status: "starting", AgentHubAgentName: "fake-agent", SourceExternalID: c.address() + "/1"}
	if err = saveGenerationRecord(w.Path, legacyRecord); err != nil {
		t.Fatal(err)
	}
	m.waitBackground()
	restarted := newAgentManager(m.server)
	m.server.agents = restarted
	m = restarted
	if err = m.recoverAgentHubGenerations(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err = m.reconcileWorkspaceChats(context.Background(), w); err != nil {
			t.Fatal(err)
		}
	}
	next, _ := loadDialogue(w.Path, c.address())
	if next.Version != 3 || next.IntervalSeconds != 0 || next.NextCheckAt != "" || next.Token == c.Token || len(next.Messages) != 5 || next.ReadThrough != 2 || next.Notes != c.Notes {
		t.Fatal("migration lost history or retained old authority/timer")
	}
	mailbox, _ := loadResourceMailboxForResource(w.Path, resource)
	if len(mailbox.Messages) != 3 {
		t.Fatalf("expected two safe inputs and one dispatch, got %d", len(mailbox.Messages))
	}
	for _, message := range mailbox.Messages {
		if strings.Contains(message.Text, "legacy dialogue prompt") || strings.Contains(message.Text, "old timer") {
			t.Fatal("obsolete prompt replayed")
		}
	}
	legacy, _ := loadResourceMailboxForResource(w.Path, c.address())
	if len(legacy.Messages) != 3 {
		t.Fatal("legacy mailbox lost")
	}
	for _, message := range legacy.Messages {
		if message.Status == resourceMessageQueued || message.Status == resourceMessageDelivering {
			t.Fatal("legacy mailbox still deliverable")
		}
	}
	records, err := loadGenerationRecords(w.Path)
	if err != nil {
		t.Fatal(err)
	}
	preserved := false
	for _, record := range records {
		if record.GenerationID == legacyRecord.GenerationID {
			preserved = record.Retired && record.AgentHubSessionID == ""
		}
	}
	if !preserved {
		t.Fatal("missing dialogue generation was recreated or lost its audit record")
	}

	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "runtime/history/turns", nil, false))
	if got := chatCall(t, m, w, resource, "disksing", "runtime/turn/end", chatRequest{}, false); got.Code != 403 {
		t.Fatal("legacy controls remain enabled")
	}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "messages", chatRequest{ID: "after-restart", Text: "new question"}, false))
	if err = m.reconcileWorkspaceChats(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	mailbox, _ = loadResourceMailboxForResource(w.Path, resource)
	if len(mailbox.Messages) != 4 {
		t.Fatal("new input did not route to execution after restart")
	}
}

func TestChatReadOpenAndReconcileNeverScheduleChecks(t *testing.T) {
	m, w := chatTestManager(t)
	for _, resource := range []string{"workspace", "project1.task1"} {
		mustChatOK(t, chatCall(t, m, w, resource, "disksing", "", nil, false))
		mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
		for range 3 {
			mustChatOK(t, chatCall(t, m, w, resource, "disksing", "runtime/history/turns", nil, false))
			mustChatOK(t, chatCall(t, m, w, resource, "disksing", "runtime/status", nil, false))
			if err := m.reconcileWorkspaceChats(context.Background(), w); err != nil {
				t.Fatal(err)
			}
		}
		c, err := loadDialogue(w.Path, dialoguePrefix+chatID(resource, "disksing"))
		if err != nil {
			t.Fatal(err)
		}
		if c.IntervalSeconds != 0 || c.NextCheckAt != "" || len(c.Outbox) != 0 {
			t.Fatal("display/open created model work")
		}
		if _, found, err := currentResourceGeneration(w.Path, c.address()); err != nil || found {
			t.Fatalf("read/open created generation: %v %v", found, err)
		}
	}
}

func TestChatStreamUserSelectionIsScopedAndValidated(t *testing.T) {
	m, w := chatTestManager(t)
	for _, test := range []struct {
		path, header string
		want         int
	}{
		{"runtime/stream?user=disksing", "", 409}, // valid selection, no generation
		{"runtime/stream?user=missing", "", 400},
		{"runtime/stream?user=missing", "disksing", 409}, // header wins
		{"runtime/history/turns?user=disksing", "", 400},
		{"runtime/status?user=disksing", "", 400},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/workspaces/"+w.ID+"/resources/project1.task1/chat/"+test.path, nil)
		if test.header != "" {
			req.Header.Set(workspaceUserHeader, test.header)
		}
		out := httptest.NewRecorder()
		m.server.handleWorkspace(out, req)
		if out.Code != test.want {
			t.Errorf("%s: %d %s", test.path, out.Code, out.Body.String())
		}
	}
}

func TestChatAndEventReadCursorsRemainIndependentPerUser(t *testing.T) {
	m, w := chatTestManager(t)
	resource := "project1.task1"
	c := resourceChat{ID: chatID(resource, "disksing"), ResourceID: resource, User: "disksing", Open: true, Epoch: 1,
		Messages: []chatMessage{{ID: "a", Role: "agent", Text: "first"}, {ID: "u", Role: "user", Text: "mine"}, {ID: "b", Role: "agent", Text: "second"}}}
	if err := saveChatLocked(w.Path, c); err != nil {
		t.Fatal(err)
	}
	records := []generationRecord{}
	for i, id := range []string{resource, c.address()} {
		generation := fmt.Sprintf("gen-read-%d", i)
		records = append(records, generationRecord{ID: generation, GenerationID: generation, WorkspaceID: w.ID, ResourceID: id, Generation: 1, AgentHubSessionID: "session-" + generation, Status: "idle", TurnNumber: 3 + i, Cwd: w.Path})
	}
	if err := rewriteTestGenerationRecords(w.Path, records); err != nil {
		t.Fatal(err)
	}
	snapshot := func(user string) resourceSnapshot {
		t.Helper()
		tree, err := m.server.treeAt(context.Background(), w.Path, user)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range tree.Projects {
			for _, task := range p.Children {
				if task.ID == resource {
					wantActivity := task.Chat != nil && task.Chat.UnreadCount > 0
					if (len(tree.Activity.Unread) == 1) != wantActivity {
						t.Fatalf("chat activity does not follow chat cursor: %+v", tree.Activity.Unread)
					}
					return task
				}
			}
		}
		t.Fatal("task missing")
		return resourceSnapshot{}
	}
	assertCounts := func(chat, execution, dialogue int) {
		t.Helper()
		item := snapshot("disksing")
		if item.Chat == nil || item.Chat.UnreadCount != chat || item.UnreadCount != execution || item.DialogueEvents == nil || item.DialogueEvents.UnreadCount != dialogue {
			t.Fatalf("chat=%+v execution=%d dialogue=%+v", item.Chat, item.UnreadCount, item.DialogueEvents)
		}
	}
	read := func(user, suffix string, through, wantStatus int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/api/workspaces/"+w.ID+"/resources/"+resource+suffix, strings.NewReader(fmt.Sprintf(`{"throughTurnNumber":%d}`, through)))
		req.Header.Set(workspaceUserHeader, user)
		out := httptest.NewRecorder()
		m.server.handleWorkspace(out, req)
		if out.Code != wantStatus {
			t.Fatalf("read %s status=%d body=%s", suffix, out.Code, out.Body.String())
		}
	}
	assertCounts(2, 3, 4)
	other := snapshot("other")
	if other.Chat != nil || other.DialogueEvents != nil || other.UnreadCount != 3 {
		t.Fatalf("user state leaked: %+v", other)
	}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "read", chatRequest{ID: "a"}, false))
	assertCounts(1, 3, 4)
	read("disksing", "/chat/runtime/read", 4, 200)
	assertCounts(1, 3, 0)
	read("other", "/chat/runtime/read", 4, 400)
	read("disksing", "/read", 2, 200)
	assertCounts(1, 1, 0)
	read("disksing", "/read", 1, 200)
	assertCounts(1, 1, 0)
	// An active Turn must not move the completed read boundary forward.
	records[0].Status = "running"
	records[0].CurrentTurnID = "active"
	records[0].TurnNumber = 4
	if err := rewriteTestGenerationRecords(w.Path, records); err != nil {
		t.Fatal(err)
	}
	read("disksing", "/read", 4, 400)
	assertCounts(1, 1, 0)
	c, err := loadChatLocked(w.Path, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.Open = false
	if err := saveChatLocked(w.Path, c); err != nil {
		t.Fatal(err)
	}
	item := snapshot("disksing")
	if item.Chat.Open || item.Chat.UnreadCount != 1 {
		t.Fatalf("closing lost history/unread: %+v", item.Chat)
	}
	if snapshot("other").UnreadCount != 3 {
		t.Fatal("execution read leaked to other user")
	}
	// Reloading user state retains both independent runtime cursors.
	state, err := loadUIStateFile(userUIStatePath(w.Path, "disksing"))
	if err != nil {
		t.Fatal(err)
	}
	if *state.ResourceStates[resource].ReadTurnNumber != 2 || *state.ResourceStates[c.address()].ReadTurnNumber != 4 {
		t.Fatalf("persisted cursors: %+v", state.ResourceStates)
	}
}

func TestChatRetiresOnlyLegacySessionAndNeverResumesIt(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	m, w, _ := newRuntimeTestManager(t, hub.URL)
	ws, _ := app.OpenWorkspace(w.Path)
	_, _ = ws.RegisterUser("disksing")
	resource := "project1.task1"
	_ = acceptTestResourceMessage(t, m, w, resource, "execution continues", resourceMessageModeSteer, nil)
	execution, _, _ := currentResourceGeneration(w.Path, resource)
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
	c, _ := loadDialogue(w.Path, dialoguePrefix+chatID(resource, "disksing"))
	legacy := execution
	legacy.ID = "legacy-active"
	legacy.GenerationID = "legacy-active"
	legacy.ResourceID = c.address()
	legacy.OwnerResourceID = resource
	legacy.RuntimeChannel = "dialogue"
	legacy.ChatEpoch = c.Epoch
	legacy.SourceExternalID = c.address() + "/1"
	cfg, client, err := m.agentHubRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	source := &agentHubSource{App: agentHubSourceApp, InstanceID: legacy.SourceInstanceID, ExternalID: legacy.SourceExternalID, Metadata: map[string]string{"resourceId": c.address(), "generationId": legacy.GenerationID}}
	session, err := client.CreateSession(context.Background(), agentHubCreateSessionRequest{AgentName: "fake-agent", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	legacy.AgentHubSessionID = session.ID
	if err = saveGenerationRecord(w.Path, legacy); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	session = fake.sessions[session.ID]
	session.State = "running"
	session.CurrentTurnID = "old-dialogue-turn"
	fake.sessions[session.ID] = session
	fake.mu.Unlock()
	for range 4 {
		if err = m.pollAgentHubSessions(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	fake.mu.Lock()
	oldState := fake.sessions[session.ID].State
	executionState := fake.sessions[execution.AgentHubSessionID].State
	created := fake.nextSession
	fake.mu.Unlock()
	if oldState != "stopped" && oldState != "archived" {
		t.Fatalf("legacy session still active: %s", oldState)
	}
	if executionState != "running" {
		t.Fatalf("execution stopped: %s", executionState)
	}
	if _, err = m.createResourceGeneration(context.Background(), w, c.address(), w.Path, cfg, client, resolvedResourceAgent{}); err == nil {
		t.Fatal("new dialogue generation allowed")
	}
	if err = m.recoverAgentHubGeneration(context.Background(), cfg, client, w, legacy, nil); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.nextSession != created || len(fake.resumeEnvironments) != 0 {
		t.Fatal("legacy runtime revived")
	}
}

func TestChatInputPresentationDoesNotPromiseAutomaticFinal(t *testing.T) {
	c := resourceChat{ID: "chat-id", Token: "capability", ResourceID: "project1.task1", User: "Ada"}
	text := c.prompt("User question")
	for _, steer := range []bool{false, true} {
		got := providerMessageTextWithContext(resourceMailboxMessage{Role: "system", Type: "user_chat_input", Text: text, ProviderContext: &providerMessageContext{Language: "zh-CN", OpenerRole: "agent", OpenerResponse: providerResponseFinalOnly}}, steer)
		if got != text || !strings.Contains(got, "pua chat publish --reply-to=chat-id.capability") {
			t.Fatal("chat wrapped as internal message")
		}
	}
}
