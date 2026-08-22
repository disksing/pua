package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/disksing/pua/agenthub/internal/companion"
	"github.com/disksing/pua/agenthub/internal/config"
	"github.com/disksing/pua/agenthub/internal/provider"
	"github.com/disksing/pua/agenthub/internal/runtime"
	"github.com/disksing/pua/agenthub/internal/semantic"
	"github.com/disksing/pua/agenthub/internal/session"
)

const APIVersion = "1"

const (
	CapabilitySessionSource            = "session.source"
	CapabilitySessionSourceMetadata    = "session.source-metadata"
	CapabilitySessionIdempotentCreate  = "session.idempotent-create"
	CapabilitySessionInputCapabilities = "session.input-capabilities"
	CapabilityMessageIdempotency       = "messages.idempotent"
	CapabilityMessageAtLeastOnce       = "messages.at-least-once"
	CapabilityMessageOpaquePayloadV2   = "messages.opaque-payload-v2"
	CapabilityTurnsStableIndex         = "turns.stable-index"
	CapabilityTurnsMaterialized        = "turns.materialized"
	CapabilityTurnsActivityItems       = "turns.activity-items"
	CapabilitySessionLaunchEnvironment = "session.launch-environment"
	// CapabilitySessionLaunchEnvironmentUpdate reports that resume accepts
	// an optional launchEnvironment overlay persisted before provider start.
	CapabilitySessionLaunchEnvironmentUpdate = "session.launch-environment-update"
	CapabilitySessionStrictStopped           = "session.strict-stopped"
	CapabilityEventsLosslessReplay           = "events.lossless-replay"
	CapabilityEventsDeltaMerge               = "events.delta-merge"
	CapabilityEventsBackwardPagination       = "events.backward-pagination"
	CapabilityEventsCanonicalTerminal        = "events.canonical-turn-terminals"
	CapabilityEventsSemanticV1               = "events.semantic-v1"
	CapabilityEventRawV1                     = "event.raw-v1"
	CapabilityActivityGlobalSSE              = "activity.global-sse"
	CapabilityRecoveryClosedTurns            = "recovery.closed-turns"
)

// ModelLister enumerates the models of a built-in provider and can drop its
// cached results. *provider.ModelCache implements it; tests substitute fakes.
type ModelLister interface {
	Models(ctx context.Context, provider config.Provider) ([]provider.Model, error)
	InvalidateAll()
}

type Server struct {
	store     *session.Store
	startedAt time.Time
	version   string
	runtime   *runtime.Manager
	config    string
	logsDir   string
	webDir    string
	webFS     fs.FS
	// publicBasePath prefixes absolute URLs emitted in response headers. The
	// internal router still uses paths relative to the AgentHub mount.
	publicBasePath string
	listen         *ListenAddress
	models         ModelLister
	quotas         *companion.Service
	// allowedOrigins holds normalized origins (see NormalizeOrigin) that are
	// trusted in addition to the daemon's own origin, for reverse proxy
	// deployments where the public browser origin differs from the daemon
	// address.
	allowedOrigins map[string]bool
	// closing, when set, is closed once the HTTP server begins shutting
	// down so long-lived handlers (SSE streams) can finish promptly.
	closing <-chan struct{}
	// configMu serializes config mutations so a whole-config PUT and a
	// single-provider toggle cannot interleave and lose each other's changes.
	configMu sync.Mutex
}

type Dependencies struct {
	Runtime    *runtime.Manager
	ConfigPath string
	WebDir     string
	// WebFS serves an embedded Web UI. WebDir takes precedence when both are
	// set so development builds can still be selected explicitly.
	WebFS fs.FS
	// PublicBasePath is AgentHub's externally visible mount path.
	PublicBasePath string
	// Listen, when set, enables the Host header guard derived from the
	// validated listen address.
	Listen *ListenAddress
	// Models, when set, enables the provider model enumeration endpoint.
	Models ModelLister
	// QuotaHTTPClient, when set, is used for OnWatch requests. Tests inject a
	// local upstream; production uses a bounded standard-library client.
	QuotaHTTPClient *http.Client
	// LogsDir is the directory service logs are written to, reported by
	// the status endpoint.
	LogsDir string
	// AllowedOrigins lists browser origins (scheme://host[:port]) trusted
	// for mutating requests in addition to the daemon's own origin, e.g.
	// the public https origin of a reverse proxy in front of the daemon.
	// Entries are normalized with NormalizeOrigin when the Server is built.
	AllowedOrigins []string
	// Closing, when set, is closed when the HTTP server starts shutting
	// down; streaming handlers must return so Shutdown can complete.
	Closing <-chan struct{}
}

func New(store *session.Store, version string, startedAt time.Time, dependencies ...Dependencies) *Server {
	server := &Server{store: store, version: version, startedAt: startedAt, quotas: companion.NewService(nil)}
	if len(dependencies) > 0 {
		server.runtime = dependencies[0].Runtime
		server.config = dependencies[0].ConfigPath
		server.logsDir = dependencies[0].LogsDir
		server.webDir = dependencies[0].WebDir
		server.webFS = dependencies[0].WebFS
		server.publicBasePath = strings.TrimRight(dependencies[0].PublicBasePath, "/")
		server.listen = dependencies[0].Listen
		server.models = dependencies[0].Models
		server.quotas = companion.NewService(dependencies[0].QuotaHTTPClient)
		server.closing = dependencies[0].Closing
		if origins := dependencies[0].AllowedOrigins; len(origins) > 0 {
			server.allowedOrigins = make(map[string]bool, len(origins))
			for _, origin := range origins {
				if normalized, err := NormalizeOrigin(origin); err == nil {
					server.allowedOrigins[normalized] = true
				}
			}
		}
	}
	return server
}

// apiRoute binds a mux pattern to a handler. doc is the canonical
// "METHOD /path" label of a documented public API route; routes with an
// empty doc are internal (health probe, the session sub-route dispatcher,
// the docs page itself) and must not be listed in api.md. The table
// returned by routes() is the canonical route inventory, so the coverage
// test in docs_test.go can prove api.md documents exactly the public API.
type apiRoute struct {
	pattern string
	handler http.HandlerFunc
	doc     string
}

func (s *Server) routes() []apiRoute {
	return []apiRoute{
		{"GET /v1/health", s.health, ""},
		{"GET /v1/status", s.status, "GET /v1/status"},
		{"GET /v1/config", s.getConfig, "GET /v1/config"},
		{"PUT /v1/config", s.putConfig, "PUT /v1/config"},
		{"PUT /v1/config/providers/{id}", s.putProviderEnabled, "PUT /v1/config/providers/{id}"},
		{"POST /v1/onwatch/test", s.testOnWatch, "POST /v1/onwatch/test"},
		{"GET /v1/quota", s.quota, "GET /v1/quota"},
		{"GET /v1/activity/events", s.activityEvents, "GET /v1/activity/events"},
		{"GET /v1/providers/{id}/models", s.providerModels, "GET /v1/providers/{id}/models"},
		{"GET /v1/agents", s.agents, "GET /v1/agents"},
		{"GET /v1/sessions", s.listSessions, "GET /v1/sessions"},
		{"POST /v1/sessions", s.createSession, "POST /v1/sessions"},
		{"/v1/sessions/", s.sessionRoute, ""}, // sub-routes dispatch through sessionOps()
		{"GET /api.md", s.apiDocs, ""},
		{"/api.md", s.apiDocsRoute, ""},
		{"/v1", s.apiNotFound, ""},
		{"/v1/", s.apiNotFound, ""},
	}
}

// publicAPILabels returns the canonical "METHOD /path" labels of every
// documented public API route, top-level and under /v1/sessions/{id}.
func (s *Server) publicAPILabels() []string {
	labels := make([]string, 0, len(s.routes())+len(s.sessionOps()))
	for _, route := range s.routes() {
		if route.doc != "" {
			labels = append(labels, route.doc)
		}
	}
	for _, op := range s.sessionOps() {
		labels = append(labels, op.doc)
	}
	return labels
}

func (s *Server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	for _, route := range s.routes() {
		mux.HandleFunc(route.pattern, route.handler)
	}
	if s.webDir != "" {
		mux.Handle("/", spaHandler(s.webDir))
	} else if s.webFS != nil {
		mux.Handle("/", spaHandlerFS(s.webFS))
	}
	return mux
}

func (s *Server) Handler() http.Handler {
	var handler http.Handler = requestMiddleware(s.allowsOrigin, s.mux())
	if s.listen != nil {
		handler = hostGuardMiddleware(s.listen, handler)
	}
	return handler
}

// hostGuardMiddleware rejects requests whose Host header does not name an
// address of this daemon, blocking DNS-rebinding attacks against browsers on
// the local network.
func hostGuardMiddleware(listen *ListenAddress, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !listen.AllowsHost(r.Host) {
			writeAPIError(w, http.StatusForbidden, "host_rejected", "request host does not match an address of this daemon", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion":    APIVersion,
		"capabilities":  s.capabilities(),
		"version":       s.version,
		"startedAt":     s.startedAt,
		"uptimeSeconds": int64(time.Since(s.startedAt).Seconds()),
		"paths": map[string]any{
			"config":   s.config,
			"sessions": s.store.Root(),
			"archive":  s.store.ArchiveRoot(),
			"logs":     s.logsDir,
		},
		"sessionStore": map[string]any{
			"path":         s.store.Root(),
			"archivePath":  s.store.ArchiveRoot(),
			"sessionCount": len(s.store.List(true)),
		},
		"runtime": s.runtimeStatus(),
	})
}

// capabilities reports only behavior available from this server instance.
// Store/API capabilities are always present. Runtime lifecycle capabilities
// are omitted from store-only instances instead of claiming behavior that
// cannot be exercised.
func (s *Server) capabilities() []string {
	capabilities := []string{
		CapabilityEventsLosslessReplay,
		CapabilityEventsDeltaMerge,
		CapabilityEventsBackwardPagination,
		CapabilityEventsSemanticV1,
		CapabilityEventRawV1,
		CapabilityActivityGlobalSSE,
		CapabilitySessionSource,
		CapabilitySessionSourceMetadata,
		CapabilitySessionIdempotentCreate,
		CapabilitySessionInputCapabilities,
		CapabilityMessageIdempotency,
		CapabilityMessageAtLeastOnce,
		CapabilityMessageOpaquePayloadV2,
		CapabilityTurnsStableIndex,
		CapabilityTurnsMaterialized,
		CapabilityTurnsActivityItems,
	}
	if s.runtime != nil {
		capabilities = append(capabilities,
			CapabilityEventsCanonicalTerminal,
			CapabilityRecoveryClosedTurns,
			CapabilitySessionLaunchEnvironment,
			CapabilitySessionLaunchEnvironmentUpdate,
			CapabilitySessionStrictStopped,
		)
	}
	return capabilities
}

func (s *Server) runtimeStatus() any {
	if s.runtime == nil {
		return map[string]any{"available": false}
	}
	return map[string]any{"available": true, "summary": s.runtime.String()}
}

func (s *Server) getConfig(w http.ResponseWriter, _ *http.Request) {
	if s.runtime == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "runtime_unavailable", "runtime is unavailable", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": s.runtime.Config().Redacted()})
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	if s.runtime == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "runtime_unavailable", "runtime is unavailable", nil)
		return
	}
	var body struct {
		Config config.Config `json:"config"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	previous := s.runtime.Config()
	next := body.Config.WithDefaults().PreserveSecrets(previous)
	renames, err := config.DetectRenames(previous, next)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "ambiguous_rename", err.Error(), nil)
		return
	}
	if err := config.Save(s.config, next); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_config", err.Error(), nil)
		return
	}
	_ = s.runtime.SetConfig(next)
	s.invalidateModels()
	s.quotas.Invalidate()
	if err := s.migrateSessionAgentReferences(renames); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "agent_rename_failed", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": next.Redacted()})
}

func (s *Server) quota(w http.ResponseWriter, r *http.Request) {
	if s.runtime == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "runtime_unavailable", "runtime is unavailable", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"quota": s.quotas.Snapshot(r.Context(), s.runtime.Config().OnWatch)})
}

func (s *Server) testOnWatch(w http.ResponseWriter, r *http.Request) {
	if s.runtime == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "runtime_unavailable", "runtime is unavailable", nil)
		return
	}
	var body struct {
		OnWatch config.OnWatch `json:"onWatch"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	candidate := config.Defaults()
	candidate.OnWatch = body.OnWatch
	candidate.OnWatch.Enabled = true
	candidate = candidate.PreserveSecrets(s.runtime.Config())
	if err := candidate.Validate(); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_config", err.Error(), nil)
		return
	}
	catalog, err := s.quotas.TestConnection(r.Context(), candidate.OnWatch)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "onwatch_unavailable", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connected": true, "providers": catalog.Providers})
}

// migrateSessionAgentReferences re-points sessions at renamed agents by
// appending a session.agent event. Only active sessions are migrated:
// archived sessions are read-only, and their stored name stays a historical
// record that remains readable. Config validation guarantees names are
// unique case-insensitively, so matching the old name case-insensitively
// cannot hit the wrong session.
func (s *Server) migrateSessionAgentReferences(renames map[string]string) error {
	if len(renames) == 0 {
		return nil
	}
	lookup := make(map[string]string, len(renames))
	for oldName, newName := range renames {
		lookup[config.NormalizeAgentName(oldName)] = newName
	}
	for _, value := range s.store.List(false) {
		newName, ok := lookup[config.NormalizeAgentName(value.AgentName)]
		if !ok {
			continue
		}
		data, err := json.Marshal(session.AgentRenameEventData{AgentName: newName})
		if err != nil {
			return err
		}
		if _, err := s.store.Append(value.ID, "session.agent", "", data); err != nil {
			return fmt.Errorf("migrate session %s to renamed agent %q: %w", value.ID, newName, err)
		}
	}
	return nil
}

// modelEnumerationTimeout bounds a whole model enumeration, including the
// provider process startup. It is generous because app-server and RPC CLIs
// can take seconds to boot, and short enough that a hung provider cannot pin
// a request handler.
const modelEnumerationTimeout = 45 * time.Second

// providerModels enumerates the models of one built-in provider through its
// official interface. It is read-only: it never creates a provider session
// and never changes the configuration. Status codes distinguish the failure
// modes a UI must render differently: 404 unknown provider, 409 disabled,
// 503 CLI unavailable, 504 enumeration timeout, 502 upstream error; an empty
// list is a 200 with an empty models array.
func (s *Server) providerModels(w http.ResponseWriter, r *http.Request) {
	if s.runtime == nil || s.models == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "runtime_unavailable", "runtime is unavailable", nil)
		return
	}
	id := r.PathValue("id")
	target, ok := s.providerByID(id)
	if !ok {
		writeAPIError(w, http.StatusNotFound, "unknown_provider", fmt.Sprintf("unknown built-in provider %q", id), nil)
		return
	}
	if !target.Enabled {
		writeAPIError(w, http.StatusConflict, "provider_disabled", fmt.Sprintf("provider %q is disabled", id), nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelEnumerationTimeout)
	defer cancel()
	models, err := s.models.Models(ctx, target)
	if err != nil {
		var modelErr *provider.ModelError
		if errors.As(err, &modelErr) {
			switch modelErr.Kind {
			case provider.ModelErrTimeout:
				writeAPIError(w, http.StatusGatewayTimeout, "provider_timeout", modelErr.Error(), nil)
				return
			case provider.ModelErrUnavailable:
				writeAPIError(w, http.StatusServiceUnavailable, "provider_unavailable", modelErr.Error(), nil)
				return
			}
		}
		writeAPIError(w, http.StatusBadGateway, "provider_error", err.Error(), nil)
		return
	}
	if models == nil {
		models = []provider.Model{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": map[string]any{"id": target.ID, "name": target.Name, "type": target.Type},
		"models":   models,
	})
}

// providerByID resolves a built-in provider id against the live
// configuration: a configured provider keeps its name, type, command and
// enabled flag; a built-in provider missing from the configuration is
// reported with its canonical definition and enabled=false.
func (s *Server) providerByID(id string) (config.Provider, bool) {
	canonical, ok := config.BuiltinProvider(id)
	if !ok {
		return config.Provider{}, false
	}
	for _, configured := range s.runtime.Config().AgentProviders {
		if configured.ID == id {
			return configured, true
		}
	}
	return canonical, true
}

func (s *Server) invalidateModels() {
	if s.models != nil {
		s.models.InvalidateAll()
	}
}

// putProviderEnabled flips the enabled flag of one built-in provider without
// touching the rest of the configuration. It is the minimal contract behind
// the four switches of the Web settings UI: clients never have to rebuild or
// resubmit the whole provider structure, and the provider's command and other
// fields survive a disable/enable cycle. A built-in provider missing from an
// old config is created with its canonical defaults.
func (s *Server) putProviderEnabled(w http.ResponseWriter, r *http.Request) {
	if s.runtime == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "runtime_unavailable", "runtime is unavailable", nil)
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	if body.Enabled == nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "enabled is required", nil)
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	next, provider, err := s.runtime.Config().SetProviderEnabled(r.PathValue("id"), *body.Enabled)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "unknown_provider", err.Error(), nil)
		return
	}
	if err := config.Save(s.config, next); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_config", err.Error(), nil)
		return
	}
	_ = s.runtime.SetConfig(next)
	s.invalidateModels()
	writeJSON(w, http.StatusOK, map[string]any{"provider": provider})
}

// agentStatus extends an agent with its effective availability. An agent is
// unavailable when its provider is disabled or missing; the Web UI hides such
// agents from the new-session choices and the daemon rejects attempts to use
// them anyway.
type agentStatus struct {
	config.Agent
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailableReason,omitempty"`
}

func (s *Server) agents(w http.ResponseWriter, _ *http.Request) {
	if s.runtime == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "runtime_unavailable", "runtime is unavailable", nil)
		return
	}
	cfg := s.runtime.Config()
	providers := make(map[string]config.Provider, len(cfg.AgentProviders))
	for _, provider := range cfg.AgentProviders {
		providers[provider.ID] = provider
	}
	agents := make([]agentStatus, 0, len(cfg.Agents))
	for _, agent := range cfg.Agents {
		status := agentStatus{Agent: agent, Available: true}
		provider, ok := providers[agent.ProviderID]
		switch {
		case !ok:
			status.Available = false
			status.UnavailableReason = fmt.Sprintf("provider %q is not configured", agent.ProviderID)
		case !provider.Enabled:
			status.Available = false
			status.UnavailableReason = fmt.Sprintf("provider %q is disabled", agent.ProviderID)
		}
		agents = append(agents, status)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": cfg.AgentProviders,
		"agents":    agents,
		"probes":    cfg.Probes(),
	})
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	archivedOnly := r.URL.Query().Get("archived") == "true"
	includeArchived := archivedOnly || r.URL.Query().Get("includeArchived") == "true"
	query := r.URL.Query()
	filter := session.ListFilter{IncludeArchived: includeArchived}
	if query.Has("sourceApp") {
		filter.SourceApp = stringPointer(query.Get("sourceApp"))
	}
	if query.Has("sourceInstanceId") {
		filter.SourceInstanceID = stringPointer(query.Get("sourceInstanceId"))
	}
	if query.Has("sourceExternalId") {
		filter.SourceExternalID = stringPointer(query.Get("sourceExternalId"))
	}
	values := s.store.Filter(filter)
	if archivedOnly {
		filtered := values[:0]
		for _, value := range values {
			if value.State == session.StateArchived {
				filtered = append(filtered, value)
			}
		}
		values = filtered
	}
	if stateFilter := strings.TrimSpace(r.URL.Query().Get("state")); stateFilter != "" {
		allowed := make(map[string]bool)
		for _, state := range strings.Split(stateFilter, ",") {
			allowed[strings.TrimSpace(state)] = true
		}
		filtered := values[:0]
		for _, value := range values {
			if allowed[value.State] {
				filtered = append(filtered, value)
			}
		}
		values = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": values})
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title             string                 `json:"title"`
		Cwd               string                 `json:"cwd"`
		AgentName         string                 `json:"agentName"`
		Source            *session.Source        `json:"source"`
		IdempotencyKey    string                 `json:"idempotencyKey"`
		LaunchEnvironment map[string]string      `json:"launchEnvironment"`
		InitialMessage    *inboundMessageRequest `json:"initialMessage"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	var initialMessage *session.MessageInput
	if body.InitialMessage != nil && body.InitialMessage.hasMessageIntent() {
		value, err := body.InitialMessage.messageInput()
		if err != nil {
			writeMessageInputError(w, err)
			return
		}
		initialMessage = &value
	}
	agentName := strings.TrimSpace(body.AgentName)
	if agentName == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "agent_required", "agentName is required: sessions are always created with an explicit agent", nil)
		return
	}
	var agent config.Agent
	var providerConfig config.Provider
	if s.runtime != nil {
		resolved, resolvedProvider, err := s.runtime.Config().Agent(agentName)
		if err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, "invalid_agent", err.Error(), nil)
			return
		}
		agent = resolved
		providerConfig = resolvedProvider
	}
	cwd, err := canonicalDirectory(body.Cwd)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_cwd", err.Error(), nil)
		return
	}
	if err := session.ValidateLaunchEnvironment(body.LaunchEnvironment); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_launch_environment", err.Error(), nil)
		return
	}
	// Persist the canonical configured name, not the spelling the client
	// sent, so the session always records the user's display form.
	canonicalName := agentName
	if agent.Name != "" {
		canonicalName = agent.Name
	}
	value, created, err := s.store.CreateOrGet(session.CreateInput{
		Title:             strings.TrimSpace(body.Title),
		Cwd:               cwd,
		AgentName:         canonicalName,
		IdempotencyKey:    body.IdempotencyKey,
		Source:            body.Source,
		LaunchEnvironment: body.LaunchEnvironment,
		Provider:          providerConfig.Type,
		InputCapabilities: provider.InputCapabilities(providerConfig.Type),
	})
	if err != nil {
		if errors.Is(err, session.ErrIdempotencyConflict) {
			writeAPIError(w, http.StatusConflict, "idempotency_conflict", err.Error(), nil)
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "session_create_failed", err.Error(), nil)
		return
	}
	if s.runtime != nil && value.State != session.StateArchived && value.State != session.StateStopped {
		if created || value.State == session.StateReady || value.State == session.StateStarting {
			started, err := s.runtime.Start(value.ID)
			if err != nil {
				writeAPIError(w, http.StatusBadGateway, "provider_start_failed", err.Error(), map[string]any{"sessionId": value.ID})
				return
			}
			value = started
		}
		if initialMessage != nil && (created || initialMessage.MessageID != "") {
			sent, err := s.runtime.SendMessage(value.ID, *initialMessage)
			if err != nil {
				writeAPIError(w, http.StatusBadGateway, "turn_start_failed", err.Error(), map[string]any{"sessionId": value.ID})
				return
			}
			value = sent
		}
	}
	w.Header().Set("Location", s.publicBasePath+"/v1/sessions/"+value.ID)
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"session": value, "created": created})
}

func stringPointer(value string) *string {
	return &value
}

// sessionOp is one operation under /v1/sessions/{id}. suffix is the fixed
// path after the session id ("" for the bare session, "approvals/{approvalId}"
// for the one nested operation); doc is the canonical label listed in
// api.md. sessionRoute dispatches through this table, so adding, renaming
// or dropping a session operation here is what the documentation coverage
// test compares api.md against.
type sessionOp struct {
	method  string
	suffix  string
	handler func(http.ResponseWriter, *http.Request, string)
	doc     string
}

func (s *Server) sessionOps() []sessionOp {
	return []sessionOp{
		{http.MethodGet, "", s.getSession, "GET /v1/sessions/{id}"},
		{http.MethodDelete, "", s.archiveSession, "DELETE /v1/sessions/{id}"},
		{http.MethodGet, "events", s.events, "GET /v1/sessions/{id}/events"},
		{http.MethodGet, "event/{sourceEventId}", s.event, "GET /v1/sessions/{id}/event/{sourceEventId}"},
		{http.MethodGet, "turns", s.turns, "GET /v1/sessions/{id}/turns"},
		{http.MethodGet, "turns/{turnId}", s.turn, "GET /v1/sessions/{id}/turns/{turnId}"},
		{http.MethodPost, "messages", s.sendMessage, "POST /v1/sessions/{id}/messages"},
		{http.MethodPost, "resume", s.resumeSession, "POST /v1/sessions/{id}/resume"},
		{http.MethodPost, "interrupt", s.interruptSession, "POST /v1/sessions/{id}/interrupt"},
		{http.MethodPost, "stop", s.stopSession, "POST /v1/sessions/{id}/stop"},
		{http.MethodPost, "approvals/{approvalId}", s.resolveApproval, "POST /v1/sessions/{id}/approvals/{approvalId}"},
	}
}

// inboundMessageRequest is shared by the messages endpoint and optional
// initialMessage. Schema v2 carries opaque caller payload. The provenance
// fields below are accepted only by the schema-v1 compatibility adapter.
type inboundMessageRequest struct {
	SchemaVersion int             `json:"schemaVersion"`
	Text          string          `json:"text"`
	Payload       json.RawMessage `json:"payload"`
	Role          string          `json:"role"`
	Sender        json.RawMessage `json:"sender"`
	Steer         bool            `json:"steer"`
	MessageID     string          `json:"messageId"`
	ReplyTo       string          `json:"replyTo"`
	CorrelationID string          `json:"correlationId"`
}

func (request inboundMessageRequest) hasMessageIntent() bool {
	return strings.TrimSpace(request.Text) != "" ||
		request.SchemaVersion != 0 || len(bytes.TrimSpace(request.Payload)) > 0 ||
		strings.TrimSpace(request.Role) != "" || request.Steer ||
		len(bytes.TrimSpace(request.Sender)) > 0 || request.MessageID != "" ||
		request.ReplyTo != "" || request.CorrelationID != ""
}

func (request inboundMessageRequest) messageInput() (session.MessageInput, error) {
	sender, err := decodeMessageSender(request.Sender)
	if err != nil {
		return session.MessageInput{}, err
	}
	return session.NormalizeMessageInput(session.MessageInput{
		SchemaVersion: request.SchemaVersion,
		Text:          request.Text,
		Payload:       request.Payload,
		Role:          session.MessageRole(request.Role),
		Sender:        sender,
		Steer:         request.Steer,
		MessageID:     request.MessageID,
		ReplyTo:       request.ReplyTo,
		CorrelationID: request.CorrelationID,
	})
}

func decodeMessageSender(raw json.RawMessage) (*session.MessageSender, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	if raw[0] != '{' {
		return nil, &session.MessageInputError{
			Code: "invalid_message_sender", Field: "sender",
			Message: "sender must be an object with optional id, name, or sessionId",
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var sender session.MessageSender
	if err := decoder.Decode(&sender); err != nil {
		return nil, &session.MessageInputError{
			Code: "invalid_message_sender", Field: "sender",
			Message: fmt.Sprintf("invalid sender: %v", err),
		}
	}
	return &sender, nil
}

func writeMessageInputError(w http.ResponseWriter, err error) {
	var inputErr *session.MessageInputError
	if errors.As(err, &inputErr) {
		details := map[string]any{"field": inputErr.Field}
		code := inputErr.Code
		// Preserve the long-standing invalid_request code for blank text while
		// exposing dedicated structured codes for provenance validation.
		if code == "invalid_message_text" {
			code = "invalid_request"
		}
		writeAPIError(w, http.StatusBadRequest, code, inputErr.Message, details)
		return
	}
	writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
}

func (s *Server) sessionRoute(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/sessions/"), "/")
	parts := strings.Split(path, "/")
	if parts[0] == "" {
		writeAPIError(w, http.StatusNotFound, "route_not_found", "API route not found", nil)
		return
	}
	id, tail := parts[0], parts[1:]
	for _, op := range s.sessionOps() {
		if r.Method != op.method {
			continue
		}
		var suffix []string
		if op.suffix != "" {
			suffix = strings.Split(op.suffix, "/")
		}
		if len(suffix) != len(tail) {
			continue
		}
		match := true
		for i, segment := range suffix {
			if strings.HasPrefix(segment, "{") {
				r.SetPathValue(strings.Trim(segment, "{}"), tail[i])
				continue
			}
			if segment != tail[i] {
				match = false
			}
		}
		if match {
			op.handler(w, r, id)
			return
		}
	}
	if len(tail) == 0 {
		// The session exists as an addressable resource; the method is not.
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed for this API route", nil)
		return
	}
	writeAPIError(w, http.StatusNotFound, "route_not_found", "API route not found", nil)
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request, id string) {
	if s.rejectArchivedSession(w, id) {
		return
	}
	var body inboundMessageRequest
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	input, err := body.messageInput()
	if err != nil {
		writeMessageInputError(w, err)
		return
	}
	if s.runtime == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "runtime_unavailable", "runtime is unavailable", nil)
		return
	}
	current, err := s.store.Get(id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if current.State == session.StateStopping {
		writeAPIError(w, http.StatusConflict, "session_stopping", "session provider is stopping", nil)
		return
	}
	if current.CurrentTurnID != "" && !input.Steer {
		writeAPIError(w, http.StatusConflict, "turn_active", "session already has an active turn; set steer=true or wait", map[string]any{"turnId": current.CurrentTurnID})
		return
	}
	value, err := s.runtime.SendMessage(id, input)
	if err != nil {
		var inputErr *session.MessageInputError
		if errors.As(err, &inputErr) {
			writeMessageInputError(w, err)
			return
		}
		s.writeRuntimeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"session": value})
}

func (s *Server) resumeSession(w http.ResponseWriter, r *http.Request, id string) {
	if s.rejectArchivedSession(w, id) {
		return
	}
	if s.runtime == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "runtime_unavailable", "runtime is unavailable", nil)
		return
	}
	// The body is optional: an empty body (or {}) resumes with the recorded
	// environment, while launchEnvironment overlays entries onto it.
	var body struct {
		LaunchEnvironment map[string]string `json:"launchEnvironment"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	if err := session.ValidateLaunchEnvironment(body.LaunchEnvironment); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_launch_environment", err.Error(), nil)
		return
	}
	current, err := s.store.Get(id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if current.State == session.StateStopping {
		writeAPIError(w, http.StatusConflict, "session_stopping", "session provider is stopping", nil)
		return
	}
	// Persist the overlay before starting the runtime so the provider picks
	// up the merged environment when it (re)starts. The update stays durable
	// even if the provider then fails to start, mirroring session creation.
	if len(body.LaunchEnvironment) > 0 {
		if _, err := s.store.UpdateLaunchEnvironment(id, body.LaunchEnvironment); err != nil {
			switch {
			case errors.Is(err, session.ErrArchived):
				writeAPIError(w, http.StatusConflict, "session_archived", "session is archived and read-only", nil)
			default:
				s.writeStoreError(w, err)
			}
			return
		}
	}
	value, err := s.runtime.Start(id)
	if err != nil {
		s.writeRuntimeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": value})
}

func (s *Server) interruptSession(w http.ResponseWriter, _ *http.Request, id string) {
	if s.rejectArchivedSession(w, id) {
		return
	}
	if s.runtime == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "runtime_unavailable", "runtime is unavailable", nil)
		return
	}
	current, err := s.store.Get(id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if current.CurrentTurnID == "" {
		writeAPIError(w, http.StatusConflict, "turn_not_active", "session has no active turn to interrupt", nil)
		return
	}
	if err := s.runtime.Interrupt(id); err != nil {
		s.writeRuntimeError(w, err)
		return
	}
	value, _ := s.store.Get(id)
	writeJSON(w, http.StatusOK, map[string]any{"session": value})
}

func (s *Server) stopSession(w http.ResponseWriter, _ *http.Request, id string) {
	if s.rejectArchivedSession(w, id) {
		return
	}
	if s.runtime == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "runtime_unavailable", "runtime is unavailable", nil)
		return
	}
	if err := s.runtime.Stop(id); err != nil {
		s.writeRuntimeError(w, err)
		return
	}
	value, _ := s.store.Get(id)
	writeJSON(w, http.StatusOK, map[string]any{"session": value})
}

func (s *Server) resolveApproval(w http.ResponseWriter, r *http.Request, id string) {
	approvalID := r.PathValue("approvalId")
	if s.rejectArchivedSession(w, id) {
		return
	}
	if s.runtime == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "runtime_unavailable", "runtime is unavailable", nil)
		return
	}
	var body struct {
		Decision string `json:"decision"`
		OptionID string `json:"optionId"`
		Text     string `json:"text"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	reply := runtime.ApprovalReply{
		Decision: body.Decision,
		OptionID: strings.TrimSpace(body.OptionID),
		Text:     strings.TrimSpace(body.Text),
	}
	switch {
	case reply.Text != "":
		if reply.Decision != "" || reply.OptionID != "" {
			writeAPIError(w, http.StatusBadRequest, "invalid_approval_decision", "text replies cannot be combined with decision or optionId", nil)
			return
		}
	case reply.OptionID != "":
		if reply.Decision != "" {
			writeAPIError(w, http.StatusBadRequest, "invalid_approval_decision", "optionId cannot be combined with decision", nil)
			return
		}
		reply.Decision = "accept"
	default:
		switch reply.Decision {
		case "accept", "acceptForSession", "decline", "cancel":
		default:
			writeAPIError(w, http.StatusBadRequest, "invalid_approval_decision", "decision must be accept, acceptForSession, decline, or cancel", nil)
			return
		}
	}
	current, err := s.store.Get(id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !containsString(current.PendingApprovalIDs, approvalID) {
		writeAPIError(w, http.StatusConflict, "approval_not_pending", "approval is not pending", map[string]any{"approvalId": approvalID})
		return
	}
	if err := s.runtime.Approve(id, approvalID, reply); err != nil {
		s.writeRuntimeError(w, err)
		return
	}
	value, _ := s.store.Get(id)
	writeJSON(w, http.StatusOK, map[string]any{"session": value})
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (s *Server) writeRuntimeError(w http.ResponseWriter, err error) {
	if errors.Is(err, session.ErrNotFound) {
		s.writeStoreError(w, err)
		return
	}
	if errors.Is(err, session.ErrArchived) {
		writeAPIError(w, http.StatusConflict, "session_archived", "session is archived and read-only", nil)
		return
	}
	writeAPIError(w, http.StatusConflict, "runtime_operation_failed", err.Error(), nil)
}

func (s *Server) getSession(w http.ResponseWriter, _ *http.Request, id string) {
	value, err := s.store.Get(id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": value})
}

func (s *Server) archiveSession(w http.ResponseWriter, _ *http.Request, id string) {
	if _, err := s.store.Get(id); err != nil {
		s.writeStoreError(w, err)
		return
	}
	if s.runtime != nil && s.runtime.IsRunning(id) {
		writeAPIError(w, http.StatusConflict, "session_active", "the session provider is still running; stop the session before archiving it", nil)
		return
	}
	value, err := s.store.Archive(id)
	if err != nil {
		switch {
		case errors.Is(err, session.ErrNotFound):
			s.writeStoreError(w, err)
		case errors.Is(err, session.ErrInvalidID):
			writeAPIError(w, http.StatusBadRequest, "invalid_session_id", err.Error(), nil)
		case errors.Is(err, session.ErrSessionActive):
			writeAPIError(w, http.StatusConflict, "session_active", err.Error(), nil)
		case errors.Is(err, session.ErrArchiveConflict):
			writeAPIError(w, http.StatusConflict, "session_archive_conflict", err.Error(), nil)
		default:
			writeAPIError(w, http.StatusInternalServerError, "session_archive_failed", err.Error(), nil)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": value})
}

// rejectArchivedSession writes 409 and reports true when the session is
// archived. Archived sessions are read-only: turns, steer, resume,
// interrupt and approval writes are rejected before reaching the runtime.
func (s *Server) rejectArchivedSession(w http.ResponseWriter, id string) bool {
	value, err := s.store.Get(id)
	if err != nil {
		s.writeStoreError(w, err)
		return true
	}
	if value.State == session.StateArchived {
		writeAPIError(w, http.StatusConflict, "session_archived", "session is archived and read-only", nil)
		return true
	}
	return false
}

type activitySession struct {
	SessionID    string                `json:"sessionId"`
	Provider     string                `json:"provider,omitempty"`
	Title        string                `json:"title,omitempty"`
	TurnID       string                `json:"turnId,omitempty"`
	EventCount   int                   `json:"eventCount"`
	Completed    bool                  `json:"completed"`
	TurnTerminal *activityTurnTerminal `json:"turnTerminal,omitempty"`
	LastEventAt  time.Time             `json:"lastEventAt"`
}

type activityTurnTerminal struct {
	TurnID  string    `json:"turnId,omitempty"`
	Status  string    `json:"status"`
	EndedAt time.Time `json:"endedAt"`
}

type activityFrame struct {
	Sequence        uint64            `json:"sequence"`
	WindowStartedAt time.Time         `json:"windowStartedAt"`
	WindowEndedAt   time.Time         `json:"windowEndedAt"`
	Sessions        []activitySession `json:"sessions"`
}

func (s *Server) activityEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "stream_unsupported", "response writer does not support streaming", nil)
		return
	}
	live := s.store.SubscribeAll()
	defer live.Cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	windowStartedAt := time.Now().UTC()
	pending := make(map[string]activitySession)
	var sequence uint64
	window := time.NewTicker(time.Second)
	heartbeat := time.NewTicker(15 * time.Second)
	defer window.Stop()
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.closing:
			return
		case <-live.Overflow():
			return
		case event := <-live.Events():
			if live.Overflowed() {
				return
			}
			if !isActivityEvent(event) {
				continue
			}
			entry := pending[event.SessionID]
			if entry.SessionID == "" {
				entry.SessionID = event.SessionID
			}
			if entry.Provider == "" || entry.Title == "" {
				if current, err := s.store.Get(event.SessionID); err == nil {
					entry.Provider = current.Provider
					entry.Title = current.Title
				}
			}
			entry.EventCount++
			entry.LastEventAt = event.Time
			if event.TurnID != "" {
				if entry.TurnTerminal != nil && event.TurnID != entry.TurnTerminal.TurnID {
					entry.Completed = false
					entry.TurnTerminal = nil
				}
				entry.TurnID = event.TurnID
			}
			switch event.Type {
			case session.EventTurnCompleted:
				entry.Completed = true
				entry.TurnTerminal = &activityTurnTerminal{TurnID: event.TurnID, Status: "completed", EndedAt: event.Time}
			case session.EventTurnFailed:
				entry.Completed = true
				entry.TurnTerminal = &activityTurnTerminal{TurnID: event.TurnID, Status: "failed", EndedAt: event.Time}
			case session.EventTurnCancelled:
				entry.Completed = true
				entry.TurnTerminal = &activityTurnTerminal{TurnID: event.TurnID, Status: "cancelled", EndedAt: event.Time}
			}
			pending[event.SessionID] = entry
		case endedAt := <-window.C:
			if live.Overflowed() {
				return
			}
			endedAt = endedAt.UTC()
			if len(pending) > 0 {
				sequence++
				sessions := make([]activitySession, 0, len(pending))
				for _, entry := range pending {
					sessions = append(sessions, entry)
				}
				sort.Slice(sessions, func(i, j int) bool { return sessions[i].SessionID < sessions[j].SessionID })
				frame := activityFrame{
					Sequence: sequence, WindowStartedAt: windowStartedAt,
					WindowEndedAt: endedAt, Sessions: sessions,
				}
				if err := writeActivitySSE(w, frame); err != nil {
					return
				}
				flusher.Flush()
				pending = make(map[string]activitySession)
			}
			windowStartedAt = endedAt
		case <-heartbeat.C:
			if err := writeSSEHeartbeat(w); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// isActivityEvent limits the monitor to user-visible work within a Turn.
// Session/process lifecycle records and raw Provider diagnostics are durable
// for recovery and debugging, but they do not mean an agent is doing work.
// In particular, a daemon shutdown appends state records for every resident
// Provider session, and background Provider stderr/metadata can arrive long
// after its last Turn. Neither should make an idle session look active.
func isActivityEvent(event session.Event) bool {
	return session.IsActivityEvent(event)
}

func writeActivitySSE(w http.ResponseWriter, frame activityFrame) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return writeSSEBounded(w, func() error {
		_, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", frame.Sequence, data)
		return err
	})
}

func (s *Server) event(w http.ResponseWriter, r *http.Request, id string) {
	if r.URL.RawQuery != "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_event_query", "single event lookup does not accept query parameters", nil)
		return
	}
	eventID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("sourceEventId")), 10, 64)
	if err != nil || eventID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_event_id", "sourceEventId must be a positive event id", nil)
		return
	}
	page, err := s.store.EventsPage(id, eventID-1, 1)
	if err != nil {
		if errors.Is(err, session.ErrEventCursorAhead) {
			writeAPIError(w, http.StatusNotFound, "event_not_found", "event not found", nil)
			return
		}
		s.writeStoreError(w, err)
		return
	}
	if len(page.Events) != 1 || page.Events[0].ID != eventID {
		writeAPIError(w, http.StatusNotFound, "event_not_found", "event not found", nil)
		return
	}
	source := page.Events[0]
	if source.SessionID != "" && source.SessionID != id {
		writeAPIError(w, http.StatusInternalServerError, "event_session_mismatch", "event belongs to a different session", nil)
		return
	}
	writeJSON(w, http.StatusOK, semantic.Detail{
		Schema: semantic.EventDetailSchema, SourceEvent: source, Frame: semantic.FrameFor(source, false),
	})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, id string) {
	after, err := parseEventCursor(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_event_cursor", err.Error(), nil)
		return
	}
	before, backward, err := parseEventBackward(r.URL.Query())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_event_cursor", err.Error(), nil)
		return
	}
	if backward && explicitEventCursor(r) {
		writeAPIError(w, http.StatusBadRequest, "invalid_event_cursor", "before/latest cannot be combined with after", nil)
		return
	}
	stream := strings.Contains(r.Header.Get("Accept"), "text/event-stream") || r.URL.Query().Get("stream") == "true"
	rangeStart, rangeEnd, ranged, err := parseEventRange(r.URL.Query())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_event_range", err.Error(), nil)
		return
	}
	if ranged {
		if backward || stream {
			writeAPIError(w, http.StatusBadRequest, "invalid_event_range", "event ranges do not support before/latest or streaming", nil)
			return
		}
		if !explicitEventCursor(r) || after < rangeStart-1 {
			after = rangeStart - 1
		}
		if after >= rangeEnd {
			writeAPIError(w, http.StatusBadRequest, "invalid_event_range", "after must be smaller than the range end", nil)
			return
		}
	}
	if !stream {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if backward {
			page, err := s.store.EventsPageBefore(id, before, limit)
			if err != nil {
				s.writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"schema": semantic.EventsSchema,
				"frames": semantic.FramesFor(page.Events),
				"page": map[string]any{
					"after":         page.After,
					"limit":         page.Limit,
					"nextAfter":     page.NextAfter,
					"hasMore":       page.HasMore,
					"before":        page.Before,
					"nextBefore":    page.NextBefore,
					"hasMoreBefore": page.HasMoreBefore,
				},
				"latestCursor": page.LatestCursor,
			})
			return
		}
		page, err := s.store.EventsPage(id, after, limit)
		if err != nil {
			if errors.Is(err, session.ErrEventCursorAhead) {
				writeAPIError(w, http.StatusConflict, "event_cursor_ahead", err.Error(), map[string]any{
					"latestCursor": page.LatestCursor,
				})
				return
			}
			s.writeStoreError(w, err)
			return
		}
		events := page.Events
		hasMore := page.HasMore
		nextAfter := page.NextAfter
		if ranged {
			end := sort.Search(len(events), func(index int) bool { return events[index].ID > rangeEnd })
			events = events[:end]
			hasMore = len(events) > 0 && events[len(events)-1].ID < rangeEnd && events[len(events)-1].ID < page.LatestCursor
			if len(events) == 0 {
				hasMore = after < rangeEnd && after < page.LatestCursor
				nextAfter = after
			} else {
				nextAfter = events[len(events)-1].ID
			}
		}
		response := map[string]any{
			"schema": semantic.EventsSchema,
			"frames": semantic.FramesFor(events),
			"page": map[string]any{
				"after":     page.After,
				"limit":     page.Limit,
				"nextAfter": nextAfter,
				"hasMore":   hasMore,
			},
			"latestCursor": page.LatestCursor,
		}
		if ranged {
			response["rangeStart"] = rangeStart
			response["rangeEnd"] = rangeEnd
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
	if backward {
		writeAPIError(w, http.StatusBadRequest, "invalid_event_cursor", "before/latest are not supported for event streams", nil)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "stream_unsupported", "response writer does not support streaming", nil)
		return
	}
	live, highWater, err := s.store.Subscribe(id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	defer live.Cancel()
	if after > highWater {
		writeAPIError(w, http.StatusConflict, "event_cursor_ahead",
			fmt.Sprintf("%s: %d > %d", session.ErrEventCursorAhead, after, highWater),
			map[string]any{"latestCursor": highWater})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	lastSent := after
	// Delta merges fold new fragments into the event at the client's cursor
	// without issuing a new id, and live append patches never move the
	// cursor, so after a disconnect the client's copy of that event may lag
	// behind. Re-send it with its current durable content before replaying
	// newer events; consumers treat the repeated id as a full replacement.
	if lastSent > 0 {
		page, err := s.store.EventsPage(id, lastSent-1, 1)
		if err != nil {
			return
		}
		if len(page.Events) == 1 {
			if err := writeSemanticSSE(w, page.Events[0], false); err != nil {
				return
			}
		}
	}
	for lastSent < highWater {
		if live.Overflowed() {
			return
		}
		replay, err := s.store.EventsThrough(id, lastSent, highWater, session.MaxEventPageSize)
		if err != nil || len(replay) == 0 {
			return
		}
		for _, event := range replay {
			if live.Overflowed() || event.ID != lastSent+1 {
				return
			}
			if err := writeSemanticSSE(w, event, false); err != nil {
				return
			}
			lastSent = event.ID
		}
		flusher.Flush()
	}
	if lastSent == after {
		flusher.Flush()
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.closing:
			// The daemon is shutting down; end the stream so
			// http.Server.Shutdown is not held open by SSE clients.
			return
		case <-live.Overflow():
			return
		case event := <-live.Events():
			if live.Overflowed() {
				return
			}
			if event.ID <= lastSent {
				// The store folds consecutive text deltas into the tail event
				// and republishes an append patch under the id the client
				// already has. Forward it so live readers extend the merged
				// content; only a new id may advance the cursor.
				if err := writeSemanticSSE(w, event, true); err != nil {
					return
				}
				flusher.Flush()
				continue
			}
			if event.ID != lastSent+1 {
				return
			}
			if err := writeSemanticSSE(w, event, false); err != nil {
				return
			}
			lastSent = event.ID
			flusher.Flush()
		case <-heartbeat.C:
			if err := writeSSEHeartbeat(w); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func parseEventRange(values url.Values) (int64, int64, bool, error) {
	startText := strings.TrimSpace(values.Get("start"))
	endText := strings.TrimSpace(values.Get("end"))
	if startText == "" && endText == "" {
		return 0, 0, false, nil
	}
	if startText == "" || endText == "" {
		return 0, 0, false, errors.New("start and end must be provided together")
	}
	start, err := strconv.ParseInt(startText, 10, 64)
	if err != nil || start <= 0 {
		return 0, 0, false, errors.New("start must be a positive event id")
	}
	end, err := strconv.ParseInt(endText, 10, 64)
	if err != nil || end < start {
		return 0, 0, false, errors.New("end must be an event id greater than or equal to start")
	}
	return start, end, true, nil
}

func (s *Server) turns(w http.ResponseWriter, r *http.Request, id string) {
	after, err := parseEventCursor(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_turn_cursor", err.Error(), nil)
		return
	}
	before, backward, err := parseEventBackward(r.URL.Query())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_turn_cursor", err.Error(), nil)
		return
	}
	if backward && explicitEventCursor(r) {
		writeAPIError(w, http.StatusBadRequest, "invalid_turn_cursor", "before/latest cannot be combined with after", nil)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	latest := backward && before == math.MaxInt64
	if latest {
		before = 0
	}
	page, err := s.store.TurnsPage(id, after, before, latest, limit)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"turns": page.Turns,
		"page": map[string]any{
			"after": page.After, "before": page.Before, "limit": page.Limit,
			"nextAfter": page.NextAfter, "nextBefore": page.NextBefore,
			"hasMore": page.HasMore, "hasMoreBefore": page.HasMoreBefore,
		},
		"latestCursor":  page.LatestCursor,
		"latestEventId": page.LatestEventID,
	})
}

func (s *Server) turn(w http.ResponseWriter, r *http.Request, id string) {
	turnID := strings.TrimSpace(r.PathValue("turnId"))
	if turnID == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_turn_id", "turn id is required", nil)
		return
	}
	turn, err := s.store.Turn(id, turnID)
	if err != nil {
		if errors.Is(err, session.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "turn_not_found", "turn not found", nil)
			return
		}
		s.writeStoreError(w, err)
		return
	}
	value, err := s.store.Get(id)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"turn": turn, "latestEventId": value.LastEventID})
}

func (s *Server) writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, session.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "session_not_found", err.Error(), nil)
		return
	}
	writeAPIError(w, http.StatusInternalServerError, "session_store_failed", err.Error(), nil)
}

func requestMiddleware(allowOrigin func(origin, host string) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if mutatingMethod(r.Method) {
			contentType := r.Header.Get("Content-Type")
			if !strings.HasPrefix(strings.ToLower(contentType), "application/json") {
				writeAPIError(w, http.StatusUnsupportedMediaType, "json_required", "Content-Type must be application/json", nil)
				return
			}
			if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" && !allowOrigin(origin, r.Host) {
				writeAPIError(w, http.StatusForbidden, "origin_rejected", "browser origin does not match the daemon origin", nil)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1024*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON value")
		}
		return err
	}
	return nil
}

// decodeOptionalJSON decodes like decodeJSON but accepts an empty body,
// leaving target untouched. Endpoints with an all-optional body (resume)
// stay compatible with clients that send no body at all.
func decodeOptionalJSON(r *http.Request, target any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	if err := decodeJSON(r, target); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

func canonicalDirectory(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("cwd is required")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("cwd is not a directory")
	}
	return resolved, nil
}

func parseEventCursor(r *http.Request) (int64, error) {
	value := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if value == "" {
		value = strings.TrimSpace(r.URL.Query().Get("after"))
	}
	if value == "" {
		return 0, nil
	}
	cursor, err := strconv.ParseInt(value, 10, 64)
	if err != nil || cursor < 0 {
		return 0, fmt.Errorf("event cursor must be a non-negative integer")
	}
	return cursor, nil
}

// explicitEventCursor reports whether the request names a forward cursor, so
// backward pagination can reject the ambiguous combination instead of
// guessing which direction the caller meant.
func explicitEventCursor(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get("Last-Event-ID")) != "" ||
		strings.TrimSpace(r.URL.Query().Get("after")) != ""
}

// parseEventBackward reads the backward pagination parameters. backward is
// true when the request asks for a tail window, either with an explicit
// exclusive before cursor or with latest=true. latest=true is equivalent to
// before=head+1; it is expressed here as the maximum cursor so the store
// clamps it to the durable head captured by the same request. The two forms
// are mutually exclusive.
func parseEventBackward(query url.Values) (before int64, backward bool, err error) {
	value := strings.TrimSpace(query.Get("before"))
	hasBefore := value != ""
	if hasBefore {
		before, err = strconv.ParseInt(value, 10, 64)
		if err != nil || before < 1 {
			return 0, false, fmt.Errorf("before cursor must be a positive integer")
		}
	}
	latest := strings.TrimSpace(query.Get("latest"))
	hasLatest := false
	if latest != "" {
		on, parseErr := strconv.ParseBool(latest)
		if parseErr != nil {
			return 0, false, fmt.Errorf("latest must be a boolean")
		}
		hasLatest = on
	}
	if hasBefore && hasLatest {
		return 0, false, fmt.Errorf("before and latest are mutually exclusive")
	}
	if hasLatest {
		return math.MaxInt64, true, nil
	}
	return before, hasBefore, nil
}

// writeSSE frames an event using the default SSE message channel instead of
// a per-type `event:` field. The payload already carries the type, and a
// single channel guarantees that consumers receive every event — including
// event types they do not know about yet — instead of silently dropping
// events their subscription list does not name.
func writeSemanticSSE(w http.ResponseWriter, event session.Event, appendMode bool) error {
	data, err := json.Marshal(semantic.FrameFor(event, appendMode))
	if err != nil {
		return err
	}
	return writeSSEBounded(w, func() error {
		_, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.ID, data)
		return err
	})
}

const sseWriteTimeout = 5 * time.Second

func writeSSEHeartbeat(w http.ResponseWriter) error {
	return writeSSEBounded(w, func() error {
		_, err := fmt.Fprint(w, ": heartbeat\n\n")
		return err
	})
}

// writeSSEBounded prevents a client that stopped reading from pinning an SSE
// handler forever. This is also what makes subscriber overflow terminal at
// the real socket boundary: a blocked write is released, the handler returns,
// and the client can reconnect from its last contiguous durable cursor.
func writeSSEBounded(w http.ResponseWriter, write func() error) error {
	controller := http.NewResponseController(w)
	deadlineSet := controller.SetWriteDeadline(time.Now().Add(sseWriteTimeout)) == nil
	err := write()
	if err == nil && deadlineSet {
		// Clear successful writes so the 15-second idle heartbeat interval
		// does not inherit the previous event's deadline. On failure the
		// expired deadline intentionally remains in force while net/http
		// flushes and closes the response; clearing it there can block the
		// handler a second time on the same non-reading socket.
		_ = controller.SetWriteDeadline(time.Time{})
	}
	if err != nil && deadlineSet {
		// A timed-out HTTP/1 write can leave a partially buffered SSE frame
		// attached to a connection even after the handler returns. AgentHub
		// serves plaintext HTTP, so explicitly take ownership and close the
		// connection when possible; the client then observes a prompt EOF and
		// can reconnect instead of waiting on a permanently truncated frame.
		if connection, _, hijackErr := controller.Hijack(); hijackErr == nil {
			_ = connection.Close()
		}
	}
	return err
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string, details any) {
	requestID, _ := session.NewID("req")
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":      code,
			"message":   message,
			"retryable": retryableAPIError(code),
			"details":   details,
			"requestId": requestID,
		},
	})
}

func retryableAPIError(code string) bool {
	switch code {
	case "runtime_unavailable",
		"provider_unavailable",
		"provider_timeout",
		"provider_error",
		"session_store_failed",
		"session_archive_failed":
		return true
	default:
		return false
	}
}

func (s *Server) apiNotFound(w http.ResponseWriter, _ *http.Request) {
	writeAPIError(w, http.StatusNotFound, "route_not_found", "API route not found", nil)
}

func (s *Server) apiDocsRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed for this API route", nil)
		return
	}
	s.apiDocs(w, r)
}

func mutatingMethod(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

// allowsOrigin reports whether a mutating request carrying the given Origin
// header may proceed: either the origin is the daemon's own origin (a
// same-origin browser page) or it was explicitly trusted through
// Dependencies.AllowedOrigins (a reverse proxy whose public origin differs
// from the daemon address). Browsers forbid forging the Origin header, so
// an explicit allowlist keeps cross-site writes rejected.
func (s *Server) allowsOrigin(origin, host string) bool {
	if sameOrigin(origin, host) {
		return true
	}
	normalized, err := NormalizeOrigin(origin)
	return err == nil && s.allowedOrigins[normalized]
}

func sameOrigin(origin, host string) bool {
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Scheme, "http") && strings.EqualFold(parsed.Host, host)
}

// NormalizeOrigin canonicalizes an origin for comparison: the scheme must
// be http or https, a host is required, and user info, path, query, and
// fragment are rejected. The result is "scheme://host[:port]" with scheme
// and host lower-cased.
func NormalizeOrigin(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid origin %q: %w", trimmed, err)
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return "", fmt.Errorf("invalid origin %q: scheme must be http or https", trimmed)
	}
	if parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid origin %q: expected scheme://host[:port] with no user info, path, query, or fragment", trimmed)
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}

func spaHandler(root string) http.Handler {
	files := http.FileServer(http.Dir(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(root, filepath.Clean(r.URL.Path))
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(root, "index.html"))
	})
}

func spaHandlerFS(root fs.FS) http.Handler {
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(filepath.Clean(r.URL.Path), "/")
		if name != "." {
			if info, err := fs.Stat(root, name); err == nil && !info.IsDir() {
				files.ServeHTTP(w, r)
				return
			}
		}
		request := r.Clone(r.Context())
		request.URL.Path = "/"
		files.ServeHTTP(w, request)
	})
}
