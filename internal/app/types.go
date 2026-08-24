package app

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Config struct {
	Version             int                       `json:"version"`
	Language            string                    `json:"language"`
	Name                string                    `json:"name,omitempty"`
	InstanceID          string                    `json:"instanceId,omitempty"`
	AgentBinding        AgentBinding              `json:"agentBinding,omitempty"`
	ResourceDefaults    ResourceAgentDefaults     `json:"resourceDefaults,omitempty"`
	GenerationPolicy    GenerationPolicyConfig    `json:"generationPolicy,omitempty"`
	StallWatchdogPolicy StallWatchdogPolicyConfig `json:"stallWatchdogPolicy,omitempty"`
}

const (
	DefaultGenerationMaxTurns                  = 20
	DefaultGenerationMaxAccumulatedTurnMinutes = 120
	DefaultGenerationMaxInactivityMinutes      = 1440
	DefaultStallWatchdogTimeoutMinutes         = 30
)

// GenerationPolicyConfig is the optional on-disk representation. Pointers
// distinguish omitted fields during migration. Enabled is the legacy switch:
// when either independent switch is absent, its value inherits Enabled.
type GenerationPolicyConfig struct {
	Enabled                   *bool `json:"enabled,omitempty"`
	BudgetEnabled             *bool `json:"budgetEnabled,omitempty"`
	MaxTurns                  *int  `json:"maxTurns,omitempty"`
	MaxAccumulatedTurnMinutes *int  `json:"maxAccumulatedTurnMinutes,omitempty"`
	InactivityEnabled         *bool `json:"inactivityEnabled,omitempty"`
	MaxInactivityMinutes      *int  `json:"maxInactivityMinutes,omitempty"`
}

// GenerationPolicy is the fully resolved Workspace policy exposed to the
// Server and UI. Existing Workspaces with no persisted policy use the
// conservative enabled defaults.
type GenerationPolicy struct {
	BudgetEnabled             bool `json:"budgetEnabled"`
	MaxTurns                  int  `json:"maxTurns"`
	MaxAccumulatedTurnMinutes int  `json:"maxAccumulatedTurnMinutes"`
	InactivityEnabled         bool `json:"inactivityEnabled"`
	MaxInactivityMinutes      int  `json:"maxInactivityMinutes"`
}

// StallWatchdogPolicyConfig is the optional on-disk representation. Pointers
// distinguish an omitted field in an older workspace.json from an explicit
// value while the policy is migrated to the default-enabled 30-minute setting.
type StallWatchdogPolicyConfig struct {
	Enabled        *bool `json:"enabled,omitempty"`
	TimeoutMinutes *int  `json:"timeoutMinutes,omitempty"`
}

// StallWatchdogPolicy is the fully resolved Workspace policy applied to every
// resource runtime, including Workspace, Projects, Tasks, and Scheduler.
type StallWatchdogPolicy struct {
	Enabled        bool `json:"enabled"`
	TimeoutMinutes int  `json:"timeoutMinutes"`
}

type AgentBinding struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// ResourceAgentDefaults selects the Agent binding applied to newly created
// Projects and Tasks in this Workspace. The Workspace's own binding lives in
// Config.AgentBinding and always initializes to the default Profile.
type ResourceAgentDefaults struct {
	Project AgentBinding `json:"project"`
	Task    AgentBinding `json:"task"`
}

// UnmarshalJSON accepts both the structured binding form and the legacy
// profile-name string form used before resource defaults could target an
// Agent directly. The removed workspace entry is accepted and ignored.
func (defaults *ResourceAgentDefaults) UnmarshalJSON(data []byte) error {
	var raw struct {
		Workspace json.RawMessage `json:"workspace"`
		Project   json.RawMessage `json:"project"`
		Task      json.RawMessage `json:"task"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	decode := func(data []byte, target *AgentBinding) error {
		if len(data) == 0 || string(data) == "null" {
			return nil
		}
		var legacy string
		if err := json.Unmarshal(data, &legacy); err == nil {
			name := strings.TrimSpace(legacy)
			if name == "" {
				return nil
			}
			*target = AgentBinding{Kind: "profile", Name: name}
			return nil
		}
		return json.Unmarshal(data, target)
	}
	if err := decode(raw.Project, &defaults.Project); err != nil {
		return err
	}
	return decode(raw.Task, &defaults.Task)
}

type WorkspaceRuntimeConfig struct {
	InstanceID          string                `json:"instanceId"`
	AgentBinding        AgentBinding          `json:"agentBinding"`
	ResourceDefaults    ResourceAgentDefaults `json:"resourceDefaults"`
	GenerationPolicy    GenerationPolicy      `json:"generationPolicy"`
	StallWatchdogPolicy StallWatchdogPolicy   `json:"stallWatchdogPolicy"`
}

type ResourceMeta struct {
	SchemaVersion int          `json:"schemaVersion"`
	ID            string       `json:"id"`
	Type          string       `json:"type"`
	Title         string       `json:"title"`
	CreatedAt     string       `json:"createdAt"`
	UpdatedAt     string       `json:"updatedAt"`
	AgentBinding  AgentBinding `json:"agentBinding,omitempty"`
}

type Project struct {
	ResourceMeta
	Description string `json:"description,omitempty"`
	// TaskDefault optionally overrides the Workspace's new-Task default
	// binding for Tasks created in this Project. An empty binding means the
	// Project inherits the Workspace default.
	TaskDefault AgentBinding `json:"taskDefault,omitempty"`
}

type Task struct {
	ResourceMeta
	Parent         string              `json:"parent"`
	Description    string              `json:"description,omitempty"`
	Template       *TaskTemplateSource `json:"template,omitempty"`
	State          TaskState           `json:"state,omitempty"`
	StateNote      string              `json:"stateNote,omitempty"`
	StateUpdatedAt string              `json:"stateUpdatedAt,omitempty"`
	// Path is populated on create responses but is not persisted in task.json.
	Path string `json:"path,omitempty"`
}

type TaskState string

const (
	TaskStateNotStarted TaskState = "not_started"
	TaskStateInProgress TaskState = "in_progress"
	TaskStateWaiting    TaskState = "waiting"
	TaskStateBlocked    TaskState = "blocked"
	TaskStatePaused     TaskState = "paused"
	TaskStateCompleted  TaskState = "completed"
	TaskStateError      TaskState = "error"
)

func IsTaskState(value TaskState) bool {
	switch value {
	case TaskStateNotStarted, TaskStateInProgress, TaskStateWaiting, TaskStateBlocked, TaskStatePaused, TaskStateCompleted, TaskStateError:
		return true
	default:
		return false
	}
}

func IsAgentTaskState(value TaskState) bool {
	switch value {
	case TaskStateWaiting, TaskStateBlocked, TaskStatePaused, TaskStateCompleted:
		return true
	default:
		return false
	}
}

type TaskTemplateSource struct {
	Name          string `json:"name"`
	SchemaVersion int    `json:"schemaVersion"`
	Digest        string `json:"digest"`
}

type Resource interface {
	resourceMeta() *ResourceMeta
}

func (project *Project) resourceMeta() *ResourceMeta { return &project.ResourceMeta }
func (task *Task) resourceMeta() *ResourceMeta       { return &task.ResourceMeta }

func (project Project) MarshalJSON() ([]byte, error) {
	type projectJSON struct {
		ResourceMeta
		Parent      *string      `json:"parent"`
		Description string       `json:"description,omitempty"`
		TaskDefault AgentBinding `json:"taskDefault,omitempty"`
	}
	return json.Marshal(projectJSON{ResourceMeta: project.ResourceMeta, Description: project.Description, TaskDefault: project.TaskDefault})
}

func (project *Project) UnmarshalJSON(data []byte) error {
	type projectJSON struct {
		ResourceMeta
		Parent      *string      `json:"parent"`
		Description string       `json:"description,omitempty"`
		TaskDefault AgentBinding `json:"taskDefault,omitempty"`
	}
	var decoded projectJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if decoded.Parent != nil {
		return fmt.Errorf("project parent must be null")
	}
	project.ResourceMeta = decoded.ResourceMeta
	project.Description = decoded.Description
	project.TaskDefault = decoded.TaskDefault
	return nil
}

// TaskRepo describes one Git worktree discovered under a Task's worktree/
// directory. It is derived from Git metadata on read and is never persisted
// in task.json.
type TaskRepo struct {
	Name         string `json:"name"`
	RepoPath     string `json:"repoPath,omitempty"`
	BarePath     string `json:"barePath,omitempty"`
	WorktreePath string `json:"worktreePath"`
	Branch       string `json:"branch"`
	TargetBranch string `json:"targetBranch"`
}
