//go:build darwin

package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/disksing/pua/internal/buildinfo"
)

const (
	defaultAddress         = "127.0.0.1:4936"
	defaultAgentHubAddress = "127.0.0.1:4646"
	manifestVersion        = 1
	stateVersion           = 2
	backendFileName        = "pua"
	agentHubFileName       = "agenthub"
	defaultProbePath       = "/api/workspaces"
	defaultStartDelay      = 100 * time.Millisecond
	pathBlockStart         = "# >>> PUA desktop managed PATH >>>"
	pathBlockEnd           = "# <<< PUA desktop managed PATH <<<"
)

// macOS applications launched from Finder or the Dock do not source the
// user's shell profile. Keep the common Homebrew and Intel Homebrew paths in
// the managed backend's environment so provider CLIs installed there can be
// found by AgentHub.
var macOSProviderPathEntries = []string{
	"/opt/homebrew/bin",
	"/opt/homebrew/sbin",
	"/usr/local/bin",
	"/usr/local/sbin",
}

// Options controls how the desktop shell finds and starts the PUA backend.
type Options struct {
	Address           string
	AgentHubAddress   string
	ConfigPath        string
	AppSupportDir     string
	BackendPath       string
	AgentHubPath      string
	AgentHubHome      string
	AgentHubStatePath string
	AgentHubLockPath  string
	CLIPath           string
	AgentHubCLIPath   string
	ShellProfile      string
	UpdateManifestURL string
	UpdatePublicKey   string
	StartupTimeout    time.Duration
	HTTPClient        *http.Client
}

// Result describes the backend selected by Ensure.
type Result struct {
	URL               string    `json:"url"`
	Managed           bool      `json:"managed"`
	PID               int       `json:"pid"`
	BackendPath       string    `json:"backendPath"`
	Digest            string    `json:"digest"`
	StartedAt         time.Time `json:"startedAt,omitempty"`
	AgentHubURL       string    `json:"agentHubUrl"`
	AgentHubManaged   bool      `json:"agentHubManaged"`
	AgentHubPID       int       `json:"agentHubPid"`
	AgentHubPath      string    `json:"agentHubPath"`
	AgentHubDigest    string    `json:"agentHubDigest"`
	AgentHubStartedAt time.Time `json:"agentHubStartedAt,omitempty"`
}

type manifest struct {
	SchemaVersion int       `json:"schemaVersion"`
	Digest        string    `json:"digest"`
	Path          string    `json:"path"`
	InstalledAt   time.Time `json:"installedAt"`
}

type backendState struct {
	SchemaVersion    int       `json:"schemaVersion"`
	PID              int       `json:"pid"`
	Endpoint         string    `json:"endpoint"`
	BackendPath      string    `json:"backendPath"`
	Digest           string    `json:"digest"`
	Managed          bool      `json:"managed"`
	StartedAt        time.Time `json:"startedAt"`
	AgentHubEndpoint string    `json:"agentHubEndpoint"`
}

type agentHubState struct {
	SchemaVersion int       `json:"schemaVersion"`
	PID           int       `json:"pid"`
	Endpoint      string    `json:"endpoint"`
	BinaryPath    string    `json:"binaryPath"`
	Digest        string    `json:"digest"`
	Managed       bool      `json:"managed"`
	StartedAt     time.Time `json:"startedAt"`
}

type agentHubDaemonState struct {
	PID       int       `json:"pid"`
	Endpoint  string    `json:"endpoint"`
	StartedAt time.Time `json:"startedAt"`
}

type serveLock struct {
	PID     int    `json:"pid"`
	Address string `json:"address"`
}

// DefaultOptions returns the production paths used by PUA.app.
func DefaultOptions() (Options, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Options{}, fmt.Errorf("find home directory: %w", err)
	}
	development := buildinfo.IsDevelopment(buildinfo.Current("macapp"))
	address, agentHubAddress := defaultAddress, defaultAgentHubAddress
	applicationName, configDirectory := "PUA", ".pua"
	if development {
		address, agentHubAddress = "127.0.0.1:14936", "127.0.0.1:14646"
		applicationName, configDirectory = "PUA Dev", ".pua-dev"
	}
	configPath := os.Getenv("PUA_SERVE_CONFIG")
	if configPath == "" {
		configPath = filepath.Join(home, configDirectory, "serve.json")
	}
	appSupport := os.Getenv("PUA_DESKTOP_HOME")
	if appSupport == "" {
		appSupport = filepath.Join(home, "Library", "Application Support", applicationName)
	}
	agentHubHome := os.Getenv("PUA_DESKTOP_AGENTHUB_HOME")
	if agentHubHome == "" && !development {
		agentHubHome = os.Getenv("AGENTHUB_HOME")
	}
	if agentHubHome == "" && development {
		agentHubHome = filepath.Join(appSupport, "agenthub-home")
	}
	agentHubStatePath := filepath.Join(home, ".agenthub", "server.json")
	agentHubLockPath := filepath.Join(home, ".agenthub", "server.lock")
	if agentHubHome != "" {
		agentHubStatePath = filepath.Join(agentHubHome, "state", "server.json")
		agentHubLockPath = filepath.Join(agentHubHome, "state", "server.lock")
	}
	cliPath := filepath.Join(home, ".pua", "bin", backendFileName)
	shellProfile := defaultShellProfile(home)
	if development {
		cliPath = filepath.Join(appSupport, "bin", backendFileName)
		shellProfile = ""
	}
	return Options{
		Address:           envOrDefault("PUA_DESKTOP_ADDRESS", address),
		AgentHubAddress:   envOrDefault("PUA_DESKTOP_AGENTHUB_ADDRESS", agentHubAddress),
		ConfigPath:        configPath,
		AppSupportDir:     appSupport,
		BackendPath:       os.Getenv("PUA_DESKTOP_BACKEND"),
		AgentHubPath:      os.Getenv("PUA_DESKTOP_AGENTHUB"),
		AgentHubHome:      agentHubHome,
		AgentHubStatePath: agentHubStatePath,
		AgentHubLockPath:  agentHubLockPath,
		CLIPath:           envOrDefault("PUA_DESKTOP_CLI_PATH", cliPath),
		AgentHubCLIPath:   os.Getenv("PUA_DESKTOP_AGENTHUB_CLI_PATH"),
		ShellProfile:      envOrDefault("PUA_DESKTOP_SHELL_PROFILE", shellProfile),
		UpdateManifestURL: envOrDefault("PUA_COMPONENT_MANIFEST_URL", buildinfo.ComponentManifestURL),
		UpdatePublicKey:   envOrDefault("PUA_COMPONENT_UPDATE_PUBLIC_KEY", buildinfo.ComponentUpdatePublicKey),
		StartupTimeout:    30 * time.Second,
		HTTPClient:        &http.Client{Timeout: 2 * time.Second},
	}, nil
}

// Ensure reconnects to a healthy PUA server or starts a managed backend.
func Ensure(ctx context.Context, options Options) (Result, error) {
	options = withDefaults(options)
	if err := validateOptions(options); err != nil {
		return Result{}, err
	}

	if result, ok := discoverExisting(ctx, options); ok {
		if !result.Managed {
			if err := installBundledCLI(options); err != nil {
				return Result{}, err
			}
			return result, nil
		}
		backendAgentHubEndpoint := result.AgentHubURL
		agentHub, err := ensureAgentHub(ctx, options)
		if err != nil {
			return Result{}, err
		}
		backendPath, digest, err := selectBackend(options)
		if err != nil {
			return Result{}, err
		}
		if err := installCLI(options, backendPath); err != nil {
			return Result{}, err
		}
		result = combineResults(result, agentHub)
		if result.Digest == digest && backendAgentHubEndpoint == agentHub.URL {
			return result, nil
		}
		if err := stopManagedBackend(ctx, options, result); err != nil {
			return Result{}, err
		}
		started, err := startBackend(ctx, options, backendPath, digest, agentHub.URL)
		return combineResults(started, agentHub), err
	}

	agentHub, err := ensureAgentHub(ctx, options)
	if err != nil {
		return Result{}, err
	}
	backendPath, digest, err := selectBackend(options)
	if err != nil {
		return Result{}, err
	}
	if err := installCLI(options, backendPath); err != nil {
		return Result{}, err
	}
	started, err := startBackend(ctx, options, backendPath, digest, agentHub.URL)
	return combineResults(started, agentHub), err
}

func combineResults(pua Result, agentHub Result) Result {
	pua.AgentHubURL = agentHub.URL
	pua.AgentHubManaged = agentHub.Managed
	pua.AgentHubPID = agentHub.PID
	pua.AgentHubPath = agentHub.BackendPath
	pua.AgentHubDigest = agentHub.Digest
	pua.AgentHubStartedAt = agentHub.StartedAt
	return pua
}

func withDefaults(options Options) Options {
	if options.Address == "" {
		options.Address = defaultAddress
	}
	if options.AgentHubAddress == "" {
		options.AgentHubAddress = defaultAgentHubAddress
	}
	if options.StartupTimeout <= 0 {
		options.StartupTimeout = 30 * time.Second
	}
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{Timeout: 2 * time.Second}
	}
	if options.AgentHubHome == "" {
		options.AgentHubHome = os.Getenv("AGENTHUB_HOME")
	}
	if options.AgentHubStatePath == "" || options.AgentHubLockPath == "" {
		if options.AgentHubHome != "" {
			options.AgentHubStatePath = filepath.Join(options.AgentHubHome, "state", "server.json")
			options.AgentHubLockPath = filepath.Join(options.AgentHubHome, "state", "server.lock")
		} else if home, err := os.UserHomeDir(); err == nil {
			options.AgentHubStatePath = filepath.Join(home, ".agenthub", "server.json")
			options.AgentHubLockPath = filepath.Join(home, ".agenthub", "server.lock")
		}
	}
	return options
}

func validateOptions(options Options) error {
	if options.ConfigPath == "" {
		return errors.New("PUA serve config path is empty")
	}
	if options.AppSupportDir == "" {
		return errors.New("desktop application support path is empty")
	}
	if _, err := endpointForAddress(options.Address); err != nil {
		return fmt.Errorf("invalid desktop backend address: %w", err)
	}
	if _, err := endpointForAddress(options.AgentHubAddress); err != nil {
		return fmt.Errorf("invalid AgentHub address: %w", err)
	}
	return nil
}

func discoverExisting(ctx context.Context, options Options) (Result, bool) {
	state, stateOK := readJSON[backendState](statePath(options))
	lock, lockOK := readJSON[serveLock](options.ConfigPath + ".lock")
	lockEndpoint, lockEndpointErr := endpointForAddress(lock.Address)
	if stateOK && state.SchemaVersion == stateVersion && state.Managed && lockOK && lockEndpointErr == nil && state.PID > 0 &&
		state.PID == lock.PID && state.Endpoint == lockEndpoint && processAlive(state.PID) &&
		healthy(ctx, options.HTTPClient, state.Endpoint) {
		return Result{
			URL:         state.Endpoint,
			Managed:     true,
			PID:         state.PID,
			BackendPath: state.BackendPath,
			Digest:      state.Digest,
			StartedAt:   state.StartedAt,
			AgentHubURL: state.AgentHubEndpoint,
		}, true
	}

	if lockOK {
		if lockEndpointErr == nil && processAlive(lock.PID) && healthy(ctx, options.HTTPClient, lockEndpoint) {
			managed := stateOK && state.SchemaVersion == stateVersion && state.Managed && state.PID == lock.PID && state.Endpoint == lockEndpoint
			return Result{URL: lockEndpoint, Managed: managed, PID: lock.PID}, true
		}
	}

	endpoint, err := endpointForAddress(options.Address)
	if err == nil && healthy(ctx, options.HTTPClient, endpoint) {
		return Result{URL: endpoint}, true
	}
	return Result{}, false
}

func selectBackend(options Options) (string, string, error) {
	if options.BackendPath != "" {
		return installBackend(options, options.BackendPath)
	}
	if current, ok := validCurrentBackend(options); ok {
		return current.Path, current.Digest, nil
	}

	source, err := bundledBackendPath()
	if err == nil {
		return installBackend(options, source)
	}
	return "", "", err
}

func selectBundledBackend(options Options, source string) (string, string, error) {
	digest, err := fileDigest(source)
	if err != nil {
		return "", "", fmt.Errorf("hash bundled PUA backend: %w", err)
	}
	if current, ok := validCurrentBackend(options); ok && current.Digest == digest {
		return current.Path, current.Digest, nil
	}
	return installBackend(options, source)
}

func validCurrentBackend(options Options) (manifest, bool) {
	current, ok := readJSON[manifest](manifestPath(options))
	if !ok || current.SchemaVersion != manifestVersion {
		return manifest{}, false
	}
	digest, err := fileDigest(current.Path)
	return current, err == nil && digest == current.Digest
}

func bundledBackendPath() (string, error) {
	return bundledComponentPath(backendFileName)
}

func installBackend(options Options, source string) (string, string, error) {
	digest, err := fileDigest(source)
	if err != nil {
		return "", "", fmt.Errorf("hash PUA backend: %w", err)
	}
	versionDir := filepath.Join(options.AppSupportDir, "backend", "versions", digest)
	destination := filepath.Join(versionDir, backendFileName)
	if err := os.MkdirAll(versionDir, 0o700); err != nil {
		return "", "", fmt.Errorf("create backend version directory: %w", err)
	}
	if installedDigest, digestErr := fileDigest(destination); digestErr != nil || installedDigest != digest {
		if err := copyExecutable(source, destination); err != nil {
			return "", "", err
		}
	}
	if err := writeJSONAtomic(manifestPath(options), manifest{
		SchemaVersion: manifestVersion,
		Digest:        digest,
		Path:          destination,
		InstalledAt:   time.Now().UTC(),
	}); err != nil {
		return "", "", fmt.Errorf("write backend manifest: %w", err)
	}
	return destination, digest, nil
}

func installBundledCLI(options Options) error {
	if options.CLIPath == "" {
		return nil
	}
	backendPath, _, err := selectBackend(options)
	if err != nil {
		return err
	}
	if err := installCLI(options, backendPath); err != nil {
		return err
	}
	if options.AgentHubCLIPath == "" {
		return nil
	}
	agentHubPath, _, err := selectAgentHub(options)
	if err != nil {
		return err
	}
	return installNamedCLI(options.AgentHubCLIPath, agentHubPath)
}

func installCLI(options Options, source string) error {
	if options.CLIPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(options.CLIPath), 0o700); err != nil {
		return fmt.Errorf("create PUA CLI directory: %w", err)
	}
	sourceDigest, err := fileDigest(source)
	if err != nil {
		return fmt.Errorf("hash PUA CLI source: %w", err)
	}
	if installedDigest, digestErr := fileDigest(options.CLIPath); digestErr != nil || installedDigest != sourceDigest {
		if err := copyExecutable(source, options.CLIPath); err != nil {
			return fmt.Errorf("install PUA CLI: %w", err)
		}
	}
	if options.ShellProfile != "" {
		if err := ensureShellPath(options.ShellProfile, filepath.Dir(options.CLIPath)); err != nil {
			return err
		}
	}
	return nil
}

func installNamedCLI(destination, source string) error {
	if destination == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create CLI directory: %w", err)
	}
	sourceDigest, err := fileDigest(source)
	if err != nil {
		return fmt.Errorf("hash CLI source: %w", err)
	}
	if installedDigest, digestErr := fileDigest(destination); digestErr != nil || installedDigest != sourceDigest {
		if err := copyExecutable(source, destination); err != nil {
			return fmt.Errorf("install CLI: %w", err)
		}
	}
	return nil
}

func ensureAgentHub(ctx context.Context, options Options) (Result, error) {
	if result, ok := discoverAgentHub(ctx, options); ok {
		return result, nil
	}
	path, digest, err := selectAgentHub(options)
	if err != nil {
		return Result{}, err
	}
	if err := installNamedCLI(options.AgentHubCLIPath, path); err != nil {
		return Result{}, err
	}
	return startAgentHub(ctx, options, path, digest)
}

func selectAgentHub(options Options) (string, string, error) {
	if options.AgentHubPath != "" {
		return installAgentHub(options, options.AgentHubPath)
	}
	if current, ok := validCurrentAgentHub(options); ok {
		return current.Path, current.Digest, nil
	}
	source, err := bundledComponentPath(agentHubFileName)
	if err != nil {
		return "", "", err
	}
	return installAgentHub(options, source)
}

func installAgentHub(options Options, source string) (string, string, error) {
	return installVersionedComponent(filepath.Join(options.AppSupportDir, "agenthub"), source, agentHubFileName)
}

func validCurrentAgentHub(options Options) (manifest, bool) {
	current, ok := readJSON[manifest](agentHubManifestPath(options))
	if !ok || current.SchemaVersion != manifestVersion {
		return manifest{}, false
	}
	digest, err := fileDigest(current.Path)
	return current, err == nil && digest == current.Digest
}

func installVersionedComponent(root, source, fileName string) (string, string, error) {
	digest, err := fileDigest(source)
	if err != nil {
		return "", "", fmt.Errorf("hash %s: %w", fileName, err)
	}
	versionDir := filepath.Join(root, "versions", digest)
	destination := filepath.Join(versionDir, fileName)
	if err := os.MkdirAll(versionDir, 0o700); err != nil {
		return "", "", fmt.Errorf("create %s version directory: %w", fileName, err)
	}
	if installedDigest, digestErr := fileDigest(destination); digestErr != nil || installedDigest != digest {
		if err := copyExecutable(source, destination); err != nil {
			return "", "", err
		}
	}
	if err := writeJSONAtomic(filepath.Join(root, "current.json"), manifest{
		SchemaVersion: manifestVersion,
		Digest:        digest,
		Path:          destination,
		InstalledAt:   time.Now().UTC(),
	}); err != nil {
		return "", "", fmt.Errorf("write %s manifest: %w", fileName, err)
	}
	return destination, digest, nil
}

func bundledComponentPath(fileName string) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find desktop executable: %w", err)
	}
	candidates := []string{
		filepath.Join(filepath.Dir(executable), "..", "Resources", fileName),
		filepath.Join(filepath.Dir(executable), fileName),
	}
	for _, candidate := range candidates {
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return filepath.Clean(candidate), nil
		}
	}
	if path, lookErr := exec.LookPath(fileName); lookErr == nil {
		return path, nil
	}
	return "", fmt.Errorf("%s not found; reinstall PUA.app or configure its path", fileName)
}

func discoverAgentHub(ctx context.Context, options Options) (Result, bool) {
	endpoint, err := agentHubEndpointForAddress(options.AgentHubAddress)
	if err != nil || !agentHubHealthy(ctx, options.HTTPClient, endpoint) {
		return Result{}, false
	}
	desktopState, desktopOK := readJSON[agentHubState](agentHubDesktopStatePath(options))
	daemonState, daemonOK := readJSON[agentHubDaemonState](options.AgentHubStatePath)
	lockPID, lockOK := readPID(options.AgentHubLockPath)
	managed := desktopOK && desktopState.SchemaVersion == stateVersion && desktopState.Managed &&
		daemonOK && lockOK && desktopState.PID > 0 && desktopState.PID == daemonState.PID && desktopState.PID == lockPID &&
		desktopState.Endpoint == endpoint && daemonState.Endpoint == endpoint && processAlive(desktopState.PID)
	if managed {
		return Result{URL: endpoint, Managed: true, PID: desktopState.PID, BackendPath: desktopState.BinaryPath, Digest: desktopState.Digest, StartedAt: desktopState.StartedAt}, true
	}
	return Result{URL: endpoint, PID: daemonState.PID, StartedAt: daemonState.StartedAt}, true
}

func startAgentHub(ctx context.Context, options Options, binaryPath, digest string) (Result, error) {
	logDir := filepath.Join(options.AppSupportDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("create desktop log directory: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, "agenthub.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return Result{}, fmt.Errorf("open AgentHub log: %w", err)
	}
	command := exec.Command(binaryPath, "serve", "--addr="+options.AgentHubAddress)
	command.Env = backendEnvironment(os.Environ(), options.CLIPath)
	if options.AgentHubHome != "" {
		command.Env = replaceEnv(command.Env, "AGENTHUB_HOME", options.AgentHubHome)
	}
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return Result{}, fmt.Errorf("start AgentHub: %w", err)
	}
	processDone := make(chan error, 1)
	go func() {
		processDone <- command.Wait()
		_ = logFile.Close()
	}()
	endpoint, _ := agentHubEndpointForAddress(options.AgentHubAddress)
	timer := time.NewTimer(options.StartupTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(defaultStartDelay)
	defer ticker.Stop()
	for {
		if agentHubHealthy(ctx, options.HTTPClient, endpoint) {
			daemon, stateOK := readJSON[agentHubDaemonState](options.AgentHubStatePath)
			lockPID, lockOK := readPID(options.AgentHubLockPath)
			if stateOK && lockOK && daemon.PID != command.Process.Pid && daemon.PID == lockPID && daemon.Endpoint == endpoint && processAlive(daemon.PID) {
				_ = command.Process.Signal(os.Interrupt)
				return Result{URL: endpoint, PID: daemon.PID, StartedAt: daemon.StartedAt}, nil
			}
			if stateOK && lockOK && daemon.PID == command.Process.Pid && daemon.PID == lockPID && daemon.Endpoint == endpoint {
				state := agentHubState{SchemaVersion: stateVersion, PID: daemon.PID, Endpoint: endpoint, BinaryPath: binaryPath,
					Digest: digest, Managed: true, StartedAt: time.Now().UTC()}
				if err := writeJSONAtomic(agentHubDesktopStatePath(options), state); err != nil {
					_ = command.Process.Signal(os.Interrupt)
					return Result{}, fmt.Errorf("write AgentHub desktop state: %w", err)
				}
				return Result{URL: endpoint, Managed: true, PID: daemon.PID, BackendPath: binaryPath, Digest: digest, StartedAt: state.StartedAt}, nil
			}
		}
		select {
		case err := <-processDone:
			return Result{}, fmt.Errorf("AgentHub exited before becoming ready: %w", err)
		case <-ctx.Done():
			_ = command.Process.Signal(os.Interrupt)
			return Result{}, ctx.Err()
		case <-timer.C:
			_ = command.Process.Signal(os.Interrupt)
			return Result{}, fmt.Errorf("AgentHub did not become ready within %s", options.StartupTimeout)
		case <-ticker.C:
		}
	}
}

func startBackend(ctx context.Context, options Options, backendPath, digest, agentHubEndpoint string) (Result, error) {
	if err := os.MkdirAll(filepath.Dir(options.ConfigPath), 0o700); err != nil {
		return Result{}, fmt.Errorf("create PUA config directory: %w", err)
	}
	logDir := filepath.Join(options.AppSupportDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("create desktop log directory: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, "backend.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return Result{}, fmt.Errorf("open backend log: %w", err)
	}

	command := exec.Command(backendPath, "serve", "--addr="+options.Address, "--no-default-workspace",
		"--agenthub-mode=external", "--agenthub-endpoint="+agentHubEndpoint)
	command.Env = backendEnvironment(os.Environ(), options.CLIPath)
	command.Env = replaceEnv(command.Env, "PUA_SERVE_CONFIG", options.ConfigPath)
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return Result{}, fmt.Errorf("start PUA backend: %w", err)
	}

	processDone := make(chan error, 1)
	go func() {
		processDone <- command.Wait()
		_ = logFile.Close()
	}()

	endpoint, _ := endpointForAddress(options.Address)
	timer := time.NewTimer(options.StartupTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(defaultStartDelay)
	defer ticker.Stop()
	for {
		if healthy(ctx, options.HTTPClient, endpoint) {
			lock, ok := readJSON[serveLock](options.ConfigPath + ".lock")
			lockEndpoint, lockErr := endpointForAddress(lock.Address)
			if ok && lockErr == nil && lock.PID != command.Process.Pid && lockEndpoint == endpoint && processAlive(lock.PID) {
				_ = command.Process.Signal(os.Interrupt)
				return Result{URL: endpoint, PID: lock.PID}, nil
			}
			if ok && lockErr == nil && lock.PID == command.Process.Pid && lockEndpoint == endpoint {
				state := backendState{
					SchemaVersion:    stateVersion,
					PID:              command.Process.Pid,
					Endpoint:         endpoint,
					BackendPath:      backendPath,
					Digest:           digest,
					Managed:          true,
					StartedAt:        time.Now().UTC(),
					AgentHubEndpoint: agentHubEndpoint,
				}
				if err := writeJSONAtomic(statePath(options), state); err != nil {
					_ = command.Process.Signal(os.Interrupt)
					return Result{}, fmt.Errorf("write backend state: %w", err)
				}
				return Result{URL: endpoint, Managed: true, PID: state.PID, BackendPath: backendPath, Digest: digest, StartedAt: state.StartedAt}, nil
			}
		}
		select {
		case err := <-processDone:
			return Result{}, fmt.Errorf("PUA backend exited before becoming ready: %w", err)
		case <-ctx.Done():
			_ = command.Process.Signal(os.Interrupt)
			return Result{}, ctx.Err()
		case <-timer.C:
			_ = command.Process.Signal(os.Interrupt)
			return Result{}, fmt.Errorf("PUA backend did not become ready within %s", options.StartupTimeout)
		case <-ticker.C:
		}
	}
}

func stopManagedBackend(ctx context.Context, options Options, result Result) error {
	state, stateOK := readJSON[backendState](statePath(options))
	lock, lockOK := readJSON[serveLock](options.ConfigPath + ".lock")
	lockEndpoint, lockEndpointErr := endpointForAddress(lock.Address)
	if !stateOK || state.SchemaVersion != stateVersion || !state.Managed || !lockOK || lockEndpointErr != nil ||
		state.PID <= 0 || state.PID != result.PID || state.PID != lock.PID || state.Endpoint != result.URL || state.Endpoint != lockEndpoint {
		return errors.New("refusing to replace PUA backend because managed process ownership changed")
	}
	if err := syscall.Kill(state.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stop managed PUA backend: %w", err)
	}

	ticker := time.NewTicker(defaultStartDelay)
	defer ticker.Stop()
	for processAlive(state.PID) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for managed PUA backend to stop: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	return nil
}

// StopManaged gracefully stops a backend only when it is still owned by this
// desktop installation. External PUA processes are never stopped.
func StopManaged(ctx context.Context, options Options, result Result) error {
	options = withDefaults(options)
	if result.Managed {
		if err := stopManagedBackend(ctx, options, result); err != nil {
			return err
		}
	}
	if result.AgentHubManaged {
		return stopManagedAgentHub(ctx, options, result)
	}
	return nil
}

func stopManagedAgentHub(ctx context.Context, options Options, result Result) error {
	state, stateOK := readJSON[agentHubState](agentHubDesktopStatePath(options))
	daemon, daemonOK := readJSON[agentHubDaemonState](options.AgentHubStatePath)
	lockPID, lockOK := readPID(options.AgentHubLockPath)
	if !stateOK || state.SchemaVersion != stateVersion || !state.Managed || !daemonOK || !lockOK ||
		state.PID <= 0 || state.PID != result.AgentHubPID || state.PID != daemon.PID || state.PID != lockPID ||
		state.Endpoint != result.AgentHubURL || state.Endpoint != daemon.Endpoint {
		return errors.New("refusing to stop AgentHub because managed process ownership changed")
	}
	if err := syscall.Kill(state.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stop managed AgentHub: %w", err)
	}
	ticker := time.NewTicker(defaultStartDelay)
	defer ticker.Stop()
	for processAlive(state.PID) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for managed AgentHub to stop: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	return nil
}

// ActiveTurnCount reports resources with an active turn across all configured
// Workspaces. It uses the same public API that drives the desktop activity UI.
func ActiveTurnCount(ctx context.Context, options Options, result Result) (int, error) {
	options = withDefaults(options)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(result.URL, "/")+defaultProbePath, nil)
	if err != nil {
		return 0, err
	}
	response, err := options.HTTPClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("read PUA workspaces: HTTP %d", response.StatusCode)
	}
	var workspaces struct {
		Workspaces []struct {
			ID string `json:"id"`
		} `json:"workspaces"`
	}
	if err := json.NewDecoder(response.Body).Decode(&workspaces); err != nil {
		return 0, fmt.Errorf("decode PUA workspaces: %w", err)
	}

	count := 0
	for _, workspace := range workspaces.Workspaces {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet,
			strings.TrimRight(result.URL, "/")+"/api/workspaces/"+url.PathEscape(workspace.ID)+"/tree", nil)
		if err != nil {
			return 0, err
		}
		response, err := options.HTTPClient.Do(request)
		if err != nil {
			return 0, err
		}
		var tree struct {
			Activity struct {
				Running []json.RawMessage `json:"running"`
			} `json:"activity"`
		}
		decodeErr := json.NewDecoder(response.Body).Decode(&tree)
		closeErr := response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("read Workspace %q activity: HTTP %d", workspace.ID, response.StatusCode)
		}
		if decodeErr != nil {
			return 0, fmt.Errorf("decode Workspace %q activity: %w", workspace.ID, decodeErr)
		}
		if closeErr != nil {
			return 0, fmt.Errorf("close Workspace %q activity response: %w", workspace.ID, closeErr)
		}
		count += len(tree.Activity.Running)
	}
	return count, nil
}

// AgentHubActiveTurnCount reports every active Turn in the shared AgentHub,
// including Sessions created directly from the AgentHub window rather than
// through a PUA Workspace.
func AgentHubActiveTurnCount(ctx context.Context, options Options, result Result) (int, error) {
	options = withDefaults(options)
	if strings.TrimSpace(result.AgentHubURL) == "" {
		return 0, errors.New("AgentHub endpoint is unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(result.AgentHubURL, "/")+"/v1/sessions", nil)
	if err != nil {
		return 0, err
	}
	response, err := options.HTTPClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("read AgentHub Sessions: HTTP %d", response.StatusCode)
	}
	var payload struct {
		Sessions []struct {
			CurrentTurnID string `json:"currentTurnId"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return 0, fmt.Errorf("decode AgentHub Sessions: %w", err)
	}
	count := 0
	for _, session := range payload.Sessions {
		if strings.TrimSpace(session.CurrentTurnID) != "" {
			count++
		}
	}
	return count, nil
}

func healthy(ctx context.Context, client *http.Client, endpoint string) bool {
	return healthyPath(ctx, client, endpoint, defaultProbePath)
}

func agentHubHealthy(ctx context.Context, client *http.Client, endpoint string) bool {
	return healthyPath(ctx, client, endpoint, "/v1/status")
}

func healthyPath(ctx context.Context, client *http.Client, endpoint, path string) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+path, nil)
	if err != nil {
		return false
	}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return response.StatusCode == http.StatusOK
}

func agentHubEndpointForAddress(address string) (string, error) {
	endpoint, err := endpointForAddress(address)
	if err != nil {
		return "", err
	}
	return endpoint + "/agenthub", nil
}

func endpointForAddress(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", fmt.Errorf("invalid port %q", port)
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	endpoint := &url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)}
	return endpoint.String(), nil
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyExecutable(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open PUA backend: %w", err)
	}
	defer input.Close()
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".pua-install-*")
	if err != nil {
		return fmt.Errorf("create backend staging file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := io.Copy(temporary, input); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("copy PUA backend: %w", err)
	}
	if err := temporary.Chmod(0o755); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("make PUA backend executable: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync PUA backend: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close PUA backend staging file: %w", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return fmt.Errorf("install PUA backend: %w", err)
	}
	return nil
}

func writeJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".desktop-state-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func readJSON[T any](path string) (T, bool) {
	var value T
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &value) != nil {
		return value, false
	}
	return value, true
}

func readPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid, err == nil && pid > 0
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

func replaceEnv(environ []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

func prependEnvPath(environ []string, directory string) []string {
	current := ""
	for _, entry := range environ {
		if strings.HasPrefix(entry, "PATH=") {
			current = strings.TrimPrefix(entry, "PATH=")
			break
		}
	}
	for _, entry := range filepath.SplitList(current) {
		if entry == directory {
			return environ
		}
	}
	if current == "" {
		return replaceEnv(environ, "PATH", directory)
	}
	return replaceEnv(environ, "PATH", directory+string(os.PathListSeparator)+current)
}

// prependEnvPaths prepends directories in the order supplied, while keeping
// the operation idempotent. The reverse iteration is needed because each
// prepend operation adds one entry at the front of PATH.
func prependEnvPaths(environ []string, directories ...string) []string {
	result := environ
	for index := len(directories) - 1; index >= 0; index-- {
		if strings.TrimSpace(directories[index]) == "" {
			continue
		}
		result = prependEnvPath(result, directories[index])
	}
	return result
}

func backendEnvironment(environ []string, cliPath string) []string {
	directories := make([]string, 0, len(macOSProviderPathEntries)+1)
	if cliPath != "" {
		directories = append(directories, filepath.Dir(cliPath))
	}
	directories = append(directories, macOSProviderPathEntries...)
	return prependEnvPaths(environ, directories...)
}

func ensureShellPath(profilePath, directory string) error {
	if info, err := os.Lstat(profilePath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		resolved, resolveErr := filepath.EvalSymlinks(profilePath)
		if resolveErr != nil {
			return fmt.Errorf("resolve shell profile symlink: %w", resolveErr)
		}
		profilePath = resolved
	}
	data, err := os.ReadFile(profilePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read shell profile: %w", err)
	}
	content := string(data)
	start := strings.Index(content, pathBlockStart)
	end := strings.Index(content, pathBlockEnd)
	if start >= 0 && end >= start {
		end += len(pathBlockEnd)
		content = strings.TrimRight(content[:start], "\n") + strings.TrimLeft(content[end:], "\n")
	}
	block := pathBlockStart + "\nexport PATH=" + strconv.Quote(directory+":$PATH") + "\n" + pathBlockEnd
	content = strings.TrimRight(content, "\n")
	if content != "" {
		content += "\n\n"
	}
	content += block + "\n"
	if string(data) == content {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(profilePath), 0o700); err != nil {
		return fmt.Errorf("create shell profile directory: %w", err)
	}
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(profilePath); statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := writeFileAtomic(profilePath, []byte(content), mode); err != nil {
		return fmt.Errorf("update shell PATH: %w", err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".pua-desktop-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func manifestPath(options Options) string {
	return filepath.Join(options.AppSupportDir, "backend", "current.json")
}

func statePath(options Options) string {
	return filepath.Join(options.AppSupportDir, "backend-state.json")
}

func agentHubManifestPath(options Options) string {
	return filepath.Join(options.AppSupportDir, "agenthub", "current.json")
}

func agentHubDesktopStatePath(options Options) string {
	return filepath.Join(options.AppSupportDir, "agenthub-state.json")
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func defaultShellProfile(home string) string {
	if filepath.Base(os.Getenv("SHELL")) == "bash" {
		return filepath.Join(home, ".bash_profile")
	}
	return filepath.Join(home, ".zprofile")
}
