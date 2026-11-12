package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func previewInputs(fake *runtimeFakeAgentHub) map[string]agentHubInboundMessage {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	result := map[string]agentHubInboundMessage{}
	for id, input := range fake.messageInputs {
		if strings.HasPrefix(id, "preview-") {
			result[id] = input
		}
	}
	return result
}

func TestQueuePreviewsDoNotReplayAcrossTurnsAndRestart(t *testing.T) {
	m, w, fake, client := workflowFixture(t)
	original := acceptTestResourceMessage(t, m, w, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	a := *workFile(t, w).find(original.ID)
	b := queueWorkflowUser(t, m, w, "b", "First question")
	c := queueWorkflowUser(t, m, w, "c", "Second question")
	reconcileWorkflow(t, m, w, client)
	previews := previewInputs(fake)
	if len(previews) != 2 {
		t.Fatalf("initial previews = %d", len(previews))
	}
	for _, input := range previews {
		_, role, sender := puaMessagePresentation(input.Text, input.Role, input.Sender, input.Payload)
		if role != "system" || sender != nil || input.ExpectedTurnID != a.TurnID {
			t.Fatalf("invalid preview identity: %+v", input)
		}
	}
	closeWorkflowTurn(fake, a, "completed", "done")
	reconcileWorkflow(t, m, w, client)
	if got := previewInputs(fake); !reflect.DeepEqual(got, previews) {
		t.Fatalf("old queue replayed after A: %+v", got)
	}
	// Reload both the manager and durable state; old successful v1 records
	// without attempt reservations must also keep suppressing previews.
	file := workFile(t, w)
	file.find(a.ID).PreviewAttempts = nil
	if err := saveTurnWork(w.Path, "project1.task1", file); err != nil {
		t.Fatal(err)
	}
	m.waitBackground()
	m = newAgentManager(m.server)
	m.server.agents = m
	reconcileWorkflow(t, m, w, client)
	if got := previewInputs(fake); !reflect.DeepEqual(got, previews) {
		t.Fatal("restart replayed old previews")
	}
	d := queueWorkflowUser(t, m, w, "d", "New arrival")
	reconcileWorkflow(t, m, w, client)
	if got := previewInputs(fake); len(got) != 3 {
		t.Fatalf("new arrival did not get exactly one preview: %+v", got)
	}
	for _, message := range []resourceMailboxMessage{c, d} {
		queued, _, _ := mailboxMessageByID(w.Path, message.ID)
		if queued.Status != resourceMessageQueued || queued.DeliveredAt != "" || queued.Role != "user" {
			t.Fatalf("preview altered formal receipt: %+v", queued)
		}
	}
	for _, message := range []resourceMailboxMessage{b, c, d} {
		work := workFile(t, w).find(message.ID)
		if work == nil || work.Context.OpenerRole != "user" {
			t.Fatalf("formal input did not start: %s", message.ID)
		}
		fake.mu.Lock()
		input := fake.messageInputs[message.ID]
		fake.mu.Unlock()
		_, role, sender := puaMessagePresentation(input.Text, input.Role, input.Sender, input.Payload)
		if role != "user" || sender == nil || sender.Name != "reader" {
			t.Fatalf("formal user identity lost: %+v", input)
		}
		closeWorkflowTurn(fake, *work, "completed", "reply")
		reconcileWorkflow(t, m, w, client)
	}
	if len(previewInputs(fake)) != 3 {
		t.Fatal("remaining list repeated at another Turn boundary")
	}
}

func TestQueuePreviewUnknownOutcomeStaysBoundToOriginalTurn(t *testing.T) {
	m, w, fake, _ := workflowFixture(t)
	original := acceptTestResourceMessage(t, m, w, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	a := *workFile(t, w).find(original.ID)
	b := queueWorkflowUser(t, m, w, "b", "First")
	c := queueWorkflowUser(t, m, w, "c", "Second")
	hub := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			// Acceptance succeeds but its acknowledgement is lost.
			fake.ServeHTTP(httptest.NewRecorder(), r)
			rw.WriteHeader(http.StatusBadGateway)
			return
		}
		fake.ServeHTTP(rw, r)
	}))
	defer hub.Close()
	client, err := newAgentHubClient(hub.URL, hub.Client())
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	session := fake.sessions[a.SessionID]
	fake.mu.Unlock()
	if err := m.previewQueuedInput(context.Background(), w, session, client, c); err != nil {
		t.Fatal(err)
	}
	f := workFile(t, w)
	if f.find(a.ID).Previews[c.ID] || f.find(a.ID).PreviewAttempts[c.ID].ExpectedTurnID != a.TurnID {
		t.Fatal("unknown outcome lost durable ownership")
	}
	previews := previewInputs(fake)
	_, goodClient, _ := m.agentHubRuntimeConfig()
	// The stored wire request is stable even if presentation changes on retry.
	changed := c
	changed.Text = "changed presentation"
	if err := m.previewQueuedInput(context.Background(), w, session, goodClient, changed); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(previews, previewInputs(fake)) || !workFile(t, w).find(a.ID).Previews[c.ID] {
		t.Fatal("same-Turn retry was not idempotent")
	}
	// Simulate a crash before the success receipt was written, then end A.
	f = workFile(t, w)
	delete(f.find(a.ID).Previews, c.ID)
	if err := saveTurnWork(w.Path, "project1.task1", f); err != nil {
		t.Fatal(err)
	}
	closeWorkflowTurn(fake, a, "completed", "")
	reconcileWorkflow(t, m, w, goodClient)
	if workFile(t, w).find(b.ID) == nil || !reflect.DeepEqual(previews, previewInputs(fake)) {
		t.Fatal("unknown preview replayed into new Turn")
	}
	queued, _, _ := mailboxMessageByID(w.Path, c.ID)
	if queued.Status != resourceMessageQueued || queued.LastError != "" {
		t.Fatalf("unknown preview contaminated formal input: %+v", queued)
	}
}

func TestChatInputRoleUpgradePreservesReceiptAndCanonicalIdentity(t *testing.T) {
	_, w, _, _ := workflowFixture(t)
	input := resourceMailboxMessage{
		ID: "old-chat", ResourceID: "project1.task1", Text: "Question", Role: "system",
		Type: "user_chat_input", RequestedMode: resourceMessageModeSteer,
		Sender:    &agentHubMessageSender{Name: "reader"},
		Causation: &resourceMessageCausation{Type: "chat", AutomaticChat: true, MessageID: "input"},
	}
	legacy := input
	legacy.Status = resourceMessageQueued
	if _, err := mutateResourceMailboxForResource(w.Path, input.ResourceID, func(mailbox *resourceMailbox) error {
		mailbox.Messages = append(mailbox.Messages, legacy)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	canonical, err := agentHubMailboxMessage(legacy)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := acceptGeneratedMailboxMessage(w.Path, input)
	if err != nil || retried.Role != "system" || !canonicalAgentHubMessageMatches(canonical, retried) {
		t.Fatalf("old delivery identity changed: %+v %v", retried, err)
	}
	rawBefore := string(canonical.Payload)
	text, role, _ := puaMessagePresentation(canonical.Text, "", nil, canonical.Payload)
	if text != "Question" || role != "user" || string(canonical.Payload) != rawBefore {
		t.Fatal("historical user presentation not corrected independently")
	}
	var decoded puaMessagePayload
	_ = json.Unmarshal(canonical.Payload, &decoded)
	if decoded.Role != "system" {
		t.Fatal("presentation rewrote historical payload")
	}
}
