package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ArchiveWarning records a non-blocking condition observed while archiving.
// Warnings are deliberately descriptive enough for a caller to decide how to
// recover without requiring PUA to mutate source code or Git state.
type ArchiveWarning struct {
	Severity     string `json:"severity"`
	Code         string `json:"code"`
	Message      string `json:"message"`
	ResourceID   string `json:"resourceId,omitempty"`
	Repo         string `json:"repo,omitempty"`
	Path         string `json:"path,omitempty"`
	Branch       string `json:"branch,omitempty"`
	TargetBranch string `json:"targetBranch,omitempty"`
}

func archiveWarning(code, message string) ArchiveWarning {
	return ArchiveWarning{Severity: "warning", Code: code, Message: message}
}

func resourceArchiveDestination(root, resourcePath string, resource Resource) (string, error) {
	meta := resource.resourceMeta()
	if isProject(resource) {
		return filepath.Join(root, archiveDir, filepath.Base(resourcePath)), nil
	}
	if task, ok := resource.(*Task); ok && task.Parent != "" {
		parentPath := filepath.Dir(resourcePath)
		return filepath.Join(parentPath, archiveDir, filepath.Base(resourcePath)), nil
	}
	return "", fmt.Errorf("unsupported resource id for archive: %s", meta.ID)
}

type archiveTaskReference struct {
	Task Task
	Path string
}

func collectProjectArchiveTasks(root, projectPath, destination string, project Project) ([]archiveTaskReference, []ArchiveWarning) {
	entries, err := readTaskEntriesInDirs(
		[]string{projectPath, filepath.Join(projectPath, archiveDir)},
		projectTaskName(project.ID),
	)
	if err != nil {
		warning := archiveWarning("child_tasks_unverifiable", fmt.Sprintf("could not inspect child Tasks of %s before archive: %v; the project directory was still archived", project.ID, err))
		warning.ResourceID = project.ID
		warning.Path = relPath(root, projectPath)
		return nil, []ArchiveWarning{warning}
	}

	refs := make([]archiveTaskReference, 0, len(entries))
	warnings := make([]ArchiveWarning, 0)
	for _, entry := range entries {
		refs = append(refs, archiveTaskReference{Task: entry.Task, Path: filepath.Join(destination, relPath(projectPath, entry.Path))})
		if !isArchivedPath(root, entry.Path) {
			warning := archiveWarning("open_child_task", fmt.Sprintf("open child Task %s is being archived as part of Project %s; its files and worktree are preserved", entry.Task.ID, project.ID))
			warning.ResourceID = entry.Task.ID
			warning.Path = relPath(root, entry.Path)
			warnings = append(warnings, warning)
		}
	}
	return refs, warnings
}

func inspectTaskRepoWorktrees(root, taskPath string, task Task) []ArchiveWarning {
	warnings := make([]ArchiveWarning, 0)
	for _, repo := range discoverTaskRepos(root, taskPath) {
		warningBase := func(code, message string) ArchiveWarning {
			warning := archiveWarning(code, message)
			warning.ResourceID = task.ID
			warning.Repo = repo.Name
			warning.Path = repo.WorktreePath
			warning.Branch = repo.Branch
			warning.TargetBranch = repo.TargetBranch
			return warning
		}

		worktreePath := repo.WorktreePath
		if !filepath.IsAbs(worktreePath) {
			worktreePath = filepath.Join(root, filepath.FromSlash(worktreePath))
		}
		worktreePath = filepath.Clean(worktreePath)
		warningPath := relPath(root, worktreePath)
		if info, err := os.Stat(worktreePath); err != nil {
			warnings = append(warnings, warningBase("worktree_unverifiable", fmt.Sprintf("Task %s repo %q worktree %s could not be inspected: %v; archive preserves the worktree as is", task.ID, repo.Name, warningPath, err)))
			continue
		} else if !info.IsDir() {
			warnings = append(warnings, warningBase("worktree_unverifiable", fmt.Sprintf("Task %s repo %q worktree %s is not a directory; Git state could not be checked", task.ID, repo.Name, warningPath)))
			continue
		}

		gitPath := filepath.Join(worktreePath, ".git")
		if _, err := os.Stat(gitPath); err != nil {
			warnings = append(warnings, warningBase("worktree_unverifiable", fmt.Sprintf("Task %s repo %q worktree %s has no usable .git metadata: %v", task.ID, repo.Name, warningPath, err)))
			continue
		}

		status, err := runGit(worktreePath, "status", "--porcelain", "--untracked-files=all")
		if err != nil {
			warnings = append(warnings, warningBase("git_unverifiable", fmt.Sprintf("could not inspect Git status for Task %s repo %q worktree %s: %s", task.ID, repo.Name, warningPath, gitErrorDetail(status, err))))
		} else if strings.TrimSpace(status) != "" {
			detail := compactGitOutput(status)
			if detail == "" {
				detail = "Git status reported changes"
			}
			warnings = append(warnings, warningBase("dirty_worktree", fmt.Sprintf("Task %s repo %q worktree %s is dirty; archive preserves uncommitted changes (%s)", task.ID, repo.Name, warningPath, detail)))
		}

		target := strings.TrimSpace(repo.TargetBranch)
		if target == "" {
			warnings = append(warnings, warningBase("target_branch_missing", fmt.Sprintf("Task %s repo %q worktree %s target branch could not be determined; merge status could not be checked", task.ID, repo.Name, warningPath)))
			continue
		}
		if output, verifyErr := runGit(worktreePath, "rev-parse", "--verify", target+"^{commit}"); verifyErr != nil {
			warnings = append(warnings, warningBase("target_branch_unverifiable", fmt.Sprintf("Task %s repo %q worktree %s target branch %q could not be resolved: %s", task.ID, repo.Name, warningPath, target, gitErrorDetail(output, verifyErr))))
			continue
		}
		output, mergeErr := runGit(worktreePath, "merge-base", "--is-ancestor", "HEAD", target)
		if mergeErr == nil {
			continue
		}
		if exitErr, ok := mergeErr.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			commits, logErr := runGit(worktreePath, "log", "--oneline", "-n", "5", target+"..HEAD")
			if logErr != nil {
				commits = ""
			}
			detail := compactGitOutput(commits)
			message := fmt.Sprintf("Task %s repo %q worktree %s has commits not merged into target branch %q; archive preserves them", task.ID, repo.Name, warningPath, target)
			if detail != "" {
				message += fmt.Sprintf(" (%s)", detail)
			}
			warnings = append(warnings, warningBase("unmerged_commits", message))
			continue
		}
		warnings = append(warnings, warningBase("git_unverifiable", fmt.Sprintf("could not compare Task %s repo %q worktree %s with target branch %q: %s", task.ID, repo.Name, warningPath, target, gitErrorDetail(output, mergeErr))))
	}
	return warnings
}

func runGit(path string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"--no-optional-locks", "-C", path}, args...)...)
	output, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func gitErrorDetail(output string, err error) string {
	if detail := compactGitOutput(output); detail != "" {
		return detail
	}
	return err.Error()
}

func compactGitOutput(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	const max = 240
	if len(value) > max {
		return value[:max] + "..."
	}
	return value
}

// repairArchivedTaskWorktrees fixes the Git metadata of worktrees that moved
// together with an archived Task. Worktrees are discovered from their new
// location; the source repository under repos/ does not move, so the
// worktree .git file still resolves the repository needed for the repair.
func repairArchivedTaskWorktrees(root, taskPath string, task Task) []ArchiveWarning {
	warnings := make([]ArchiveWarning, 0)
	for _, repo := range discoverTaskRepos(root, taskPath) {
		if err := repairRepoWorktree(root, repo); err != nil {
			warning := archiveWarning("worktree_repair_failed", fmt.Sprintf("archived Task %s repo %q moved to %s but Git worktree repair failed: %v; the archive is complete and can be repaired later", task.ID, repo.Name, relPath(root, taskPath), err))
			warning.ResourceID = task.ID
			warning.Repo = repo.Name
			warning.Path = repo.WorktreePath
			warnings = append(warnings, warning)
		}
	}
	return warnings
}

func sortArchiveWarnings(warnings []ArchiveWarning) []ArchiveWarning {
	if len(warnings) == 0 {
		return nil
	}
	sort.SliceStable(warnings, func(i, j int) bool {
		if warnings[i].ResourceID != warnings[j].ResourceID {
			return warnings[i].ResourceID < warnings[j].ResourceID
		}
		if warnings[i].Repo != warnings[j].Repo {
			return warnings[i].Repo < warnings[j].Repo
		}
		if warnings[i].Code != warnings[j].Code {
			return warnings[i].Code < warnings[j].Code
		}
		return warnings[i].Message < warnings[j].Message
	})
	return warnings
}
