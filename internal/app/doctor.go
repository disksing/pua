package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/disksing/pua/internal/localize"
)

const (
	DoctorSeverityError   = "error"
	DoctorSeverityWarning = "warning"
)

// DoctorAgent describes one AgentHub catalog entry used by the optional
// binding check. The filesystem checks never require AgentHub connectivity.
type DoctorAgent struct {
	Name              string `json:"name"`
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailableReason,omitempty"`
}

// DoctorProfile describes one PUA Profile and its configured Agent target.
type DoctorProfile struct {
	Key       string `json:"key"`
	AgentName string `json:"agentName"`
}

// DoctorBindingCatalog is nil when dependency checks could not be completed.
type DoctorBindingCatalog struct {
	Profiles []DoctorProfile `json:"profiles"`
	Agents   []DoctorAgent   `json:"agents"`
}

type DoctorOptions struct {
	BindingCatalog *DoctorBindingCatalog
	BindingError   string
	// ServiceError is populated by the service supervisor integration when a
	// Workspace's versioned service graph cannot be loaded or validated. The
	// app package keeps this as text so it does not depend on internal/serve.
	ServiceError string
}

type DoctorIssue struct {
	Severity   string `json:"severity"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	Path       string `json:"path,omitempty"`
	ResourceID string `json:"resourceId,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

type DoctorSummary struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
}

type DoctorReport struct {
	Workspace string        `json:"workspace"`
	Language  string        `json:"language"`
	CheckedAt string        `json:"checkedAt"`
	Complete  bool          `json:"complete"`
	Summary   DoctorSummary `json:"summary"`
	Issues    []DoctorIssue `json:"issues"`
}

type doctorScanner struct {
	root             string
	language         string
	languageKnown    bool
	options          DoctorOptions
	report           DoctorReport
	openResources    map[string]string
	resourceBindings []doctorResourceBinding
	seenResources    map[string]string
}

type doctorResourceBinding struct {
	resourceID string
	path       string
	binding    AgentBinding
}

type doctorTemplate struct {
	valid bool
}

// CheckWorkspace performs a read-only inspection of one explicit Workspace.
// It deliberately scans canonical open-resource directory positions instead
// of using Tree, whose ordinary read path skips malformed resources.
func CheckWorkspace(root string, options DoctorOptions) (DoctorReport, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return DoctorReport{}, errors.New("workspace root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return DoctorReport{}, err
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		return DoctorReport{}, err
	}
	if !info.IsDir() {
		return DoctorReport{}, fmt.Errorf("workspace root is not a directory: %s", abs)
	}
	scanner := &doctorScanner{
		root:          abs,
		language:      defaultLanguage,
		options:       options,
		openResources: map[string]string{"workspace": ".", SchedulerResourceID: schedulerDir},
		seenResources: make(map[string]string),
		report: DoctorReport{
			Workspace: abs,
			Language:  defaultLanguage,
			CheckedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Complete:  true,
			Issues:    []DoctorIssue{},
		},
	}
	scanner.scanWorkspaceConfig()
	scanner.scanWorkspaceFiles()
	scanner.scanProjects()
	scanner.scanScheduler()
	scanner.scanBindings()
	if strings.TrimSpace(options.ServiceError) != "" {
		scanner.issue(DoctorSeverityError, "service_configuration_invalid", filepath.Join(".pua", "services"), "workspace", options.ServiceError, "Repair the service definition or remove the invalid service before restarting pua serve.")
	}
	scanner.finish()
	return scanner.report, nil
}

func (s *doctorScanner) finish() {
	s.report.Language = s.language
	for i := range s.report.Issues {
		issue := &s.report.Issues[i]
		kind := ""
		if strings.HasPrefix(issue.Code, "template_") {
			kind = "template_validation"
		}
		data := map[string]string{"Code": issue.Code, "Kind": kind, "Original": issue.Message}
		issue.Message = strings.TrimSpace(localize.MustRender(s.language, "doctor-message.txt", data))
		if issue.Suggestion != "" {
			data["Original"] = issue.Suggestion
			issue.Suggestion = strings.TrimSpace(localize.MustRender(s.language, "doctor-suggestion.txt", data))
		}
	}
	severityRank := func(value string) int {
		if value == DoctorSeverityError {
			return 0
		}
		return 1
	}
	sort.SliceStable(s.report.Issues, func(i, j int) bool {
		left, right := s.report.Issues[i], s.report.Issues[j]
		if severityRank(left.Severity) != severityRank(right.Severity) {
			return severityRank(left.Severity) < severityRank(right.Severity)
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.ResourceID != right.ResourceID {
			return left.ResourceID < right.ResourceID
		}
		return left.Code < right.Code
	})
	for _, issue := range s.report.Issues {
		switch issue.Severity {
		case DoctorSeverityError:
			s.report.Summary.Errors++
		case DoctorSeverityWarning:
			s.report.Summary.Warnings++
		}
	}
}

func (s *doctorScanner) issue(severity, code, path, resourceID, message, suggestion string) {
	s.report.Issues = append(s.report.Issues, DoctorIssue{
		Severity: severity, Code: code, Path: filepath.ToSlash(path), ResourceID: resourceID,
		Message: message, Suggestion: suggestion,
	})
}

func (s *doctorScanner) scanWorkspaceConfig() {
	canonical := filepath.Join(s.root, workspaceConfigFile)
	if !pathExists(canonical) {
		s.issue(DoctorSeverityError, "workspace_config_missing", workspaceConfigFile, "workspace",
			"workspace.json is missing", "Restore workspace.json from backup or reconstruct it before using PUA.")
		return
	}
	if !s.requireRegularFile(canonical, workspaceConfigFile, "workspace", DoctorSeverityError) {
		return
	}
	var config Config
	if err := readDoctorJSON(canonical, &config); err != nil {
		s.issue(DoctorSeverityError, "workspace_config_invalid", workspaceConfigFile, "workspace",
			"workspace.json cannot be read: "+err.Error(), "Repair the JSON manually or restore it from backup.")
		return
	}
	versionSupported := config.Version == 1
	if !versionSupported {
		s.issue(DoctorSeverityError, "workspace_version_unsupported", workspaceConfigFile, "workspace",
			fmt.Sprintf("workspace version %d is unsupported; expected 1", config.Version), "Run the matching PUA migration after reviewing the file.")
	}
	language, err := NormalizeLanguage(config.Language)
	if err != nil {
		s.issue(DoctorSeverityError, "workspace_language_invalid", workspaceConfigFile, "workspace", err.Error(), "Set language to en or zh-CN.")
	} else {
		s.language = language
		s.languageKnown = versionSupported
	}
	if strings.TrimSpace(config.InstanceID) == "" {
		s.issue(DoctorSeverityError, "workspace_instance_id_missing", workspaceConfigFile, "workspace",
			"workspace instanceId is missing", "Run pua migrate after backing up the Workspace.")
	}
	if binding, err := NormalizeAgentBinding(config.AgentBinding); err != nil {
		s.issue(DoctorSeverityError, "agent_binding_invalid", workspaceConfigFile, "workspace", err.Error(), "Select a valid Profile or Agent binding.")
	} else {
		s.resourceBindings = append(s.resourceBindings, doctorResourceBinding{resourceID: "workspace", path: workspaceConfigFile, binding: binding})
	}
	for kind, binding := range map[string]AgentBinding{
		"project": config.ResourceDefaults.Project,
		"task":    config.ResourceDefaults.Task,
	} {
		binding = normalizeDefaultBinding(binding)
		s.resourceBindings = append(s.resourceBindings, doctorResourceBinding{
			resourceID: "default:" + kind, path: workspaceConfigFile, binding: binding,
		})
	}
}

func (s *doctorScanner) scanWorkspaceFiles() {
	s.checkManagedFile(filepath.Join(s.root, "AGENTS.md"), "AGENTS.md", "workspace", puaPromptBlock(s.language))
	s.checkOptionalMarkdown(filepath.Join(s.root, wikiDir, "index.md"), filepath.Join(wikiDir, "index.md"), "workspace")
	for _, dir := range []string{reposDir, wikiDir} {
		s.requireDirectory(filepath.Join(s.root, dir), dir, "workspace", DoctorSeverityWarning)
	}
}

func (s *doctorScanner) scanProjects() {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		s.issue(DoctorSeverityError, "workspace_unreadable", ".", "workspace", err.Error(), "Restore read access to the Workspace.")
		return
	}
	for _, entry := range entries {
		if entry.Name() == archiveDir {
			continue
		}
		path := filepath.Join(s.root, entry.Name())
		candidate := topProjectDirName.MatchString(entry.Name())
		if !candidate && entry.IsDir() {
			candidate = pathExists(filepath.Join(path, projectJSONFile))
		}
		if !candidate {
			continue
		}
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			s.issue(DoctorSeverityError, "project_path_invalid", entry.Name(), "", "project path must be a real directory", "Replace the path with a regular directory after reviewing its contents.")
			continue
		}
		s.scanProject(path, entry.Name())
	}
}

func (s *doctorScanner) scanProject(path, name string) {
	rel := relPath(s.root, path)
	metadata := filepath.Join(path, projectJSONFile)
	if !pathExists(metadata) {
		s.issue(DoctorSeverityError, "project_metadata_missing", filepath.Join(rel, projectJSONFile), "",
			"project directory is missing project.json", "Restore project.json from backup or reconstruct the Project metadata.")
		return
	}
	if pathExists(filepath.Join(path, taskJSONFile)) {
		s.issue(DoctorSeverityError, "resource_metadata_conflict", rel, "", "project directory also contains task.json", "Remove the incorrect metadata file after reviewing both files.")
	}
	if !s.requireRegularFile(metadata, filepath.Join(rel, projectJSONFile), "", DoctorSeverityError) {
		return
	}
	var project Project
	if err := readProjectAtDir(path, &project); err != nil {
		s.issue(DoctorSeverityError, "project_metadata_invalid", filepath.Join(rel, projectJSONFile), "", err.Error(), "Repair project.json manually or restore it from backup.")
		return
	}
	resourceID := project.ID
	if !resourceDirNameMatches(name, &project) {
		s.issue(DoctorSeverityError, "project_directory_mismatch", rel, resourceID,
			"project directory name does not match its resource id", "Rename the directory to the canonical projectN[-slug] form.")
	}
	s.registerResource(resourceID, rel)
	s.openResources[resourceID] = rel
	s.resourceBindings = append(s.resourceBindings, doctorResourceBinding{resourceID: resourceID, path: filepath.Join(rel, projectJSONFile), binding: project.AgentBinding})
	if strings.TrimSpace(project.TaskDefault.Name) != "" {
		s.resourceBindings = append(s.resourceBindings, doctorResourceBinding{resourceID: "default:" + resourceID + ".task", path: filepath.Join(rel, projectJSONFile), binding: project.TaskDefault})
	}
	s.checkResourceTimestamps(project.CreatedAt, project.UpdatedAt, filepath.Join(rel, projectJSONFile), resourceID)
	s.checkOptionalMarkdown(filepath.Join(path, projectMDFile), filepath.Join(rel, projectMDFile), resourceID)
	s.checkManagedFile(filepath.Join(path, "AGENTS.md"), filepath.Join(rel, "AGENTS.md"), resourceID, taskAgentsBlock(&project, s.language))
	templates := s.scanTemplates(path, project)
	s.scanTasks(path, project, templates)
}

func (s *doctorScanner) scanTasks(projectPath string, project Project, templates map[string]doctorTemplate) {
	entries, err := os.ReadDir(projectPath)
	if err != nil {
		s.issue(DoctorSeverityError, "project_unreadable", relPath(s.root, projectPath), project.ID, err.Error(), "Restore read access to the Project directory.")
		return
	}
	for _, entry := range entries {
		if entry.Name() == archiveDir {
			continue
		}
		path := filepath.Join(projectPath, entry.Name())
		candidate := taskDirName.MatchString(entry.Name())
		if !candidate && entry.IsDir() {
			candidate = pathExists(filepath.Join(path, taskJSONFile))
		}
		if !candidate {
			continue
		}
		rel := relPath(s.root, path)
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			s.issue(DoctorSeverityError, "task_path_invalid", rel, "", "task path must be a real directory", "Replace the path with a regular directory after reviewing its contents.")
			continue
		}
		s.scanTask(path, entry.Name(), project, templates)
	}
}

func (s *doctorScanner) scanTask(path, name string, project Project, templates map[string]doctorTemplate) {
	rel := relPath(s.root, path)
	metadata := filepath.Join(path, taskJSONFile)
	if !pathExists(metadata) {
		s.issue(DoctorSeverityError, "task_metadata_missing", filepath.Join(rel, taskJSONFile), "",
			"task directory is missing task.json", "Restore task.json from backup or reconstruct the Task metadata.")
		return
	}
	if pathExists(filepath.Join(path, projectJSONFile)) {
		s.issue(DoctorSeverityError, "resource_metadata_conflict", rel, "", "task directory also contains project.json", "Remove the incorrect metadata file after reviewing both files.")
	}
	if !s.requireRegularFile(metadata, filepath.Join(rel, taskJSONFile), "", DoctorSeverityError) {
		return
	}
	var task Task
	if err := readTaskAtDir(path, &task); err != nil {
		s.issue(DoctorSeverityError, "task_metadata_invalid", filepath.Join(rel, taskJSONFile), "", err.Error(), "Repair task.json manually or restore it from backup.")
		return
	}
	resourceID := task.ID
	if !resourceDirNameMatches(name, &task) {
		s.issue(DoctorSeverityError, "task_directory_mismatch", rel, resourceID,
			"task directory name does not match its resource id", "Rename the directory to the canonical taskN[-slug] form.")
	}
	if task.Parent != project.ID {
		s.issue(DoctorSeverityError, "task_parent_mismatch", filepath.Join(rel, taskJSONFile), resourceID,
			fmt.Sprintf("task parent %q does not match containing project %q", task.Parent, project.ID), "Correct parent or move the Task to its canonical Project.")
	}
	s.registerResource(resourceID, rel)
	s.openResources[resourceID] = rel
	s.resourceBindings = append(s.resourceBindings, doctorResourceBinding{resourceID: resourceID, path: filepath.Join(rel, taskJSONFile), binding: task.AgentBinding})
	s.checkResourceTimestamps(task.CreatedAt, task.UpdatedAt, filepath.Join(rel, taskJSONFile), resourceID)
	s.checkOptionalMarkdown(filepath.Join(path, taskMDFile), filepath.Join(rel, taskMDFile), resourceID)
	s.checkManagedFile(filepath.Join(path, "AGENTS.md"), filepath.Join(rel, "AGENTS.md"), resourceID, taskAgentsBlock(&task, s.language))
	s.scanTaskRepos(task, path, rel)
	if task.Template != nil {
		template, ok := templates[task.Template.Name]
		switch {
		case !ok:
			s.issue(DoctorSeverityWarning, "task_template_missing", filepath.Join(rel, taskJSONFile), resourceID,
				fmt.Sprintf("recorded template %q no longer exists", task.Template.Name), "Restore the template if future reproducibility matters.")
		case !template.valid:
			s.issue(DoctorSeverityWarning, "task_template_invalid", filepath.Join(rel, taskJSONFile), resourceID,
				fmt.Sprintf("recorded template %q is invalid", task.Template.Name), "Repair the Project template.")
		}
	}
}

func (s *doctorScanner) scanTemplates(projectPath string, project Project) map[string]doctorTemplate {
	result := make(map[string]doctorTemplate)
	dir := filepath.Join(projectPath, "templates")
	relDir := relPath(s.root, dir)
	if !s.requireDirectory(dir, relDir, project.ID, DoctorSeverityWarning) {
		return result
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		s.issue(DoctorSeverityWarning, "templates_unreadable", relDir, project.ID, err.Error(), "Restore read access to the templates directory.")
		return result
	}
	for _, entry := range entries {
		if strings.ToLower(filepath.Ext(entry.Name())) != ".md" {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		path := filepath.Join(dir, entry.Name())
		rel := relPath(s.root, path)
		if !safeTemplateName(name) {
			s.issue(DoctorSeverityError, "template_name_invalid", rel, project.ID, "template filename is not safe", "Rename the template using letters, digits, dots, underscores, or hyphens.")
			result[name] = doctorTemplate{}
			continue
		}
		if !s.requireRegularFile(path, rel, project.ID, DoctorSeverityError) {
			result[name] = doctorTemplate{}
			continue
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			s.issue(DoctorSeverityError, "template_unreadable", rel, project.ID, readErr.Error(), "Restore read access to the template.")
			result[name] = doctorTemplate{}
			continue
		}
		template := parseTaskTemplate(name, rel, string(data))
		valid := len(template.Errors) == 0
		result[name] = doctorTemplate{valid: valid}
		for _, issue := range append(template.Errors, template.Warnings...) {
			severity := DoctorSeverityWarning
			if issue.Severity == "error" {
				severity = DoctorSeverityError
			}
			message := issue.Message
			if issue.Path != "" {
				message = issue.Path + ": " + message
			}
			s.issue(severity, "template_"+issue.Code, rel, project.ID, message, "Repair the template before using it to create another Task.")
		}
	}
	return result
}

// scanTaskRepos checks the worktrees discovered under a Task's worktree/
// directory. Worktree metadata is derived from Git, so the checks focus on
// whether the source repository still exists inside the Workspace.
func (s *doctorScanner) scanTaskRepos(task Task, taskPath, taskRel string) {
	for _, repo := range discoverTaskRepos(s.root, taskPath) {
		storage := strings.TrimSpace(taskRepoStoragePath(repo))
		if storage == "" {
			s.issue(DoctorSeverityWarning, "task_worktree_storage_external", repo.WorktreePath, task.ID, fmt.Sprintf("worktree %q source repository is outside the Workspace or could not be determined", repo.Name), "Keep Task worktrees based on repositories under repos/.")
			continue
		}
		abs, ok := s.safeReferencedPath(storage)
		if !ok {
			s.issue(DoctorSeverityError, "task_worktree_storage_unsafe", repo.WorktreePath, task.ID, fmt.Sprintf("worktree %q source repository path is outside the Workspace", repo.Name), "Keep Task worktrees based on repositories under repos/.")
		} else if !isDir(abs) {
			s.issue(DoctorSeverityWarning, "task_worktree_storage_not_found", storage, task.ID, fmt.Sprintf("worktree %q source repository does not exist", repo.Name), "Restore the repository or recreate the worktree.")
		} else if (repo.BarePath != "" && !pathExists(filepath.Join(abs, "HEAD"))) || (repo.RepoPath != "" && !isGitCheckout(abs)) {
			s.issue(DoctorSeverityWarning, "task_worktree_storage_invalid", storage, task.ID, fmt.Sprintf("worktree %q source repository does not look like a Git repository", repo.Name), "Repair or replace the repository checkout.")
		}
	}
}

func (s *doctorScanner) scanScheduler() {
	dir := filepath.Join(s.root, schedulerDir)
	if !s.requireDirectory(dir, schedulerDir, SchedulerResourceID, DoctorSeverityError) {
		return
	}
	jsonPath := schedulerJSONPath(s.root)
	if !s.requireRegularFile(jsonPath, filepath.Join(schedulerDir, schedulerJSONFile), SchedulerResourceID, DoctorSeverityError) {
		return
	}
	config, err := readSchedulerJSON(jsonPath)
	if err != nil {
		s.issue(DoctorSeverityError, "scheduler_config_invalid", filepath.Join(schedulerDir, schedulerJSONFile), SchedulerResourceID, err.Error(), "Repair scheduler.json manually or restore it from backup.")
		return
	}
	s.resourceBindings = append(s.resourceBindings, doctorResourceBinding{resourceID: SchedulerResourceID, path: filepath.Join(schedulerDir, schedulerJSONFile), binding: config.AgentBinding})
	for _, schedule := range config.Schedules {
		target := strings.TrimSpace(schedule.Target)
		if target == "workspace" || target == SchedulerResourceID {
			continue
		}
		if _, ok := s.openResources[target]; !ok {
			s.issue(DoctorSeverityError, "schedule_target_missing", filepath.Join(schedulerDir, schedulerJSONFile), SchedulerResourceID,
				fmt.Sprintf("schedule %s targets missing or archived resource %q", schedule.ID, target), "Update or remove the schedule.")
		}
	}
	s.checkOptionalMarkdown(filepath.Join(dir, schedulerMarkdownFile), filepath.Join(schedulerDir, schedulerMarkdownFile), SchedulerResourceID)
	s.checkManagedFile(filepath.Join(dir, "AGENTS.md"), filepath.Join(schedulerDir, "AGENTS.md"), SchedulerResourceID, schedulerAgentsBlock(s.language))
}

func (s *doctorScanner) scanBindings() {
	if s.options.BindingCatalog == nil {
		s.report.Complete = false
		message := strings.TrimSpace(s.options.BindingError)
		if message == "" {
			message = "Agent/Profile binding checks were skipped because no PUA Server catalog was available"
		}
		s.issue(DoctorSeverityWarning, "agent_catalog_unavailable", "", "", message, "Start or connect to the owning pua serve process and run doctor again.")
		return
	}
	profiles := make(map[string]DoctorProfile)
	agents := make(map[string]DoctorAgent)
	for _, profile := range s.options.BindingCatalog.Profiles {
		profiles[strings.ToLower(strings.TrimSpace(profile.Key))] = profile
	}
	for _, agent := range s.options.BindingCatalog.Agents {
		agents[strings.ToLower(strings.TrimSpace(agent.Name))] = agent
	}
	for _, ref := range s.resourceBindings {
		binding, err := NormalizeAgentBinding(ref.binding)
		if err != nil {
			continue
		}
		switch binding.Kind {
		case "agent":
			s.checkAgentTarget(agents, binding.Name, ref, "agent_binding_target_missing")
		case "profile":
			profile, ok := profiles[strings.ToLower(binding.Name)]
			if !ok || strings.TrimSpace(profile.AgentName) == "" {
				s.issue(DoctorSeverityWarning, "agent_profile_missing", ref.path, ref.resourceID,
					fmt.Sprintf("Agent Profile %q cannot be resolved and will use fallback behavior", binding.Name), "Select an existing Profile or Agent binding.")
				continue
			}
			s.checkAgentTarget(agents, profile.AgentName, ref, "agent_profile_target_missing")
		}
	}
}

func (s *doctorScanner) checkAgentTarget(agents map[string]DoctorAgent, name string, ref doctorResourceBinding, missingCode string) {
	agent, ok := agents[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		s.issue(DoctorSeverityError, missingCode, ref.path, ref.resourceID, fmt.Sprintf("configured Agent %q is not present in the AgentHub catalog", name), "Select an existing available Agent.")
		return
	}
	if !agent.Available {
		reason := strings.TrimSpace(agent.UnavailableReason)
		if reason == "" {
			reason = "the Agent is unavailable"
		}
		s.issue(DoctorSeverityError, "agent_target_unavailable", ref.path, ref.resourceID, fmt.Sprintf("configured Agent %q is unavailable: %s", agent.Name, reason), "Repair the Provider or select another Agent.")
	}
}

func (s *doctorScanner) checkManagedFile(path, rel, resourceID, expected string) {
	if !s.requireRegularFile(path, rel, resourceID, DoctorSeverityError) {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		s.issue(DoctorSeverityError, "agents_file_unreadable", rel, resourceID, err.Error(), "Restore read access to AGENTS.md.")
		return
	}
	content := string(data)
	start, end, found, boundsErr := managedBlockBounds(content)
	if !found && boundsErr == nil {
		s.issue(DoctorSeverityError, "agents_managed_section_missing", rel, resourceID, "PUA managed AGENTS.md section is missing", "Run pua migrate after reviewing local instructions.")
		return
	}
	if boundsErr != nil {
		s.issue(DoctorSeverityError, "agents_managed_markers_invalid", rel, resourceID, "PUA managed AGENTS.md markers are duplicated, incomplete, or out of order", "Repair the markers, then run pua migrate.")
		return
	}
	if !s.languageKnown {
		return
	}
	if content[start:end] != expected {
		s.issue(DoctorSeverityError, "agents_managed_section_modified", rel, resourceID, "PUA managed AGENTS.md section is missing current content or has been modified", "Run pua migrate after reviewing local instructions outside the managed section.")
	}
}

func (s *doctorScanner) checkOptionalMarkdown(path, rel, resourceID string) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		s.issue(DoctorSeverityWarning, "markdown_file_missing", rel, resourceID, filepath.Base(rel)+" is missing", "Restore the file if the resource context is still needed.")
		return
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		s.issue(DoctorSeverityWarning, "markdown_file_invalid", rel, resourceID, filepath.Base(rel)+" must be a regular file", "Replace the path with a regular Markdown file.")
	}
}

func (s *doctorScanner) checkResourceTimestamps(createdAt, updatedAt, path, resourceID string) {
	created, createdErr := time.Parse(time.RFC3339Nano, createdAt)
	updated, updatedErr := time.Parse(time.RFC3339Nano, updatedAt)
	if createdErr != nil || updatedErr != nil {
		s.issue(DoctorSeverityError, "resource_timestamp_invalid", path, resourceID, "createdAt and updatedAt must be RFC3339 timestamps", "Correct the resource timestamps.")
		return
	}
	if updated.Before(created) {
		s.issue(DoctorSeverityWarning, "resource_timestamp_order_invalid", path, resourceID, "updatedAt is earlier than createdAt", "Correct the resource timestamps if this was not intentional.")
	}
}

func (s *doctorScanner) registerResource(id, path string) {
	if previous, ok := s.seenResources[id]; ok {
		s.issue(DoctorSeverityError, "resource_id_duplicate", path, id, fmt.Sprintf("resource id is also used by %s", previous), "Keep exactly one open resource with this id.")
		return
	}
	s.seenResources[id] = path
}

func (s *doctorScanner) requireRegularFile(path, rel, resourceID, severity string) bool {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		code := "required_file_missing"
		if filepath.Base(path) == "AGENTS.md" {
			code = "agents_file_missing"
		}
		s.issue(severity, code, rel, resourceID, filepath.Base(path)+" is missing", "Restore the required file.")
		return false
	}
	if err != nil {
		s.issue(severity, "required_file_unreadable", rel, resourceID, err.Error(), "Restore access to the required file.")
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		s.issue(severity, "required_file_invalid", rel, resourceID, filepath.Base(path)+" must be a regular file and may not be a symbolic link", "Replace it with a regular file.")
		return false
	}
	return true
}

func (s *doctorScanner) requireDirectory(path, rel, resourceID, severity string) bool {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		s.issue(severity, "required_directory_missing", rel, resourceID, filepath.Base(path)+" directory is missing", "Restore or recreate the directory after reviewing Workspace data.")
		return false
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		s.issue(severity, "required_directory_invalid", rel, resourceID, filepath.Base(path)+" must be a real directory", "Replace the path with a regular directory.")
		return false
	}
	return true
}

func (s *doctorScanner) safeReferencedPath(value string) (string, bool) {
	abs := value
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(s.root, filepath.FromSlash(value))
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(s.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return abs, false
	}
	return abs, true
}

func readDoctorJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return err
	}
	return nil
}
