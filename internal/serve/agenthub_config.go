package serve

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

const (
	agentHubConfigVersion = 6
	agentHubSourceApp     = "pua"
)

type systemAgentProfileDefinition struct {
	Key         string
	Description string
}

var systemAgentProfileDefinitions = []systemAgentProfileDefinition{
	{Key: "default", Description: "Balanced, recommended agent"},
}

type agentHubServeConfig struct {
	Version            int                    `json:"version"`
	ActiveID           string                 `json:"activeId,omitempty"`
	Workspaces         []serveWorkspace       `json:"workspaces"`
	AgentHubEndpoint   string                 `json:"agentHubEndpoint"`
	AgentHubInstanceID string                 `json:"agentHubInstanceId"`
	AgentProfiles      []agentHubProfileRoute `json:"agentProfiles,omitempty"`
}

type agentHubProfileRoute struct {
	Key         string `json:"key"`
	Description string `json:"description,omitempty"`
	AgentName   string `json:"agentName"`
}

func agentHubConfigFromServeConfig(cfg config) agentHubServeConfig {
	routes := make([]agentHubProfileRoute, 0, len(cfg.AgentProfiles))
	for _, route := range cfg.AgentProfiles {
		routes = append(routes, agentHubProfileRoute{
			Key: route.Key, Description: route.Description, AgentName: route.AgentName,
		})
	}
	return agentHubServeConfig{
		Version: cfg.Version, ActiveID: cfg.ActiveID, Workspaces: cfg.Workspaces,
		AgentHubEndpoint: cfg.AgentHubEndpoint, AgentHubInstanceID: cfg.AgentHubInstanceID,
		AgentProfiles: routes,
	}
}

func serveConfigFromAgentHubConfig(cfg agentHubServeConfig) config {
	routes := make([]agentProfileRoute, 0, len(cfg.AgentProfiles))
	for _, route := range cfg.AgentProfiles {
		routes = append(routes, agentProfileRoute{
			Key: route.Key, Description: route.Description, AgentName: route.AgentName,
		})
	}
	return config{
		Version: cfg.Version, ActiveID: cfg.ActiveID, Workspaces: cfg.Workspaces,
		AgentHubEndpoint: cfg.AgentHubEndpoint, AgentHubInstanceID: cfg.AgentHubInstanceID,
		AgentProfiles: routes,
	}
}

func applyAgentHubConfigFields(cfg *config, agentHub agentHubServeConfig) {
	cfg.AgentHubEndpoint = agentHub.AgentHubEndpoint
	cfg.AgentHubInstanceID = agentHub.AgentHubInstanceID
	cfg.AgentProfiles = serveConfigFromAgentHubConfig(agentHub).AgentProfiles
}

func sameAgentHubConfigFields(left, right agentHubServeConfig) bool {
	return left.AgentHubEndpoint == right.AgentHubEndpoint &&
		left.AgentHubInstanceID == right.AgentHubInstanceID &&
		reflect.DeepEqual(left.AgentProfiles, right.AgentProfiles)
}

func effectiveAgentHubEndpoint(configured string) (string, error) {
	return normalizeAgentHubEndpoint(configured)
}

func (s *server) effectiveAgentHubEndpoint(configured string) (string, error) {
	if s != nil && strings.TrimSpace(s.agentHubEndpoint) != "" {
		return normalizeAgentHubEndpoint(s.agentHubEndpoint)
	}
	return effectiveAgentHubEndpoint(configured)
}

func newAgentHubInstanceID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate AgentHub instance id: %w", err)
	}
	return "pua-" + hex.EncodeToString(random[:]), nil
}

func normalizeAgentHubConfig(cfg agentHubServeConfig, catalog agentHubCatalog) (agentHubServeConfig, error) {
	cfg.Version = agentHubConfigVersion
	if cfg.Workspaces == nil {
		cfg.Workspaces = []serveWorkspace{}
	}
	endpoint, err := normalizeAgentHubEndpoint(cfg.AgentHubEndpoint)
	if err != nil {
		return agentHubServeConfig{}, err
	}
	cfg.AgentHubEndpoint = endpoint
	cfg.AgentHubInstanceID = strings.TrimSpace(cfg.AgentHubInstanceID)
	if cfg.AgentHubInstanceID == "" {
		cfg.AgentHubInstanceID, err = newAgentHubInstanceID()
		if err != nil {
			return agentHubServeConfig{}, err
		}
	}
	cfg.AgentProfiles, err = normalizeAgentHubProfileRoutes(cfg.AgentProfiles, catalog)
	if err != nil {
		return agentHubServeConfig{}, err
	}
	return cfg, nil
}

func normalizeAgentHubProfileRoutes(routes []agentHubProfileRoute, catalog agentHubCatalog) ([]agentHubProfileRoute, error) {
	systemTargets := make(map[string]string, len(systemAgentProfileDefinitions))
	for _, route := range routes {
		key := strings.ToLower(strings.TrimSpace(route.Key))
		if key == "" {
			return nil, errors.New("Agent Profile key is required")
		}
		if isSystemAgentProfileKey(key) {
			if _, exists := systemTargets[key]; !exists {
				systemTargets[key] = strings.TrimSpace(route.AgentName)
			}
		}
	}

	available := availableAgentHubAgents(catalog)
	fallback := ""
	if len(available) > 0 {
		fallback = available[0].Name
	}
	normalized := make([]agentHubProfileRoute, 0, len(routes)+len(systemAgentProfileDefinitions))
	seen := make(map[string]bool, len(routes)+len(systemAgentProfileDefinitions))
	for _, definition := range systemAgentProfileDefinitions {
		agentName := strings.TrimSpace(systemTargets[definition.Key])
		if agentName == "" {
			agentName = fallback
		}
		canonicalName, err := canonicalAgentHubAgentName(agentName, catalog.Agents)
		if err != nil {
			return nil, fmt.Errorf("system Agent Profile %s: %w", definition.Key, err)
		}
		normalized = append(normalized, agentHubProfileRoute{
			Key:         definition.Key,
			Description: definition.Description,
			AgentName:   canonicalName,
		})
		seen[definition.Key] = true
	}

	for _, route := range routes {
		key := strings.ToLower(strings.TrimSpace(route.Key))
		if key == "" {
			return nil, errors.New("Agent Profile key is required")
		}
		if isSystemAgentProfileKey(key) {
			continue
		}
		if seen[key] {
			return nil, fmt.Errorf("duplicate Agent Profile key: %s", key)
		}
		if strings.TrimSpace(route.AgentName) == "" {
			return nil, fmt.Errorf("Agent Profile %s requires an AgentHub agent", key)
		}
		agentName, err := canonicalAgentHubAgentName(route.AgentName, catalog.Agents)
		if err != nil {
			return nil, fmt.Errorf("Agent Profile %s: %w", key, err)
		}
		seen[key] = true
		normalized = append(normalized, agentHubProfileRoute{
			Key:         key,
			Description: strings.Join(strings.Fields(route.Description), " "),
			AgentName:   agentName,
		})
	}
	return normalized, nil
}

func normalizeConfigAgentProfileRoutes(routes []agentProfileRoute) ([]agentProfileRoute, error) {
	hubRoutes := make([]agentHubProfileRoute, 0, len(routes))
	for _, route := range routes {
		hubRoutes = append(hubRoutes, agentHubProfileRoute{
			Key: route.Key, Description: route.Description, AgentName: route.AgentName,
		})
	}
	normalized, err := normalizeAgentHubProfileRoutes(hubRoutes, agentHubCatalog{})
	if err != nil {
		return nil, err
	}
	configRoutes := make([]agentProfileRoute, 0, len(normalized))
	for _, route := range normalized {
		configRoutes = append(configRoutes, agentProfileRoute{
			Key: route.Key, Description: route.Description, AgentName: route.AgentName,
		})
	}
	return configRoutes, nil
}

func agentProfileRoutesEqual(a, b []agentProfileRoute) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func isSystemAgentProfileKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, definition := range systemAgentProfileDefinitions {
		if definition.Key == key {
			return true
		}
	}
	return false
}

func canonicalAgentHubAgentName(name string, agents []agentHubAgent) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	var matches []string
	for _, agent := range agents {
		if strings.EqualFold(strings.TrimSpace(agent.Name), name) {
			matches = append(matches, agent.Name)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return name, nil
	}
	return "", fmt.Errorf("%q is ambiguous in the AgentHub catalog", name)
}

func availableAgentHubAgents(catalog agentHubCatalog) []agentHubAgent {
	agents := make([]agentHubAgent, 0, len(catalog.Agents))
	for _, agent := range catalog.Agents {
		if agent.Available {
			agents = append(agents, agent)
		}
	}
	return agents
}

func atomicWriteConfig(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".gui-agenthub-*.tmp")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	// Sync the directory entry as well as the file contents. Without this
	// barrier a successful return can still lose the rename after a crash,
	// allowing runtime effects that no durable configuration owns.
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
