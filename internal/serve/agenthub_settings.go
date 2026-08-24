package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	serveConfig, err := s.loadConfig()
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	cfg := agentHubConfigFromServeConfig(serveConfig)
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
	committed, err := s.transactConfig(func(latest *config) (bool, error) {
		latestAgentHub := agentHubConfigFromServeConfig(*latest)
		// A concurrent settings writer owns its newer AgentHub fields. A
		// catalog fetched for the older endpoint must never rewrite them.
		if !sameAgentHubConfigFields(latestAgentHub, persistedConfig) {
			return false, nil
		}
		candidate := latestAgentHub
		if s.agentHubMode != "" {
			candidate.AgentHubEndpoint = effective
		}
		normalized, err := normalizeAgentHubConfig(candidate, catalog)
		if err != nil {
			return false, err
		}
		changed := !sameAgentHubConfigFields(latestAgentHub, normalized)
		applyAgentHubConfigFields(latest, normalized)
		return changed, nil
	})
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	cfg = agentHubConfigFromServeConfig(committed)
	response.Config = cfg
	response.Revision = s.settingsRevisionOrEmpty()
	return response, nil
}

func (s *server) saveAgentHubSettings(ctx context.Context, request updateAgentHubSettingsRequest) (agentHubSettingsResponse, error) {
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
	if request.AgentProviders != nil || request.Agents != nil {
		configuredAgentHub, err = client.Config(ctx)
		if err != nil {
			return agentHubSettingsResponse{}, fmt.Errorf("read AgentHub config: %w", err)
		}
		if request.AgentProviders != nil {
			configuredAgentHub.AgentProviders = request.AgentProviders
		}
		if request.Agents != nil {
			configuredAgentHub.Agents = request.Agents
		}
		configuredAgentHub, err = client.SaveConfig(ctx, configuredAgentHub)
		if err != nil {
			return agentHubSettingsResponse{}, fmt.Errorf("save AgentHub config: %w", err)
		}
	}
	committed, err := s.transactConfig(func(latest *config) (bool, error) {
		candidate := agentHubConfigFromServeConfig(*latest)
		candidate.AgentHubEndpoint = configured
		candidate.AgentProfiles = request.AgentProfiles
		candidate, err = normalizeAgentHubConfig(candidate, catalog)
		if err != nil {
			return false, err
		}
		applyAgentHubConfigFields(latest, candidate)
		return true, nil
	})
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	cfg := agentHubConfigFromServeConfig(committed)
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
	cfg, err := s.loadConfig()
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
	provider, err := client.SetProviderEnabled(r.Context(), providerID, *request.Enabled)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
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

func (s *server) validatePersistedAgentHubConfig(ctx context.Context) (bool, error) {
	serveConfig, err := s.loadConfig()
	if err != nil {
		return false, err
	}
	cfg := agentHubConfigFromServeConfig(serveConfig)
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
	_, err = s.transactConfig(func(latest *config) (bool, error) {
		latestAgentHub := agentHubConfigFromServeConfig(*latest)
		if !sameAgentHubConfigFields(latestAgentHub, cfg) {
			return false, nil
		}
		normalized, err := normalizeAgentHubConfig(latestAgentHub, catalog)
		if err != nil {
			return false, err
		}
		changed := !sameAgentHubConfigFields(latestAgentHub, normalized)
		applyAgentHubConfigFields(latest, normalized)
		return changed, nil
	})
	if err != nil {
		return true, err
	}
	return true, nil
}
