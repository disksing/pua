package client

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/disksing/pua/agenthub/internal/session"
)

func TestRequireCapabilitiesRejectsOldAndIncompleteDaemons(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "old daemon without negotiation fields",
			body: `{"version":"0.1.0"}`,
			want: `incompatible AgentHub API version ""`,
		},
		{
			name: "missing capability",
			body: `{"apiVersion":"1","capabilities":["session.source"]}`,
			want: "events.lossless-replay",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			err := New(server.URL).RequireCapabilities("1", "session.source", "events.lossless-replay")
			var incompatible *IncompatibleDaemonError
			if !errors.As(err, &incompatible) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want IncompatibleDaemonError containing %q", err, test.want)
			}
		})
	}
}

func TestRequireCapabilitiesAllowsUnknownAdditions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"1","capabilities":["session.source","future.feature"]}`))
	}))
	defer server.Close()
	if err := New(server.URL).RequireCapabilities("1", "session.source"); err != nil {
		t.Fatal(err)
	}
}

func TestRequestReturnsStructuredAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"runtime_unavailable","message":"runtime is unavailable","retryable":true,"details":{"scope":"runtime"},"requestId":"req_1"}}`))
	}))
	defer server.Close()
	_, err := New(server.URL).Status()
	var apiError *APIError
	if !errors.As(err, &apiError) {
		t.Fatalf("error = %v, want APIError", err)
	}
	if apiError.StatusCode != http.StatusServiceUnavailable || apiError.Code != "runtime_unavailable" || !apiError.Retryable || apiError.RequestID != "req_1" {
		t.Fatalf("APIError = %+v", apiError)
	}
}

func TestSendMessageInputResultExposesProviderDelivery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/sessions/ses_test/messages" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(session.MessageSendResult{
			Session: session.Session{ID: "ses_test"},
			Delivery: session.MessageProviderDelivery{
				MessageID: "msg-1",
				State:     session.MessageProviderDeliveryPending,
			},
		})
	}))
	defer server.Close()
	result, err := New(server.URL).SendMessageInputResult("ses_test", session.MessageInput{Text: "hello", MessageID: "msg-1"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session.ID != "ses_test" || result.Delivery.MessageID != "msg-1" || result.Delivery.State != session.MessageProviderDeliveryPending {
		t.Fatalf("message result = %+v", result)
	}
}

func TestEventsAfterPagesToInitialDurableHead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		latest := int64(2500)
		end := min(after+1000, latest)
		frames := make([]map[string]any, 0, end-after)
		for id := after + 1; id <= end; id++ {
			frames = append(frames, map[string]any{"schema": "agenthub.semantic-events.v1", "cursor": id, "mode": "replace", "source": map[string]any{"eventId": id, "type": "provider.test", "sessionId": "ses_test", "time": "2026-08-22T00:00:00Z"}, "events": []any{}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema":       "agenthub.semantic-events.v1",
			"frames":       frames,
			"latestCursor": latest,
			"page": map[string]any{
				"after": after, "limit": 1000, "nextAfter": end, "hasMore": end < latest,
			},
		})
	}))
	defer server.Close()
	events, err := New(server.URL).EventsAfter("ses_test", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2500 || events[0].Cursor != 1 || events[2499].Cursor != 2500 {
		t.Fatalf("events = %d (%d..%d)", len(events), events[0].Cursor, events[len(events)-1].Cursor)
	}
}

func TestEventsPageBeforeSendsBackwardCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("before"); got != "931" {
			t.Errorf("before = %q, want 931", got)
		}
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("limit = %q, want 100", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema":       "agenthub.semantic-events.v1",
			"frames":       []map[string]any{{"schema": "agenthub.semantic-events.v1", "cursor": 831}, {"schema": "agenthub.semantic-events.v1", "cursor": 930}},
			"latestCursor": 1030,
			"page": map[string]any{
				"after": 0, "limit": 100, "nextAfter": 930, "hasMore": true,
				"before": 931, "nextBefore": 831, "hasMoreBefore": true,
			},
		})
	}))
	defer server.Close()
	page, err := New(server.URL).EventsPageBefore("ses_test", 931, 100)
	if err != nil {
		t.Fatal(err)
	}
	if page.Page.Before != 931 || page.Page.NextBefore != 831 || !page.Page.HasMoreBefore {
		t.Fatalf("backward page metadata = %+v", page.Page)
	}
	if page.Page.NextAfter != 930 || !page.Page.HasMore || page.LatestCursor != 1030 {
		t.Fatalf("forward metadata = %+v latest=%d", page.Page, page.LatestCursor)
	}
}

func TestEventsAfterStopsOnCursorGap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema":       "agenthub.semantic-events.v1",
			"frames":       []map[string]any{{"schema": "agenthub.semantic-events.v1", "cursor": 1}, {"schema": "agenthub.semantic-events.v1", "cursor": 3}},
			"latestCursor": 3,
			"page":         map[string]any{"after": 0, "limit": 1000, "nextAfter": 3, "hasMore": false},
		})
	}))
	defer server.Close()
	_, err := New(server.URL).EventsAfter("ses_test", 0)
	var gap *EventCursorGapError
	if !errors.As(err, &gap) || gap.Expected != 2 || gap.Got != 3 {
		t.Fatalf("gap error = %#v", err)
	}
}

func TestEventsPageRejectsMissingSemanticSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"events":[],"latestCursor":0}`))
	}))
	defer server.Close()
	if _, err := New(server.URL).EventsPage("ses_test", 0, 10); err == nil || !strings.Contains(err.Error(), "unsupported events schema") {
		t.Fatalf("error = %v", err)
	}
}

// TestResumeWithEnvironmentSendsOverlay pins the client request shape: an
// overlay goes out as launchEnvironment, while Resume (and a nil overlay)
// send the same empty object older clients always sent.
func TestResumeWithEnvironmentSendsOverlay(t *testing.T) {
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/sessions/ses_test/resume" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		bodies = append(bodies, string(body))
		_ = json.NewEncoder(w).Encode(map[string]any{"session": session.Session{ID: "ses_test"}})
	}))
	defer server.Close()
	client := New(server.URL)
	if _, err := client.ResumeWithEnvironment("ses_test", map[string]string{"SESSION_CONTEXT_ID": "context-new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resume("ses_test"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2", len(bodies))
	}
	if bodies[0] != `{"launchEnvironment":{"SESSION_CONTEXT_ID":"context-new"}}` {
		t.Fatalf("overlay body = %s", bodies[0])
	}
	if bodies[1] != `{}` {
		t.Fatalf("plain resume body = %s, want {}", bodies[1])
	}
}
