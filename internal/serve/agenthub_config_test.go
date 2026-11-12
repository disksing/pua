package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestReadAgentHubConfigRejectsRemovedVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.json")
	if err := os.WriteFile(path, []byte(`{"version":2,"workspaces":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readAgentHubConfigFile(path); err == nil || !strings.Contains(err.Error(), "unsupported PUA serve configuration version") {
		t.Fatalf("expected removed config version to be rejected, got %v", err)
	}
}

func TestReadAgentHubConfigUpgradesVersionThreeDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.json")
	if err := os.WriteFile(path, []byte(`{"version":3,"workspaces":[],"agentHubEndpoint":"http://127.0.0.1:4646","agentHubInstanceId":"pua-old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := readAgentHubConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != 6 {
		t.Fatalf("upgraded config = %#v", cfg)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"version": 6`)) {
		t.Fatalf("version three config was not written back: %s", data)
	}
}

func TestReadAgentHubConfigDropsRemovedResourceDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.json")
	legacy := `{"version":5,"workspaces":[],"agentHubEndpoint":"http://127.0.0.1:4646","agentHubInstanceId":"pua-old","resourceDefaults":{"workspace":"fast","project":"default","task":"reasoning"}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := readAgentHubConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != 6 {
		t.Fatalf("migrated config = %#v", cfg)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"resourceDefaults"`)) {
		t.Fatalf("removed resourceDefaults survived the version 6 upgrade: %s", data)
	}
}

func TestAgentHubSettingsSaveValidatesCurrentConfig(t *testing.T) {
	var catalog agentHubCatalog
	readJSONFixture(t, "agenthub-catalog.json", &catalog)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/status":
			writeFakeAgentHubJSON(t, w, map[string]any{
				"apiVersion": "1", "capabilities": requiredAgentHubCapabilities, "version": "test",
			})
		case "/v1/agents":
			writeFakeAgentHubJSON(t, w, catalog)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	path := filepath.Join(t.TempDir(), "serve.json")
	server := &server{config: path}
	request := httptest.NewRequest(http.MethodPut, "/api/settings/agenthub", strings.NewReader(`{
		"endpoint":`+strconv.Quote(fake.URL)+`,
		"agentProfiles":[
			{"key":"default","agentName":"kimi-k3"},
			{"key":"reasoning","agentName":"gpt-5.6-sol"},
			{"key":"codex","description":"deep","agentName":"gpt-5.6-sol"},
			{"key":"fast","agentName":"gpt-5.3-codex-spark"}
		]
	}`))
	recorder := httptest.NewRecorder()
	server.handleAgentHubSettings(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("save returned %d: %s", recorder.Code, recorder.Body.String())
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(saved, []byte(`"version": 6`)) || !bytes.Contains(saved, []byte(`"agentHubInstanceId"`)) || bytes.Contains(saved, []byte(`"resourceDefaults"`)) {
		t.Fatalf("unexpected persisted config: %s", saved)
	}
	if bytes.Contains(saved, []byte(`"agentProviders"`)) || bytes.Contains(saved, []byte(`"defaultChatAgentId"`)) {
		t.Fatalf("persisted config contains removed fields: %s", saved)
	}
	var savedConfig agentHubServeConfig
	if err := json.Unmarshal(saved, &savedConfig); err != nil {
		t.Fatal(err)
	}
	if _, ok := findAgentHubProfileRoute(savedConfig.AgentProfiles, "scheduler"); ok {
		t.Fatalf("save synthesized a scheduler profile: %+v", savedConfig.AgentProfiles)
	}
	usesAgentHub, err := server.validatePersistedAgentHubConfig(context.Background())
	if err != nil || !usesAgentHub {
		t.Fatalf("startup validation: uses=%v err=%v", usesAgentHub, err)
	}
}

func TestAgentHubSettingsRoundTripsAgentHubConfigAndProviderCommand(t *testing.T) {
	var catalog agentHubCatalog
	readJSONFixture(t, "agenthub-catalog.json", &catalog)
	configured := agentHubConfiguredConfig{
		Version:        1,
		AgentProviders: []agentHubConfiguredProvider{{ID: "codex", Name: "Codex", Type: "codex", Command: "codex"}},
		Agents:         []agentHubConfiguredAgent{{Name: "Default", ProviderID: "codex", Options: map[string]string{"model": "gpt-test"}, Environment: map[string]string{"MODE": "test"}}},
		OnWatch:        agentHubConfiguredOnWatch{ServerURL: "http://127.0.0.1:9211", AuthMode: "trusted_proxy", RefreshIntervalSeconds: 60},
	}
	var saved agentHubConfiguredConfig
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/status":
			writeFakeAgentHubJSON(t, w, map[string]any{"apiVersion": "1", "capabilities": requiredAgentHubCapabilities, "version": "test"})
		case "/v1/agents":
			writeFakeAgentHubJSON(t, w, catalog)
		case "/v1/config":
			if r.Method == http.MethodGet {
				writeFakeAgentHubJSON(t, w, map[string]any{"config": configured})
				return
			}
			var envelope struct {
				Config agentHubConfiguredConfig `json:"config"`
			}
			if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
				t.Errorf("decode AgentHub config: %v", err)
			}
			saved = envelope.Config
			configured = saved
			writeFakeAgentHubJSON(t, w, map[string]any{"config": saved})
		case "/v1/config/providers/codex":
			var request struct {
				Command string `json:"command"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode provider command: %v", err)
			}
			configured.AgentProviders[0].Command = request.Command
			writeFakeAgentHubJSON(t, w, map[string]any{"provider": configured.AgentProviders[0]})
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()

	path := filepath.Join(t.TempDir(), "serve.json")
	server := &server{config: path}
	request := httptest.NewRequest(http.MethodPut, "/api/settings/agenthub", strings.NewReader(`{
		"endpoint":`+strconv.Quote(fake.URL)+`,
		"agentProfiles":[],
		"agentProviders":[{"id":"codex","name":"Codex","type":"codex","command":"/opt/homebrew/bin/codex"}],
		"agents":[{"name":"Worker","providerId":"codex","options":{"model":"gpt-worker"},"environment":{"MODE":"ci"}}]
	}`))
	recorder := httptest.NewRecorder()
	server.handleAgentHubSettings(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("save returned %d: %s", recorder.Code, recorder.Body.String())
	}

	var response agentHubSettingsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.AgentConfig.Agents) != 1 || response.AgentConfig.Agents[0].Name != "Worker" {
		t.Fatalf("save response did not project AgentHub config: %+v", response.AgentConfig)
	}
	if len(saved.Agents) != 1 || saved.Agents[0].Name != "Worker" || saved.AgentProviders[0].Command != "/opt/homebrew/bin/codex" {
		t.Fatalf("PUA did not save AgentHub config through the API: %+v", saved)
	}

	command := httptest.NewRequest(http.MethodPut, "/api/settings/agenthub/providers/codex", strings.NewReader(`{"command":"/usr/local/bin/codex"}`))
	commandRecorder := httptest.NewRecorder()
	server.handleSettings(commandRecorder, command)
	if commandRecorder.Code != http.StatusOK {
		t.Fatalf("provider command returned %d: %s", commandRecorder.Code, commandRecorder.Body.String())
	}
	var commandResponse struct {
		Provider agentHubConfiguredProvider `json:"provider"`
	}
	if err := json.Unmarshal(commandRecorder.Body.Bytes(), &commandResponse); err != nil {
		t.Fatal(err)
	}
	if commandResponse.Provider.ID != "codex" || commandResponse.Provider.Command != "/usr/local/bin/codex" {
		t.Fatalf("unexpected provider command response: %+v", commandResponse.Provider)
	}
}

func TestAgentHubSettingsSaveAllowsUnavailableProfileTarget(t *testing.T) {
	var catalog agentHubCatalog
	readJSONFixture(t, "agenthub-catalog.json", &catalog)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/status":
			writeFakeAgentHubJSON(t, w, map[string]any{
				"apiVersion": "1", "capabilities": requiredAgentHubCapabilities, "version": "test",
			})
		case "/v1/agents":
			catalog.Agents[0].Available = false
			catalog.Agents[0].UnavailableReason = "provider unavailable"
			writeFakeAgentHubJSON(t, w, catalog)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	path := filepath.Join(t.TempDir(), "serve.json")
	server := &server{config: path}
	request := httptest.NewRequest(http.MethodPut, "/api/settings/agenthub", strings.NewReader(`{
		"endpoint":`+strconv.Quote(fake.URL)+`,
		"agentProfiles":[
			{"key":"default","agentName":"missing-default-agent"},
			{"key":"fast","agentName":"disabled-agent"},
			{"key":"reasoning","agentName":"missing-reasoning-agent"},
			{"key":"scheduler","agentName":"missing-scheduler-agent"}
		]
	}`))
	recorder := httptest.NewRecorder()
	server.handleAgentHubSettings(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("save rejected unavailable targets: %d: %s", recorder.Code, recorder.Body.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved agentHubServeConfig
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	for _, profile := range []struct{ key, target string }{
		{key: "default", target: "missing-default-agent"},
		{key: "fast", target: "disabled-agent"},
		{key: "reasoning", target: "missing-reasoning-agent"},
		{key: "scheduler", target: "missing-scheduler-agent"},
	} {
		if got := configuredAgentHubProfileTarget(saved.AgentProfiles, profile.key); got != profile.target {
			t.Fatalf("saved target for %s = %q, want %q: %+v", profile.key, got, profile.target, saved.AgentProfiles)
		}
	}
}

func TestAgentHubSettingsReadDoesNotSynthesizeScheduler(t *testing.T) {
	var catalog agentHubCatalog
	readJSONFixture(t, "agenthub-catalog.json", &catalog)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/status":
			writeFakeAgentHubJSON(t, w, map[string]any{
				"apiVersion": "1", "capabilities": requiredAgentHubCapabilities, "version": "test",
			})
		case "/v1/agents":
			writeFakeAgentHubJSON(t, w, catalog)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	path := filepath.Join(t.TempDir(), "serve.json")
	legacy := agentHubServeConfig{
		Version: agentHubConfigVersion, AgentHubEndpoint: fake.URL,
		AgentHubInstanceID: "stable-id",
		AgentProfiles: []agentHubProfileRoute{
			{Key: "default", AgentName: "kimi-k3"},
			{Key: "fast", AgentName: "gpt-5.3-codex-spark"},
			{Key: "reasoning", AgentName: "gpt-5.6-sol"},
		},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	server := &server{config: path}
	response, err := server.readAgentHubSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findAgentHubProfileRoute(response.Config.AgentProfiles, "scheduler"); ok {
		t.Fatalf("settings response synthesized scheduler: %+v", response.Config.AgentProfiles)
	}
	persisted, err := readAgentHubConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findAgentHubProfileRoute(persisted.AgentProfiles, "scheduler"); ok {
		t.Fatalf("settings normalization persisted scheduler: %+v", persisted.AgentProfiles)
	}
	reloaded, err := server.readAgentHubSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findAgentHubProfileRoute(reloaded.Config.AgentProfiles, "scheduler"); ok {
		t.Fatalf("settings reload synthesized scheduler: %+v", reloaded.Config.AgentProfiles)
	}
}

func TestAgentHubGenerationProjectionSchemaIgnoresUnknownOldFields(t *testing.T) {
	data, err := json.Marshal(generationRecord{
		ID: "gen-1", WorkspaceID: "workspace-1", AgentHubSessionID: "ses_1",
		AgentHubAgentName: "gpt-5.6-sol",
		SourceExternalID:  "workspace-1/run-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"agentHubSessionId":"ses_1"`,
		`"agentHubAgentName":"gpt-5.6-sol"`,
		`"sourceExternalId":"workspace-1/run-1"`,
	} {
		if !bytes.Contains(data, []byte(field)) {
			t.Fatalf("run projection is missing %s: %s", field, data)
		}
	}
	var current generationRecord
	if err := json.Unmarshal([]byte(`{"id":"old","workspaceId":"workspace-1","providerSessionId":"thread-1","agentHubEventCursor":42,"status":"stopped"}`), &current); err != nil {
		t.Fatal(err)
	}
	if current.ID != "old" || current.AgentHubSessionID != "" {
		t.Fatalf("unknown old run fields changed current projection: %+v", current)
	}
}

func TestAgentHubConfigEndpointAndValidation(t *testing.T) {
	endpoint, err := effectiveAgentHubEndpoint("http://configured.test:4646/agenthub/")
	if err != nil || endpoint != "http://configured.test:4646/agenthub" {
		t.Fatalf("configured endpoint: %q, %v", endpoint, err)
	}
	var catalog agentHubCatalog
	readJSONFixture(t, "agenthub-catalog.json", &catalog)
	cfg, err := normalizeAgentHubConfig(agentHubServeConfig{
		AgentHubEndpoint:   defaultAgentHubEndpoint,
		AgentHubInstanceID: "stable-id",
		AgentProfiles: []agentHubProfileRoute{
			{Key: " DEFAULT ", AgentName: "Gpt-5.6-Sol"},
			{Key: " DEEP ", AgentName: "gpt-5.6-sol"},
		},
	}, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != agentHubConfigVersion || cfg.AgentHubInstanceID != "stable-id" || configuredAgentHubProfileTarget(cfg.AgentProfiles, "default") != "gpt-5.6-sol" {
		t.Fatalf("unexpected normalized config: %+v", cfg)
	}
	if len(cfg.AgentProfiles) != len(systemAgentProfileDefinitions)+1 || configuredAgentHubProfileTarget(cfg.AgentProfiles, "deep") != "gpt-5.6-sol" {
		t.Fatalf("unexpected normalized profile routes: %+v", cfg.AgentProfiles)
	}
	if _, ok := findAgentHubProfileRoute(cfg.AgentProfiles, "scheduler"); ok {
		t.Fatalf("normalization synthesized scheduler: %+v", cfg.AgentProfiles)
	}
	for index := range cfg.AgentProfiles {
		if cfg.AgentProfiles[index].Key == "deep" {
			cfg.AgentProfiles[index].AgentName = "missing"
		}
	}
	cfg, err = normalizeAgentHubConfig(cfg, catalog)
	if err != nil || configuredAgentHubProfileTarget(cfg.AgentProfiles, "deep") != "missing" {
		t.Fatalf("expected unavailable route to be preserved, got cfg=%+v err=%v", cfg, err)
	}
}

func TestSystemAgentProfilesAreFixedAndReserved(t *testing.T) {
	var catalog agentHubCatalog
	readJSONFixture(t, "agenthub-catalog.json", &catalog)
	cfg, err := normalizeAgentHubConfig(agentHubServeConfig{
		AgentHubEndpoint:   defaultAgentHubEndpoint,
		AgentHubInstanceID: "stable-id",
		AgentProfiles: []agentHubProfileRoute{
			{Key: "default", Description: "user override", AgentName: "gpt-5.6-sol"},
			{Key: "DEFAULT", Description: "conflicting user profile", AgentName: "kimi-k3"},
			{Key: "fast", Description: "user override", AgentName: "gpt-5.3-codex-spark"},
			{Key: "SCHEDULER", Description: "user override", AgentName: "grok-4.5"},
			{Key: "custom", Description: "custom route", AgentName: "missing-agent"},
		},
	}, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AgentProfiles) != len(systemAgentProfileDefinitions)+3 {
		t.Fatalf("unexpected profile count: %+v", cfg.AgentProfiles)
	}
	for _, definition := range systemAgentProfileDefinitions {
		route, ok := findAgentHubProfileRoute(cfg.AgentProfiles, definition.Key)
		if !ok || route.Description != definition.Description {
			t.Fatalf("system profile %s was not fixed: %+v", definition.Key, cfg.AgentProfiles)
		}
	}
	custom, ok := findAgentHubProfileRoute(cfg.AgentProfiles, "custom")
	if !ok || custom.AgentName != "missing-agent" {
		t.Fatalf("unavailable custom target was not preserved: %+v", cfg.AgentProfiles)
	}
	if got := configuredAgentHubProfileTarget(cfg.AgentProfiles, "scheduler"); got != "grok-4.5" {
		t.Fatalf("ordinary custom scheduler target was not preserved: %q (%+v)", got, cfg.AgentProfiles)
	}
	scheduler, ok := findAgentHubProfileRoute(cfg.AgentProfiles, "scheduler")
	if !ok || scheduler.Description != "user override" {
		t.Fatalf("scheduler should be normalized as an ordinary custom profile: %+v", cfg.AgentProfiles)
	}
}

func TestNoSchedulerProfileIsSynthesized(t *testing.T) {
	var catalog agentHubCatalog
	readJSONFixture(t, "agenthub-catalog.json", &catalog)
	cfg, err := normalizeAgentHubConfig(agentHubServeConfig{
		AgentHubEndpoint: defaultAgentHubEndpoint, AgentHubInstanceID: "stable-id",
	}, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findAgentHubProfileRoute(cfg.AgentProfiles, "scheduler"); ok {
		t.Fatalf("scheduler was synthesized: %+v", cfg.AgentProfiles)
	}
}

func TestLoadConfigDoesNotPersistSchedulerMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.json")
	legacy := agentHubServeConfig{
		Version: agentHubConfigVersion, AgentHubEndpoint: defaultAgentHubEndpoint,
		AgentHubInstanceID: "stable-id",
		AgentProfiles: []agentHubProfileRoute{
			{Key: "default", AgentName: "default-agent"},
			{Key: "fast", AgentName: "fast-agent"},
			{Key: "reasoning", AgentName: "reasoning-agent"},
		},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &server{config: path}
	cfg, err := srv.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := configuredAgentProfileName(cfg.AgentProfiles, "scheduler"); got != "" {
		t.Fatalf("load synthesized scheduler target %q: %+v", got, cfg.AgentProfiles)
	}
	persisted, err := readAgentHubConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findAgentHubProfileRoute(persisted.AgentProfiles, "scheduler"); ok {
		t.Fatalf("load persisted scheduler: %+v", persisted.AgentProfiles)
	}
	reloaded, err := (&server{config: path}).loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := configuredAgentProfileName(reloaded.AgentProfiles, "scheduler"); got != "" {
		t.Fatalf("reload synthesized scheduler target %q: %+v", got, reloaded.AgentProfiles)
	}
}

func configuredAgentHubProfileTarget(routes []agentHubProfileRoute, key string) string {
	route, ok := findAgentHubProfileRoute(routes, key)
	if !ok {
		return ""
	}
	return route.AgentName
}

func findAgentHubProfileRoute(routes []agentHubProfileRoute, key string) (agentHubProfileRoute, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, route := range routes {
		if strings.ToLower(strings.TrimSpace(route.Key)) == key {
			return route, true
		}
	}
	return agentHubProfileRoute{}, false
}

func readJSONFixture(t *testing.T, name string, output any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, output); err != nil {
		t.Fatal(err)
	}
}
