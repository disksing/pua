package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/disksing/pua/internal/security"
)

// ServiceSecretResolver resolves a named secret at service start time. A
// resolver is deliberately called for every start/restart so a rotated secret
// is never persisted in a service definition or inherited from a previous
// process.
type ServiceSecretResolver interface {
	ResolveSecret(name string) (value string, source string, err error)
}

var (
	errServiceDisabled        = errors.New("service is disabled; enable it first")
	errServiceManagerStopping = errors.New("service manager is stopping")
)

type ServiceSecretResolverFunc func(string) (string, string, error)

func (f ServiceSecretResolverFunc) ResolveSecret(name string) (string, string, error) {
	if f == nil {
		return "", "", fmt.Errorf("secret resolver is unavailable")
	}
	return f(name)
}

// EnvironmentSecretResolver is a conservative default resolver. It checks an
// explicitly supplied map first and then an environment variable whose name is
// either the reference itself or PUA_SECRET_<reference>. Values are never
// written to disk by the supervisor.
type EnvironmentSecretResolver struct{ Values map[string]string }

func (r EnvironmentSecretResolver) ResolveSecret(name string) (string, string, error) {
	if value, ok := r.Values[name]; ok {
		return value, "configured", nil
	}
	if value, ok := os.LookupEnv(name); ok {
		return value, "environment:" + name, nil
	}
	key := "PUA_SECRET_" + strings.ToUpper(strings.NewReplacer(".", "_", "/", "_", ":", "_", "-", "_").Replace(name))
	if value, ok := os.LookupEnv(key); ok {
		return value, "environment:" + key, nil
	}
	return "", "", fmt.Errorf("secret %q is not available", name)
}

type ServiceManagerOptions struct {
	Resolver ServiceSecretResolver
	Now      func() time.Time
}

// serviceDefinitionStore is the filesystem boundary for durable service
// definitions. Keeping it manager-local lets tests inject precise failures
// without changing process or runtime-state persistence.
type serviceDefinitionStore struct {
	writeJSON func(string, any, os.FileMode, func(string, string) error) error
	rename    func(string, string) error
	remove    func(string) error
}

func defaultServiceDefinitionStore() serviceDefinitionStore {
	return serviceDefinitionStore{
		writeJSON: writeServiceJSONWithRename,
		rename:    os.Rename,
		remove:    os.Remove,
	}
}

// serviceRuntimeStateStore is the durable ownership boundary for service
// process state. Tests replace it to exercise read and write failures without
// relying on platform-specific filesystem permissions.
type serviceRuntimeStateStore struct {
	readFile  func(string) ([]byte, error)
	writeJSON func(string, any, os.FileMode) error
}

func defaultServiceRuntimeStateStore() serviceRuntimeStateStore {
	return serviceRuntimeStateStore{
		readFile:  os.ReadFile,
		writeJSON: writeServiceJSON,
	}
}

var errServiceBindingsPathEscape = errors.New("service bindings path escapes the workspace control directory")

const defaultServiceProcessTerminationGrace = 5 * time.Second

const (
	serviceExportIdentityCheckAttempts = 4
	serviceExportMaxBytes              = 1 << 20
)

var errServiceExportIdentityChanged = errors.New("service export hand-off changed during identity check")

type serviceProcessExit struct {
	err  error
	code int
}

type serviceFailureTransition struct {
	at                  time.Time
	lastError           string
	resetAfterStableRun bool
	requireAttention    bool
}

type serviceRuntime struct {
	config ServiceConfig
	// processConfig is an immutable copy of the definition used by the
	// currently owned process generation. config may move ahead during Apply,
	// but stop, failure, and orphan cleanup must continue to use this snapshot.
	processConfig         *ServiceConfig
	processCommandDigest  string
	status                ServiceStatus
	process               *exec.Cmd
	exit                  <-chan serviceProcessExit
	started               time.Time
	stableSince           time.Time
	environment           []string
	secretValues          []string
	secretNames           map[string]ServiceSecretMetadata
	exportSecrets         map[string]string
	exportMu              sync.Mutex
	exportAccepted        bool
	exportViolation       string
	exportCleanupFailure  string
	rejectedExportSecrets []string
	// suppressHookDiagnostics is latched for this process generation whenever
	// an export candidate cannot be inspected completely. Readiness and cleanup
	// hooks still run, but their untrusted output must not enter durable state or
	// events because it may contain an undiscovered value from the hand-off.
	suppressHookDiagnostics atomic.Bool
	// pendingExportKeys is manager-mutex state. It records public export
	// names whose accepted values moved after this generation became ready.
	// The committed bit prevents a dependent process from being stopped before
	// the producer's matching public status has been durably written.
	pendingExportKeys     map[string]struct{}
	exportKeysCommitted   bool
	redactor              *security.Redactor
	exports               ServiceExportFile
	logWriters            []*serviceLogWriter
	terminationPending    bool
	orphanRecoveryPending bool
	orphanCleanupComplete bool
	processOwnership      serviceProcessOwnership
}

type serviceRuntimeConfigSnapshot struct {
	runtime                 *serviceRuntime
	config                  ServiceConfig
	processConfig           *ServiceConfig
	processCommandDigest    string
	status                  ServiceStatus
	process                 *exec.Cmd
	exit                    <-chan serviceProcessExit
	started                 time.Time
	stableSince             time.Time
	environment             []string
	secretValues            []string
	secretNames             map[string]ServiceSecretMetadata
	exportSecrets           map[string]string
	exportAccepted          bool
	exportViolation         string
	exportCleanupFailure    string
	rejectedExportSecrets   []string
	suppressHookDiagnostics bool
	pendingExportKeys       map[string]struct{}
	exportKeysCommitted     bool
	redactor                *security.Redactor
	exports                 ServiceExportFile
	logWriters              []*serviceLogWriter
	terminationPending      bool
	orphanRecoveryPending   bool
	orphanCleanupComplete   bool
	processOwnership        serviceProcessOwnership
}

// persistedServiceRuntimeState keeps process cleanup provenance and unfinished
// lifecycle intent beside the public status without exposing either through
// ServiceStatus. Secret references may be present, but the primary command,
// resolved secret values, and child environment remain out of runtime state.
type persistedServiceRuntimeState struct {
	ServiceStatus
	ProcessConfig         *persistedServiceProcessConfig `json:"processConfig,omitempty"`
	OrphanRecoveryPending bool                           `json:"orphanRecoveryPending,omitempty"`
	// PendingExportChanges contains public variable names only. Persisting the
	// names with the accepted status lets reconstruction preserve an unfinished
	// invalidation without retaining another copy of public values or any
	// exported secret.
	PendingExportChanges []string `json:"pendingExportChanges,omitempty"`
	// SuppressHookDiagnostics preserves the fail-closed generation boundary
	// across manager reconstruction without retaining any hand-off bytes.
	SuppressHookDiagnostics bool `json:"suppressHookDiagnostics,omitempty"`
}

type persistedServiceProcessConfig struct {
	SchemaVersion int                           `json:"schemaVersion"`
	ID            string                        `json:"id"`
	CommandDigest string                        `json:"commandDigest"`
	CWD           string                        `json:"cwd,omitempty"`
	Environment   map[string]ServiceEnvironment `json:"environment,omitempty"`
	Exports       bool                          `json:"exports,omitempty"`
	Cleanup       *ServiceCleanupConfig         `json:"cleanup,omitempty"`
}

type serviceFileSnapshot struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

// ServiceManager owns all mutable service state for one Workspace. It is the
// only component that starts/stops service processes and writes runtime state.
type ServiceManager struct {
	root     string
	mu       sync.Mutex
	configs  map[string]ServiceConfig
	graph    serviceDependencyGraph
	runtimes map[string]*serviceRuntime
	resolver ServiceSecretResolver
	now      func() time.Time
	// processTerminationGrace is fixed in production and shortened only by
	// real-process tests that exercise graceful escalation deterministically.
	processTerminationGrace    time.Duration
	processPlatform            *serviceProcessPlatform
	definitionStore            serviceDefinitionStore
	definitionTransactionStore serviceDefinitionTransactionStore
	runtimeStateStore          serviceRuntimeStateStore
	exportOpenFile             func(string, int, os.FileMode) (*os.File, error)
	exportScrubPath            func(string) error
	stopping                   bool
	started                    bool
}

// NewServiceManager loads versioned service definitions for root. A missing
// .pua/services directory is a valid empty configuration for old Workspaces.
func NewServiceManager(root string, options ServiceManagerOptions) (*ServiceManager, error) {
	return newServiceManager(root, options, defaultServiceRuntimeStateStore())
}

func newServiceManager(root string, options ServiceManagerOptions, runtimeStateStore serviceRuntimeStateStore) (*ServiceManager, error) {
	root, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("workspace root is required")
	}
	defaults := defaultServiceRuntimeStateStore()
	if runtimeStateStore.readFile == nil {
		runtimeStateStore.readFile = defaults.readFile
	}
	if runtimeStateStore.writeJSON == nil {
		runtimeStateStore.writeJSON = defaults.writeJSON
	}
	m := &ServiceManager{root: filepath.Clean(root), configs: map[string]ServiceConfig{}, graph: serviceDependencyGraph{}, runtimes: map[string]*serviceRuntime{}, resolver: options.Resolver, now: options.Now, processTerminationGrace: defaultServiceProcessTerminationGrace, processPlatform: nativeServiceProcessPlatform(), definitionStore: defaultServiceDefinitionStore(), definitionTransactionStore: defaultServiceDefinitionTransactionStore(), runtimeStateStore: runtimeStateStore}
	if m.now == nil {
		m.now = time.Now
	}
	if err := m.loadLocked(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *ServiceManager) serviceProcessPlatform() *serviceProcessPlatform {
	if m != nil && m.processPlatform != nil {
		return m.processPlatform
	}
	return nativeServiceProcessPlatform()
}

func (m *ServiceManager) Root() string {
	if m == nil {
		return ""
	}
	return m.root
}

func (m *ServiceManager) loadLocked() error {
	if err := m.recoverServiceDefinitionTransactionLocked(); err != nil {
		return err
	}
	configs, err := m.readServiceDefinitionsLocked()
	if err != nil {
		return err
	}
	graph, err := validatedServiceDependencyGraph(m.root, configs)
	if err != nil {
		return err
	}
	m.configs = configs
	m.graph = graph
	for id, cfg := range configs {
		rt := m.runtimes[id]
		if rt == nil {
			rt = &serviceRuntime{config: cfg, status: ServiceStatus{SchemaVersion: serviceSchemaVersion, ID: id, Exports: ServiceExports{Variables: map[string]string{}}}}
			m.runtimes[id] = rt
		} else {
			rt.config = cfg
		}
		if err := m.loadStatusLocked(rt); err != nil {
			return fmt.Errorf("load service %s runtime state: %w", id, err)
		}
		rt.status.Dependencies = append([]string(nil), graph[id]...)
	}
	for id := range m.runtimes {
		if _, ok := configs[id]; !ok && m.runtimes[id].process == nil {
			delete(m.runtimes, id)
		}
	}
	if _, err := m.loadBindingsLocked(); err != nil {
		return err
	}
	return nil
}

func (m *ServiceManager) readServiceDefinitionsLocked() (map[string]ServiceConfig, error) {
	dir := filepath.Join(m.root, ".pua", serviceConfigDir)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return map[string]ServiceConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read service directory: %w", err)
	}
	if !pathWithinResolved(filepath.Join(m.root, ".pua"), dir) {
		return nil, errors.New("service directory must remain inside the workspace control directory")
	}
	configs := make(map[string]ServiceConfig)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || entry.Name() == serviceBindingsFile {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		servicePath := filepath.Join(dir, entry.Name())
		if !pathWithinResolved(dir, servicePath) {
			return nil, fmt.Errorf("service %s path escapes the workspace control directory", id)
		}
		data, err := os.ReadFile(servicePath)
		if err != nil {
			return nil, fmt.Errorf("read service %s: %w", id, err)
		}
		var cfg ServiceConfig
		if err := decodeStrictServiceJSON(bytes.NewReader(data), &cfg); err != nil {
			return nil, fmt.Errorf("decode service %s: %w", id, err)
		}
		if cfg.ID == "" {
			cfg.ID = id
		}
		if cfg.ID != id {
			return nil, fmt.Errorf("service %s has mismatched id %q", id, cfg.ID)
		}
		configs[id] = defaultServiceConfig(cfg)
	}
	return configs, nil
}

// loadBindingsLocked is the single persistence boundary for Workspace binding
// references. Each call returns newly allocated maps so callers cannot mutate
// another read, and a missing file has the same initialized default everywhere.
func (m *ServiceManager) loadBindingsLocked() (ServiceBindings, error) {
	path := serviceBindingsPath(m.root)
	if !pathWithinResolved(filepath.Join(m.root, ".pua"), path) {
		return ServiceBindings{}, errServiceBindingsPathEscape
	}
	bindings := ServiceBindings{
		SchemaVersion: serviceSchemaVersion,
		Variables:     map[string]string{},
		Secrets:       map[string]string{},
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return bindings, nil
	}
	if err != nil {
		return ServiceBindings{}, fmt.Errorf("read service bindings: %w", err)
	}
	if err := decodeStrictServiceJSON(bytes.NewReader(data), &bindings); err != nil {
		return ServiceBindings{}, fmt.Errorf("decode service bindings: %w", err)
	}
	if err := m.validateBindingsLocked(bindings); err != nil {
		return ServiceBindings{}, fmt.Errorf("service bindings: %w", err)
	}
	return bindings, nil
}

func (m *ServiceManager) loadStatusLocked(rt *serviceRuntime) error {
	path := filepath.Join(serviceRuntimePath(m.root, rt.status.ID), "state.json")
	if !pathWithinResolved(filepath.Join(m.root, ".pua"), path) {
		return errors.New("service runtime state path escapes the workspace control directory")
	}
	readFile := m.runtimeStateStore.readFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	data, err := readFile(path)
	if os.IsNotExist(err) {
		rt.status = initialServiceStatus(rt.config)
		return nil
	}
	if err != nil {
		return err
	}
	var persisted persistedServiceRuntimeState
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&persisted); err != nil {
		return err
	}
	status := persisted.ServiceStatus
	if status.ID != rt.config.ID {
		return fmt.Errorf("runtime state id %q does not match service %q", status.ID, rt.config.ID)
	}
	if status.SchemaVersion == 0 {
		status.SchemaVersion = serviceSchemaVersion
	}
	if status.Exports.Variables == nil {
		status.Exports.Variables = map[string]string{}
	}
	rt.orphanRecoveryPending = persisted.OrphanRecoveryPending || status.PID > 0 || status.ProcessGroup > 0
	rt.suppressHookDiagnostics.Store(persisted.SuppressHookDiagnostics)
	rt.orphanCleanupComplete = false
	if persisted.ProcessConfig != nil {
		if persisted.ProcessConfig.SchemaVersion != serviceSchemaVersion {
			return fmt.Errorf("runtime process config has unsupported schema version %d", persisted.ProcessConfig.SchemaVersion)
		}
		if persisted.ProcessConfig.ID != status.ID {
			return fmt.Errorf("runtime process config id %q does not match service %q", persisted.ProcessConfig.ID, status.ID)
		}
		if persisted.ProcessConfig.CommandDigest == "" || persisted.ProcessConfig.CommandDigest != status.CommandDigest {
			return errors.New("runtime process config does not match the owned process command")
		}
		processConfig := persisted.ProcessConfig.serviceConfig()
		if err := validateServiceConfig(m.root, processConfig); err != nil {
			return fmt.Errorf("runtime process config: %w", err)
		}
		if status.PID > 0 || status.ProcessGroup > 0 || persisted.OrphanRecoveryPending {
			rt.processConfig = &processConfig
			rt.processCommandDigest = persisted.ProcessConfig.CommandDigest
		}
	} else if (status.PID > 0 || status.ProcessGroup > 0) && status.CommandDigest != "" && serviceCommandDigest(rt.config) == status.CommandDigest {
		// Runtime states written before processConfig was introduced remain safe
		// to clean up when the current definition still identifies the process.
		processConfig := cloneServiceConfig(rt.config)
		rt.processConfig = &processConfig
		rt.processCommandDigest = status.CommandDigest
	}
	rt.status = status
	rt.exports = ServiceExportFile{SchemaVersion: serviceExportSchema, Variables: cloneStringMap(status.Exports.Variables)}
	if len(persisted.PendingExportChanges) > 0 {
		rt.pendingExportKeys = make(map[string]struct{}, len(persisted.PendingExportChanges))
		for _, name := range persisted.PendingExportChanges {
			if !environmentNamePattern.MatchString(name) {
				return fmt.Errorf("invalid pending export variable name %q", name)
			}
			rt.pendingExportKeys[name] = struct{}{}
		}
		rt.exportKeysCommitted = len(rt.pendingExportKeys) > 0
	}
	rt.secretNames = make(map[string]ServiceSecretMetadata, len(status.Exports.Secrets))
	for _, metadata := range status.Exports.Secrets {
		if metadata.Name != "" {
			rt.secretNames[metadata.Name] = metadata
		}
	}
	return nil
}

func initialServiceStatus(cfg ServiceConfig) ServiceStatus {
	state := ServiceStateDisabled
	if cfg.Enabled {
		state = ServiceStateStopped
	}
	return ServiceStatus{SchemaVersion: serviceSchemaVersion, ID: cfg.ID, Enabled: cfg.Enabled, State: state, Dependencies: append([]string(nil), cfg.DependsOn...), Readiness: ServiceReadinessStatus{Configured: cfg.Readiness != nil}, Cleanup: ServiceCleanupStatus{Configured: cfg.Cleanup != nil}, Exports: ServiceExports{Variables: map[string]string{}}}
}

// Start begins ownership of the Workspace's services and performs an initial
// reconciliation. Reconcile is safe to call from the pua serve loop after this.
func (m *ServiceManager) Start(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started && !m.stopping {
		return m.reconcileLocked(ctx)
	}
	m.stopping = false
	m.started = true
	return m.reconcileLocked(ctx)
}

// Stop suppresses future automatic restarts, terminates every owned process
// group, and runs configured cleanup commands before returning.
func (m *ServiceManager) Stop(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopping = true
	m.started = false
	var first error
	ids := m.sortedIDsLocked()
	for i := len(ids) - 1; i >= 0; i-- {
		rt := m.runtimes[ids[i]]
		if rt == nil {
			continue
		}
		cleanupErr := error(nil)
		if rt.process != nil || rt.status.ProcessGroup > 0 {
			cleanupErr = m.stopProcessLocked(ctx, rt, false)
		} else {
			cleanupErr = m.scrubActiveServiceExport(rt, rt.config, "scrub service export while stopping manager")
			if cleanupErr != nil {
				cleanupErr = errors.Join(exportProtocolViolationError(rt), cleanupErr)
			} else {
				cleanupErr = exportCleanupError(rt)
			}
		}
		if cleanupErr != nil && first == nil {
			first = cleanupErr
		}
		if cleanupErr != nil {
			rt.status.AttentionRequired = true
			rt.status.State = ServiceStateAttentionRequired
		} else if rt.config.Enabled {
			if rt.status.State != ServiceStateBackoff && rt.status.State != ServiceStateAttentionRequired {
				rt.status.State = ServiceStateStopped
			}
		} else {
			rt.status.State = ServiceStateDisabled
		}
		if rt.process == nil && cleanupErr == nil {
			rt.status.PID, rt.status.ProcessGroup = 0, 0
		}
		if err := m.persistStatusLocked(rt); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Reconcile observes process exits, dependency readiness, readiness commands,
// exports, and due backoff timers. It never starts a service whose dependency
// is not ready.
func (m *ServiceManager) Reconcile(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping {
		return nil
	}
	return m.reconcileLocked(ctx)
}

func (m *ServiceManager) reconcileLocked(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	result := m.recoverOrphansLocked()
	if err := m.invalidateStaleServiceGenerationsLocked(ctx); err != nil {
		return errors.Join(result, err)
	}
	manualStopBlocked, manualStopErr := m.reconcileManualStopChainsLocked(ctx)
	ids := m.sortedIDsLocked()
	for _, id := range ids {
		rt := m.runtimes[id]
		if rt == nil || rt.orphanRecoveryPending {
			continue
		}
		if _, blocked := manualStopBlocked[id]; blocked {
			continue
		}
		for _, dependency := range m.graph[id] {
			if _, blocked := manualStopBlocked[dependency]; blocked {
				manualStopBlocked[id] = struct{}{}
				break
			}
		}
		if _, blocked := manualStopBlocked[id]; blocked {
			continue
		}
		if err := m.reconcileOneLocked(ctx, rt, m.graph[id]); err != nil {
			result = errors.Join(result, err)
		}
	}
	return errors.Join(result, manualStopErr)
}

// reconcileManualStopChainsLocked resumes a StopService operation that
// persisted its operator intent but failed before reaching the selected
// service. All live manual-stop targets are combined into one reverse-
// topological pass so overlapping chains are stopped once in stable order. A
// failed chain returns its target/dependent closure so ordinary reconciliation
// can continue for disconnected services without relaunching a partial stop.
func (m *ServiceManager) reconcileManualStopChainsLocked(ctx context.Context) (map[string]struct{}, error) {
	targets := make(map[string]struct{})
	for id, rt := range m.runtimes {
		if rt == nil || !rt.status.ManualStop || rt.orphanRecoveryPending || (rt.process == nil && rt.status.ProcessGroup <= 0) {
			continue
		}
		targets[id] = struct{}{}
	}
	if len(targets) == 0 {
		return nil, nil
	}

	targetImpacts := make(map[string]map[string]struct{}, len(targets))
	impacted := make(map[string]struct{})
	for target := range targets {
		closure := serviceDependencyChangeSet(m.graph, m.graph, map[string]struct{}{target: {}})
		targetImpacts[target] = closure
		for id := range closure {
			impacted[id] = struct{}{}
		}
	}
	stopOrder, err := serviceGraphSubsetStopOrder(m.graph, impacted)
	if err != nil {
		return impacted, err
	}
	blocked := make(map[string]struct{})
	blockFailureAt := func(failedID string) {
		for _, closure := range targetImpacts {
			if _, affected := closure[failedID]; !affected {
				continue
			}
			for id := range closure {
				blocked[id] = struct{}{}
			}
		}
	}
	var result error
	for _, id := range stopOrder {
		rt := m.runtimes[id]
		if rt == nil || rt.orphanRecoveryPending || (rt.process == nil && rt.status.ProcessGroup <= 0) {
			continue
		}
		// When a prior stop in this pass failed, keep its dependencies alive.
		// Independent chains remain eligible, allowing multiple durable targets
		// to make progress without violating dependent-first shutdown.
		if m.hasLiveServiceDependentLocked(id) {
			continue
		}

		_, manualTarget := targets[id]
		retryingTermination := rt.terminationPending
		if manualTarget {
			err = m.stopProcessLocked(ctx, rt, true)
			if err == nil {
				rt.status.Readiness.Ready = false
				if retryingTermination {
					rt.status.AttentionRequired = false
					rt.status.LastError = ""
				}
				if !rt.status.AttentionRequired {
					rt.status.State = ServiceStateStopped
				}
				err = m.persistStatusLocked(rt)
			}
		} else {
			err = m.stopRuntimeForDependencyChangeLocked(ctx, rt)
			if err == nil && retryingTermination {
				rt.status.AttentionRequired = false
				rt.status.LastError = ""
				rt.status.State = ServiceStateStopped
				err = m.persistStatusLocked(rt)
			}
		}
		if err != nil {
			blockFailureAt(id)
			result = errors.Join(result, fmt.Errorf("retry manual stop at service %q: %w", id, err))
		}
	}
	if result == nil {
		for _, id := range stopOrder {
			if _, target := targets[id]; !target {
				continue
			}
			rt := m.runtimes[id]
			if rt != nil && (rt.process != nil || rt.status.ProcessGroup > 0) {
				blockFailureAt(id)
				return blocked, fmt.Errorf("retry manual stop at service %q: live dependent still blocks shutdown", id)
			}
		}
	}
	return blocked, result
}

func (m *ServiceManager) hasLiveServiceDependentLocked(id string) bool {
	dependents := serviceDependencyChangeSet(m.graph, m.graph, map[string]struct{}{id: {}})
	delete(dependents, id)
	for candidateID := range dependents {
		candidate := m.runtimes[candidateID]
		if candidate == nil || (candidate.process == nil && candidate.status.ProcessGroup <= 0) {
			continue
		}
		return true
	}
	return false
}

// invalidateStaleServiceGenerationsLocked completes an Apply whose desired
// definition was persisted but whose first dependent-first stop attempt did
// not finish. Current-manager processes retain their immutable processConfig,
// so reconciliation can retry the whole affected chain without ever starting
// the changed dependency ahead of a stale consumer.
func (m *ServiceManager) invalidateStaleServiceGenerationsLocked(ctx context.Context) error {
	changed := make(map[string]struct{})
	for id, rt := range m.runtimes {
		if rt == nil || rt.process == nil || rt.processConfig == nil {
			continue
		}
		if serviceConfigDigest(*rt.processConfig) != serviceConfigDigest(rt.config) {
			changed[id] = struct{}{}
		}
	}
	if len(changed) == 0 {
		return nil
	}
	impacted := serviceDependencyChangeSet(m.graph, m.graph, changed)
	stopOrder, err := serviceGraphSubsetStopOrder(m.graph, impacted)
	if err != nil {
		return err
	}
	for _, id := range stopOrder {
		if err := m.stopRuntimeForDependencyChangeLocked(ctx, m.runtimes[id]); err != nil {
			return err
		}
	}
	return nil
}

func (m *ServiceManager) reconcileOneLocked(ctx context.Context, rt *serviceRuntime, dependencies []string) error {
	status := &rt.status
	cfg := rt.config
	status.Enabled = cfg.Enabled
	status.Dependencies = append([]string(nil), dependencies...)
	if rt.terminationPending && rt.process != nil {
		if err := m.stopProcessLocked(ctx, rt, status.ManualStop); err != nil {
			return err
		}
	}
	if rt.process == nil && status.ProcessGroup > 0 {
		status.State = ServiceStateAttentionRequired
		status.AttentionRequired = true
		if status.LastError == "" {
			status.LastError = fmt.Sprintf("service process group %d ownership is unresolved", status.ProcessGroup)
		}
		return m.persistStatusLocked(rt)
	}
	if !cfg.Enabled {
		if rt.process != nil {
			if err := m.stopProcessLocked(ctx, rt, true); err != nil {
				status.AttentionRequired = true
				status.State = ServiceStateAttentionRequired
			}
		}
		if !status.AttentionRequired {
			status.State = ServiceStateDisabled
		}
		status.ManualStop = false
		if rt.process == nil && !status.AttentionRequired {
			status.PID, status.ProcessGroup = 0, 0
		}
		return m.persistStatusLocked(rt)
	}
	if status.ManualStop && rt.process == nil {
		status.State = ServiceStateStopped
		return m.persistStatusLocked(rt)
	}
	if rt.process != nil {
		select {
		case exit := <-rt.exit:
			if err := m.handleProcessExitLocked(ctx, rt, exit); err != nil {
				return err
			}
		default:
		}
		if rt.process == nil {
			return nil
		}
	}
	if !m.dependenciesReadyLocked(dependencies) {
		if rt.process != nil {
			if err := m.stopProcessLocked(ctx, rt, false); err != nil {
				status.AttentionRequired = true
				status.State = ServiceStateAttentionRequired
				return m.persistStatusLocked(rt)
			}
		}
		if status.AttentionRequired {
			status.State = ServiceStateAttentionRequired
		} else {
			status.State = ServiceStateBlocked
		}
		status.Readiness.Ready = false
		return m.persistStatusLocked(rt)
	}
	now := m.now()
	if rt.process == nil {
		if status.State == ServiceStateBackoff || status.State == ServiceStateAttentionRequired {
			if next, err := time.Parse(time.RFC3339Nano, status.NextRetryAt); err == nil && now.Before(next) {
				return nil
			}
		}
		return m.startProcessLocked(ctx, rt)
	}
	return m.observeReadyLocked(ctx, rt)
}

func (m *ServiceManager) dependenciesReadyLocked(dependencies []string) bool {
	for _, dep := range dependencies {
		rt := m.runtimes[dep]
		if rt == nil || rt.status.State != ServiceStateReady {
			return false
		}
	}
	return true
}

func (m *ServiceManager) startProcessLocked(ctx context.Context, rt *serviceRuntime) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := defaultServiceConfig(rt.config)
	processConfig := cloneServiceConfig(cfg)
	rt.processConfig = &processConfig
	rt.processCommandDigest = serviceCommandDigest(cfg)
	// A failed resolution must never fall back to a previous generation's
	// resolved environment or secrets when it invokes this generation's
	// cleanup hook.
	rt.environment = nil
	rt.secretValues = nil
	rt.secretNames = nil
	rt.exportSecrets = nil
	rt.exportAccepted = false
	rt.exportViolation = ""
	rt.rejectedExportSecrets = nil
	rt.suppressHookDiagnostics.Store(false)
	rt.pendingExportKeys = nil
	rt.exportKeysCommitted = false
	rt.redactor = nil
	rt.exports = ServiceExportFile{SchemaVersion: serviceExportSchema, Variables: map[string]string{}}
	rt.status.Exports = publicExports(rt.exports, nil)
	env, secrets, names, exports, err := m.resolveEnvironmentLocked(cfg)
	if err != nil {
		return m.failStartLocked(ctx, rt, err)
	}
	if !pathWithinResolved(filepath.Join(m.root, ".pua"), serviceRuntimePath(m.root, cfg.ID)) {
		return m.failStartLocked(ctx, rt, errors.New("service runtime path escapes the workspace control directory"))
	}
	if err := os.MkdirAll(serviceRuntimePath(m.root, cfg.ID), 0o700); err != nil {
		return m.failStartLocked(ctx, rt, err)
	}
	if err := m.scrubActiveServiceExport(rt, cfg, "clear previous service export"); err != nil {
		return m.failStartLocked(ctx, rt, err)
	}
	rt.exportCleanupFailure = ""
	command := append(append([]string{}, cfg.Command...), cfg.Args...)
	cmd := exec.Command(command[0], command[1:]...)
	cwd := cfg.CWD
	if cwd == "" {
		cwd = m.root
	}
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(m.root, cwd)
	}
	cmd.Dir = filepath.Clean(cwd)
	if !pathWithinResolved(m.root, cmd.Dir) {
		return m.failStartLocked(ctx, rt, errors.New("service cwd escapes the workspace"))
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A service descendant may inherit the copy pipes that os/exec creates for
	// the redacting log writers. Once the leader exits, bound the final pipe
	// drain so the descendant cannot hide that exit from reconciliation. The
	// existing unexpected-exit path then verifies and reaps the residual group.
	cmd.WaitDelay = serviceCommandWaitDelay
	instanceToken := valueFromEnvironment(env, serviceInstanceTokenEnvironment)
	var identityMarker *os.File
	identityMarkerPath := ""
	processPlatform := m.serviceProcessPlatform()
	if processPlatform.identityMarkerRequired {
		identityMarker, identityMarkerPath, err = openServiceProcessIdentityMarker(m.root, cfg.ID, instanceToken)
		if err != nil {
			return m.failStartLocked(ctx, rt, err)
		}
		cmd.ExtraFiles = []*os.File{identityMarker}
	}
	redactor := security.NewRedactor(secrets...)
	stdoutSink := newServiceLogSink(filepath.Join(serviceRuntimePath(m.root, cfg.ID), "stdout.log"), cfg.LogRotation)
	stderrSink := newServiceLogSink(filepath.Join(serviceRuntimePath(m.root, cfg.ID), "stderr.log"), cfg.LogRotation)
	gatedLogs := requiresInitialExport(cfg)
	var exportGuard func() error
	if gatedLogs {
		// The pipe copier runs outside the manager mutex. Capture this process
		// generation's immutable config so a concurrent Apply cannot make its
		// export gate observe the replacement definition.
		exportGuard = func() error { return m.guardServiceLogExportForConfig(rt, cfg) }
	}
	stdoutWriter := newServiceLogWriter(stdoutSink, redactor, gatedLogs, exportGuard)
	stderrWriter := newServiceLogWriter(stderrSink, redactor, gatedLogs, exportGuard)
	cmd.Stdout, cmd.Stderr = stdoutWriter, stderrWriter
	// Keep the resolved environment in the runtime before Start so a failed
	// exec can still run cleanup with the same environment and redact any
	// resolver-provided secret from its diagnostics.
	rt.environment = env
	rt.secretValues = secrets
	rt.secretNames = names
	rt.exportSecrets = map[string]string{}
	rt.exportAccepted = false
	rt.exportViolation = ""
	rt.exportCleanupFailure = ""
	rt.rejectedExportSecrets = nil
	rt.redactor = redactor
	rt.exports = exports
	rt.status.Exports = publicExports(exports, names)
	if err := cmd.Start(); err != nil {
		if identityMarker != nil {
			_ = identityMarker.Close()
			_ = os.Remove(identityMarkerPath)
		}
		_ = stdoutWriter.Close()
		_ = stderrWriter.Close()
		return m.failStartLocked(ctx, rt, err)
	}
	if identityMarker != nil {
		_ = identityMarker.Close()
	}
	processStartID := ""
	if processPlatform.identityInspectionAvailable {
		processIdentity, identityErr := processPlatform.readProcessIdentity(cmd.Process.Pid)
		if identityErr != nil || processIdentity.processGroup != cmd.Process.Pid || processIdentity.startID == "" {
			_ = terminateProcessGroup(cmd.Process.Pid, true)
			_ = cmd.Wait()
			_ = os.Remove(identityMarkerPath)
			_ = stdoutWriter.Close()
			_ = stderrWriter.Close()
			if identityErr == nil {
				identityErr = errProcessIdentityUnavailable
			}
			return m.failStartLocked(ctx, rt, fmt.Errorf("record service process identity: %w", identityErr))
		}
		processStartID = processIdentity.startID
	}
	exit := make(chan serviceProcessExit, 1)
	go func() {
		err := cmd.Wait()
		code := 0
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		exit <- serviceProcessExit{err: err, code: code}
	}()
	rt.config = cfg
	rt.process = cmd
	rt.exit = exit
	rt.started = m.now()
	rt.stableSince = rt.started
	rt.environment = env
	rt.secretValues = secrets
	rt.secretNames = names
	rt.exportSecrets = map[string]string{}
	rt.exports = exports
	rt.logWriters = []*serviceLogWriter{stdoutWriter, stderrWriter}
	rt.processOwnership = serviceProcessOwnershipCurrentManager
	rt.status.State = ServiceStateStarting
	rt.status.PID = cmd.Process.Pid
	rt.status.ProcessGroup = cmd.Process.Pid
	rt.status.StartedAt = rt.started.Format(time.RFC3339Nano)
	rt.status.ExitedAt = ""
	rt.status.ExitCode = 0
	rt.status.ExitError = ""
	rt.status.LastError = ""
	rt.status.NextRetryAt = ""
	rt.status.CommandDigest = serviceCommandDigest(cfg)
	rt.status.InstanceToken = instanceToken
	rt.status.ProcessStartID = processStartID
	rt.status.Readiness = ServiceReadinessStatus{Configured: cfg.Readiness != nil}
	rt.status.Cleanup = ServiceCleanupStatus{Configured: cfg.Cleanup != nil}
	rt.status.Exports = publicExports(exports, names)
	rt.status.UpdatedAt = rt.started.Format(time.RFC3339Nano)
	if err := m.persistStatusLocked(rt); err != nil {
		stopErr := m.stopProcessLocked(ctx, rt, false)
		message := security.NewRedactor(rt.secretValues...).RedactString(err.Error())
		rt.status.State = ServiceStateAttentionRequired
		rt.status.AttentionRequired = true
		rt.status.Readiness.Ready = false
		rt.status.LastError = message
		return errors.Join(err, stopErr)
	}
	_ = m.appendEventLocked(rt, map[string]any{"type": "started", "pid": cmd.Process.Pid, "time": rt.status.StartedAt})
	return m.observeReadyLocked(ctx, rt)
}

func (m *ServiceManager) failStartLocked(ctx context.Context, rt *serviceRuntime, cause error) error {
	message := security.NewRedactor(rt.secretValues...).RedactString(cause.Error())
	transitionAt := m.now()
	_ = m.appendEventLocked(rt, map[string]any{"type": "start_failed", "error": message, "time": transitionAt.Format(time.RFC3339Nano)})
	requireAttention := rt.exportCleanupFailure != ""
	if cleanupErr := m.runCleanupLocked(ctx, rt); cleanupErr != nil {
		cleanupMessage := security.NewRedactor(rt.secretValues...).RedactString(cleanupErr.Error())
		message += "; " + cleanupMessage
		requireAttention = true
	}
	rt.processConfig = nil
	rt.processCommandDigest = ""
	return m.transitionServiceFailureLocked(rt, serviceFailureTransition{
		at:               transitionAt,
		lastError:        message,
		requireAttention: requireAttention,
	})
}

func (m *ServiceManager) observeReadyLocked(ctx context.Context, rt *serviceRuntime) error {
	if rt.process == nil {
		return nil
	}
	// A prior pass may have committed an export update but failed partway
	// through stopping its consumers. Finish that dependent-first invalidation
	// before accepting another update or allowing the outer reconcile pass to
	// start an already-stopped descendant.
	if rt.exportKeysCommitted && len(rt.pendingExportKeys) > 0 {
		if err := m.invalidateChangedExportDependentsLocked(ctx, rt); err != nil {
			return err
		}
	}
	if err := m.exportProtocolErrorLocked(rt); err != nil {
		return m.readinessFailedLocked(ctx, rt, err)
	}
	now := m.now()
	if rt.stableSince.IsZero() {
		rt.stableSince = now
	}
	if rt.config.Restart.ResetAfter > 0 && now.Sub(rt.stableSince) >= rt.config.Restart.ResetAfter && rt.status.FailureCount > 0 {
		rt.status.FailureCount = 0
		rt.status.AttentionRequired = false
	}
	if rt.config.Readiness == nil {
		wasReady := rt.status.State == ServiceStateReady && rt.status.Readiness.Ready
		previousVariables := cloneStringMap(rt.exports.Variables)
		exports, err := m.readExportsLocked(rt)
		if err != nil && requiresInitialExport(rt.config) && strings.Contains(err.Error(), "initial export") {
			exports, err = m.waitForInitialExportLocked(ctx, rt)
		}
		if err != nil {
			return m.readinessFailedLocked(ctx, rt, err)
		}
		m.acceptObservedExportsLocked(rt, previousVariables, exports, wasReady)
		if err := m.releaseStartupLogsLocked(rt); err != nil {
			return m.readinessFailedLocked(ctx, rt, err)
		}
		rt.status.State = ServiceStateReady
		rt.status.Readiness.Ready = true
		rt.status.Exports = publicExports(rt.exports, rt.secretNames)
		return m.persistAndInvalidateChangedExportsLocked(ctx, rt)
	}
	last, _ := time.Parse(time.RFC3339Nano, rt.status.Readiness.LastCheck)
	if !last.IsZero() && now.Sub(last) < rt.config.Readiness.Interval && rt.status.Readiness.Ready && len(rt.pendingExportKeys) == 0 {
		return nil
	}
	wasReady := rt.status.State == ServiceStateReady && rt.status.Readiness.Ready
	previousVariables := cloneStringMap(rt.exports.Variables)
	exports, err := m.readExportsLocked(rt)
	if err != nil && !rt.status.Readiness.Ready && strings.Contains(err.Error(), "initial export") {
		exports, err = m.waitForInitialExportLocked(ctx, rt)
	}
	if err != nil {
		return m.readinessFailedLocked(ctx, rt, err)
	}
	m.acceptObservedExportsLocked(rt, previousVariables, exports, wasReady)
	if err := m.runReadinessLocked(ctx, rt); err != nil {
		return m.readinessFailedLocked(ctx, rt, err)
	}
	// A process that does not declare exports may still write to the control
	// path while its readiness command is running. Observe the path again after
	// that synchronization point so the bytes are scrubbed without ever being
	// decoded or published.
	if !rt.config.Exports {
		exports, err = m.readExportsLocked(rt)
		if err != nil {
			return m.readinessFailedLocked(ctx, rt, err)
		}
		m.acceptObservedExportsLocked(rt, previousVariables, exports, wasReady)
	}
	if err := m.releaseStartupLogsLocked(rt); err != nil {
		return m.readinessFailedLocked(ctx, rt, err)
	}
	rt.status.State = ServiceStateReady
	rt.status.Readiness.Ready = true
	rt.status.Readiness.LastCheck = now.Format(time.RFC3339Nano)
	rt.status.Readiness.LastError = ""
	rt.status.Exports = publicExports(rt.exports, rt.secretNames)
	rt.status.UpdatedAt = now.Format(time.RFC3339Nano)
	return m.persistAndInvalidateChangedExportsLocked(ctx, rt)
}

func (m *ServiceManager) acceptObservedExportsLocked(rt *serviceRuntime, previousVariables map[string]string, export ServiceExportFile, wasReady bool) {
	rt.exports = export
	if !wasReady || equalStringMap(previousVariables, export.Variables) {
		return
	}
	if rt.pendingExportKeys == nil {
		rt.pendingExportKeys = map[string]struct{}{}
	}
	for name := range changedStringMapKeys(previousVariables, export.Variables) {
		rt.pendingExportKeys[name] = struct{}{}
	}
	rt.exportKeysCommitted = false
}

func (m *ServiceManager) persistAndInvalidateChangedExportsLocked(ctx context.Context, rt *serviceRuntime) error {
	pending := len(rt.pendingExportKeys) > 0
	if pending {
		rt.exportKeysCommitted = true
	}
	if err := m.persistStatusLocked(rt); err != nil {
		if pending {
			rt.exportKeysCommitted = false
		}
		return err
	}
	if !pending {
		return nil
	}
	return m.invalidateChangedExportDependentsLocked(ctx, rt)
}

// invalidateChangedExportDependentsLocked restarts only consumers whose
// environment names a changed public export, plus their transitive consumers.
// The producer itself and graph-only dependents that do not consume the
// changed value keep their process generation. Pending changes are cleared
// only after every running consumer has stopped successfully, so a later
// reconcile retries a partial invalidation before starting descendants.
func (m *ServiceManager) invalidateChangedExportDependentsLocked(ctx context.Context, producer *serviceRuntime) error {
	if producer == nil || !producer.exportKeysCommitted || len(producer.pendingExportKeys) == 0 {
		return nil
	}
	direct := make(map[string]struct{})
	for id, cfg := range m.configs {
		if id != producer.config.ID && serviceConfigReferencesChangedExports(cfg, producer.config.ID, producer.pendingExportKeys) {
			direct[id] = struct{}{}
		}
	}
	if len(direct) == 0 {
		producer.pendingExportKeys = nil
		producer.exportKeysCommitted = false
		return nil
	}
	impacted := serviceDependencyChangeSet(m.graph, m.graph, direct)
	stopOrder, err := serviceGraphSubsetStopOrder(m.graph, impacted)
	if err != nil {
		return err
	}
	for _, id := range stopOrder {
		candidate := m.runtimes[id]
		if candidate == nil || candidate.status.ManualStop || !candidate.config.Enabled || (candidate.process == nil && candidate.status.ProcessGroup <= 0) {
			continue
		}
		retryingTermination := candidate.terminationPending
		if err := m.stopRuntimeForDependencyChangeLocked(ctx, candidate); err != nil {
			return fmt.Errorf("stop service %q after %q export change: %w", id, producer.config.ID, err)
		}
		if retryingTermination {
			candidate.status.AttentionRequired = false
			candidate.status.LastError = ""
			candidate.status.State = ServiceStateStopped
			if err := m.persistStatusLocked(candidate); err != nil {
				return err
			}
		}
	}
	producer.pendingExportKeys = nil
	producer.exportKeysCommitted = false
	return nil
}

func changedStringMapKeys(left, right map[string]string) map[string]struct{} {
	changed := make(map[string]struct{})
	for key, value := range left {
		if other, ok := right[key]; !ok || other != value {
			changed[key] = struct{}{}
		}
	}
	for key, value := range right {
		if other, ok := left[key]; !ok || other != value {
			changed[key] = struct{}{}
		}
	}
	return changed
}

func serviceConfigReferencesChangedExports(cfg ServiceConfig, producerID string, changed map[string]struct{}) bool {
	for _, entry := range cfg.Environment {
		for _, match := range serviceTemplatePattern.FindAllStringSubmatch(entry.Template, -1) {
			if len(match) != 3 || match[1] != producerID {
				continue
			}
			if _, ok := changed[match[2]]; ok {
				return true
			}
		}
	}
	return false
}

func (m *ServiceManager) waitForInitialExportLocked(ctx context.Context, rt *serviceRuntime) (ServiceExportFile, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := time.Duration(0)
	if rt.config.Readiness != nil {
		timeout = rt.config.Readiness.Timeout
	}
	if timeout <= 0 {
		timeout = defaultReadyTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		exports, err := m.readExportsLocked(rt)
		if err == nil {
			return exports, nil
		}
		if !strings.Contains(err.Error(), "initial export") {
			return ServiceExportFile{}, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ServiceExportFile{}, err
		}
		if remaining > 50*time.Millisecond {
			remaining = 50 * time.Millisecond
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ServiceExportFile{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *ServiceManager) readExportsLocked(rt *serviceRuntime) (ServiceExportFile, error) {
	rt.exportMu.Lock()
	defer rt.exportMu.Unlock()
	cfg := rt.config
	if rt.processConfig != nil && (rt.process != nil || rt.status.ProcessGroup > 0) {
		cfg = *rt.processConfig
	}
	if !cfg.Exports {
		return m.readExportsWithGateLocked(rt, cfg, false)
	}
	m.promoteRejectedExportSecretsLocked(rt)
	if failure := exportFailureLocked(rt); failure != nil {
		cleanupErr := m.scrubActiveServiceExportLocked(rt, cfg, "scrub service export after protocol violation")
		return ServiceExportFile{}, errors.Join(failure, cleanupErr)
	}
	return m.readExportsWithGateLocked(rt, cfg, false)
}

// guardServiceLogExportForConfig runs in the os/exec pipe copier before raw
// service bytes enter the streaming redactor. Atomic export replacements are
// therefore observed in process order before output written after the
// replacement can be persisted. It never takes the manager mutex because
// shutdown closes writers while holding that mutex.
func (m *ServiceManager) guardServiceLogExportForConfig(rt *serviceRuntime, cfg ServiceConfig) error {
	rt.exportMu.Lock()
	defer rt.exportMu.Unlock()
	if !cfg.Exports {
		_, err := m.readExportsWithGateLocked(rt, cfg, true)
		return err
	}
	if !rt.exportAccepted {
		return nil
	}
	if failure := exportFailureLocked(rt); failure != nil {
		cleanupErr := m.scrubActiveServiceExportLocked(rt, cfg, "scrub service export after protocol violation")
		return errors.Join(failure, cleanupErr)
	}
	_, err := m.readExportsWithGateLocked(rt, cfg, true)
	return err
}

func (m *ServiceManager) readExportsWithGateLocked(rt *serviceRuntime, cfg ServiceConfig, fromLog bool) (ServiceExportFile, error) {
	path := filepath.Join(serviceRuntimePath(m.root, cfg.ID), "export.json")
	if !pathWithinResolved(filepath.Join(m.root, ".pua"), path) {
		return ServiceExportFile{}, m.rejectExportLocked(rt, errors.New("service export path escapes the workspace control directory"), nil, fromLog)
	}
	if !cfg.Exports {
		empty := ServiceExportFile{SchemaVersion: serviceExportSchema, Variables: map[string]string{}}
		if err := scrubAndRemoveServiceExportPath(path); err != nil {
			return empty, fmt.Errorf("scrub disabled service export hand-off: %w", err)
		}
		return empty, nil
	}
	var handoff *serviceExportHandoff
	var err error
	for attempt := 0; attempt < serviceExportIdentityCheckAttempts; attempt++ {
		handoff, err = openServiceExportHandoffWithOpen(path, m.exportOpenFile)
		if !errors.Is(err, errServiceExportIdentityChanged) {
			break
		}
	}
	if errors.Is(err, errServiceExportIdentityChanged) {
		err = errors.New("service export hand-off changed repeatedly during identity checks")
	}
	if os.IsNotExist(err) {
		if requiresInitialExport(cfg) {
			message := "service has not written its initial export"
			if rt.exportAccepted {
				message = "service removed its accepted export hand-off"
			}
			return ServiceExportFile{}, m.rejectExportLocked(rt, errors.New(message), nil, fromLog)
		}
		return ServiceExportFile{SchemaVersion: serviceExportSchema, Variables: map[string]string{}}, nil
	}
	if err != nil {
		// No later hook output is safe to publish when an existing hand-off
		// changed too often or could not be opened and read through the verified
		// descriptor. Its uninspected bytes may include a value printed by the
		// readiness or cleanup command.
		rt.suppressHookDiagnostics.Store(true)
		return ServiceExportFile{}, m.rejectExportLocked(rt, err, nil, fromLog)
	}
	defer handoff.file.Close()
	return m.readExportHandoffWithGateLocked(rt, handoff, fromLog)
}

type serviceExportHandoff struct {
	path string
	file *os.File
	data []byte
}

// openServiceExportHandoff keeps the accepted inode open through validation and
// scrubbing. A service may atomically replace the pathname after this read; the
// replacement must remain untouched so the next log guard can inspect it.
func openServiceExportHandoff(path string) (*serviceExportHandoff, error) {
	return openServiceExportHandoffWithOpen(path, nil)
}

func openServiceExportHandoffWithOpen(path string, openFile func(string, int, os.FileMode) (*os.File, error)) (*serviceExportHandoff, error) {
	if openFile == nil {
		openFile = os.OpenFile
	}
	readFile, err := openFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	readInfo, err := readFile.Stat()
	if err != nil {
		_ = readFile.Close()
		return nil, err
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		_ = readFile.Close()
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: path disappeared after opening", errServiceExportIdentityChanged)
		}
		return nil, err
	}
	if !readInfo.Mode().IsRegular() || !pathInfo.Mode().IsRegular() {
		_ = readFile.Close()
		return nil, errors.New("service export hand-off must be a regular file")
	}
	if !os.SameFile(readInfo, pathInfo) {
		_ = readFile.Close()
		return nil, errServiceExportIdentityChanged
	}
	data, err := readBoundedServiceExport(readFile)
	if err != nil {
		cleanupErr := removeVerifiedServiceExportHandoff(path, readFile)
		_ = readFile.Close()
		if cleanupErr != nil {
			return nil, fmt.Errorf("read service export hand-off: %v; remove unreadable hand-off: %w", err, cleanupErr)
		}
		return nil, fmt.Errorf("read service export hand-off: %w", err)
	}
	// Exporters may deliberately create a read-only hand-off. Changing the mode
	// through the verified descriptor targets that inode even if the exporter
	// concurrently replaces the pathname.
	if err := readFile.Chmod(0o600); err != nil {
		cleanupErr := removeVerifiedServiceExportHandoff(path, readFile)
		_ = readFile.Close()
		if cleanupErr != nil {
			return nil, fmt.Errorf("prepare service export hand-off for scrubbing: %v; remove hand-off: %w", err, cleanupErr)
		}
		return nil, fmt.Errorf("prepare service export hand-off for scrubbing: %w", err)
	}
	writeFile, err := openFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		cleanupErr := removeVerifiedServiceExportHandoff(path, readFile)
		_ = readFile.Close()
		if os.IsNotExist(err) && cleanupErr == nil {
			return nil, fmt.Errorf("%w: path disappeared before writable open", errServiceExportIdentityChanged)
		}
		if cleanupErr != nil {
			return nil, fmt.Errorf("open service export hand-off for scrubbing: %v; remove hand-off: %w", err, cleanupErr)
		}
		return nil, fmt.Errorf("open service export hand-off for scrubbing: %w", err)
	}
	writeInfo, err := writeFile.Stat()
	if err != nil || !os.SameFile(readInfo, writeInfo) {
		_ = writeFile.Close()
		cleanupErr := removeVerifiedServiceExportHandoff(path, readFile)
		_ = readFile.Close()
		if err != nil {
			if cleanupErr != nil {
				return nil, fmt.Errorf("verify service export hand-off for scrubbing: %v; remove hand-off: %w", err, cleanupErr)
			}
			return nil, fmt.Errorf("verify service export hand-off for scrubbing: %w", err)
		}
		if cleanupErr != nil {
			return nil, fmt.Errorf("service export hand-off changed while opening for scrubbing; remove hand-off: %w", cleanupErr)
		}
		return nil, errServiceExportIdentityChanged
	}
	_ = readFile.Close()
	return &serviceExportHandoff{path: path, file: writeFile, data: data}, nil
}

// readBoundedServiceExport reads one sentinel byte past the protocol limit so
// callers can distinguish an exactly-full document from an oversized one
// without sizing an allocation from an untrusted file. The returned slice can
// therefore never exceed serviceExportMaxBytes+1 bytes.
func readBoundedServiceExport(reader io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(reader, serviceExportMaxBytes+1))
}

func (m *ServiceManager) readExportHandoffWithGateLocked(rt *serviceRuntime, handoff *serviceExportHandoff, fromLog bool) (ServiceExportFile, error) {
	if len(handoff.data) > serviceExportMaxBytes {
		rt.suppressHookDiagnostics.Store(true)
		candidateSecrets := bestEffortJSONStrings(handoff.data)
		registerServiceExportCandidates(rt, candidateSecrets)
		cause := errors.New("service export exceeds 1 MiB")
		cause = scrubRejectedExport(handoff, serviceExportSchema, cause)
		return ServiceExportFile{}, m.rejectExportLocked(rt, cause, candidateSecrets, fromLog)
	}
	var export ServiceExportFile
	if err := decodeStrictServiceJSON(bytes.NewReader(handoff.data), &export); err != nil {
		rt.suppressHookDiagnostics.Store(true)
		candidateSecrets := bestEffortJSONStrings(handoff.data)
		registerServiceExportCandidates(rt, candidateSecrets)
		cause := scrubRejectedExport(handoff, serviceExportSchema, errors.New("decode export: invalid JSON hand-off"))
		return ServiceExportFile{}, m.rejectExportLocked(rt, cause, candidateSecrets, fromLog)
	}
	candidateSecrets := make([]string, 0, len(export.Secrets)*2)
	for name, value := range export.Secrets {
		candidateSecrets = append(candidateSecrets, name, value)
	}
	registerServiceExportCandidates(rt, candidateSecrets)
	if export.SchemaVersion != serviceExportSchema {
		cause := scrubRejectedExport(handoff, serviceExportSchema, fmt.Errorf("unsupported export schema version %d", export.SchemaVersion))
		return ServiceExportFile{}, m.rejectExportLocked(rt, cause, candidateSecrets, fromLog)
	}
	if export.Variables == nil {
		export.Variables = map[string]string{}
	}
	if rt.secretNames == nil {
		rt.secretNames = map[string]ServiceSecretMetadata{}
	}
	for name, value := range export.Secrets {
		if !validSecretName(name) || strings.ContainsRune(value, '\x00') {
			cause := scrubRejectedExport(handoff, export.SchemaVersion, errors.New("invalid exported secret name or value"))
			return ServiceExportFile{}, m.rejectExportLocked(rt, cause, candidateSecrets, fromLog)
		}
	}
	candidateRedactor := security.NewRedactor(candidateSecrets...)
	for name, value := range export.Variables {
		if !environmentNamePattern.MatchString(name) || strings.ContainsRune(value, '\x00') {
			cause := scrubRejectedExport(handoff, export.SchemaVersion, errors.New("invalid exported variable name or value"))
			return ServiceExportFile{}, m.rejectExportLocked(rt, cause, candidateSecrets, fromLog)
		}
		if candidateRedactor.ContainsSecret([]byte(value)) || rt.redactor != nil && rt.redactor.ContainsSecret([]byte(value)) {
			cause := scrubRejectedExport(handoff, export.SchemaVersion, errors.New("exported variable contains a secret; place it under secrets"))
			return ServiceExportFile{}, m.rejectExportLocked(rt, cause, candidateSecrets, fromLog)
		}
	}
	if rt.exportAccepted && export.Secrets != nil && !equalStringMap(export.Secrets, rt.exportSecrets) {
		sanitized := ServiceExportFile{SchemaVersion: export.SchemaVersion, Variables: cloneStringMap(export.Variables)}
		if err := writeSanitizedExportHandoff(handoff, sanitized); err != nil {
			return ServiceExportFile{}, m.rejectExportLocked(rt, fmt.Errorf("scrub rejected exported secrets: %w", err), candidateSecrets, fromLog)
		}
		return ServiceExportFile{}, m.rejectExportLocked(rt, errors.New("service exported secrets are immutable after the initial hand-off"), candidateSecrets, fromLog)
	}
	if len(export.Secrets) > 0 {
		if !rt.exportAccepted {
			rt.exportSecrets = cloneStringMap(export.Secrets)
			for name := range export.Secrets {
				rt.secretNames[name] = ServiceSecretMetadata{Name: name, Source: "service-export", UpdatedAt: m.now().Format(time.RFC3339Nano)}
			}
			for _, candidate := range candidateSecrets {
				if !containsString(rt.secretValues, candidate) {
					rt.secretValues = append(rt.secretValues, candidate)
				}
			}
		}
		// The export file is an IPC hand-off, not durable secret storage. Keep
		// variables available for later reads but overwrite this accepted inode
		// with a secret-free representation as soon as its secret values have
		// been registered in memory. A failure to scrub is fail-closed.
		sanitized := ServiceExportFile{SchemaVersion: export.SchemaVersion, Variables: cloneStringMap(export.Variables)}
		if err := writeSanitizedExportHandoff(handoff, sanitized); err != nil {
			return ServiceExportFile{}, fmt.Errorf("scrub exported secrets: %w", err)
		}
	} else {
		// A scrubbed hand-off deliberately has no durable secret values. Restore
		// the accepted values from process-local memory so readiness polls and
		// later service bindings cannot erase the only usable copy.
		export.Secrets = cloneStringMap(rt.exportSecrets)
	}
	rt.exportAccepted = true
	return export, nil
}

func registerServiceExportCandidates(rt *serviceRuntime, candidates []string) {
	if rt == nil || rt.redactor == nil {
		return
	}
	for _, candidate := range candidates {
		rt.redactor.Register(candidate)
	}
}

// bestEffortJSONStrings recovers complete JSON string tokens even when the
// surrounding export document is malformed. Rejected hand-offs are otherwise
// reported opaquely, but their candidate names and values may also have been
// printed by the process before rejection and must join the redaction set.
func bestEffortJSONStrings(data []byte) []string {
	values := []string{}
	seen := map[string]struct{}{}
	for start := bytes.IndexByte(data, '"'); start >= 0; {
		end := start + 1
		for end < len(data) {
			switch data[end] {
			case '\\':
				end += 2
				continue
			case '"':
				var value string
				if err := json.Unmarshal(data[start:end+1], &value); err == nil && value != "" {
					if _, found := seen[value]; !found {
						seen[value] = struct{}{}
						values = append(values, value)
					}
				}
				data = data[end+1:]
				start = bytes.IndexByte(data, '"')
				end = -1
			}
			if end < 0 {
				break
			}
			end++
		}
		if end >= len(data) {
			break
		}
	}
	return values
}

func (m *ServiceManager) rejectExportLocked(rt *serviceRuntime, cause error, secretValues []string, fromLog bool) error {
	if fromLog {
		for _, value := range secretValues {
			if !containsString(rt.rejectedExportSecrets, value) {
				rt.rejectedExportSecrets = append(rt.rejectedExportSecrets, value)
			}
		}
	} else {
		for _, value := range secretValues {
			if !containsString(rt.secretValues, value) {
				rt.secretValues = append(rt.secretValues, value)
			}
		}
	}
	if !rt.exportAccepted && !fromLog {
		return cause
	}
	message := cause.Error()
	if rt.exportViolation == "" {
		rt.exportViolation = message
	}
	return exportFailureLocked(rt)
}

func exportFailureLocked(rt *serviceRuntime) error {
	if rt == nil {
		return nil
	}
	var result error
	if rt.exportViolation != "" {
		result = errors.New(rt.exportViolation)
	}
	if rt.exportCleanupFailure != "" {
		result = errors.Join(result, errors.New(rt.exportCleanupFailure))
	}
	return result
}

// scrubActiveServiceExportLocked is called with rt.exportMu held. It destroys
// the exact inode moved out of the active pathname, including the contents of
// any hard links, without following a symlink or touching a newer replacement.
// Cleanup failures have their own latch so an earlier protocol violation can
// never hide the durable-secret cleanup failure.
func (m *ServiceManager) scrubActiveServiceExportLocked(rt *serviceRuntime, cfg ServiceConfig, context string) error {
	path := filepath.Join(serviceRuntimePath(m.root, cfg.ID), "export.json")
	if !pathWithinResolved(filepath.Join(m.root, ".pua"), path) {
		return latchExportCleanupFailureLocked(rt, errors.New("service export cleanup path escapes the workspace control directory"))
	}
	scrubPath := m.exportScrubPath
	if scrubPath == nil {
		scrubPath = scrubAndRemoveServiceExportPath
	}
	if err := scrubPath(path); err != nil {
		return latchExportCleanupFailureLocked(rt, fmt.Errorf("%s: %w", context, err))
	}
	return nil
}

func (m *ServiceManager) scrubActiveServiceExport(rt *serviceRuntime, cfg ServiceConfig, context string) error {
	rt.exportMu.Lock()
	defer rt.exportMu.Unlock()
	return m.scrubActiveServiceExportLocked(rt, cfg, context)
}

func exportCleanupError(rt *serviceRuntime) error {
	if rt == nil {
		return nil
	}
	rt.exportMu.Lock()
	defer rt.exportMu.Unlock()
	if rt.exportCleanupFailure == "" {
		return nil
	}
	return errors.New(rt.exportCleanupFailure)
}

func exportProtocolViolationError(rt *serviceRuntime) error {
	if rt == nil {
		return nil
	}
	rt.exportMu.Lock()
	defer rt.exportMu.Unlock()
	if rt.exportViolation == "" {
		return nil
	}
	return errors.New(rt.exportViolation)
}

func latchExportCleanupFailureLocked(rt *serviceRuntime, cause error) error {
	if rt == nil || cause == nil {
		return cause
	}
	message := cause.Error()
	if rt.exportCleanupFailure == "" {
		rt.exportCleanupFailure = message
		return cause
	} else if !strings.Contains(rt.exportCleanupFailure, message) {
		previous := rt.exportCleanupFailure
		rt.exportCleanupFailure += "; " + message
		return errors.Join(errors.New(previous), cause)
	}
	return cause
}

func (m *ServiceManager) promoteRejectedExportSecretsLocked(rt *serviceRuntime) {
	for _, value := range rt.rejectedExportSecrets {
		if !containsString(rt.secretValues, value) {
			rt.secretValues = append(rt.secretValues, value)
		}
	}
	rt.rejectedExportSecrets = nil
}

func (m *ServiceManager) exportProtocolErrorLocked(rt *serviceRuntime) error {
	rt.exportMu.Lock()
	defer rt.exportMu.Unlock()
	m.promoteRejectedExportSecretsLocked(rt)
	return exportFailureLocked(rt)
}

func scrubRejectedExport(handoff *serviceExportHandoff, schemaVersion int, cause error) error {
	if err := writeSanitizedExportHandoff(handoff, ServiceExportFile{SchemaVersion: schemaVersion, Variables: map[string]string{}}); err != nil {
		return fmt.Errorf("%v; scrub rejected export: %w", cause, err)
	}
	return cause
}

func writeSanitizedExportHandoff(handoff *serviceExportHandoff, export ServiceExportFile) error {
	if err := writeSanitizedExport(handoff.file, export); err != nil {
		if cleanupErr := removeVerifiedServiceExportHandoff(handoff.path, handoff.file); cleanupErr != nil {
			return fmt.Errorf("%v; remove unsanitized export hand-off: %w", err, cleanupErr)
		}
		return err
	}
	return nil
}

type serviceExportHandoffOperations struct {
	rename func(string, string) error
	link   func(string, string) error
	remove func(string) error
}

func defaultServiceExportHandoffOperations() serviceExportHandoffOperations {
	return serviceExportHandoffOperations{
		rename: os.Rename,
		link:   os.Link,
		remove: os.Remove,
	}
}

// scrubAndRemoveServiceExportPath atomically moves the current pathname out of
// the service-visible location before destroying its contents. The quarantine
// descriptor checks in scrubAndRemoveQuarantinedServiceExport ensure that a
// concurrently replaced pathname is never followed or overwritten.
func scrubAndRemoveServiceExportPath(path string) error {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	operations := defaultServiceExportHandoffOperations()
	temp, err := os.CreateTemp(filepath.Dir(path), ".export-handoff-*")
	if err != nil {
		return err
	}
	quarantinePath := temp.Name()
	if err := temp.Close(); err != nil {
		_ = operations.remove(quarantinePath)
		return err
	}
	if err := operations.rename(path, quarantinePath); err != nil {
		_ = operations.remove(quarantinePath)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	quarantineInfo, err := os.Lstat(quarantinePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return scrubAndRemoveQuarantinedServiceExport(quarantinePath, quarantineInfo, operations)
}

// removeVerifiedServiceExportHandoff removes only the inode already held by
// file. Renaming it to a private name before unlinking closes the lstat/remove
// race; if the pathname changed, the replacement is left untouched.
func removeVerifiedServiceExportHandoff(path string, file *os.File) error {
	return removeVerifiedServiceExportHandoffWithOperations(path, file, defaultServiceExportHandoffOperations())
}

func removeVerifiedServiceExportHandoffWithOperations(path string, file *os.File, operations serviceExportHandoffOperations) error {
	openedInfo, err := file.Stat()
	if err != nil {
		return err
	}
	pathInfo, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !pathInfo.Mode().IsRegular() || !os.SameFile(openedInfo, pathInfo) {
		return nil
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".export-handoff-*")
	if err != nil {
		return err
	}
	quarantinePath := temp.Name()
	if closeErr := temp.Close(); closeErr != nil {
		_ = operations.remove(quarantinePath)
		return closeErr
	}
	if err := operations.rename(path, quarantinePath); err != nil {
		_ = operations.remove(quarantinePath)
		return err
	}
	quarantineInfo, err := os.Lstat(quarantinePath)
	if err != nil {
		return err
	}
	if os.SameFile(openedInfo, quarantineInfo) {
		return scrubAndRemoveQuarantinedServiceExport(quarantinePath, openedInfo, operations)
	}
	// The path changed between verification and rename. Restore the moved
	// replacement without overwriting a still newer hand-off.
	if err := operations.link(quarantinePath, path); err != nil {
		restoreErr := fmt.Errorf("restore concurrently replaced export hand-off: %w", err)
		cleanupErr := scrubAndRemoveQuarantinedServiceExport(quarantinePath, quarantineInfo, operations)
		if cleanupErr != nil {
			return errors.Join(restoreErr, fmt.Errorf("discard unrestored export hand-off: %w", cleanupErr))
		}
		return restoreErr
	}
	return operations.remove(quarantinePath)
}

// scrubAndRemoveQuarantinedServiceExport destroys the contents of the exact
// inode moved into quarantine before unlinking its private pathname. Truncating
// through a verified descriptor also scrubs any hard links to that inode. A
// symlink is unlinked without following it, and a changed quarantine pathname
// is left untouched.
func scrubAndRemoveQuarantinedServiceExport(path string, expected os.FileInfo, operations serviceExportHandoffOperations) error {
	if !expected.Mode().IsRegular() {
		current, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !os.SameFile(expected, current) {
			return errors.New("quarantined export hand-off changed before unlink")
		}
		return operations.remove(path)
	}

	file, err := openQuarantinedServiceExportForScrubbing(path, expected)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("scrub quarantined export hand-off: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync scrubbed export hand-off: %w", err)
	}
	current, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(opened, current) {
		return errors.New("quarantined export hand-off changed before unlink")
	}
	return operations.remove(path)
}

func openQuarantinedServiceExportForScrubbing(path string, expected os.FileInfo) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err == nil {
		if verifyErr := verifyQuarantinedServiceExport(file, expected); verifyErr != nil {
			_ = file.Close()
			return nil, verifyErr
		}
		return file, nil
	}

	// A concurrent replacement may be read-only. Grant write access through a
	// verified descriptor, then reopen and verify the same inode before use.
	readFile, readErr := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if readErr != nil {
		return nil, errors.Join(err, readErr)
	}
	defer readFile.Close()
	if verifyErr := verifyQuarantinedServiceExport(readFile, expected); verifyErr != nil {
		return nil, verifyErr
	}
	if chmodErr := readFile.Chmod(0o600); chmodErr != nil {
		return nil, errors.Join(err, fmt.Errorf("prepare quarantined export hand-off for scrubbing: %w", chmodErr))
	}
	file, err = os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if verifyErr := verifyQuarantinedServiceExport(file, expected); verifyErr != nil {
		_ = file.Close()
		return nil, verifyErr
	}
	return file, nil
}

func verifyQuarantinedServiceExport(file *os.File, expected os.FileInfo) error {
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(opened, expected) {
		return errors.New("quarantined export hand-off changed before scrubbing")
	}
	return nil
}

func writeSanitizedExport(file *os.File, export ServiceExportFile) error {
	data, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return discardRejectedExport(file, err)
	}
	data = append(data, '\n')
	if err := file.Chmod(0o600); err != nil {
		return discardRejectedExport(file, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return discardRejectedExport(file, err)
	}
	if _, err := file.Write(data); err != nil {
		return discardRejectedExport(file, err)
	}
	if err := file.Truncate(int64(len(data))); err != nil {
		return discardRejectedExport(file, err)
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return nil
}

func discardRejectedExport(file *os.File, cause error) error {
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("write sanitized export: %v; discard rejected export: %w", cause, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("write sanitized export: %v; sync discarded export: %w", cause, err)
	}
	return cause
}

func equalStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if other, ok := right[key]; !ok || other != value {
			return false
		}
	}
	return true
}

func requiresInitialExport(cfg ServiceConfig) bool {
	// Export gating is an explicit protocol declaration. Readiness alone never
	// implies that the process will write an export hand-off.
	return cfg.Exports
}

func (m *ServiceManager) runReadinessLocked(ctx context.Context, rt *serviceRuntime) error {
	if ctx == nil {
		ctx = context.Background()
	}
	command := rt.config.Readiness.Command
	if len(command) == 0 {
		return nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, rt.config.Readiness.Timeout)
	defer cancel()
	dir := serviceCWD(m.root, rt.config.CWD)
	if !pathWithinResolved(m.root, dir) {
		return errors.New("readiness cwd escapes the workspace")
	}
	var output serviceCommandOutput
	err := runServiceGroupCommand(checkCtx, command, dir, rt.environment, &output)
	if err != nil {
		text := serviceHookDiagnostic(rt, &output)
		if text != "" {
			return fmt.Errorf("readiness failed: %w: %s", err, text)
		}
		return fmt.Errorf("readiness failed: %w", err)
	}
	return nil
}

func (m *ServiceManager) readinessFailedLocked(ctx context.Context, rt *serviceRuntime, cause error) error {
	// Read and scrub the exact hand-off left by the failed readiness command
	// before its output can enter any durable diagnostic. A rejected or unsafe
	// hand-off makes the unverified command output unusable; retain only the
	// opaque protocol failure in that case.
	if _, exportErr := m.readExportsLocked(rt); exportErr != nil {
		cause = fmt.Errorf("readiness failed after rejected export hand-off: %w", exportErr)
	}
	message := security.NewRedactor(rt.secretValues...).RedactString(cause.Error())
	rt.status.Readiness.Ready = false
	rt.status.Readiness.LastCheck = m.now().Format(time.RFC3339Nano)
	rt.status.Readiness.LastError = message
	rt.status.LastError = message
	_ = m.appendEventLocked(rt, map[string]any{"type": "readiness_failed", "error": message, "time": rt.status.Readiness.LastCheck})
	cleanupErr := m.stopProcessLocked(ctx, rt, false)
	if cleanupErr != nil {
		cleanupMessage := security.NewRedactor(rt.secretValues...).RedactString(cleanupErr.Error())
		message += "; " + cleanupMessage
	}
	return m.transitionServiceFailureLocked(rt, serviceFailureTransition{
		at:               m.now(),
		lastError:        message,
		requireAttention: cleanupErr != nil,
	})
}

func (m *ServiceManager) handleProcessExitLocked(ctx context.Context, rt *serviceRuntime, exit serviceProcessExit) error {
	if rt.process == nil {
		return nil
	}
	var exportErr error
	if rt.processConfig != nil && requiresInitialExport(*rt.processConfig) {
		_, exportErr = m.readExportsLocked(rt)
	}
	for _, writer := range rt.logWriters {
		_ = writer.Close()
	}
	if protocolErr := m.exportProtocolErrorLocked(rt); protocolErr != nil {
		exportErr = protocolErr
	}
	if exit.err == nil && exportErr != nil {
		exit.err = exportErr
	}
	groupErr := m.terminateRuntimeProcessGroupLocked(ctx, rt, m.processTerminationGrace)
	rt.process = nil
	rt.exit = nil
	rt.logWriters = nil
	rt.status.ExitedAt = m.now().Format(time.RFC3339Nano)
	rt.status.ExitCode = exit.code
	if exit.err != nil {
		rt.status.ExitError = security.NewRedactor(rt.secretValues...).RedactString(exit.err.Error())
	}
	if groupErr != nil {
		_ = m.appendEventLocked(rt, map[string]any{"type": "exited", "code": exit.code, "error": rt.status.ExitError, "time": rt.status.ExitedAt})
		return m.failOrphanRecoveryLocked(rt, groupErr)
	}
	rt.status.PID, rt.status.ProcessGroup = 0, 0
	exportConfig := rt.config
	if rt.processConfig != nil {
		exportConfig = *rt.processConfig
	}
	exportCleanupErr := m.scrubActiveServiceExport(rt, exportConfig, "scrub service export after process exit")
	if exportCleanupErr != nil {
		exportCleanupErr = errors.Join(exportProtocolViolationError(rt), exportCleanupErr)
	} else {
		exportCleanupErr = exportCleanupError(rt)
	}
	if rt.status.ManualStop || m.stopping || !rt.config.Enabled {
		if exportCleanupErr != nil {
			rt.status.LastError = security.NewRedactor(rt.secretValues...).RedactString(exportCleanupErr.Error())
			rt.status.AttentionRequired = true
			rt.status.State = ServiceStateAttentionRequired
			return errors.Join(exportCleanupErr, m.persistStatusLocked(rt))
		}
		rt.status.State = ServiceStateStopped
		return m.persistStatusLocked(rt)
	}
	transitionAt := m.now()
	_ = m.appendEventLocked(rt, map[string]any{"type": "exited", "code": exit.code, "error": rt.status.ExitError, "time": rt.status.ExitedAt})
	cleanupErr := errors.Join(exportCleanupErr, m.runCleanupLocked(ctx, rt))
	rt.processConfig = nil
	rt.processCommandDigest = ""
	lastError := rt.status.ExitError
	if cleanupErr != nil {
		lastError = security.NewRedactor(rt.secretValues...).RedactString(cleanupErr.Error())
	}
	return m.transitionServiceFailureLocked(rt, serviceFailureTransition{
		at:                  transitionAt,
		lastError:           lastError,
		resetAfterStableRun: true,
		requireAttention:    cleanupErr != nil,
	})
}

func (m *ServiceManager) transitionServiceFailureLocked(rt *serviceRuntime, failure serviceFailureTransition) error {
	if failure.resetAfterStableRun && rt.config.Restart.ResetAfter > 0 && !rt.stableSince.IsZero() && failure.at.Sub(rt.stableSince) >= rt.config.Restart.ResetAfter {
		rt.status.FailureCount = 0
		rt.status.AttentionRequired = false
	}
	rt.status.FailureCount++
	rt.status.AttentionRequired = rt.status.FailureCount >= 5 || failure.requireAttention
	rt.status.State = ServiceStateBackoff
	if rt.status.AttentionRequired {
		rt.status.State = ServiceStateAttentionRequired
	}
	rt.status.LastError = failure.lastError
	rt.status.NextRetryAt = failure.at.Add(restartDelay(rt.config.Restart, rt.status.FailureCount)).Format(time.RFC3339Nano)
	return m.persistStatusLocked(rt)
}

func (m *ServiceManager) stopProcessLocked(ctx context.Context, rt *serviceRuntime, manual bool) error {
	if rt.process == nil && rt.status.ProcessGroup <= 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if manual {
		rt.status.ManualStop = true
	}
	reconstructedProcess := rt.process == nil
	grace := m.processTerminationGrace
	if grace <= 0 {
		grace = defaultServiceProcessTerminationGrace
	}
	if err := m.terminateRuntimeProcessGroupLocked(ctx, rt, grace); err != nil {
		return m.failProcessTerminationLocked(rt, err)
	}
	if rt.process != nil {
		if err := waitForServiceProcessLeader(rt.exit); err != nil {
			return m.failProcessTerminationLocked(rt, err)
		}
	}
	recoveredTermination := rt.terminationPending || rt.process == nil
	exportConfig := rt.config
	if rt.processConfig != nil {
		exportConfig = *rt.processConfig
	}
	if requiresInitialExport(exportConfig) {
		_, _ = m.readExportsLocked(rt)
	}
	for _, writer := range rt.logWriters {
		_ = writer.Close()
	}
	_ = m.exportProtocolErrorLocked(rt)
	exportCleanupErr := m.scrubActiveServiceExport(rt, exportConfig, "scrub service export after process termination")
	if exportCleanupErr != nil {
		exportCleanupErr = errors.Join(exportProtocolViolationError(rt), exportCleanupErr)
	} else {
		exportCleanupErr = exportCleanupError(rt)
	}
	rt.process = nil
	rt.exit = nil
	rt.logWriters = nil
	rt.terminationPending = false
	rt.processOwnership = serviceProcessOwnershipReconstructed
	rt.status.PID, rt.status.ProcessGroup = 0, 0
	if recoveredTermination && (manual || m.stopping) {
		rt.status.AttentionRequired = false
		rt.status.LastError = ""
		rt.status.State = ServiceStateStopped
	}
	var cleanupErr error
	if reconstructedProcess {
		cleanupErr = m.restoreProcessCleanupEnvironmentLocked(rt)
	}
	if cleanupErr == nil {
		cleanupErr = m.runCleanupLocked(ctx, rt)
	}
	rt.processConfig = nil
	rt.processCommandDigest = ""
	if cleanupErr != nil || exportCleanupErr != nil {
		cleanupErr = errors.Join(exportCleanupErr, cleanupErr)
		rt.status.LastError = security.NewRedactor(rt.secretValues...).RedactString(cleanupErr.Error())
		rt.status.Cleanup.LastError = rt.status.LastError
		rt.status.State = ServiceStateAttentionRequired
		rt.status.AttentionRequired = true
		return errors.Join(cleanupErr, m.persistStatusLocked(rt))
	}
	return nil
}

func (m *ServiceManager) terminateRuntimeProcessGroupLocked(ctx context.Context, rt *serviceRuntime, grace time.Duration) error {
	if rt.status.ProcessGroup <= 0 {
		return nil
	}
	leaderPID := rt.status.PID
	if rt.process != nil && rt.process.Process != nil {
		leaderPID = rt.process.Process.Pid
	}
	processPlatform := m.serviceProcessPlatform()
	markerPath := ""
	if processPlatform.identityMarkerRequired {
		markerPath = serviceProcessIdentityMarkerPath(m.root, rt.status.ID, rt.status.InstanceToken)
	}
	err := terminateOwnedServiceProcessGroup(ctx, serviceProcessGroupIdentity{
		leaderPID:       leaderPID,
		processGroup:    rt.status.ProcessGroup,
		startID:         rt.status.ProcessStartID,
		instanceToken:   rt.status.InstanceToken,
		commandDigest:   rt.status.CommandDigest,
		markerPath:      markerPath,
		ownership:       rt.processOwnership,
		processPlatform: processPlatform,
	}, grace)
	if err == nil && markerPath != "" {
		_ = os.Remove(markerPath)
	}
	return err
}

func waitForServiceProcessLeader(wait <-chan serviceProcessExit) error {
	if wait == nil {
		return errors.New("service process leader wait is unavailable")
	}
	timer := time.NewTimer(serviceCommandWaitDelay)
	defer timer.Stop()
	select {
	case _, ok := <-wait:
		if !ok {
			return errors.New("service process leader wait closed without a result")
		}
		return nil
	case <-timer.C:
		return errors.New("service process leader was not reaped after its process group exited")
	}
}

func (m *ServiceManager) failProcessTerminationLocked(rt *serviceRuntime, cause error) error {
	message := security.NewRedactor(rt.secretValues...).RedactString(cause.Error())
	appendFailure := !rt.terminationPending || rt.status.LastError != message
	rt.terminationPending = rt.process != nil
	rt.status.LastError = message
	rt.status.State = ServiceStateAttentionRequired
	rt.status.AttentionRequired = true
	rt.status.Readiness.Ready = false
	if appendFailure {
		_ = m.appendEventLocked(rt, map[string]any{"type": "stop_failed", "error": message, "time": m.now().Format(time.RFC3339Nano)})
	}
	return errors.Join(errors.New(message), m.persistStatusLocked(rt))
}

func (m *ServiceManager) runCleanupLocked(ctx context.Context, rt *serviceRuntime) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if rt.processConfig == nil || rt.processConfig.Cleanup == nil || len(rt.processConfig.Cleanup.Command) == 0 {
		return nil
	}
	processConfig := rt.processConfig
	cleanup := processConfig.Cleanup
	attempts := cleanup.Retries + 1
	if attempts < 1 {
		attempts = 1
	}
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		checkCtx, cancel := context.WithTimeout(ctx, cleanup.Timeout)
		dir := serviceCWD(m.root, processConfig.CWD)
		if !pathWithinResolved(m.root, dir) {
			cancel()
			last = errors.New("cleanup cwd escapes the workspace")
			rt.status.Cleanup.Attempts++
			rt.status.Cleanup.LastRun = m.now().Format(time.RFC3339Nano)
			rt.status.Cleanup.LastError = last.Error()
			continue
		}
		var output serviceCommandOutput
		err := runServiceGroupCommand(checkCtx, cleanup.Command, dir, rt.environment, &output)
		cancel()
		rt.status.Cleanup.Attempts++
		rt.status.Cleanup.LastRun = m.now().Format(time.RFC3339Nano)
		if err == nil {
			rt.status.Cleanup.Succeeded = true
			rt.status.Cleanup.LastError = ""
			return nil
		}
		message := serviceHookDiagnostic(rt, &output)
		if message != "" {
			last = fmt.Errorf("cleanup failed: %w: %s", err, message)
		} else {
			last = fmt.Errorf("cleanup failed: %w", err)
		}
		rt.status.Cleanup.LastError = last.Error()
	}
	rt.status.Cleanup.Succeeded = false
	return last
}

func serviceHookDiagnostic(rt *serviceRuntime, output *serviceCommandOutput) string {
	if rt != nil && rt.suppressHookDiagnostics.Load() {
		return ""
	}
	return output.diagnostic(security.NewRedactor(rt.secretValues...))
}

func (m *ServiceManager) resolveEnvironmentLocked(cfg ServiceConfig) ([]string, []string, map[string]ServiceSecretMetadata, ServiceExportFile, error) {
	byName := inheritedServiceEnvironment()
	secrets := []string{}
	names := map[string]ServiceSecretMetadata{}
	exports := ServiceExportFile{SchemaVersion: serviceExportSchema, Variables: map[string]string{}}
	environmentNames := make([]string, 0, len(cfg.Environment))
	for name := range cfg.Environment {
		environmentNames = append(environmentNames, name)
	}
	sort.Strings(environmentNames)
	for _, name := range environmentNames {
		entry := cfg.Environment[name]
		value, source, err := m.resolveEnvironmentValueLocked(entry)
		if err != nil {
			return nil, nil, nil, exports, fmt.Errorf("environment %s: %w", name, err)
		}
		if strings.ContainsRune(value, '\x00') {
			return nil, nil, nil, exports, fmt.Errorf("environment %s contains NUL", name)
		}
		byName[name] = value
		secretName := entry.SecretName
		if matches := secretTemplatePattern.FindStringSubmatch(entry.Template); secretName == "" && len(matches) == 2 && matches[0] == entry.Template {
			secretName = matches[1]
		}
		if secretName != "" {
			secrets = append(secrets, value)
			names[secretName] = ServiceSecretMetadata{Name: secretName, Source: source, UpdatedAt: m.now().Format(time.RFC3339Nano)}
		} else if source == "service-secret" {
			secrets = append(secrets, value)
			names[name] = ServiceSecretMetadata{Name: name, Source: source, UpdatedAt: m.now().Format(time.RFC3339Nano)}
		}
	}
	// Supervisor control values are authoritative even when a service
	// definition happens to contain the same name.
	byName["PUA_SERVICE_EXPORT_PATH"] = filepath.Join(serviceRuntimePath(m.root, cfg.ID), "export.json")
	byName["PUA_SERVICE_INSTANCE_TOKEN"] = newServiceInstanceToken()
	byName["PUA_SERVICE_COMMAND_DIGEST"] = serviceCommandDigest(cfg)
	keys := make([]string, 0, len(byName))
	for key := range byName {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// Rebuild from the map so overridden daemon values do not leave duplicate
	// entries in the child environment.
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, key+"="+byName[key])
	}
	return values, secrets, names, exports, nil
}

// inheritedServiceEnvironment deliberately carries only process-execution,
// locale, temporary-directory, and user-location basics into a service. In
// particular, daemon credentials and all PUA_* values stay supervisor-private
// unless the service explicitly declares a resolved environment entry.
func inheritedServiceEnvironment() map[string]string {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !inheritedServiceEnvironmentName(name) {
			continue
		}
		values[name] = value
	}
	return values
}

func inheritedServiceEnvironmentName(name string) bool {
	switch name {
	case "PATH",
		"HOME", "USER", "LOGNAME", "SHELL", "XDG_RUNTIME_DIR",
		"TMPDIR", "TMP", "TEMP",
		"LANG", "LANGUAGE", "TZ", "__CF_USER_TEXT_ENCODING",
		"LC_ALL", "LC_ADDRESS", "LC_COLLATE", "LC_CTYPE",
		"LC_IDENTIFICATION", "LC_MEASUREMENT", "LC_MESSAGES",
		"LC_MONETARY", "LC_NAME", "LC_NUMERIC", "LC_PAPER",
		"LC_TELEPHONE", "LC_TIME":
		return true
	default:
		return false
	}
}

func (m *ServiceManager) resolveEnvironmentValueLocked(entry ServiceEnvironment) (string, string, error) {
	if entry.SecretName != "" {
		return m.resolveSecretLocked(entry.SecretName)
	}
	if entry.Template == "" {
		return entry.Literal, "literal", nil
	}
	if matches := secretTemplatePattern.FindStringSubmatch(entry.Template); len(matches) == 2 && matches[0] == entry.Template {
		return m.resolveSecretLocked(matches[1])
	}
	return m.resolveTemplateLocked(entry.Template)
}

func (m *ServiceManager) resolveSecretLocked(name string) (string, string, error) {
	if m.resolver == nil {
		m.resolver = EnvironmentSecretResolver{}
	}
	value, source, err := m.resolver.ResolveSecret(name)
	if err != nil {
		return "", "", fmt.Errorf("secret %q is unavailable", name)
	}
	if strings.ContainsRune(value, '\x00') {
		return "", "", fmt.Errorf("secret %q contains NUL", name)
	}
	return value, source, nil
}

func (m *ServiceManager) resolveTemplateLocked(template string) (string, string, error) {
	if match := serviceTemplatePattern.FindStringSubmatch(template); len(match) == 3 && match[0] == template {
		rt := m.runtimes[match[1]]
		if rt == nil || rt.status.State != ServiceStateReady {
			return "", "", fmt.Errorf("service %q is not ready", match[1])
		}
		if value, ok := rt.exports.Variables[match[2]]; ok {
			return value, "service-template", nil
		}
		if value, ok := rt.exports.Secrets[match[2]]; ok {
			return value, "service-secret", nil
		}
		return "", "", fmt.Errorf("service %q export %q is unavailable", match[1], match[2])
	}
	var missing error
	result := serviceTemplatePattern.ReplaceAllStringFunc(template, func(raw string) string {
		match := serviceTemplatePattern.FindStringSubmatch(raw)
		if len(match) != 3 {
			missing = fmt.Errorf("invalid service template %q", raw)
			return ""
		}
		rt := m.runtimes[match[1]]
		if rt == nil || rt.status.State != ServiceStateReady {
			missing = fmt.Errorf("service %q is not ready", match[1])
			return ""
		}
		value, ok := rt.exports.Variables[match[2]]
		if !ok {
			missing = fmt.Errorf("service %q export %q is unavailable", match[1], match[2])
			return ""
		}
		return value
	})
	if missing != nil {
		return "", "", missing
	}
	if strings.Contains(result, "${service.") {
		return "", "", fmt.Errorf("invalid service template %q", template)
	}
	return result, "service-template", nil
}

// recoverOrphansLocked reclaims persisted process ownership in the same
// dependent-first order used for ordinary service shutdown. Every candidate is
// attempted even when an earlier recovery fails, so one broken cleanup cannot
// strand an unrelated process group. A pending runtime is excluded from normal
// reconciliation until a later pass completes its recovery.
func (m *ServiceManager) recoverOrphansLocked() error {
	pending := make(map[string]struct{})
	for id, rt := range m.runtimes {
		if serviceRuntimeNeedsOrphanRecovery(rt) {
			pending[id] = struct{}{}
		}
	}
	if len(pending) == 0 {
		return nil
	}
	order, err := serviceGraphSubsetStopOrder(m.graph, pending)
	if err != nil {
		return err
	}
	var result error
	for _, id := range order {
		if err := m.recoverOrphanLocked(m.runtimes[id]); err != nil {
			result = errors.Join(result, fmt.Errorf("recover service %s orphan: %w", id, err))
		}
	}
	return result
}

func (m *ServiceManager) recoverOrphanLocked(rt *serviceRuntime) error {
	if !serviceRuntimeNeedsOrphanRecovery(rt) {
		return nil
	}
	rt.orphanRecoveryPending = true
	if rt.status.PID > 0 || rt.status.ProcessGroup > 0 {
		if err := m.terminateRuntimeProcessGroupLocked(context.Background(), rt, 500*time.Millisecond); err != nil {
			return m.failOrphanRecoveryLocked(rt, err)
		}
		rt.processOwnership = serviceProcessOwnershipReconstructed
		rt.status.PID, rt.status.ProcessGroup = 0, 0
	}
	if !rt.orphanCleanupComplete {
		cleanupErr := m.restoreProcessCleanupEnvironmentLocked(rt)
		if cleanupErr == nil {
			cleanupErr = m.runCleanupLocked(context.Background(), rt)
		}
		if cleanupErr != nil {
			message := security.NewRedactor(rt.secretValues...).RedactString(cleanupErr.Error())
			rt.status.State = ServiceStateAttentionRequired
			rt.status.AttentionRequired = true
			rt.status.Readiness.Ready = false
			rt.status.LastError = message
			rt.status.Cleanup.LastError = message
			return errors.Join(errors.New(message), m.persistStatusLocked(rt))
		}
		rt.orphanCleanupComplete = true
	}
	processConfig := rt.processConfig
	processCommandDigest := rt.processCommandDigest
	rt.orphanRecoveryPending = false
	rt.orphanCleanupComplete = false
	rt.processConfig = nil
	rt.processCommandDigest = ""
	rt.status.State = ServiceStateStopped
	// Successful orphan reaping resolves process ownership, not operator
	// intent. A failed manual stop persists this bit so reconciliation remains
	// suppressed until an explicit Start, Restart, or Enable clears it.
	rt.status.AttentionRequired = false
	rt.status.LastError = ""
	if err := m.persistStatusLocked(rt); err != nil {
		// Keep recovery explicit until its final durable state is recorded. The
		// cleanup already completed in this manager, so a retry only rewrites the
		// status and cannot duplicate the cleanup side effect or launch a process.
		rt.orphanRecoveryPending = true
		rt.orphanCleanupComplete = true
		rt.processConfig = processConfig
		rt.processCommandDigest = processCommandDigest
		message := security.NewRedactor(rt.secretValues...).RedactString(err.Error())
		rt.status.State = ServiceStateAttentionRequired
		rt.status.AttentionRequired = true
		rt.status.Readiness.Ready = false
		rt.status.LastError = message
		return err
	}
	return nil
}

func serviceRuntimeNeedsOrphanRecovery(rt *serviceRuntime) bool {
	return rt != nil && rt.process == nil && (rt.orphanRecoveryPending || rt.status.PID > 0 || rt.status.ProcessGroup > 0)
}

func (m *ServiceManager) restoreProcessCleanupEnvironmentLocked(rt *serviceRuntime) error {
	if rt.processConfig == nil || rt.processConfig.Cleanup == nil || len(rt.processConfig.Cleanup.Command) == 0 {
		return nil
	}
	environment, secrets, names, _, err := m.resolveEnvironmentLocked(*rt.processConfig)
	if err != nil {
		return fmt.Errorf("restore process cleanup environment: %w", err)
	}
	environment = replaceEnvironmentValue(environment, serviceInstanceTokenEnvironment, rt.status.InstanceToken)
	environment = replaceEnvironmentValue(environment, serviceCommandDigestEnvironment, rt.processCommandDigest)
	rt.environment = environment
	rt.secretValues = secrets
	rt.secretNames = names
	rt.redactor = security.NewRedactor(secrets...)
	return nil
}

func (m *ServiceManager) failOrphanRecoveryLocked(rt *serviceRuntime, cause error) error {
	rt.orphanRecoveryPending = true
	rt.orphanCleanupComplete = false
	message := security.NewRedactor(rt.secretValues...).RedactString(cause.Error())
	rt.status.State = ServiceStateAttentionRequired
	rt.status.AttentionRequired = true
	rt.status.Readiness.Ready = false
	rt.status.LastError = message
	_ = m.appendEventLocked(rt, map[string]any{"type": "orphan_reap_failed", "error": message, "time": m.now().Format(time.RFC3339Nano)})
	return m.persistStatusLocked(rt)
}

func (m *ServiceManager) sortedIDsLocked() []string {
	if len(m.graph) != len(m.configs) {
		graph, err := buildServiceDependencyGraph(m.configs)
		if err == nil {
			m.graph = graph
		}
	}
	ids, err := m.graph.topologicalOrder()
	if err == nil {
		return ids
	}
	return sortedServiceConfigIDs(m.configs)
}

func (m *ServiceManager) persistStatusLocked(rt *serviceRuntime) error {
	rt.status.SchemaVersion = serviceSchemaVersion
	rt.status.UpdatedAt = m.now().Format(time.RFC3339Nano)
	path := filepath.Join(serviceRuntimePath(m.root, rt.status.ID), "state.json")
	if !pathWithinResolved(filepath.Join(m.root, ".pua"), path) {
		return errors.New("service runtime state path escapes the workspace control directory")
	}
	writeJSON := m.runtimeStateStore.writeJSON
	if writeJSON == nil {
		writeJSON = writeServiceJSON
	}
	persisted := persistedServiceRuntimeState{
		ServiceStatus:           cloneServiceStatus(rt.status),
		OrphanRecoveryPending:   rt.orphanRecoveryPending,
		PendingExportChanges:    sortedStringSet(rt.pendingExportKeys),
		SuppressHookDiagnostics: rt.suppressHookDiagnostics.Load(),
	}
	if (rt.status.PID > 0 || rt.status.ProcessGroup > 0 || rt.orphanRecoveryPending) && rt.processConfig != nil {
		persisted.ProcessConfig = newPersistedServiceProcessConfig(rt)
	}
	if err := writeJSON(path, persisted, 0o600); err != nil {
		return fmt.Errorf("persist service %s runtime state: %w", rt.status.ID, err)
	}
	return nil
}

func (m *ServiceManager) releaseStartupLogsLocked(rt *serviceRuntime) error {
	var first error
	for _, writer := range rt.logWriters {
		if err := writer.Release(); err != nil && first == nil {
			first = err
			message := security.NewRedactor(rt.secretValues...).RedactString(err.Error())
			rt.status.LastError = message
			rt.status.AttentionRequired = true
			_ = m.appendEventLocked(rt, map[string]any{"type": "log_flush_failed", "error": message, "time": m.now().Format(time.RFC3339Nano)})
		}
	}
	if protocolErr := m.exportProtocolErrorLocked(rt); protocolErr != nil {
		return protocolErr
	}
	return first
}

func (m *ServiceManager) appendEventLocked(rt *serviceRuntime, event map[string]any) error {
	path := filepath.Join(serviceRuntimePath(m.root, rt.config.ID), "events.jsonl")
	if !pathWithinResolved(filepath.Join(m.root, ".pua"), path) {
		return errors.New("service event path escapes the workspace control directory")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	data = security.NewRedactor(rt.secretValues...).Redact(data)
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err = file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

func (m *ServiceManager) sortedStatusLocked() []ServiceStatus {
	ids := m.sortedIDsLocked()
	result := make([]ServiceStatus, 0, len(ids))
	for _, id := range ids {
		if rt := m.runtimes[id]; rt != nil {
			status := rt.status
			status.Exports = publicExports(rt.exports, rt.secretNames)
			result = append(result, status)
		}
	}
	return result
}

func publicExports(export ServiceExportFile, names map[string]ServiceSecretMetadata) ServiceExports {
	variables := map[string]string{}
	for key, value := range export.Variables {
		variables[key] = value
	}
	metadata := make([]ServiceSecretMetadata, 0, len(names))
	for _, value := range names {
		metadata = append(metadata, value)
	}
	sort.Slice(metadata, func(i, j int) bool { return metadata[i].Name < metadata[j].Name })
	return ServiceExports{Variables: variables, Secrets: metadata, UpdatedAt: time.Now().Format(time.RFC3339Nano)}
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return map[string]string{}
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneStringSet(values map[string]struct{}) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]struct{}, len(values))
	for value := range values {
		result[value] = struct{}{}
	}
	return result
}

func sortedStringSet(values map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func restartDelay(cfg ServiceRestartConfig, failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	delay := cfg.InitialDelay
	if delay <= 0 {
		delay = defaultRestartDelay
	}
	multiplier := cfg.Multiplier
	if multiplier <= 0 {
		multiplier = 2
	}
	maxDelay := cfg.MaxDelay
	if maxDelay <= 0 {
		maxDelay = defaultRestartMax
	}
	for i := 1; i < failures; i++ {
		next := time.Duration(float64(delay) * multiplier)
		if next <= delay || next > maxDelay {
			delay = maxDelay
			break
		}
		delay = next
	}
	if delay > maxDelay {
		delay = maxDelay
	}
	return delay
}

func serviceCWD(root, cwd string) string {
	if cwd == "" {
		return root
	}
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(root, cwd)
	}
	return filepath.Clean(cwd)
}
func valueFromEnvironment(values []string, key string) string {
	for _, value := range values {
		if strings.HasPrefix(value, key+"=") {
			return strings.TrimPrefix(value, key+"=")
		}
	}
	return ""
}

func replaceEnvironmentValue(values []string, key, replacement string) []string {
	prefix := key + "="
	for index, value := range values {
		if strings.HasPrefix(value, prefix) {
			values[index] = prefix + replacement
			return values
		}
	}
	return append(values, prefix+replacement)
}

// List returns public status snapshots. It never exposes resolved secret
// values; only names, source metadata and exported variables are returned.
func (m *ServiceManager) List() []ServiceStatus {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sortedStatusLocked()
}

// NextDeadline returns the earliest persisted retry or readiness check due.
// A short fallback in the serve loop still observes process exits whose wait
// notification happened between reconcile passes.
func (m *ServiceManager) NextDeadline(now time.Time) time.Time {
	if m == nil {
		return time.Time{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var earliest time.Time
	for _, rt := range m.runtimes {
		if rt == nil || !rt.config.Enabled || rt.status.ManualStop {
			continue
		}
		if next, err := time.Parse(time.RFC3339Nano, rt.status.NextRetryAt); err == nil && !next.IsZero() {
			if earliest.IsZero() || next.Before(earliest) {
				earliest = next
			}
		}
		if rt.process != nil && rt.config.Readiness != nil {
			last, _ := time.Parse(time.RFC3339Nano, rt.status.Readiness.LastCheck)
			if last.IsZero() {
				last = now
			}
			check := last.Add(rt.config.Readiness.Interval)
			if earliest.IsZero() || check.Before(earliest) {
				earliest = check
			}
		}
	}
	return earliest
}

func (m *ServiceManager) Show(id string) (ServiceStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.runtimes[id]
	if rt == nil {
		return ServiceStatus{}, os.ErrNotExist
	}
	status := rt.status
	status.Exports = publicExports(rt.exports, rt.secretNames)
	return status, nil
}

func (m *ServiceManager) Exports(id string) (ServiceExports, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.runtimes[id]
	if rt == nil {
		return ServiceExports{}, os.ErrNotExist
	}
	if rt.exports.Variables == nil {
		if export, err := m.readExportsLocked(rt); err == nil {
			rt.exports = export
		}
	}
	return publicExports(rt.exports, rt.secretNames), nil
}

// Bindings returns the persisted Workspace binding references. It never
// resolves secret values, and a missing file is represented by an empty,
// current-schema binding set.
func (m *ServiceManager) Bindings() (ServiceBindings, error) {
	if m == nil {
		return ServiceBindings{}, errors.New("service manager is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadBindingsLocked()
}

// ApplyBindings validates and atomically persists Workspace binding
// references while holding the same lock as service lifecycle updates. The
// returned value is normalized to the current persistence schema.
func (m *ServiceManager) ApplyBindings(bindings ServiceBindings) (ServiceBindings, error) {
	if m == nil {
		return ServiceBindings{}, errors.New("service manager is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !pathWithinResolved(filepath.Join(m.root, ".pua"), serviceBindingsPath(m.root)) {
		return ServiceBindings{}, errServiceBindingsPathEscape
	}
	bindings.Variables = cloneStringMap(bindings.Variables)
	bindings.Secrets = cloneStringMap(bindings.Secrets)
	if err := m.validateBindingsLocked(bindings); err != nil {
		return ServiceBindings{}, err
	}
	bindings.SchemaVersion = serviceSchemaVersion
	if err := writeServiceJSON(serviceBindingsPath(m.root), bindings, 0o600); err != nil {
		return ServiceBindings{}, err
	}
	return bindings, nil
}

func (m *ServiceManager) validateBindingsLocked(bindings ServiceBindings) error {
	return m.validateBindingsForConfigsLocked(bindings, m.configs)
}

func (m *ServiceManager) validateBindingsForConfigsLocked(bindings ServiceBindings, configs map[string]ServiceConfig) error {
	if bindings.SchemaVersion != 0 && bindings.SchemaVersion != serviceSchemaVersion {
		return fmt.Errorf("unsupported schema version %d", bindings.SchemaVersion)
	}
	for name, value := range bindings.Variables {
		if !environmentNamePattern.MatchString(name) {
			return fmt.Errorf("invalid variable name %q", name)
		}
		if reservedServiceBindingName(name) {
			return fmt.Errorf("variable name %q is reserved for PUA provenance", name)
		}
		if secretTemplatePattern.MatchString(value) {
			return fmt.Errorf("secret references must not appear in variable mapping %q", name)
		}
		if strings.Contains(value, "${") {
			if err := validateEnvironmentTemplate(value, "", configs); err != nil {
				return fmt.Errorf("variable mapping %q: %w", name, err)
			}
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("variable %q contains NUL", name)
		}
		if match := serviceTemplatePattern.FindStringSubmatch(value); len(match) == 3 && match[0] == value {
			if rt := m.runtimes[match[1]]; rt != nil {
				if _, secret := rt.exports.Secrets[match[2]]; secret {
					return fmt.Errorf("secret service export %q must be mapped under secrets", match[2])
				}
			}
		}
	}
	for name, value := range bindings.Secrets {
		if !environmentNamePattern.MatchString(name) {
			return fmt.Errorf("invalid secret variable name %q", name)
		}
		if reservedServiceBindingName(name) {
			return fmt.Errorf("secret variable name %q is reserved for PUA provenance", name)
		}
		if match := secretTemplatePattern.FindStringSubmatch(value); len(match) == 2 && match[0] == value && validSecretName(match[1]) {
			continue
		}
		if match := serviceTemplatePattern.FindStringSubmatch(value); len(match) == 3 && match[0] == value {
			if _, ok := configs[match[1]]; !ok {
				return fmt.Errorf("secret mapping %q references unknown service %q", name, match[1])
			}
			continue
		}
		return fmt.Errorf("secret mapping %q must be a complete secret or service export reference", name)
	}
	return nil
}

func cloneServiceConfigs(configs map[string]ServiceConfig) map[string]ServiceConfig {
	result := make(map[string]ServiceConfig, len(configs))
	for id, cfg := range configs {
		result[id] = cloneServiceConfig(cfg)
	}
	return result
}

func cloneServiceConfig(cfg ServiceConfig) ServiceConfig {
	cfg.Command = append([]string(nil), cfg.Command...)
	cfg.Args = append([]string(nil), cfg.Args...)
	cfg.DependsOn = append([]string(nil), cfg.DependsOn...)
	if cfg.Environment != nil {
		environment := make(map[string]ServiceEnvironment, len(cfg.Environment))
		for name, value := range cfg.Environment {
			environment[name] = value
		}
		cfg.Environment = environment
	}
	if cfg.Readiness != nil {
		readiness := *cfg.Readiness
		readiness.Command = append([]string(nil), readiness.Command...)
		cfg.Readiness = &readiness
	}
	if cfg.Cleanup != nil {
		cleanup := *cfg.Cleanup
		cleanup.Command = append([]string(nil), cleanup.Command...)
		cfg.Cleanup = &cleanup
	}
	return cfg
}

func newPersistedServiceProcessConfig(rt *serviceRuntime) *persistedServiceProcessConfig {
	if rt == nil || rt.processConfig == nil {
		return nil
	}
	cfg := cloneServiceConfig(*rt.processConfig)
	digest := rt.processCommandDigest
	if digest == "" {
		digest = rt.status.CommandDigest
	}
	return &persistedServiceProcessConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            cfg.ID,
		CommandDigest: digest,
		CWD:           cfg.CWD,
		Environment:   cfg.Environment,
		Exports:       cfg.Exports,
		Cleanup:       cfg.Cleanup,
	}
}

func (cfg *persistedServiceProcessConfig) serviceConfig() ServiceConfig {
	if cfg == nil {
		return ServiceConfig{}
	}
	// A harmless absolute command lets the existing service validator check
	// the persisted cleanup CWD, environment declarations, and cleanup command.
	// The primary process command is deliberately absent from runtime state.
	return defaultServiceConfig(cloneServiceConfig(ServiceConfig{
		SchemaVersion: cfg.SchemaVersion,
		ID:            cfg.ID,
		Enabled:       true,
		Command:       []string{"/bin/true"},
		CWD:           cfg.CWD,
		Environment:   cfg.Environment,
		Exports:       cfg.Exports,
		Cleanup:       cfg.Cleanup,
	}))
}

func (m *ServiceManager) validateConfigTransactionLocked(configs map[string]ServiceConfig) (serviceDependencyGraph, error) {
	graph, err := validatedServiceDependencyGraph(m.root, configs)
	if err != nil {
		return nil, err
	}
	bindings, err := m.loadBindingsLocked()
	if err != nil {
		return nil, err
	}
	if err := m.validateBindingsForConfigsLocked(bindings, configs); err != nil {
		return nil, fmt.Errorf("service bindings: %w", err)
	}
	return graph, nil
}

func (m *ServiceManager) persistDefinitionLocked(cfg ServiceConfig) error {
	path := serviceConfigPath(m.root, cfg.ID)
	if !pathWithinResolved(filepath.Join(m.root, ".pua", serviceConfigDir), path) {
		return errors.New("service definition path escapes the workspace control directory")
	}
	store := m.definitionStore
	defaults := defaultServiceDefinitionStore()
	if store.writeJSON == nil {
		store.writeJSON = defaults.writeJSON
	}
	if store.rename == nil {
		store.rename = defaults.rename
	}
	return store.writeJSON(path, cfg, 0o600, store.rename)
}

func (m *ServiceManager) removeDefinitionLocked(id string) error {
	path := serviceConfigPath(m.root, id)
	if !pathWithinResolved(filepath.Join(m.root, ".pua", serviceConfigDir), path) {
		return errors.New("service definition path escapes the workspace control directory")
	}
	remove := m.definitionStore.remove
	if remove == nil {
		remove = os.Remove
	}
	if err := remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func serviceDefinitionOperationError(rt *serviceRuntime, err error) error {
	if err == nil || rt == nil || len(rt.secretValues) == 0 {
		return err
	}
	message := security.NewRedactor(rt.secretValues...).RedactString(err.Error())
	if message == err.Error() {
		return err
	}
	return errors.New(message)
}

// ApplyAll applies a collection request as one configuration transaction. The
// collection augments or replaces matching definitions, just as repeated
// Apply calls did historically, but validation and rollback cover the entire
// request rather than each element independently.
func (m *ServiceManager) ApplyAll(configs []ServiceConfig) error {
	if m == nil {
		return errors.New("service manager is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applyAllLocked(configs, true)
}

func (m *ServiceManager) applyAllLocked(configs []ServiceConfig, rollbackLifecycle bool) error {
	if len(configs) == 0 {
		return nil
	}

	next := cloneServiceConfigs(m.configs)
	requested := make(map[string]ServiceConfig, len(configs))
	for _, candidate := range configs {
		candidate = defaultServiceConfig(candidate)
		if _, duplicate := requested[candidate.ID]; duplicate {
			return ServiceValidationError{"services", fmt.Sprintf("duplicate service id %q", candidate.ID)}
		}
		requested[candidate.ID] = candidate
		next[candidate.ID] = candidate
	}
	graph, err := m.validateConfigTransactionLocked(next)
	if err != nil {
		return err
	}
	order, err := graph.topologicalOrder()
	if err != nil {
		return err
	}
	changed := make([]string, 0, len(requested))
	for _, id := range order {
		candidate, ok := requested[id]
		if !ok {
			continue
		}
		current, exists := m.configs[id]
		if !exists || serviceConfigDigest(current) != serviceConfigDigest(candidate) {
			changed = append(changed, id)
		}
	}
	if len(changed) == 0 {
		return nil
	}

	oldConfigs, oldGraph := m.configs, m.graph
	changedSet := make(map[string]struct{}, len(changed))
	for _, id := range changed {
		changedSet[id] = struct{}{}
	}
	impacted := serviceDependencyChangeSet(oldGraph, graph, changedSet)
	oldOrder, err := serviceGraphSubsetOrder(oldGraph, impacted)
	if err != nil {
		return err
	}
	oldStopOrder, err := serviceGraphSubsetStopOrder(oldGraph, impacted)
	if err != nil {
		return err
	}
	newOrder, err := serviceGraphSubsetOrder(graph, impacted)
	if err != nil {
		return err
	}
	newStopOrder, err := serviceGraphSubsetStopOrder(graph, impacted)
	if err != nil {
		return err
	}
	transactionIDs := mergeServiceOrders(oldOrder, newOrder)
	runtimeSnapshots := make(map[string]serviceRuntimeConfigSnapshot, len(transactionIDs))
	definitionSnapshots := make(map[string]serviceFileSnapshot, len(transactionIDs))
	stateSnapshots := make(map[string]serviceFileSnapshot, len(transactionIDs))
	eventSnapshots := make(map[string]serviceFileSnapshot, len(transactionIDs))
	for _, id := range transactionIDs {
		if rt := m.runtimes[id]; rt != nil {
			runtimeSnapshots[id] = snapshotServiceRuntimeConfig(rt)
		}
		definitionSnapshots[id], err = snapshotServiceFile(serviceConfigPath(m.root, id))
		if err != nil {
			return err
		}
		stateSnapshots[id], err = snapshotServiceFile(filepath.Join(serviceRuntimePath(m.root, id), "state.json"))
		if err != nil {
			return err
		}
		eventSnapshots[id], err = snapshotServiceFile(filepath.Join(serviceRuntimePath(m.root, id), "events.jsonl"))
		if err != nil {
			return err
		}
	}

	useDefinitionTransaction := rollbackLifecycle
	rollback := func(cause error) error {
		if !rollbackLifecycle {
			return cause
		}
		definitionRollbackErr := m.beginServiceDefinitionTransactionLocked(oldConfigs, changed, definitionSnapshots)
		rollbackErr := m.rollbackAppliedServicesLocked(transactionIDs, newStopOrder, oldOrder, oldConfigs, oldGraph, runtimeSnapshots, stateSnapshots, eventSnapshots)
		if definitionRollbackErr == nil && rollbackErr == nil {
			definitionRollbackErr = m.finishServiceDefinitionTransactionLocked()
		}
		return errors.Join(cause, definitionRollbackErr, rollbackErr)
	}
	if useDefinitionTransaction {
		if err := m.beginServiceDefinitionTransactionLocked(next, changed, nil); err != nil {
			return rollback(serviceDefinitionOperationError(nil, err))
		}
	} else {
		for _, id := range changed {
			cfg := requested[id]
			if err := m.persistDefinitionLocked(cfg); err != nil {
				return rollback(serviceDefinitionOperationError(m.runtimes[id], err))
			}
		}
	}

	m.configs, m.graph = next, graph
	for _, id := range changed {
		cfg := requested[id]
		rt := m.runtimes[id]
		if rt == nil {
			rt = &serviceRuntime{config: cfg, status: initialServiceStatus(cfg)}
			m.runtimes[id] = rt
		} else {
			rt.config = cfg
			rt.status.Enabled = cfg.Enabled
			rt.status.Readiness.Configured = cfg.Readiness != nil
			rt.status.Cleanup.Configured = cfg.Cleanup != nil
			if cfg.Enabled {
				if rt.status.State == ServiceStateDisabled {
					rt.status.State = ServiceStateStopped
				}
			} else {
				rt.status.State = ServiceStateDisabled
			}
		}
	}
	for _, id := range newOrder {
		if rt := m.runtimes[id]; rt != nil {
			rt.status.Dependencies = append([]string(nil), graph[id]...)
		}
	}
	// Existing processes reflect the old graph and old exports. Stop every
	// affected running generation in reverse old topological order before
	// reconciling the new graph. stopProcessLocked deliberately uses
	// processConfig, so each cleanup runs with the generation it belongs to.
	for _, id := range oldStopOrder {
		rt := m.runtimes[id]
		if rt == nil || (rt.process == nil && rt.status.ProcessGroup <= 0) {
			continue
		}
		if err := m.stopRuntimeForDependencyChangeLocked(context.Background(), rt); err != nil {
			return rollback(err)
		}
	}

	for _, id := range newOrder {
		rt := m.runtimes[id]
		if rt == nil {
			continue
		}
		rt.status.Dependencies = append([]string(nil), graph[id]...)
		if err := m.persistStatusLocked(rt); err != nil {
			return rollback(err)
		}
	}
	if m.started && !m.stopping {
		if err := m.reconcileLocked(context.Background()); err != nil {
			return rollback(err)
		}
	}
	if useDefinitionTransaction {
		if err := m.finishServiceDefinitionTransactionLocked(); err != nil {
			return rollback(err)
		}
	}
	return nil
}

// serviceDependencyChangeSet returns changed services plus every transitive
// dependent in either graph. The old graph identifies processes whose current
// environment may contain stale exports; the new graph covers newly affected
// relationships in a batch transaction.
func serviceDependencyChangeSet(oldGraph, newGraph serviceDependencyGraph, changed map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{}, len(changed))
	for id := range changed {
		result[id] = struct{}{}
	}
	for _, graph := range []serviceDependencyGraph{oldGraph, newGraph} {
		for added := true; added; {
			added = false
			for id, dependencies := range graph {
				if _, ok := result[id]; ok {
					continue
				}
				for _, dependency := range dependencies {
					if _, ok := result[dependency]; ok {
						result[id] = struct{}{}
						added = true
						break
					}
				}
			}
		}
	}
	return result
}

func serviceGraphSubsetOrder(graph serviceDependencyGraph, selected map[string]struct{}) ([]string, error) {
	order, err := graph.topologicalOrder()
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(selected))
	for _, id := range order {
		if _, ok := selected[id]; ok {
			result = append(result, id)
		}
	}
	return result, nil
}

func serviceGraphSubsetStopOrder(graph serviceDependencyGraph, selected map[string]struct{}) ([]string, error) {
	if _, err := graph.topologicalOrder(); err != nil {
		return nil, err
	}
	remaining := make(map[string]struct{}, len(selected))
	for id := range selected {
		if _, ok := graph[id]; ok {
			remaining[id] = struct{}{}
		}
	}
	result := make([]string, 0, len(remaining))
	for len(remaining) > 0 {
		candidates := make([]string, 0)
		for id := range remaining {
			hasDependent := false
			for other := range remaining {
				if other != id && containsString(graph[other], id) {
					hasDependent = true
					break
				}
			}
			if !hasDependent {
				candidates = append(candidates, id)
			}
		}
		if len(candidates) == 0 {
			return nil, ServiceValidationError{"dependencies", "dependency cycle"}
		}
		sort.Strings(candidates)
		for _, id := range candidates {
			delete(remaining, id)
			result = append(result, id)
		}
	}
	return result, nil
}

func mergeServiceOrders(orders ...[]string) []string {
	seen := make(map[string]struct{})
	var result []string
	for _, order := range orders {
		for _, id := range order {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			result = append(result, id)
		}
	}
	return result
}

func (m *ServiceManager) stopRuntimeForDependencyChangeLocked(ctx context.Context, rt *serviceRuntime) error {
	if rt == nil || (rt.process == nil && rt.status.ProcessGroup <= 0) {
		return nil
	}
	if err := m.stopProcessLocked(ctx, rt, false); err != nil {
		return err
	}
	rt.status.Readiness.Ready = false
	if !rt.status.AttentionRequired {
		if rt.config.Enabled {
			rt.status.State = ServiceStateStopped
		} else {
			rt.status.State = ServiceStateDisabled
		}
	}
	return m.persistStatusLocked(rt)
}

func snapshotServiceRuntimeConfig(rt *serviceRuntime) serviceRuntimeConfigSnapshot {
	return serviceRuntimeConfigSnapshot{
		runtime:                 rt,
		config:                  cloneServiceConfig(rt.config),
		processConfig:           cloneOptionalServiceConfig(rt.processConfig),
		processCommandDigest:    rt.processCommandDigest,
		status:                  cloneServiceStatus(rt.status),
		process:                 rt.process,
		exit:                    rt.exit,
		started:                 rt.started,
		stableSince:             rt.stableSince,
		environment:             append([]string(nil), rt.environment...),
		secretValues:            append([]string(nil), rt.secretValues...),
		secretNames:             cloneServiceSecretMetadataMap(rt.secretNames),
		exportSecrets:           cloneStringMap(rt.exportSecrets),
		exportAccepted:          rt.exportAccepted,
		exportViolation:         rt.exportViolation,
		exportCleanupFailure:    rt.exportCleanupFailure,
		rejectedExportSecrets:   append([]string(nil), rt.rejectedExportSecrets...),
		suppressHookDiagnostics: rt.suppressHookDiagnostics.Load(),
		pendingExportKeys:       cloneStringSet(rt.pendingExportKeys),
		exportKeysCommitted:     rt.exportKeysCommitted,
		redactor:                rt.redactor,
		exports:                 cloneServiceExportFile(rt.exports),
		logWriters:              append([]*serviceLogWriter(nil), rt.logWriters...),
		terminationPending:      rt.terminationPending,
		orphanRecoveryPending:   rt.orphanRecoveryPending,
		orphanCleanupComplete:   rt.orphanCleanupComplete,
		processOwnership:        rt.processOwnership,
	}
}

func cloneServiceStatus(status ServiceStatus) ServiceStatus {
	status.Dependencies = append([]string(nil), status.Dependencies...)
	status.Exports.Variables = cloneStringMap(status.Exports.Variables)
	if status.Exports.Secrets != nil {
		status.Exports.Secrets = append([]ServiceSecretMetadata{}, status.Exports.Secrets...)
	}
	return status
}

func cloneOptionalServiceConfig(cfg *ServiceConfig) *ServiceConfig {
	if cfg == nil {
		return nil
	}
	cloned := cloneServiceConfig(*cfg)
	return &cloned
}

func cloneServiceSecretMetadataMap(values map[string]ServiceSecretMetadata) map[string]ServiceSecretMetadata {
	if values == nil {
		return nil
	}
	result := make(map[string]ServiceSecretMetadata, len(values))
	for name, metadata := range values {
		result[name] = metadata
	}
	return result
}

func cloneServiceExportFile(export ServiceExportFile) ServiceExportFile {
	export.Variables = cloneStringMap(export.Variables)
	export.Secrets = cloneStringMap(export.Secrets)
	return export
}

func restoreServiceRuntimeConfig(rt *serviceRuntime, snapshot serviceRuntimeConfigSnapshot) {
	rt.config = cloneServiceConfig(snapshot.config)
	rt.processConfig = cloneOptionalServiceConfig(snapshot.processConfig)
	rt.processCommandDigest = snapshot.processCommandDigest
	rt.status = cloneServiceStatus(snapshot.status)
	rt.process = snapshot.process
	rt.exit = snapshot.exit
	rt.started = snapshot.started
	rt.stableSince = snapshot.stableSince
	rt.environment = append([]string(nil), snapshot.environment...)
	rt.secretValues = append([]string(nil), snapshot.secretValues...)
	rt.secretNames = cloneServiceSecretMetadataMap(snapshot.secretNames)
	rt.exportSecrets = cloneStringMap(snapshot.exportSecrets)
	rt.exportAccepted = snapshot.exportAccepted
	rt.exportViolation = snapshot.exportViolation
	rt.exportCleanupFailure = snapshot.exportCleanupFailure
	rt.rejectedExportSecrets = append([]string(nil), snapshot.rejectedExportSecrets...)
	rt.suppressHookDiagnostics.Store(snapshot.suppressHookDiagnostics)
	rt.pendingExportKeys = cloneStringSet(snapshot.pendingExportKeys)
	rt.exportKeysCommitted = snapshot.exportKeysCommitted
	rt.redactor = snapshot.redactor
	rt.exports = cloneServiceExportFile(snapshot.exports)
	rt.logWriters = append([]*serviceLogWriter(nil), snapshot.logWriters...)
	rt.terminationPending = snapshot.terminationPending
	rt.orphanRecoveryPending = snapshot.orphanRecoveryPending
	rt.orphanCleanupComplete = snapshot.orphanCleanupComplete
	rt.processOwnership = snapshot.processOwnership
}

func snapshotServiceFile(path string) (serviceFileSnapshot, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return serviceFileSnapshot{path: path}, nil
	}
	if err != nil {
		return serviceFileSnapshot{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return serviceFileSnapshot{}, err
	}
	return serviceFileSnapshot{path: path, data: data, mode: info.Mode().Perm(), exists: true}, nil
}

func restoreServiceFile(snapshot serviceFileSnapshot) error {
	if !snapshot.exists {
		if err := os.Remove(snapshot.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeServiceDataAtomic(snapshot.path, snapshot.data, snapshot.mode, os.Rename)
}

func (m *ServiceManager) rollbackAppliedServicesLocked(ids, stopOrder, restartOrder []string, configs map[string]ServiceConfig, graph serviceDependencyGraph, runtimes map[string]serviceRuntimeConfigSnapshot, states, events map[string]serviceFileSnapshot) error {
	var result error
	for _, id := range stopOrder {
		rt := m.runtimes[id]
		snapshot, existed := runtimes[id]
		if rt != nil && rt.process != nil && (!existed || rt.process != snapshot.process) {
			result = errors.Join(result, m.stopProcessLocked(context.Background(), rt, false))
		}
	}
	m.configs, m.graph = configs, graph
	restart := make(map[string]struct{})
	for _, id := range ids {
		snapshot, existed := runtimes[id]
		if !existed {
			delete(m.runtimes, id)
			continue
		}
		rt := snapshot.runtime
		current := m.runtimes[id]
		processPreserved := current != nil && current.process != nil && current.process == snapshot.process
		if snapshot.process == nil && snapshot.status.ProcessGroup > 0 && current != nil && current.process == nil && current.status.ProcessGroup == snapshot.status.ProcessGroup {
			processPreserved = true
		}
		restoreServiceRuntimeConfig(rt, snapshot)
		m.runtimes[id] = rt
		if (snapshot.process != nil || snapshot.status.ProcessGroup > 0) && !processPreserved {
			rt.process = nil
			rt.exit = nil
			rt.logWriters = nil
			rt.status.PID, rt.status.ProcessGroup = 0, 0
			rt.status.Readiness.Ready = false
			rt.status.State = ServiceStateStopped
			rt.processOwnership = serviceProcessOwnershipReconstructed
			restart[id] = struct{}{}
		}
	}
	for _, snapshots := range []map[string]serviceFileSnapshot{states, events} {
		for index := len(ids) - 1; index >= 0; index-- {
			result = errors.Join(result, restoreServiceFile(snapshots[ids[index]]))
		}
	}
	for _, id := range restartOrder {
		if _, ok := restart[id]; !ok {
			continue
		}
		if m.started && !m.stopping {
			result = errors.Join(result, m.reconcileOneLocked(context.Background(), m.runtimes[id], graph[id]))
		}
	}
	return result
}

func (m *ServiceManager) Apply(cfg ServiceConfig) error {
	if m == nil {
		return errors.New("service manager is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applyAllLocked([]ServiceConfig{cfg}, false)
}

func (m *ServiceManager) Remove(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.runtimes[id]
	if rt == nil {
		return os.ErrNotExist
	}
	for dependentID, dependencies := range m.graph {
		if dependentID != id && containsString(dependencies, id) {
			return fmt.Errorf("service %q is required by %q", id, dependentID)
		}
	}
	next := cloneServiceConfigs(m.configs)
	delete(next, id)
	graph, err := m.validateConfigTransactionLocked(next)
	if err != nil {
		return err
	}
	// Persist a disabled definition before any process side effect. If stopping
	// or final deletion fails (or the daemon exits between those steps), the
	// filesystem remains authoritative and reconstruction cannot restart the
	// service being removed.
	safeConfig := rt.config
	safeConfig.Enabled = false
	if err := m.persistDefinitionLocked(safeConfig); err != nil {
		return serviceDefinitionOperationError(rt, err)
	}
	retained := cloneServiceConfigs(m.configs)
	retained[id] = safeConfig
	m.configs = retained
	rt.config = safeConfig
	rt.status.Enabled = false
	if rt.process != nil || rt.status.ProcessGroup > 0 {
		if err := m.stopProcessLocked(ctx, rt, false); err != nil {
			return err
		}
	} else {
		exportCleanupErr := m.scrubActiveServiceExport(rt, rt.config, "scrub service export while removing service")
		if exportCleanupErr != nil {
			exportCleanupErr = errors.Join(exportProtocolViolationError(rt), exportCleanupErr)
		} else {
			exportCleanupErr = exportCleanupError(rt)
		}
		if err := exportCleanupErr; err != nil {
			rt.status.LastError = security.NewRedactor(rt.secretValues...).RedactString(err.Error())
			rt.status.AttentionRequired = true
			rt.status.State = ServiceStateAttentionRequired
			return errors.Join(err, m.persistStatusLocked(rt))
		}
	}
	// The disabled definition still exists until removeDefinitionLocked
	// succeeds, so a failed removal retains a safely stopped manager whose
	// visible state matches the durable source of truth.
	rt.status.ManualStop = false
	rt.status.Readiness.Ready = false
	if !rt.status.AttentionRequired {
		rt.status.State = ServiceStateDisabled
		rt.status.PID, rt.status.ProcessGroup = 0, 0
	}
	if err := m.persistStatusLocked(rt); err != nil {
		return err
	}
	if err := m.removeDefinitionLocked(id); err != nil {
		return serviceDefinitionOperationError(rt, err)
	}
	m.configs = next
	m.graph = graph
	delete(m.runtimes, id)
	return nil
}

func (m *ServiceManager) Enable(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, ok := m.configs[id]
	if !ok {
		return os.ErrNotExist
	}
	cfg.Enabled = true
	next := cloneServiceConfigs(m.configs)
	next[id] = cfg
	graph, err := m.validateConfigTransactionLocked(next)
	if err != nil {
		return err
	}
	rt := m.runtimes[id]
	if err := m.persistDefinitionLocked(cfg); err != nil {
		return serviceDefinitionOperationError(rt, err)
	}
	m.configs = next
	m.graph = graph
	rt.config = cfg
	rt.status.Enabled = true
	rt.status.ManualStop = false
	if rt.process == nil && rt.status.ProcessGroup <= 0 {
		rt.status.FailureCount = 0
		rt.status.AttentionRequired = false
		rt.status.LastError = ""
		rt.status.NextRetryAt = ""
		rt.status.State = ServiceStateStopped
	} else if rt.status.AttentionRequired {
		rt.status.State = ServiceStateAttentionRequired
	}
	if err := m.persistStatusLocked(rt); err != nil {
		return err
	}
	if m.started && !m.stopping {
		return m.reconcileLocked(context.Background())
	}
	return nil
}
func (m *ServiceManager) Disable(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, ok := m.configs[id]
	if !ok {
		return os.ErrNotExist
	}
	cfg.Enabled = false
	next := cloneServiceConfigs(m.configs)
	next[id] = cfg
	graph, err := m.validateConfigTransactionLocked(next)
	if err != nil {
		return err
	}
	rt := m.runtimes[id]
	if err := m.persistDefinitionLocked(cfg); err != nil {
		return serviceDefinitionOperationError(rt, err)
	}
	m.configs = next
	m.graph = graph
	rt.config = cfg
	rt.status.Enabled = false
	rt.status.ManualStop = false
	if err := m.persistStatusLocked(rt); err != nil {
		return err
	}
	if err := m.stopServiceDependencyChainLocked(ctx, id, false); err != nil {
		return err
	}
	rt.status.State = ServiceStateDisabled
	rt.status.Readiness.Ready = false
	rt.status.PID, rt.status.ProcessGroup = 0, 0
	return m.persistStatusLocked(rt)
}
func (m *ServiceManager) StartService(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping {
		return errServiceManagerStopping
	}
	rt := m.runtimes[id]
	if rt == nil {
		return os.ErrNotExist
	}
	if !rt.config.Enabled {
		return fmt.Errorf("start service %q: %w", id, errServiceDisabled)
	}
	if serviceRuntimeNeedsOrphanRecovery(rt) {
		rt.status.State = ServiceStateAttentionRequired
		rt.status.AttentionRequired = true
		if rt.status.LastError == "" {
			if rt.status.ProcessGroup > 0 {
				rt.status.LastError = fmt.Sprintf("service process group %d ownership is unresolved", rt.status.ProcessGroup)
			} else {
				rt.status.LastError = "service orphan recovery is pending"
			}
		}
		return errors.Join(errors.New(rt.status.LastError), m.persistStatusLocked(rt))
	}
	rt.status.ManualStop = false
	rt.status.FailureCount = 0
	rt.status.AttentionRequired = false
	rt.status.LastError = ""
	rt.status.ExitError = ""
	rt.status.NextRetryAt = ""
	rt.status.State = ServiceStateStopped
	if !m.started {
		m.started = true
	}
	if err := m.persistStatusLocked(rt); err != nil {
		return err
	}
	return m.reconcileLocked(ctx)
}
func (m *ServiceManager) StopService(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.runtimes[id]
	if rt == nil {
		return os.ErrNotExist
	}
	rt.status.ManualStop = true
	if err := m.persistStatusLocked(rt); err != nil {
		return err
	}
	if err := m.stopServiceDependencyChainLocked(ctx, id, true); err != nil {
		return err
	}
	exportCleanupErr := m.scrubActiveServiceExport(rt, rt.config, "scrub service export while stopping service")
	if exportCleanupErr != nil {
		exportCleanupErr = errors.Join(exportProtocolViolationError(rt), exportCleanupErr)
	} else {
		exportCleanupErr = exportCleanupError(rt)
	}
	if err := exportCleanupErr; err != nil {
		rt.status.LastError = security.NewRedactor(rt.secretValues...).RedactString(err.Error())
		rt.status.AttentionRequired = true
		rt.status.State = ServiceStateAttentionRequired
		return errors.Join(err, m.persistStatusLocked(rt))
	}
	rt.status.State = ServiceStateStopped
	return m.persistStatusLocked(rt)
}

// stopServiceDependencyChainLocked stops every running transitive dependent
// before the selected service. Only the selected service receives manual-stop
// intent; dependents remain eligible for dependency-ordered reconciliation
// when the selected service is explicitly started or enabled again.
func (m *ServiceManager) stopServiceDependencyChainLocked(ctx context.Context, id string, manualTarget bool) error {
	changed := map[string]struct{}{id: {}}
	impacted := serviceDependencyChangeSet(m.graph, m.graph, changed)
	stopOrder, err := serviceGraphSubsetStopOrder(m.graph, impacted)
	if err != nil {
		return err
	}
	for _, candidateID := range stopOrder {
		candidate := m.runtimes[candidateID]
		if candidate == nil || (candidate.process == nil && candidate.status.ProcessGroup <= 0) {
			continue
		}
		if candidateID == id && manualTarget {
			if err := m.stopProcessLocked(ctx, candidate, true); err != nil {
				return err
			}
			candidate.status.Readiness.Ready = false
			if !candidate.status.AttentionRequired {
				candidate.status.State = ServiceStateStopped
			}
			if err := m.persistStatusLocked(candidate); err != nil {
				return err
			}
			continue
		}
		if err := m.stopRuntimeForDependencyChangeLocked(ctx, candidate); err != nil {
			if candidateID != id {
				return fmt.Errorf("stop dependent service %q before %q: %w", candidateID, id, err)
			}
			return err
		}
	}
	return nil
}

func (m *ServiceManager) RestartService(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping {
		return errServiceManagerStopping
	}
	rt := m.runtimes[id]
	if rt == nil {
		return os.ErrNotExist
	}
	if !rt.config.Enabled {
		return fmt.Errorf("restart service %q: %w", id, errServiceDisabled)
	}
	changed := map[string]struct{}{id: {}}
	impacted := serviceDependencyChangeSet(m.graph, m.graph, changed)
	stopOrder, err := serviceGraphSubsetStopOrder(m.graph, impacted)
	if err != nil {
		return err
	}
	for _, candidateID := range stopOrder {
		candidate := m.runtimes[candidateID]
		if candidate == nil || (candidate.process == nil && candidate.status.ProcessGroup <= 0) {
			continue
		}
		if err := m.stopRuntimeForDependencyChangeLocked(ctx, candidate); err != nil {
			return err
		}
	}
	rt.status.ManualStop = false
	rt.status.FailureCount = 0
	rt.status.AttentionRequired = false
	rt.status.LastError = ""
	rt.status.ExitError = ""
	rt.status.NextRetryAt = ""
	rt.status.Readiness.Ready = false
	rt.status.State = ServiceStateStopped
	if !m.started {
		m.started = true
	}
	if err := m.persistStatusLocked(rt); err != nil {
		return err
	}
	return m.reconcileLocked(ctx)
}

func writeServiceJSON(path string, value any, mode os.FileMode) error {
	return writeServiceJSONWithRename(path, value, mode, os.Rename)
}

func writeServiceJSONWithRename(path string, value any, mode os.FileMode, rename func(string, string) error) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeServiceDataAtomic(path, data, mode, rename)
}

func writeServiceDataAtomic(path string, data []byte, mode os.FileMode, rename func(string, string) error) error {
	if rename == nil {
		rename = os.Rename
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".service-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := rename(tempPath, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// ValidateServices loads all definitions and validates dependency/template
// cycles without changing runtime state.
func ValidateServices(root string) error {
	m, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return validateServiceGraph(m.root, m.configs)
}

// ResolveBindings evaluates Workspace service bindings for an AgentHub
// launch. Variables are safe to persist in launchEnvironment; secret values
// are returned separately and must be sent only as an ephemeral overlay.
func (m *ServiceManager) ResolveBindings() (map[string]string, map[string]string, error) {
	if m == nil {
		return nil, nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	bindings, err := m.loadBindingsLocked()
	if err != nil {
		return nil, nil, err
	}
	variables := make(map[string]string, len(bindings.Variables))
	for key, value := range bindings.Variables {
		resolved, source, err := m.resolveEnvironmentValueLocked(ServiceEnvironment{Template: value})
		if value == "" || !strings.Contains(value, "${service.") {
			resolved = value
		}
		if err != nil {
			return nil, nil, fmt.Errorf("binding variable %s: %w", key, err)
		}
		if source == "service-secret" {
			return nil, nil, fmt.Errorf("secret service export %q must be mapped under secrets", key)
		}
		variables[key] = resolved
	}
	secrets := make(map[string]string, len(bindings.Secrets))
	for key, value := range bindings.Secrets {
		match := secretTemplatePattern.FindStringSubmatch(value)
		entry := ServiceEnvironment{}
		serviceSecret := false
		if len(match) == 2 {
			entry.SecretName = match[1]
		} else if serviceMatch := serviceTemplatePattern.FindStringSubmatch(value); len(serviceMatch) == 3 && serviceMatch[0] == value {
			entry.Template = value
			serviceSecret = true
		} else {
			return nil, nil, fmt.Errorf("binding secret %s is not a complete secret reference", key)
		}
		resolved, source, err := m.resolveEnvironmentValueLocked(entry)
		if err != nil {
			return nil, nil, fmt.Errorf("binding secret %s: %w", key, err)
		}
		if serviceSecret && source != "service-secret" {
			return nil, nil, fmt.Errorf("binding secret %s references a non-secret service export", key)
		}
		secrets[key] = resolved
	}
	return variables, secrets, nil
}

// LoadServiceConfig reads one file without starting it. It is used by CLI and
// API callers before Apply so invalid definitions are rejected atomically.
func LoadServiceConfig(path string) (ServiceConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ServiceConfig{}, err
	}
	var cfg ServiceConfig
	if err := decodeStrictServiceJSON(bytes.NewReader(data), &cfg); err != nil {
		return ServiceConfig{}, err
	}
	if cfg.ID == "" {
		cfg.ID = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return defaultServiceConfig(cfg), nil
}

func (m *ServiceManager) LogsContext(ctx context.Context, id, stream string, follow bool) (io.ReadCloser, error) {
	m.mu.Lock()
	rt := m.runtimes[id]
	m.mu.Unlock()
	if rt == nil {
		return nil, os.ErrNotExist
	}
	if stream != "stderr" {
		stream = "stdout"
	}
	path := filepath.Join(serviceRuntimePath(m.root, id), stream+".log")
	if !pathWithinResolved(filepath.Join(m.root, ".pua"), path) {
		return nil, errors.New("service log path escapes the workspace control directory")
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		if mkdirErr := os.MkdirAll(filepath.Dir(path), 0o700); mkdirErr != nil {
			return nil, mkdirErr
		}
		file, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	}
	if err != nil {
		return nil, err
	}
	if !follow {
		return file, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return &followLogReader{
		file:         file,
		path:         path,
		boundary:     filepath.Join(m.root, ".pua"),
		ctx:          ctx,
		pollInterval: 200 * time.Millisecond,
	}, nil
}

type followLogReader struct {
	file         *os.File
	path         string
	boundary     string
	ctx          context.Context
	pollInterval time.Duration
}

func (r *followLogReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		n, err := r.file.Read(p)
		if n > 0 {
			return n, nil
		}
		if err != io.EOF {
			return n, err
		}
		retry, refreshErr := r.refreshAtEOF()
		if refreshErr != nil {
			return 0, refreshErr
		}
		if retry {
			continue
		}
		select {
		case <-r.ctx.Done():
			return 0, io.EOF
		case <-time.After(r.pollInterval):
		}
	}
}

// refreshAtEOF follows the active log pathname after rotation and rewinds a
// file that was truncated in place. When rotation replaces the active inode,
// the old descriptor is drained before it is closed so writes completed just
// before the rename are not skipped.
func (r *followLogReader) refreshAtEOF() (bool, error) {
	offset, err := r.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return false, err
	}
	currentInfo, err := r.file.Stat()
	if err != nil {
		return false, err
	}
	if currentInfo.Size() > offset {
		return true, nil
	}

	activeInfo, err := os.Lstat(r.path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if activeInfo.Mode()&os.ModeSymlink != 0 || !pathWithinResolved(r.boundary, r.path) {
		return false, fmt.Errorf("refusing unsafe service log %s", r.path)
	}
	if os.SameFile(currentInfo, activeInfo) {
		if currentInfo.Size() < offset {
			_, err = r.file.Seek(0, io.SeekStart)
			return err == nil, err
		}
		return false, nil
	}

	replacement, err := os.Open(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	replacementInfo, err := replacement.Stat()
	if err != nil {
		_ = replacement.Close()
		return false, err
	}
	activeInfo, err = os.Lstat(r.path)
	if err != nil || activeInfo.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(replacementInfo, activeInfo) || !pathWithinResolved(r.boundary, r.path) {
		_ = replacement.Close()
		if err != nil && !os.IsNotExist(err) {
			return false, err
		}
		return false, nil
	}

	// The coordinated sink closes its writer before renaming, so once the
	// replacement is visible this final size check accounts for all data on the
	// old inode.
	currentInfo, err = r.file.Stat()
	if err != nil {
		_ = replacement.Close()
		return false, err
	}
	if currentInfo.Size() > offset {
		_ = replacement.Close()
		return true, nil
	}
	old := r.file
	r.file = replacement
	if err := old.Close(); err != nil {
		return false, err
	}
	return true, nil
}

func (r *followLogReader) Close() error { return r.file.Close() }
