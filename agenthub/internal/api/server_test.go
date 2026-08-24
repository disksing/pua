package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disksing/pua/agenthub/internal/config"
	"github.com/disksing/pua/agenthub/internal/runtime"
	"github.com/disksing/pua/agenthub/internal/semantic"
	"github.com/disksing/pua/agenthub/internal/session"
)

func newGuardedTestServer(t *testing.T) (*httptest.Server, *ListenAddress) {
	t.Helper()
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	listen := resolveForTest(t, "192.168.1.10:4646", nil, testLANIPv4)
	server := httptest.NewServer(New(store, "test", time.Now(), Dependencies{Listen: listen}).Handler())
	t.Cleanup(server.Close)
	return server, listen
}

func TestHostGuardRejectsForeignHost(t *testing.T) {
	server, _ := newGuardedTestServer(t)
	for host, want := range map[string]int{
		"192.168.1.10:4646": http.StatusOK,
		"127.0.0.1:4646":    http.StatusOK,
		"localhost:4646":    http.StatusOK,
		"myhost:4646":       http.StatusOK,
		"evil.example":      http.StatusForbidden,
		"192.168.1.11:4646": http.StatusForbidden,
	} {
		request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/health", nil)
		request.Host = host
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("Host %q: status = %d, want %d", host, response.StatusCode, want)
		}
	}
}

func TestMutationAcceptsSameOriginLANBrowser(t *testing.T) {
	server, _ := newGuardedTestServer(t)
	body, _ := json.Marshal(map[string]any{"title": "LAN", "cwd": t.TempDir(), "agentName": "Agent"})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/sessions", bytes.NewReader(body))
	request.Host = "192.168.1.10:4646"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://192.168.1.10:4646")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("unexpected status: %s", response.Status)
	}
}

func TestMutationRejectsCrossOriginOnLANListener(t *testing.T) {
	server, _ := newGuardedTestServer(t)
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/sessions", strings.NewReader(`{}`))
	request.Host = "192.168.1.10:4646"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://evil.example")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("unexpected status: %s", response.Status)
	}
}

func TestMutationAcceptsAllowedProxyOrigin(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(store, "test", time.Now(), Dependencies{
		AllowedOrigins: []string{"https://agenthub.example:18443"},
	}).Handler())
	defer server.Close()

	body, _ := json.Marshal(map[string]any{"title": "proxied", "cwd": t.TempDir(), "agentName": "Agent"})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/sessions", bytes.NewReader(body))
	// A reverse proxy terminates TLS and typically rewrites Host to the
	// upstream address, so the daemon sees its own host while the browser
	// Origin carries the public https origin.
	request.Host = "127.0.0.1:4646"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://agenthub.example:18443")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("unexpected status: %s", response.Status)
	}
}

func TestMutationRejectsUnlistedProxyOrigin(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(store, "test", time.Now(), Dependencies{
		AllowedOrigins: []string{"https://agenthub.example:18443"},
	}).Handler())
	defer server.Close()

	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/sessions", strings.NewReader(`{}`))
	request.Host = "127.0.0.1:4646"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://other.example:18443")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("unexpected status: %s", response.Status)
	}
}

func TestNormalizeOrigin(t *testing.T) {
	valid := map[string]string{
		"https://agenthub.example:18443":   "https://agenthub.example:18443",
		"http://192.168.2.150:4646":        "http://192.168.2.150:4646",
		"HTTPS://AgentHub.Example:18443":   "https://agenthub.example:18443",
		" https://agenthub.example:18443 ": "https://agenthub.example:18443",
	}
	for input, want := range valid {
		got, err := NormalizeOrigin(input)
		if err != nil {
			t.Fatalf("NormalizeOrigin(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("NormalizeOrigin(%q) = %q, want %q", input, got, want)
		}
	}
	invalid := []string{
		"",
		"agenthub.example:18443",
		"ftp://agenthub.example",
		"https://agenthub.example/path",
		"https://agenthub.example?x=1",
		"https://user@agenthub.example",
		"https://agenthub.example#frag",
	}
	for _, input := range invalid {
		if got, err := NormalizeOrigin(input); err == nil {
			t.Fatalf("NormalizeOrigin(%q) = %q, want error", input, got)
		}
	}
}

func TestSessionAPIUsesEventLog(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(store, "test", time.Now()).Handler())
	defer server.Close()

	body, _ := json.Marshal(map[string]any{
		"title":             "API session",
		"cwd":               t.TempDir(),
		"agentName":         "Agent",
		"launchEnvironment": map[string]string{"SESSION_CONTEXT_ID": "context-api"},
	})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/sessions", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("unexpected status: %s", response.Status)
	}
	var created struct {
		Session session.Session `json:"session"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Session.State != session.StateReady || created.Session.LastEventID != 1 {
		t.Fatalf("unexpected session: %+v", created.Session)
	}
	if created.Session.LaunchEnvironment["SESSION_CONTEXT_ID"] != "context-api" {
		t.Fatalf("launch environment was not persisted: %+v", created.Session)
	}

	response, err = http.Get(server.URL + "/v1/sessions/" + created.Session.ID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var history struct {
		Frames []semantic.Frame `json:"frames"`
	}
	if err := json.NewDecoder(response.Body).Decode(&history); err != nil {
		t.Fatal(err)
	}
	if len(history.Frames) != 1 || len(history.Frames[0].Events) != 1 || history.Frames[0].Events[0].Type != "session.created" {
		t.Fatalf("unexpected history: %+v", history.Frames)
	}
	historyData, _ := json.Marshal(history.Frames[0].Events[0].Data)
	if !bytes.Contains(historyData, []byte(`"launchEnvironment":{"SESSION_CONTEXT_ID":"context-api"}`)) {
		t.Fatalf("session.created did not preserve launchEnvironment: %s", historyData)
	}
}

func TestSemanticEventsHideRawAndSingleEventReturnsExactDiagnostic(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(session.CreateInput{Title: "Raw diagnostic", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	tool, err := store.Append(created.ID, "tool.event", "turn_1", mustMarshal(t, map[string]any{
		"method": "item/started",
		"raw": map[string]any{"secret": "provider-private", "item": map[string]any{
			"id": "call_1", "type": "commandExecution", "command": []string{"true"}, "status": "inProgress",
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(store, "test", time.Now()).Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/v1/sessions/" + created.ID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	publicBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || bytes.Contains(publicBody, []byte("provider-private")) || bytes.Contains(publicBody, []byte(`"raw"`)) {
		t.Fatalf("public events leaked raw: status=%d body=%s", response.StatusCode, publicBody)
	}
	var public struct {
		Schema string           `json:"schema"`
		Frames []semantic.Frame `json:"frames"`
	}
	if err := json.Unmarshal(publicBody, &public); err != nil {
		t.Fatal(err)
	}
	if public.Schema != semantic.EventsSchema || len(public.Frames) != 2 || len(public.Frames[1].Events) != 1 || public.Frames[1].Events[0].Type != "tool.call" {
		t.Fatalf("public events = %+v", public)
	}

	response, err = http.Get(server.URL + "/v1/sessions/" + created.ID + "/event/" + strconv.FormatInt(tool.ID, 10))
	if err != nil {
		t.Fatal(err)
	}
	var detail semantic.Detail
	if err := json.NewDecoder(response.Body).Decode(&detail); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || detail.Schema != semantic.EventDetailSchema || detail.SourceEvent.ID != tool.ID || !bytes.Contains(detail.SourceEvent.Data, []byte("provider-private")) {
		t.Fatalf("detail = status=%d %+v", response.StatusCode, detail)
	}
	frameData, _ := json.Marshal(detail.Frame)
	if bytes.Contains(frameData, []byte("provider-private")) || bytes.Contains(frameData, []byte(`"raw"`)) {
		t.Fatalf("detail frame leaked raw: %s", frameData)
	}

	for path, wantStatus := range map[string]int{
		"/event/0":         http.StatusBadRequest,
		"/event/not-id":    http.StatusBadRequest,
		"/event/999":       http.StatusNotFound,
		"/event/1?raw=yes": http.StatusBadRequest,
	} {
		response, err := http.Get(server.URL + "/v1/sessions/" + created.ID + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != wantStatus {
			t.Errorf("%s status=%d, want %d", path, response.StatusCode, wantStatus)
		}
	}
}

func TestCreateSessionRejectsInvalidLaunchEnvironment(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(store, "test", time.Now()).Handler())
	defer server.Close()
	body, _ := json.Marshal(map[string]any{
		"cwd":               t.TempDir(),
		"agentName":         "Agent",
		"launchEnvironment": map[string]string{"BAD=NAME": "value"},
	})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/sessions", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var parsed struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnprocessableEntity || parsed.Error.Code != "invalid_launch_environment" {
		t.Fatalf("status = %d, code = %q", response.StatusCode, parsed.Error.Code)
	}
}

func TestEphemeralEnvironmentValidationDoesNotLeakInput(t *testing.T) {
	root := t.TempDir()
	store, err := session.Open(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Version:        1,
		AgentProviders: []config.Provider{{ID: "provider", Type: "pi", Enabled: true, Command: "missing-test-command"}},
		Agents:         []config.Agent{{Name: "Pi Agent", ProviderID: "provider"}},
	}
	manager := runtime.New(store, cfg)
	t.Cleanup(manager.Close)
	server := httptest.NewServer(New(store, "test", time.Now(), Dependencies{
		Runtime:              manager,
		EphemeralEnvironment: true,
	}).Handler())
	defer server.Close()

	created, err := store.Create(session.CreateInput{Cwd: t.TempDir(), AgentName: "Pi Agent", Provider: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	baselineEvents, err := store.EventsAfter(created.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		path        string
		invalidName string
		value       string
		valueMarker string
	}{
		{
			name:        "create invalid name",
			path:        "/v1/sessions",
			invalidName: "EPHEMERAL_CREATE_NAME=PRIVATE",
			value:       "ephemeral-create-name-value",
			valueMarker: "ephemeral-create-name-value",
		},
		{
			name:        "create invalid value",
			path:        "/v1/sessions",
			invalidName: "EPHEMERAL_CREATE_VALUE_PRIVATE",
			value:       "ephemeral-create-value\x00private",
			valueMarker: "ephemeral-create-value",
		},
		{
			name:        "resume invalid name",
			path:        "/v1/sessions/" + created.ID + "/resume",
			invalidName: "EPHEMERAL_RESUME_NAME=PRIVATE",
			value:       "ephemeral-resume-name-value",
			valueMarker: "ephemeral-resume-name-value",
		},
		{
			name:        "resume invalid value",
			path:        "/v1/sessions/" + created.ID + "/resume",
			invalidName: "EPHEMERAL_RESUME_VALUE_PRIVATE",
			value:       "ephemeral-resume-value\x00private",
			valueMarker: "ephemeral-resume-value",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := map[string]any{
				"ephemeralEnvironment": map[string]string{test.invalidName: test.value},
			}
			if test.path == "/v1/sessions" {
				body["cwd"] = t.TempDir()
				body["agentName"] = "Pi Agent"
			}
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequest(http.MethodPost, server.URL+test.path, bytes.NewReader(encoded))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			responseBody, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d: %s", response.StatusCode, http.StatusUnprocessableEntity, responseBody)
			}
			var parsed struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(responseBody, &parsed); err != nil {
				t.Fatal(err)
			}
			if parsed.Error.Code != "invalid_ephemeral_environment" {
				t.Fatalf("code = %q, want invalid_ephemeral_environment", parsed.Error.Code)
			}
			if parsed.Error.Message != "ephemeralEnvironment contains an invalid variable name or value" {
				t.Fatalf("message = %q", parsed.Error.Message)
			}
			for _, secret := range []string{test.invalidName, test.valueMarker} {
				if bytes.Contains(responseBody, []byte(secret)) {
					t.Fatalf("response leaked ephemeral input %q: %s", secret, responseBody)
				}
			}
		})
	}

	events, err := store.EventsAfter(created.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != len(baselineEvents) {
		t.Fatalf("rejected resumes appended events: got %d, want %d", len(events), len(baselineEvents))
	}
	if err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, test := range tests {
			for _, secret := range []string{test.invalidName, test.valueMarker} {
				if bytes.Contains(contents, []byte(secret)) {
					t.Fatalf("%s persisted ephemeral input %q", path, secret)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSessionSourceAPIAndCombinedFilters(t *testing.T) {
	root := t.TempDir()
	store, err := session.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(store, "test", time.Now()).Handler())

	create := func(title string, sourceValue any) session.Session {
		t.Helper()
		body := map[string]any{
			"title": title, "cwd": t.TempDir(), "agentName": "Agent",
		}
		if sourceValue != nil {
			body["source"] = sourceValue
		}
		encoded, _ := json.Marshal(body)
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/sessions", bytes.NewReader(encoded))
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("create %q status = %s", title, response.Status)
		}
		var result struct {
			Session session.Session `json:"session"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result.Session
	}
	sourceValue := map[string]any{"app": "pua", "instanceId": "mac-1", "externalId": "task-1"}
	puaOne := create("pua one", sourceValue)
	puaDuplicate := create("pua duplicate", sourceValue)
	puaTwo := create("pua two", map[string]any{"app": "pua", "instanceId": "mac-2", "externalId": "task-2"})
	other := create("other", map[string]any{"app": "other", "instanceId": "mac-1", "externalId": "task-1"})
	legacy := create("legacy", nil)

	if puaOne.Source == nil || puaOne.Source.App != "pua" {
		t.Fatalf("create response source = %+v", puaOne.Source)
	}
	fetched := getSession(t, server, puaOne.ID)
	if fetched.Source == nil || !reflect.DeepEqual(*fetched.Source, session.Source{App: "pua", InstanceID: "mac-1", ExternalID: "task-1"}) {
		t.Fatalf("GET response source = %+v", fetched.Source)
	}
	stateData, err := json.Marshal(session.StateEventData{State: session.StateStopped})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(puaTwo.ID, "session.state", "", stateData); err != nil {
		t.Fatal(err)
	}
	stoppedDuplicate, err := json.Marshal(session.StateEventData{
		State: session.StateStopped, Reason: session.StopReasonRequested,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(puaDuplicate.ID, "session.state", "", stoppedDuplicate); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Archive(puaDuplicate.ID); err != nil {
		t.Fatal(err)
	}

	assertList := func(query string, want ...string) {
		t.Helper()
		response, err := http.Get(server.URL + "/v1/sessions?" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var result struct {
			Sessions []session.Session `json:"sessions"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		got := make(map[string]bool, len(result.Sessions))
		for _, value := range result.Sessions {
			got[value.ID] = true
			if value.Source == nil {
				t.Fatalf("%s: listed session %s omitted source", query, value.ID)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("%s: ids = %v, want %v", query, got, want)
		}
		for _, id := range want {
			if !got[id] {
				t.Fatalf("%s: missing %s in %v", query, id, got)
			}
		}
	}
	assertList("sourceApp=pua", puaOne.ID, puaTwo.ID)
	assertList("sourceInstanceId=mac-1", puaOne.ID, other.ID)
	assertList("sourceExternalId=task-2", puaTwo.ID)
	assertList("sourceApp=pua&sourceInstanceId=mac-1&sourceExternalId=task-1", puaOne.ID)
	assertList("includeArchived=true&sourceApp=pua&sourceInstanceId=mac-1&sourceExternalId=task-1", puaOne.ID, puaDuplicate.ID)
	assertList("archived=true&sourceApp=pua&sourceInstanceId=mac-1", puaDuplicate.ID)
	assertList("sourceApp=pua&sourceExternalId=task-2&state=stopped", puaTwo.ID)

	for _, id := range []string{legacy.ID, other.ID} {
		if list := listIDs(t, server, "?sourceApp=pua"); list[id] != "" {
			t.Fatalf("unmatched session %s appeared in source filter", id)
		}
	}

	server.Close()
	reopened, err := session.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	value, err := reopened.Get(puaOne.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.Source == nil || !reflect.DeepEqual(*value.Source, session.Source{App: "pua", InstanceID: "mac-1", ExternalID: "task-1"}) {
		t.Fatalf("source after daemon-style reopen = %+v", value.Source)
	}
}

func TestSessionListCursorPagination(t *testing.T) {
	now := time.Date(2026, 8, 23, 1, 2, 3, 0, time.UTC)
	values := []session.Session{
		{ID: "ses_d", UpdatedAt: now.Add(2 * time.Minute)},
		{ID: "ses_c", UpdatedAt: now.Add(time.Minute)},
		{ID: "ses_b", UpdatedAt: now},
		{ID: "ses_a", UpdatedAt: now},
	}
	first, queryErr := paginateSessions(values, url.Values{"limit": {"2"}})
	if queryErr != nil {
		t.Fatal(queryErr)
	}
	if got := []string{first.sessions[0].ID, first.sessions[1].ID}; !reflect.DeepEqual(got, []string{"ses_d", "ses_c"}) {
		t.Fatalf("first page ids = %v", got)
	}
	if !first.metadata.HasMore || first.metadata.NextCursor == "" || first.metadata.Limit != 2 {
		t.Fatalf("first page metadata = %+v", first.metadata)
	}
	second, queryErr := paginateSessions(values, url.Values{
		"limit": {"2"}, "cursor": {first.metadata.NextCursor},
	})
	if queryErr != nil {
		t.Fatal(queryErr)
	}
	if got := []string{second.sessions[0].ID, second.sessions[1].ID}; !reflect.DeepEqual(got, []string{"ses_b", "ses_a"}) {
		t.Fatalf("second page ids = %v", got)
	}
	if second.metadata.HasMore || second.metadata.NextCursor != "" {
		t.Fatalf("second page metadata = %+v", second.metadata)
	}
}

func TestSessionListPaginationValidation(t *testing.T) {
	for _, test := range []struct {
		query url.Values
		code  string
	}{
		{query: url.Values{"limit": {"0"}}, code: "invalid_session_limit"},
		{query: url.Values{"limit": {"501"}}, code: "invalid_session_limit"},
		{query: url.Values{"limit": {"many"}}, code: "invalid_session_limit"},
		{query: url.Values{"cursor": {"not-a-cursor"}}, code: "invalid_session_cursor"},
	} {
		_, queryErr := paginateSessions(nil, test.query)
		if queryErr == nil || queryErr.code != test.code {
			t.Errorf("query %v error = %+v, want %s", test.query, queryErr, test.code)
		}
	}
}

func TestMutationRejectsCrossOriginBrowser(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler := New(store, "test", time.Now()).Handler()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4646/v1/sessions", strings.NewReader(`{}`))
	request.Host = "127.0.0.1:4646"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://evil.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("unexpected status: %d", response.Code)
	}
}

func newConfigTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	store, err := session.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Version:        1,
		AgentProviders: []config.Provider{{ID: "provider", Type: "pi", Enabled: true, Command: "missing-test-command"}},
		Agents:         []config.Agent{{Name: "Pi Agent", ProviderID: "provider"}},
	}
	manager := runtime.New(store, cfg)
	server := httptest.NewServer(New(store, "test", time.Now(), Dependencies{Runtime: manager, ConfigPath: filepath.Join(root, "config.json")}).Handler())
	t.Cleanup(server.Close)
	return server
}

func TestAgentsAndConfigAPI(t *testing.T) {
	server := newConfigTestServer(t)
	response, err := http.Get(server.URL + "/v1/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %s", response.Status)
	}
	var body struct {
		Agents []config.Agent `json:"agents"`
		Probes []config.Probe `json:"probes"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Agents) != 1 || len(body.Probes) != 1 || body.Probes[0].Available {
		t.Fatalf("unexpected response: %+v", body)
	}
}

func TestCreateSessionRequiresExplicitAgent(t *testing.T) {
	server := newConfigTestServer(t)
	post := func(body string) (int, string) {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/sessions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var parsed struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.NewDecoder(response.Body).Decode(&parsed); err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, parsed.Error.Code
	}
	cwd := t.TempDir()
	cases := []struct {
		name string
		body string
		want int
		code string
	}{
		{"missing agentName", `{"cwd":"` + cwd + `"}`, http.StatusUnprocessableEntity, "agent_required"},
		{"blank agentName", `{"cwd":"` + cwd + `","agentName":"  "}`, http.StatusUnprocessableEntity, "agent_required"},
		{"unknown agent", `{"cwd":"` + cwd + `","agentName":"ghost"}`, http.StatusUnprocessableEntity, "invalid_agent"},
		// A valid name passes validation (case-insensitively) and only
		// fails later because the test provider cannot start.
		{"agent name matches case-insensitively", `{"cwd":"` + cwd + `","agentName":"pi agent"}`, http.StatusBadGateway, "provider_start_failed"},
		{"removed agentId field", `{"cwd":"` + cwd + `","agentId":"agent"}`, http.StatusBadRequest, "invalid_request"},
		{"removed selector field", `{"cwd":"` + cwd + `","agentName":"Pi Agent","selector":{"tags":["fast"]}}`, http.StatusBadRequest, "invalid_request"},
	}
	for _, item := range cases {
		status, code := post(item.body)
		if status != item.want || code != item.code {
			t.Errorf("%s: status = %d, code = %q, want %d %s", item.name, status, code, item.want, item.code)
		}
	}
}

func putConfig(t *testing.T, server *httptest.Server, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPut, server.URL+"/v1/config", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var parsed struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, parsed.Error.Code
}

func TestPutConfigRoundTrip(t *testing.T) {
	server := newConfigTestServer(t)
	updated := `{"config":{
		"version": 1,
		"agentProviders": [
			{"id": "provider", "name": "Pi", "type": "pi", "enabled": true, "command": "missing-test-command"},
			{"id": "second", "name": "Kimi", "type": "kimi", "enabled": false}
		],
		"agents": [
			{"name": "Pi Agent", "providerId": "provider"},
			{"name": "Pi Agent B", "providerId": "provider", "options": {"model": "m"}}
		]
	}}`
	status, code := putConfig(t, server, updated)
	if status != http.StatusOK || code != "" {
		t.Fatalf("PUT /v1/config: status = %d, code = %q", status, code)
	}

	response, err := http.Get(server.URL + "/v1/config")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var configBody struct {
		Config config.Config `json:"config"`
	}
	if err := json.NewDecoder(response.Body).Decode(&configBody); err != nil {
		t.Fatal(err)
	}
	cfg := configBody.Config
	if len(cfg.AgentProviders) != 2 || len(cfg.Agents) != 2 {
		t.Fatalf("unexpected config after save: %+v", cfg)
	}
	if cfg.Agents[1].Options["model"] != "m" {
		t.Fatalf("saved fields lost: %+v", cfg)
	}

	response, err = http.Get(server.URL + "/v1/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var agentsBody struct {
		Agents []config.Agent `json:"agents"`
		Probes []config.Probe `json:"probes"`
	}
	if err := json.NewDecoder(response.Body).Decode(&agentsBody); err != nil {
		t.Fatal(err)
	}
	if len(agentsBody.Agents) != 2 {
		t.Fatalf("GET /v1/agents does not reflect saved config: %+v", agentsBody)
	}
	// Only enabled providers are probed; the disabled second one is absent.
	if len(agentsBody.Probes) != 1 || agentsBody.Probes[0].ProviderID != "provider" {
		t.Fatalf("unexpected probes after save: %+v", agentsBody.Probes)
	}
}

// Removed profile-routing fields are rejected by the strict config decoder,
// so new writes can never reintroduce them through the API.
func TestPutConfigRejectsRemovedProfileFields(t *testing.T) {
	server := newConfigTestServer(t)
	cases := map[string]string{
		"agentProfiles": `{"config":{"agentProviders":[{"id":"p","type":"pi","enabled":true}],
			"agentProfiles":[{"key":"fast","agentId":"a"}]}}`,
		"defaultChatAgentId": `{"config":{"agentProviders":[{"id":"p","type":"pi","enabled":true}],
			"defaultChatAgentId":"a"}}`,
	}
	for name, body := range cases {
		status, code := putConfig(t, server, body)
		if status != http.StatusBadRequest || code != "invalid_request" {
			t.Errorf("%s: status = %d, code = %q, want 400 invalid_request", name, status, code)
		}
	}
}

func TestPutConfigRejectsInvalidConfig(t *testing.T) {
	server := newConfigTestServer(t)
	cases := map[string]string{
		"duplicate provider id": `{"config":{"agentProviders":[
			{"id":"p","type":"pi","enabled":true},
			{"id":"p","type":"kimi","enabled":true}]}}`,
		"unsupported provider type": `{"config":{"agentProviders":[
			{"id":"p","type":"bogus","enabled":true}]}}`,
		"dangling agent provider": `{"config":{"agentProviders":[
			{"id":"p","type":"pi","enabled":true}],
			"agents":[{"name":"A","providerId":"ghost"}]}}`,
		"duplicate agent names": `{"config":{"agentProviders":[
			{"id":"p","type":"pi","enabled":true}],
			"agents":[{"name":"Codex","providerId":"p"},{"name":" codex ","providerId":"p"}]}}`,
		"blank agent name": `{"config":{"agentProviders":[
			{"id":"p","type":"pi","enabled":true}],
			"agents":[{"name":"  ","providerId":"p"}]}}`,
	}
	for name, body := range cases {
		status, code := putConfig(t, server, body)
		if status != http.StatusUnprocessableEntity || code != "invalid_config" {
			t.Errorf("%s: status = %d, code = %q, want 422 invalid_config", name, status, code)
		}
	}
	// A failed save must not change the existing config.
	response, err := http.Get(server.URL + "/v1/config")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var configBody struct {
		Config config.Config `json:"config"`
	}
	if err := json.NewDecoder(response.Body).Decode(&configBody); err != nil {
		t.Fatal(err)
	}
	if len(configBody.Config.Agents) != 1 {
		t.Fatalf("config changed after rejected saves: %+v", configBody.Config)
	}
}

func TestPutConfigRejectsInvalidRequest(t *testing.T) {
	server := newConfigTestServer(t)
	cases := map[string]string{
		"malformed JSON":               `{"config":`,
		"wrong config type":            `{"config":"nope"}`,
		"unknown field without config": `{"conf":{}}`,
		// The removed agent id must never be written back through the API.
		"legacy agent id field": `{"config":{"agentProviders":[{"id":"p","type":"pi","enabled":true}],
			"agents":[{"id":"a","name":"A","providerId":"p"}]}}`,
	}
	for name, body := range cases {
		status, code := putConfig(t, server, body)
		if status != http.StatusBadRequest || code != "invalid_request" {
			t.Errorf("%s: status = %d, code = %q, want 400 invalid_request", name, status, code)
		}
	}
}

func TestSSEReplaysFromCursor(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(session.CreateInput{Title: "SSE", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, "session.state", "", mustMarshal(t, session.StateEventData{State: session.StateStopped})); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(store, "test", time.Now()).Handler())
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/sessions/"+created.ID+"/events", nil)
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Last-Event-ID", "1")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	// The first frame re-sends the cursor event with its current durable
	// content so clients heal tail events that merged while disconnected.
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "id: 1\n" {
		t.Fatalf("SSE replay must re-send the cursor event first: %q", line)
	}
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\n" {
			break
		}
	}
	line, err = reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "id: 2\n" {
		t.Fatalf("unexpected SSE cursor replay: %q", line)
	}
	cancel()
}

func TestRESTEventPaginationReportsDurableHead(t *testing.T) {
	store, created := seedEventStore(t, 1001)
	server := httptest.NewServer(New(store, "test", time.Now()).Handler())
	defer server.Close()

	var first struct {
		Frames []semantic.Frame `json:"frames"`
		Page   struct {
			After     int64 `json:"after"`
			Limit     int   `json:"limit"`
			NextAfter int64 `json:"nextAfter"`
			HasMore   bool  `json:"hasMore"`
		} `json:"page"`
		LatestCursor int64 `json:"latestCursor"`
	}
	response, err := http.Get(server.URL + "/v1/sessions/" + created.ID + "/events?after=0&limit=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(&first); err != nil {
		t.Fatal(err)
	}
	if len(first.Frames) != 1000 || first.Page.After != 0 || first.Page.Limit != 1000 ||
		first.Page.NextAfter != 1000 || !first.Page.HasMore || first.LatestCursor != 1001 {
		t.Fatalf("unexpected first page: frames=%d page=%+v latest=%d", len(first.Frames), first.Page, first.LatestCursor)
	}

	var second struct {
		Frames       []semantic.Frame `json:"frames"`
		LatestCursor int64            `json:"latestCursor"`
		Page         struct {
			NextAfter int64 `json:"nextAfter"`
			HasMore   bool  `json:"hasMore"`
		} `json:"page"`
	}
	response, err = http.Get(server.URL + "/v1/sessions/" + created.ID + "/events?after=1000&limit=1000")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(&second); err != nil {
		t.Fatal(err)
	}
	if len(second.Frames) != 1 || second.Frames[0].Cursor != 1001 ||
		second.Page.NextAfter != 1001 || second.Page.HasMore || second.LatestCursor != 1001 {
		t.Fatalf("unexpected second page: %+v", second)
	}
}

func TestEventCursorValidation(t *testing.T) {
	store, created := seedEventStore(t, 1)
	server := httptest.NewServer(New(store, "test", time.Now()).Handler())
	defer server.Close()
	for _, test := range []struct {
		query  string
		status int
		code   string
	}{
		{"after=invalid", http.StatusBadRequest, "invalid_event_cursor"},
		{"after=-1", http.StatusBadRequest, "invalid_event_cursor"},
		{"after=2", http.StatusConflict, "event_cursor_ahead"},
	} {
		response, err := http.Get(server.URL + "/v1/sessions/" + created.ID + "/events?" + test.query)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			response.Body.Close()
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != test.status || body.Error.Code != test.code {
			t.Errorf("%s: status=%d code=%q", test.query, response.StatusCode, body.Error.Code)
		}
	}
}

func TestSSEReplaysEntireBacklog(t *testing.T) {
	for _, total := range []int{1, 1000, 1001} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			store, created := seedEventStore(t, total)
			writer := newSSERecorder(total, 0)
			ctx, cancel := context.WithCancel(context.Background())
			request := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+created.ID+"/events?stream=true", nil).WithContext(ctx)
			done := make(chan struct{})
			go func() {
				New(store, "test", time.Now()).events(writer, request, created.ID)
				close(done)
			}()
			waitClosed(t, writer.reached, "SSE backlog")
			cancel()
			waitClosed(t, done, "SSE handler")
			assertContiguousIDs(t, writer.IDs(), 1, total)
		})
	}
}

func TestSSECatchesEventsAppendedDuringBacklogReplay(t *testing.T) {
	store, created := seedEventStore(t, 1100)
	writer := newSSERecorder(1101, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+created.ID+"/events?stream=true", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		New(store, "test", time.Now()).events(writer, request, created.ID)
		close(done)
	}()
	waitClosed(t, writer.blocked, "blocked backlog replay")
	if _, err := store.Append(created.ID, "provider.concurrent", "", mustMarshal(t, map[string]int{"id": 1101})); err != nil {
		t.Fatal(err)
	}
	close(writer.release)
	waitClosed(t, writer.reached, "SSE live catch-up")
	cancel()
	waitClosed(t, done, "SSE handler")
	assertContiguousIDs(t, writer.IDs(), 1, 1101)
}

func TestSSEStopsImmediatelyAfterSubscriberOverflow(t *testing.T) {
	store, created := seedEventStore(t, 1)
	writer := newSSERecorder(0, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+created.ID+"/events?stream=true", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		New(store, "test", time.Now()).events(writer, request, created.ID)
		close(done)
	}()
	if _, err := store.Append(created.ID, "provider.live", "", mustMarshal(t, map[string]int{"id": 2})); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, writer.blocked, "blocked live write")
	for id := 3; id <= 3+256; id++ {
		if _, err := store.Append(created.ID, "provider.overflow", "", mustMarshal(t, map[string]int{"id": id})); err != nil {
			t.Fatal(err)
		}
	}
	close(writer.release)
	waitClosed(t, done, "overflowed SSE handler")
	ids := writer.IDs()
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("overflowed connection continued sending buffered events: %v", ids)
	}

	// Reconnect from the last contiguous id. The missing tail comes from the
	// durable log, not the discarded in-memory subscriber queue. The replay
	// starts by re-sending the cursor event (id 2) so tail merges that
	// happened while disconnected reach the client.
	resume := newSSERecorder(258, 0)
	resumeCtx, resumeCancel := context.WithCancel(context.Background())
	resumeRequest := httptest.NewRequest(
		http.MethodGet,
		"/v1/sessions/"+created.ID+"/events?stream=true&after=2",
		nil,
	).WithContext(resumeCtx)
	resumeDone := make(chan struct{})
	go func() {
		New(store, "test", time.Now()).events(resume, resumeRequest, created.ID)
		close(resumeDone)
	}()
	waitClosed(t, resume.reached, "overflow reconnect replay")
	resumeCancel()
	waitClosed(t, resumeDone, "overflow reconnect handler")
	resumeIDs := resume.IDs()
	if len(resumeIDs) != 258 || resumeIDs[0] != 2 {
		t.Fatalf("reconnect replay must start with the cursor event: %v", resumeIDs[:min(4, len(resumeIDs))])
	}
	assertContiguousIDs(t, resumeIDs[1:], 3, 259)
}

func TestEventsRecoverAfterStoreRestart(t *testing.T) {
	root := t.TempDir()
	store, err := session.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(session.CreateInput{Title: "Restart", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for id := 2; id <= 25; id++ {
		if _, err := store.Append(created.ID, "provider.before_restart", "", mustMarshal(t, map[string]int{"id": id})); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := session.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	page, err := reopened.EventsPage(created.ID, 10, 5)
	if err != nil {
		t.Fatal(err)
	}
	if page.LatestCursor != 25 || page.NextAfter != 15 || !page.HasMore {
		t.Fatalf("restart REST page = %+v", page)
	}
	writer := newSSERecorder(16, 0)
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+created.ID+"/events?stream=true&after=10", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		New(reopened, "test", time.Now()).events(writer, request, created.ID)
		close(done)
	}()
	waitClosed(t, writer.reached, "restart SSE replay")
	cancel()
	waitClosed(t, done, "restart SSE handler")
	if ids := writer.IDs(); len(ids) != 16 || ids[0] != 10 {
		t.Fatalf("restart replay must start with the cursor event: %v", ids)
	} else {
		assertContiguousIDs(t, ids[1:], 11, 25)
	}
}

// Every event — including types a consumer has never heard of — must arrive
// on the default SSE message channel, so no event is silently dropped just
// because the client did not subscribe to its type name.
func TestSSEStreamsUnknownEventTypes(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(session.CreateInput{Title: "SSE", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, "provider.some.future.event", "", mustMarshal(t, map[string]any{"novel": true})); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(store, "test", time.Now()).Handler())
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/sessions/"+created.ID+"/events?stream=true", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	reader := bufio.NewReader(response.Body)
	var frames []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\n" {
			if len(frames) > 0 {
				break
			}
			continue
		}
		frames = append(frames, strings.TrimSuffix(line, "\n"))
		if strings.HasPrefix(frames[len(frames)-1], "data: ") {
			// The data line is the last line of a frame.
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if len(frames) != 2 || frames[0] != "id: 1" || !strings.HasPrefix(frames[1], "data: ") {
		t.Fatalf("unexpected SSE frame: %q", frames)
	}
	var frame semantic.Frame
	if err := json.Unmarshal([]byte(strings.TrimPrefix(frames[1], "data: ")), &frame); err != nil {
		t.Fatal(err)
	}
	if len(frame.Events) != 1 || frame.Events[0].Type != "session.created" {
		t.Fatalf("first replayed frame = %+v, want session.created", frame)
	}

	// The custom event must also be framed without an `event:` name field.
	frames = frames[:0]
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\n" {
			break
		}
		frames = append(frames, strings.TrimSuffix(line, "\n"))
	}
	if len(frames) != 2 || frames[0] != "id: 2" || !strings.HasPrefix(frames[1], "data: ") {
		t.Fatalf("unknown event frame must use the default message channel: %q", frames)
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(frames[1], "data: ")), &frame); err != nil {
		t.Fatal(err)
	}
	if len(frame.Events) != 1 || frame.Events[0].Type != "unknown" {
		t.Fatalf("second frame = %+v, want sanitized unknown", frame)
	}
	cancel()
}

// Consecutive text deltas merge into one durable event; the live stream
// forwards small append patches under the id the client already has, and
// reconnecting at that id first re-sends the event's full accumulated
// content so fragments missed while disconnected are healed.
func TestSSEForwardsDeltaMergePatches(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(session.CreateInput{Title: "SSE", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(store, "test", time.Now()).Handler())
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/sessions/"+created.ID+"/events?stream=true", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)

	if id, _ := readSSEFrame(t, reader); id != 1 {
		t.Fatalf("replayed frame id = %d, want 1", id)
	}
	if _, err := store.Append(created.ID, "turn.started", "turn_1", nil); err != nil {
		t.Fatal(err)
	}
	if id, _ := readSSEFrame(t, reader); id != 2 {
		t.Fatalf("turn.started frame id = %d, want 2", id)
	}
	appendDelta := func(text string) {
		t.Helper()
		if _, err := store.Append(created.ID, "message.assistant.delta", "turn_1",
			mustMarshal(t, map[string]any{"text": text, "method": "item/agentMessage/delta"})); err != nil {
			t.Fatal(err)
		}
	}
	appendDelta("Hello")
	id, frame := readSSEFrame(t, reader)
	if id != 3 || deltaFrameText(t, frame) != "Hello" {
		t.Fatalf("first delta frame = %d %+v", id, frame)
	}
	appendDelta("!")
	id, frame = readSSEFrame(t, reader)
	if id != 3 {
		t.Fatalf("patch frame id = %d, want 3", id)
	}
	patchData := frame.Events[0].Data.(map[string]any)
	if frame.Mode != "append" || patchData["text"] != "!" {
		t.Fatalf("live frame = %+v, want append patch with only the new fragment", patchData)
	}
	if _, err := store.Append(created.ID, "turn.completed", "turn_1", nil); err != nil {
		t.Fatal(err)
	}
	if id, _ := readSSEFrame(t, reader); id != 4 {
		t.Fatalf("frame after patch id = %d, want 4", id)
	}
	cancel()

	// Reconnect from the merged tail event: the replay must first re-send it
	// with the full accumulated content, then continue with newer events.
	reconnect, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/sessions/"+created.ID+"/events?stream=true&after=3", nil)
	reconnectResponse, err := http.DefaultClient.Do(reconnect)
	if err != nil {
		t.Fatal(err)
	}
	defer reconnectResponse.Body.Close()
	reconnectReader := bufio.NewReader(reconnectResponse.Body)
	id, frame = readSSEFrame(t, reconnectReader)
	if id != 3 || deltaFrameText(t, frame) != "Hello!" {
		t.Fatalf("reconnect first frame = %d %q, want the full merged event 3", id, deltaFrameText(t, frame))
	}
	if frame.Mode != "replace" {
		t.Fatalf("replayed frame mode = %q, want replace", frame.Mode)
	}
	if id, _ := readSSEFrame(t, reconnectReader); id != 4 {
		t.Fatalf("reconnect second frame id = %d, want 4", id)
	}
}

func readSSEFrame(t *testing.T, reader *bufio.Reader) (int64, semantic.Frame) {
	t.Helper()
	var idLine, dataLine string
	for dataLine == "" {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		trimmed := strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(trimmed, "id: "):
			idLine = trimmed
		case strings.HasPrefix(trimmed, "data: "):
			dataLine = trimmed
		}
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(idLine, "id: "), 10, 64)
	if err != nil {
		t.Fatalf("invalid SSE id line %q: %v", idLine, err)
	}
	var frame semantic.Frame
	if err := json.Unmarshal([]byte(strings.TrimPrefix(dataLine, "data: ")), &frame); err != nil {
		t.Fatal(err)
	}
	return id, frame
}

func deltaFrameText(t *testing.T, frame semantic.Frame) string {
	t.Helper()
	if len(frame.Events) != 1 {
		t.Fatalf("delta frame events = %d, want 1", len(frame.Events))
	}
	data, _ := frame.Events[0].Data.(map[string]any)
	text, _ := data["text"].(string)
	return text
}

// When the server starts shutting down, live SSE streams must end on their
// own so http.Server.Shutdown is not blocked until its deadline by clients
// that keep the connection open.
func TestSSEStopsWhenServerCloses(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(session.CreateInput{Title: "SSE", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	closing := make(chan struct{})
	server := httptest.NewServer(New(store, "test", time.Now(), Dependencies{Closing: closing}).Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/v1/sessions/" + created.ID + "/events?stream=true")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "id: 1\n" {
		t.Fatalf("expected replayed first event, got %q (%v)", line, err)
	}

	close(closing)
	deadline := time.After(5 * time.Second)
	errCh := make(chan error, 1)
	go func() {
		for {
			if _, err := reader.ReadString('\n'); err != nil {
				errCh <- err
				return
			}
		}
	}()
	select {
	case err := <-errCh:
		if err != io.EOF {
			t.Fatalf("stream ended with %v, want clean EOF", err)
		}
	case <-deadline:
		t.Fatal("SSE stream did not end after the server started closing")
	}
}

type sseRecorder struct {
	header  http.Header
	want    int
	blockAt int

	mu          sync.Mutex
	ids         []int
	reached     chan struct{}
	reachedOnce sync.Once
	blocked     chan struct{}
	blockedOnce sync.Once
	release     chan struct{}
}

func newSSERecorder(want, blockAt int) *sseRecorder {
	return &sseRecorder{
		header:  make(http.Header),
		want:    want,
		blockAt: blockAt,
		reached: make(chan struct{}),
		blocked: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *sseRecorder) Header() http.Header {
	return w.header
}

func (w *sseRecorder) WriteHeader(int) {}

func (w *sseRecorder) Flush() {}

func (w *sseRecorder) Write(data []byte) (int, error) {
	line := strings.SplitN(string(data), "\n", 2)[0]
	if !strings.HasPrefix(line, "id: ") {
		return len(data), nil
	}
	id, err := strconv.Atoi(strings.TrimPrefix(line, "id: "))
	if err != nil {
		return 0, err
	}
	w.mu.Lock()
	w.ids = append(w.ids, id)
	count := len(w.ids)
	w.mu.Unlock()
	if w.want > 0 && count == w.want {
		w.reachedOnce.Do(func() { close(w.reached) })
	}
	if w.blockAt > 0 && count == w.blockAt {
		w.blockedOnce.Do(func() { close(w.blocked) })
		<-w.release
	}
	return len(data), nil
}

func (w *sseRecorder) IDs() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]int(nil), w.ids...)
}

func seedEventStore(t *testing.T, total int) (*session.Store, session.Session) {
	t.Helper()
	root := t.TempDir()
	store, err := session.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(session.CreateInput{Title: "Backlog", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if total < 1 {
		t.Fatal("seed total must include session.created")
	}
	path := filepath.Join(root, created.ID, "events.jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for id := 2; id <= total; id++ {
		event := session.Event{
			ID:        int64(id),
			Time:      time.Unix(int64(id), 0).UTC(),
			Type:      "provider.seed",
			SessionID: created.ID,
			Data:      mustMarshal(t, map[string]int{"id": id}),
		}
		if err := encoder.Encode(event); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Sync(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := session.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return reopened, created
}

func waitClosed(t *testing.T, channel <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func assertContiguousIDs(t *testing.T, ids []int, first, last int) {
	t.Helper()
	if len(ids) != last-first+1 {
		t.Fatalf("received %d ids, want %d (%d..%d)", len(ids), last-first+1, first, last)
	}
	for index, id := range ids {
		if want := first + index; id != want {
			t.Fatalf("id[%d] = %d, want %d", index, id, want)
		}
	}
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func createTestSession(t *testing.T, server *httptest.Server) session.Session {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"title": "archive api", "cwd": t.TempDir(), "agentName": "Agent"})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/sessions", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create status: %s", response.Status)
	}
	var created struct {
		Session session.Session `json:"session"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	return created.Session
}

func listIDs(t *testing.T, server *httptest.Server, query string) map[string]string {
	t.Helper()
	response, err := http.Get(server.URL + "/v1/sessions" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var listed struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	result := make(map[string]string)
	for _, value := range listed.Sessions {
		result[value.ID] = value.State
	}
	return result
}

func deleteSession(t *testing.T, server *httptest.Server, id string) *http.Response {
	t.Helper()
	request, _ := http.NewRequest(http.MethodDelete, server.URL+"/v1/sessions/"+id, bytes.NewReader([]byte("{}")))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestArchiveEndpointMovesAndHidesSession(t *testing.T) {
	root := t.TempDir()
	store, err := session.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(store, "test", time.Now()).Handler())
	defer server.Close()
	created := createTestSession(t, server)
	if _, err := store.Append(created.ID, "session.state", "", mustMarshal(t, session.StateEventData{
		State: session.StateStopped, Reason: session.StopReasonRequested,
	})); err != nil {
		t.Fatal(err)
	}

	response := deleteSession(t, server, created.ID)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("archive status: %s", response.Status)
	}
	var archived struct {
		Session session.Session `json:"session"`
	}
	if err := json.NewDecoder(response.Body).Decode(&archived); err != nil {
		t.Fatal(err)
	}
	if archived.Session.State != session.StateArchived {
		t.Fatalf("state = %q, want archived", archived.Session.State)
	}

	// The directory physically moved into Archive/.
	if _, err := os.Stat(filepath.Join(root, created.ID)); !os.IsNotExist(err) {
		t.Fatalf("active directory still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, session.ArchiveDirName, created.ID, "events.jsonl")); err != nil {
		t.Fatalf("archived events missing: %v", err)
	}

	// Hidden by default, visible through the explicit archived view and the
	// includeArchived flag.
	if _, ok := listIDs(t, server, "")[created.ID]; ok {
		t.Fatal("archived session appears in the default list")
	}
	if state := listIDs(t, server, "?archived=true")[created.ID]; state != session.StateArchived {
		t.Fatalf("archived view state = %q", state)
	}
	if state := listIDs(t, server, "?includeArchived=true")[created.ID]; state != session.StateArchived {
		t.Fatalf("includeArchived state = %q", state)
	}

	// Metadata and history stay readable.
	response, err = http.Get(server.URL + "/v1/sessions/" + created.ID)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("get archived status: %s", response.Status)
	}
	response, err = http.Get(server.URL + "/v1/sessions/" + created.ID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var history struct {
		Frames []semantic.Frame `json:"frames"`
	}
	if err := json.NewDecoder(response.Body).Decode(&history); err != nil {
		t.Fatal(err)
	}
	last := history.Frames[len(history.Frames)-1].Events
	if len(last) != 1 || last[0].Type != "session.archived" {
		t.Fatalf("last frame = %+v, want session.archived", last)
	}

	// Repeating the archive is idempotent.
	response = deleteSession(t, server, created.ID)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("repeat archive status: %s", response.Status)
	}
}

func TestArchiveEndpointErrors(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(store, "test", time.Now()).Handler())
	defer server.Close()

	response := deleteSession(t, server, "ses_missing")
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing archive status: %s", response.Status)
	}

	// A running session conflicts and keeps its directory.
	created := createTestSession(t, server)
	if _, err := store.Append(created.ID, "turn.started", "turn_open", nil); err != nil {
		t.Fatal(err)
	}
	response = deleteSession(t, server, created.ID)
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("active archive status: %s", response.Status)
	}
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&failure); err != nil {
		t.Fatal(err)
	}
	if failure.Error.Code != "session_active" {
		t.Fatalf("error code = %q, want session_active", failure.Error.Code)
	}
	value, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.State != session.StateRunning {
		t.Fatalf("state changed to %q after conflict", value.State)
	}
}

func TestArchivedSessionRejectsWrites(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(store, "test", time.Now()).Handler())
	defer server.Close()
	created := createTestSession(t, server)
	if _, err := store.Append(created.ID, "session.state", "", mustMarshal(t, session.StateEventData{
		State: session.StateStopped, Reason: session.StopReasonRequested,
	})); err != nil {
		t.Fatal(err)
	}
	response := deleteSession(t, server, created.ID)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("archive status: %s", response.Status)
	}

	for _, path := range []string{"messages", "resume", "interrupt", "stop", "approvals/apr_1"} {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/sessions/"+created.ID+"/"+path, bytes.NewReader([]byte("{}")))
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.NewDecoder(response.Body).Decode(&failure)
		response.Body.Close()
		if response.StatusCode != http.StatusConflict || failure.Error.Code != "session_archived" {
			t.Fatalf("POST %s: status = %s code = %q, want 409 session_archived", path, response.Status, failure.Error.Code)
		}
	}

	// A resume carrying a launchEnvironment overlay is rejected the same
	// way, and the archived environment stays untouched.
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/sessions/"+created.ID+"/resume", strings.NewReader(`{"launchEnvironment":{"SESSION_CONTEXT_ID":"context-new"}}`))
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("archived resume with environment: status = %s, want 409", response.Status)
	}
	value, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(value.LaunchEnvironment) != 0 {
		t.Fatalf("archived launch environment changed: %+v", value.LaunchEnvironment)
	}
}

// TestResumeSessionAcceptsOptionalLaunchEnvironment covers the resume
// request contract: the body stays optional, a valid overlay is validated
// and persisted before the provider starts (so it survives even this
// test's failing provider), and invalid input is rejected without touching
// the durable environment.
func TestResumeSessionAcceptsOptionalLaunchEnvironment(t *testing.T) {
	root := t.TempDir()
	store, err := session.Open(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Version:        1,
		AgentProviders: []config.Provider{{ID: "provider", Type: "pi", Enabled: true, Command: "missing-test-command"}},
		Agents:         []config.Agent{{Name: "Pi Agent", ProviderID: "provider"}},
	}
	manager := runtime.New(store, cfg)
	t.Cleanup(manager.Close)
	server := httptest.NewServer(New(store, "test", time.Now(), Dependencies{Runtime: manager}).Handler())
	defer server.Close()
	created, err := store.Create(session.CreateInput{
		Cwd:               t.TempDir(),
		AgentName:         "Pi Agent",
		LaunchEnvironment: map[string]string{"SESSION_CONTEXT_ID": "context-old", "KEEP": "original"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The test provider binary does not exist, so a resume that passes
	// request validation reaches the runtime and fails there with a
	// conflict; anything else means the request was rejected earlier.
	postResume := func(body *string) (int, string) {
		t.Helper()
		var reader *strings.Reader
		if body != nil {
			reader = strings.NewReader(*body)
		} else {
			reader = strings.NewReader("")
		}
		request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/sessions/"+created.ID+"/resume", reader)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var parsed struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.NewDecoder(response.Body).Decode(&parsed)
		return response.StatusCode, parsed.Error.Code
	}

	// Empty body and {} remain valid resumes.
	for name, body := range map[string]*string{
		"empty body":   nil,
		"empty object": stringPointer(`{}`),
	} {
		status, code := postResume(body)
		if status != http.StatusConflict || code != "runtime_operation_failed" {
			t.Fatalf("%s: status/code = %d/%q, want 409/runtime_operation_failed", name, status, code)
		}
		value, err := store.Get(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(value.LaunchEnvironment, map[string]string{"SESSION_CONTEXT_ID": "context-old", "KEEP": "original"}) {
			t.Fatalf("%s changed the environment: %+v", name, value.LaunchEnvironment)
		}
	}

	// A valid overlay is persisted before the provider start fails, and
	// keeps keys the overlay did not mention.
	status, code := postResume(stringPointer(`{"launchEnvironment":{"SESSION_CONTEXT_ID":"context-new","EXTRA":"x"}}`))
	if status != http.StatusConflict || code != "runtime_operation_failed" {
		t.Fatalf("overlay resume: status/code = %d/%q, want 409/runtime_operation_failed", status, code)
	}
	value, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"SESSION_CONTEXT_ID": "context-new", "KEEP": "original", "EXTRA": "x"}
	if !maps.Equal(value.LaunchEnvironment, want) {
		t.Fatalf("environment after overlay resume = %+v, want %+v", value.LaunchEnvironment, want)
	}
	events, err := store.EventsAfter(created.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var environmentEvents int
	for _, event := range events {
		if event.Type != "session.launch-environment" {
			continue
		}
		environmentEvents++
		var data session.LaunchEnvironmentEventData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(data.Environment, want) {
			t.Fatalf("session.launch-environment payload = %+v, want %+v", data.Environment, want)
		}
	}
	if environmentEvents != 1 {
		t.Fatalf("session.launch-environment events = %d, want 1", environmentEvents)
	}

	// Invalid input is rejected without touching the durable environment.
	for name, item := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"invalid variable name": {`{"launchEnvironment":{"BAD=NAME":"v"}}`, http.StatusUnprocessableEntity, "invalid_launch_environment"},
		"malformed JSON":        {`{"launchEnvironment":`, http.StatusBadRequest, "invalid_request"},
		"unknown field":         {`{"environment":{}}`, http.StatusBadRequest, "invalid_request"},
	} {
		status, code := postResume(&item.body)
		if status != item.status || code != item.code {
			t.Fatalf("%s: status/code = %d/%q, want %d/%s", name, status, code, item.status, item.code)
		}
	}
	value, err = store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(value.LaunchEnvironment, want) {
		t.Fatalf("rejected resume changed the environment: %+v", value.LaunchEnvironment)
	}
}

// newToggleTestServer starts a daemon whose config holds a single built-in
// provider (pi) with a custom command and one agent bound to it.
func newToggleTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	store, err := session.Open(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Version:        1,
		AgentProviders: []config.Provider{{ID: "pi", Name: "Pi Coding Agent", Type: "pi", Enabled: true, Command: "missing-test-command"}},
		Agents:         []config.Agent{{Name: "Pi Agent", ProviderID: "pi"}},
	}
	configPath := filepath.Join(root, "config.json")
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	manager := runtime.New(store, cfg)
	server := httptest.NewServer(New(store, "test", time.Now(), Dependencies{Runtime: manager, ConfigPath: configPath}).Handler())
	t.Cleanup(server.Close)
	return server, configPath
}

func toggleProvider(t *testing.T, server *httptest.Server, id, body string) (int, map[string]any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPut, server.URL+"/v1/config/providers/"+id, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var parsed map[string]any
	if err := json.NewDecoder(response.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, parsed
}

func getAgents(t *testing.T, server *httptest.Server) []map[string]any {
	t.Helper()
	response, err := http.Get(server.URL + "/v1/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Agents
}

func TestToggleProviderDisableEnableRoundTrip(t *testing.T) {
	server, configPath := newToggleTestServer(t)

	// Disable: only the flag flips, the custom command is preserved, and the
	// change is persisted to disk.
	status, body := toggleProvider(t, server, "pi", `{"enabled": false}`)
	if status != http.StatusOK {
		t.Fatalf("disable: status = %d, body = %v", status, body)
	}
	provider := body["provider"].(map[string]any)
	if provider["enabled"] != false || provider["command"] != "missing-test-command" {
		t.Fatalf("disable lost the underlying configuration: %v", provider)
	}
	onDisk, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.AgentProviders[0].Enabled || onDisk.AgentProviders[0].Command != "missing-test-command" {
		t.Fatalf("toggle was not persisted: %+v", onDisk.AgentProviders[0])
	}

	// The agent of a disabled provider is reported unavailable and new
	// sessions naming it are rejected even when the client bypasses the UI.
	agents := getAgents(t, server)
	if len(agents) != 1 || agents[0]["available"] != false || agents[0]["unavailableReason"] == nil {
		t.Fatalf("agent of disabled provider should be unavailable: %v", agents)
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/sessions", strings.NewReader(
		`{"cwd":"`+t.TempDir()+`","agentName":"Pi Agent"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var created map[string]any
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnprocessableEntity || created["error"].(map[string]any)["code"] != "invalid_agent" {
		t.Fatalf("session creation with a disabled provider: status = %d, body = %v", response.StatusCode, created)
	}

	// Re-enable restores availability without losing the command.
	status, body = toggleProvider(t, server, "pi", `{"enabled": true}`)
	if status != http.StatusOK {
		t.Fatalf("enable: status = %d, body = %v", status, body)
	}
	provider = body["provider"].(map[string]any)
	if provider["enabled"] != true || provider["command"] != "missing-test-command" {
		t.Fatalf("enable lost the underlying configuration: %v", provider)
	}
	agents = getAgents(t, server)
	if len(agents) != 1 || agents[0]["available"] != true {
		t.Fatalf("agent of re-enabled provider should be available: %v", agents)
	}
}

func TestToggleProviderCreatesBuiltinDefault(t *testing.T) {
	server, _ := newToggleTestServer(t)
	status, body := toggleProvider(t, server, "kimi", `{"enabled": true}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	provider := body["provider"].(map[string]any)
	if provider["id"] != "kimi" || provider["type"] != "kimi" || provider["enabled"] != true {
		t.Fatalf("missing built-in provider was not created with defaults: %v", provider)
	}
}

func TestToggleProviderRejectsBadRequests(t *testing.T) {
	server, _ := newToggleTestServer(t)
	cases := []struct {
		name string
		id   string
		body string
		want int
		code string
	}{
		{"unknown provider", "ghost", `{"enabled": true}`, http.StatusNotFound, "unknown_provider"},
		{"non-builtin custom provider", "provider", `{"enabled": false}`, http.StatusNotFound, "unknown_provider"},
		{"missing enabled flag", "pi", `{}`, http.StatusBadRequest, "invalid_request"},
		{"wrong enabled type", "pi", `{"enabled": "yes"}`, http.StatusBadRequest, "invalid_request"},
		{"unknown field", "pi", `{"enabled": true, "command": "x"}`, http.StatusBadRequest, "invalid_request"},
	}
	for _, item := range cases {
		status, body := toggleProvider(t, server, item.id, item.body)
		code, _ := body["error"].(map[string]any)["code"].(string)
		if status != item.want || code != item.code {
			t.Errorf("%s: status = %d, code = %q, want %d %s", item.name, status, code, item.want, item.code)
		}
	}
}

// The session records the canonical configured display name, not the
// spelling the client sent.
func TestCreateSessionPersistsCanonicalAgentName(t *testing.T) {
	server := newConfigTestServer(t)
	body, _ := json.Marshal(map[string]any{"cwd": t.TempDir(), "agentName": " pi AGENT "})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/sessions", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	// The test provider cannot start; the session still exists.
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("unexpected status: %s", response.Status)
	}
	var parsed struct {
		Error struct {
			Details struct {
				SessionID string `json:"sessionId"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	value := getSession(t, server, parsed.Error.Details.SessionID)
	if value.AgentName != "Pi Agent" {
		t.Fatalf("session did not record the canonical name: %+v", value)
	}
}

// When session creation fails at the provider, the API must surface the
// real provider error and the session must end at the strict stopped boundary
// with a startup_error reason — never stuck in starting.
func TestCreateSessionProviderFailureSurfacesError(t *testing.T) {
	server := newConfigTestServer(t)
	body, _ := json.Marshal(map[string]any{"cwd": t.TempDir(), "agentName": "Pi Agent"})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/sessions", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("unexpected status: %s", response.Status)
	}
	var parsed struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				SessionID string `json:"sessionId"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Error.Code != "provider_start_failed" {
		t.Fatalf("error code = %q", parsed.Error.Code)
	}
	if parsed.Error.Message == "" || parsed.Error.Message == "failed" {
		t.Fatalf("error message must carry the provider cause: %q", parsed.Error.Message)
	}
	value := getSession(t, server, parsed.Error.Details.SessionID)
	if value.State != session.StateStopped || value.StopReason != session.StopReasonStartupError {
		t.Fatalf("unexpected session terminal state: %+v", value)
	}
}

// Renaming an agent through PUT /v1/config re-points the sessions that
// referenced the old name at the new one via a session.agent event.
func TestPutConfigRenameMigratesSessionReferences(t *testing.T) {
	server := newConfigTestServer(t)
	body, _ := json.Marshal(map[string]any{"cwd": t.TempDir(), "agentName": "Pi Agent"})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/sessions", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Error struct {
			Details struct {
				SessionID string `json:"sessionId"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.NewDecoder(response.Body).Decode(&created)
	response.Body.Close()
	sessionID := created.Error.Details.SessionID
	if sessionID == "" {
		t.Fatal("session was not created")
	}

	renamed := `{"config":{
		"version": 1,
		"agentProviders": [
			{"id": "provider", "name": "Pi", "type": "pi", "enabled": true, "command": "missing-test-command"}
		],
		"agents": [{"name": "Pi Agent X", "providerId": "provider"}]
	}}`
	status, code := putConfig(t, server, renamed)
	if status != http.StatusOK || code != "" {
		t.Fatalf("rename save failed: status = %d, code = %q", status, code)
	}
	value := getSession(t, server, sessionID)
	if value.AgentName != "Pi Agent X" {
		t.Fatalf("session did not follow the rename: %+v", value)
	}
	response, err = http.Get(server.URL + "/v1/sessions/" + sessionID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var eventsBody struct {
		Frames []semantic.Frame `json:"frames"`
	}
	if err := json.NewDecoder(response.Body).Decode(&eventsBody); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, frame := range eventsBody.Frames {
		for _, event := range frame.Events {
			data, _ := json.Marshal(event.Data)
			if event.Type == "session.agent" && strings.Contains(string(data), "Pi Agent X") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("rename did not append a session.agent event")
	}
}

// A rename that matches several identical new agents is rejected instead of
// guessed; the sessions keep referencing the old name.
func TestPutConfigRejectsAmbiguousRename(t *testing.T) {
	server := newConfigTestServer(t)
	ambiguous := `{"config":{
		"version": 1,
		"agentProviders": [
			{"id": "provider", "name": "Pi", "type": "pi", "enabled": true, "command": "missing-test-command"}
		],
		"agents": [
			{"name": "Pi Agent X", "providerId": "provider"},
			{"name": "Pi Agent Y", "providerId": "provider"}
		]
	}}`
	status, code := putConfig(t, server, ambiguous)
	if status != http.StatusUnprocessableEntity || code != "ambiguous_rename" {
		t.Fatalf("ambiguous rename: status = %d, code = %q", status, code)
	}
	response, err := http.Get(server.URL + "/v1/config")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var configBody struct {
		Config config.Config `json:"config"`
	}
	if err := json.NewDecoder(response.Body).Decode(&configBody); err != nil {
		t.Fatal(err)
	}
	if len(configBody.Config.Agents) != 1 || configBody.Config.Agents[0].Name != "Pi Agent" {
		t.Fatalf("config changed after the rejected rename: %+v", configBody.Config)
	}
}

func getSession(t *testing.T, server *httptest.Server, id string) session.Session {
	t.Helper()
	response, err := http.Get(server.URL + "/v1/sessions/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body struct {
		Session session.Session `json:"session"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Session
}

func TestStatusReportsUnifiedDataPaths(t *testing.T) {
	root := t.TempDir()
	store, err := session.Open(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(store, "test", time.Now(), Dependencies{
		ConfigPath: filepath.Join(root, "config.json"),
		LogsDir:    filepath.Join(root, "logs"),
	}).Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %s", response.Status)
	}
	var body struct {
		Paths map[string]string `json:"paths"`
		Store map[string]any    `json:"sessionStore"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"config":   filepath.Join(root, "config.json"),
		"sessions": filepath.Join(root, "sessions"),
		"archive":  filepath.Join(root, "sessions", "Archive"),
		"logs":     filepath.Join(root, "logs"),
	}
	for key, value := range want {
		if body.Paths[key] != value {
			t.Errorf("paths.%s = %q, want %q", key, body.Paths[key], value)
		}
	}
	if body.Store["path"] != want["sessions"] || body.Store["archivePath"] != want["archive"] {
		t.Errorf("sessionStore = %+v", body.Store)
	}
}

func TestStatusCapabilitiesAreBackedByHTTPBehavior(t *testing.T) {
	root := t.TempDir()
	store, err := session.Open(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(session.CreateInput{
		Cwd:               t.TempDir(),
		AgentName:         "Agent",
		Source:            &session.Source{App: "pua", InstanceID: "mac-1", ExternalID: "task-30"},
		LaunchEnvironment: map[string]string{"SESSION_CONTEXT_ID": "context-30"},
	})
	if err != nil {
		t.Fatal(err)
	}
	turnID := "turn_contract"
	if _, err := store.Append(created.ID, "turn.started", turnID, []byte(`{"text":"work"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, "approval.requested", turnID, []byte(`{"approvalId":"approval-1"}`)); err != nil {
		t.Fatal(err)
	}

	// Manager construction is the daemon-recovery boundary. It must close
	// the pending approval and open turn before this instance advertises
	// runtime lifecycle capabilities.
	manager := runtime.New(store, config.Config{})
	server := httptest.NewServer(New(store, "test", time.Now(), Dependencies{Runtime: manager}).Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var status struct {
		APIVersion   string   `json:"apiVersion"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.APIVersion != APIVersion {
		t.Fatalf("apiVersion = %q", status.APIVersion)
	}
	wantCapabilities := []string{
		CapabilityEventsLosslessReplay,
		CapabilityEventsDeltaMerge,
		CapabilityEventsBackwardPagination,
		CapabilityEventsSemanticV1,
		CapabilityEventRawV1,
		CapabilityActivityGlobalSSE,
		CapabilitySessionSource,
		CapabilitySessionSourceMetadata,
		CapabilitySessionIdempotentCreate,
		CapabilitySessionInputCapabilities,
		CapabilityMessageIdempotency,
		CapabilityMessageAtLeastOnce,
		CapabilityMessageDeliveryResult,
		CapabilityMessageOpaquePayloadV2,
		CapabilityTurnsStableIndex,
		CapabilityTurnsMaterialized,
		CapabilityTurnsActivityItems,
		CapabilityEventsCanonicalTerminal,
		CapabilityRecoveryClosedTurns,
		CapabilitySessionLaunchEnvironment,
		CapabilitySessionLaunchEnvironmentUpdate,
		CapabilitySessionStrictStopped,
	}
	if strings.Join(status.Capabilities, ",") != strings.Join(wantCapabilities, ",") {
		t.Fatalf("capabilities = %v, want %v", status.Capabilities, wantCapabilities)
	}

	response, err = http.Get(server.URL + "/v1/sessions?sourceApp=pua&sourceInstanceId=mac-1&sourceExternalId=task-30")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var listed struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Sessions) != 1 ||
		listed.Sessions[0].LaunchEnvironment["SESSION_CONTEXT_ID"] != "context-30" ||
		listed.Sessions[0].State != session.StateStopped ||
		listed.Sessions[0].StopReason != session.StopReasonDaemonRecovery ||
		listed.Sessions[0].CurrentTurnID != "" ||
		len(listed.Sessions[0].PendingApprovalIDs) != 0 {
		t.Fatalf("recovered session = %+v", listed.Sessions)
	}

	response, err = http.Get(server.URL + "/v1/sessions/" + created.ID + "/events?after=0&limit=2")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var firstPage struct {
		Frames       []semantic.Frame `json:"frames"`
		LatestCursor int64            `json:"latestCursor"`
		Page         struct {
			NextAfter int64 `json:"nextAfter"`
			HasMore   bool  `json:"hasMore"`
		} `json:"page"`
	}
	if err := json.NewDecoder(response.Body).Decode(&firstPage); err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Frames) != 2 || !firstPage.Page.HasMore || firstPage.LatestCursor <= firstPage.Page.NextAfter {
		t.Fatalf("lossless first page = %+v", firstPage)
	}
	allEvents, err := store.EventsAfter(created.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var terminal *session.Event
	for i := range allEvents {
		if allEvents[i].Type == session.EventTurnCancelled {
			terminal = &allEvents[i]
		}
	}
	if terminal == nil || string(terminal.Data) != `{"reason":"daemon_recovery"}` {
		t.Fatalf("canonical terminal event = %+v", terminal)
	}
}

func TestStatusOmitsUnavailableRuntimeCapabilities(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	New(store, "test", time.Now()).Handler().ServeHTTP(response, request)
	var body struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	want := []string{
		CapabilityEventsLosslessReplay, CapabilityEventsDeltaMerge, CapabilityEventsBackwardPagination,
		CapabilityEventsSemanticV1, CapabilityEventRawV1,
		CapabilityActivityGlobalSSE, CapabilitySessionSource, CapabilitySessionSourceMetadata,
		CapabilitySessionIdempotentCreate, CapabilitySessionInputCapabilities,
		CapabilityMessageIdempotency, CapabilityMessageAtLeastOnce, CapabilityMessageDeliveryResult, CapabilityMessageOpaquePayloadV2, CapabilityTurnsStableIndex, CapabilityTurnsMaterialized,
		CapabilityTurnsActivityItems,
	}
	if strings.Join(body.Capabilities, ",") != strings.Join(want, ",") {
		t.Fatalf("capabilities = %v, want %v", body.Capabilities, want)
	}
}

func TestEveryAPINon2xxResponseUsesStructuredEnvelope(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(session.CreateInput{Cwd: t.TempDir(), AgentName: "Agent"})
	if err != nil {
		t.Fatal(err)
	}
	handler := New(store, "test", time.Now()).Handler()
	tests := []struct {
		name        string
		method      string
		path        string
		contentType string
		body        string
		code        string
		retryable   bool
	}{
		{"unknown API route", http.MethodGet, "/v1/unknown", "", "", "route_not_found", false},
		{"API root", http.MethodGet, "/v1", "", "", "route_not_found", false},
		{"session method", http.MethodPut, "/v1/sessions/" + created.ID, "application/json", `{}`, "method_not_allowed", false},
		{"docs method", http.MethodPost, "/api.md", "application/json", `{}`, "method_not_allowed", false},
		{"missing runtime", http.MethodGet, "/v1/config", "", "", "runtime_unavailable", true},
		{"invalid JSON", http.MethodPost, "/v1/sessions", "application/json", `{} {}`, "invalid_request", false},
		{"wrong media type", http.MethodPost, "/v1/sessions", "text/plain", `{}`, "json_required", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code < 400 {
				t.Fatalf("status = %d", response.Code)
			}
			if got := response.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q", got)
			}
			var body struct {
				Error struct {
					Code      string          `json:"code"`
					Message   string          `json:"message"`
					Retryable *bool           `json:"retryable"`
					Details   json.RawMessage `json:"details"`
					RequestID string          `json:"requestId"`
				} `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != test.code || body.Error.Message == "" || body.Error.Retryable == nil ||
				*body.Error.Retryable != test.retryable || body.Error.RequestID == "" {
				t.Fatalf("error envelope = %+v", body.Error)
			}
		})
	}
}

func TestSessionConflictCodesAreStable(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := runtime.New(store, config.Config{})
	active, err := store.Create(session.CreateInput{Cwd: t.TempDir(), AgentName: "Agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(active.ID, "turn.started", "turn_active", []byte(`{"text":"work"}`)); err != nil {
		t.Fatal(err)
	}
	stopping, err := store.Create(session.CreateInput{Cwd: t.TempDir(), AgentName: "Agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(stopping.ID, "session.state", "", []byte(`{"state":"stopping"}`)); err != nil {
		t.Fatal(err)
	}
	handler := New(store, "test", time.Now(), Dependencies{Runtime: manager}).Handler()
	tests := []struct {
		name   string
		path   string
		body   string
		status int
		code   string
	}{
		{"blank message", "/v1/sessions/" + active.ID + "/messages", `{"text":" "}`, http.StatusBadRequest, "invalid_request"},
		{"active turn", "/v1/sessions/" + active.ID + "/messages", `{"text":"next"}`, http.StatusConflict, "turn_active"},
		{"interrupt without turn", "/v1/sessions/" + stopping.ID + "/interrupt", `{}`, http.StatusConflict, "turn_not_active"},
		{"resume while stopping", "/v1/sessions/" + stopping.ID + "/resume", `{}`, http.StatusConflict, "session_stopping"},
		{"resume with environment while stopping", "/v1/sessions/" + stopping.ID + "/resume", `{"launchEnvironment":{"A":"b"}}`, http.StatusConflict, "session_stopping"},
		{"invalid approval decision", "/v1/sessions/" + active.ID + "/approvals/approval-1", `{"decision":"maybe"}`, http.StatusBadRequest, "invalid_approval_decision"},
		{"approval text with decision", "/v1/sessions/" + active.ID + "/approvals/approval-1", `{"text":"hi","decision":"accept"}`, http.StatusBadRequest, "invalid_approval_decision"},
		{"approval option with decision", "/v1/sessions/" + active.ID + "/approvals/approval-1", `{"optionId":"opt-1","decision":"accept"}`, http.StatusBadRequest, "invalid_approval_decision"},
		{"approval text not pending", "/v1/sessions/" + active.ID + "/approvals/approval-1", `{"text":"hi"}`, http.StatusConflict, "approval_not_pending"},
		{"approval option not pending", "/v1/sessions/" + active.ID + "/approvals/approval-1", `{"optionId":"opt-1"}`, http.StatusConflict, "approval_not_pending"},
		{"approval not pending", "/v1/sessions/" + active.ID + "/approvals/approval-1", `{"decision":"accept"}`, http.StatusConflict, "approval_not_pending"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if response.Code != test.status || body.Error.Code != test.code {
				t.Fatalf("status/code = %d/%s, want %d/%s", response.Code, body.Error.Code, test.status, test.code)
			}
		})
	}
}

func TestMessageSourceValidationUsesStructuredErrors(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(session.CreateInput{Cwd: t.TempDir(), AgentName: "Agent"})
	if err != nil {
		t.Fatal(err)
	}
	handler := New(store, "test", time.Now()).Handler()

	tests := []struct {
		name string
		body string
		code string
	}{
		{name: "unknown role", body: `{"text":"hello","role":"developer"}`, code: "invalid_message_role"},
		{name: "assistant role", body: `{"text":"spoof","role":"assistant"}`, code: "assistant_message_forbidden"},
		{name: "sender shape", body: `{"text":"notice","role":"system","sender":"scheduler"}`, code: "invalid_message_sender"},
		{name: "empty sender", body: `{"text":"notice","role":"system","sender":{}}`, code: "invalid_message_sender"},
		{name: "unknown schema", body: `{"schemaVersion":3,"text":"notice"}`, code: "invalid_message_schema"},
		{name: "v2 legacy mixing", body: `{"schemaVersion":2,"text":"notice","payload":{},"role":"system"}`, code: "mixed_message_schema"},
		{name: "payload without v2", body: `{"text":"notice","payload":{}}`, code: "mixed_message_schema"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+created.ID+"/messages", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			var body struct {
				Error struct {
					Code    string          `json:"code"`
					Details json.RawMessage `json:"details"`
				} `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusBadRequest || body.Error.Code != test.code || len(body.Error.Details) == 0 {
				t.Fatalf("response = %d %+v", response.Code, body.Error)
			}
		})
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"wake","role":"system","sender":{"name":"Scheduler"}}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("valid source request must pass validation before runtime check: status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/sessions/"+created.ID+"/messages", strings.NewReader(`{"schemaVersion":2,"text":"already formatted","payload":{"schema":"app.v1","role":"anything"}}`))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("valid opaque request must pass validation before runtime check: status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestMessageResponseReportsPendingProviderDelivery(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(session.CreateInput{Cwd: t.TempDir(), AgentName: "Unavailable Agent"})
	if err != nil {
		t.Fatal(err)
	}
	manager := runtime.New(store, config.Config{})
	t.Cleanup(manager.Close)
	handler := New(store, "test", time.Now(), Dependencies{Runtime: manager}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"keep this pending","messageId":"stable-pending-1"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	var body struct {
		Session  session.Session                 `json:"session"`
		Delivery session.MessageProviderDelivery `json:"delivery"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusAccepted || body.Session.ID != created.ID {
		t.Fatalf("response = %d %+v", response.Code, body)
	}
	if body.Delivery.MessageID != "stable-pending-1" || body.Delivery.State != session.MessageProviderDeliveryPending {
		t.Fatalf("delivery = %+v, want stable pending", body.Delivery)
	}
	message, found, err := store.DurableMessageByID(created.ID, "stable-pending-1")
	if err != nil || !found || message.Delivered {
		t.Fatalf("durable message = %+v, found=%v, err=%v", message, found, err)
	}
}
