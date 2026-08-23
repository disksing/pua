package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/localize"
)

const maxTaskStateRecoveryAttempts = 3

func taskStateContinuationText(language string) string {
	return strings.TrimSpace(localize.MustRender(language, "task-continuation.md", nil))
}

func taskStateContinuationExhaustedNote(language string) string {
	return strings.TrimSpace(localize.MustRender(language, "task-continuation-exhausted.txt", nil))
}

func taskWaitingScheduleText(language, resourceID string) string {
	return strings.TrimSpace(localize.MustRender(language, "task-waiting-schedule.md", map[string]string{"ResourceID": resourceID}))
}

func taskWaitingScheduleExhaustedNote(language string) string {
	return strings.TrimSpace(localize.MustRender(language, "task-waiting-schedule-exhausted.txt", nil))
}

func taskStateContinuationMessageID(resourceID, generationID, chainID, marker string, attempt int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d", resourceID, generationID, chainID, marker, attempt)))
	return "task-state-" + hex.EncodeToString(digest[:12])
}

func taskWaitingScheduleMessageID(resourceID, generationID, chainID, marker string, attempt int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d", resourceID, generationID, chainID, marker, attempt)))
	return "task-waiting-schedule-" + hex.EncodeToString(digest[:12])
}

func taskDetail(workspacePath, resourceID string) (app.ResourceDetailView, bool, error) {
	puaWorkspace, err := app.OpenWorkspace(workspacePath)
	if err != nil {
		return app.ResourceDetailView{}, false, err
	}
	detail, err := puaWorkspace.Resource(resourceID)
	if err != nil {
		return app.ResourceDetailView{}, false, err
	}
	return detail, detail.Type == "task", nil
}

func (m *agentManager) recordTaskStartFailure(workspace serveWorkspace, message resourceMailboxMessage, cause error) (bool, error) {
	if cause == nil || !strings.Contains(normalizedResourceID(message.ResourceID), ".task") || message.GenerationID != "" {
		return false, nil
	}
	updated, err := updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
		current.TaskStartFailureCount++
		if current.TaskStartFailureCount < maxTaskStateRecoveryAttempts {
			return
		}
		now := time.Now().Format(time.RFC3339Nano)
		current.Status = resourceMessageUndeliverable
		current.TerminalAt = now
		current.UpdatedAt = now
		current.LastErrorCode = "task_state_retry_exhausted"
		current.LastError = cause.Error()
	})
	if err != nil || updated.TaskStartFailureCount < maxTaskStateRecoveryAttempts {
		return false, err
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		return false, err
	}
	note := strings.Join(strings.Fields(cause.Error()), " ")
	if _, err := puaWorkspace.SetTaskState(message.ResourceID, app.TaskStateError, note); err != nil {
		return false, err
	}
	return true, nil
}

// prepareTaskWorkChain runs at the durable delivery boundary. Ordinary input
// starts a fresh budget; a generated continuation keeps the current budget.
func (m *agentManager) prepareTaskWorkChain(workspace serveWorkspace, message resourceMailboxMessage, rt *agentRuntime) error {
	if !strings.Contains(normalizedResourceID(message.ResourceID), ".task") || message.Status != resourceMessageQueued {
		return nil
	}
	detail, task, err := taskDetail(workspace.Path, message.ResourceID)
	if err != nil || !task {
		return err
	}
	if message.Type != resourceMessageTypeTaskContinuation {
		if _, err = rt.mutateGeneration(func(record *generationRecord) {
			record.TaskStateChainID = message.ID
			record.TaskStateContinuationCount = 0
			// A fresh external work chain supersedes any terminal marker from
			// the previous Turn. Consume it before exposing in_progress so a
			// delayed completion worker cannot attribute the new state to the
			// old Turn.
			record.TaskStateCompletionMarker = record.CompletionMarker
		}); err != nil {
			return err
		}
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		return err
	}
	_, err = puaWorkspace.SetTaskState(detail.ID, app.TaskStateInProgress, "")
	return err
}

func (m *agentManager) scheduleTaskTurnCompletion(rt *agentRuntime, record generationRecord) {
	if rt == nil || strings.TrimSpace(record.CompletionMarker) == "" {
		return
	}
	// Most terminal events do not require another Turn. Handle those inline so
	// startup recovery cannot leave an unnecessary background write racing with
	// shutdown or test Workspace cleanup. In-progress Tasks and waiting Tasks
	// need the resource controller for continuation or Scheduler validation.
	detail, task, err := taskDetail(rt.workspace.Path, record.ResourceID)
	if err == nil && (!task || (detail.State != app.TaskStateInProgress && detail.State != app.TaskStateWaiting)) {
		_ = markTaskTurnCompletionHandled(rt, record.CompletionMarker)
		return
	}
	m.runBackground(func() {
		_ = m.withResourceController(context.Background(), rt.workspace, record.ResourceID, func() error {
			return m.handleTaskTurnCompletionLocked(context.Background(), rt)
		})
	})
}

func markTaskTurnCompletionHandled(rt *agentRuntime, marker string) error {
	_, err := rt.mutateGeneration(func(current *generationRecord) {
		if current.CompletionMarker == marker {
			current.TaskStateCompletionMarker = marker
		}
	})
	return err
}

func taskHasTargetSchedule(snapshot app.SchedulerSnapshot, resourceID string) bool {
	resourceID = normalizedResourceID(resourceID)
	for _, schedule := range snapshot.Schedules {
		if schedule.EffectiveState == app.ScheduleStateActive && schedule.Trigger != nil && normalizedResourceID(schedule.Target) == resourceID {
			return true
		}
	}
	return false
}

// taskCompletionSupersededByWork distinguishes a genuinely quiescent terminal
// boundary from an old completion observed while newer input is already queued
// or active. The resource controller serializes this check with delivery.
func taskCompletionSupersededByWork(workspacePath string, record generationRecord) (bool, error) {
	if generationHasActiveTurn(record) {
		return true, nil
	}
	mailbox, err := loadHotResourceMailbox(workspacePath, record.ResourceID)
	if err != nil {
		return false, err
	}
	for _, message := range mailbox.Messages {
		if normalizedResourceID(message.ResourceID) != normalizedResourceID(record.ResourceID) {
			continue
		}
		if message.Status == resourceMessageQueued ||
			((message.Status == resourceMessageDelivering || message.Status == resourceMessageInterrupting) && message.ID != record.TaskStateChainID) {
			return true, nil
		}
	}
	return false, nil
}

func (m *agentManager) handleTaskTurnCompletionLocked(ctx context.Context, rt *agentRuntime) error {
	record := rt.snapshotGeneration()
	if !strings.Contains(normalizedResourceID(record.ResourceID), ".task") {
		return nil
	}
	marker := strings.TrimSpace(record.CompletionMarker)
	if marker == "" || marker == record.TaskStateCompletionMarker {
		return nil
	}
	detail, task, err := taskDetail(rt.workspace.Path, record.ResourceID)
	if err != nil || !task {
		return err
	}
	if detail.State != app.TaskStateInProgress && detail.State != app.TaskStateWaiting {
		return markTaskTurnCompletionHandled(rt, marker)
	}
	superseded, err := taskCompletionSupersededByWork(rt.workspace.Path, record)
	if err != nil {
		return err
	}
	if superseded {
		return markTaskTurnCompletionHandled(rt, marker)
	}
	puaWorkspace, err := app.OpenWorkspace(rt.workspace.Path)
	if err != nil {
		return err
	}
	if detail.State == app.TaskStateWaiting {
		snapshot, scheduleErr := newNativeScheduler(m, rt.workspace).Snapshot(m.now())
		if scheduleErr != nil {
			return scheduleErr
		}
		if taskHasTargetSchedule(snapshot, record.ResourceID) {
			return markTaskTurnCompletionHandled(rt, marker)
		}
	}
	language, err := puaWorkspace.Language()
	if err != nil {
		return err
	}
	if record.TaskStateContinuationCount >= maxTaskStateRecoveryAttempts {
		note := taskStateContinuationExhaustedNote(language)
		if detail.State == app.TaskStateWaiting {
			note = taskWaitingScheduleExhaustedNote(language)
		}
		if _, err := puaWorkspace.SetTaskState(record.ResourceID, app.TaskStateError, note); err != nil {
			return err
		}
		return markTaskTurnCompletionHandled(rt, marker)
	}
	attempt := record.TaskStateContinuationCount + 1
	instanceID, err := workspaceInstanceID(rt.workspace.Path)
	if err != nil {
		return err
	}
	messageID := taskStateContinuationMessageID(record.ResourceID, record.GenerationID, record.TaskStateChainID, marker, attempt)
	text := taskStateContinuationText(language)
	reason := "task_state_in_progress"
	if detail.State == app.TaskStateWaiting {
		messageID = taskWaitingScheduleMessageID(record.ResourceID, record.GenerationID, record.TaskStateChainID, marker, attempt)
		text = taskWaitingScheduleText(language, record.ResourceID)
		reason = "task_waiting_without_schedule"
	}
	generated := resourceMailboxMessage{
		ID: messageID, ResourceID: record.ResourceID, Text: text,
		RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue,
		Type: resourceMessageTypeTaskContinuation,
		Causation: &resourceMessageCausation{
			Type: resourceMessageTypeTaskContinuation, SourceWorkspaceInstanceID: instanceID,
			SourceResourceID: record.ResourceID, MessageID: record.TaskStateChainID, GenerationID: record.GenerationID,
			TurnID: record.CompletionTurnID, TurnReference: marker, Reason: reason,
		},
	}
	if _, err := acceptGeneratedMailboxMessage(rt.workspace.Path, generated); err != nil {
		return err
	}
	if _, err := rt.mutateGeneration(func(current *generationRecord) {
		if current.CompletionMarker == marker {
			current.TaskStateContinuationCount = attempt
			current.TaskStateCompletionMarker = marker
		}
	}); err != nil {
		return err
	}
	return m.reconcileResourceMailboxLocked(ctx, rt.workspace, record.ResourceID)
}
