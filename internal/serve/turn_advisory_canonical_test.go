package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func TestCanonicalAdvisoryVerifiesLegacySourceWithoutRelaxingIdentity(t *testing.T) {
	payload, err := marshalPUAMessagePayload(puaMessagePayload{
		Schema: puaMessagePayloadSchema, Role: "system", Type: "chat_state", Text: "Chat changed",
	})
	if err != nil {
		t.Fatal(err)
	}
	expected := agentHubInboundMessage{
		SchemaVersion: 2, MessageID: "advisory", Text: "Chat changed",
		Steer: true, ExpectedTurnID: "origin", Payload: payload,
	}
	tests := []struct {
		name       string
		projected  func(map[string]any)
		source     func(*agentHubEvent, map[string]any)
		frame      func(*agentHubSemanticFrame)
		sourceCode int
		legacy     bool
		wantOK     bool
		wantDetail bool
	}{
		{name: "complete projection", wantOK: true},
		{name: "legacy projection", legacy: true, wantOK: true, wantDetail: true},
		{name: "legacy source missing scope", legacy: true, source: func(_ *agentHubEvent, data map[string]any) { delete(data, "expectedTurnId") }, wantDetail: true},
		{name: "legacy source wrong scope", legacy: true, source: func(_ *agentHubEvent, data map[string]any) { data["expectedTurnId"] = "other" }, wantDetail: true},
		{name: "legacy source wrong text", legacy: true, source: func(_ *agentHubEvent, data map[string]any) { data["text"] = "other" }, wantDetail: true},
		{name: "legacy source wrong payload", legacy: true, source: func(_ *agentHubEvent, data map[string]any) {
			data["payload"] = map[string]any{"schema": puaMessagePayloadSchema, "text": "other"}
		}, wantDetail: true},
		{name: "legacy source wrong steer", legacy: true, source: func(_ *agentHubEvent, data map[string]any) { data["steer"] = false }, wantDetail: true},
		{name: "legacy source wrong schema", legacy: true, source: func(_ *agentHubEvent, data map[string]any) { data["schemaVersion"] = 1 }, wantDetail: true},
		{name: "legacy source wrong ID", legacy: true, source: func(_ *agentHubEvent, data map[string]any) { data["messageId"] = "other" }, wantDetail: true},
		{name: "legacy source wrong session", legacy: true, source: func(event *agentHubEvent, _ map[string]any) { event.SessionID = "other" }, wantDetail: true},
		{name: "legacy source missing session", legacy: true, source: func(event *agentHubEvent, _ map[string]any) { event.SessionID = "" }, wantDetail: true},
		{name: "legacy source wrong Turn", legacy: true, source: func(event *agentHubEvent, _ map[string]any) { event.TurnID = "other" }, wantDetail: true},
		{name: "legacy source wrong event", legacy: true, source: func(event *agentHubEvent, _ map[string]any) { event.ID++ }, wantDetail: true},
		{name: "legacy source wrong type", legacy: true, source: func(event *agentHubEvent, _ map[string]any) { event.Type = "provider.event" }, wantDetail: true},
		{name: "legacy detail unavailable", legacy: true, sourceCode: http.StatusServiceUnavailable, wantDetail: true},
		{name: "explicit empty scope", projected: func(data map[string]any) { data["expectedTurnId"] = "" }},
		{name: "explicit wrong scope", projected: func(data map[string]any) { data["expectedTurnId"] = "other" }},
		{name: "complete wrong content", projected: func(data map[string]any) { data["text"] = "other" }},
		{name: "complete wrong Turn", frame: func(frame *agentHubSemanticFrame) {
			frame.Source.TurnID, frame.Events[0].TurnID = "other", "other"
		}},
		{name: "complete no Turn", frame: func(frame *agentHubSemanticFrame) {
			frame.Source.TurnID, frame.Events[0].TurnID = "", ""
		}},
		{name: "complete wrong session", frame: func(frame *agentHubSemanticFrame) { frame.Events[0].SessionID = "other" }},
		{name: "legacy wrong source cursor", legacy: true, frame: func(frame *agentHubSemanticFrame) { frame.Events[0].SourceEventID++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := agentHubEvent{ID: 9, Type: "message.input", SessionID: "session", TurnID: "origin"}
			raw := fakeMessageInputData(expected)
			projected := fakeMessageInputData(expected)
			if test.legacy {
				delete(projected, "expectedTurnId")
			}
			if test.projected != nil {
				test.projected(projected)
			}
			data, _ := json.Marshal(projected)
			source.Data = data
			frame := runtimeFakeSemanticFrame(source)
			if test.frame != nil {
				test.frame(&frame)
			}
			if test.source != nil {
				test.source(&source, raw)
			}
			source.Data, _ = json.Marshal(raw)
			detailCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/event/") {
					detailCalls++
					if test.sourceCode != 0 {
						w.WriteHeader(test.sourceCode)
						return
					}
					writeRuntimeFakeJSON(w, agentHubEventDetail{
						Schema: "agenthub.event-detail.v1", SourceEvent: source, Frame: frame,
					})
					return
				}
				writeRuntimeFakeJSON(w, map[string]any{
					"schema": "agenthub.semantic-events.v1", "frames": []agentHubSemanticFrame{frame}, "latestCursor": 9,
				})
			}))
			defer server.Close()
			client := &agentHubClient{endpoint: server.URL, httpClient: server.Client()}
			advisory := turnAdvisory{ID: expected.MessageID, SessionID: "session", OriginTurnID: "origin", Input: &expected}
			turn, found, err := canonicalAdvisory(context.Background(), client, &advisory)
			if test.wantOK {
				if err != nil || !found || turn != "origin" {
					t.Fatalf("canonical recovery: turn=%q found=%v err=%v", turn, found, err)
				}
			} else if err == nil || found {
				t.Fatalf("conflicting input accepted: turn=%q found=%v err=%v", turn, found, err)
			}
			if (detailCalls == 1) != test.wantDetail || detailCalls > 1 {
				t.Fatalf("detail calls=%d wantDetail=%v", detailCalls, test.wantDetail)
			}
		})
	}
}

func advisoryProjectionHandler(fake *runtimeFakeAgentHub, legacy bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		fake.ServeHTTP(recorder, r)
		if legacy && strings.HasSuffix(r.URL.Path, "/events") && recorder.Code == http.StatusOK {
			var page struct {
				Schema       string                  `json:"schema"`
				Frames       []agentHubSemanticFrame `json:"frames"`
				LatestCursor int64                   `json:"latestCursor"`
			}
			if json.Unmarshal(recorder.Body.Bytes(), &page) == nil {
				for frameIndex := range page.Frames {
					for eventIndex := range page.Frames[frameIndex].Events {
						event := &page.Frames[frameIndex].Events[eventIndex]
						var data map[string]any
						if event.Type == "message.input" && json.Unmarshal(event.Data, &data) == nil {
							delete(data, "expectedTurnId")
							event.Data, _ = json.Marshal(data)
						}
					}
				}
				writeRuntimeFakeJSON(w, page)
				return
			}
		}
		for key, values := range recorder.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	})
}

func TestScopedUnknownAdvisoryRestartsAndDrainsFIFOWithoutResend(t *testing.T) {
	for _, recovery := range []struct {
		legacy   bool
		terminal string
	}{
		{legacy: false},
		{legacy: true},
		{legacy: false, terminal: "completed"},
		{legacy: true, terminal: "completed"},
		{legacy: true, terminal: "failed"},
		{legacy: true, terminal: "cancelled"},
	} {
		t.Run(fmt.Sprintf("legacy=%v/terminal=%s", recovery.legacy, recovery.terminal), func(t *testing.T) {
			fake := newRuntimeFakeAgentHub()
			fake.enforceMessageIDs, fake.materializeTurns = true, true
			server := httptest.NewServer(advisoryProjectionHandler(fake, recovery.legacy))
			defer server.Close()
			manager, workspace, _ := newRuntimeTestManager(t, server.URL)
			root, err := app.OpenWorkspace(workspace.Path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := root.RegisterUser("reader"); err != nil {
				t.Fatal(err)
			}
			message := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "background", resourceMessageModeEnqueue, nil)
			origin := *workFile(t, workspace).find(message.ID)
			setAdvisoryProvider(fake, origin.SessionID, "codex")
			fake.mu.Lock()
			fake.messageHook = func(_ string, input agentHubInboundMessage) {
				if strings.HasPrefix(input.MessageID, "chat-state-") {
					fake.mu.Lock()
					fake.failEvents = true
					fake.mu.Unlock()
				}
			}
			fake.mu.Unlock()
			mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "open", chatRequest{}, false))
			_, client, err := manager.agentHubRuntimeConfig()
			if err != nil {
				t.Fatal(err)
			}
			reconcileWorkflow(t, manager, workspace, client)
			file := workFile(t, workspace)
			if len(file.Advisories) != 1 || file.Advisories[0].State != "unknown" || !file.advisoryUnowned() {
				t.Fatalf("unknown scoped advisory not retained: %+v", file.Advisories)
			}
			advisory := file.Advisories[0]
			first := queueWorkflowUser(t, manager, workspace, "first", "User input")
			second := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "Agent input", resourceMessageModeEnqueue, nil)
			fake.mu.Lock()
			fake.failEvents, fake.messageHook = false, nil
			fake.mu.Unlock()
			if recovery.terminal != "" {
				closeWorkflowTurn(fake, origin, recovery.terminal, "")
			}
			restarted, restartedClient := restartWorkflowManager(t, manager)
			for range 2 {
				reconcileWorkflow(t, restarted, workspace, restartedClient)
			}
			recovered := workFile(t, workspace).Advisories[0]
			wantState := "accepted"
			if recovery.terminal == "completed" {
				wantState = "completed"
			} else if recovery.terminal != "" {
				wantState = "failed"
			}
			if recovered.State != wantState || recovered.TurnID != origin.TurnID || recovered.HistoryRef == "" ||
				!reflect.DeepEqual(recovered.Input, advisory.Input) {
				t.Fatalf("unknown advisory not recovered exactly: %+v", recovered)
			}
			if recovery.terminal == "" {
				if workFile(t, workspace).find(first.ID) != nil || workFile(t, workspace).find(second.ID) != nil {
					t.Fatal("formal input crossed the active advisory barrier")
				}
				closeWorkflowTurn(fake, origin, "completed", "")
			}
			restarted, restartedClient = restartWorkflowManager(t, restarted)
			for range 2 {
				reconcileWorkflow(t, restarted, workspace, restartedClient)
			}
			file = workFile(t, workspace)
			if advisoryPending(file.Advisories[0]) || file.find(first.ID) == nil || file.find(second.ID) != nil {
				t.Fatalf("first FIFO input not delivered alone: %+v", file)
			}
			firstWork := *file.find(first.ID)
			if firstWork.TurnID == origin.TurnID {
				t.Fatal("user input reused the advisory Turn")
			}
			closeWorkflowTurn(fake, firstWork, "completed", "")
			for range 2 {
				reconcileWorkflow(t, restarted, workspace, restartedClient)
			}
			if workFile(t, workspace).find(second.ID) == nil {
				t.Fatal("second FIFO input remained blocked")
			}
			for _, id := range []string{advisory.ID, first.ID, second.ID} {
				if count := countSentInput(fake, id); count != 1 {
					t.Fatalf("input %s sent %d times", id, count)
				}
			}
		})
	}
}

func TestConflictingUnknownAdvisoryRetainsBarrierAfterRestart(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	message := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	origin := *workFile(t, workspace).find(message.ID)
	setAdvisoryProvider(fake, origin.SessionID, "codex")
	mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "close", chatRequest{}, false))
	reconcileWorkflow(t, manager, workspace, client)
	file := workFile(t, workspace)
	if len(file.Advisories) != 1 || file.Advisories[0].State != "accepted" {
		t.Fatalf("missing accepted advisory: %+v", file.Advisories)
	}
	advisory := &file.Advisories[0]
	advisory.TurnID, advisory.HistoryRef, advisory.State = "", "", "unknown"
	if err := saveTurnWork(workspace.Path, "project1.task1", file); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	for index := range fake.events[origin.SessionID] {
		event := &fake.events[origin.SessionID][index]
		var input agentHubInboundMessage
		if event.Type == "message.input" && json.Unmarshal(event.Data, &input) == nil && input.MessageID == advisory.ID {
			input.Text = "conflicting durable input"
			event.Data, _ = json.Marshal(input)
		}
	}
	fake.mu.Unlock()
	queued := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "Waiting", resourceMessageModeEnqueue, nil)
	closeWorkflowTurn(fake, origin, "completed", "")
	restarted, restartedClient := restartWorkflowManager(t, manager)
	for range 2 {
		reconcileWorkflow(t, restarted, workspace, restartedClient)
	}
	file = workFile(t, workspace)
	if !file.advisoryUnowned() || file.Advisories[0].State != "unknown" ||
		!strings.Contains(file.Advisories[0].LastError, "conflicts") || file.find(queued.ID) != nil {
		t.Fatalf("conflict silently released the barrier: %+v", file)
	}
	if countSentInput(fake, advisory.ID) != 1 || countSentInput(fake, queued.ID) != 0 {
		t.Fatal("conflict resent an input")
	}
}
