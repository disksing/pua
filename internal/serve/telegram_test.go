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
	"time"
	"unicode/utf16"

	"github.com/disksing/pua/internal/app"
)

type telegramTestAPI struct {
	mu        sync.Mutex
	calls     []telegramTestCall
	topics    bool
	webhook   string
	nextTopic int64
	sendCode  int
	broken    bool
	updates   []telegramUpdate
}
type telegramTestCall struct {
	Method string
	Body   map[string]any
}

func (f *telegramTestAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	method := filepath.Base(r.URL.Path)
	body := map[string]any{}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.calls = append(f.calls, telegramTestCall{method, body})
	var result any = true
	switch method {
	case "getMe":
		result = map[string]any{"id": 99, "is_bot": true, "username": "test_bot", "has_topics_enabled": f.topics}
	case "getWebhookInfo":
		result = map[string]any{"url": f.webhook}
	case "createForumTopic":
		f.nextTopic++
		result = map[string]any{"message_thread_id": f.nextTopic}
	case "getUpdates":
		result = f.updates
		if result == nil {
			result = []telegramUpdate{}
		}
	case "sendMessage":
		if f.broken {
			_, _ = w.Write([]byte("broken"))
			return
		}
		if f.sendCode != 0 {
			writeJSON(w, map[string]any{"ok": false, "error_code": f.sendCode, "description": "do not expose 99:secret", "parameters": map[string]int{"retry_after": 3}})
			return
		}
		result = map[string]any{"message_id": 123}
	}
	writeJSON(w, map[string]any{"ok": true, "result": result})
}
func telegramFixture(t *testing.T) (*telegramRuntime, *telegramTestAPI, telegramState) {
	t.Helper()
	m, w := chatTestManager(t)
	f := &telegramTestAPI{topics: true, nextTopic: 40}
	api := httptest.NewServer(f)
	t.Cleanup(api.Close)
	rt := m.server.telegramFor(w)
	rt.apiURL = api.URL
	st, err := rt.load()
	if err != nil {
		t.Fatal(err)
	}
	st.Enabled = true
	st.Token = "99:abcdefghijklmnopqrstuvwxyz"
	st.BotID = 99
	st.BotName = "test_bot"
	if err = rt.save(st); err != nil {
		t.Fatal(err)
	}
	return rt, f, st
}
func telegramSettingsCall(t *testing.T, rt *telegramRuntime, method, op string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(method, "/api/workspaces/"+rt.workspace.ID+"/telegram"+op, bytes.NewReader(data))
	out := httptest.NewRecorder()
	rt.server.handleWorkspace(out, req)
	return out
}
func telegramInput(id, account, thread int64, text string) telegramUpdate {
	return telegramUpdate{ID: id, Message: &telegramMessage{ID: id, ThreadID: thread, From: telegramUser{ID: account}, Chat: telegramChat{ID: account, Type: "private"}, Text: text}}
}
func telegramProcess(t *testing.T, rt *telegramRuntime, st *telegramState, u telegramUpdate) {
	t.Helper()
	if err := rt.process(context.Background(), st, u); err != nil {
		t.Fatal(err)
	}
	if err := rt.save(*st); err != nil {
		t.Fatal(err)
	}
}
func telegramLoad(t *testing.T, rt *telegramRuntime) telegramState {
	t.Helper()
	st, e := rt.load()
	if e != nil {
		t.Fatal(e)
	}
	return st
}
func telegramOpen(t *testing.T, rt *telegramRuntime, st *telegramState, resource string) telegramRoute {
	t.Helper()
	st.Accounts[777] = "disksing"
	telegramProcess(t, rt, st, telegramInput(1, 777, 0, "/open "+resource))
	for _, route := range st.Routes {
		if route.Resource == resource {
			return route
		}
	}
	t.Fatal("topic not opened")
	return telegramRoute{}
}
func telegramPublish(t *testing.T, rt *telegramRuntime, resource, id, text string) {
	t.Helper()
	mustChatOK(t, chatCall(t, rt.server.agents, rt.workspace, resource, "disksing", "publish", chatRequest{ID: id, Text: text}, true))
}
func telegramFlush(t *testing.T, rt *telegramRuntime, st *telegramState) {
	t.Helper()
	if err := rt.flush(context.Background(), st); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramSettingsSecretsValidationAndPairing(t *testing.T) {
	rt, f, _ := telegramFixture(t)
	out := telegramSettingsCall(t, rt, http.MethodPut, "", map[string]any{"enabled": true})
	mustChatOK(t, out)
	if strings.Contains(out.Body.String(), "abcdefghijklmnopqrstuvwxyz") {
		t.Fatal("token leaked")
	}
	info, _ := os.Stat(rt.path())
	if info.Mode().Perm() != 0600 {
		t.Fatalf("credentials mode %o", info.Mode().Perm())
	}
	info, _ = os.Stat(filepath.Dir(rt.path()))
	if info.Mode().Perm() != 0700 {
		t.Fatal("credentials directory not private")
	}
	f.topics = false
	out = telegramSettingsCall(t, rt, http.MethodPut, "", map[string]any{"enabled": true})
	if out.Code != 400 {
		t.Fatal(out.Code)
	}
	f.topics = true
	f.webhook = "https://example.test/hook"
	out = telegramSettingsCall(t, rt, http.MethodPut, "", map[string]any{"enabled": true})
	if out.Code != 409 {
		t.Fatal(out.Code)
	}
	f.webhook = ""
	out = telegramSettingsCall(t, rt, http.MethodPost, "/pair", map[string]any{"user": "absent"})
	if out.Code != 400 {
		t.Fatal(out.Code)
	}
	out = telegramSettingsCall(t, rt, http.MethodPost, "/pair", map[string]any{"user": "disksing"})
	mustChatOK(t, out)
	var pair struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(out.Body.Bytes(), &pair)
	code := strings.Split(pair.URL, "?start=")[1]
	st := telegramLoad(t, rt)
	if _, ok := st.Pairings[code]; ok {
		t.Fatal("pair secret stored unhashed")
	}
	group := telegramInput(1, 777, 0, "/start "+code)
	group.Message.Chat.Type = "supergroup"
	telegramProcess(t, rt, &st, group)
	if len(st.Accounts) != 0 {
		t.Fatal("group paired")
	}
	telegramProcess(t, rt, &st, telegramInput(2, 777, 0, "/start "+code))
	if st.Accounts[777] != "disksing" || len(st.Pairings) != 0 {
		t.Fatal("pairing failed")
	}
	telegramProcess(t, rt, &st, telegramInput(3, 888, 0, "/start "+code))
	if st.Accounts[888] != "" {
		t.Fatal("pair reused")
	}
	out = telegramSettingsCall(t, rt, http.MethodPost, "/unbind", map[string]any{"user": "disksing"})
	mustChatOK(t, out)
	if len(telegramLoad(t, rt).Accounts) != 0 {
		t.Fatal("unbind failed")
	}
}
func TestTelegramTopicsRouteInputAndPublishWithoutDuplicates(t *testing.T) {
	rt, f, st := telegramFixture(t)
	a := telegramOpen(t, rt, &st, "project1.task1")
	b := telegramOpen(t, rt, &st, "workspace")
	if a.ThreadID == b.ThreadID || a.ThreadID == 0 {
		t.Fatal("topics not distinct")
	}
	telegramProcess(t, rt, &st, telegramInput(10, 777, a.ThreadID, "task input"))
	telegramProcess(t, rt, &st, telegramInput(11, 777, b.ThreadID, "workspace input"))
	telegramProcess(t, rt, &st, telegramInput(10, 777, a.ThreadID, "task input"))
	telegramProcess(t, rt, &st, telegramInput(12, 888, a.ThreadID, "unauthorized"))
	c, e := loadDialogue(rt.workspace.Path, dialoguePrefix+chatID(a.Resource, "disksing"))
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Messages) != 1 || len(c.Outbox) != 1 || c.Outbox[0].Target != a.Resource {
		t.Fatalf("incorrect input routing: %#v", c)
	}
	telegramPublish(t, rt, b.Resource, "b-reply", "workspace answer")
	telegramPublish(t, rt, a.Resource, "a-reply", "task answer")
	before := len(f.calls)
	telegramFlush(t, rt, &st)
	rt.nextSend = map[int64]time.Time{}
	telegramFlush(t, rt, &st)
	counts := map[string]int{}
	for _, call := range f.calls[before:] {
		if call.Method != "sendMessage" {
			continue
		}
		text := call.Body["text"].(string)
		counts[text]++
		thread := int64(call.Body["message_thread_id"].(float64))
		if text == "task answer" && thread != a.ThreadID || text == "workspace answer" && thread != b.ThreadID {
			t.Fatal("cross-topic reply")
		}
	}
	if counts["task answer"] != 1 || counts["workspace answer"] != 1 || len(counts) != 2 {
		t.Fatalf("unexpected messages: %v", counts)
	}
	// Restart from durable cursor; no replay of previously delivered replies.
	st = telegramLoad(t, rt)
	rt.lastFiles = map[string]time.Time{}
	rt.nextSend = map[int64]time.Time{}
	before = len(f.calls)
	telegramFlush(t, rt, &st)
	if len(f.calls) != before {
		t.Fatal("duplicate after restart")
	}
}
func TestTelegramChatClosureEpochAndResume(t *testing.T) {
	rt, f, st := telegramFixture(t)
	route := telegramOpen(t, rt, &st, "project1.task1")
	telegramPublish(t, rt, route.Resource, "old", "reply before close")
	mustChatOK(t, chatCall(t, rt.server.agents, rt.workspace, route.Resource, "disksing", "close", chatRequest{}, false))
	before := len(f.calls)
	telegramFlush(t, rt, &st)
	if len(f.calls) != before || !st.Routes[0].Paused {
		t.Fatal("closed chat delivered")
	}
	telegramProcess(t, rt, &st, telegramInput(2, 777, route.ThreadID, "/resume"))
	if st.Routes[0].Epoch == route.Epoch || st.Routes[0].Paused {
		t.Fatal("resume did not renew epoch")
	}
	before = len(f.calls)
	rt.nextSend = map[int64]time.Time{}
	telegramFlush(t, rt, &st)
	if len(f.calls) != before {
		t.Fatal("old epoch reply replayed")
	}
	telegramPublish(t, rt, route.Resource, "new", "new reply")
	telegramFlush(t, rt, &st)
	if f.calls[len(f.calls)-1].Body["text"] != "new reply" {
		t.Fatal("new reply missing")
	}
}
func TestTelegramUnknownDeliveryIsHeldAndRetryIsFenced(t *testing.T) {
	rt, f, st := telegramFixture(t)
	route := telegramOpen(t, rt, &st, "project1.task1")
	telegramPublish(t, rt, route.Resource, "answer", "hello")
	f.broken = true
	telegramFlush(t, rt, &st)
	if st.Routes[0].Pending.Status != "unknown" {
		t.Fatal("ambiguous send retried")
	}
	st = telegramLoad(t, rt)
	before := len(f.calls)
	telegramFlush(t, rt, &st)
	if len(f.calls) != before {
		t.Fatal("unknown delivery was repeated")
	}
	route = st.Routes[0]
	out := telegramSettingsCall(t, rt, http.MethodPost, "/delivery", map[string]any{"route": route.key(), "delivery": "stale", "action": "skip"})
	if out.Code != 409 {
		t.Fatal("stale delivery accepted")
	}
	out = telegramSettingsCall(t, rt, http.MethodPost, "/delivery", map[string]any{"route": route.key(), "delivery": route.deliveryKey(), "action": "retry"})
	mustChatOK(t, out)
	f.broken = false
	st = telegramLoad(t, rt)
	rt.nextSend = map[int64]time.Time{}
	telegramFlush(t, rt, &st)
	if st.Routes[0].Pending != nil || st.Routes[0].Cursor != 1 {
		t.Fatal("manual retry failed")
	}
	out = telegramSettingsCall(t, rt, http.MethodPost, "/delivery", map[string]any{"route": route.key(), "delivery": route.deliveryKey(), "action": "retry"})
	if out.Code != 409 {
		t.Fatal("completed delivery replayed")
	}
}
func TestTelegramRateLimitAndSplitResume(t *testing.T) {
	rt, f, st := telegramFixture(t)
	route := telegramOpen(t, rt, &st, "project1.task1")
	answer := strings.Repeat("🧪", 2000) + strings.Repeat("x", 3700)
	telegramPublish(t, rt, route.Resource, "long", answer)
	f.sendCode = 429
	telegramFlush(t, rt, &st)
	p := st.Routes[0].Pending
	if p.Status != "queued" || !p.RetryAt.After(time.Now()) || p.Part != 0 {
		t.Fatal("429 backoff missing")
	}
	before := len(f.calls)
	telegramFlush(t, rt, &st)
	if len(f.calls) != before {
		t.Fatal("retried before retry_after")
	}
	f.sendCode = 0
	p.RetryAt = time.Time{}
	rt.nextSend = map[int64]time.Time{}
	telegramFlush(t, rt, &st)
	if st.Routes[0].Pending.Part != 1 {
		t.Fatal("first part not committed")
	}
	st = telegramLoad(t, rt)
	sent := []string{}
	for _, call := range f.calls[before:] {
		if call.Method == "sendMessage" {
			sent = append(sent, call.Body["text"].(string))
		}
	}
	before = len(f.calls)
	for i := 0; i < 5 && st.Routes[0].Pending != nil; i++ {
		rt.nextSend = map[int64]time.Time{}
		telegramFlush(t, rt, &st)
	}
	for _, call := range f.calls[before:] {
		if call.Method == "sendMessage" {
			sent = append(sent, call.Body["text"].(string))
		}
	}
	if strings.Join(sent, "") != answer {
		t.Fatal("split reply lost or repeated text")
	}
	for _, part := range sent {
		if len(utf16.Encode([]rune(part))) > 4096 {
			t.Fatal("part exceeds limit")
		}
	}
}
func TestTelegramTopicCreationCrashReservationAndBinding(t *testing.T) {
	rt, f, st := telegramFixture(t)
	st.Accounts[777] = "disksing"
	st.Routes = []telegramRoute{{User: "disksing", ChatID: 777, Resource: "project1.task1", Paused: true}}
	telegramProcess(t, rt, &st, telegramInput(1, 777, 0, "/open project1.task1"))
	for _, call := range f.calls {
		if call.Method == "createForumTopic" {
			t.Fatal("unknown creation replayed")
		}
	}
	telegramProcess(t, rt, &st, telegramInput(2, 777, 123, "/bind project1.task1"))
	if st.Routes[0].ThreadID != 123 || st.Routes[0].Paused {
		t.Fatal("binding repair failed")
	}
	telegramProcess(t, rt, &st, telegramInput(3, 777, 123, "/bind workspace"))
	if len(st.Routes) != 1 || st.Routes[0].Resource != "project1.task1" {
		t.Fatal("topic silently rebound")
	}
}
func TestTelegramPairingExpiryRevocationAndSensitiveError(t *testing.T) {
	rt, f, st := telegramFixture(t)
	st.Pairings[telegramPairKey("expired")] = telegramPairing{User: "disksing", Expires: time.Now().Add(-time.Second)}
	telegramProcess(t, rt, &st, telegramInput(1, 777, 0, "/start expired"))
	if len(st.Accounts) != 0 {
		t.Fatal("expired pairing accepted")
	}
	route := telegramOpen(t, rt, &st, "project1.task1")
	telegramPublish(t, rt, route.Resource, "answer", "private reply")
	st.Inbox = []telegramUpdate{telegramInput(9, 777, route.ThreadID, "queued input")}
	if e := rt.save(st); e != nil {
		t.Fatal(e)
	}
	out := telegramSettingsCall(t, rt, http.MethodPost, "/unbind", map[string]any{"user": "disksing"})
	mustChatOK(t, out)
	st = telegramLoad(t, rt)
	if len(st.Inbox) != 0 || len(st.Routes) != 0 {
		t.Fatal("revoked work retained")
	}
	route = telegramOpen(t, rt, &st, "project1.task1")
	telegramPublish(t, rt, route.Resource, "answer2", "private reply2")
	f.sendCode = 403
	telegramFlush(t, rt, &st)
	out = telegramSettingsCall(t, rt, http.MethodGet, "", nil)
	if strings.Contains(out.Body.String(), "99:secret") || strings.Contains(out.Body.String(), "private reply2") {
		t.Fatal("sensitive error leaked")
	}
}
func TestTelegramSettingsRejectBotReuseAndChatCapability(t *testing.T) {
	rt, _, _ := telegramFixture(t)
	cfg, e := rt.server.loadConfig()
	if e != nil {
		t.Fatal(e)
	}
	other := rt.workspace
	other.ID = "another"
	other.InstanceID = "another-instance"
	cfg.Workspaces = append(cfg.Workspaces, other)
	if e = rt.server.saveConfig(cfg); e != nil {
		t.Fatal(e)
	}
	otherRT := rt.server.telegramFor(other)
	otherState, e := otherRT.load()
	if e != nil {
		t.Fatal(e)
	}
	otherState.BotID = 99
	if e = otherRT.save(otherState); e != nil {
		t.Fatal(e)
	}
	out := telegramSettingsCall(t, rt, http.MethodPut, "", map[string]any{"enabled": true})
	if out.Code != 409 {
		t.Fatal("bot reuse accepted")
	}
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/workspaces/%s/telegram", rt.workspace.ID), nil)
	req.Header.Set("X-PUA-Chat-Token", "capability")
	out = httptest.NewRecorder()
	rt.server.handleWorkspace(out, req)
	if out.Code != 403 {
		t.Fatal("chat capability accessed settings")
	}
}

func TestTelegramSettingsRejectCrossOriginAndPreserveOtherInbox(t *testing.T) {
	rt, _, st := telegramFixture(t)
	req := httptest.NewRequest("POST", "/api/workspaces/"+rt.workspace.ID+"/telegram/disconnect", strings.NewReader("{}"))
	req.Header.Set("Origin", "https://untrusted.example")
	out := httptest.NewRecorder()
	rt.server.handleWorkspace(out, req)
	if out.Code != 403 {
		t.Fatal("cross-origin mutation accepted")
	}
	st.Accounts[777] = "disksing"
	st.Accounts[888] = "other"
	st.Inbox = []telegramUpdate{telegramInput(10, 777, 1, "revoked"), telegramInput(11, 888, 2, "keep")}
	if e := rt.save(st); e != nil {
		t.Fatal(e)
	}
	out = telegramSettingsCall(t, rt, http.MethodPost, "/unbind", map[string]any{"user": "disksing"})
	mustChatOK(t, out)
	st = telegramLoad(t, rt)
	if len(st.Inbox) != 1 || st.Inbox[0].ID != 11 || st.Accounts[888] != "other" {
		t.Fatal("unbind discarded another user's input")
	}
}
func TestTelegramRestartedSendingIsUnknownAndDeletedTopicCanBeRebound(t *testing.T) {
	rt, f, st := telegramFixture(t)
	route := telegramOpen(t, rt, &st, "project1.task1")
	telegramPublish(t, rt, route.Resource, "answer", "answer")
	st.Routes[0].Pending = &telegramDelivery{Text: "answer", Chunks: []string{"answer"}, NextCursor: 1, Status: "sending"}
	if e := rt.save(st); e != nil {
		t.Fatal(e)
	}
	st = telegramLoad(t, rt)
	before := len(f.calls)
	telegramFlush(t, rt, &st)
	if len(f.calls) != before || st.Routes[0].Pending.Status != "unknown" {
		t.Fatal("in-flight send repeated after restart")
	}
	telegramProcess(t, rt, &st, telegramInput(2, 777, 456, "/bind project1.task1"))
	if len(st.Routes) != 1 || st.Routes[0].ThreadID != 456 || st.Routes[0].Pending != nil {
		t.Fatal("deleted topic repair failed")
	}
	telegramProcess(t, rt, &st, telegramInput(3, 777, route.ThreadID, "old topic input"))
	c, e := loadDialogue(rt.workspace.Path, dialoguePrefix+chatID(route.Resource, "disksing"))
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Messages) != 1 {
		t.Fatal("old topic still routes input")
	}
}
func TestTelegramWorkerCommitsInboxBeforeAcknowledgingUpdates(t *testing.T) {
	rt, f, st := telegramFixture(t)
	route := telegramOpen(t, rt, &st, "project1.task1")
	f.mu.Lock()
	f.updates = []telegramUpdate{telegramInput(10, 777, route.ThreadID, "durable input")}
	f.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rt.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rt.mu.Lock()
		saved, e := rt.load()
		rt.mu.Unlock()
		if e == nil && saved.Offset == 11 && len(saved.Inbox) == 0 {
			c, e := loadDialogue(rt.workspace.Path, dialoguePrefix+chatID(route.Resource, "disksing"))
			if e == nil && len(c.Messages) == 1 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	done = make(chan struct{})
	close(done)
	c, e := loadDialogue(rt.workspace.Path, dialoguePrefix+chatID(route.Resource, "disksing"))
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Messages) != 1 || len(c.Outbox) != 1 {
		t.Fatal("worker lost or repeated input")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call.Method == "getUpdates" && call.Body["offset"].(float64) > 10 {
			saved, e := rt.load()
			if e != nil || saved.Offset < 11 {
				t.Fatal("offset acknowledged without durable receipt")
			}
		}
	}
}

func TestTelegramMenuRejectsOverflowPages(t *testing.T) {
	rt, _, st := telegramFixture(t)
	ws, e := app.OpenWorkspace(rt.workspace.Path)
	if e != nil {
		t.Fatal(e)
	}
	for _, page := range []string{"9223372036854775807", "999999999999999999999999", "-1"} {
		_, buttons, e := rt.resourceMenu(ws, st, "disksing", []string{"/projects", page})
		if e != nil {
			t.Fatal(e)
		}
		if len(buttons) == 0 || buttons[0][0].Data != "/open project1" {
			t.Fatalf("invalid page lost resources: %s", page)
		}
	}
}

func TestTelegramUnknownResourceDoesNotBlockLaterInput(t *testing.T) {
	rt, _, st := telegramFixture(t)
	st.Accounts[777] = "disksing"
	for i, resource := range []string{"project999.task999", "../../other", "dialogue-invalid"} {
		telegramProcess(t, rt, &st, telegramInput(int64(i+1), 777, 0, "/open "+resource))
	}
	if len(st.Routes) != 0 {
		t.Fatal("invalid resource created a topic")
	}
	route := telegramOpen(t, rt, &st, "project1.task1")
	telegramProcess(t, rt, &st, telegramInput(10, 777, route.ThreadID, "valid follow-up"))
	c, e := loadDialogue(rt.workspace.Path, dialoguePrefix+chatID(route.Resource, "disksing"))
	if e != nil || len(c.Messages) != 1 {
		t.Fatal("invalid resource blocked subsequent input")
	}
}
