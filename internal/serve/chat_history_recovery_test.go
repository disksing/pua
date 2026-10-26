package serve

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func TestChatReplyRestoredFromRetiredGenerationHistory(t *testing.T) {
	fake := newHistoryFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	m, w, _ := newRuntimeTestManager(t, hub.URL)
	ws, err := app.OpenWorkspace(w.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ws.RegisterUser("reader"); err != nil {
		t.Fatal(err)
	}
	resource := "project1.task1"
	mustChatOK(t, chatCall(t, m, w, resource, "reader", "open", chatRequest{}, false))
	mustChatOK(t, chatCall(t, m, w, resource, "reader", "messages", chatRequest{ID: "source-request", Text: "Notify me when done"}, false))
	c, err := loadDialogue(w.Path, dialoguePrefix+chatID(resource, "reader"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format(time.RFC3339Nano)
	old := generationRecord{ID: "gen-old", WorkspaceID: w.ID, ResourceID: resource, Generation: 1, GenerationID: "gen-old", AgentHubSessionID: "ses-old", Status: "archived", CreatedAt: now, UpdatedAt: now}
	saveRetiredGenerationForTest(t, w.Path, old, "recovery_fixture")
	if err := saveGenerationRecord(w.Path, generationRecord{ID: "gen-new", WorkspaceID: w.ID, ResourceID: resource, Generation: 2, GenerationID: "gen-new", Status: "starting", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	turn := historyTestTurn("turn-source", 1, true)
	turn.Items[0].Text = c.Outbox[0].Text
	fake.mu.Lock()
	fake.turns["ses-old"] = []agentHubTurn{turn}
	fake.mu.Unlock()
	fake.base.mu.Lock()
	fake.base.sessions["ses-old"] = agentHubSession{ID: "ses-old", State: "archived"}
	fake.base.mu.Unlock()
	instance, err := resourceHistoryInstanceID(w)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := encodeResourceHistoryReference(resourceHistoryReference{Kind: "turn", InstanceID: instance, ResourceID: resource, GenerationID: "gen-old", TurnID: "turn-source"})
	if err != nil {
		t.Fatal(err)
	}
	out := httptest.NewRecorder()
	m.server.handleWorkspace(out, httptest.NewRequest(http.MethodGet, "/api/workspaces/"+w.ID+"/resources/"+resource+"/history/turns/"+ref, nil))
	if out.Code != http.StatusOK {
		t.Fatalf("history %d: %s", out.Code, out.Body.String())
	}
	var detail resourceHistoryTurnDetail
	if err := json.Unmarshal(out.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Items) != 1 {
		t.Fatalf("missing source: %#v", detail.Items)
	}
	text := detail.Items[0].Text
	prefix := "pua chat publish --reply-to="
	_, tail, ok := strings.Cut(text, prefix)
	if !ok {
		t.Fatal("history lost reply instructions")
	}
	capability := strings.SplitN(strings.Fields(tail)[0], ".", 2)
	publish := func() *httptest.ResponseRecorder {
		data, _ := json.Marshal(chatRequest{ID: "result-stable", Text: "Result complete"})
		req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+w.ID+"/resources/"+resource+"/chat/publish", bytes.NewReader(data))
		req.Header.Set("X-PUA-Chat", capability[0])
		req.Header.Set("X-PUA-Chat-Token", capability[1])
		response := httptest.NewRecorder()
		m.server.handleWorkspace(response, req)
		return response
	}
	mustChatOK(t, publish())
	mustChatOK(t, publish())
	current, _ := loadDialogue(w.Path, c.address())
	if len(current.Messages) != 2 {
		t.Fatal("recovery duplicated publication")
	}
	mustChatOK(t, chatCall(t, m, w, resource, "reader", "close", chatRequest{}, false))
	mustChatOK(t, chatCall(t, m, w, resource, "reader", "open", chatRequest{}, false))
	if result := publish(); result.Code != http.StatusConflict {
		t.Fatalf("stale recovered capability accepted: %d", result.Code)
	}
}
