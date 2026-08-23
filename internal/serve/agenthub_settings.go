package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
)

type agentHubSettingsResponse struct {
	Mode               string                 `json:"mode"`
	Config             agentHubServeConfig    `json:"config"`
	AgentConfig        agentHubSettingsConfig `json:"agentConfig"`
	ConfiguredEndpoint string                 `json:"configuredEndpoint"`
	EffectiveEndpoint  string                 `json:"effectiveEndpoint"`
	Connected          bool                   `json:"connected"`
	Compatible         bool                   `json:"compatible"`
	Status             *agentHubStatus        `json:"status,omitempty"`
	Catalog            agentHubCatalog        `json:"catalog"`
	Revision           string                 `json:"revision,omitempty"`
	Error              string                 `json:"error,omitempty"`
}

type agentHubSettingsConfig struct {
	AgentProviders []agentHubConfiguredProvider `json:"providers"`
	Agents         []agentHubConfiguredAgent    `json:"agents"`
}

type updateAgentHubSettingsRequest struct {
	Endpoint       string                       `json:"endpoint"`
	AgentProfiles  []agentHubProfileRoute       `json:"agentProfiles"`
	AgentProviders []agentHubConfiguredProvider `json:"agentProviders"`
	Agents         []agentHubConfiguredAgent    `json:"agents"`
}

func (s *server) handleAgentHubSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		response, err := s.readAgentHubSettings(r.Context())
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, response)
	case http.MethodPut:
		var request updateAgentHubSettingsRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		response, err := s.saveAgentHubSettings(r.Context(), request)
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, response)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *server) readAgentHubSettings(ctx context.Context) (agentHubSettingsResponse, error) {
	cfg, err := readAgentHubConfigFile(s.config)
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	configChanged := false
	structuralProfiles, err := normalizeAgentHubProfileRoutes(cfg.AgentProfiles, agentHubCatalog{})
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	if !reflect.DeepEqual(cfg.AgentProfiles, structuralProfiles) {
		cfg.AgentProfiles = structuralProfiles
		if _, statErr := os.Stat(s.config); statErr == nil {
			if err := writeAgentHubConfigFile(s.config, cfg); err != nil {
				return agentHubSettingsResponse{}, err
			}
			configChanged = true
		} else if !os.IsNotExist(statErr) {
			return agentHubSettingsResponse{}, statErr
		}
	}
	persistedConfig := cfg
	configured := cfg.AgentHubEndpoint
	if configured == "" {
		configured = defaultAgentHubEndpoint
	}
	effective, err := s.effectiveAgentHubEndpoint(configured)
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	if s.agentHubMode != "" {
		configured = effective
		cfg.AgentHubEndpoint = effective
	}
	response := agentHubSettingsResponse{
		Mode:               s.agentHubMode,
		Config:             cfg,
		ConfiguredEndpoint: configured,
		EffectiveEndpoint:  effective,
		Catalog:            agentHubCatalog{Providers: []agentHubProvider{}, Agents: []agentHubAgent{}, Probes: []agentHubProbe{}},
	}
	client, err := newAgentHubClient(effective, nil)
	if err != nil {
		response.Error = err.Error()
		return response, nil
	}
	status, err := client.Status(ctx)
	if err != nil {
		response.Error = err.Error()
		return response, nil
	}
	response.Connected = true
	response.Status = &status
	if err := validateAgentHubStatus(status); err != nil {
		response.Error = err.Error()
		return response, nil
	}
	response.Compatible = true
	catalog, err := client.Agents(ctx)
	if err != nil {
		response.Error = err.Error()
		return response, nil
	}
	response.Catalog = catalog
	configuredAgentHub, err := client.Config(ctx)
	if err != nil {
		response.Error = err.Error()
		return response, nil
	}
	response.AgentConfig = projectAgentHubSettingsConfig(configuredAgentHub)
	cfg, err = normalizeAgentHubConfig(cfg, catalog)
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	if !reflect.DeepEqual(persistedConfig, cfg) {
		if err := writeAgentHubConfigFile(s.config, cfg); err != nil {
			return agentHubSettingsResponse{}, err
		}
		configChanged = true
	}
	if configChanged {
		s.agents.requestReconcile(reconcileScheduler)
	}
	response.Config = cfg
	response.Revision = s.settingsRevisionOrEmpty()
	return response, nil
}

func (s *server) saveAgentHubSettings(ctx context.Context, request updateAgentHubSettingsRequest) (agentHubSettingsResponse, error) {
	cfg, err := readAgentHubConfigFile(s.config)
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	persistedConfig := cfg
	configured, err := normalizeAgentHubEndpoint(request.Endpoint)
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	effective, err := s.effectiveAgentHubEndpoint(configured)
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	if s.agentHubMode != "" {
		configured = effective
	}
	client, err := newAgentHubClient(effective, nil)
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	status, err := client.Status(ctx)
	if err != nil {
		return agentHubSettingsResponse{}, fmt.Errorf("validate AgentHub status: %w", err)
	}
	if err := validateAgentHubStatus(status); err != nil {
		return agentHubSettingsResponse{}, err
	}
	catalog, err := client.Agents(ctx)
	if err != nil {
		return agentHubSettingsResponse{}, fmt.Errorf("validate AgentHub catalog: %w", err)
	}
	var configuredAgentHub agentHubConfiguredConfig
	agentHubConfigChanged := false
	if request.AgentProviders != nil || request.Agents != nil {
		configuredAgentHub, err = client.Config(ctx)
		if err != nil {
			return agentHubSettingsResponse{}, fmt.Errorf("read AgentHub config: %w", err)
		}
		persistedAgentHub := configuredAgentHub
		if request.AgentProviders != nil {
			configuredAgentHub.AgentProviders = request.AgentProviders
		}
		if request.Agents != nil {
			configuredAgentHub.Agents = request.Agents
		}
		if !reflect.DeepEqual(persistedAgentHub, configuredAgentHub) {
			configuredAgentHub, err = client.SaveConfig(ctx, configuredAgentHub)
			if err != nil {
				return agentHubSettingsResponse{}, fmt.Errorf("save AgentHub config: %w", err)
			}
			agentHubConfigChanged = !reflect.DeepEqual(persistedAgentHub, configuredAgentHub)
		}
	}
	cfg.AgentHubEndpoint = configured
	cfg.AgentProfiles = request.AgentProfiles
	cfg, err = normalizeAgentHubConfig(cfg, catalog)
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	if err := writeAgentHubConfigFile(s.config, cfg); err != nil {
		return agentHubSettingsResponse{}, err
	}
	if agentHubConfigChanged || !reflect.DeepEqual(persistedConfig, cfg) {
		s.agents.requestReconcile(reconcileScheduler)
	}
	return agentHubSettingsResponse{
		Mode:               s.agentHubMode,
		Config:             cfg,
		AgentConfig:        projectAgentHubSettingsConfig(configuredAgentHub),
		ConfiguredEndpoint: configured,
		EffectiveEndpoint:  effective,
		Connected:          true,
		Compatible:         true,
		Status:             &status,
		Catalog:            catalog,
		Revision:           s.settingsRevisionOrEmpty(),
	}, nil
}

func projectAgentHubSettingsConfig(config agentHubConfiguredConfig) agentHubSettingsConfig {
	return agentHubSettingsConfig{
		AgentProviders: config.AgentProviders,
		Agents:         config.Agents,
	}
}

func (s *server) handleAgentHubProviderSettings(w http.ResponseWriter, r *http.Request, providerID string) {
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		Enabled *bool `json:"enabled"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Enabled == nil {
		if err == nil {
			err = fmt.Errorf("enabled is required")
		}
		writeError(w, err, http.StatusBadRequest)
		return
	}
	cfg, err := readAgentHubConfigFile(s.config)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	configured := cfg.AgentHubEndpoint
	if configured == "" {
		configured = defaultAgentHubEndpoint
	}
	effective, err := s.effectiveAgentHubEndpoint(configured)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	client, err := newAgentHubClient(effective, nil)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	status, err := client.Status(r.Context())
	if err != nil {
		writeError(w, fmt.Errorf("validate AgentHub status: %w", err), http.StatusBadRequest)
		return
	}
	if err := validateAgentHubStatus(status); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	configuredAgentHub, err := client.Config(r.Context())
	if err != nil {
		writeError(w, fmt.Errorf("read AgentHub config: %w", err), http.StatusBadRequest)
		return
	}
	provider := agentHubConfiguredProvider{}
	changed := true
	for _, configuredProvider := range configuredAgentHub.AgentProviders {
		if strings.TrimSpace(configuredProvider.ID) == strings.TrimSpace(providerID) {
			provider = configuredProvider
			changed = configuredProvider.Enabled != *request.Enabled
			break
		}
	}
	if changed {
		provider, err = client.SetProviderEnabled(r.Context(), providerID, *request.Enabled)
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		s.agents.requestReconcile(reconcileScheduler)
	}
	writeJSON(w, struct {
		Provider agentHubConfiguredProvider `json:"provider"`
	}{Provider: provider})
}

// settingsRevisionOrEmpty best-effort computes the settings revision after a
// settings read or save; a revision failure must not fail the settings flow
// itself, so the frontend simply falls back to polling.
func (s *server) settingsRevisionOrEmpty() string {
	revision, err := s.currentSettingsRevision()
	if err != nil {
		return ""
	}
	return revision
}

func writeAgentHubConfigFile(path string, cfg agentHubServeConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteConfig(path, append(data, '\n'))
}

func readAgentHubConfigFile(path string) (agentHubServeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return agentHubServeConfig{
				Version: agentHubConfigVersion, Workspaces: []serveWorkspace{},
				AgentHubEndpoint: defaultAgentHubEndpoint,
			}, nil
		}
		return agentHubServeConfig{}, err
	}
	var version struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &version); err != nil {
		return agentHubServeConfig{}, err
	}
	if version.Version < 3 {
		return agentHubServeConfig{}, fmt.Errorf("unsupported PUA serve configuration version %d; migrate the configuration before starting pua serve", version.Version)
	}
	needsUpgrade := version.Version != agentHubConfigVersion
	var cfg agentHubServeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return agentHubServeConfig{}, err
	}
	cfg.Version = agentHubConfigVersion
	if needsUpgrade {
		if err := writeAgentHubConfigFile(path, cfg); err != nil {
			return agentHubServeConfig{}, err
		}
	}
	return cfg, nil
}

func (s *server) validatePersistedAgentHubConfig(ctx context.Context) (bool, error) {
	cfg, err := readAgentHubConfigFile(s.config)
	if err != nil {
		return false, err
	}
	if cfg.AgentHubInstanceID == "" {
		return false, nil
	}
	effective, err := s.effectiveAgentHubEndpoint(cfg.AgentHubEndpoint)
	if err != nil {
		return true, err
	}
	client, err := newAgentHubClient(effective, nil)
	if err != nil {
		return true, err
	}
	status, err := client.Status(ctx)
	if err != nil {
		return true, fmt.Errorf("connect to AgentHub: %w", err)
	}
	if err := validateAgentHubStatus(status); err != nil {
		return true, err
	}
	catalog, err := client.Agents(ctx)
	if err != nil {
		return true, err
	}
	normalized, err := normalizeAgentHubConfig(cfg, catalog)
	if err != nil {
		return true, err
	}
	if !reflect.DeepEqual(cfg, normalized) {
		if err := writeAgentHubConfigFile(s.config, normalized); err != nil {
			return true, err
		}
	}
	return true, nil
}
