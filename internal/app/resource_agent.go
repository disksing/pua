package app

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/disksing/pua/internal/workspacepath"
)

func defaultAgentBinding() AgentBinding {
	return AgentBinding{Kind: "profile", Name: "default"}
}

// normalizeDefaultBinding normalizes one configured resource default. An
// empty or unknown kind falls back to the default Profile; Profile names are
// case-insensitive while Agent names keep their canonical case.
func normalizeDefaultBinding(value AgentBinding) AgentBinding {
	kind := strings.ToLower(strings.TrimSpace(value.Kind))
	name := strings.TrimSpace(value.Name)
	if kind != "agent" {
		kind = "profile"
		name = strings.ToLower(name)
	}
	if name == "" {
		return defaultAgentBinding()
	}
	return AgentBinding{Kind: kind, Name: name}
}

func normalizeResourceDefaults(value ResourceAgentDefaults) ResourceAgentDefaults {
	return ResourceAgentDefaults{
		Project: normalizeDefaultBinding(value.Project),
		Task:    normalizeDefaultBinding(value.Task),
	}
}

func defaultGenerationPolicy() GenerationPolicy {
	return GenerationPolicy{
		Enabled:                   true,
		MaxTurns:                  DefaultGenerationMaxTurns,
		MaxAccumulatedTurnMinutes: DefaultGenerationMaxAccumulatedTurnMinutes,
	}
}

func defaultStallWatchdogPolicy() StallWatchdogPolicy {
	return StallWatchdogPolicy{
		Enabled:        true,
		TimeoutMinutes: DefaultStallWatchdogTimeoutMinutes,
	}
}

func resolveGenerationPolicy(value GenerationPolicyConfig) (GenerationPolicy, error) {
	policy := defaultGenerationPolicy()
	if value.Enabled != nil {
		policy.Enabled = *value.Enabled
	}
	if value.MaxTurns != nil {
		policy.MaxTurns = *value.MaxTurns
	}
	if value.MaxAccumulatedTurnMinutes != nil {
		policy.MaxAccumulatedTurnMinutes = *value.MaxAccumulatedTurnMinutes
	}
	return normalizeGenerationPolicy(policy)
}

func normalizeGenerationPolicy(policy GenerationPolicy) (GenerationPolicy, error) {
	if policy.MaxTurns < 0 || policy.MaxTurns > 100000 {
		return GenerationPolicy{}, errors.New("generation max turns must be between 0 and 100000")
	}
	if policy.MaxAccumulatedTurnMinutes < 0 || policy.MaxAccumulatedTurnMinutes > 525600 {
		return GenerationPolicy{}, errors.New("generation accumulated turn minutes must be between 0 and 525600")
	}
	if policy.Enabled && policy.MaxTurns == 0 && policy.MaxAccumulatedTurnMinutes == 0 {
		return GenerationPolicy{}, errors.New("an enabled generation policy requires at least one non-zero budget")
	}
	return policy, nil
}

func generationPolicyConfig(policy GenerationPolicy) GenerationPolicyConfig {
	enabled := policy.Enabled
	maxTurns := policy.MaxTurns
	maxMinutes := policy.MaxAccumulatedTurnMinutes
	return GenerationPolicyConfig{
		Enabled:                   &enabled,
		MaxTurns:                  &maxTurns,
		MaxAccumulatedTurnMinutes: &maxMinutes,
	}
}

func resolveStallWatchdogPolicy(value StallWatchdogPolicyConfig) (StallWatchdogPolicy, error) {
	policy := defaultStallWatchdogPolicy()
	if value.Enabled != nil {
		policy.Enabled = *value.Enabled
	}
	if value.TimeoutMinutes != nil {
		policy.TimeoutMinutes = *value.TimeoutMinutes
	}
	return normalizeStallWatchdogPolicy(policy)
}

func normalizeStallWatchdogPolicy(policy StallWatchdogPolicy) (StallWatchdogPolicy, error) {
	if policy.TimeoutMinutes < 1 || policy.TimeoutMinutes > 525600 {
		return StallWatchdogPolicy{}, errors.New("stall watchdog timeout minutes must be between 1 and 525600")
	}
	return policy, nil
}

func stallWatchdogPolicyConfig(policy StallWatchdogPolicy) StallWatchdogPolicyConfig {
	enabled := policy.Enabled
	timeoutMinutes := policy.TimeoutMinutes
	return StallWatchdogPolicyConfig{Enabled: &enabled, TimeoutMinutes: &timeoutMinutes}
}

// NormalizeAgentBinding validates one explicit resource binding. An empty
// legacy binding is normalized to the default Profile during migration.
func NormalizeAgentBinding(value AgentBinding) (AgentBinding, error) {
	value.Kind = strings.ToLower(strings.TrimSpace(value.Kind))
	value.Name = strings.TrimSpace(value.Name)
	if value.Kind == "" && value.Name == "" {
		return defaultAgentBinding(), nil
	}
	if value.Kind != "profile" && value.Kind != "agent" {
		return AgentBinding{}, errors.New("agent binding kind must be profile or agent")
	}
	if value.Name == "" {
		return AgentBinding{}, errors.New("agent binding name is required")
	}
	if len(value.Name) > 80 || strings.ContainsRune(value.Name, '\x00') {
		return AgentBinding{}, errors.New("agent binding name is invalid")
	}
	if value.Kind == "profile" {
		value.Name = strings.ToLower(value.Name)
	}
	return value, nil
}

func newWorkspaceInstanceID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "ws-" + hex.EncodeToString(random[:]), nil
}

// EnsureResourceRuntime performs the minimal, lossless stage-one conversion:
// it assigns a stable Workspace instance id, initializes missing resource
// defaults to the default Profile, and assigns explicit bindings to every
// open resource that predates bindings. Existing configured values are never
// overwritten; the Workspace owns its defaults after initialization. Existing
// files and worktrees are not otherwise changed.
func (w *Workspace) EnsureResourceRuntime() (WorkspaceRuntimeConfig, error) {
	if err := w.require(); err != nil {
		return WorkspaceRuntimeConfig{}, err
	}
	var result WorkspaceRuntimeConfig
	err := withWorkspaceMutationLock(w.root, func() error {
		cfg, err := readWorkspaceConfig(w.root)
		if err != nil {
			return err
		}
		changed := false
		normalizedDefaults := normalizeResourceDefaults(cfg.ResourceDefaults)
		if cfg.ResourceDefaults != normalizedDefaults {
			cfg.ResourceDefaults = normalizedDefaults
			changed = true
		}
		generationPolicy, err := resolveGenerationPolicy(cfg.GenerationPolicy)
		if err != nil {
			return err
		}
		if cfg.GenerationPolicy.Enabled == nil || cfg.GenerationPolicy.MaxTurns == nil || cfg.GenerationPolicy.MaxAccumulatedTurnMinutes == nil {
			cfg.GenerationPolicy = generationPolicyConfig(generationPolicy)
			changed = true
		}
		stallWatchdogPolicy, err := resolveStallWatchdogPolicy(cfg.StallWatchdogPolicy)
		if err != nil {
			return err
		}
		if cfg.StallWatchdogPolicy.Enabled == nil || cfg.StallWatchdogPolicy.TimeoutMinutes == nil {
			cfg.StallWatchdogPolicy = stallWatchdogPolicyConfig(stallWatchdogPolicy)
			changed = true
		}
		if strings.TrimSpace(cfg.InstanceID) == "" {
			cfg.InstanceID, err = newWorkspaceInstanceID()
			if err != nil {
				return err
			}
			changed = true
		}
		if strings.TrimSpace(cfg.AgentBinding.Name) == "" {
			cfg.AgentBinding = defaultAgentBinding()
			changed = true
		}
		binding, err := NormalizeAgentBinding(cfg.AgentBinding)
		if err != nil {
			return err
		}
		if cfg.AgentBinding != binding {
			cfg.AgentBinding = binding
			changed = true
		}
		if changed {
			if err := writeWorkspaceConfig(w.root, cfg); err != nil {
				return err
			}
		}
		if err := ensureOpenResourceBindings(w.root, cfg.ResourceDefaults); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(workspacepath.ControlDir(w.root), "runtime"), 0o700); err != nil {
			return err
		}
		result = WorkspaceRuntimeConfig{InstanceID: cfg.InstanceID, AgentBinding: cfg.AgentBinding, ResourceDefaults: cfg.ResourceDefaults, GenerationPolicy: generationPolicy, StallWatchdogPolicy: stallWatchdogPolicy}
		return nil
	})
	if err != nil {
		return WorkspaceRuntimeConfig{}, &APIError{Operation: "initialize resource runtime", Kind: "runtime", Workspace: w.root, Err: err}
	}
	return result, nil
}

func ensureOpenResourceBindings(root string, defaults ResourceAgentDefaults) error {
	projects, err := readProjectEntriesInDirs([]string{root})
	if err != nil {
		return err
	}
	for _, entry := range projects {
		project := entry.Project
		if strings.TrimSpace(project.AgentBinding.Name) == "" {
			project.AgentBinding = normalizeDefaultBinding(defaults.Project)
			project.UpdatedAt = time.Now().Format(time.RFC3339)
			if err := writeResourceMetadata(entry.Path, &project); err != nil {
				return err
			}
		}
		tasks, err := readTaskEntriesInDirs([]string{entry.Path}, projectTaskName(project.ID))
		if err != nil {
			return err
		}
		for _, taskEntry := range tasks {
			task := taskEntry.Task
			if strings.TrimSpace(task.AgentBinding.Name) != "" {
				continue
			}
			task.AgentBinding = normalizeDefaultBinding(defaults.Task)
			task.UpdatedAt = time.Now().Format(time.RFC3339)
			if err := writeResourceMetadata(taskEntry.Path, &task); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *Workspace) RuntimeConfig() (WorkspaceRuntimeConfig, error) {
	if err := w.require(); err != nil {
		return WorkspaceRuntimeConfig{}, err
	}
	cfg, err := readWorkspaceConfig(w.root)
	if err != nil {
		return WorkspaceRuntimeConfig{}, err
	}
	binding, err := NormalizeAgentBinding(cfg.AgentBinding)
	if err != nil {
		return WorkspaceRuntimeConfig{}, err
	}
	if strings.TrimSpace(cfg.InstanceID) == "" {
		return WorkspaceRuntimeConfig{}, fmt.Errorf("Workspace resource runtime is not initialized")
	}
	policy, err := resolveGenerationPolicy(cfg.GenerationPolicy)
	if err != nil {
		return WorkspaceRuntimeConfig{}, err
	}
	stallWatchdogPolicy, err := resolveStallWatchdogPolicy(cfg.StallWatchdogPolicy)
	if err != nil {
		return WorkspaceRuntimeConfig{}, err
	}
	return WorkspaceRuntimeConfig{InstanceID: cfg.InstanceID, AgentBinding: binding, ResourceDefaults: normalizeResourceDefaults(cfg.ResourceDefaults), GenerationPolicy: policy, StallWatchdogPolicy: stallWatchdogPolicy}, nil
}

// SetGenerationPolicy replaces the Workspace-wide automatic generation
// rotation policy. It applies uniformly to Workspace, Project, Task, and
// Scheduler resources.
func (w *Workspace) SetGenerationPolicy(policy GenerationPolicy) (GenerationPolicy, error) {
	if err := w.require(); err != nil {
		return GenerationPolicy{}, err
	}
	policy, err := normalizeGenerationPolicy(policy)
	if err != nil {
		return GenerationPolicy{}, &APIError{Operation: "set generation policy", Kind: "generation_policy", Workspace: w.root, Err: err}
	}
	err = withWorkspaceMutationLock(w.root, func() error {
		cfg, err := readWorkspaceConfig(w.root)
		if err != nil {
			return err
		}
		cfg.GenerationPolicy = generationPolicyConfig(policy)
		return writeWorkspaceConfig(w.root, cfg)
	})
	if err != nil {
		return GenerationPolicy{}, &APIError{Operation: "set generation policy", Kind: "generation_policy", Workspace: w.root, Err: err}
	}
	return policy, nil
}

// SetStallWatchdogPolicy replaces the Workspace-wide Turn stall watchdog. It
// applies uniformly to Workspace, Project, Task, and Scheduler resources.
func (w *Workspace) SetStallWatchdogPolicy(policy StallWatchdogPolicy) (StallWatchdogPolicy, error) {
	if err := w.require(); err != nil {
		return StallWatchdogPolicy{}, err
	}
	policy, err := normalizeStallWatchdogPolicy(policy)
	if err != nil {
		return StallWatchdogPolicy{}, &APIError{Operation: "set stall watchdog policy", Kind: "stall_watchdog_policy", Workspace: w.root, Err: err}
	}
	err = withWorkspaceMutationLock(w.root, func() error {
		cfg, err := readWorkspaceConfig(w.root)
		if err != nil {
			return err
		}
		cfg.StallWatchdogPolicy = stallWatchdogPolicyConfig(policy)
		return writeWorkspaceConfig(w.root, cfg)
	})
	if err != nil {
		return StallWatchdogPolicy{}, &APIError{Operation: "set stall watchdog policy", Kind: "stall_watchdog_policy", Workspace: w.root, Err: err}
	}
	return policy, nil
}

func (w *Workspace) ResourceAgentBinding(id string) (AgentBinding, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(id) == "workspace" {
		cfg, err := w.RuntimeConfig()
		return cfg.AgentBinding, err
	}
	if strings.TrimSpace(id) == SchedulerResourceID {
		cfg, err := w.Scheduler()
		return cfg.AgentBinding, err
	}
	result, err := w.ResourceValue(id)
	if err != nil {
		return AgentBinding{}, err
	}
	binding, err := NormalizeAgentBinding(result.Resource().resourceMeta().AgentBinding)
	if err != nil {
		return AgentBinding{}, &APIError{Operation: "read resource agent binding", Kind: "binding", Workspace: w.root, ResourceID: id, Err: err}
	}
	return binding, nil
}

func (w *Workspace) SetResourceAgentBinding(id string, binding AgentBinding) (AgentBinding, error) {
	binding, err := NormalizeAgentBinding(binding)
	if err != nil {
		return AgentBinding{}, &APIError{Operation: "set resource agent binding", Kind: "binding", Workspace: w.root, ResourceID: id, Err: err}
	}
	err = withWorkspaceMutationLock(w.root, func() error {
		if strings.TrimSpace(id) == "" || strings.TrimSpace(id) == "workspace" {
			cfg, err := readWorkspaceConfig(w.root)
			if err != nil {
				return err
			}
			cfg.AgentBinding = binding
			return writeWorkspaceConfig(w.root, cfg)
		}
		if strings.TrimSpace(id) == SchedulerResourceID {
			cfg, err := readSchedulerJSON(schedulerJSONPath(w.root))
			if err != nil {
				return err
			}
			cfg.AgentBinding = binding
			return writeSchedulerJSON(schedulerJSONPath(w.root), cfg)
		}
		path, resource, err := loadOpenResource(w.root, strings.TrimSpace(id))
		if err != nil {
			return err
		}
		resource.resourceMeta().AgentBinding = binding
		resource.resourceMeta().UpdatedAt = time.Now().Format(time.RFC3339)
		return writeResourceMetadata(path, resource)
	})
	if err != nil {
		return AgentBinding{}, &APIError{Operation: "set resource agent binding", Kind: "binding", Workspace: w.root, ResourceID: id, Err: err}
	}
	return binding, nil
}

// SetResourceAgentDefaults updates the Workspace-level default bindings
// applied to newly created Projects and Tasks. Existing resources keep their
// explicit bindings.
func (w *Workspace) SetResourceAgentDefaults(defaults ResourceAgentDefaults) (ResourceAgentDefaults, error) {
	if err := w.require(); err != nil {
		return ResourceAgentDefaults{}, err
	}
	normalized := normalizeResourceDefaults(defaults)
	if _, err := NormalizeAgentBinding(normalized.Project); err != nil {
		return ResourceAgentDefaults{}, &APIError{Operation: "set resource agent defaults", Kind: "binding", Workspace: w.root, Err: err}
	}
	if _, err := NormalizeAgentBinding(normalized.Task); err != nil {
		return ResourceAgentDefaults{}, &APIError{Operation: "set resource agent defaults", Kind: "binding", Workspace: w.root, Err: err}
	}
	err := withWorkspaceMutationLock(w.root, func() error {
		cfg, err := readWorkspaceConfig(w.root)
		if err != nil {
			return err
		}
		cfg.ResourceDefaults = normalized
		return writeWorkspaceConfig(w.root, cfg)
	})
	if err != nil {
		return ResourceAgentDefaults{}, &APIError{Operation: "set resource agent defaults", Kind: "binding", Workspace: w.root, Err: err}
	}
	return normalized, nil
}

// SetProjectTaskDefault sets the Project-level default binding applied to
// newly created Tasks. An empty binding clears the override so the Project
// inherits the Workspace default.
func (w *Workspace) SetProjectTaskDefault(id string, binding AgentBinding) (AgentBinding, error) {
	if err := w.require(); err != nil {
		return AgentBinding{}, err
	}
	cleared := strings.TrimSpace(binding.Kind) == "" && strings.TrimSpace(binding.Name) == ""
	if cleared {
		binding = AgentBinding{}
	} else {
		var err error
		binding, err = NormalizeAgentBinding(binding)
		if err != nil {
			return AgentBinding{}, &APIError{Operation: "set project task default", Kind: "binding", Workspace: w.root, ResourceID: id, Err: err}
		}
	}
	err := withWorkspaceMutationLock(w.root, func() error {
		path, resource, err := loadOpenResource(w.root, strings.TrimSpace(id))
		if err != nil {
			return err
		}
		project, ok := resource.(*Project)
		if !ok {
			return fmt.Errorf("resource %s is not a project", id)
		}
		project.TaskDefault = binding
		project.UpdatedAt = time.Now().Format(time.RFC3339)
		return writeResourceMetadata(path, project)
	})
	if err != nil {
		return AgentBinding{}, &APIError{Operation: "set project task default", Kind: "binding", Workspace: w.root, ResourceID: id, Err: err}
	}
	return binding, nil
}
