package serve

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/disksing/pua/internal/workspacepath"
)

const (
	serviceSchemaVersion  = 1
	serviceExportSchema   = 1
	serviceRuntimeDir     = "services"
	serviceConfigDir      = "services"
	serviceBindingsFile   = "bindings.json"
	defaultRestartDelay   = time.Second
	defaultRestartMax     = 5 * time.Minute
	defaultRestartReset   = 10 * time.Minute
	defaultReadyInterval  = 5 * time.Second
	defaultReadyTimeout   = 5 * time.Second
	defaultCleanupTimeout = 10 * time.Second
	defaultLogMaxBytes    = 10 << 20
	defaultLogMaxFiles    = 5
)

var (
	serviceIDPattern       = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	serviceTemplatePattern = regexp.MustCompile(`\$\{service\.([a-z][a-z0-9_-]{0,63})\.([A-Za-z_][A-Za-z0-9_]*)\}`)
	secretTemplatePattern  = regexp.MustCompile(`\$\{secret[\.:]([A-Za-z0-9_.:/-]+)\}`)
)

// ServiceConfig is the versioned definition stored in .pua/services/<id>.json.
// Commands are always executed as argument arrays; no field is interpreted by
// a shell.
type ServiceConfig struct {
	SchemaVersion int                           `json:"schemaVersion"`
	ID            string                        `json:"id"`
	Enabled       bool                          `json:"enabled"`
	Command       []string                      `json:"command"`
	Args          []string                      `json:"args,omitempty"`
	CWD           string                        `json:"cwd,omitempty"`
	Environment   map[string]ServiceEnvironment `json:"environment,omitempty"`
	DependsOn     []string                      `json:"dependsOn,omitempty"`
	// Exports declares that the process atomically writes its complete initial
	// export hand-off before startup output may be persisted.
	Exports     bool                     `json:"exports,omitempty"`
	Readiness   *ServiceReadinessConfig  `json:"readiness,omitempty"`
	Cleanup     *ServiceCleanupConfig    `json:"cleanup,omitempty"`
	Restart     ServiceRestartConfig     `json:"restart,omitempty"`
	LogRotation ServiceLogRotationConfig `json:"logRotation,omitempty"`
}

// ServiceEnvironment is intentionally a small sum type. A string is a
// literal or a ${service.<id>.<key>} template; secret values are represented
// by a reference and never by their resolved value.
type ServiceEnvironment struct {
	Literal    string `json:"-"`
	Template   string `json:"-"`
	SecretName string `json:"-"`
}

func (v *ServiceEnvironment) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return errors.New("environment value is empty")
	}
	if data[0] == '"' {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		return v.fromString(value)
	}
	var object struct {
		Literal   *string `json:"literal"`
		Value     *string `json:"value"`
		Template  *string `json:"template"`
		Secret    *string `json:"secret"`
		SecretRef *string `json:"secretRef"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&object); err != nil {
		return fmt.Errorf("environment value must be a string or object: %w", err)
	}
	count := 0
	if object.Literal != nil {
		count++
	}
	if object.Value != nil {
		count++
	}
	if object.Template != nil {
		count++
	}
	if object.Secret != nil {
		count++
	}
	if object.SecretRef != nil {
		count++
	}
	if count != 1 {
		return errors.New("environment value must contain exactly one of literal, value, template, secret, or secretRef")
	}
	switch {
	case object.Secret != nil || object.SecretRef != nil:
		name := ""
		if object.Secret != nil {
			name = strings.TrimSpace(*object.Secret)
		} else {
			name = strings.TrimSpace(*object.SecretRef)
		}
		if !validSecretName(name) {
			return fmt.Errorf("invalid secret reference %q", name)
		}
		*v = ServiceEnvironment{SecretName: name}
	case object.Template != nil:
		*v = ServiceEnvironment{Template: *object.Template}
	case object.Literal != nil:
		*v = ServiceEnvironment{Literal: *object.Literal}
	default:
		return v.fromString(*object.Value)
	}
	return nil
}

func (v *ServiceEnvironment) fromString(value string) error {
	if matches := secretTemplatePattern.FindStringSubmatch(value); len(matches) == 2 && matches[0] == value {
		if !validSecretName(matches[1]) {
			return fmt.Errorf("invalid secret reference %q", matches[1])
		}
		*v = ServiceEnvironment{SecretName: matches[1]}
		return nil
	}
	if strings.Contains(value, "${") {
		*v = ServiceEnvironment{Template: value}
		return nil
	}
	*v = ServiceEnvironment{Literal: value}
	return nil
}

func (v ServiceEnvironment) MarshalJSON() ([]byte, error) {
	switch {
	case v.SecretName != "":
		return json.Marshal(map[string]string{"secret": v.SecretName})
	case v.Template != "":
		return json.Marshal(v.Template)
	default:
		return json.Marshal(v.Literal)
	}
}

type ServiceReadinessConfig struct {
	Command  []string      `json:"command"`
	Interval time.Duration `json:"interval,omitempty"`
	Timeout  time.Duration `json:"timeout,omitempty"`
}

// Duration fields accept either a JSON duration string ("2s") or a positive
// integer number of nanoseconds. This keeps hand-authored service files
// readable while preserving Go's normal duration representation.
func (c *ServiceReadinessConfig) UnmarshalJSON(data []byte) error {
	var raw struct {
		Command  []string        `json:"command"`
		Interval json.RawMessage `json:"interval"`
		Timeout  json.RawMessage `json:"timeout"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	interval, err := decodeServiceDuration(raw.Interval, defaultReadyInterval)
	if err != nil {
		return fmt.Errorf("readiness interval: %w", err)
	}
	timeout, err := decodeServiceDuration(raw.Timeout, defaultReadyTimeout)
	if err != nil {
		return fmt.Errorf("readiness timeout: %w", err)
	}
	*c = ServiceReadinessConfig{Command: raw.Command, Interval: interval, Timeout: timeout}
	return nil
}

func (c ServiceReadinessConfig) MarshalJSON() ([]byte, error) {
	interval, timeout := "", ""
	if c.Interval > 0 {
		interval = c.Interval.String()
	}
	if c.Timeout > 0 {
		timeout = c.Timeout.String()
	}
	return json.Marshal(struct {
		Command  []string `json:"command"`
		Interval string   `json:"interval,omitempty"`
		Timeout  string   `json:"timeout,omitempty"`
	}{c.Command, interval, timeout})
}

type ServiceCleanupConfig struct {
	Command []string      `json:"command"`
	Timeout time.Duration `json:"timeout,omitempty"`
	Retries int           `json:"retries,omitempty"`
}

func (c *ServiceCleanupConfig) UnmarshalJSON(data []byte) error {
	var raw struct {
		Command []string        `json:"command"`
		Timeout json.RawMessage `json:"timeout"`
		Retries int             `json:"retries"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	timeout, err := decodeServiceDuration(raw.Timeout, defaultCleanupTimeout)
	if err != nil {
		return fmt.Errorf("cleanup timeout: %w", err)
	}
	*c = ServiceCleanupConfig{Command: raw.Command, Timeout: timeout, Retries: raw.Retries}
	return nil
}

func (c ServiceCleanupConfig) MarshalJSON() ([]byte, error) {
	timeout := ""
	if c.Timeout > 0 {
		timeout = c.Timeout.String()
	}
	return json.Marshal(struct {
		Command []string `json:"command"`
		Timeout string   `json:"timeout,omitempty"`
		Retries int      `json:"retries,omitempty"`
	}{c.Command, timeout, c.Retries})
}

type ServiceRestartConfig struct {
	InitialDelay time.Duration `json:"initialDelay,omitempty"`
	Multiplier   float64       `json:"multiplier,omitempty"`
	MaxDelay     time.Duration `json:"maxDelay,omitempty"`
	ResetAfter   time.Duration `json:"resetAfter,omitempty"`
}

func (c *ServiceRestartConfig) UnmarshalJSON(data []byte) error {
	var raw struct {
		InitialDelay json.RawMessage `json:"initialDelay"`
		Multiplier   float64         `json:"multiplier"`
		MaxDelay     json.RawMessage `json:"maxDelay"`
		ResetAfter   json.RawMessage `json:"resetAfter"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	initial, err := decodeServiceDuration(raw.InitialDelay, defaultRestartDelay)
	if err != nil {
		return fmt.Errorf("restart initialDelay: %w", err)
	}
	maxDelay, err := decodeServiceDuration(raw.MaxDelay, defaultRestartMax)
	if err != nil {
		return fmt.Errorf("restart maxDelay: %w", err)
	}
	reset, err := decodeServiceDuration(raw.ResetAfter, defaultRestartReset)
	if err != nil {
		return fmt.Errorf("restart resetAfter: %w", err)
	}
	*c = ServiceRestartConfig{InitialDelay: initial, Multiplier: raw.Multiplier, MaxDelay: maxDelay, ResetAfter: reset}
	return nil
}

func (c ServiceRestartConfig) MarshalJSON() ([]byte, error) {
	initial, maxDelay, reset := "", "", ""
	if c.InitialDelay > 0 {
		initial = c.InitialDelay.String()
	}
	if c.MaxDelay > 0 {
		maxDelay = c.MaxDelay.String()
	}
	if c.ResetAfter > 0 {
		reset = c.ResetAfter.String()
	}
	return json.Marshal(struct {
		InitialDelay string  `json:"initialDelay,omitempty"`
		Multiplier   float64 `json:"multiplier,omitempty"`
		MaxDelay     string  `json:"maxDelay,omitempty"`
		ResetAfter   string  `json:"resetAfter,omitempty"`
	}{initial, c.Multiplier, maxDelay, reset})
}

type ServiceLogRotationConfig struct {
	MaxBytes int64 `json:"maxBytes,omitempty"`
	MaxFiles int   `json:"maxFiles,omitempty"`
}

// ServiceExportFile is the only data a service writes to its export path.
// Secrets are accepted in-memory, but callers must not serialize the resolved
// values into ServiceStatus or any other durable record.
type ServiceExportFile struct {
	SchemaVersion int               `json:"schemaVersion"`
	Variables     map[string]string `json:"variables,omitempty"`
	Secrets       map[string]string `json:"secrets,omitempty"`
}

type ServiceSecretMetadata struct {
	Name      string `json:"name"`
	Source    string `json:"source,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

type ServiceExports struct {
	Variables map[string]string       `json:"variables,omitempty"`
	Secrets   []ServiceSecretMetadata `json:"secrets,omitempty"`
	UpdatedAt string                  `json:"updatedAt,omitempty"`
}

type ServiceReadinessStatus struct {
	Configured bool   `json:"configured"`
	Ready      bool   `json:"ready"`
	LastCheck  string `json:"lastCheck,omitempty"`
	LastError  string `json:"lastError,omitempty"`
}

type ServiceCleanupStatus struct {
	Configured bool   `json:"configured"`
	Attempts   int    `json:"attempts,omitempty"`
	LastRun    string `json:"lastRun,omitempty"`
	LastError  string `json:"lastError,omitempty"`
	Succeeded  bool   `json:"succeeded"`
}

// ServiceState is the persisted and API-visible lifecycle state of a Workspace
// service.
type ServiceState string

const (
	ServiceStateDisabled          ServiceState = "disabled"
	ServiceStateStopped           ServiceState = "stopped"
	ServiceStateBlocked           ServiceState = "blocked"
	ServiceStateStarting          ServiceState = "starting"
	ServiceStateRunning           ServiceState = "running"
	ServiceStateReady             ServiceState = "ready"
	ServiceStateBackoff           ServiceState = "backoff"
	ServiceStateAttentionRequired ServiceState = "attention_required"
)

type ServiceStatus struct {
	SchemaVersion     int                    `json:"schemaVersion"`
	ID                string                 `json:"id"`
	Enabled           bool                   `json:"enabled"`
	State             ServiceState           `json:"state"`
	PID               int                    `json:"pid,omitempty"`
	ProcessGroup      int                    `json:"processGroup,omitempty"`
	StartedAt         string                 `json:"startedAt,omitempty"`
	ExitedAt          string                 `json:"exitedAt,omitempty"`
	ExitCode          int                    `json:"exitCode,omitempty"`
	ExitError         string                 `json:"exitError,omitempty"`
	FailureCount      int                    `json:"failureCount,omitempty"`
	NextRetryAt       string                 `json:"nextRetryAt,omitempty"`
	LastError         string                 `json:"lastError,omitempty"`
	AttentionRequired bool                   `json:"attentionRequired,omitempty"`
	Dependencies      []string               `json:"dependencies,omitempty"`
	Readiness         ServiceReadinessStatus `json:"readiness"`
	Cleanup           ServiceCleanupStatus   `json:"cleanup"`
	Exports           ServiceExports         `json:"exports"`
	CommandDigest     string                 `json:"commandDigest,omitempty"`
	InstanceToken     string                 `json:"instanceToken,omitempty"`
	UpdatedAt         string                 `json:"updatedAt,omitempty"`
	ManualStop        bool                   `json:"manualStop,omitempty"`
}

type ServiceBindings struct {
	SchemaVersion int               `json:"schemaVersion"`
	Variables     map[string]string `json:"variables,omitempty"`
	Secrets       map[string]string `json:"secrets,omitempty"`
}

type ServiceValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e ServiceValidationError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

func decodeServiceDuration(data json.RawMessage, fallback time.Duration) (time.Duration, error) {
	if len(bytes.TrimSpace(data)) == 0 || string(bytes.TrimSpace(data)) == "null" {
		return fallback, nil
	}
	var text string
	if data[0] == '"' {
		if err := json.Unmarshal(data, &text); err != nil {
			return 0, err
		}
		d, err := time.ParseDuration(strings.TrimSpace(text))
		if err != nil {
			return 0, err
		}
		if d <= 0 {
			return 0, errors.New("duration must be positive")
		}
		return d, nil
	}
	var number int64
	if err := json.Unmarshal(data, &number); err != nil {
		return 0, errors.New("expected a duration string or nanoseconds")
	}
	if number <= 0 {
		return 0, errors.New("duration must be positive")
	}
	return time.Duration(number), nil
}

func defaultServiceConfig(c ServiceConfig) ServiceConfig {
	if c.Restart.InitialDelay <= 0 {
		c.Restart.InitialDelay = defaultRestartDelay
	}
	if c.Restart.Multiplier <= 0 {
		c.Restart.Multiplier = 2
	}
	if c.Restart.MaxDelay <= 0 {
		c.Restart.MaxDelay = defaultRestartMax
	}
	if c.Restart.ResetAfter <= 0 {
		c.Restart.ResetAfter = defaultRestartReset
	}
	if c.LogRotation.MaxBytes <= 0 {
		c.LogRotation.MaxBytes = defaultLogMaxBytes
	}
	if c.LogRotation.MaxFiles <= 0 {
		c.LogRotation.MaxFiles = defaultLogMaxFiles
	}
	if c.Readiness != nil {
		if c.Readiness.Interval <= 0 {
			c.Readiness.Interval = defaultReadyInterval
		}
		if c.Readiness.Timeout <= 0 {
			c.Readiness.Timeout = defaultReadyTimeout
		}
	}
	if c.Cleanup != nil {
		if c.Cleanup.Timeout <= 0 {
			c.Cleanup.Timeout = defaultCleanupTimeout
		}
		if c.Cleanup.Retries < 0 {
			c.Cleanup.Retries = 0
		}
	}
	return c
}

func validSecretName(name string) bool {
	if name == "" || len(name) > 256 {
		return false
	}
	for _, r := range name {
		if !(r == '_' || r == '-' || r == '.' || r == '/' || r == ':' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func reservedServiceBindingName(name string) bool {
	switch name {
	case "PUA_WORKSPACE_ROOT", "PUA_WORKSPACE_INSTANCE_ID", "PUA_RESOURCE_ID":
		return true
	default:
		return false
	}
}

func validateServiceConfig(root string, configs map[string]ServiceConfig, candidate ServiceConfig) error {
	candidate = defaultServiceConfig(candidate)
	if candidate.SchemaVersion != serviceSchemaVersion {
		return ServiceValidationError{"schemaVersion", fmt.Sprintf("unsupported schema version %d", candidate.SchemaVersion)}
	}
	if !serviceIDPattern.MatchString(candidate.ID) {
		return ServiceValidationError{"id", "must match ^[a-z][a-z0-9_-]{0,63}$"}
	}
	if len(candidate.Command) == 0 || strings.TrimSpace(candidate.Command[0]) == "" {
		return ServiceValidationError{"command", "must contain an executable"}
	}
	for i, value := range append(append([]string{}, candidate.Command...), candidate.Args...) {
		if strings.ContainsRune(value, '\x00') {
			return ServiceValidationError{fmt.Sprintf("command[%d]", i), "must not contain NUL"}
		}
		if secretTemplatePattern.MatchString(value) || strings.Contains(value, "${secret") {
			return ServiceValidationError{fmt.Sprintf("command[%d]", i), "secret references are not allowed in command arguments"}
		}
	}
	cwd := candidate.CWD
	if cwd == "" {
		cwd = root
	}
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(root, cwd)
	}
	abs, err := filepath.Abs(filepath.Clean(cwd))
	if err != nil {
		return ServiceValidationError{"cwd", err.Error()}
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if !pathWithinResolved(rootAbs, abs) {
		return ServiceValidationError{"cwd", "must remain inside the workspace"}
	}
	if _, err := os.Stat(abs); err != nil && !os.IsNotExist(err) {
		return ServiceValidationError{"cwd", err.Error()}
	}
	for name, value := range candidate.Environment {
		if !environmentNamePattern.MatchString(name) {
			return ServiceValidationError{"environment." + name, "invalid environment variable name"}
		}
		if value.SecretName != "" && !validSecretName(value.SecretName) {
			return ServiceValidationError{"environment." + name, "invalid secret reference"}
		}
		if value.Template != "" {
			if err := validateEnvironmentTemplate(value.Template, candidate.ID, configs); err != nil {
				return ServiceValidationError{"environment." + name, err.Error()}
			}
			for _, match := range serviceTemplatePattern.FindAllStringSubmatch(value.Template, -1) {
				if match[0] == "" {
					return ServiceValidationError{"environment." + name, "invalid service template"}
				}
				if match[1] == candidate.ID {
					return ServiceValidationError{"environment." + name, "service cannot reference itself"}
				}
				if _, ok := configs[match[1]]; !ok {
					return ServiceValidationError{"environment." + name, fmt.Sprintf("unknown service %q", match[1])}
				}
			}
		}
	}
	seenDeps := map[string]bool{}
	for _, dep := range candidate.DependsOn {
		if !serviceIDPattern.MatchString(dep) {
			return ServiceValidationError{"dependsOn", fmt.Sprintf("invalid service id %q", dep)}
		}
		if dep == candidate.ID {
			return ServiceValidationError{"dependsOn", "service cannot depend on itself"}
		}
		if seenDeps[dep] {
			return ServiceValidationError{"dependsOn", fmt.Sprintf("duplicate dependency %q", dep)}
		}
		seenDeps[dep] = true
		if _, ok := configs[dep]; !ok {
			return ServiceValidationError{"dependsOn", fmt.Sprintf("unknown service %q", dep)}
		}
	}
	for field, command := range map[string][]string{"readiness.command": readinessCommand(candidate), "cleanup.command": cleanupCommand(candidate)} {
		for index, value := range command {
			if strings.ContainsRune(value, '\x00') || secretTemplatePattern.MatchString(value) || strings.Contains(value, "${secret") {
				return ServiceValidationError{fmt.Sprintf("%s[%d]", field, index), "secret references and NUL are not allowed in command arguments"}
			}
		}
	}
	if candidate.Restart.Multiplier < 1 {
		return ServiceValidationError{"restart.multiplier", "must be at least 1"}
	}
	if candidate.Restart.MaxDelay < candidate.Restart.InitialDelay {
		return ServiceValidationError{"restart.maxDelay", "must be at least initialDelay"}
	}
	if candidate.LogRotation.MaxFiles < 1 {
		return ServiceValidationError{"logRotation.maxFiles", "must be positive"}
	}
	return nil
}

func validateEnvironmentTemplate(value, serviceID string, configs map[string]ServiceConfig) error {
	if !strings.Contains(value, "${") {
		return nil
	}
	secretMatches := secretTemplatePattern.FindAllStringSubmatch(value, -1)
	if strings.Contains(value, "${secret") {
		if len(secretMatches) != 1 || secretMatches[0][0] != value {
			return errors.New("secret references must occupy the complete value")
		}
		if !validSecretName(secretMatches[0][1]) {
			return fmt.Errorf("invalid secret reference %q", secretMatches[0][1])
		}
		return nil
	}
	serviceMatches := serviceTemplatePattern.FindAllStringSubmatch(value, -1)
	if strings.Contains(value, "${service.") && len(serviceMatches) == 0 {
		return errors.New("invalid service template")
	}
	for _, match := range serviceMatches {
		if len(match) != 3 {
			return errors.New("invalid service template")
		}
		if match[1] == serviceID {
			return errors.New("service cannot reference itself")
		}
		if _, ok := configs[match[1]]; !ok {
			return fmt.Errorf("unknown service %q", match[1])
		}
	}
	for offset := 0; ; {
		relative := strings.Index(value[offset:], "${")
		if relative < 0 {
			break
		}
		start := offset + relative
		end := strings.IndexByte(value[start:], '}')
		if end < 0 {
			return errors.New("invalid template")
		}
		raw := value[start : start+end+1]
		if serviceTemplatePattern.FindStringSubmatch(raw) == nil {
			return errors.New("invalid template")
		}
		offset = start + end + 1
	}
	// A template marker that was neither a service nor a secret reference is
	// ambiguous and must not silently become a literal environment value.
	if strings.Contains(value, "${") && len(serviceMatches) == 0 {
		return errors.New("invalid template")
	}
	return nil
}

func readinessCommand(cfg ServiceConfig) []string {
	if cfg.Readiness == nil {
		return nil
	}
	return cfg.Readiness.Command
}
func cleanupCommand(cfg ServiceConfig) []string {
	if cfg.Cleanup == nil {
		return nil
	}
	return cfg.Cleanup.Command
}

func validateServiceGraph(root string, configs map[string]ServiceConfig) error {
	for id, cfg := range configs {
		cfg = defaultServiceConfig(cfg)
		cfg.ID = id
		if err := validateServiceConfig(root, configs, cfg); err != nil {
			return err
		}
	}
	state := make(map[string]uint8)
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return ServiceValidationError{"dependsOn", fmt.Sprintf("dependency cycle includes %q", id)}
		case 2:
			return nil
		}
		state[id] = 1
		for _, dep := range configs[id].DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	ids := make([]string, 0, len(configs))
	for id := range configs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	// Service export templates are edges in the same readiness graph. A
	// template cycle must fail closed instead of leaving two services blocked
	// forever at runtime.
	templateEdges := make(map[string][]string, len(configs))
	for id, cfg := range configs {
		for _, value := range cfg.Environment {
			for _, match := range serviceTemplatePattern.FindAllStringSubmatch(value.Template, -1) {
				if len(match) == 3 {
					templateEdges[id] = append(templateEdges[id], match[1])
				}
			}
		}
	}
	state = make(map[string]uint8)
	var visitTemplate func(string) error
	visitTemplate = func(id string) error {
		switch state[id] {
		case 1:
			return ServiceValidationError{"environment", fmt.Sprintf("service template cycle includes %q", id)}
		case 2:
			return nil
		}
		state[id] = 1
		for _, dep := range templateEdges[id] {
			if err := visitTemplate(dep); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for _, id := range ids {
		if err := visitTemplate(id); err != nil {
			return err
		}
	}
	return nil
}

func pathWithin(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	if root == target {
		return true
	}
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func pathWithinResolved(root, target string) bool {
	rootAbs, err := resolveBoundaryPath(root)
	if err != nil {
		return false
	}
	targetAbs, err := resolveBoundaryPath(target)
	if err != nil {
		return false
	}
	return pathWithin(rootAbs, targetAbs)
}

// resolveBoundaryPath resolves the nearest existing ancestor before appending
// any not-yet-created path components. Resolving only filepath.EvalSymlinks
// on the complete target is insufficient: a missing child below a symlinked
// directory would otherwise be compared lexically and could escape the
// workspace control directory when it is later created.
func resolveBoundaryPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	missing := []string{}
	current := filepath.Clean(abs)
	for {
		if _, statErr := os.Lstat(current); statErr == nil {
			resolved, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return "", resolveErr
			}
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		} else if !os.IsNotExist(statErr) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return current, nil
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func newServiceInstanceToken() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err == nil {
		return hex.EncodeToString(data[:])
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func serviceCommandDigest(cfg ServiceConfig) string {
	b, _ := json.Marshal(struct {
		Command []string
		Args    []string
		CWD     string
	}{cfg.Command, cfg.Args, cfg.CWD})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func serviceConfigDigest(cfg ServiceConfig) string {
	b, _ := json.Marshal(cfg)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func serviceConfigPath(root, id string) string {
	return filepath.Join(workspacepath.ControlDir(root), serviceConfigDir, id+".json")
}
func serviceBindingsPath(root string) string {
	return filepath.Join(workspacepath.ControlDir(root), serviceConfigDir, serviceBindingsFile)
}
func serviceRuntimePath(root, id string) string {
	return filepath.Join(workspacepath.ControlDir(root), "runtime", serviceRuntimeDir, id)
}
