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
		rec, _, _ := currentResourceGeneration(w.Path, c.address())
		req.Header.Set("X-PUA-Chat", c.ID)
		req.Header.Set("X-PUA-Chat-Token", c.Token)
		req.Header.Set("X-PUA-Chat-Generation", rec.GenerationID)
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
	mailbox, err := loadResourceMailboxForResource(w.Path, c.address())
	if err != nil {
		t.Fatal(err)
	}
	if len(mailbox.Messages) != 1 {
		t.Fatalf("outbox replay created %d messages", len(mailbox.Messages))
	}
	internal, err := loadResourceMailboxForResource(w.Path, resource)
	if err != nil {
		t.Fatal(err)
	}
	if len(internal.Messages) != 0 {
		t.Fatal("user input leaked to execution")
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
	if reopened.Epoch != c.Epoch+1 || reopened.Token == c.Token || len(reopened.Messages) != 1 {
		t.Fatal("reopen lost history or reused authority")
	}
}
func TestChatAgentFenceAndDispatch(t *testing.T) {
	m, w := chatTestManager(t)
	resource := "project1.task1"
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
	c, _ := loadDialogue(w.Path, dialoguePrefix+chatID(resource, "disksing"))
	record := generationRecord{ID: "chat-test-gen", GenerationID: "chat-test-gen", Generation: 1, WorkspaceID: w.ID, ResourceID: c.address(), OwnerResourceID: resource, ChatEpoch: c.Epoch, Status: "idle"}
	if err := saveGenerationRecord(w.Path, record); err != nil {
		t.Fatal(err)
	}
	publish := chatRequest{ID: "reply-1", Text: "I have delegated the work."}
	if r := chatCall(t, m, w, resource, "disksing", "publish", publish, false); r.Code != 403 {
		t.Fatalf("user published as agent: %d", r.Code)
	}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "publish", publish, true))
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "publish", publish, true))
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "dispatch", chatRequest{ID: "work-1", Text: "Implement the agreed change."}, true))
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "close", chatRequest{}, false))
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
	if r := chatCall(t, m, w, resource, "disksing", "publish", chatRequest{ID: "stale", Text: "old turn"}, true); r.Code != 409 {
		t.Fatalf("stale generation published: %d", r.Code)
	}
	if err := m.reconcileWorkspaceChats(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	mailbox, err := loadResourceMailboxForResource(w.Path, resource)
	if err != nil {
		t.Fatal(err)
	}
	if len(mailbox.Messages) != 1 {
		t.Fatalf("closing cancelled dispatched work: %d", len(mailbox.Messages))
	}
}
func TestChatBindingDoesNotOpenAndInboxDiscarded(t *testing.T) {
	m, w := chatTestManager(t)
	mustChatOK(t, chatCall(t, m, w, "project1.task1", "disksing", "binding", chatRequest{Binding: app.AgentBinding{Kind: "agent", Name: "example"}}, false))
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

func TestChatCreatesIndependentRuntimeAndClosesOnlyDialogue(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	m, w, _ := newRuntimeTestManager(t, hub.URL)
	ws, err := app.OpenWorkspace(w.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ws.RegisterUser("disksing"); err != nil {
		t.Fatal(err)
	}
	resource := "project1.task1"
	_ = acceptTestResourceMessage(t, m, w, resource, "execute independently", resourceMessageModeSteer, nil)
	internal, found, err := currentResourceGeneration(w.Path, resource)
	if err != nil || !found {
		t.Fatalf("internal: %v %v", found, err)
	}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "messages", chatRequest{ID: "first", Text: "How is it going?"}, false))
	if err = m.reconcileWorkspaceChats(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	c, _ := loadDialogue(w.Path, dialoguePrefix+chatID(resource, "disksing"))
	if err = m.withResourceController(context.Background(), w, c.address(), func() error { return m.reconcileResourceMailboxLocked(context.Background(), w, c.address()) }); err != nil {
		t.Fatal(err)
	}
	dialogue, found, err := currentResourceGeneration(w.Path, c.address())
	if err != nil || !found {
		t.Fatalf("dialogue: %v %v", found, err)
	}
	if internal.AgentHubSessionID == dialogue.AgentHubSessionID {
		t.Fatal("channels share Session")
	}
	// The browser's native SSE request uses query-based user selection and
	// must resolve only this user's dialogue generation, never execution.
	streamPath := "/api/workspaces/" + w.ID + "/resources/" + resource + "/chat/runtime/stream?user=disksing&generationId="
	stream := httptest.NewRecorder()
	m.server.handleWorkspace(stream, httptest.NewRequest(http.MethodGet, streamPath+dialogue.GenerationID, nil))
	if stream.Code != 200 || !strings.Contains(stream.Body.String(), dialogue.AgentHubSessionID) || strings.Contains(stream.Body.String(), internal.AgentHubSessionID) {
		t.Fatalf("dialogue stream crossed channels: %d %s", stream.Code, stream.Body.String())
	}
	wrongChannel := httptest.NewRecorder()
	m.server.handleWorkspace(wrongChannel, httptest.NewRequest(http.MethodGet, streamPath+internal.GenerationID, nil))
	if wrongChannel.Code != 409 {
		t.Fatalf("execution generation accepted by dialogue stream: %d", wrongChannel.Code)
	}
	fake.mu.Lock()
	launch := fake.sessions[dialogue.AgentHubSessionID].LaunchEnvironment
	fake.mu.Unlock()
	if launch["PUA_RESOURCE_ID"] != resource || launch["PUA_CHANNEL"] != "dialogue" || launch["PUA_CHAT_TOKEN"] != c.Token {
		t.Fatalf("incorrect dialogue launch context: keys=%v", len(launch))
	}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "publish", chatRequest{ID: "public", Text: "Work continues."}, true))
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "close", chatRequest{}, false))
	for range 5 {
		if err = m.reconcileWorkspaceChats(context.Background(), w); err != nil {
			t.Fatal(err)
		}
		if err = m.pollAgentHubSessions(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	fake.mu.Lock()
	internalSession := fake.sessions[internal.AgentHubSessionID]
	dialogueSession := fake.sessions[dialogue.AgentHubSessionID]
	fake.mu.Unlock()
	if internalSession.State != "running" {
		t.Fatalf("closing chat stopped execution: %s", internalSession.State)
	}
	if dialogueSession.State != "archived" && dialogueSession.State != "stopped" {
		t.Fatalf("dialogue did not stop: %s", dialogueSession.State)
	}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "messages", chatRequest{ID: "second", Text: "Continue discussion."}, false))
	if err = m.reconcileWorkspaceChats(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		_ = m.withResourceController(context.Background(), w, c.address(), func() error { return m.reconcileResourceMailboxLocked(context.Background(), w, c.address()) })
		_ = m.pollAgentHubSessions(context.Background())
	}
	next, found, err := currentResourceGeneration(w.Path, c.address())
	if err != nil || !found || next.GenerationID == dialogue.GenerationID {
		t.Fatalf("reopen did not create successor: %v %v %s", found, err, next.GenerationID)
	}
}

func TestChatTimerCoalescingNotesAndReadCursor(t *testing.T) {
	m, w := chatTestManager(t)
	resource := "project1.task1"
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
	c, _ := loadDialogue(w.Path, dialoguePrefix+chatID(resource, "disksing"))
	rec := generationRecord{ID: "timer-gen", GenerationID: "timer-gen", Generation: 1, WorkspaceID: w.ID, ResourceID: c.address(), OwnerResourceID: resource, ChatEpoch: c.Epoch, Status: "idle"}
	if err := saveGenerationRecord(w.Path, rec); err != nil {
		t.Fatal(err)
	}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "notes", chatRequest{Text: "Follow project1.task1; nothing published yet."}, true))
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "timer", chatRequest{Seconds: 60}, true))
	for iteration := range 2 {
		chatMu.Lock()
		current, err := loadChatLocked(w.Path, c.ID)
		if err != nil {
			chatMu.Unlock()
			t.Fatal(err)
		}
		current.NextCheckAt = fmt.Sprintf("2020-01-01T00:00:0%dZ", iteration)
		if err = saveChatLocked(w.Path, current); err != nil {
			chatMu.Unlock()
			t.Fatal(err)
		}
		chatMu.Unlock()
		if err = m.reconcileWorkspaceChats(context.Background(), w); err != nil {
			t.Fatal(err)
		}
	}
	mailbox, err := loadResourceMailboxForResource(w.Path, c.address())
	if err != nil {
		t.Fatal(err)
	}
	if len(mailbox.Messages) != 1 {
		t.Fatalf("timer accumulated %d wakeups", len(mailbox.Messages))
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

func TestChatTimerOneShotMigrationAndBusyDeferral(t *testing.T) {
	m, w := chatTestManager(t)
	resource := "project1.task1"
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "open", chatRequest{}, false))
	c, err := loadDialogue(w.Path, dialoguePrefix+chatID(resource, "disksing"))
	if err != nil {
		t.Fatal(err)
	}
	rec := generationRecord{ID: "one-shot", GenerationID: "one-shot", Generation: 1, WorkspaceID: w.ID, ResourceID: c.address(), ChatEpoch: c.Epoch, Status: "running"}
	if err := saveGenerationRecord(w.Path, rec); err != nil {
		t.Fatal(err)
	}
	// Simulate an on-disk v1 timer created before this upgrade, without using
	// the current writer (which always persists v2).
	c.Version = 1
	c.IntervalSeconds = 60
	c.NextCheckAt = "2020-01-01T00:00:00Z"
	data, _ := json.Marshal(c)
	if err := os.WriteFile(chatPath(w.Path, c.ID), data, 0600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := m.reconcileWorkspaceChats(context.Background(), w); err != nil {
			t.Fatal(err)
		}
	}
	current, _ := loadDialogue(w.Path, c.address())
	if current.Version != 2 || current.NextCheckAt != c.NextCheckAt || len(current.Outbox) != 0 {
		t.Fatalf("busy legacy timer changed or queued: %+v", current.public())
	}
	rec.Status = "idle"
	if err := saveGenerationRecord(w.Path, rec); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := m.reconcileWorkspaceChats(context.Background(), w); err != nil {
			t.Fatal(err)
		}
	}
	current, _ = loadDialogue(w.Path, c.address())
	if current.IntervalSeconds != 0 || current.NextCheckAt != "" || len(current.Outbox) != 1 {
		t.Fatalf("one-shot not consumed exactly once: %+v", current.public())
	}
	if !strings.Contains(current.Outbox[0].Text, "This timer is now off") {
		t.Fatal("timer policy missing from legacy wakeup")
	}
	// The appointment remains consumed across manager restarts / disk reads,
	// even after the old mailbox delivery no longer blocks another wakeup.
	if err := markResourceMailboxArchived(w.Path, c.address()); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := m.reconcileWorkspaceChats(context.Background(), w); err != nil {
			t.Fatal(err)
		}
	}
	current, _ = loadDialogue(w.Path, c.address())
	if len(current.Outbox) != 1 || current.NextCheckAt != "" {
		t.Fatal("timer automatically repeated")
	}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "timer", chatRequest{Seconds: 600}, true))
	current, _ = loadDialogue(w.Path, c.address())
	if current.IntervalSeconds != 600 || current.NextCheckAt == "" {
		t.Fatal("explicit rearm failed")
	}
	mustChatOK(t, chatCall(t, m, w, resource, "disksing", "timer", chatRequest{Seconds: 0}, true))
	current, _ = loadDialogue(w.Path, c.address())
	if current.IntervalSeconds != 0 || current.NextCheckAt != "" {
		t.Fatal("cancel failed")
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
