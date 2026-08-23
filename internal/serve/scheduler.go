package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/localize"
)

const (
	schedulerOccurrenceReasonTime      = "scheduled_time"
	schedulerOccurrenceReasonCoalesced = "coalesced_after_downtime"
	schedulerOutcomeAccepted           = "mailbox_accepted"
	schedulerOutcomeBusy               = "skipped_target_busy"
	schedulerOutcomePaused             = "skipped_while_paused"
	schedulerOutcomeAttention          = "attention_required"
)

// NativeScheduler owns the three scheduler boundaries: portable definitions
// plus runtime projection (Snapshot), serialized mutations (Change), and
// deterministic due-work processing (Reconcile). It never creates a second
// execution protocol; occurrences are ordinary resource mailbox messages.
type NativeScheduler struct {
	manager   *agentManager
	workspace serveWorkspace
}

type NativeSchedulerChange struct {
	Operation        app.ScheduleChangeOperation
	ID               string
	ExpectedRevision uint64
	Description      *string
	Condition        *string
	Guard            *string
	Target           *string
	Trigger          *app.ScheduleTrigger
}

func newNativeScheduler(manager *agentManager, workspace serveWorkspace) *NativeScheduler {
	return &NativeScheduler{manager: manager, workspace: workspace}
}

func schedulerConfigDigest(config app.SchedulerConfig) (string, error) {
	data, err := json.Marshal(config.Schedules)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func schedulerTriggerDigest(trigger *app.ScheduleTrigger) (string, error) {
	if trigger == nil {
		return "", nil
	}
	data, err := json.Marshal(trigger)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (n *NativeScheduler) Snapshot(now time.Time) (app.SchedulerSnapshot, error) {
	workspace, err := app.OpenWorkspace(n.workspace.Path)
	if err != nil {
		return app.SchedulerSnapshot{}, err
	}
	config, err := workspace.Scheduler()
	if err != nil {
		return app.SchedulerSnapshot{}, err
	}
	store, err := loadResourceMailboxStoreForRead(n.workspace.Path, app.SchedulerResourceID)
	if err != nil {
		return app.SchedulerSnapshot{}, err
	}
	result := app.SchedulerSnapshot{SchemaVersion: config.SchemaVersion, AgentBinding: config.AgentBinding, Schedules: make([]app.ScheduleSnapshot, 0, len(config.Schedules))}
	var earliest time.Time
	for _, schedule := range config.Schedules {
		runtime := store.Scheduler.Schedules[schedule.ID]
		effective := schedule.State
		projection := app.ScheduleSnapshot{Schedule: schedule, EffectiveState: effective}
		if runtime.Revision == schedule.Revision && runtime.EffectiveState != "" {
			effective = runtime.EffectiveState
			projection.EffectiveState = effective
			projection.NextRunAt = runtime.NextRunAt
			projection.LastOccurrenceAt = runtime.LastOccurrenceAt
			projection.LastOutcome = runtime.LastOutcome
			projection.LastError = runtime.LastError
			deadline := schedulerRuntimeDeadline(runtime, now)
			if !deadline.IsZero() && (earliest.IsZero() || deadline.Before(earliest)) {
				earliest = deadline
			}
		}
		result.Schedules = append(result.Schedules, projection)
	}
	if !earliest.IsZero() {
		if earliest.Before(now) {
			earliest = now
		}
		result.NextWakeAt = earliest.Format(time.RFC3339Nano)
	}
	return result, nil
}

func (n *NativeScheduler) Change(ctx context.Context, change NativeSchedulerChange) (app.Schedule, error) {
	if err := ctx.Err(); err != nil {
		return app.Schedule{}, err
	}
	if err := change.Operation.Validate(); err != nil {
		return app.Schedule{}, err
	}
	workspace, err := app.OpenWorkspace(n.workspace.Path)
	if err != nil {
		return app.Schedule{}, err
	}
	switch change.Operation {
	case app.ScheduleChangeCreate:
		if change.Description == nil || change.Condition == nil || change.Target == nil || change.Trigger == nil {
			return app.Schedule{}, errors.New("create requires description, condition, target, and trigger")
		}
		guard := ""
		if change.Guard != nil {
			guard = *change.Guard
		}
		return workspace.AddSchedule(app.CreateScheduleInput{
			Description: *change.Description,
			Condition:   *change.Condition,
			Guard:       guard,
			Target:      *change.Target,
			Trigger:     change.Trigger,
		})
	case app.ScheduleChangeUpdate:
		if change.ID == "" || change.ExpectedRevision == 0 {
			return app.Schedule{}, errors.New("update requires id and expectedRevision")
		}
		if change.Description == nil && change.Condition == nil && change.Guard == nil && change.Target == nil && change.Trigger == nil {
			return app.Schedule{}, errors.New("update requires at least one changed field")
		}
		return workspace.UpdateSchedule(app.UpdateScheduleInput{
			ID:               change.ID,
			ExpectedRevision: change.ExpectedRevision,
			Description:      change.Description,
			Condition:        change.Condition,
			Guard:            change.Guard,
			Target:           change.Target,
			Trigger:          change.Trigger,
		})
	case app.ScheduleChangePause:
		if change.ID == "" {
			return app.Schedule{}, errors.New("pause requires id")
		}
		return workspace.PauseSchedule(change.ID)
	case app.ScheduleChangeResume:
		if change.ID == "" {
			return app.Schedule{}, errors.New("resume requires id")
		}
		config, err := workspace.Scheduler()
		if err != nil {
			return app.Schedule{}, err
		}
		for _, schedule := range config.Schedules {
			if schedule.ID == change.ID && schedule.State == app.ScheduleStatePaused {
				if err := n.reconcileSchedule(ctx, schedule, n.manager.now()); err != nil {
					return app.Schedule{}, err
				}
				break
			}
		}
		resumed, err := workspace.ResumeSchedule(change.ID)
		if err != nil {
			return app.Schedule{}, err
		}
		runtime, runtimeErr := n.schedulerRuntime(change.ID)
		if runtimeErr != nil {
			return app.Schedule{}, runtimeErr
		}
		if runtimeCompletesSameOneTimeOccurrence(runtime, resumed) && runtime.Revision != resumed.Revision {
			runtime.Revision = resumed.Revision
			runtime.TriggerDigest, runtimeErr = schedulerTriggerDigest(resumed.Trigger)
			if runtimeErr != nil {
				return app.Schedule{}, runtimeErr
			}
			runtimeErr = n.storeSchedulerRuntime(change.ID, runtime)
		}
		if runtimeErr == nil && runtime.EffectiveState == schedulerOutcomeAttention {
			runtimeErr = n.reconcileSchedule(ctx, resumed, n.manager.now())
		}
		return resumed, runtimeErr
	case app.ScheduleChangeRemove:
		if change.ID == "" {
			return app.Schedule{}, errors.New("remove requires id")
		}
		return workspace.RemoveSchedule(change.ID)
	}
	return app.Schedule{}, fmt.Errorf("Scheduler change %q was not dispatched", change.Operation)
}

// Reconcile recovers any frozen occurrence, prepares due work, advances
// durable cursors, and returns the exact next timer deadline.
func (n *NativeScheduler) Reconcile(ctx context.Context, now time.Time) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	workspace, err := app.OpenWorkspace(n.workspace.Path)
	if err != nil {
		return time.Time{}, err
	}
	config, err := workspace.Scheduler()
	if err != nil {
		return time.Time{}, err
	}
	if err := n.cancelLegacyTicks(ctx); err != nil {
		return time.Time{}, err
	}
	if err := n.reconcileMigration(ctx, config); err != nil {
		return time.Time{}, err
	}
	known := make(map[string]bool, len(config.Schedules))
	for _, schedule := range config.Schedules {
		known[schedule.ID] = true
		if err := n.reconcileSchedule(ctx, schedule, now); err != nil {
			return time.Time{}, fmt.Errorf("reconcile schedule %s: %w", schedule.ID, err)
		}
	}
	store, err := loadResourceMailboxStoreForRead(n.workspace.Path, app.SchedulerResourceID)
	if err != nil {
		return time.Time{}, err
	}
	stale := false
	for id := range store.Scheduler.Schedules {
		if !known[id] {
			stale = true
			break
		}
	}
	if stale {
		_, err = mutateResourceMailboxStoreForResource(n.workspace.Path, app.SchedulerResourceID, func(store *resourceMailboxStore) error {
			for id := range store.Scheduler.Schedules {
				if !known[id] {
					delete(store.Scheduler.Schedules, id)
				}
			}
			return nil
		})
		if err != nil {
			return time.Time{}, err
		}
	}
	snapshot, err := n.Snapshot(now)
	if err != nil || snapshot.NextWakeAt == "" {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, snapshot.NextWakeAt)
}

func (n *NativeScheduler) reconcileSchedule(ctx context.Context, schedule app.Schedule, now time.Time) error {
	runtime, err := n.schedulerRuntime(schedule.ID)
	if err != nil {
		return err
	}
	triggerDigest, err := schedulerTriggerDigest(schedule.Trigger)
	if err != nil {
		return err
	}
	revisionChanged := runtime.Revision != schedule.Revision
	triggerChanged := runtime.TriggerDigest != triggerDigest
	if revisionChanged || triggerChanged {
		sameKnownTrigger := runtime.TriggerDigest != "" && !triggerChanged
		if runtimeCompletesSameOneTimeOccurrence(runtime, schedule) && (sameKnownTrigger || runtime.TriggerDigest == "") {
			runtime.Revision = schedule.Revision
			runtime.TriggerDigest = triggerDigest
		} else if revisionChanged && sameKnownTrigger && runtime.EffectiveState == schedulerOutcomeAttention && runtime.AttentionTarget == schedule.Target && schedule.State == app.ScheduleStateActive {
			runtime.Revision = schedule.Revision
		} else {
			runtime, err = initialScheduleRuntime(schedule, now)
			if err != nil {
				return err
			}
		}
		if err := n.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
			return err
		}
	}
	if schedule.State == app.ScheduleStateNeedsCompilation || schedule.State == app.ScheduleStateCompleted {
		return nil
	}
	if schedule.State == app.ScheduleStatePaused {
		return n.reconcilePausedSchedule(schedule, runtime, now)
	}
	if runtime.EffectiveState == app.ScheduleStateCompleted {
		return nil
	}
	if runtime.EffectiveState == schedulerOutcomeAttention {
		if runtime.Prepared == nil {
			return nil
		}
		return n.deliverPrepared(ctx, schedule, runtime, now)
	}
	if retry := generationTime(runtime.RetryAt); !retry.IsZero() && now.Before(retry) {
		return nil
	}
	if runtime.Prepared != nil {
		return n.deliverPrepared(ctx, schedule, runtime, now)
	}
	due := generationTime(runtime.NextRunAt)
	if due.IsZero() || due.After(now) {
		return nil
	}
	last, next, count, truncated, err := app.CoalescedScheduleOccurrence(*schedule.Trigger, due, now)
	if err != nil {
		return n.recordScheduleError(schedule.ID, runtime, now, err)
	}
	reason := schedulerOccurrenceReasonTime
	if count > 1 || truncated {
		reason = schedulerOccurrenceReasonCoalesced
	}
	prepared, err := n.prepareOccurrence(schedule, due, last, next, count, reason)
	if err != nil {
		return n.recordScheduleError(schedule.ID, runtime, now, err)
	}
	runtime.Prepared = &prepared
	runtime.RetryAt = ""
	runtime.LastError = ""
	if err := n.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
		return err
	}
	return n.deliverPrepared(ctx, schedule, runtime, now)
}

func runtimeCompletesSameOneTimeOccurrence(runtime schedulerScheduleRuntime, schedule app.Schedule) bool {
	if runtime.EffectiveState != app.ScheduleStateCompleted || schedule.Trigger == nil || schedule.Trigger.Type != app.ScheduleTriggerAt || runtime.LastOccurrenceAt == "" {
		return false
	}
	completedAt, completedErr := time.Parse(time.RFC3339Nano, runtime.LastOccurrenceAt)
	triggerAt, triggerErr := time.Parse(time.RFC3339Nano, schedule.Trigger.At)
	return completedErr == nil && triggerErr == nil && completedAt.Equal(triggerAt)
}

func initialScheduleRuntime(schedule app.Schedule, now time.Time) (schedulerScheduleRuntime, error) {
	triggerDigest, err := schedulerTriggerDigest(schedule.Trigger)
	if err != nil {
		return schedulerScheduleRuntime{}, err
	}
	runtime := schedulerScheduleRuntime{Revision: schedule.Revision, TriggerDigest: triggerDigest, EffectiveState: schedule.State}
	if schedule.Trigger == nil {
		return runtime, nil
	}
	if schedule.State == app.ScheduleStatePaused {
		if schedule.Trigger.Type == app.ScheduleTriggerAt {
			at, _ := time.Parse(time.RFC3339Nano, schedule.Trigger.At)
			if !at.After(now) {
				runtime.EffectiveState = app.ScheduleStateCompleted
				runtime.LastOccurrenceAt = at.Format(time.RFC3339Nano)
				runtime.LastOutcome = schedulerOutcomePaused
			}
		}
		return runtime, nil
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, schedule.UpdatedAt)
	if err != nil {
		return runtime, err
	}
	if schedule.Trigger.Type == app.ScheduleTriggerAt {
		at, _ := time.Parse(time.RFC3339Nano, schedule.Trigger.At)
		runtime.NextRunAt = at.Format(time.RFC3339Nano)
		return runtime, nil
	}
	next, err := app.NextScheduleOccurrence(*schedule.Trigger, updatedAt)
	if err != nil {
		return runtime, err
	}
	if !next.IsZero() {
		runtime.NextRunAt = next.Format(time.RFC3339Nano)
	}
	return runtime, nil
}

func (n *NativeScheduler) reconcilePausedSchedule(schedule app.Schedule, runtime schedulerScheduleRuntime, now time.Time) error {
	changed := runtime.Prepared != nil || runtime.NextRunAt != "" || runtime.RetryAt != "" || runtime.EffectiveState != app.ScheduleStatePaused
	runtime.Prepared, runtime.NextRunAt, runtime.RetryAt = nil, "", ""
	runtime.EffectiveState = app.ScheduleStatePaused
	if schedule.Trigger != nil && schedule.Trigger.Type == app.ScheduleTriggerAt {
		at, _ := time.Parse(time.RFC3339Nano, schedule.Trigger.At)
		if !at.After(now) {
			runtime.EffectiveState = app.ScheduleStateCompleted
			runtime.LastOccurrenceAt = at.Format(time.RFC3339Nano)
			runtime.LastOutcome = schedulerOutcomePaused
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return n.storeSchedulerRuntime(schedule.ID, runtime)
}

func (n *NativeScheduler) prepareOccurrence(schedule app.Schedule, first, last, next time.Time, count int, reason string) (schedulerPreparedOccurrence, error) {
	instanceID, err := workspaceInstanceID(n.workspace.Path)
	if err != nil {
		return schedulerPreparedOccurrence{}, err
	}
	language, err := workspaceContentLanguage(n.workspace.Path)
	if err != nil {
		return schedulerPreparedOccurrence{}, err
	}
	revision := fmt.Sprintf("%d", schedule.Revision)
	occurrenceID := notificationMessageID("schedule-occurrence", instanceID, schedule.ID, revision, first.Format(time.RFC3339Nano))
	messageID := notificationMessageID(resourceMessageTypeScheduleOccurrence, instanceID, schedule.ID, revision, first.Format(time.RFC3339Nano))
	causation := &resourceMessageCausation{
		Type: resourceMessageTypeScheduleOccurrence, SourceWorkspaceInstanceID: instanceID,
		SourceResourceID: app.SchedulerResourceID, Reason: reason,
		ScheduleID: schedule.ID, ScheduleRevision: schedule.Revision,
		OccurrenceID: occurrenceID, ScheduledFor: first.Format(time.RFC3339Nano),
		CoalescedFrom: first.Format(time.RFC3339Nano), CoalescedThrough: last.Format(time.RFC3339Nano), CoalescedCount: count,
	}
	prepared := schedulerPreparedOccurrence{
		ScheduleID: schedule.ID, ScheduleRevision: schedule.Revision,
		OccurrenceID: occurrenceID, MessageID: messageID, Target: schedule.Target,
		ScheduledFor: first.Format(time.RFC3339Nano), CoalescedThrough: last.Format(time.RFC3339Nano),
		CoalescedCount: count, Reason: reason, Causation: causation,
	}
	if !next.IsZero() {
		prepared.NextRunAt = next.Format(time.RFC3339Nano)
	}
	prepared.Text = strings.TrimSuffix(localize.MustRender(language, "scheduler-occurrence.md", map[string]any{
		"OccurrenceID": occurrenceID, "ScheduleID": schedule.ID, "Revision": schedule.Revision,
		"ScheduledFor": prepared.ScheduledFor, "CoalescedThrough": prepared.CoalescedThrough,
		"CoalescedCount": count, "Action": schedule.Description, "Guard": schedule.Guard,
		"HasGuard": schedule.Guard != "", "Condition": schedule.Condition,
		"HasNext": prepared.NextRunAt != "", "NextRunAt": prepared.NextRunAt,
	}), "\n")
	return prepared, nil
}

func (n *NativeScheduler) deliverPrepared(ctx context.Context, schedule app.Schedule, runtime schedulerScheduleRuntime, now time.Time) error {
	prepared := runtime.Prepared
	if prepared == nil {
		return nil
	}
	exists, archived, _, targetErr := resourceExistsAndArchived(n.workspace.Path, prepared.Target)
	if targetErr != nil && !errors.Is(targetErr, app.ErrResourceNotFound) {
		return n.recordScheduleError(schedule.ID, runtime, now, targetErr)
	}
	if targetErr != nil || !exists || archived {
		reason := "target resource is unavailable"
		if targetErr != nil {
			reason = targetErr.Error()
		} else if archived {
			reason = "target resource is archived"
		}
		runtime.NextRunAt = ""
		runtime.RetryAt = ""
		runtime.EffectiveState = schedulerOutcomeAttention
		runtime.LastOutcome = schedulerOutcomeAttention
		runtime.LastError = reason
		runtime.AttentionTarget = prepared.Target
		return n.storeSchedulerRuntime(schedule.ID, runtime)
	}

	deliver := func() error {
		_, alreadyAccepted, err := mailboxMessageByID(n.workspace.Path, prepared.MessageID)
		if err != nil {
			return err
		}
		if !alreadyAccepted {
			if schedule.Trigger.Type != app.ScheduleTriggerAt {
				busy, err := n.targetBusy(prepared.Target)
				if err != nil {
					return err
				}
				if busy {
					runtime.Prepared = nil
					runtime.NextRunAt = prepared.NextRunAt
					runtime.RetryAt = ""
					runtime.RetryCount = 0
					runtime.LastOccurrenceAt = prepared.CoalescedThrough
					runtime.LastOutcome = schedulerOutcomeBusy
					runtime.LastError = ""
					runtime.AttentionTarget = ""
					return n.storeSchedulerRuntime(schedule.ID, runtime)
				}
			}
			if _, _, _, err := n.manager.resolveMailboxGenerationAgent(ctx, n.workspace, prepared.Target); err != nil {
				var apiErr *resourceAPIError
				if errors.As(err, &apiErr) && apiErr.Code == "binding_unavailable" {
					runtime.NextRunAt = ""
					runtime.RetryAt = ""
					runtime.RetryCount = 0
					runtime.EffectiveState = schedulerOutcomeAttention
					runtime.LastOutcome = schedulerOutcomeAttention
					runtime.LastError = err.Error()
					runtime.AttentionTarget = prepared.Target
					return n.storeSchedulerRuntime(schedule.ID, runtime)
				}
				runtime.EffectiveState = schedule.State
				runtime.AttentionTarget = ""
				return err
			}
		}
		runtime.EffectiveState = schedule.State
		runtime.AttentionTarget = ""
		message := resourceMailboxMessage{
			ID: prepared.MessageID, ResourceID: prepared.Target, Text: prepared.Text,
			RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, ModeFrozen: true,
			Type: resourceMessageTypeScheduleOccurrence, Causation: cloneMailboxCausation(prepared.Causation),
			SenderWorkspaceInstanceID: prepared.Causation.SourceWorkspaceInstanceID,
		}
		accepted, err := acceptGeneratedMailboxMessage(n.workspace.Path, message)
		if err != nil {
			return err
		}
		runtime.Prepared = nil
		runtime.NextRunAt = prepared.NextRunAt
		runtime.RetryAt = ""
		runtime.RetryCount = 0
		runtime.LastOccurrenceAt = prepared.CoalescedThrough
		runtime.LastOutcome = schedulerOutcomeAccepted
		runtime.LastError = ""
		runtime.AttentionTarget = ""
		if prepared.NextRunAt == "" {
			runtime.EffectiveState = app.ScheduleStateCompleted
		}
		if err := n.storeSchedulerRuntime(schedule.ID, runtime); err != nil {
			return err
		}
		if accepted.Status == resourceMessageQueued || accepted.Status == resourceMessageDelivering || accepted.Status == resourceMessageInterrupting {
			if err := n.manager.reconcileResourceMailboxLocked(ctx, n.workspace, prepared.Target); err != nil {
				recordMailboxFailure(n.workspace.Path, accepted.ID, err)
				n.manager.requestReconcile(reconcileMailboxes)
			}
		}
		return nil
	}

	var err error
	if prepared.Target == app.SchedulerResourceID {
		err = deliver()
	} else {
		err = n.manager.withResourceController(ctx, n.workspace, prepared.Target, deliver)
	}
	if err == nil {
		return nil
	}
	return n.recordScheduleError(schedule.ID, runtime, now, err)
}

func (n *NativeScheduler) targetBusy(resourceID string) (bool, error) {
	record, found, err := currentResourceGeneration(n.workspace.Path, resourceID)
	if err != nil {
		return false, err
	}
	if found && generationHasActiveTurn(record) {
		return true, nil
	}
	return mailboxPendingForResource(n.workspace.Path, resourceID)
}

func (n *NativeScheduler) recordScheduleError(id string, runtime schedulerScheduleRuntime, now time.Time, cause error) error {
	runtime.RetryCount++
	delay := 5 * time.Second
	for index := 1; index < runtime.RetryCount && delay < 30*time.Minute; index++ {
		delay *= 2
	}
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	runtime.RetryAt = now.Add(delay).Format(time.RFC3339Nano)
	runtime.LastError = cause.Error()
	if err := n.storeSchedulerRuntime(id, runtime); err != nil {
		return errors.Join(cause, err)
	}
	return nil
}

func (n *NativeScheduler) schedulerRuntime(id string) (schedulerScheduleRuntime, error) {
	store, err := loadResourceMailboxStoreForRead(n.workspace.Path, app.SchedulerResourceID)
	if err != nil {
		return schedulerScheduleRuntime{}, err
	}
	return store.Scheduler.Schedules[id], nil
}

func (n *NativeScheduler) storeSchedulerRuntime(id string, runtime schedulerScheduleRuntime) error {
	_, err := mutateResourceMailboxStoreForResource(n.workspace.Path, app.SchedulerResourceID, func(store *resourceMailboxStore) error {
		if store.Scheduler.Schedules == nil {
			store.Scheduler.Schedules = make(map[string]schedulerScheduleRuntime)
		}
		store.Scheduler.Schedules[id] = runtime
		return nil
	})
	return err
}

func schedulerRuntimeDeadline(runtime schedulerScheduleRuntime, now time.Time) time.Time {
	if runtime.EffectiveState == app.ScheduleStatePaused || runtime.EffectiveState == app.ScheduleStateCompleted || runtime.EffectiveState == schedulerOutcomeAttention {
		return time.Time{}
	}
	if runtime.RetryAt != "" {
		return generationTime(runtime.RetryAt)
	}
	if runtime.Prepared != nil {
		return now
	}
	return generationTime(runtime.NextRunAt)
}

func (n *NativeScheduler) cancelLegacyTicks(ctx context.Context) error {
	store, err := loadResourceMailboxStoreForRead(n.workspace.Path, app.SchedulerResourceID)
	if err != nil {
		return err
	}
	needsCancellation := false
	for _, message := range store.Mailbox.Messages {
		if message.Type == resourceMessageTypeSchedulerTick && message.Status == resourceMessageQueued {
			needsCancellation = true
			break
		}
	}
	if !needsCancellation {
		return nil
	}
	_, err = mutateResourceMailboxStoreForResource(n.workspace.Path, app.SchedulerResourceID, func(store *resourceMailboxStore) error {
		now := time.Now().Format(time.RFC3339Nano)
		for index := range store.Mailbox.Messages {
			message := &store.Mailbox.Messages[index]
			if message.Type != resourceMessageTypeSchedulerTick || message.Status != resourceMessageQueued {
				continue
			}
			message.Status = resourceMessageUndeliverable
			message.TerminalAt, message.UpdatedAt = now, now
			message.LastErrorCode = "scheduler_v1_retired"
			message.LastError = "Legacy scheduler tick was cancelled during native scheduler migration"
		}
		return ctx.Err()
	})
	return err
}

func (n *NativeScheduler) reconcileMigration(ctx context.Context, config app.SchedulerConfig) error {
	pending := make([]app.Schedule, 0)
	for _, schedule := range config.Schedules {
		if schedule.State == app.ScheduleStateNeedsCompilation {
			pending = append(pending, schedule)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	digest, err := schedulerConfigDigest(config)
	if err != nil {
		return err
	}
	store, err := loadResourceMailboxStoreForRead(n.workspace.Path, app.SchedulerResourceID)
	if err != nil || store.Scheduler.MigrationDigest == digest {
		return err
	}
	instanceID, err := workspaceInstanceID(n.workspace.Path)
	if err != nil {
		return err
	}
	language, err := workspaceContentLanguage(n.workspace.Path)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(pending))
	for _, schedule := range pending {
		ids = append(ids, schedule.ID)
	}
	messageID := notificationMessageID(resourceMessageTypeScheduleMigration, instanceID, digest)
	text := strings.TrimSuffix(localize.MustRender(language, "scheduler-migration.md", map[string]any{
		"MessageID": messageID, "ScheduleIDs": strings.Join(ids, ", "), "Count": len(ids),
	}), "\n")
	message := resourceMailboxMessage{
		ID: messageID, ResourceID: app.SchedulerResourceID, Text: text,
		RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, ModeFrozen: true,
		Type:      resourceMessageTypeScheduleMigration,
		Causation: &resourceMessageCausation{Type: resourceMessageTypeScheduleMigration, SourceWorkspaceInstanceID: instanceID, SourceResourceID: app.SchedulerResourceID, Reason: "compile_migrated_schedules", ScheduleDigest: digest},
	}
	accepted, err := acceptGeneratedMailboxMessage(n.workspace.Path, message)
	if err != nil {
		return err
	}
	_, err = mutateResourceMailboxStoreForResource(n.workspace.Path, app.SchedulerResourceID, func(store *resourceMailboxStore) error {
		store.Scheduler.MigrationDigest = digest
		return nil
	})
	if err != nil {
		return err
	}
	if accepted.Status == resourceMessageQueued || accepted.Status == resourceMessageDelivering || accepted.Status == resourceMessageInterrupting {
		if err := n.manager.reconcileResourceMailboxLocked(ctx, n.workspace, app.SchedulerResourceID); err != nil {
			recordMailboxFailure(n.workspace.Path, accepted.ID, err)
			n.manager.requestReconcile(reconcileMailboxes)
		}
	}
	return nil
}

func (m *agentManager) reconcileSchedulerLocked(ctx context.Context, workspace serveWorkspace) error {
	return m.withResourceController(ctx, workspace, app.SchedulerResourceID, func() error {
		_, err := newNativeScheduler(m, workspace).Reconcile(ctx, m.now())
		return err
	})
}
