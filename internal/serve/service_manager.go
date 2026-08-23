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

var errServiceBindingsPathEscape = errors.New("service bindings path escapes the workspace control directory")

type serviceProcessExit struct {
	err  error
	code int
}

type serviceRuntime struct {
	config                ServiceConfig
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
	rejectedExportSecrets []string
	redactor              *security.Redactor
	exports               ServiceExportFile
	logWriters            []*serviceLogWriter
}

// ServiceManager owns all mutable service state for one Workspace. It is the
// only component that starts/stops service processes and writes runtime state.
type ServiceManager struct {
	root     string
	mu       sync.Mutex
	configs  map[string]ServiceConfig
	runtimes map[string]*serviceRuntime
	resolver ServiceSecretResolver
	now      func() time.Time
	ctx      context.Context
	stopping bool
	started  bool
}

// NewServiceManager loads versioned service definitions for root. A missing
// .pua/services directory is a valid empty configuration for old Workspaces.
func NewServiceManager(root string, options ServiceManagerOptions) (*ServiceManager, error) {
	root, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("workspace root is required")
	}
	m := &ServiceManager{root: filepath.Clean(root), configs: map[string]ServiceConfig{}, runtimes: map[string]*serviceRuntime{}, resolver: options.Resolver, now: options.Now}
	if m.now == nil {
		m.now = time.Now
	}
	if err := m.loadLocked(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *ServiceManager) Root() string {
	if m == nil {
		return ""
	}
	return m.root
}

func (m *ServiceManager) loadLocked() error {
	dir := filepath.Join(m.root, ".pua", serviceConfigDir)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read service directory: %w", err)
	}
	if !pathWithinResolved(filepath.Join(m.root, ".pua"), dir) {
		return errors.New("service directory must remain inside the workspace control directory")
	}
	configs := make(map[string]ServiceConfig)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || entry.Name() == serviceBindingsFile {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		servicePath := filepath.Join(dir, entry.Name())
		if !pathWithinResolved(dir, servicePath) {
			return fmt.Errorf("service %s path escapes the workspace control directory", id)
		}
		data, err := os.ReadFile(servicePath)
		if err != nil {
			return fmt.Errorf("read service %s: %w", id, err)
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		var cfg ServiceConfig
		if err := decoder.Decode(&cfg); err != nil {
			return fmt.Errorf("decode service %s: %w", id, err)
		}
		if cfg.ID == "" {
			cfg.ID = id
		}
		if cfg.ID != id {
			return fmt.Errorf("service %s has mismatched id %q", id, cfg.ID)
		}
		configs[id] = defaultServiceConfig(cfg)
	}
	if err := validateServiceGraph(m.root, configs); err != nil {
		return err
	}
	m.configs = configs
	for id, cfg := range configs {
		rt := m.runtimes[id]
		if rt == nil {
			rt = &serviceRuntime{config: cfg, status: ServiceStatus{SchemaVersion: serviceSchemaVersion, ID: id, Exports: ServiceExports{Variables: map[string]string{}}}}
			m.runtimes[id] = rt
		} else {
			rt.config = cfg
		}
		_ = m.loadStatusLocked(rt)
	}
	for id := range m.runtimes {
		if _, ok := configs[id]; !ok && m.runtimes[id].process == nil {
			delete(m.runtimes, id)
		}
	}
	if err := m.validateBindingsFileLocked(); err != nil {
		return err
	}
	return nil
}

func (m *ServiceManager) validateBindingsFileLocked() error {
	path := serviceBindingsPath(m.root)
	if !pathWithinResolved(filepath.Join(m.root, ".pua"), path) {
		return errServiceBindingsPathEscape
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read service bindings: %w", err)
	}
	var bindings ServiceBindings
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bindings); err != nil {
		return fmt.Errorf("decode service bindings: %w", err)
	}
	if err := m.validateBindingsLocked(bindings); err != nil {
		return fmt.Errorf("service bindings: %w", err)
	}
	return nil
}

func (m *ServiceManager) loadStatusLocked(rt *serviceRuntime) error {
	path := filepath.Join(serviceRuntimePath(m.root, rt.status.ID), "state.json")
	if !pathWithinResolved(filepath.Join(m.root, ".pua"), path) {
		return errors.New("service runtime state path escapes the workspace control directory")
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		rt.status = initialServiceStatus(rt.config)
		return nil
	}
	if err != nil {
		return err
	}
	var status ServiceStatus
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&status); err != nil {
		return err
	}
	if status.ID != rt.config.ID {
		return fmt.Errorf("runtime state id %q does not match service %q", status.ID, rt.config.ID)
	}
	if status.SchemaVersion == 0 {
		status.SchemaVersion = serviceSchemaVersion
	}
	if status.Exports.Variables == nil {
		status.Exports.Variables = map[string]string{}
	}
	rt.status = status
	rt.exports = ServiceExportFile{SchemaVersion: serviceExportSchema, Variables: cloneStringMap(status.Exports.Variables)}
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
	m.ctx = ctx
	m.stopping = false
	m.started = true
	for _, rt := range m.runtimes {
		m.recoverOrphanLocked(rt)
	}
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
		if rt.process != nil {
			cleanupErr = m.stopProcessLocked(ctx, rt, false)
			if cleanupErr != nil && first == nil {
				first = cleanupErr
			}
		}
		rt.status.ManualStop = false
		if rt.config.Enabled {
			if cleanupErr != nil {
				rt.status.AttentionRequired = true
				rt.status.State = ServiceStateAttentionRequired
			} else if rt.status.State != ServiceStateBackoff && rt.status.State != ServiceStateAttentionRequired {
				rt.status.State = ServiceStateStopped
			}
		} else {
			rt.status.State = ServiceStateDisabled
		}
		rt.status.PID, rt.status.ProcessGroup = 0, 0
		m.persistStatusLocked(rt)
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
	if m.ctx == nil {
		m.ctx = ctx
	}
	ids := m.sortedIDsLocked()
	var first error
	for _, id := range ids {
		rt := m.runtimes[id]
		if rt == nil {
			continue
		}
		if err := m.reconcileOneLocked(ctx, rt); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (m *ServiceManager) reconcileOneLocked(ctx context.Context, rt *serviceRuntime) error {
	status := &rt.status
	cfg := rt.config
	status.Enabled = cfg.Enabled
	status.Dependencies = append([]string(nil), cfg.DependsOn...)
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
		status.PID, status.ProcessGroup = 0, 0
		m.persistStatusLocked(rt)
		return nil
	}
	if status.ManualStop && rt.process == nil {
		status.State = ServiceStateStopped
		m.persistStatusLocked(rt)
		return nil
	}
	if rt.process != nil {
		select {
		case exit := <-rt.exit:
			m.handleProcessExitLocked(ctx, rt, exit)
		default:
		}
		if rt.process == nil {
			return nil
		}
	}
	if !m.dependenciesReadyLocked(cfg) {
		if rt.process != nil {
			if err := m.stopProcessLocked(ctx, rt, false); err != nil {
				status.AttentionRequired = true
				status.State = ServiceStateAttentionRequired
				m.persistStatusLocked(rt)
				return nil
			}
		}
		if status.AttentionRequired {
			status.State = ServiceStateAttentionRequired
		} else {
			status.State = ServiceStateBlocked
		}
		status.Readiness.Ready = false
		m.persistStatusLocked(rt)
		return nil
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

func (m *ServiceManager) dependenciesReadyLocked(cfg ServiceConfig) bool {
	for _, dep := range cfg.DependsOn {
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
	if requiresInitialExport(cfg) {
		exportPath := filepath.Join(serviceRuntimePath(m.root, cfg.ID), "export.json")
		if err := os.Remove(exportPath); err != nil && !os.IsNotExist(err) {
			return m.failStartLocked(ctx, rt, fmt.Errorf("clear previous service export: %w", err))
		}
	}
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
	redactor := security.NewRedactor(secrets...)
	stdoutSink := newServiceLogSink(filepath.Join(serviceRuntimePath(m.root, cfg.ID), "stdout.log"), cfg.LogRotation)
	stderrSink := newServiceLogSink(filepath.Join(serviceRuntimePath(m.root, cfg.ID), "stderr.log"), cfg.LogRotation)
	gatedLogs := requiresInitialExport(cfg)
	var exportGuard func() error
	if gatedLogs {
		exportGuard = func() error { return m.guardServiceLogExport(rt) }
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
	rt.rejectedExportSecrets = nil
	rt.redactor = redactor
	rt.exports = exports
	if err := cmd.Start(); err != nil {
		_ = stdoutWriter.Close()
		_ = stderrWriter.Close()
		return m.failStartLocked(ctx, rt, err)
	}
	processStartID := ""
	if serviceProcessIdentityRequired() {
		processIdentity, identityErr := readServiceProcessIdentity(cmd.Process.Pid)
		if identityErr != nil || processIdentity.processGroup != cmd.Process.Pid || processIdentity.startID == "" {
			_ = terminateProcessGroup(cmd.Process.Pid, true)
			_ = cmd.Wait()
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
	rt.status.InstanceToken = valueFromEnvironment(env, "PUA_SERVICE_INSTANCE_TOKEN")
	rt.status.ProcessStartID = processStartID
	rt.status.Readiness = ServiceReadinessStatus{Configured: cfg.Readiness != nil}
	rt.status.Cleanup = ServiceCleanupStatus{Configured: cfg.Cleanup != nil}
	rt.status.Exports = publicExports(exports, names)
	rt.status.UpdatedAt = rt.started.Format(time.RFC3339Nano)
	_ = m.appendEventLocked(rt, map[string]any{"type": "started", "pid": cmd.Process.Pid, "time": rt.status.StartedAt})
	m.persistStatusLocked(rt)
	return m.observeReadyLocked(ctx, rt)
}

func (m *ServiceManager) failStartLocked(ctx context.Context, rt *serviceRuntime, cause error) error {
	message := security.NewRedactor(rt.secretValues...).RedactString(cause.Error())
	rt.status.FailureCount++
	rt.status.LastError = message
	rt.status.AttentionRequired = rt.status.FailureCount >= 5
	rt.status.State = ServiceStateBackoff
	if rt.status.AttentionRequired {
		rt.status.State = ServiceStateAttentionRequired
	}
	delay := restartDelay(rt.config.Restart, rt.status.FailureCount)
	rt.status.NextRetryAt = m.now().Add(delay).Format(time.RFC3339Nano)
	rt.status.UpdatedAt = m.now().Format(time.RFC3339Nano)
	_ = m.appendEventLocked(rt, map[string]any{"type": "start_failed", "error": message, "time": rt.status.UpdatedAt})
	if cleanupErr := m.runCleanupLocked(ctx, rt); cleanupErr != nil {
		cleanupMessage := security.NewRedactor(rt.secretValues...).RedactString(cleanupErr.Error())
		rt.status.AttentionRequired = true
		rt.status.State = ServiceStateAttentionRequired
		rt.status.LastError = message + "; " + cleanupMessage
	}
	m.persistStatusLocked(rt)
	return nil
}

func (m *ServiceManager) observeReadyLocked(ctx context.Context, rt *serviceRuntime) error {
	if rt.process == nil {
		return nil
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
		exports, err := m.readExportsLocked(rt)
		if err != nil && requiresInitialExport(rt.config) && strings.Contains(err.Error(), "initial export") {
			exports, err = m.waitForInitialExportLocked(ctx, rt)
		}
		if err != nil {
			return m.readinessFailedLocked(ctx, rt, err)
		}
		rt.exports = exports
		if err := m.releaseStartupLogsLocked(rt); err != nil {
			return m.readinessFailedLocked(ctx, rt, err)
		}
		rt.status.State = ServiceStateReady
		rt.status.Readiness.Ready = true
		rt.status.Exports = publicExports(rt.exports, rt.secretNames)
		m.persistStatusLocked(rt)
		return nil
	}
	last, _ := time.Parse(time.RFC3339Nano, rt.status.Readiness.LastCheck)
	if !last.IsZero() && now.Sub(last) < rt.config.Readiness.Interval && rt.status.Readiness.Ready {
		return nil
	}
	exports, err := m.readExportsLocked(rt)
	if err != nil && !rt.status.Readiness.Ready && strings.Contains(err.Error(), "initial export") {
		exports, err = m.waitForInitialExportLocked(ctx, rt)
	}
	if err != nil {
		return m.readinessFailedLocked(ctx, rt, err)
	}
	rt.exports = exports
	if err := m.runReadinessLocked(ctx, rt); err != nil {
		return m.readinessFailedLocked(ctx, rt, err)
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
	m.persistStatusLocked(rt)
	return nil
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
	m.promoteRejectedExportSecretsLocked(rt)
	if rt.exportViolation != "" {
		return ServiceExportFile{}, errors.New(rt.exportViolation)
	}
	return m.readExportsWithGateLocked(rt, false)
}

// guardServiceLogExport runs in the os/exec pipe copier before raw service
// bytes enter the streaming redactor. Atomic export replacements are therefore
// observed in process order before output written after the replacement can be
// persisted. It never takes the manager mutex because shutdown closes writers
// while holding that mutex.
func (m *ServiceManager) guardServiceLogExport(rt *serviceRuntime) error {
	rt.exportMu.Lock()
	defer rt.exportMu.Unlock()
	if !rt.exportAccepted {
		return nil
	}
	if rt.exportViolation != "" {
		return errors.New(rt.exportViolation)
	}
	_, err := m.readExportsWithGateLocked(rt, true)
	return err
}

func (m *ServiceManager) readExportsWithGateLocked(rt *serviceRuntime, fromLog bool) (ServiceExportFile, error) {
	path := filepath.Join(serviceRuntimePath(m.root, rt.config.ID), "export.json")
	if !pathWithinResolved(filepath.Join(m.root, ".pua"), path) {
		return ServiceExportFile{}, m.rejectExportLocked(rt, errors.New("service export path escapes the workspace control directory"), nil, fromLog)
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if requiresInitialExport(rt.config) {
			message := "service has not written its initial export"
			if rt.exportAccepted {
				message = "service removed its accepted export hand-off"
			}
			return ServiceExportFile{}, m.rejectExportLocked(rt, errors.New(message), nil, fromLog)
		}
		return ServiceExportFile{SchemaVersion: serviceExportSchema, Variables: map[string]string{}}, nil
	}
	if err != nil {
		return ServiceExportFile{}, m.rejectExportLocked(rt, err, nil, fromLog)
	}
	if len(data) > 1<<20 {
		cause := errors.New("service export exceeds 1 MiB")
		cause = scrubRejectedExport(path, serviceExportSchema, cause)
		return ServiceExportFile{}, m.rejectExportLocked(rt, cause, nil, fromLog)
	}
	var export ServiceExportFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&export); err != nil {
		cause := scrubRejectedExport(path, serviceExportSchema, fmt.Errorf("decode export: %w", err))
		return ServiceExportFile{}, m.rejectExportLocked(rt, cause, nil, fromLog)
	}
	if export.SchemaVersion != serviceExportSchema {
		cause := scrubRejectedExport(path, serviceExportSchema, fmt.Errorf("unsupported export schema version %d", export.SchemaVersion))
		return ServiceExportFile{}, m.rejectExportLocked(rt, cause, nil, fromLog)
	}
	if export.Variables == nil {
		export.Variables = map[string]string{}
	}
	if rt.secretNames == nil {
		rt.secretNames = map[string]ServiceSecretMetadata{}
	}
	candidateSecrets := make([]string, 0, len(export.Secrets))
	for name, value := range export.Secrets {
		candidateSecrets = append(candidateSecrets, value)
		if rt.redactor != nil {
			rt.redactor.Register(value)
		}
		if !validSecretName(name) || strings.ContainsRune(value, '\x00') {
			cause := scrubRejectedExport(path, export.SchemaVersion, fmt.Errorf("invalid exported secret %q", name))
			return ServiceExportFile{}, m.rejectExportLocked(rt, cause, candidateSecrets, fromLog)
		}
	}
	candidateRedactor := security.NewRedactor(candidateSecrets...)
	for name, value := range export.Variables {
		if !environmentNamePattern.MatchString(name) || strings.ContainsRune(value, '\x00') {
			cause := scrubRejectedExport(path, export.SchemaVersion, fmt.Errorf("invalid exported variable %q", name))
			return ServiceExportFile{}, m.rejectExportLocked(rt, cause, candidateSecrets, fromLog)
		}
		if candidateRedactor.ContainsSecret([]byte(value)) || rt.redactor != nil && rt.redactor.ContainsSecret([]byte(value)) {
			cause := scrubRejectedExport(path, export.SchemaVersion, fmt.Errorf("exported variable %q contains a secret; place it under secrets", name))
			return ServiceExportFile{}, m.rejectExportLocked(rt, cause, candidateSecrets, fromLog)
		}
	}
	if rt.exportAccepted && export.Secrets != nil && !equalStringMap(export.Secrets, rt.exportSecrets) {
		sanitized := ServiceExportFile{SchemaVersion: export.SchemaVersion, Variables: cloneStringMap(export.Variables)}
		if err := writeSanitizedExport(path, sanitized); err != nil {
			return ServiceExportFile{}, m.rejectExportLocked(rt, fmt.Errorf("scrub rejected exported secrets: %w", err), candidateSecrets, fromLog)
		}
		return ServiceExportFile{}, m.rejectExportLocked(rt, errors.New("service exported secrets are immutable after the initial hand-off"), candidateSecrets, fromLog)
	}
	if len(export.Secrets) > 0 {
		if !rt.exportAccepted {
			rt.exportSecrets = cloneStringMap(export.Secrets)
			for name, value := range export.Secrets {
				rt.secretNames[name] = ServiceSecretMetadata{Name: name, Source: "service-export", UpdatedAt: m.now().Format(time.RFC3339Nano)}
				if !containsString(rt.secretValues, value) {
					rt.secretValues = append(rt.secretValues, value)
				}
			}
		}
		// The export file is an IPC hand-off, not durable secret storage. Keep
		// variables available for later reads but atomically replace the file
		// with a secret-free representation as soon as its secret values have
		// been registered in memory. A failure to scrub is fail-closed.
		sanitized := ServiceExportFile{SchemaVersion: export.SchemaVersion, Variables: cloneStringMap(export.Variables)}
		if err := writeSanitizedExport(path, sanitized); err != nil {
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

func (m *ServiceManager) rejectExportLocked(rt *serviceRuntime, cause error, secretValues []string, fromLog bool) error {
	if !rt.exportAccepted && !fromLog {
		return cause
	}
	message := cause.Error()
	if rt.exportViolation == "" {
		rt.exportViolation = message
	}
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
	return errors.New(rt.exportViolation)
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
	if rt.exportViolation == "" {
		return nil
	}
	return errors.New(rt.exportViolation)
}

func scrubRejectedExport(path string, schemaVersion int, cause error) error {
	if err := writeSanitizedExport(path, ServiceExportFile{SchemaVersion: schemaVersion, Variables: map[string]string{}}); err != nil {
		return fmt.Errorf("%v; scrub rejected export: %w", cause, err)
	}
	return cause
}

func writeSanitizedExport(path string, export ServiceExportFile) error {
	if err := writeServiceJSON(path, export, 0o600); err != nil {
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("write sanitized export: %v; remove rejected export: %w", err, removeErr)
		}
		return err
	}
	return nil
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
	cmd := exec.CommandContext(checkCtx, command[0], command[1:]...)
	cmd.Dir = serviceCWD(m.root, rt.config.CWD)
	if !pathWithinResolved(m.root, cmd.Dir) {
		return errors.New("readiness cwd escapes the workspace")
	}
	cmd.Env = rt.environment
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output bytes.Buffer
	redactor := security.NewRedactor(rt.secretValues...)
	stream := redactor.NewStream(&output)
	defer stream.Close()
	cmd.Stdout, cmd.Stderr = stream, stream
	err := cmd.Run()
	if err != nil && cmd.Process != nil {
		_ = terminateProcessGroup(cmd.Process.Pid, true)
	}
	if err != nil {
		text := strings.TrimSpace(redactor.RedactString(output.String()))
		if text != "" {
			return fmt.Errorf("readiness failed: %w: %s", err, text)
		}
		return fmt.Errorf("readiness failed: %w", err)
	}
	return nil
}

func (m *ServiceManager) readinessFailedLocked(ctx context.Context, rt *serviceRuntime, cause error) error {
	message := security.NewRedactor(rt.secretValues...).RedactString(cause.Error())
	rt.status.Readiness.Ready = false
	rt.status.Readiness.LastCheck = m.now().Format(time.RFC3339Nano)
	rt.status.Readiness.LastError = message
	rt.status.LastError = message
	_ = m.appendEventLocked(rt, map[string]any{"type": "readiness_failed", "error": message, "time": rt.status.Readiness.LastCheck})
	// An export may have been written even though the readiness command failed;
	// load it before closing the gated log writers so any newly exported secret
	// is included in cleanup/error redaction. The startup buffer is discarded
	// because readiness did not establish that the service is safe to publish.
	_, _ = m.readExportsLocked(rt)
	cleanupErr := m.stopProcessLocked(ctx, rt, false)
	rt.status.FailureCount++
	rt.status.AttentionRequired = rt.status.FailureCount >= 5
	rt.status.State = ServiceStateBackoff
	if rt.status.AttentionRequired {
		rt.status.State = ServiceStateAttentionRequired
	}
	rt.status.NextRetryAt = m.now().Add(restartDelay(rt.config.Restart, rt.status.FailureCount)).Format(time.RFC3339Nano)
	if cleanupErr != nil {
		rt.status.AttentionRequired = true
		rt.status.State = ServiceStateAttentionRequired
		cleanupMessage := security.NewRedactor(rt.secretValues...).RedactString(cleanupErr.Error())
		rt.status.LastError = message + "; " + cleanupMessage
	}
	m.persistStatusLocked(rt)
	return nil
}

func (m *ServiceManager) handleProcessExitLocked(ctx context.Context, rt *serviceRuntime, exit serviceProcessExit) {
	if rt.process == nil {
		return
	}
	var exportErr error
	if requiresInitialExport(rt.config) {
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
	rt.process = nil
	rt.exit = nil
	rt.logWriters = nil
	rt.status.PID, rt.status.ProcessGroup = 0, 0
	rt.status.ExitedAt = m.now().Format(time.RFC3339Nano)
	rt.status.ExitCode = exit.code
	if exit.err != nil {
		rt.status.ExitError = security.NewRedactor(rt.secretValues...).RedactString(exit.err.Error())
	}
	if rt.status.ManualStop || m.stopping || !rt.config.Enabled {
		rt.status.State = ServiceStateStopped
		m.persistStatusLocked(rt)
		return
	}
	if rt.config.Restart.ResetAfter > 0 && !rt.stableSince.IsZero() && m.now().Sub(rt.stableSince) >= rt.config.Restart.ResetAfter {
		rt.status.FailureCount = 0
		rt.status.AttentionRequired = false
	}
	rt.status.FailureCount++
	rt.status.AttentionRequired = rt.status.FailureCount >= 5
	rt.status.State = ServiceStateBackoff
	if rt.status.AttentionRequired {
		rt.status.State = ServiceStateAttentionRequired
	}
	rt.status.LastError = rt.status.ExitError
	rt.status.NextRetryAt = m.now().Add(restartDelay(rt.config.Restart, rt.status.FailureCount)).Format(time.RFC3339Nano)
	rt.status.UpdatedAt = rt.status.ExitedAt
	_ = m.appendEventLocked(rt, map[string]any{"type": "exited", "code": exit.code, "error": rt.status.ExitError, "time": rt.status.ExitedAt})
	cleanupErr := m.runCleanupLocked(ctx, rt)
	if cleanupErr != nil {
		rt.status.AttentionRequired = true
		rt.status.State = ServiceStateAttentionRequired
		rt.status.LastError = security.NewRedactor(rt.secretValues...).RedactString(cleanupErr.Error())
	}
	m.persistStatusLocked(rt)
}

func (m *ServiceManager) stopProcessLocked(ctx context.Context, rt *serviceRuntime, manual bool) error {
	if rt.process == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if manual {
		rt.status.ManualStop = true
	}
	pid := rt.process.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	wait := rt.exit
	timer := time.NewTimer(5 * time.Second)
	select {
	case <-wait:
	case <-timer.C:
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	case <-ctx.Done():
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	if requiresInitialExport(rt.config) {
		_, _ = m.readExportsLocked(rt)
	}
	for _, writer := range rt.logWriters {
		_ = writer.Close()
	}
	_ = m.exportProtocolErrorLocked(rt)
	rt.process = nil
	rt.exit = nil
	rt.logWriters = nil
	rt.status.PID, rt.status.ProcessGroup = 0, 0
	if err := m.runCleanupLocked(ctx, rt); err != nil {
		rt.status.LastError = security.NewRedactor(rt.secretValues...).RedactString(err.Error())
		rt.status.Cleanup.LastError = rt.status.LastError
		rt.status.State = ServiceStateAttentionRequired
		rt.status.AttentionRequired = true
		m.persistStatusLocked(rt)
		return err
	}
	return nil
}

func (m *ServiceManager) runCleanupLocked(ctx context.Context, rt *serviceRuntime) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if rt.config.Cleanup == nil || len(rt.config.Cleanup.Command) == 0 {
		return nil
	}
	cleanup := rt.config.Cleanup
	attempts := cleanup.Retries + 1
	if attempts < 1 {
		attempts = 1
	}
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		checkCtx, cancel := context.WithTimeout(ctx, cleanup.Timeout)
		cmd := exec.CommandContext(checkCtx, cleanup.Command[0], cleanup.Command[1:]...)
		cmd.Dir = serviceCWD(m.root, rt.config.CWD)
		if !pathWithinResolved(m.root, cmd.Dir) {
			cancel()
			last = errors.New("cleanup cwd escapes the workspace")
			rt.status.Cleanup.Attempts++
			rt.status.Cleanup.LastRun = m.now().Format(time.RFC3339Nano)
			rt.status.Cleanup.LastError = last.Error()
			continue
		}
		cmd.Env = rt.environment
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		var output bytes.Buffer
		redactor := security.NewRedactor(rt.secretValues...)
		stream := redactor.NewStream(&output)
		cmd.Stdout, cmd.Stderr = stream, stream
		err := cmd.Run()
		if err != nil && cmd.Process != nil {
			_ = terminateProcessGroup(cmd.Process.Pid, true)
		}
		_ = stream.Close()
		cancel()
		rt.status.Cleanup.Attempts++
		rt.status.Cleanup.LastRun = m.now().Format(time.RFC3339Nano)
		if err == nil {
			rt.status.Cleanup.Succeeded = true
			rt.status.Cleanup.LastError = ""
			return nil
		}
		message := strings.TrimSpace(redactor.RedactString(output.String()))
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

func (m *ServiceManager) resolveEnvironmentLocked(cfg ServiceConfig) ([]string, []string, map[string]ServiceSecretMetadata, ServiceExportFile, error) {
	values := append([]string(nil), os.Environ()...)
	byName := make(map[string]string, len(values))
	for _, value := range values {
		if key, val, ok := strings.Cut(value, "="); ok {
			byName[key] = val
		}
	}
	secrets := []string{}
	names := map[string]ServiceSecretMetadata{}
	exports := ServiceExportFile{SchemaVersion: serviceExportSchema, Variables: map[string]string{}}
	for name, entry := range cfg.Environment {
		value, source, err := m.resolveEnvironmentValueLocked(entry)
		if err != nil {
			return nil, nil, nil, exports, fmt.Errorf("environment %s: %w", name, err)
		}
		byName[name] = value
		if entry.SecretName != "" {
			secrets = append(secrets, value)
			names[entry.SecretName] = ServiceSecretMetadata{Name: entry.SecretName, Source: source, UpdatedAt: m.now().Format(time.RFC3339Nano)}
		} else if source == "service-secret" {
			secrets = append(secrets, value)
			names[name] = ServiceSecretMetadata{Name: name, Source: source, UpdatedAt: m.now().Format(time.RFC3339Nano)}
		}
	}
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
	values = values[:0]
	for _, key := range keys {
		values = append(values, key+"="+byName[key])
	}
	return values, secrets, names, exports, nil
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

func (m *ServiceManager) recoverOrphanLocked(rt *serviceRuntime) {
	if rt.status.PID <= 0 || (rt.status.State != ServiceStateRunning && rt.status.State != ServiceStateStarting && rt.status.State != ServiceStateReady) {
		return
	}
	if rt.status.ProcessGroup > 0 && processIdentityMatches(rt.status.PID, rt.status.ProcessGroup, rt.status.ProcessStartID, rt.status.InstanceToken, rt.status.CommandDigest) {
		reapOrphanProcessGroup(rt.status.ProcessGroup)
	}
	rt.status.PID, rt.status.ProcessGroup = 0, 0
	rt.status.State = ServiceStateStopped
	rt.status.ManualStop = false
	m.persistStatusLocked(rt)
}

func (m *ServiceManager) sortedIDsLocked() []string {
	ids := make([]string, 0, len(m.configs))
	for id := range m.configs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// Stable topological order; lexical order breaks ties.
	result := make([]string, 0, len(ids))
	seen := map[string]bool{}
	var visit func(string)
	visit = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		for _, dep := range m.configs[id].DependsOn {
			visit(dep)
		}
		result = append(result, id)
	}
	for _, id := range ids {
		visit(id)
	}
	return result
}

func (m *ServiceManager) persistStatusLocked(rt *serviceRuntime) {
	rt.status.SchemaVersion = serviceSchemaVersion
	rt.status.UpdatedAt = m.now().Format(time.RFC3339Nano)
	path := filepath.Join(serviceRuntimePath(m.root, rt.status.ID), "state.json")
	if pathWithinResolved(filepath.Join(m.root, ".pua"), path) {
		_ = writeServiceJSON(path, rt.status, 0o600)
	}
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
	return m.readBindingsLocked()
}

func (m *ServiceManager) readBindingsLocked() (ServiceBindings, error) {
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
		return ServiceBindings{}, err
	}
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&bindings); err != nil {
		return ServiceBindings{}, err
	}
	return bindings, nil
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
			if err := validateEnvironmentTemplate(value, "", m.configs); err != nil {
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
			if m.runtimes[match[1]] == nil {
				return fmt.Errorf("secret mapping %q references unknown service %q", name, match[1])
			}
			continue
		}
		return fmt.Errorf("secret mapping %q must be a complete secret or service export reference", name)
	}
	return nil
}

func (m *ServiceManager) Apply(cfg ServiceConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg = defaultServiceConfig(cfg)
	next := make(map[string]ServiceConfig, len(m.configs)+1)
	for id, existing := range m.configs {
		next[id] = existing
	}
	next[cfg.ID] = cfg
	if err := validateServiceGraph(m.root, next); err != nil {
		return err
	}
	if !pathWithinResolved(filepath.Join(m.root, ".pua"), filepath.Join(m.root, ".pua", serviceConfigDir)) {
		return errors.New("service directory must remain inside the workspace control directory")
	}
	if rt := m.runtimes[cfg.ID]; rt != nil && rt.process != nil && serviceConfigDigest(rt.config) != serviceConfigDigest(cfg) {
		_ = m.stopProcessLocked(context.Background(), rt, false)
	}
	if err := writeServiceJSON(serviceConfigPath(m.root, cfg.ID), cfg, 0o600); err != nil {
		return err
	}
	m.configs[cfg.ID] = cfg
	if m.runtimes[cfg.ID] == nil {
		rt := &serviceRuntime{config: cfg, status: initialServiceStatus(cfg)}
		m.runtimes[cfg.ID] = rt
		m.persistStatusLocked(rt)
	} else {
		rt := m.runtimes[cfg.ID]
		rt.config = cfg
		rt.status.Enabled = cfg.Enabled
		rt.status.Dependencies = append([]string(nil), cfg.DependsOn...)
		rt.status.Readiness.Configured = cfg.Readiness != nil
		rt.status.Cleanup.Configured = cfg.Cleanup != nil
		if rt.process == nil {
			if cfg.Enabled {
				if rt.status.State == ServiceStateDisabled {
					rt.status.State = ServiceStateStopped
				}
			} else {
				rt.status.State = ServiceStateDisabled
			}
		}
		m.persistStatusLocked(rt)
	}
	return nil
}

func (m *ServiceManager) Remove(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.runtimes[id]
	if rt == nil {
		return os.ErrNotExist
	}
	for dependentID, cfg := range m.configs {
		if dependentID != id && containsString(cfg.DependsOn, id) {
			return fmt.Errorf("service %q is required by %q", id, dependentID)
		}
		if dependentID != id {
			for _, environment := range cfg.Environment {
				for _, match := range serviceTemplatePattern.FindAllStringSubmatch(environment.Template, -1) {
					if len(match) == 3 && match[1] == id {
						return fmt.Errorf("service %q is referenced by %q", id, dependentID)
					}
				}
			}
		}
	}
	if rt.process != nil {
		_ = m.stopProcessLocked(ctx, rt, true)
	}
	delete(m.configs, id)
	delete(m.runtimes, id)
	if err := os.Remove(serviceConfigPath(m.root, id)); err != nil && !os.IsNotExist(err) {
		return err
	}
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
	m.configs[id] = cfg
	rt := m.runtimes[id]
	rt.config = cfg
	rt.status.ManualStop = false
	rt.status.FailureCount = 0
	rt.status.AttentionRequired = false
	rt.status.LastError = ""
	rt.status.NextRetryAt = ""
	rt.status.State = ServiceStateStopped
	return writeServiceJSON(serviceConfigPath(m.root, id), cfg, 0o600)
}
func (m *ServiceManager) Disable(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, ok := m.configs[id]
	if !ok {
		return os.ErrNotExist
	}
	cfg.Enabled = false
	m.configs[id] = cfg
	rt := m.runtimes[id]
	rt.config = cfg
	var cleanupErr error
	if rt.process != nil {
		cleanupErr = m.stopProcessLocked(ctx, rt, true)
	}
	rt.status.ManualStop = false
	if cleanupErr != nil {
		rt.status.AttentionRequired = true
		rt.status.State = ServiceStateAttentionRequired
	} else {
		rt.status.State = ServiceStateDisabled
	}
	m.persistStatusLocked(rt)
	return writeServiceJSON(serviceConfigPath(m.root, id), cfg, 0o600)
}
func (m *ServiceManager) StartService(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.runtimes[id]
	if rt == nil {
		return os.ErrNotExist
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
		m.stopping = false
		m.ctx = context.Background()
	}
	return m.reconcileOneLocked(ctx, rt)
}
func (m *ServiceManager) StopService(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.runtimes[id]
	if rt == nil {
		return os.ErrNotExist
	}
	rt.status.ManualStop = true
	if rt.process != nil {
		if err := m.stopProcessLocked(ctx, rt, true); err != nil {
			return err
		}
	}
	rt.status.State = ServiceStateStopped
	m.persistStatusLocked(rt)
	return nil
}
func (m *ServiceManager) RestartService(ctx context.Context, id string) error {
	if err := m.StopService(ctx, id); err != nil {
		return err
	}
	return m.StartService(ctx, id)
}

func writeServiceJSON(path string, value any, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
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
	if err := os.Rename(tempPath, path); err != nil {
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
	if !pathWithinResolved(filepath.Join(m.root, ".pua"), serviceBindingsPath(m.root)) {
		return nil, nil, errServiceBindingsPathEscape
	}
	data, err := os.ReadFile(serviceBindingsPath(m.root))
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var bindings ServiceBindings
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bindings); err != nil {
		return nil, nil, err
	}
	if err := m.validateBindingsLocked(bindings); err != nil {
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
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return ServiceConfig{}, err
	}
	if cfg.ID == "" {
		cfg.ID = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return defaultServiceConfig(cfg), nil
}

func (m *ServiceManager) Logs(id string, follow bool) (io.ReadCloser, error) {
	return m.LogsContext(context.Background(), id, "stdout", follow)
}

// LogsStream reads one of the private redacted service log streams. The
// combined view is intentionally represented by stdout for compatibility;
// callers that need ordering between streams should request both streams.
func (m *ServiceManager) LogsStream(id, stream string, follow bool) (io.ReadCloser, error) {
	return m.LogsContext(context.Background(), id, stream, follow)
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
	return &followLogReader{file: file, ctx: ctx}, nil
}

type followLogReader struct {
	file *os.File
	ctx  context.Context
}

func (r *followLogReader) Read(p []byte) (int, error) {
	for {
		n, err := r.file.Read(p)
		if n > 0 {
			return n, nil
		}
		if err != io.EOF {
			return n, err
		}
		select {
		case <-r.ctx.Done():
			return 0, io.EOF
		case <-time.After(200 * time.Millisecond):
		}
	}
}
func (r *followLogReader) Close() error { return r.file.Close() }
