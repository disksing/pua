package app

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SetTaskState persists the workflow state of an open Task. Authorization for
// server-owned versus Agent-owned states belongs to the caller.
func (w *Workspace) SetTaskState(id string, state TaskState, note string) (Task, error) {
	return w.SetTaskStateContext(context.Background(), id, state, note)
}

func (w *Workspace) SetTaskStateContext(ctx context.Context, id string, state TaskState, note string) (Task, error) {
	if err := w.require(); err != nil {
		return Task{}, err
	}
	id = strings.TrimSpace(id)
	note = strings.TrimSpace(note)
	if !IsTaskState(state) {
		return Task{}, &APIError{Operation: "set task state", Kind: "state", Workspace: w.root, ResourceID: id, Err: fmt.Errorf("invalid task state %q", state)}
	}
	if (state == TaskStateWaiting || state == TaskStateBlocked || state == TaskStatePaused) && note == "" {
		return Task{}, &APIError{Operation: "set task state", Kind: "state", Workspace: w.root, ResourceID: id, Err: fmt.Errorf("state %q requires a note", state)}
	}
	if strings.ContainsAny(note, "\r\n") {
		return Task{}, &APIError{Operation: "set task state", Kind: "state", Workspace: w.root, ResourceID: id, Err: fmt.Errorf("task state note must be a single line")}
	}
	var result Task
	err := withWorkspaceFileLock(ctx, w.root, workspaceMutationLockFile, func() error {
		path, resource, err := loadOpenResource(w.root, id)
		if err != nil {
			return err
		}
		task, ok := resource.(*Task)
		if !ok {
			return fmt.Errorf("resource %s is not a task", id)
		}
		now := time.Now().Format(time.RFC3339Nano)
		task.State = state
		task.StateNote = note
		task.StateUpdatedAt = now
		task.UpdatedAt = now
		if err := writeResourceMetadata(path, task); err != nil {
			return err
		}
		result = *task
		return nil
	})
	if err != nil {
		return Task{}, &APIError{Operation: "set task state", Kind: "state", Workspace: w.root, ResourceID: id, Err: err}
	}
	return result, nil
}
