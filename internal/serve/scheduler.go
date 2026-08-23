package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/localize"
)

const (
	schedulerTickReasonInterval = "interval"
	schedulerTickReasonChanged  = "schedule_changed"
	schedulerTickReasonRecovery = "server_recovery"
)

func schedulerConfigDigest(config app.SchedulerConfig) (string, error) {
	data, err := json.Marshal(config.Schedules)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func schedulerTickMessage(language, reason, messageID string) string {
	return strings.TrimSuffix(localize.MustRender(language, "scheduler-tick.md", map[string]string{
		"MessageID": messageID, "Reason": reason,
	}), "\n")
}

func pendingSchedulerTick(mailbox resourceMailbox) bool {
	for _, message := range mailbox.Messages {
		if message.ResourceID == app.SchedulerResourceID && message.Type == resourceMessageTypeSchedulerTick &&
			(message.Status == resourceMessageQueued || message.Status == resourceMessageDelivering || message.Status == resourceMessageInterrupting) {
			return true
		}
	}
	return false
}

func latestSchedulerTick(mailbox resourceMailbox) (resourceMailboxMessage, bool) {
	var latest resourceMailboxMessage
	found := false
	for _, message := range mailbox.Messages {
		if message.ResourceID != app.SchedulerResourceID || message.Type != resourceMessageTypeSchedulerTick {
			continue
		}
		if !found || message.Sequence > latest.Sequence {
			latest, found = message, true
		}
	}
	return latest, found
}

func cancelPendingSchedulerTicks(ctx context.Context, workspacePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store, err := loadResourceMailboxStoreForRead(workspacePath, app.SchedulerResourceID)
	if err != nil {
		return err
	}
	needsMutation := false
	for _, message := range store.Mailbox.Messages {
		if message.ResourceID == app.SchedulerResourceID && message.Type == resourceMessageTypeSchedulerTick && message.Status == resourceMessageQueued {
			needsMutation = true
			break
		}
	}
	if !needsMutation {
		return nil
	}
	_, err = mutateResourceMailboxStoreForResource(workspacePath, app.SchedulerResourceID, func(store *resourceMailboxStore) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := time.Now().Format(time.RFC3339Nano)
		for index := range store.Mailbox.Messages {
			message := &store.Mailbox.Messages[index]
			if message.ResourceID != app.SchedulerResourceID || message.Type != resourceMessageTypeSchedulerTick || message.Status != resourceMessageQueued {
				continue
			}
			message.Status = resourceMessageUndeliverable
			message.TerminalAt = now
			message.UpdatedAt = now
			message.LastErrorCode = "scheduler_empty"
			message.LastError = "Scheduler tick was cancelled because the schedule list is empty"
		}
		store.Scheduler = schedulerCheckpointFromMessages(app.SchedulerResourceID, store.Mailbox.Messages)
		return nil
	})
	return err
}

func markSchedulerTickTerminal(workspacePath string, message resourceMailboxMessage, turn agentHubTurn) error {
	terminalAt := strings.TrimSpace(turn.EndedAt)
	if terminalAt == "" {
		terminalAt = strings.TrimSpace(turn.CompletedAt)
	}
	if terminalAt == "" {
		terminalAt = time.Now().Format(time.RFC3339Nano)
	}
	_, err := mutateResourceMailboxStoreForResource(workspacePath, app.SchedulerResourceID, func(store *resourceMailboxStore) error {
		for index := range store.Mailbox.Messages {
			if store.Mailbox.Messages[index].ID == message.ID {
				store.Mailbox.Messages[index].TurnTerminalAt = terminalAt
				if strings.TrimSpace(turn.TurnID) != "" {
					store.Mailbox.Messages[index].TurnID = turn.TurnID
				}
			}
		}
		store.Scheduler = schedulerCheckpointFromMessages(app.SchedulerResourceID, store.Mailbox.Messages)
		store.Scheduler.TurnTerminalAt = terminalAt
		store.Scheduler.TurnStatus = strings.TrimSpace(turn.Status)
		return nil
	})
	return err
}

func checkpointSchedulerTickMessage(workspacePath string, message resourceMailboxMessage) error {
	_, err := mutateResourceMailboxStoreForResource(workspacePath, app.SchedulerResourceID, func(store *resourceMailboxStore) error {
		store.Scheduler = schedulerCheckpointFromMessages(app.SchedulerResourceID, store.Mailbox.Messages)
		if store.Scheduler.LastTickMessageID == "" {
			store.Scheduler.LastTickMessageID = message.ID
			store.Scheduler.GenerationID = message.GenerationID
			store.Scheduler.AgentHubSessionID = message.AgentHubSessionID
			store.Scheduler.TurnID = message.TurnID
			if message.Causation != nil {
				store.Scheduler.ConfigDigest = message.Causation.ScheduleDigest
				store.Scheduler.Reason = message.Causation.Reason
			}
			store.Scheduler.AcceptedAt = message.AcceptedAt
		}
		return nil
	})
	return err
}

func schedulerTickTerminal(ctx context.Context, workspacePath string, message resourceMailboxMessage, client *agentHubClient) (agentHubTurn, bool, error) {
	if message.Status == resourceMessageUndeliverable || message.Status == resourceMessageDeliveryUnknown {
		_ = markSchedulerTickTerminal(workspacePath, message, agentHubTurn{Status: message.Status, Closed: true, EndedAt: message.TerminalAt})
		return agentHubTurn{Status: message.Status, Closed: true, EndedAt: message.TerminalAt}, true, nil
	}
	if message.Status != resourceMessageDelivered || strings.TrimSpace(message.GenerationID) == "" {
		return agentHubTurn{}, false, nil
	}
	if strings.TrimSpace(message.TurnTerminalAt) != "" {
		return agentHubTurn{TurnID: message.TurnID, Status: "completed", Closed: true, EndedAt: message.TurnTerminalAt}, true, nil
	}
	record, found, err := generationRecordByID(workspacePath, message.GenerationID)
	if err != nil {
		return agentHubTurn{}, false, err
	}
	if !found {
		return agentHubTurn{}, true, nil
	}
	if strings.TrimSpace(message.TurnID) == "" {
		turn, turnFound, turnErr := findSchedulerTickTurn(ctx, client, record.AgentHubSessionID, message)
		if turnErr != nil {
			return agentHubTurn{}, false, turnErr
		}
		if turnFound {
			_, _ = updateMailboxMessage(workspacePath, message.ID, func(current *resourceMailboxMessage) {
				current.TurnID = turn.TurnID
			})
			if turn.Closed {
				_ = markSchedulerTickTerminal(workspacePath, message, turn)
			}
			return turn, turn.Closed, nil
		}
		if !isLiveAgentStatus(record.Status) {
			terminal := agentHubTurn{Status: record.Status, Closed: true, EndedAt: record.UpdatedAt}
			_ = markSchedulerTickTerminal(workspacePath, message, terminal)
			return terminal, true, nil
		}
		return agentHubTurn{}, false, nil
	}
	turn, _, turnErr := client.SessionTurn(ctx, record.AgentHubSessionID, message.TurnID)
	if turnErr == nil {
		if turn.Closed {
			_ = markSchedulerTickTerminal(workspacePath, message, turn)
		}
		return turn, turn.Closed, nil
	}
	if record.CompletionMarker != "" && record.CompletionTurnID == message.TurnID {
		terminal := agentHubTurn{TurnID: message.TurnID, Status: record.CompletionState, Closed: true, EndedAt: record.CompletionAt}
		_ = markSchedulerTickTerminal(workspacePath, message, terminal)
		return terminal, true, nil
	}
	if !isLiveAgentStatus(record.Status) {
		terminal := agentHubTurn{TurnID: message.TurnID, Status: record.Status, Closed: true, EndedAt: record.UpdatedAt}
		_ = markSchedulerTickTerminal(workspacePath, message, terminal)
		return terminal, true, nil
	}
	return agentHubTurn{}, false, turnErr
}

func findSchedulerTickTurn(ctx context.Context, client *agentHubClient, sessionID string, message resourceMailboxMessage) (agentHubTurn, bool, error) {
	canonical, canonicalFound, canonicalErr := findCanonicalAgentHubMessage(ctx, client, sessionID, message, 0)
	if canonicalErr != nil {
		return agentHubTurn{}, false, canonicalErr
	}
	if canonicalFound && strings.TrimSpace(canonical.TurnID) != "" {
		turn, _, err := client.SessionTurn(ctx, sessionID, canonical.TurnID)
		if err != nil {
			return agentHubTurn{}, false, err
		}
		return turn, true, nil
	}
	// Compatibility fallback for historical materialized Turns whose opener
	// predates stable message IDs.
	page, err := client.SessionTurns(ctx, sessionID, 0, true, 50)
	if err != nil {
		return agentHubTurn{}, false, err
	}
	var best agentHubTurn
	found := false
	for _, turn := range page.Turns {
		matches := false
		for _, item := range turn.Items {
			if item.Role == "system" && strings.TrimSpace(item.Text) == message.Text {
				matches = true
				break
			}
		}
		preview := strings.TrimSpace(turn.TriggerPreview)
		if !matches && turn.TriggerRole == "system" && preview != "" && strings.HasPrefix(message.Text, preview) {
			matches = true
		}
		if matches && (!found || turn.StartEventID > best.StartEventID) {
			best, found = turn, true
		}
	}
	if found && strings.TrimSpace(best.TurnID) == "" {
		best.TurnID = strings.TrimSpace(best.ID)
	}
	return best, found && best.TurnID != "", nil
}

// reconcileSchedulerLocked serializes Scheduler reconciliation with the
// Scheduler resource controller. It converts elapsed Server time and durable
// Scheduler configuration changes into ordinary enqueue-only mailbox
// messages. AgentHub remains the canonical Turn owner.
func (m *agentManager) reconcileSchedulerLocked(ctx context.Context, workspace serveWorkspace, client *agentHubClient) error {
	return m.withResourceController(ctx, workspace, app.SchedulerResourceID, func() error {
		return m.reconcileScheduler(ctx, workspace, client)
	})
}

func (m *agentManager) reconcileScheduler(ctx context.Context, workspace serveWorkspace, client *agentHubClient) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		return err
	}
	config, err := puaWorkspace.Scheduler()
	if err != nil {
		return err
	}
	digest, err := schedulerConfigDigest(config)
	if err != nil {
		return err
	}
	previousDigest, observedBefore := m.schedulerDigests[workspace.Path]
	m.schedulerDigests[workspace.Path] = digest
	if len(config.Schedules) == 0 {
		return cancelPendingSchedulerTicks(ctx, workspace.Path)
	}
	mailbox, err := loadResourceMailboxForResource(workspace.Path, app.SchedulerResourceID)
	if err != nil {
		return err
	}
	hot := resourceMailbox{Version: mailbox.Version, NextSequence: mailbox.NextSequence, Messages: []resourceMailboxMessage{}}
	for _, message := range mailbox.Messages {
		if !message.receipt {
			hot.Messages = append(hot.Messages, message)
		}
	}
	if pendingSchedulerTick(hot) {
		return nil
	}
	last, found := latestSchedulerTick(mailbox)
	store, storeErr := loadResourceMailboxStoreForRead(workspace.Path, app.SchedulerResourceID)
	if storeErr != nil {
		return storeErr
	}
	if !found && strings.TrimSpace(store.Scheduler.LastTickMessageID) != "" {
		checkpoint := store.Scheduler
		last = resourceMailboxMessage{
			ID: checkpoint.LastTickMessageID, ResourceID: app.SchedulerResourceID,
			Status: resourceMessageDelivered, AcceptedAt: checkpoint.AcceptedAt,
			GenerationID: checkpoint.GenerationID, AgentHubSessionID: checkpoint.AgentHubSessionID, TurnID: checkpoint.TurnID,
			TurnTerminalAt: checkpoint.TurnTerminalAt,
			Causation:      &resourceMessageCausation{Type: resourceMessageTypeSchedulerTick, SourceWorkspaceInstanceID: mailboxInstanceID(workspace.Path), SourceResourceID: app.SchedulerResourceID, Reason: checkpoint.Reason, ScheduleDigest: checkpoint.ConfigDigest},
		}
		found = true
	}
	reason, basis := "", ""
	if !found {
		if observedBefore && previousDigest != digest {
			reason = schedulerTickReasonChanged
		} else {
			reason = schedulerTickReasonRecovery
		}
		basis = digest
	} else if last.Causation == nil || last.Causation.ScheduleDigest != digest {
		reason, basis = schedulerTickReasonChanged, last.ID
	} else {
		turn, terminal, terminalErr := schedulerTickTerminal(ctx, workspace.Path, last, client)
		if terminalErr != nil {
			return terminalErr
		}
		if !terminal {
			return nil
		}
		basis = last.ID
		if strings.TrimSpace(turn.Status) != "completed" {
			reason = schedulerTickReasonRecovery
		} else {
			terminalAt := strings.TrimSpace(turn.EndedAt)
			if terminalAt == "" {
				terminalAt = strings.TrimSpace(turn.CompletedAt)
			}
			endedAt, parseErr := time.Parse(time.RFC3339Nano, terminalAt)
			if parseErr != nil {
				return fmt.Errorf("Scheduler tick %s has invalid terminal time: %w", last.ID, parseErr)
			}
			if m.now().Before(endedAt.Add(time.Duration(config.WakeIntervalMinutes) * time.Minute)) {
				return nil
			}
			reason = schedulerTickReasonInterval
		}
	}
	instanceID, err := workspaceInstanceID(workspace.Path)
	if err != nil {
		return err
	}
	language, err := workspaceContentLanguage(workspace.Path)
	if err != nil {
		return err
	}
	messageID := notificationMessageID(resourceMessageTypeSchedulerTick, instanceID, reason, digest, basis)
	message := resourceMailboxMessage{
		ID:         messageID,
		ResourceID: app.SchedulerResourceID, Text: schedulerTickMessage(language, reason, messageID),
		RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, ModeFrozen: true,
		Type: resourceMessageTypeSchedulerTick,
		Causation: &resourceMessageCausation{
			Type: resourceMessageTypeSchedulerTick, SourceWorkspaceInstanceID: instanceID,
			SourceResourceID: app.SchedulerResourceID, Reason: reason, ScheduleDigest: digest,
		},
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	accepted, err := acceptGeneratedMailboxMessage(workspace.Path, message)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkpointSchedulerTickMessage(workspace.Path, accepted); err != nil {
		return err
	}
	if accepted.Status == resourceMessageQueued || accepted.Status == resourceMessageDelivering || accepted.Status == resourceMessageInterrupting {
		if err := m.reconcileResourceMailboxLocked(ctx, workspace, app.SchedulerResourceID); err != nil {
			recordMailboxFailure(workspace.Path, accepted.ID, err)
			return err
		}
	}
	return nil
}
