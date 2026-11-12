package serve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

type settingsResponse struct {
	Workspaces []serveWorkspace `json:"workspaces"`
	ActiveID   string           `json:"activeId,omitempty"`
	Revision   string           `json:"revision"`
}

const agentOptionModel = "model"

func (s *server) handleSettings(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/settings"), "/")
	if path == "" {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.writeSettings(w)
		return
	}
	if path == "agenthub" {
		s.handleAgentHubSettings(w, r)
		return
	}
	if path == "system" {
		s.handleSystemSettings(w, r)
		return
	}
	if strings.HasPrefix(path, "agenthub/providers/") {
		providerID, err := url.PathUnescape(strings.TrimPrefix(path, "agenthub/providers/"))
		if err != nil || strings.TrimSpace(providerID) == "" || strings.Contains(providerID, "/") {
			http.NotFound(w, r)
			return
		}
		s.handleAgentHubProviderSettings(w, r, providerID)
		return
	}
	if path == "revision" {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.writeSettingsRevision(w)
		return
	}
	http.NotFound(w, r)
}

func (s *server) writeSettings(w http.ResponseWriter) {
	cfg, err := s.loadConfig()
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	workspaces := resolvedWorkspaceSummaries(cfg.Workspaces)
	writeJSON(w, settingsResponse{Workspaces: workspaces, ActiveID: cfg.ActiveID, Revision: settingsRevision(cfg, workspaces)})
}

// settingsRevision computes a cheap content hash over the parts of the serve
// configuration the Web UI renders: workspaces (with resolved display names),
// the active Workspace, the AgentHub endpoint, and the Agent Profile routes.
// The frontend polls /api/settings/revision during auto refresh and only
// refetches the full settings payloads when this value changes, so edits made
// from another browser tab, another client, or the CLI propagate without a
// page reload.
func settingsRevision(cfg config, workspaces []serveWorkspace) string {
	payload := struct {
		ActiveID   string              `json:"activeId,omitempty"`
		Workspaces []serveWorkspace    `json:"workspaces"`
		Endpoint   string              `json:"agentHubEndpoint,omitempty"`
		Profiles   []agentProfileRoute `json:"agentProfiles,omitempty"`
	}{
		ActiveID:   cfg.ActiveID,
		Workspaces: workspaces,
		Endpoint:   cfg.AgentHubEndpoint,
		Profiles:   cfg.AgentProfiles,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func (s *server) currentSettingsRevision() (string, error) {
	cfg, err := s.loadConfig()
	if err != nil {
		return "", err
	}
	return settingsRevision(cfg, resolvedWorkspaceSummaries(cfg.Workspaces)), nil
}

func (s *server) writeSettingsRevision(w http.ResponseWriter) {
	revision, err := s.currentSettingsRevision()
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"revision": revision})
}

func findAgentProfileRoute(routes []agentProfileRoute, key string) (agentProfileRoute, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, route := range routes {
		if strings.ToLower(strings.TrimSpace(route.Key)) == key {
			return route, true
		}
	}
	return agentProfileRoute{}, false
}

func configuredAgentProfileName(routes []agentProfileRoute, key string) string {
	route, ok := findAgentProfileRoute(routes, key)
	if !ok {
		return ""
	}
	return strings.TrimSpace(route.AgentName)
}
