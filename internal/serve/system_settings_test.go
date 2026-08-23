package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/disksing/pua/internal/buildinfo"
)

func TestSystemSettingsReportsPUAAndAgentHubRuntimeFacts(t *testing.T) {
	statusRequests := 0
	fakeHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/status" {
			http.NotFound(w, r)
			return
		}
		statusRequests++
		writeFakeAgentHubJSON(t, w, map[string]any{
			"apiVersion": "1", "capabilities": requiredAgentHubCapabilities, "version": "hub-commit",
			"paths": map[string]string{
				"config": "/var/lib/agenthub/config.json", "sessions": "/var/lib/agenthub/sessions",
				"archive": "/var/lib/agenthub/sessions/Archive", "logs": "/var/lib/agenthub/logs",
			},
		})
	}))
	defer fakeHub.Close()

	previousBranch, previousSHA := buildinfo.Branch, buildinfo.SHA
	buildinfo.Branch, buildinfo.SHA = "task549", "pua-commit"
	t.Cleanup(func() { buildinfo.Branch, buildinfo.SHA = previousBranch, previousSHA })

	configPath := filepath.Join(t.TempDir(), "serve.json")
	s := &server{addr: "192.168.2.150:4936", config: configPath, agentHubMode: agentHubModeExternal}
	if err := s.saveConfig(config{
		Version: agentHubConfigVersion,
		Workspaces: []serveWorkspace{
			{ID: "workspace-a", Name: "Workspace A", Path: "/srv/pua/workspace-a"},
			{ID: "workspace-b", Name: "Workspace B", Path: "/srv/pua/workspace-b"},
		},
		AgentHubEndpoint: fakeHub.URL,
	}); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	s.handleSettings(recorder, httptest.NewRequest(http.MethodGet, "/api/settings/system", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET system settings returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var response systemSettingsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if statusRequests != 1 {
		t.Fatalf("AgentHub status requests = %d, want 1", statusRequests)
	}
	if response.PUA.Address != "192.168.2.150" || response.PUA.Port != "4936" || response.PUA.ConfigPath != configPath {
		t.Fatalf("unexpected PUA network or config info: %+v", response.PUA)
	}
	if response.PUA.BuildBranch != "task549" || response.PUA.BuildCommit != "pua-commit" || len(response.PUA.Workspaces) != 2 {
		t.Fatalf("unexpected PUA build or Workspace info: %+v", response.PUA)
	}
	if response.AgentHub.Mode != agentHubModeExternal || response.AgentHub.Endpoint != fakeHub.URL || !response.AgentHub.Connected || !response.AgentHub.Compatible {
		t.Fatalf("unexpected AgentHub connection info: %+v", response.AgentHub)
	}
	if response.AgentHub.Address != "127.0.0.1" || response.AgentHub.Port == "" || response.AgentHub.Version != "hub-commit" {
		t.Fatalf("unexpected AgentHub network or version info: %+v", response.AgentHub)
	}
	if response.AgentHub.Paths.Config != "/var/lib/agenthub/config.json" || response.AgentHub.Paths.Sessions != "/var/lib/agenthub/sessions" || response.AgentHub.Paths.Archive != "/var/lib/agenthub/sessions/Archive" || response.AgentHub.Paths.Logs != "/var/lib/agenthub/logs" {
		t.Fatalf("unexpected AgentHub paths: %+v", response.AgentHub.Paths)
	}
}

func TestSystemSettingsKeepsPUAInfoWhenAgentHubIsUnavailable(t *testing.T) {
	fakeHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer fakeHub.Close()

	configPath := filepath.Join(t.TempDir(), "serve.json")
	s := &server{addr: "[::1]:4936", config: configPath, agentHubMode: agentHubModeExternal}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{}, AgentHubEndpoint: fakeHub.URL + "/agenthub"}); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	s.handleSystemSettings(recorder, httptest.NewRequest(http.MethodGet, "/api/settings/system", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET system settings returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var response systemSettingsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.PUA.Address != "::1" || response.PUA.Port != "4936" {
		t.Fatalf("unexpected PUA address: %+v", response.PUA)
	}
	if response.AgentHub.Connected || response.AgentHub.Compatible || response.AgentHub.Error == "" || response.AgentHub.Endpoint != fakeHub.URL+"/agenthub" {
		t.Fatalf("unexpected unavailable AgentHub info: %+v", response.AgentHub)
	}
}

func TestSystemSettingsRejectsMutation(t *testing.T) {
	s := &server{}
	recorder := httptest.NewRecorder()
	s.handleSystemSettings(recorder, httptest.NewRequest(http.MethodPost, "/api/settings/system", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST system settings returned %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
	}
}

func TestSplitEndpointUsesDefaultPorts(t *testing.T) {
	for _, test := range []struct {
		endpoint string
		host     string
		port     string
	}{
		{endpoint: "http://agenthub.local/agenthub", host: "agenthub.local", port: "80"},
		{endpoint: "https://agenthub.example/agenthub", host: "agenthub.example", port: "443"},
	} {
		host, port := splitEndpoint(test.endpoint)
		if host != test.host || port != test.port {
			t.Fatalf("splitEndpoint(%q) = %q, %q; want %q, %q", test.endpoint, host, port, test.host, test.port)
		}
	}
}
