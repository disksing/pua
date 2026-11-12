package serve

import (
	"context"
	"net/http/httptest"
	"testing"
)

// A successful delivery appends its canonical message.input event at the end
// of the session log, so the post-delivery canonical lookup must scan from
// the pre-delivery cursor instead of rescanning the full history. Large
// sessions otherwise pay a multi-second event rescan on every message while
// holding the resource job queue, which stalls the chat UI's post-send
// status refresh and keeps the "Submitting" indicator visible until the
// agent's first message arrives.
func TestDeliveredMailboxTurnIDScansFromPreDeliveryCursor(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	server := httptest.NewServer(fake)
	defer server.Close()
	client, err := newAgentHubClient(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := "ses_cursor_hint"
	message := resourceMailboxMessage{ID: "msg-cursor-hint", Text: "hello canonical", Role: "user", ActualMode: resourceMessageModeEnqueue}

	fake.mu.Lock()
	fake.sessions[sessionID] = agentHubSession{ID: sessionID, State: "running"}
	// Older history that a hinted scan must skip.
	for range 5 {
		fake.appendLocked(sessionID, "agent.message", map[string]any{"text": "old"})
	}
	before := fake.sessions[sessionID]
	canonical := fake.appendLocked(sessionID, "message.input", fakeMessageInputData(agentHubInboundMessage{
		MessageID: message.ID, Text: message.Text, Role: "user",
	}))
	fake.events[sessionID][len(fake.events[sessionID])-1].TurnID = "turn_42"
	after := fake.sessions[sessionID]
	fake.mu.Unlock()

	delivered := agentHubSession{ID: sessionID, State: "running"}
	turnID := deliveredMailboxTurnID(context.Background(), client, sessionID, message, delivered, before)
	if turnID != "turn_42" {
		t.Fatalf("canonical Turn from the post-delivery event was not found: %q", turnID)
	}

	// A snapshot taken after the canonical event must not find it again; the
	// enqueue fallback stays unsubscribed instead of inventing a Turn.
	if turnID := deliveredMailboxTurnID(context.Background(), client, sessionID, message, delivered, after); turnID != "" {
		t.Fatalf("stale pre-delivery cursor must not rediscover the canonical event: %q", turnID)
	}

	// Recovery paths pass 0 and still scan the full history.
	if _, found, err := findCanonicalAgentHubMessage(context.Background(), client, sessionID, message, 0); err != nil || !found {
		t.Fatalf("full recovery scan did not find the canonical event: found=%v err=%v", found, err)
	}
	if canonical.ID != before.LastEventID+1 {
		t.Fatalf("test setup drifted: canonical event %d does not follow the pre-delivery cursor %d", canonical.ID, before.LastEventID)
	}
}
