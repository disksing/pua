package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/disksing/pua/agenthub/internal/companion"
	"github.com/disksing/pua/agenthub/internal/config"
	"github.com/disksing/pua/agenthub/internal/runtime"
	"github.com/disksing/pua/agenthub/internal/session"
)

func companionTestServer(t *testing.T, cfg config.Config, upstream *httptest.Server) (*httptest.Server, string, *session.Store) {
	t.Helper()
	root := t.TempDir()
	store, err := session.Open(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	manager := runtime.New(store, cfg)
	dependencies := Dependencies{Runtime: manager, ConfigPath: configPath}
	if upstream != nil {
		dependencies.QuotaHTTPClient = upstream.Client()
	}
	server := httptest.NewServer(New(store, "test", time.Now(), dependencies).Handler())
	t.Cleanup(server.Close)
	return server, configPath, store
}

func TestConfigAPIDropsLegacyCompanionAndOnWatchCredentials(t *testing.T) {
	cfg := config.Defaults()
	cfg.Magpie = config.Magpie{}
	cfg.LegacyOnWatch = json.RawMessage(`{"enabled":true,"refreshIntervalSeconds":30,"password":"secret"}`)
	server, configPath, _ := companionTestServer(t, cfg, nil)

	response, err := http.Get(server.URL + "/v1/config")
	if err != nil {
		t.Fatal(err)
	}
	var fetched struct {
		Config config.Config `json:"config"`
	}
	if err := json.NewDecoder(response.Body).Decode(&fetched); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if fetched.Config.LegacyOnWatch != nil || fetched.Config.Magpie.Enabled || !fetched.Config.Magpie.MigratedFromOnWatch || fetched.Config.Magpie.RefreshIntervalSeconds != 30 {
		t.Fatalf("GET /v1/config did not migrate the legacy configuration: %+v", fetched)
	}
	if fetched.Config.LegacyCompanion != nil {
		t.Fatal("GET /v1/config exposed legacy companion settings")
	}
	// Old clients may still submit this field during a rolling upgrade. Accept
	// it for compatibility, but discard it before updating runtime or disk.
	fetched.Config.LegacyCompanion = json.RawMessage(`{"enableBeeping":false,"beepChord":"a-minor"}`)
	fetched.Config.LegacyOnWatch = cfg.LegacyOnWatch
	body, _ := json.Marshal(map[string]any{"config": fetched.Config})
	request, _ := http.NewRequest(http.MethodPut, server.URL+"/v1/config", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PUT config status = %s", response.Status)
	}
	var savedResponse struct {
		Config config.Config `json:"config"`
	}
	if err := json.NewDecoder(response.Body).Decode(&savedResponse); err != nil {
		t.Fatal(err)
	}
	if savedResponse.Config.LegacyOnWatch != nil {
		t.Fatal("PUT /v1/config response exposed the legacy OnWatch config")
	}
	if savedResponse.Config.LegacyCompanion != nil {
		t.Fatal("PUT /v1/config response exposed legacy companion settings")
	}
	stored, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LegacyOnWatch != nil || stored.LegacyCompanion != nil || stored.Magpie != fetched.Config.Magpie {
		t.Fatalf("stored config = %+v", stored)
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`"companion"`, `"onWatch"`, "secret"} {
		if bytes.Contains(written, []byte(value)) {
			t.Errorf("stored config retained %q", value)
		}
	}
}

func TestQuotaAndMagpieTestRoutes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/v1/magpie/quotas?kept=1" {
			t.Errorf("unexpected Magpie request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Forwarded-User") != "" {
			t.Error("received unexpected credentials")
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"provider":"kimi","name":"Kimi Code","kind":"subscription","user":"account@example.test","windows":[{"name":"5h","used":12,"remaining":88}]}]}`))
	}))
	defer upstream.Close()
	cfg := config.Defaults()
	cfg.Magpie.ServerURL = upstream.URL
	server, configPath, _ := companionTestServer(t, cfg, upstream)
	testBody, _ := json.Marshal(map[string]any{"magpie": cfg.Magpie})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/magpie/test", bytes.NewReader(testBody))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var tested struct {
		Connected bool `json:"connected"`
		Cards     int  `json:"cards"`
	}
	if err := json.NewDecoder(response.Body).Decode(&tested); err != nil || response.StatusCode != http.StatusOK || !tested.Connected || tested.Cards != 1 {
		t.Fatalf("connection test = %+v, status = %s, err = %v", tested, response.Status, err)
	}
	response.Body.Close()
	stored, err := config.Load(configPath)
	if err != nil || stored.Magpie.Enabled {
		t.Fatalf("connection test changed config: %+v, %v", stored.Magpie, err)
	}
	readQuota := func() companion.QuotaSnapshot {
		t.Helper()
		response, err := http.Get(server.URL + "/v1/quota")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var body struct {
			Quota companion.QuotaSnapshot `json:"quota"`
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body.Quota
	}
	if quota := readQuota(); quota.Configured || quota.Connected {
		t.Fatalf("connection test enabled quota: %+v", quota)
	}
	cfg.Magpie.Enabled = true
	body, _ := json.Marshal(map[string]any{"config": cfg})
	request, _ = http.NewRequest(http.MethodPut, server.URL+"/v1/config", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("enable Magpie status = %s", response.Status)
	}
	quota := readQuota()
	if !quota.Connected || len(quota.Providers) != 1 || quota.Providers[0].Provider != "kimi" || *quota.Providers[0].Quotas[0].RemainingPercent != 88 {
		t.Fatalf("enabled quota = %+v", quota)
	}
}

func TestActivitySSEAggregatesAllSessionsPerSecond(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, _ := store.Create(session.CreateInput{Title: "First", Cwd: t.TempDir(), AgentName: "Codex"})
	second, _ := store.Create(session.CreateInput{Title: "Second", Cwd: t.TempDir(), AgentName: "Kimi"})
	third, _ := store.Create(session.CreateInput{Title: "Third", Cwd: t.TempDir(), AgentName: "Pi"})
	fourth, _ := store.Create(session.CreateInput{Title: "Fourth", Cwd: t.TempDir(), AgentName: "OpenCode"})
	noise, _ := store.Create(session.CreateInput{Title: "Idle background", Cwd: t.TempDir(), AgentName: "Codex"})
	providerData, _ := json.Marshal(session.ProviderEventData{Provider: "codex"})
	_, _ = store.Append(first.ID, "session.provider", "", providerData)
	server := httptest.NewServer(New(store, "test", time.Now()).Handler())
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/activity/events", nil)
	request.Header.Set("Accept", "text/event-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type = %q", response.Header.Get("Content-Type"))
	}
	_, _ = store.Append(first.ID, "message.assistant.delta", "turn-1", []byte(`{"text":"a"}`))
	_, _ = store.Append(first.ID, session.EventTurnCompleted, "turn-1", []byte(`{}`))
	_, _ = store.Append(second.ID, session.EventTurnFailed, "turn-2", []byte(`{"error":"boom"}`))
	_, _ = store.Append(third.ID, session.EventTurnCancelled, "turn-3", []byte(`{"reason":"stopped"}`))
	_, _ = store.Append(fourth.ID, session.EventTurnCompleted, "turn-4a", []byte(`{}`))
	_, _ = store.Append(fourth.ID, "turn.started", "turn-4b", []byte(`{}`))
	// Daemon shutdown/recovery bookkeeping and idle Provider maintenance are
	// durable events, but must not create or refresh Activity Monitor rows.
	_, _ = store.Append(first.ID, "provider.metadata", "turn-1", []byte(`{"kind":"usage_update"}`))
	_, _ = store.Append(first.ID, session.EventMessageDelivery, "turn-1", []byte(`{"state":"accepted"}`))
	_, _ = store.Append(noise.ID, "provider.stderr", "", []byte(`{"text":"background refresh failed"}`))
	_, _ = store.Append(noise.ID, "provider.event", "", []byte(`{"method":"skills/changed"}`))
	_, _ = store.Append(noise.ID, "session.state", "", []byte(`{"state":"stopping"}`))
	_, _ = store.Append(noise.ID, "session.state", "", []byte(`{"state":"stopped","reason":"requested"}`))

	scanner := bufio.NewScanner(response.Body)
	var frame activityFrame
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) > 6 && line[:6] == "data: " {
			if err := json.Unmarshal([]byte(line[6:]), &frame); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if frame.Sequence == 0 || len(frame.Sessions) != 4 {
		t.Fatalf("activity frame = %+v, scanner error = %v", frame, scanner.Err())
	}
	byID := map[string]activitySession{}
	for _, value := range frame.Sessions {
		byID[value.SessionID] = value
	}
	if byID[first.ID].EventCount != 2 || !byID[first.ID].Completed || byID[first.ID].Provider != "codex" || byID[first.ID].Title != "First" {
		t.Fatalf("first activity = %+v", byID[first.ID])
	}
	if _, ok := byID[noise.ID]; ok {
		t.Fatalf("idle background session appeared in activity frame: %+v", byID[noise.ID])
	}
	if byID[first.ID].TurnID != "turn-1" || byID[first.ID].TurnTerminal == nil || byID[first.ID].TurnTerminal.Status != "completed" || byID[first.ID].TurnTerminal.TurnID != "turn-1" || byID[first.ID].TurnTerminal.EndedAt.IsZero() {
		t.Fatalf("first terminal activity = %+v", byID[first.ID])
	}
	if byID[second.ID].EventCount != 1 || !byID[second.ID].Completed || byID[second.ID].TurnTerminal == nil || byID[second.ID].TurnTerminal.Status != "failed" {
		t.Fatalf("second activity = %+v", byID[second.ID])
	}
	if !byID[third.ID].Completed || byID[third.ID].TurnTerminal == nil || byID[third.ID].TurnTerminal.Status != "cancelled" {
		t.Fatalf("third activity = %+v", byID[third.ID])
	}
	if byID[fourth.ID].EventCount != 2 || byID[fourth.ID].Completed || byID[fourth.ID].TurnID != "turn-4b" || byID[fourth.ID].TurnTerminal != nil {
		t.Fatalf("fourth activity = %+v", byID[fourth.ID])
	}
}

func TestActivityEventClassification(t *testing.T) {
	tests := []struct {
		name  string
		event session.Event
		want  bool
	}{
		{name: "turn start", event: session.Event{Type: "turn.started", TurnID: "turn-1"}, want: true},
		{name: "assistant output", event: session.Event{Type: "message.assistant.delta", TurnID: "turn-1"}, want: true},
		{name: "reasoning", event: session.Event{Type: "message.reasoning.delta", TurnID: "turn-1"}, want: true},
		{name: "tool", event: session.Event{Type: "tool.event", TurnID: "turn-1"}, want: true},
		{name: "approval", event: session.Event{Type: "approval.requested", TurnID: "turn-1"}, want: true},
		{name: "turn error", event: session.Event{Type: "provider.error", TurnID: "turn-1"}, want: true},
		{name: "terminal", event: session.Event{Type: session.EventTurnCancelled, TurnID: "turn-1"}, want: true},
		{name: "background stderr", event: session.Event{Type: "provider.stderr"}},
		{name: "raw provider notification", event: session.Event{Type: "provider.event", TurnID: "turn-1"}},
		{name: "provider metadata", event: session.Event{Type: "provider.metadata", TurnID: "turn-1"}},
		{name: "delivery bookkeeping", event: session.Event{Type: session.EventMessageDelivery, TurnID: "turn-1"}},
		{name: "session lifecycle", event: session.Event{Type: "session.state"}},
		{name: "turn event without turn", event: session.Event{Type: "tool.event"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isActivityEvent(test.event); got != test.want {
				t.Fatalf("isActivityEvent(%+v) = %v, want %v", test.event, got, test.want)
			}
		})
	}
}
