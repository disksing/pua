package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAgentHubClientContract(t *testing.T) {
	var methods []string
	var approvalReplies []agentHubApprovalReply
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/status":
			writeFakeAgentHubJSON(t, w, map[string]any{
				"apiVersion":   "1",
				"capabilities": requiredAgentHubCapabilities,
				"version":      "test",
				"future":       true,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/agents":
			writeFakeAgentHubJSON(t, w, map[string]any{
				"providers": []any{map[string]any{"id": "codex", "name": "Codex", "type": "codex", "enabled": true}},
				"agents":    []any{map[string]any{"name": "gpt-5.6-sol", "providerId": "codex", "available": true, "future": "ignored"}},
				"probes":    []any{},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
			var request agentHubCreateSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode create request: %v", err)
			}
			if request.AgentName != "gpt-5.6-sol" || request.Source == nil || request.Source.ExternalID != "gen-1" {
				t.Errorf("unexpected create request: %+v", request)
			}
			w.WriteHeader(http.StatusCreated)
			writeFakeAgentHubJSON(t, w, sessionEnvelope("ses_1", "ready"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			if r.URL.Query().Get("sourceApp") != "pua" || r.URL.Query().Get("sourceInstanceId") != "instance-1" {
				t.Errorf("unexpected session filter: %s", r.URL.RawQuery)
			}
			writeFakeAgentHubJSON(t, w, map[string]any{"sessions": []any{sessionData("ses_1", "ready")}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/ses_1":
			writeFakeAgentHubJSON(t, w, sessionEnvelope("ses_1", "ready"))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/sessions/ses_1/approvals/"):
			var reply agentHubApprovalReply
			if err := json.NewDecoder(r.Body).Decode(&reply); err != nil {
				t.Errorf("decode approval reply: %v", err)
			}
			approvalReplies = append(approvalReplies, reply)
			writeFakeAgentHubJSON(t, w, sessionEnvelope("ses_1", "ready"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions/ses_1/resume":
			var request agentHubResumeRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode resume request: %v", err)
			}
			if len(request.LaunchEnvironment) != 0 || len(request.EphemeralEnvironment) != 0 {
				t.Errorf("unexpected resume environment overlays: %+v", request)
			}
			writeFakeAgentHubJSON(t, w, sessionEnvelope("ses_1", "ready"))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/sessions/ses_1/"):
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("missing JSON content type for %s", r.URL.Path)
			}
			writeFakeAgentHubJSON(t, w, sessionEnvelope("ses_1", "ready"))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/sessions/ses_1":
			writeFakeAgentHubJSON(t, w, sessionEnvelope("ses_1", "archived"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := newAgentHubClient(server.URL+"/", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	status, err := client.Status(ctx)
	if err != nil || validateAgentHubStatus(status) != nil {
		t.Fatalf("status: %+v, %v", status, err)
	}
	catalog, err := client.Agents(ctx)
	if err != nil || len(catalog.Agents) != 1 || catalog.Agents[0].Name != "gpt-5.6-sol" {
		t.Fatalf("agents: %+v, %v", catalog, err)
	}
	created, err := client.CreateSession(ctx, agentHubCreateSessionRequest{
		Cwd:       t.TempDir(),
		AgentName: "gpt-5.6-sol",
		Source:    &agentHubSource{App: "pua", InstanceID: "instance-1", ExternalID: "gen-1"},
	})
	if err != nil || created.ID != "ses_1" {
		t.Fatalf("create: %+v, %v", created, err)
	}
	sessions, err := client.ListSessions(ctx, agentHubSessionFilter{SourceApp: "pua", SourceInstanceID: "instance-1"})
	if err != nil || len(sessions) != 1 {
		t.Fatalf("list: %+v, %v", sessions, err)
	}
	if _, err := client.GetSession(ctx, "ses_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Message(ctx, "ses_1", agentHubInboundMessage{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Message(ctx, "ses_1", agentHubInboundMessage{
		Text: "steer", Steer: true, Role: "user",
		Sender: &agentHubMessageSender{Name: "Browser User"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Approval(ctx, "ses_1", "approval/1", agentHubApprovalReply{Decision: "accept"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Approval(ctx, "ses_1", "approval/2", agentHubApprovalReply{OptionID: "option-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Approval(ctx, "ses_1", "approval/3", agentHubApprovalReply{Text: "another answer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Interrupt(ctx, "ses_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Stop(ctx, "ses_1"); err != nil {
		t.Fatal(err)
	}
	if resumed, err := client.ResumeWithEnvironment(ctx, "ses_1", nil, nil); err != nil || resumed.ID != "ses_1" || resumed.State != "ready" {
		t.Fatalf("resume: %+v, %v", resumed, err)
	}
	archived, err := client.Archive(ctx, "ses_1")
	if err != nil || archived.State != "archived" {
		t.Fatalf("archive: %+v, %v", archived, err)
	}
	if len(methods) != 14 {
		t.Fatalf("expected all client operations, got %d: %v", len(methods), methods)
	}
	wantReplies := []agentHubApprovalReply{
		{Decision: "accept"},
		{OptionID: "option-a"},
		{Text: "another answer"},
	}
	if fmt.Sprint(approvalReplies) != fmt.Sprint(wantReplies) {
		t.Fatalf("unexpected approval payloads: got %#v want %#v", approvalReplies, wantReplies)
	}
}

func TestAgentHubClientStructuredError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		writeFakeAgentHubJSON(t, w, map[string]any{"error": map[string]any{
			"code": "runtime_unavailable", "message": "try later", "retryable": true,
			"details": map[string]any{"sessionId": "ses_1"}, "requestId": "req_1",
		}})
	}))
	defer server.Close()
	client, err := newAgentHubClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Status(context.Background())
	var apiErr *agentHubAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected structured API error, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 503 || apiErr.Code != "runtime_unavailable" || !apiErr.Retryable || apiErr.RequestID != "req_1" || !strings.Contains(string(apiErr.Details), "ses_1") {
		t.Fatalf("unexpected API error: %+v", apiErr)
	}
}

func TestAgentHubClientTimeoutAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := newAgentHubClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = client.Status(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
}

func TestAgentHubStatusValidation(t *testing.T) {
	valid := agentHubStatus{APIVersion: "1", Capabilities: append([]string{"future.capability"}, requiredAgentHubCapabilities...)}
	if err := validateAgentHubStatus(valid); err != nil {
		t.Fatal(err)
	}
	valid.APIVersion = "2"
	if err := validateAgentHubStatus(valid); err == nil || !strings.Contains(err.Error(), "apiVersion") {
		t.Fatalf("expected version error, got %v", err)
	}
	valid.APIVersion = "1"
	valid.Capabilities = requiredAgentHubCapabilities[:len(requiredAgentHubCapabilities)-1]
	if err := validateAgentHubStatus(valid); err == nil || !strings.Contains(err.Error(), "recovery.closed-turns") {
		t.Fatalf("expected capability error, got %v", err)
	}
	valid.Capabilities = make([]string, 0, len(requiredAgentHubCapabilities)-1)
	for _, capability := range requiredAgentHubCapabilities {
		if capability != "events.semantic-v1" {
			valid.Capabilities = append(valid.Capabilities, capability)
		}
	}
	if err := validateAgentHubStatus(valid); err == nil || !strings.Contains(err.Error(), "events.semantic-v1") {
		t.Fatalf("expected semantic events capability error, got %v", err)
	}
}

func TestNormalizeAgentHubEndpoint(t *testing.T) {
	got, err := normalizeAgentHubEndpoint("")
	if err != nil || got != defaultAgentHubEndpoint {
		t.Fatalf("default endpoint: %q, %v", got, err)
	}
	got, err = normalizeAgentHubEndpoint("http://localhost:4646/")
	if err != nil || got != "http://localhost:4646" {
		t.Fatalf("normalized endpoint: %q, %v", got, err)
	}
	got, err = normalizeAgentHubEndpoint("http://localhost:4646/agenthub/")
	if err != nil || got != "http://localhost:4646/agenthub" {
		t.Fatalf("normalized base path endpoint: %q, %v", got, err)
	}
	for _, invalid := range []string{"localhost:4646", "ftp://localhost", "http://user@localhost", "http://localhost?a=b", "http://localhost/base"} {
		if _, err := normalizeAgentHubEndpoint(invalid); err == nil {
			t.Errorf("expected %q to be invalid", invalid)
		}
	}
}

func sessionData(id, state string) map[string]any {
	return map[string]any{
		"id": id, "title": "test", "cwd": "/tmp", "agentName": "gpt-5.6-sol",
		"state": state, "lastEventId": 0, "createdAt": "2026-01-01T00:00:00Z",
		"updatedAt": "2026-01-01T00:00:00Z", "future": "ignored",
	}
}

func sessionEnvelope(id, state string) map[string]any {
	return map[string]any{"session": sessionData(id, state)}
}

func writeFakeAgentHubJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode fake response: %v", err)
	}
}

func TestAgentHubClientResponseSizeLimit(t *testing.T) {
	limit := agentHubMaxResponseBytes
	agentHubMaxResponseBytes = 1 << 10
	defer func() { agentHubMaxResponseBytes = limit }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeFakeAgentHubJSON(t, w, map[string]any{
			"apiVersion":   "1",
			"capabilities": requiredAgentHubCapabilities,
			"version":      strings.Repeat("x", 2<<10),
		})
	}))
	defer server.Close()
	client, err := newAgentHubClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size limit error, got %v", err)
	}
	if strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("size limit must not surface as truncation error: %v", err)
	}
}

func TestAgentHubClientNetworkFailure(t *testing.T) {
	client, err := newAgentHubClient("http://127.0.0.1:1", &http.Client{Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Status(context.Background())
	if err == nil {
		t.Fatal("expected network failure")
	}
	if fmt.Sprint(err) == "" {
		t.Fatal("network failure should be actionable")
	}
}
