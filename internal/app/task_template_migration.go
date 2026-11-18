package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RemoveLegacyTaskTemplates removes the former Project template storage and
// Task source metadata. Serve calls it for owned Workspaces before serving them.
func (w *Workspace) RemoveLegacyTaskTemplates() error {
	if err := w.require(); err != nil {
		return err
	}
	return withWorkspaceMutationLock(w.root, func() error {
		root, err := os.OpenRoot(w.root)
		if err != nil {
			return err
		}
		defer root.Close()

		projectDirs, err := templateMigrationResourceDirs(w.root)
		if err != nil {
			return err
		}
		projects, err := readProjectEntriesInDirs(projectDirs)
		if err != nil {
			return err
		}
		for _, project := range projects {
			if err := removeProjectTaskTemplates(root, w.root, project); err != nil {
				return fmt.Errorf("retire templates for %s: %w", project.Project.ID, err)
			}
		}
		return refreshTemplateMigrationGuidance(w.root)
	})
}

func templateMigrationResourceDirs(parent string) ([]string, error) {
	dirs := []string{parent}
	archive := filepath.Join(parent, archiveDir)
	info, err := os.Lstat(archive)
	if os.IsNotExist(err) {
		return dirs, nil
	}
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		dirs = append(dirs, archive)
	}
	return dirs, nil
}

func removeProjectTaskTemplates(root *os.Root, workspacePath string, project projectListEntry) error {
	dirs, err := templateMigrationResourceDirs(project.Path)
	if err != nil {
		return err
	}
	tasks, err := readTaskEntriesInDirs(dirs, projectTaskName(project.Project.ID))
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if err := removeTaskTemplateSource(filepath.Join(task.Path, taskJSONFile)); err != nil {
			return fmt.Errorf("clean task %s metadata: %w", task.Task.ID, err)
		}
	}

	templates := filepath.Join(relPath(workspacePath, project.Path), "templates")
	if _, err := root.Lstat(templates); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := root.RemoveAll(templates); err != nil {
		return err
	}
	return syncDirectory(project.Path)
}

func removeTaskTemplateSource(path string) error {
	content, mode, err := readRegularDocument(path)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(content, &fields); err != nil {
		return err
	}
	if _, ok := fields["template"]; !ok {
		return nil
	}
	delete(fields, "template")
	updated, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	return writeDocumentAtomic(path, append(updated, '\n'), mode)
}

func refreshTemplateMigrationGuidance(root string) error {
	path := filepath.Join(root, "AGENTS.md")
	info, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if info != nil && !info.Mode().IsRegular() {
		return fmt.Errorf("Workspace guidance must be a regular file: %s", path)
	}
	language, err := workspaceLanguage(root)
	if err != nil {
		return err
	}
	return updateAgentsMDWithBlock(path, puaPromptBlock(language))
}
