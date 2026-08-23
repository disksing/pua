package serve

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/localize"
)

const doctorScanInterval = time.Minute

type doctorWorkspaceReport struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Path   string           `json:"path"`
	Report app.DoctorReport `json:"report"`
}

type doctorSnapshot struct {
	CheckedAt  string                  `json:"checkedAt,omitempty"`
	Checking   bool                    `json:"checking"`
	Complete   bool                    `json:"complete"`
	Summary    app.DoctorSummary       `json:"summary"`
	Error      string                  `json:"error,omitempty"`
	Workspaces []doctorWorkspaceReport `json:"workspaces"`
}

type doctorMonitor struct {
	server  *server
	mu      sync.RWMutex
	current doctorSnapshot
	trigger chan struct{}
}

func newDoctorMonitor(server *server) *doctorMonitor {
	return &doctorMonitor{
		server:  server,
		current: doctorSnapshot{Checking: true, Complete: false, Workspaces: []doctorWorkspaceReport{}},
		trigger: make(chan struct{}, 1),
	}
}

func (m *doctorMonitor) start(ctx context.Context) {
	if m == nil || m.server == nil || m.server.agents == nil {
		return
	}
	m.server.agents.runBackground(func() {
		m.scan(ctx)
		ticker := time.NewTicker(doctorScanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.scan(ctx)
			case <-m.trigger:
				m.scan(ctx)
			}
		}
	})
}

func (m *doctorMonitor) requestScan() {
	if m == nil {
		return
	}
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

func (m *doctorMonitor) snapshot() doctorSnapshot {
	if m == nil {
		return doctorSnapshot{Complete: false, Error: "Doctor monitor is not initialized", Workspaces: []doctorWorkspaceReport{}}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := m.current
	result.Workspaces = append([]doctorWorkspaceReport{}, m.current.Workspaces...)
	return result
}

func (m *doctorMonitor) scan(ctx context.Context) {
	m.mu.Lock()
	current := m.current
	current.Checking = true
	m.current = current
	m.mu.Unlock()

	checkedAt := time.Now().UTC().Format(time.RFC3339Nano)
	snapshot := doctorSnapshot{CheckedAt: checkedAt, Complete: true, Workspaces: []doctorWorkspaceReport{}}
	cfg, err := m.server.loadConfig()
	if err != nil {
		snapshot.Complete = false
		snapshot.Error = err.Error()
		m.store(snapshot)
		return
	}
	options := app.DoctorOptions{}
	if catalog, catalogErr := m.bindingCatalog(ctx, cfg); catalogErr != nil {
		options.BindingError = catalogErr.Error()
	} else {
		options.BindingCatalog = catalog
	}
	for _, workspace := range cfg.Workspaces {
		workspaceOptions := options
		if serviceErr := ValidateServices(workspace.Path); serviceErr != nil {
			workspaceOptions.ServiceError = serviceErr.Error()
		}
		report, scanErr := app.CheckWorkspace(workspace.Path, workspaceOptions)
		if scanErr != nil {
			language, languageErr := workspaceContentLanguage(workspace.Path)
			if languageErr != nil {
				language = localize.English
			}
			messageData := map[string]string{"Code": "workspace_scan_failed", "Original": scanErr.Error()}
			suggestionData := map[string]string{
				"Code": "workspace_scan_failed", "Original": "Restore the configured Workspace path and read access.",
			}
			report = app.DoctorReport{
				Workspace: workspace.Path, Language: language, CheckedAt: checkedAt, Complete: false,
				Summary: app.DoctorSummary{Errors: 1},
				Issues: []app.DoctorIssue{{
					Severity: app.DoctorSeverityError, Code: "workspace_scan_failed",
					Message:    strings.TrimSpace(localize.MustRender(language, "doctor-message.txt", messageData)),
					Suggestion: strings.TrimSpace(localize.MustRender(language, "doctor-suggestion.txt", suggestionData)),
				}},
			}
		}
		snapshot.Workspaces = append(snapshot.Workspaces, doctorWorkspaceReport{
			ID: workspace.ID, Name: workspace.Name, Path: workspace.Path, Report: report,
		})
		snapshot.Summary.Errors += report.Summary.Errors
		snapshot.Summary.Warnings += report.Summary.Warnings
		if !report.Complete {
			snapshot.Complete = false
		}
	}
	m.store(snapshot)
}

func (m *doctorMonitor) store(snapshot doctorSnapshot) {
	snapshot.Checking = false
	m.mu.Lock()
	m.current = snapshot
	m.mu.Unlock()
}

func (m *doctorMonitor) bindingCatalog(ctx context.Context, cfg config) (*app.DoctorBindingCatalog, error) {
	endpoint, err := m.server.effectiveAgentHubEndpoint(cfg.AgentHubEndpoint)
	if err != nil {
		return nil, err
	}
	client, err := newAgentHubClient(endpoint, nil)
	if err != nil {
		return nil, err
	}
	status, err := client.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to AgentHub: %w", err)
	}
	if err := validateAgentHubStatus(status); err != nil {
		return nil, err
	}
	catalog, err := client.Agents(ctx)
	if err != nil {
		return nil, fmt.Errorf("query AgentHub catalog: %w", err)
	}
	result := &app.DoctorBindingCatalog{
		Profiles: make([]app.DoctorProfile, 0, len(cfg.AgentProfiles)),
		Agents:   make([]app.DoctorAgent, 0, len(catalog.Agents)),
	}
	for _, profile := range cfg.AgentProfiles {
		result.Profiles = append(result.Profiles, app.DoctorProfile{Key: profile.Key, AgentName: profile.AgentName})
	}
	for _, agent := range catalog.Agents {
		result.Agents = append(result.Agents, app.DoctorAgent{
			Name: agent.Name, Available: agent.Available, UnavailableReason: agent.UnavailableReason,
		})
	}
	return result, nil
}

func (s *server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	if strings.Trim(r.URL.Path, "/") != "api/doctor" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.doctor.snapshot())
	case http.MethodPost:
		s.doctor.requestScan()
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
