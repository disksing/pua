package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/workspacepath"
)

func TestTelegramTemporaryResourceFailureRetainsAndRevalidates(t *testing.T) {
	rt, api, st := telegramFixture(t)
	route := telegramOpen(t, rt, &st, "project1.task1")
	telegramPublish(t, rt, route.Resource, "old", "synthetic pending")
	api.broken = true
	telegramFlush(t, rt, &st)
	api.broken = false
	pending := st.Routes[0].Pending
	cursor := st.Routes[0].Cursor

	ws, err := app.OpenWorkspace(rt.workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	resource, err := ws.ResourceValue(route.Resource)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rt.workspace.Path, resource.Path, "task.json")
	backup := filepath.Join(t.TempDir(), "task.json")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	telegramFlush(t, rt, &st)
	if !st.Routes[0].Paused || st.Routes[0].PauseReason != telegramPauseResource || st.Routes[0].PausedAt == "" ||
		st.Routes[0].Pending != pending || st.Routes[0].Cursor != cursor {
		t.Fatal("temporary resource error did not preserve the delivery and cursor")
	}
	pausedAt := st.Routes[0].PausedAt
	telegramFlush(t, rt, &st)
	if st.Routes[0].PausedAt != pausedAt {
		t.Fatal("repeated failure changed the pause timestamp")
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	st = telegramLoad(t, rt)
	before := len(api.calls)
	telegramFlush(t, rt, &st)
	if st.Routes[0].Paused || st.Routes[0].Pending == nil || st.Routes[0].Pending.Status != "unknown" ||
		st.Routes[0].Cursor != cursor || len(api.calls) != before {
		t.Fatal("revalidation lost or retried the uncertain delivery")
	}
	telegramProcess(t, rt, &st, telegramInput(10, 777, route.ThreadID, "new input"))
	c, err := loadDialogue(rt.workspace.Path, dialoguePrefix+chatID(route.Resource, route.User))
	if err != nil || len(c.Messages) != 2 {
		t.Fatal("input did not resume after the resource recovered")
	}
}

func TestTelegramStateUpgradePreservesLegacyPause(t *testing.T) {
	rt, _, st := telegramFixture(t)
	route := telegramOpen(t, rt, &st, "project1.task1")
	st.Version = 1
	st.Routes[0].Paused = true
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rt.path(), data, 0600); err != nil {
		t.Fatal(err)
	}
	old, err := rt.load()
	if err != nil || old.Version != 1 || len(old.Routes) != 1 || old.Routes[0].PauseReason != "" {
		t.Fatal("version 1 state did not load as a legacy pause")
	}
	telegramFlush(t, rt, &old)
	if !old.Routes[0].Paused {
		t.Fatal("legacy pause was released during upgrade")
	}
	if err := rt.save(old); err != nil {
		t.Fatal(err)
	}
	upgraded := telegramLoad(t, rt)
	if upgraded.Version != telegramStateVersion || !upgraded.Routes[0].Paused || upgraded.Routes[0].Epoch != route.Epoch {
		t.Fatal("upgraded state did not retain the legacy pause")
	}
	upgraded.Version = telegramStateVersion + 1
	data, err = json.Marshal(upgraded)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rt.path(), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.load(); err == nil {
		t.Fatal("unsupported future state was accepted")
	}
}

func TestTelegramReopenHoldsOldDeliveriesAndSendsNewMessages(t *testing.T) {
	for _, scanWhileClosed := range []bool{false, true} {
		name := "reopen_before_scan"
		if scanWhileClosed {
			name = "scan_while_closed"
		}
		t.Run(name, func(t *testing.T) {
			rt, api, st := telegramFixture(t)
			route := telegramOpen(t, rt, &st, "project1.task1")
			telegramPublish(t, rt, route.Resource, "old-1", "synthetic secret old 1")
			api.broken = true
			telegramFlush(t, rt, &st)
			api.broken = false
			telegramPublish(t, rt, route.Resource, "old-2", "synthetic secret old 2")
			mustChatOK(t, chatCall(t, rt.server.agents, rt.workspace, route.Resource, route.User, "close", chatRequest{}, false))
			if scanWhileClosed {
				before := len(api.calls)
				telegramFlush(t, rt, &st)
				if !st.Routes[0].Paused || st.Routes[0].PauseReason != telegramPauseClosed ||
					st.Routes[0].Pending == nil || len(api.calls) != before {
					t.Fatal("closed chat did not retain and block the pending delivery")
				}
			}
			mustChatOK(t, chatCall(t, rt.server.agents, rt.workspace, route.Resource, route.User, "open", chatRequest{}, false))
			telegramPublish(t, rt, route.Resource, "new", "synthetic new")
			rt.nextSend = map[int64]time.Time{}
			before := len(api.calls)
			telegramFlush(t, rt, &st)
			r := st.Routes[0]
			if r.Paused || r.Epoch == route.Epoch || len(r.Held) != 1 || r.Held[0].Count != 2 ||
				r.Held[0].Pending == nil || r.Held[0].Pending.Status != "unknown" {
				t.Fatalf("old delivery was not retained across reopening: %#v", r.Held)
			}
			if len(api.calls) != before+1 || api.calls[before].Body["text"] != "synthetic new" {
				t.Fatal("did not send exactly the new-period message")
			}
			st = telegramLoad(t, rt)
			r = st.Routes[0]
			if len(r.Held) != 1 || r.Held[0].Pending.Text != "synthetic secret old 1" {
				t.Fatal("held delivery was not durable")
			}
			out := telegramSettingsCall(t, rt, http.MethodGet, "", nil)
			mustChatOK(t, out)
			var settings struct {
				DeliveryProblems []struct {
					PendingStatus string `json:"pendingStatus"`
				} `json:"deliveryProblems"`
			}
			if err := json.Unmarshal(out.Body.Bytes(), &settings); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.Body.String(), "synthetic secret") || strings.Contains(out.Body.String(), st.Token) ||
				len(settings.DeliveryProblems) != 1 || settings.DeliveryProblems[0].PendingStatus != "unknown" {
				t.Fatal("review metadata leaked content or omitted the unknown status")
			}
			body := map[string]any{"route": r.key(), "delivery": r.Held[0].ID, "action": "retry"}
			if telegramSettingsCall(t, rt, http.MethodPost, "/delivery", body).Code != http.StatusConflict {
				t.Fatal("old-period delivery could be retried")
			}
			body["action"] = "acknowledge"
			mustChatOK(t, telegramSettingsCall(t, rt, http.MethodPost, "/delivery", body))
			if len(telegramLoad(t, rt).Routes[0].Held) != 0 {
				t.Fatal("acknowledged review was not cleared")
			}
			if telegramSettingsCall(t, rt, http.MethodPost, "/delivery", body).Code != http.StatusConflict {
				t.Fatal("stale acknowledgement was accepted")
			}
		})
	}
}

func TestTelegramManualPausePreservesPendingAndRequiresResume(t *testing.T) {
	rt, api, st := telegramFixture(t)
	route := telegramOpen(t, rt, &st, "project1.task1")
	telegramPublish(t, rt, route.Resource, "queued", "synthetic queued")
	st.Routes[0].Pending = &telegramDelivery{Text: "synthetic queued", Chunks: []string{"synthetic queued"}, NextCursor: 1, Status: "queued"}
	telegramProcess(t, rt, &st, telegramInput(2, 777, route.ThreadID, "/stop"))
	before := len(api.calls)
	telegramFlush(t, rt, &st)
	if len(api.calls) != before || st.Routes[0].PauseReason != telegramPauseManual || st.Routes[0].Pending == nil {
		t.Fatal("manual pause did not retain and block the queued delivery")
	}
	telegramProcess(t, rt, &st, telegramInput(3, 777, route.ThreadID, "blocked input"))
	c, err := loadDialogue(rt.workspace.Path, dialoguePrefix+chatID(route.Resource, route.User))
	if err != nil || len(c.Messages) != 1 {
		t.Fatal("input was accepted while manually paused")
	}
	telegramProcess(t, rt, &st, telegramInput(4, 777, route.ThreadID, "/resume"))
	if st.Routes[0].Paused || st.Routes[0].Pending == nil {
		t.Fatal("manual resume discarded the queued delivery")
	}
	rt.nextSend = map[int64]time.Time{}
	before = len(api.calls)
	telegramFlush(t, rt, &st)
	if len(api.calls) != before+1 || api.calls[before].Body["text"] != "synthetic queued" {
		t.Fatal("queued delivery did not continue after explicit resume")
	}
}

func TestTelegramManualResumeDoesNotRetryUnknownDelivery(t *testing.T) {
	rt, api, st := telegramFixture(t)
	route := telegramOpen(t, rt, &st, "project1.task1")
	telegramPublish(t, rt, route.Resource, "old", "synthetic old")
	api.broken = true
	telegramFlush(t, rt, &st)
	api.broken = false
	telegramProcess(t, rt, &st, telegramInput(2, 777, route.ThreadID, "/stop"))
	telegramProcess(t, rt, &st, telegramInput(3, 777, route.ThreadID, "/resume"))
	before := len(api.calls)
	rt.nextSend = map[int64]time.Time{}
	telegramFlush(t, rt, &st)
	if st.Routes[0].Paused || st.Routes[0].Pending == nil || st.Routes[0].Pending.Status != "unknown" || len(api.calls) != before {
		t.Fatal("manual resume lost or retried the uncertain delivery")
	}
}

func TestTelegramInputCanReconcileReopenedChat(t *testing.T) {
	rt, api, st := telegramFixture(t)
	route := telegramOpen(t, rt, &st, "project1.task1")
	telegramPublish(t, rt, route.Resource, "old", "synthetic old")
	api.broken = true
	telegramFlush(t, rt, &st)
	api.broken = false
	mustChatOK(t, chatCall(t, rt.server.agents, rt.workspace, route.Resource, route.User, "close", chatRequest{}, false))
	mustChatOK(t, chatCall(t, rt.server.agents, rt.workspace, route.Resource, route.User, "open", chatRequest{}, false))
	telegramProcess(t, rt, &st, telegramInput(2, 777, route.ThreadID, "synthetic new input"))
	c, err := loadDialogue(rt.workspace.Path, dialoguePrefix+chatID(route.Resource, route.User))
	if err != nil || len(c.Messages) != 2 || c.Messages[1].Text != "synthetic new input" ||
		len(st.Routes[0].Held) != 1 || st.Routes[0].Held[0].Pending == nil || st.Routes[0].Held[0].Pending.Status != "unknown" {
		t.Fatal("input-first recovery lost the input or the old delivery")
	}
	telegramPublish(t, rt, route.Resource, "new", "synthetic new reply")
	rt.nextSend = map[int64]time.Time{}
	before := len(api.calls)
	telegramFlush(t, rt, &st)
	if len(api.calls) != before+1 || api.calls[before].Body["text"] != "synthetic new reply" {
		t.Fatal("input-first recovery did not send only the new reply")
	}
}

func TestTelegramLegacyPauseNeedsConfirmationAndHoldsHistory(t *testing.T) {
	rt, api, st := telegramFixture(t)
	route := telegramOpen(t, rt, &st, "project1.task1")
	telegramPublish(t, rt, route.Resource, "old", "synthetic old")
	st.Routes[0].Paused = true
	st.Routes[0].PauseReason = ""
	before := len(api.calls)
	telegramFlush(t, rt, &st)
	if !st.Routes[0].Paused || len(api.calls) != before {
		t.Fatal("legacy pause was automatically released")
	}
	telegramProcess(t, rt, &st, telegramInput(2, 777, route.ThreadID, "/stop"))
	if st.Routes[0].PauseReason != "" {
		t.Fatal("stop obscured the unknown legacy reason")
	}
	telegramProcess(t, rt, &st, telegramInput(3, 777, route.ThreadID, "/resume"))
	if !st.Routes[0].Paused || st.Routes[0].Cursor != 0 ||
		!strings.Contains(api.calls[len(api.calls)-1].Body["text"].(string), "/resume confirm") {
		t.Fatal("legacy recovery did not ask for explicit confirmation")
	}
	telegramProcess(t, rt, &st, telegramInput(4, 777, route.ThreadID, "/resume confirm"))
	if st.Routes[0].Paused || st.Routes[0].Cursor != 1 || len(st.Routes[0].Held) != 1 || st.Routes[0].Held[0].Count != 1 {
		t.Fatal("legacy recovery did not retain a review notice")
	}
	telegramPublish(t, rt, route.Resource, "new", "synthetic new")
	rt.nextSend = map[int64]time.Time{}
	before = len(api.calls)
	telegramFlush(t, rt, &st)
	if len(api.calls) != before+1 || api.calls[before].Body["text"] != "synthetic new" {
		t.Fatal("legacy recovery replayed older messages or missed a new message")
	}
}

func TestTelegramUnknownEpochRequiresExplicitRecovery(t *testing.T) {
	rt, api, st := telegramFixture(t)
	route := telegramOpen(t, rt, &st, "project1.task1")
	telegramPublish(t, rt, route.Resource, "old", "synthetic old")
	mustChatOK(t, chatCall(t, rt.server.agents, rt.workspace, route.Resource, route.User, "close", chatRequest{}, false))
	mustChatOK(t, chatCall(t, rt.server.agents, rt.workspace, route.Resource, route.User, "open", chatRequest{}, false))
	telegramPublish(t, rt, route.Resource, "new", "synthetic new before upgrade")
	// Model a Chat reopened by the older version, before epoch boundaries existed.
	chatMu.Lock()
	c, err := loadChatLocked(rt.workspace.Path, chatID(route.Resource, route.User))
	if err == nil {
		c.EpochStart = nil
		err = saveChatLocked(rt.workspace.Path, c)
	}
	chatMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	before := len(api.calls)
	telegramFlush(t, rt, &st)
	if !st.Routes[0].Paused || st.Routes[0].PauseReason != telegramPauseEpoch || len(api.calls) != before || st.Routes[0].Cursor != 0 {
		t.Fatal("unknown epoch boundary resumed or advanced without a decision")
	}
	telegramProcess(t, rt, &st, telegramInput(2, 777, route.ThreadID, "/resume"))
	if !st.Routes[0].Paused || !strings.Contains(api.calls[len(api.calls)-1].Body["text"].(string), "/resume confirm") {
		t.Fatal("unknown boundary did not request explicit recovery")
	}
	telegramProcess(t, rt, &st, telegramInput(3, 777, route.ThreadID, "/resume confirm"))
	if st.Routes[0].Paused || len(st.Routes[0].Held) != 1 || st.Routes[0].Held[0].Count != 2 || st.Routes[0].Cursor != 2 {
		t.Fatal("explicit recovery did not retain the ambiguous history")
	}
}

func TestTelegramUnrecognizedPauseReasonDoesNotAutomaticallyResume(t *testing.T) {
	rt, api, st := telegramFixture(t)
	route := telegramOpen(t, rt, &st, "project1.task1")
	telegramPublish(t, rt, route.Resource, "old", "synthetic old")
	st.Routes[0].pause("future_reason")
	before := len(api.calls)
	telegramFlush(t, rt, &st)
	telegramProcess(t, rt, &st, telegramInput(2, 777, route.ThreadID, "/stop"))
	if !st.Routes[0].Paused || st.Routes[0].PauseReason != "future_reason" || len(api.calls) != before+1 {
		t.Fatal("unknown pause was automatically released or overwritten")
	}
	telegramProcess(t, rt, &st, telegramInput(3, 777, route.ThreadID, "/resume"))
	if !st.Routes[0].Paused || !strings.Contains(api.calls[len(api.calls)-1].Body["text"].(string), "/resume confirm") {
		t.Fatal("unknown pause did not require explicit recovery")
	}
}

func TestTelegramRebindRetainsUnknownDelivery(t *testing.T) {
	rt, api, st := telegramFixture(t)
	route := telegramOpen(t, rt, &st, "project1.task1")
	telegramPublish(t, rt, route.Resource, "old", "synthetic old")
	api.broken = true
	telegramFlush(t, rt, &st)
	api.broken = false
	telegramProcess(t, rt, &st, telegramInput(2, 777, 456, "/bind project1.task1"))
	r := st.Routes[0]
	if r.ThreadID != 456 || r.Paused || r.Pending != nil || len(r.Held) != 1 ||
		r.Held[0].Pending == nil || r.Held[0].Pending.Status != "unknown" || r.Held[0].Reason != "topic_rebound" {
		t.Fatal("rebind discarded or transferred the unknown delivery")
	}
	telegramPublish(t, rt, route.Resource, "new", "synthetic new")
	rt.nextSend = map[int64]time.Time{}
	before := len(api.calls)
	telegramFlush(t, rt, &st)
	if len(api.calls) != before+1 || api.calls[before].Body["text"] != "synthetic new" ||
		int64(api.calls[before].Body["message_thread_id"].(float64)) != 456 {
		t.Fatal("new topic did not receive exactly the new delivery")
	}
}

func TestTelegramUnpairedAndArchivedRoutesCannotSend(t *testing.T) {
	t.Run("unpaired can resume after revalidation", func(t *testing.T) {
		rt, api, st := telegramFixture(t)
		route := telegramOpen(t, rt, &st, "project1.task1")
		telegramPublish(t, rt, route.Resource, "answer", "synthetic answer")
		delete(st.Accounts, route.ChatID)
		before := len(api.calls)
		telegramFlush(t, rt, &st)
		if len(api.calls) != before || st.Routes[0].PauseReason != telegramPauseUnpaired {
			t.Fatal("unpaired route was not blocked")
		}
		st.Accounts[route.ChatID] = route.User
		telegramFlush(t, rt, &st)
		if st.Routes[0].Paused || len(api.calls) != before+1 {
			t.Fatal("paired route did not recover")
		}
	})
	t.Run("archived stays blocked", func(t *testing.T) {
		rt, api, st := telegramFixture(t)
		route := telegramOpen(t, rt, &st, "project1.task1")
		telegramPublish(t, rt, route.Resource, "answer", "synthetic answer")
		ws, err := app.OpenWorkspace(rt.workspace.Path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ws.ArchiveResource(route.Resource); err != nil {
			t.Fatal(err)
		}
		before := len(api.calls)
		telegramFlush(t, rt, &st)
		if len(api.calls) != before || st.Routes[0].PauseReason != telegramPauseArchived {
			t.Fatal("archived resource was not blocked")
		}
	})
}

func TestTelegramInputRecoversAfterUserVerificationFailure(t *testing.T) {
	rt, api, st := telegramFixture(t)
	route := telegramOpen(t, rt, &st, "project1.task1")
	profile := filepath.Join(workspacepath.ControlDir(rt.workspace.Path), "users", route.User, "profile.json")
	backup := filepath.Join(t.TempDir(), "profile.json")
	if err := os.Rename(profile, backup); err != nil {
		t.Fatal(err)
	}
	telegramProcess(t, rt, &st, telegramInput(2, 777, route.ThreadID, "synthetic rejected"))
	if st.Routes[0].PauseReason != telegramPauseUser ||
		!strings.Contains(api.calls[len(api.calls)-1].Body["text"].(string), "not sent to PUA") {
		t.Fatal("unverifiable user did not receive an actionable rejection")
	}
	if err := os.Rename(backup, profile); err != nil {
		t.Fatal(err)
	}
	telegramProcess(t, rt, &st, telegramInput(3, 777, route.ThreadID, "synthetic accepted"))
	c, err := loadDialogue(rt.workspace.Path, dialoguePrefix+chatID(route.Resource, route.User))
	if err != nil || st.Routes[0].Paused || len(c.Messages) != 1 || c.Messages[0].Text != "synthetic accepted" {
		t.Fatal("verified user did not recover safely")
	}
}

func TestTelegramCommandsCannotAccessAnotherUsersRoute(t *testing.T) {
	rt, api, st := telegramFixture(t)
	route := telegramOpen(t, rt, &st, "project1.task1")
	st.Accounts[route.ChatID] = "other"
	telegramProcess(t, rt, &st, telegramInput(2, 777, route.ThreadID, "/status"))
	if strings.Contains(api.calls[len(api.calls)-1].Body["text"].(string), route.Resource) {
		t.Fatal("status exposed another user's topic")
	}
	telegramProcess(t, rt, &st, telegramInput(3, 777, route.ThreadID, "/bind "+route.Resource))
	if len(st.Routes) != 1 || st.Routes[0].User != route.User {
		t.Fatal("binding reassigned another user's topic")
	}
}

func TestTelegramCloseWaitsForInFlightDelivery(t *testing.T) {
	rt, _, st := telegramFixture(t)
	route := telegramOpen(t, rt, &st, "project1.task1")
	mustChatOK(t, chatCall(t, rt.server.agents, rt.workspace, "project1", route.User, "open", chatRequest{}, false))
	telegramPublish(t, rt, route.Resource, "answer", "synthetic answer")
	started := make(chan struct{})
	release := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		writeJSON(w, map[string]any{"ok": true, "result": map[string]any{"message_id": 1}})
	}))
	defer api.Close()
	rt.apiURL = api.URL
	flushDone := make(chan error, 1)
	go func() { flushDone <- rt.flush(context.Background(), &st) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("delivery did not start")
	}
	otherClosed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		otherClosed <- chatCall(t, rt.server.agents, rt.workspace, "project1", route.User, "close", chatRequest{}, false)
	}()
	select {
	case out := <-otherClosed:
		mustChatOK(t, out)
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("an unrelated chat was blocked by the delivery")
	}
	closed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		closed <- chatCall(t, rt.server.agents, rt.workspace, route.Resource, route.User, "close", chatRequest{}, false)
	}()
	select {
	case <-closed:
		close(release)
		t.Fatal("close completed while the old delivery was still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-flushDone; err != nil {
		t.Fatal(err)
	}
	mustChatOK(t, <-closed)
}
