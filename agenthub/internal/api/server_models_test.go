package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disksing/pua/agenthub/internal/config"
	"github.com/disksing/pua/agenthub/internal/provider"
	"github.com/disksing/pua/agenthub/internal/runtime"
	"github.com/disksing/pua/agenthub/internal/session"
)

// fakeModelLister implements ModelLister for handler tests: no real provider
// processes are ever spawned.
type fakeModelLister struct {
	models      []provider.Model
	err         error
	calls       atomic.Int32
	invalidates atomic.Int32
}

func (f *fakeModelLister) Models(_ context.Context, _ config.Provider) ([]provider.Model, error) {
	f.calls.Add(1)
	return f.models, f.err
}

func (f *fakeModelLister) InvalidateAll() { f.invalidates.Add(1) }

func newModelsTestServer(t *testing.T, cfg config.Config, lister ModelLister) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	store, err := session.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	manager := runtime.New(store, cfg)
	server := httptest.NewServer(New(store, "test", time.Now(), Dependencies{
		Runtime:    manager,
		ConfigPath: filepath.Join(root, "config.json"),
		Models:     lister,
	}).Handler())
	t.Cleanup(server.Close)
	return server
}

func modelsTestConfig() config.Config {
	return config.Config{
		Version: 1,
		AgentProviders: []config.Provider{
			{ID: "codex", Name: "Codex app-server", Type: "codex"},
			{ID: "kimi", Name: "Kimi Code", Type: "kimi"},
		},
		Agents: []config.Agent{{Name: "Codex", ProviderID: "codex"}},
	}
}

func getProviderModels(t *testing.T, server *httptest.Server, id string) (int, map[string]any) {
	t.Helper()
	response, err := http.Get(server.URL + "/v1/providers/" + id + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

func errorCode(body map[string]any) string {
	value, _ := body["error"].(map[string]any)
	code, _ := value["code"].(string)
	return code
}

func TestProviderModelsSuccess(t *testing.T) {
	lister := &fakeModelLister{models: []provider.Model{
		{ID: "gpt-5.6-sol", Label: "GPT-5.6-Sol", Default: true},
		{ID: "gpt-5.5", Label: "GPT-5.5"},
	}}
	server := newModelsTestServer(t, modelsTestConfig(), lister)
	status, body := getProviderModels(t, server, "codex")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	models, ok := body["models"].([]any)
	if !ok || len(models) != 2 {
		t.Fatalf("models = %v", body["models"])
	}
	first, _ := models[0].(map[string]any)
	if first["id"] != "gpt-5.6-sol" || first["label"] != "GPT-5.6-Sol" || first["default"] != true {
		t.Fatalf("first model = %v", first)
	}
	providerInfo, _ := body["provider"].(map[string]any)
	if providerInfo["id"] != "codex" || providerInfo["type"] != "codex" {
		t.Fatalf("provider = %v", providerInfo)
	}
	if lister.calls.Load() != 1 {
		t.Fatalf("lister calls = %d", lister.calls.Load())
	}
}

func TestProviderModelsEmptyListIsSuccess(t *testing.T) {
	lister := &fakeModelLister{models: []provider.Model{}}
	server := newModelsTestServer(t, modelsTestConfig(), lister)
	status, body := getProviderModels(t, server, "codex")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	models, ok := body["models"].([]any)
	if !ok || len(models) != 0 {
		t.Fatalf("models = %v, want empty array", body["models"])
	}
}

func TestProviderModelsUnknownProvider(t *testing.T) {
	server := newModelsTestServer(t, modelsTestConfig(), &fakeModelLister{})
	status, body := getProviderModels(t, server, "ghost")
	if status != http.StatusNotFound || errorCode(body) != "unknown_provider" {
		t.Fatalf("status = %d, body = %v", status, body)
	}
}

func TestProviderModelsBuiltinMissingFromConfig(t *testing.T) {
	// A built-in provider absent from the configuration falls back to its
	// canonical definition and is still enumerated.
	lister := &fakeModelLister{models: []provider.Model{{ID: "pi-1", Label: "Pi 1"}}}
	server := newModelsTestServer(t, modelsTestConfig(), lister)
	status, body := getProviderModels(t, server, "pi")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	providerInfo, _ := body["provider"].(map[string]any)
	if providerInfo["id"] != "pi" || providerInfo["type"] != "pi" {
		t.Fatalf("provider = %v", providerInfo)
	}
	if lister.calls.Load() != 1 {
		t.Fatalf("lister calls = %d", lister.calls.Load())
	}
}

func TestProviderModelsErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"timeout", &provider.ModelError{Kind: provider.ModelErrTimeout, Err: errors.New("slow")}, http.StatusGatewayTimeout, "provider_timeout"},
		{"unavailable", &provider.ModelError{Kind: provider.ModelErrUnavailable, Err: errors.New("no CLI")}, http.StatusServiceUnavailable, "provider_unavailable"},
		{"upstream", &provider.ModelError{Kind: provider.ModelErrUpstream, Err: errors.New("bad output")}, http.StatusBadGateway, "provider_error"},
		{"plain", errors.New("unexpected"), http.StatusBadGateway, "provider_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newModelsTestServer(t, modelsTestConfig(), &fakeModelLister{err: tc.err})
			status, body := getProviderModels(t, server, "codex")
			if status != tc.wantStatus || errorCode(body) != tc.wantCode {
				t.Fatalf("status = %d code = %q, want %d %q", status, errorCode(body), tc.wantStatus, tc.wantCode)
			}
		})
	}
}

func TestProviderModelsCacheInvalidatedOnConfigChanges(t *testing.T) {
	lister := &fakeModelLister{}
	server := newModelsTestServer(t, modelsTestConfig(), lister)

	// Whole-config PUT invalidates.
	body := strings.NewReader(`{"config":{"version":1,"agentProviders":[{"id":"codex","name":"Codex app-server","type":"codex"}],"agents":[{"name":"Codex","providerId":"codex"}]}}`)
	request, _ := http.NewRequest(http.MethodPut, server.URL+"/v1/config", body)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PUT /v1/config status = %d", response.StatusCode)
	}
	if lister.invalidates.Load() != 1 {
		t.Fatalf("invalidates after PUT config = %d", lister.invalidates.Load())
	}

	// A provider command update invalidates.
	body = strings.NewReader(`{"command":""}`)
	request, _ = http.NewRequest(http.MethodPut, server.URL+"/v1/config/providers/codex", body)
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PUT provider command status = %d", response.StatusCode)
	}
	if lister.invalidates.Load() != 2 {
		t.Fatalf("invalidates after provider command update = %d", lister.invalidates.Load())
	}
}
