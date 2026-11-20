package app

import (
	"os"
	"path/filepath"
)

func (w *Workspace) HasCurrentTurnGuidance() (bool, error) {
	current := false
	err := withWorkspaceMutationLock(w.root, func() error {
		language, err := workspaceLanguage(w.root)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(w.root, "AGENTS.md"))
		if err != nil {
			return err
		}
		content := string(data)
		start, end, found, err := managedBlockBounds(content)
		if err != nil {
			return err
		}
		current = found && content[start:end] == puaPromptBlock(language)
		return nil
	})
	return current, err
}

func (w *Workspace) RefreshWorkspaceGuidance() error {
	if err := w.require(); err != nil {
		return err
	}
	return withWorkspaceMutationLock(w.root, func() error {
		path := filepath.Join(w.root, "AGENTS.md")
		if err := requireRegularOrMissing(path); err != nil {
			return err
		}
		language, err := workspaceLanguage(w.root)
		if err != nil {
			return err
		}
		return updateAgentsMDWithBlock(path, puaPromptBlock(language))
	})
}

// RefreshTurnGuidance updates only managed instructions, preserving custom text.
// It is called at new-protocol input boundaries, never to change an active Turn.
func (w *Workspace) RefreshTurnGuidance(resourceID string) error {
	return withWorkspaceMutationLock(w.root, func() error {
		language, err := workspaceLanguage(w.root)
		if err != nil {
			return err
		}
		if err := updateAgentsMD(filepath.Join(w.root, "AGENTS.md"), language); err != nil {
			return err
		}
		if resourceID == "workspace" {
			return nil
		}
		if resourceID == SchedulerResourceID {
			_, err := ensureSchedulerLocked(w.root, language)
			return err
		}
		path, resource, err := loadResource(w.root, resourceID)
		if err != nil {
			return err
		}
		return updateTaskAgentsMD(w.root, path, resource, language)
	})
}
