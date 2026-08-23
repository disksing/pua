package serve

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/disksing/pua/internal/buildinfo"
)

type systemSettingsResponse struct {
	PUA      puaSystemInfo      `json:"pua"`
	AgentHub agentHubSystemInfo `json:"agentHub"`
}

type puaSystemInfo struct {
	Address     string                `json:"address"`
	Port        string                `json:"port"`
	ConfigPath  string                `json:"configPath"`
	Workspaces  []systemWorkspaceInfo `json:"workspaces"`
	BuildBranch string                `json:"buildBranch"`
	BuildCommit string                `json:"buildCommit"`
}

type systemWorkspaceInfo struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type agentHubSystemInfo struct {
	Mode       string        `json:"mode"`
	Address    string        `json:"address"`
	Port       string        `json:"port"`
	Endpoint   string        `json:"endpoint"`
	Connected  bool          `json:"connected"`
	Compatible bool          `json:"compatible"`
	Version    string        `json:"version"`
	Paths      agentHubPaths `json:"paths"`
	Error      string        `json:"error"`
}

func (s *server) handleSystemSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	response, err := s.readSystemSettings(r.Context())
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, response)
}

func (s *server) readSystemSettings(ctx context.Context) (systemSettingsResponse, error) {
	cfg, err := s.loadConfig()
	if err != nil {
		return systemSettingsResponse{}, err
	}
	address, port := splitAddress(s.addr)
	build := buildinfo.Current()
	response := systemSettingsResponse{
		PUA: puaSystemInfo{
			Address: address, Port: port, ConfigPath: s.config,
			BuildBranch: build.Branch, BuildCommit: build.SHA,
			Workspaces: make([]systemWorkspaceInfo, 0, len(cfg.Workspaces)),
		},
	}
	for _, workspace := range resolvedWorkspaceSummaries(cfg.Workspaces) {
		response.PUA.Workspaces = append(response.PUA.Workspaces, systemWorkspaceInfo{Name: workspace.Name, Path: workspace.Path})
	}

	configured := cfg.AgentHubEndpoint
	if configured == "" {
		configured = defaultAgentHubEndpoint
	}
	effective, err := s.effectiveAgentHubEndpoint(configured)
	if err != nil {
		return systemSettingsResponse{}, err
	}
	hubAddress, hubPort := splitEndpoint(effective)
	mode := strings.TrimSpace(s.agentHubMode)
	if mode == "" {
		mode = agentHubModeEmbedded
	}
	response.AgentHub = agentHubSystemInfo{
		Mode: mode, Address: hubAddress, Port: hubPort, Endpoint: effective,
	}
	client, err := newAgentHubClient(effective, nil)
	if err != nil {
		response.AgentHub.Error = err.Error()
		return response, nil
	}
	status, err := client.Status(ctx)
	if err != nil {
		response.AgentHub.Error = err.Error()
		return response, nil
	}
	response.AgentHub.Connected = true
	response.AgentHub.Version = status.Version
	response.AgentHub.Paths = status.Paths
	if err := validateAgentHubStatus(status); err != nil {
		response.AgentHub.Error = err.Error()
		return response, nil
	}
	response.AgentHub.Compatible = true
	return response, nil
}

func splitAddress(address string) (string, string) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return strings.TrimSpace(address), ""
	}
	return host, port
}

func splitEndpoint(endpoint string) (string, string) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", ""
	}
	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return parsed.Hostname(), port
}
