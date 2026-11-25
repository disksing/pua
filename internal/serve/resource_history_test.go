package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

type historyFakeAgentHub struct {
	base     *runtimeFakeAgentHub
	mu       sync.Mutex
	turns    map[string][]agentHubTurn
	failures map[string]bool
}

func newHistoryFakeAgentHub() *historyFakeAgentHub {
	return &historyFakeAgentHub{base: newRuntimeFakeAgentHub(), turns: make(map[string][]agentHubTurn), failures: make(map[string]bool)}
}

func (f *historyFakeAgentHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 4 && parts[0] == "v1" && parts[1] == "sessions" && parts[3] == "turns" {
		sessionID, _ := url.PathUnescape(parts[2])
		f.mu.Lock()
		turns, found := f.turns[sessionID]
		failed := f.failures[sessionID]
		f.mu.Unlock()
		if failed {
			w.WriteHeader(http.StatusInternalServerError)
			writeRuntimeFakeJSON(w, map[string]any{"error": map[string]any{"code": "session_store_failed", "message": "synthetic corrupt history"}})
			return
		}
		if !found {
			w.WriteHeader(http.StatusNotFound)
			writeRuntimeFakeJSON(w, map[string]any{"error": map[string]any{"code": "session_not_found", "message": "session not found"}})
			return
		}
		if len(parts) == 5 {
			turnID, _ := url.PathUnescape(parts[4])
			for _, turn := range turns {
				if turn.ID == turnID || turn.TurnID == turnID {
					writeRuntimeFakeJSON(w, map[string]any{"turn": turn, "latestEventId": 9})
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			writeRuntimeFakeJSON(w, map[string]any{"error": map[string]any{"code": "turn_not_found", "message": "turn not found"}})
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > len(turns) {
			limit = len(turns)
		}
		before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
		end := len(turns)
		if before > 0 {
			end = 0
			for end < len(turns) && turns[end].FirstEventID < before {
				end++
			}
		}
		start := end - limit
		if start < 0 {
			start = 0
		}
		selected := append([]agentHubTurn(nil), turns[start:end]...)
		nextBefore := int64(0)
		if len(selected) > 0 {
			nextBefore = selected[0].FirstEventID
		}
		writeRuntimeFakeJSON(w, map[string]any{
			"turns": selected,
			"page":  map[string]any{"nextBefore": nextBefore, "hasMoreBefore": start > 0},
			"latestCursor": func() int64 {
				if len(turns) == 0 {
					return 0
				}
				return turns[len(turns)-1].FirstEventID
			}(),
			"latestEventId": 9,
		})
		return
	}
	f.base.ServeHTTP(w, r)
}

func historyTestTurn(id string, first int64, closed bool) agentHubTurn {
	status := "active"
	if closed {
		status = "completed"
	}
	return agentHubTurn{
		ID: id, TurnID: id, Status: status, Closed: closed,
		StartedAt: "2026-08-13T00:00:00Z", FirstEventID: first, StartEventID: first,
		LastEventID: first + 1, EndEventID: first + 1, TriggerPreview: "prompt " + id,
		FinalReplyPreview: "reply " + id, EventCount: 2, ToolEventCount: 1,
		Items: []agentHubTurnItem{{
			Type: "message", Role: "user", Text: "prompt " + id,
			StartEventID: first, EndEventID: first, StartedAt: "2026-08-13T00:00:00Z", EndedAt: "2026-08-13T00:00:00Z", Count: 1,
		}},
	}
}

func TestHistoryGapForDistinguishesStartingSessionFromMissingHistory(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		code      string
		message   string
		retryable bool
	}{
		{
			name: "starting generation", status: "starting", code: "session_starting",
			message: "generation is waiting for its AgentHub Session to start", retryable: true,
		},
		{
			name: "unbound generation outside startup", status: "recovering", code: "session_missing",
			message: "generation has no AgentHub Session reference", retryable: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gap := historyGapFor(generationRecord{Status: test.status}, errors.New("unbound session"))
			if gap.Code != test.code || gap.Message != test.message || gap.Retryable != test.retryable {
				t.Fatalf("history gap = %#v", gap)
			}
		})
	}
}

func TestResourceHistoryPreservesCancelledTurnStatus(t *testing.T) {
	fake := newHistoryFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	now := time.Now().Format(time.RFC3339Nano)
	if err := saveGenerationRecord(workspace.Path, generationRecord{
		ID: "gen-cancelled-history", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-cancelled-history", AgentHubSessionID: "ses-cancelled-history",
		Status: "idle", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	cancelled := historyTestTurn("turn-cancelled", 1, true)
	cancelled.Status = "cancelled"
	cancelled.FinalReplyPreview = ""
	fake.mu.Lock()
	fake.turns["ses-cancelled-history"] = []agentHubTurn{cancelled}
	fake.mu.Unlock()

	recorder := httptest.NewRecorder()
	manager.server.handleWorkspace(recorder, httptest.NewRequest(http.MethodGet,
		"/api/workspaces/"+workspace.ID+"/resources/project1.task1/history/turns?limit=1", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("cancelled history failed: %d %s", recorder.Code, recorder.Body.String())
	}
	var page resourceHistoryPage
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Segments) != 1 || len(page.Segments[0].Turns) != 1 || page.Segments[0].Turns[0].Status != "cancelled" || page.Segments[0].Turns[0].FinalReplyPreview != "" {
		t.Fatalf("cancelled history projection = %#v", page)
	}
}

func TestResourceHistoryTurnByIDDoesNotDependOnHeadPage(t *testing.T) {
	fake := newHistoryFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)

	now := time.Now().Format(time.RFC3339Nano)
	if err := saveGenerationRecord(workspace.Path, generationRecord{
		ID: "gen-targeted-history", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-targeted-history", AgentHubSessionID: "ses-targeted-history",
		Status: "idle", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.turns["ses-targeted-history"] = []agentHubTurn{
		historyTestTurn("turn-old", 1, true),
		historyTestTurn("turn-middle", 3, true),
		historyTestTurn("turn-new", 5, true),
	}
	fake.mu.Unlock()

	head := httptest.NewRecorder()
	manager.server.handleWorkspace(head, httptest.NewRequest(http.MethodGet,
		"/api/workspaces/"+workspace.ID+"/resources/project1.task1/history/turns?limit=1", nil))
	var page resourceHistoryPage
	if head.Code != http.StatusOK || json.Unmarshal(head.Body.Bytes(), &page) != nil ||
		len(page.Segments) != 1 || len(page.Segments[0].Turns) != 1 || page.Segments[0].Turns[0].TurnID != "turn-new" {
		t.Fatalf("history head = %d %#v %s", head.Code, page, head.Body.String())
	}

	targeted := httptest.NewRecorder()
	manager.server.handleWorkspace(targeted, httptest.NewRequest(http.MethodGet,
		"/api/workspaces/"+workspace.ID+"/resources/project1.task1/history/turns/by-id?generationId=gen-targeted-history&turnId=turn-old", nil))
	var detail resourceHistoryTurnDetail
	if targeted.Code != http.StatusOK || json.Unmarshal(targeted.Body.Bytes(), &detail) != nil ||
		detail.Turn.TurnID != "turn-old" || detail.Turn.Reference == "" || len(detail.Items) != 1 {
		t.Fatalf("targeted Turn = %d %#v %s", targeted.Code, detail, targeted.Body.String())
	}

	tests := []struct {
		name string
		path string
		code int
		want string
	}{
		{name: "missing identity", path: "?generationId=gen-targeted-history", code: http.StatusBadRequest, want: "invalid_request"},
		{name: "unknown generation", path: "?generationId=gen-missing&turnId=turn-old", code: http.StatusConflict, want: "generation_changed"},
		{name: "unknown turn", path: "?generationId=gen-targeted-history&turnId=turn-missing", code: http.StatusNotFound, want: "history_turn_not_found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			manager.server.handleWorkspace(recorder, httptest.NewRequest(http.MethodGet,
				"/api/workspaces/"+workspace.ID+"/resources/project1.task1/history/turns/by-id"+test.path, nil))
			if recorder.Code != test.code || !strings.Contains(recorder.Body.String(), test.want) {
				t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestResourceHistoryPaginatesAcrossGenerationsWithGap(t *testing.T) {
	fake := newHistoryFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)

	now := time.Now().Format(time.RFC3339Nano)
	records := []generationRecord{
		{ID: "gen-3", WorkspaceID: workspace.ID, ResourceID: "project1.task1", Generation: 3, GenerationID: "gen-3", AgentHubSessionID: "ses-3", Title: "Task (gen #3)", BindingKind: "profile", BindingName: "default", AgentHubAgentName: "fake-agent", Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "gen-2", WorkspaceID: workspace.ID, ResourceID: "project1.task1", Generation: 2, GenerationID: "gen-2", AgentHubSessionID: "ses-missing", Title: "Task (gen #2)", BindingKind: "profile", BindingName: "default", AgentHubAgentName: "fake-agent", Status: "archived", CreatedAt: now, UpdatedAt: now},
		{ID: "gen-1", WorkspaceID: workspace.ID, ResourceID: "project1.task1", Generation: 1, GenerationID: "gen-1", AgentHubSessionID: "ses-1", Title: "Original title (gen #1)", BindingKind: "agent", BindingName: "Old agent", AgentHubAgentName: "Old agent", Status: "archived", CreatedAt: now, UpdatedAt: now},
	}
	if err := saveGenerationRecord(workspace.Path, records[0]); err != nil {
		t.Fatal(err)
	}
	for _, record := range records[1:] {
		saveRetiredGenerationForTest(t, workspace.Path, record, "history_fixture")
	}
	turnA := historyTestTurn("turn-a", 1, true)
	turnA.Items = []agentHubTurnItem{{
		Type: "activity", StartEventID: 1, EndEventID: 2, StartedAt: now, EndedAt: now,
		Count: 4, ThinkingCount: 2, ReasoningUpdateCount: 5, ToolCallCount: 2,
	}}
	fake.mu.Lock()
	fake.turns["ses-3"] = []agentHubTurn{turnA, historyTestTurn("turn-b", 5, false)}
	fake.turns["ses-1"] = []agentHubTurn{historyTestTurn("turn-old", 1, true)}
	fake.mu.Unlock()
	fake.base.mu.Lock()
	fake.base.sessions["ses-3"] = agentHubSession{ID: "ses-3", State: "running"}
	fake.base.sessions["ses-1"] = agentHubSession{ID: "ses-1", State: "archived"}
	fake.base.events["ses-3"] = []agentHubEvent{{ID: 1, SessionID: "ses-3", TurnID: "turn-a", Type: "message.input", Time: now, Data: json.RawMessage(`{"text":"prompt turn-a"}`)}}
	fake.base.mu.Unlock()
	_, err := mutateResourceMailbox(workspace.Path, func(mailbox *resourceMailbox) error {
		mailbox.NextSequence = 1
		mailbox.Messages = append(mailbox.Messages, resourceMailboxMessage{
			ID: "msg-trigger", Sequence: 1, ResourceID: "project1.task1", Text: "prompt turn-b", Role: "agent",
			RequestedMode: "steer", ActualMode: "enqueue", DowngradeReason: "no_active_turn", Status: resourceMessageDelivered,
			AcceptedAt: now, DeliveredAt: now, GenerationID: "gen-3", AgentHubSessionID: "ses-3", TurnID: "turn-b",
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	first := httptest.NewRecorder()
	manager.server.handleWorkspace(first, httptest.NewRequest(http.MethodGet,
		"/api/workspaces/"+workspace.ID+"/resources/project1.task1/history/turns?limit=2", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first history page failed: %d %s", first.Code, first.Body.String())
	}
	var page resourceHistoryPage
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Segments) != 1 || len(page.Segments[0].Turns) != 2 || !page.Page.HasMore || page.Page.NextCursor == "" {
		t.Fatalf("first history page mismatch: %#v", page)
	}
	if page.Segments[0].Turns[1].TriggerDelivery == nil || page.Segments[0].Turns[1].TriggerDelivery.MessageID != "msg-trigger" {
		t.Fatalf("mailbox delivery was not associated: %#v", page.Segments[0].Turns[1])
	}
	turnReference := page.Segments[0].Turns[0].Reference

	for _, test := range []struct {
		since string
		count int
	}{
		{"previous", 1},
		{time.Unix(0, 0).UTC().Format(time.RFC3339), 2},
	} {
		response := httptest.NewRecorder()
		manager.server.handleWorkspace(response, httptest.NewRequest(http.MethodGet,
			"/api/workspaces/"+workspace.ID+"/resources/project1.task1/history/turns?limit=2&generationHeadersSince="+url.QueryEscape(test.since), nil))
		var withHeaders resourceHistoryPage
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &withHeaders) != nil ||
			len(withHeaders.GenerationHeaders) != test.count || withHeaders.GenerationHeaders[0].GenerationID != "gen-2" ||
			len(withHeaders.Segments) != 1 || len(withHeaders.Segments[0].Turns) != 2 || withHeaders.Page != page.Page {
			t.Fatalf("generation headers for %s: %d %s", test.since, response.Code, response.Body.String())
		}
	}

	// A new head Turn cannot shift the stable cursor into duplicate results.
	fake.mu.Lock()
	fake.turns["ses-3"] = append(fake.turns["ses-3"], historyTestTurn("turn-new", 8, false))
	fake.mu.Unlock()
	second := httptest.NewRecorder()
	manager.server.handleWorkspace(second, httptest.NewRequest(http.MethodGet,
		"/api/workspaces/"+workspace.ID+"/resources/project1.task1/history/turns?limit=2&cursor="+url.QueryEscape(page.Page.NextCursor), nil))
	if second.Code != http.StatusOK {
		t.Fatalf("second history page failed: %d %s", second.Code, second.Body.String())
	}
	if err := json.Unmarshal(second.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Segments) != 2 || page.Segments[0].Gap == nil || page.Segments[0].Gap.Code != "session_missing" ||
		len(page.Segments[1].Turns) != 1 || page.Segments[1].Turns[0].TurnID != "turn-old" || page.Page.HasMore {
		t.Fatalf("cross-generation gap page mismatch: %#v", page)
	}

	detailRecorder := httptest.NewRecorder()
	manager.server.handleWorkspace(detailRecorder, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/workspaces/%s/resources/project1.task1/history/turns/%s", workspace.ID, turnReference), nil))
	if detailRecorder.Code != http.StatusOK {
		t.Fatalf("Turn detail failed: %d %s", detailRecorder.Code, detailRecorder.Body.String())
	}
	var detail resourceHistoryTurnDetail
	if err := json.Unmarshal(detailRecorder.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Turn.TurnID != "turn-a" || len(detail.Items) != 1 || detail.Items[0].StartEventRef == "" ||
		detail.Items[0].Type != "activity" || detail.Items[0].ThinkingCount != 2 ||
		detail.Items[0].ReasoningUpdateCount != 5 || detail.Items[0].ToolCallCount != 2 {
		t.Fatalf("Turn detail mismatch: %#v", detail)
	}

	eventRecorder := httptest.NewRecorder()
	manager.server.handleWorkspace(eventRecorder, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/workspaces/%s/resources/project1.task1/history/events/%s", workspace.ID, detail.Items[0].StartEventRef), nil))
	if eventRecorder.Code != http.StatusOK {
		t.Fatalf("Event detail failed: %d %s", eventRecorder.Code, eventRecorder.Body.String())
	}
	var event resourceHistoryEventDetail
	if err := json.Unmarshal(eventRecorder.Body.Bytes(), &event); err != nil || event.SourceEvent.ID != 1 || event.Frame.Cursor != 1 || event.Generation.GenerationID != "gen-3" {
		t.Fatalf("Event detail mismatch: event=%#v err=%v", event, err)
	}
}

func TestResourceHistoryRejectsCrossResourceReference(t *testing.T) {
	fake := newHistoryFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	instanceID, err := resourceHistoryInstanceID(workspace)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := encodeResourceHistoryReference(resourceHistoryReference{
		Kind: "turn", InstanceID: instanceID, ResourceID: "project1", GenerationID: "gen-1", TurnID: "turn-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	manager.server.handleWorkspace(recorder, httptest.NewRequest(http.MethodGet,
		"/api/workspaces/"+workspace.ID+"/resources/project1.task1/history/turns/"+reference, nil))
	var response map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &response)
	if recorder.Code != http.StatusBadRequest || response["code"] != "invalid_history_reference" {
		t.Fatalf("cross-resource reference response = %d %#v", recorder.Code, response)
	}
}

func TestResourceLiveRoutesBindCurrentGenerationAndUploadToResource(t *testing.T) {
	fake := newHistoryFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	now := time.Now().Format(time.RFC3339Nano)
	record := generationRecord{
		ID: "gen-current", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-current", AgentHubSessionID: "ses-current",
		Status: "idle", CreatedAt: now, UpdatedAt: now,
	}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	fake.base.mu.Lock()
	fake.base.sessions["ses-current"] = agentHubSession{ID: "ses-current", State: "idle"}
	fake.base.events["ses-current"] = []agentHubEvent{{ID: 1, SessionID: "ses-current", Type: "message.input", Time: now, Data: json.RawMessage(`{"text":"hello"}`)}}
	fake.base.mu.Unlock()

	events := httptest.NewRecorder()
	manager.server.handleWorkspace(events, httptest.NewRequest(http.MethodGet,
		"/api/workspaces/"+workspace.ID+"/resources/project1.task1/events?generationId=gen-current&after=0&limit=10", nil))
	if events.Code != http.StatusOK || events.Header().Get("X-PUA-Generation-ID") != "gen-current" || !strings.Contains(events.Body.String(), `"cursor":1`) {
		t.Fatalf("resource events response = %d headers=%v body=%s", events.Code, events.Header(), events.Body.String())
	}

	mismatch := httptest.NewRecorder()
	manager.server.handleWorkspace(mismatch, httptest.NewRequest(http.MethodGet,
		"/api/workspaces/"+workspace.ID+"/resources/project1.task1/events?generationId=gen-stale", nil))
	if mismatch.Code != http.StatusConflict || !strings.Contains(mismatch.Body.String(), "generation_changed") {
		t.Fatalf("stale generation response = %d %s", mismatch.Code, mismatch.Body.String())
	}

	oldGeneration := generationRecord{
		ID: "gen-old", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 0, GenerationID: "gen-old", AgentHubSessionID: "ses-old",
		Status: "stopped", CreatedAt: now, UpdatedAt: now,
	}
	saveRetiredGenerationForTest(t, workspace.Path, oldGeneration, "history_fixture")
	fake.base.mu.Lock()
	fake.base.sessions["ses-old"] = agentHubSession{ID: "ses-old", State: "stopped"}
	fake.base.events["ses-old"] = []agentHubEvent{{ID: 2, SessionID: "ses-old", Type: "message.input", Time: now, Data: json.RawMessage(`{"text":"archived"}`)}}
	fake.base.mu.Unlock()

	historical := httptest.NewRecorder()
	manager.server.handleWorkspace(historical, httptest.NewRequest(http.MethodGet,
		"/api/workspaces/"+workspace.ID+"/resources/project1.task1/events?generationId=gen-old&after=0&limit=10", nil))
	if historical.Code != http.StatusOK || historical.Header().Get("X-PUA-Generation-ID") != "gen-old" || !strings.Contains(historical.Body.String(), `"cursor":2`) {
		t.Fatalf("historical generation events response = %d headers=%v body=%s", historical.Code, historical.Header(), historical.Body.String())
	}

	upload := httptest.NewRecorder()
	request := agentUploadRequest(t, "notes.txt", "resource attachment")
	request.URL.Path = "/api/workspaces/" + workspace.ID + "/resources/project1.task1/uploads"
	manager.server.handleWorkspace(upload, request)
	if upload.Code != http.StatusOK {
		t.Fatalf("resource upload response = %d %s", upload.Code, upload.Body.String())
	}
	opened, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	resource, err := opened.ResourceValue("project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(workspace.Path, filepath.FromSlash(resource.Path), "artifacts", "upload", "notes.txt"))
	if err != nil || string(data) != "resource attachment" {
		t.Fatalf("resource attachment = %q err=%v", data, err)
	}
	records, err := loadGenerationRecords(workspace.Path)
	if err != nil || len(records) != 2 {
		t.Fatalf("upload created or removed a generation: runs=%#v err=%v", records, err)
	}
}

func TestResourceStreamAllowsResumableSuspendedCurrentGeneration(t *testing.T) {
	fake := newHistoryFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	now := time.Now().Format(time.RFC3339Nano)
	record := generationRecord{
		ID: "gen-suspended", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-suspended", AgentHubSessionID: "ses-suspended",
		Status: "idle-suspended", IdleSleepStopRequested: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	fake.base.mu.Lock()
	fake.base.sessions[record.AgentHubSessionID] = agentHubSession{ID: record.AgentHubSessionID, State: "stopped"}
	fake.base.events[record.AgentHubSessionID] = []agentHubEvent{{
		ID: 1, SessionID: record.AgentHubSessionID, Type: "session.state", Time: now,
		Data: json.RawMessage(`{"state":"stopped"}`),
	}}
	fake.base.mu.Unlock()

	stream := httptest.NewRecorder()
	manager.server.handleWorkspace(stream, httptest.NewRequest(http.MethodGet,
		"/api/workspaces/"+workspace.ID+"/resources/project1.task1/stream?generationId=gen-suspended", nil))
	if stream.Code != http.StatusOK || stream.Header().Get("X-PUA-Generation-ID") != record.GenerationID || !strings.Contains(stream.Body.String(), `"state":"stopped"`) {
		t.Fatalf("suspended resource stream response = %d headers=%v body=%s", stream.Code, stream.Header(), stream.Body.String())
	}
}

func TestResourceEventStreamabilityFollowsGenerationLifecycle(t *testing.T) {
	base := generationRecord{Status: "stopped", AgentHubSessionID: "ses-current"}
	tests := []struct {
		name   string
		record generationRecord
		want   bool
	}{
		{name: "live idle", record: generationRecord{Status: "idle"}, want: true},
		{name: "idle suspended", record: generationRecord{Status: "idle-suspended", AgentHubSessionID: "ses-current"}, want: true},
		{name: "recoverable stopped", record: base, want: true},
		{name: "missing session", record: generationRecord{Status: "stopped"}, want: false},
		{name: "resume unavailable", record: func() generationRecord { value := base; value.SessionResumeUnavailable = true; return value }(), want: false},
		{name: "replacement pending", record: func() generationRecord { value := base; value.ReplacementPending = true; return value }(), want: false},
		{name: "archive requested", record: func() generationRecord { value := base; value.ArchivedTaskStopRequested = true; return value }(), want: false},
		{name: "retired", record: generationRecord{Status: "archived", AgentHubSessionID: "ses-current"}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isResourceEventStreamable(test.record); got != test.want {
				t.Fatalf("isResourceEventStreamable(%#v) = %v, want %v", test.record, got, test.want)
			}
		})
	}
}

func TestResourceHistoryReferencesSurviveRestartAndResourceArchive(t *testing.T) {
	fake := newHistoryFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, configPath := newRuntimeTestManager(t, hub.URL)
	now := time.Now().Format(time.RFC3339Nano)
	records := []generationRecord{
		{ID: "gen-new", WorkspaceID: workspace.ID, ResourceID: "project1.task1", Generation: 2, GenerationID: "gen-new", AgentHubSessionID: "ses-new", Status: "idle", CreatedAt: now, UpdatedAt: now},
		{ID: "gen-old", WorkspaceID: workspace.ID, ResourceID: "project1.task1", Generation: 1, GenerationID: "gen-old", AgentHubSessionID: "ses-old", Status: "archived", CreatedAt: now, UpdatedAt: now},
	}
	if err := saveGenerationRecord(workspace.Path, records[0]); err != nil {
		t.Fatal(err)
	}
	saveRetiredGenerationForTest(t, workspace.Path, records[1], "history_fixture")
	fake.mu.Lock()
	fake.turns["ses-new"] = []agentHubTurn{historyTestTurn("same-turn-id", 10, true)}
	fake.turns["ses-old"] = []agentHubTurn{historyTestTurn("same-turn-id", 1, true)}
	fake.mu.Unlock()

	first := httptest.NewRecorder()
	manager.server.handleWorkspace(first, httptest.NewRequest(http.MethodGet,
		"/api/workspaces/"+workspace.ID+"/resources/project1.task1/history/turns?limit=1", nil))
	var head resourceHistoryPage
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &head) != nil || len(head.Segments) != 1 || len(head.Segments[0].Turns) != 1 || !head.Page.HasMore {
		t.Fatalf("history head before restart = %d %#v %s", first.Code, head, first.Body.String())
	}
	turnRef, cursor := head.Segments[0].Turns[0].Reference, head.Page.NextCursor

	opened, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.ArchiveResource("project1.task1"); err != nil {
		t.Fatal(err)
	}
	restartedServer := &server{config: configPath, addr: "127.0.0.1:4936"}
	restarted := newAgentManager(restartedServer)
	restartedServer.agents = restarted

	detail := httptest.NewRecorder()
	restartedServer.handleWorkspace(detail, httptest.NewRequest(http.MethodGet,
		"/api/workspaces/"+workspace.ID+"/resources/project1.task1/history/turns/"+turnRef, nil))
	var turnDetail resourceHistoryTurnDetail
	if detail.Code != http.StatusOK || json.Unmarshal(detail.Body.Bytes(), &turnDetail) != nil || turnDetail.Turn.Generation.GenerationID != "gen-new" {
		t.Fatalf("archived Turn detail after restart = %d %s", detail.Code, detail.Body.String())
	}
	older := httptest.NewRecorder()
	restartedServer.handleWorkspace(older, httptest.NewRequest(http.MethodGet,
		"/api/workspaces/"+workspace.ID+"/resources/project1.task1/history/turns?limit=1&cursor="+url.QueryEscape(cursor), nil))
	var tail resourceHistoryPage
	if older.Code != http.StatusOK || json.Unmarshal(older.Body.Bytes(), &tail) != nil || len(tail.Segments) != 1 ||
		len(tail.Segments[0].Turns) != 1 || tail.Segments[0].Turns[0].Generation.GenerationID != "gen-old" || tail.Page.HasMore {
		t.Fatalf("archived history tail after restart = %d %#v %s", older.Code, tail, older.Body.String())
	}
	if tail.Segments[0].Turns[0].Reference == turnRef {
		t.Fatal("same AgentHub Turn id collided across generations")
	}
}
