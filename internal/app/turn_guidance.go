package app

import "path/filepath"

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
