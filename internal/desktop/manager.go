//go:build darwin

package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/disksing/pua/internal/buildinfo"
	componentupdate "github.com/disksing/pua/internal/update"
	productversion "github.com/disksing/pua/internal/version"
)

const (
	desktopConfigVersion        = 1
	desktopManagerProtocol      = 1
	componentUpdateStateVersion = 1
	componentUpdateInterval     = 24 * time.Hour
)

type Config struct {
	SchemaVersion int    `json:"schemaVersion"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	AutoCheck     bool   `json:"autoCheck"`
}

type ComponentStatus struct {
	Name        string    `json:"name"`
	State       string    `json:"state"`
	Version     string    `json:"version"`
	Commit      string    `json:"commit"`
	Managed     bool      `json:"managed"`
	PID         int       `json:"pid"`
	Endpoint    string    `json:"endpoint"`
	Path        string    `json:"path"`
	Digest      string    `json:"digest"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	Error       string    `json:"error,omitempty"`
	UpdateState string    `json:"updateState,omitempty"`
}

type Status struct {
	AppVersion             string          `json:"appVersion"`
	AppBuild               string          `json:"appBuild"`
	DesktopManagerProtocol int             `json:"desktopManagerProtocol"`
	Config                 Config          `json:"config"`
	PUA                    ComponentStatus `json:"pua"`
	AgentHub               ComponentStatus `json:"agentHub"`
	ActiveTurns            int             `json:"activeTurns"`
	Exposed                bool            `json:"exposed"`
	ExposureWarning        string          `json:"exposureWarning,omitempty"`
	LastError              string          `json:"lastError,omitempty"`
	Updates                UpdateCheck     `json:"updates"`
}

type UpdateCheck struct {
	State       string               `json:"state"`
	CheckedAt   time.Time            `json:"checkedAt,omitempty"`
	GeneratedAt time.Time            `json:"generatedAt,omitempty"`
	Plan        componentupdate.Plan `json:"plan"`
	Error       string               `json:"error,omitempty"`
}

type componentUpdateState struct {
	SchemaVersion int       `json:"schemaVersion"`
	LastCheckedAt time.Time `json:"lastCheckedAt"`
}

// Manager serializes all service lifecycle transitions. The UI may poll Status
// concurrently, but start/stop/restart/config writes are never interleaved.
type Manager struct {
	mu      sync.Mutex
	options Options
	result  Result
	lastErr error
	updates UpdateCheck
}

func NewManager(options Options) (*Manager, error) {
	options = withDefaults(options)
	if err := validateOptions(options); err != nil {
		return nil, err
	}
	config, err := loadConfig(options)
	if err != nil {
		return nil, err
	}
	options.Address = net.JoinHostPort(config.Host, strconv.Itoa(config.Port))
	return &Manager{options: options}, nil
}

func (m *Manager) Ensure(ctx context.Context) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result, err := Ensure(ctx, m.options)
	m.result, m.lastErr = result, err
	return result, err
}

func (m *Manager) Start(ctx context.Context) error {
	_, err := m.Ensure(ctx)
	return err
}

func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := m.refreshResult(ctx)
	if !result.Managed && !result.AgentHubManaged {
		if result.URL != "" || result.AgentHubURL != "" {
			return errors.New("external services are read-only and cannot be stopped by PUA.app")
		}
		return nil
	}
	if err := StopManaged(ctx, m.options, result); err != nil {
		m.lastErr = err
		return err
	}
	m.result = Result{}
	m.lastErr = nil
	return nil
}

func (m *Manager) Restart(ctx context.Context) error {
	if err := m.Stop(ctx); err != nil {
		return err
	}
	return m.Start(ctx)
}

func (m *Manager) SaveConfig(config Config) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	normalized, address, err := normalizeConfig(config)
	if err != nil {
		return false, err
	}
	current, err := loadConfig(m.options)
	if err != nil {
		return false, err
	}
	restartRequired := current.Host != normalized.Host || current.Port != normalized.Port
	if err := writeJSONAtomic(configPath(m.options), normalized); err != nil {
		return false, fmt.Errorf("save desktop service configuration: %w", err)
	}
	m.options.Address = address
	return restartRequired, nil
}

func (m *Manager) Status(ctx context.Context) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := m.refreshResult(ctx)
	config, configErr := loadConfig(m.options)
	app := buildinfo.Current("macapp")
	status := Status{
		AppVersion: app.Version, AppBuild: app.SHA, DesktopManagerProtocol: desktopManagerProtocol,
		Config:  config,
		Updates: m.updates,
		PUA: ComponentStatus{Name: "PUA Server", State: "stopped", Managed: result.Managed, PID: result.PID,
			Endpoint: result.URL, Path: result.BackendPath, Digest: result.Digest, StartedAt: result.StartedAt},
		AgentHub: ComponentStatus{Name: "AgentHub", State: "stopped", Managed: result.AgentHubManaged, PID: result.AgentHubPID,
			Endpoint: result.AgentHubURL, Path: result.AgentHubPath, Digest: result.AgentHubDigest, StartedAt: result.AgentHubStartedAt},
	}
	if configErr != nil {
		status.LastError = configErr.Error()
	}
	if m.lastErr != nil {
		status.LastError = m.lastErr.Error()
	}
	if result.URL != "" {
		status.PUA.State = "running"
		if !result.Managed {
			status.PUA.State = "external"
		}
	}
	if result.AgentHubURL != "" {
		status.AgentHub.State = "running"
		if !result.AgentHubManaged {
			status.AgentHub.State = "external"
		}
	}
	puaPath, agentHubPath := m.componentPaths(result)
	if status.PUA.Path == "" {
		status.PUA.Path = puaPath
	}
	if status.AgentHub.Path == "" {
		status.AgentHub.Path = agentHubPath
	}
	status.PUA.Version, status.PUA.Commit = binaryBuildInfo(ctx, puaPath, "pua")
	status.AgentHub.Version, status.AgentHub.Commit = binaryBuildInfo(ctx, agentHubPath, "agenthub")
	if active, err := m.activeTurnsLocked(ctx, result); err == nil {
		status.ActiveTurns = active
	}
	status.Exposed = !isLoopbackHost(config.Host)
	if status.Exposed {
		status.ExposureWarning = "PUA has no authentication. This address also exposes the proxied AgentHub service to the network."
	}
	return status
}

func (m *Manager) CheckUpdates(ctx context.Context) (UpdateCheck, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	check, err := m.checkUpdatesLocked(ctx)
	m.updates = check
	if err != nil {
		m.lastErr = err
	}
	return check, err
}

func (m *Manager) checkUpdatesLocked(ctx context.Context) (UpdateCheck, error) {
	check := UpdateCheck{State: "checking", CheckedAt: time.Now().UTC()}
	publicKey, err := componentupdate.ParsePublicKey(m.options.UpdatePublicKey)
	if err != nil {
		check.State, check.Error = "unavailable", err.Error()
		return check, err
	}
	if strings.TrimSpace(m.options.UpdateManifestURL) == "" {
		err = errors.New("component update manifest URL is not configured")
		check.State, check.Error = "unavailable", err.Error()
		return check, err
	}
	manifest, err := componentupdate.Fetch(ctx, m.options.HTTPClient, m.options.UpdateManifestURL, publicKey)
	if err != nil {
		check.State, check.Error = "error", err.Error()
		return check, err
	}
	installed, err := m.installedVersions(ctx)
	if err != nil {
		check.State, check.Error = "error", err.Error()
		return check, err
	}
	plan, err := componentupdate.Resolve(manifest, installed)
	if err != nil {
		check.State, check.Error = "error", err.Error()
		return check, err
	}
	check.State = "current"
	if plan.PUA != nil || plan.AgentHub != nil || plan.AppUpdateRequired {
		check.State = "available"
	}
	check.GeneratedAt, check.Plan = manifest.GeneratedAt, plan
	_ = writeJSONAtomic(componentUpdateStatePath(m.options), componentUpdateState{
		SchemaVersion: componentUpdateStateVersion, LastCheckedAt: check.CheckedAt,
	})
	return check, nil
}

func (m *Manager) AutomaticUpdateDue(now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := readJSON[componentUpdateState](componentUpdateStatePath(m.options))
	return !ok || state.SchemaVersion != componentUpdateStateVersion || state.LastCheckedAt.IsZero() || !now.Before(state.LastCheckedAt.Add(componentUpdateInterval))
}

func (m *Manager) installedVersions(ctx context.Context) (componentupdate.Installed, error) {
	result := m.refreshResult(ctx)
	puaPath, agentHubPath := m.componentPaths(result)
	puaVersion, _ := binaryBuildInfo(ctx, puaPath, "pua")
	agentHubVersion, _ := binaryBuildInfo(ctx, agentHubPath, "agenthub")
	if puaVersion == "" || agentHubVersion == "" {
		return componentupdate.Installed{}, errors.New("could not read installed PUA and AgentHub versions")
	}
	return componentupdate.Installed{PUAVersion: puaVersion, AgentHubVersion: agentHubVersion,
		ManagerProtocol: desktopManagerProtocol, OS: "darwin", Arch: runtimeArch()}, nil
}

func (m *Manager) componentPaths(result Result) (string, string) {
	puaPath, agentHubPath := result.BackendPath, result.AgentHubPath
	if puaPath == "" {
		if current, ok := validCurrentBackend(m.options); ok {
			puaPath = current.Path
		} else if bundled, err := bundledComponentPath(backendFileName); err == nil {
			puaPath = bundled
		}
	}
	if agentHubPath == "" {
		if current, ok := validCurrentAgentHub(m.options); ok {
			agentHubPath = current.Path
		} else if bundled, err := bundledComponentPath(agentHubFileName); err == nil {
			agentHubPath = bundled
		}
	}
	return puaPath, agentHubPath
}

func (m *Manager) InstallUpdates(ctx context.Context, components []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	check, err := m.checkUpdatesLocked(ctx)
	m.updates = check
	if err != nil {
		m.lastErr = err
		return err
	}
	plan := check.Plan
	want := func(name string) bool {
		if len(components) == 0 {
			return true
		}
		for _, component := range components {
			if strings.EqualFold(strings.TrimSpace(component), name) {
				return true
			}
		}
		return false
	}
	if !want("pua") {
		plan.PUA = nil
	}
	if !want("agenthub") {
		plan.AgentHub = nil
	}
	plan.AppUpdateRequired, plan.RequiredManager = false, 0
	for _, selection := range []*componentupdate.Selection{plan.PUA, plan.AgentHub} {
		if selection != nil && selection.Release.MinDesktopManagerProtocol > desktopManagerProtocol {
			plan.AppUpdateRequired = true
			if selection.Release.MinDesktopManagerProtocol > plan.RequiredManager {
				plan.RequiredManager = selection.Release.MinDesktopManagerProtocol
			}
		}
	}
	if plan.PUA == nil {
		plan.CompatibilityError = ""
	}
	if plan.PUA == nil && plan.AgentHub == nil {
		return nil
	}
	if plan.AppUpdateRequired {
		return fmt.Errorf("PUA.app must be updated before installing components requiring manager protocol %d", plan.RequiredManager)
	}
	if plan.CompatibilityError != "" {
		return errors.New(plan.CompatibilityError)
	}
	if plan.PUA != nil && plan.AgentHub == nil {
		installed, installedErr := m.installedVersions(ctx)
		minimum, minimumErr := productversion.Parse(plan.PUA.Release.MinAgentHubVersion)
		current, currentErr := productversion.Parse(installed.AgentHubVersion)
		if installedErr != nil || minimumErr != nil || currentErr != nil {
			return errors.New("could not validate the selected PUA update against the installed AgentHub")
		}
		if productversion.Compare(current, minimum) < 0 {
			return fmt.Errorf("PUA %s requires AgentHub %s or newer; include the AgentHub update",
				plan.PUA.Release.Version, plan.PUA.Release.MinAgentHubVersion)
		}
	}
	result := m.refreshResult(ctx)
	if result.URL == "" || !result.Managed {
		return errors.New("component updates require the PUA Server managed by this app")
	}
	if plan.AgentHub != nil {
		if result.AgentHubURL == "" || !result.AgentHubManaged {
			return errors.New("AgentHub updates require the AgentHub managed by this app")
		}
		active, countErr := AgentHubActiveTurnCount(ctx, m.options, result)
		if countErr != nil {
			return fmt.Errorf("confirm AgentHub is idle before update: %w", countErr)
		}
		if active > 0 {
			return fmt.Errorf("AgentHub has %d active turn(s); wait for them to finish before updating", active)
		}
	}
	staging, err := os.MkdirTemp(filepath.Join(m.options.AppSupportDir, "updates"), ".staging-*")
	if err != nil {
		if os.IsNotExist(err) {
			if mkdirErr := os.MkdirAll(filepath.Join(m.options.AppSupportDir, "updates"), 0o700); mkdirErr == nil {
				staging, err = os.MkdirTemp(filepath.Join(m.options.AppSupportDir, "updates"), ".staging-*")
			}
		}
		if err != nil {
			return fmt.Errorf("create component update staging directory: %w", err)
		}
	}
	defer os.RemoveAll(staging)
	type stagedComponent struct {
		selection *componentupdate.Selection
		path      string
	}
	pua := stagedComponent{selection: plan.PUA, path: filepath.Join(staging, backendFileName)}
	agentHub := stagedComponent{selection: plan.AgentHub, path: filepath.Join(staging, agentHubFileName)}
	for _, component := range []stagedComponent{pua, agentHub} {
		if component.selection == nil {
			continue
		}
		if err := componentupdate.DownloadAsset(ctx, m.options.HTTPClient, component.selection.Asset, component.path); err != nil {
			return err
		}
		if err := verifyDeveloperID(component.path, component.selection.Asset); err != nil {
			return err
		}
		version, _ := binaryBuildInfo(ctx, component.path, component.selection.Release.Component)
		if version != component.selection.Release.Version {
			return fmt.Errorf("downloaded %s reports version %q, want %q", component.selection.Release.Component, version, component.selection.Release.Version)
		}
	}
	oldPUA, oldPUAOK := readJSON[manifest](manifestPath(m.options))
	oldAgentHub, oldAgentHubOK := readJSON[manifest](agentHubManifestPath(m.options))
	rollback := func() {
		if oldPUAOK {
			_ = writeJSONAtomic(manifestPath(m.options), oldPUA)
		}
		if oldAgentHubOK {
			_ = writeJSONAtomic(agentHubManifestPath(m.options), oldAgentHub)
		}
		_, _ = Ensure(context.Background(), m.options)
	}
	if err := stopManagedBackend(ctx, m.options, result); err != nil {
		return err
	}
	if plan.AgentHub != nil {
		if err := stopManagedAgentHub(ctx, m.options, result); err != nil {
			rollback()
			return err
		}
	}
	if plan.AgentHub != nil {
		if _, _, err := installAgentHub(m.options, agentHub.path); err != nil {
			rollback()
			return err
		}
	}
	if plan.PUA != nil {
		if _, _, err := installBackend(m.options, pua.path); err != nil {
			rollback()
			return err
		}
	}
	started, err := Ensure(ctx, m.options)
	if err != nil {
		rollback()
		return fmt.Errorf("start updated components: %w", err)
	}
	m.result, m.lastErr = started, nil
	m.updates.State = "installed"
	return nil
}

func (m *Manager) ActiveTurns(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeTurnsLocked(ctx, m.refreshResult(ctx))
}

func (m *Manager) activeTurnsLocked(ctx context.Context, result Result) (int, error) {
	if result.AgentHubURL != "" {
		return AgentHubActiveTurnCount(ctx, m.options, result)
	}
	if result.URL != "" {
		return ActiveTurnCount(ctx, m.options, result)
	}
	return 0, nil
}

func runtimeArch() string {
	return runtime.GOARCH
}

func verifyDeveloperID(path string, asset componentupdate.Asset) error {
	verify := exec.Command("codesign", "--verify", "--strict", "--verbose=2", path)
	if output, err := verify.CombinedOutput(); err != nil {
		return fmt.Errorf("verify downloaded component code signature: %w: %s", err, strings.TrimSpace(string(output)))
	}
	inspect := exec.Command("codesign", "-d", "--verbose=4", path)
	output, err := inspect.CombinedOutput()
	if err != nil {
		return fmt.Errorf("inspect downloaded component code signature: %w", err)
	}
	if asset.SigningTeamID != "" || asset.SigningIdentifier != "" {
		teamID := codesignMetadataValue(string(output), "TeamIdentifier")
		identifier := codesignMetadataValue(string(output), "Identifier")
		if teamID != asset.SigningTeamID || identifier != asset.SigningIdentifier {
			return fmt.Errorf("downloaded component signing metadata is %q/%q, want %q/%q",
				teamID, identifier, asset.SigningTeamID, asset.SigningIdentifier)
		}
		return nil
	}
	if !strings.Contains(string(output), "Authority="+asset.CodeIdentity) {
		return fmt.Errorf("downloaded component is not signed by %q", asset.CodeIdentity)
	}
	return nil
}

func codesignMetadataValue(output, key string) string {
	prefix := key + "="
	for _, line := range strings.Split(output, "\n") {
		if value, found := strings.CutPrefix(strings.TrimSpace(line), prefix); found {
			return value
		}
	}
	return ""
}

func (m *Manager) Options() Options {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.options
}

func (m *Manager) Result() Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.result
}

func (m *Manager) refreshResult(ctx context.Context) Result {
	pua, puaOK := discoverExisting(ctx, m.options)
	agentHub, agentHubOK := discoverAgentHub(ctx, m.options)
	if !puaOK && !agentHubOK {
		m.result = Result{}
		return m.result
	}
	if !puaOK {
		pua = Result{}
	}
	if agentHubOK {
		pua = combineResults(pua, agentHub)
	}
	m.result = pua
	return m.result
}

func loadConfig(options Options) (Config, error) {
	config := Config{SchemaVersion: desktopConfigVersion, AutoCheck: true}
	host, portText, err := net.SplitHostPort(options.Address)
	if err != nil {
		return Config{}, fmt.Errorf("read desktop address: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return Config{}, fmt.Errorf("read desktop port: %w", err)
	}
	config.Host, config.Port = host, port
	stored, ok := readJSON[Config](configPath(options))
	if !ok {
		return config, nil
	}
	if stored.SchemaVersion != desktopConfigVersion {
		return Config{}, fmt.Errorf("unsupported desktop config schema %d", stored.SchemaVersion)
	}
	normalized, _, err := normalizeConfig(stored)
	return normalized, err
}

func normalizeConfig(config Config) (Config, string, error) {
	host := strings.Trim(strings.TrimSpace(config.Host), "[]")
	if host == "" || strings.ContainsAny(host, " \t\r\n/?#[]@") || strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return Config{}, "", errors.New("host must be a hostname or IP address without a URL scheme")
	}
	if config.Port < 1 || config.Port > 65535 {
		return Config{}, "", errors.New("port must be between 1 and 65535")
	}
	address := net.JoinHostPort(host, strconv.Itoa(config.Port))
	if _, err := endpointForAddress(address); err != nil {
		return Config{}, "", err
	}
	config.SchemaVersion = desktopConfigVersion
	config.Host = host
	return config, address, nil
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func binaryBuildInfo(ctx context.Context, path, component string) (string, string) {
	if strings.TrimSpace(path) == "" {
		return "", ""
	}
	commandCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	command := exec.CommandContext(commandCtx, path, "--version-json")
	output, err := command.Output()
	if err != nil {
		return "", ""
	}
	var info buildinfo.Info
	if err := json.Unmarshal(output, &info); err != nil || info.Component != component {
		return "", ""
	}
	return info.Version, info.SHA
}

func configPath(options Options) string {
	return filepath.Join(options.AppSupportDir, "desktop-config.json")
}

func componentUpdateStatePath(options Options) string {
	return filepath.Join(options.AppSupportDir, "component-update-state.json")
}

func OpenFullDiskAccessSettings() error {
	urls := []string{
		"x-apple.systempreferences:com.apple.settings.PrivacySecurity.extension?Privacy_AllFiles",
		"x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles",
	}
	var lastErr error
	for _, target := range urls {
		if err := exec.Command("open", target).Run(); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return fmt.Errorf("open Full Disk Access settings: %w", lastErr)
}
