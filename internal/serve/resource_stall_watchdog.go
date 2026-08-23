package serve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/localize"
)

func stallWatchdogRecoveryText(language string) string {
	return strings.TrimSpace(localize.MustRender(language, "turn-stall-recovery.md", nil))
}

// frozenStallWatchdogRecoveryText preserves a message that was durably
// accepted before a language migration or binary upgrade. The deterministic
// message id must keep the original localized body across retries.
func frozenStallWatchdogRecoveryText(workspacePath, messageID, language string) (string, error) {
	localized := stallWatchdogRecoveryText(language)
	existing, found, err := mailboxMessageByID(workspacePath, messageID)
	if err != nil || !found || existing.Type != resourceMessageTypeTurnStallRecovery {
		return localized, err
	}
	for _, supportedLanguage := range []string{localize.English, localize.SimplifiedChinese} {
		if existing.Text == stallWatchdogRecoveryText(supportedLanguage) {
			return existing.Text, nil
		}
	}
	return localized, nil
}

// reconcileStallWatchdogLocked observes one Session's semantic activity clock.
// It is called from the same resource controller that owns mailbox and
// generation lifecycle work, so only one recovery chain can be created for a
// resource at a time.
func (m *agentManager) reconcileStallWatchdogLocked(ctx context.Context, cfg config, workspace serveWorkspace, observed generationRecord, rt *agentRuntime, session agentHubSession, client *agentHubClient) error {
	if rt == nil || client == nil || strings.TrimSpace(observed.GenerationID) == "" || strings.TrimSpace(session.ID) == "" {
		return nil
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		return err
	}
	runtimeConfig, err := puaWorkspace.RuntimeConfig()
	if err != nil {
		return err
	}
	policy := runtimeConfig.StallWatchdogPolicy

	if err := m.advanceStallWatchdogState(workspace, observed, rt, session); err != nil {
		return err
	}
	record := rt.snapshotGeneration()
	activeTurnID := strings.TrimSpace(activeAgentHubTurnID(session))
	if !policy.Enabled || policy.TimeoutMinutes < 1 || session.State != "running" || activeTurnID == "" || len(session.PendingApprovalIDs) > 0 {
		return nil
	}
	if state := record.StallWatchdog; state != nil {
		if state.RecoveryExhausted {
			return nil
		}
		if state.RecoveryTurnID != "" {
			// A new active Turn means the bounded recovery chain already reached a
			// new boundary. The next poll may evaluate it independently.
			if state.RecoveryTurnID != activeTurnID {
				_, err := rt.mutateGeneration(func(current *generationRecord) {
					if current.StallWatchdog != nil && current.StallWatchdog.RecoveryTurnID == state.RecoveryTurnID {
						current.StallWatchdog = nil
					}
				})
				return err
			}
			lastActivity := agentHubActivityTime(session, record)
			now := m.resourceNow()
			if !lastActivity.IsZero() && !now.Before(lastActivity) && now.Sub(lastActivity) >= time.Duration(policy.TimeoutMinutes)*time.Minute {
				_, err := rt.mutateGeneration(func(current *generationRecord) {
					if current.StallWatchdog != nil && current.StallWatchdog.RecoveryTurnID == state.RecoveryTurnID {
						current.StallWatchdog.RecoveryExhausted = true
					}
				})
				if err == nil {
					rt.addPUANotice(m, "warning", "agenthub/turn-stall-watchdog", "The recovered Turn is stalled again; automatic recovery is bounded to one attempt.")
				}
				return err
			}
			return nil
		}
		// The original stale Turn is still active while Stop is in flight or its
		// outcome is unknown. The durable guard prevents a duplicate Stop.
		if state.TurnID == activeTurnID {
			return nil
		}
		// A resumed Session can be observed before the mailbox projection records
		// the recovery Turn. Bind the first different active Turn to this chain.
		_, err := rt.mutateGeneration(func(current *generationRecord) {
			if current.StallWatchdog != nil && current.StallWatchdog.RecoveryTurnID == "" {
				current.StallWatchdog.RecoveryTurnID = activeTurnID
			}
		})
		return err
	}

	lastActivity := agentHubActivityTime(session, record)
	now := m.resourceNow()
	if lastActivity.IsZero() || now.Before(lastActivity) || now.Sub(lastActivity) < time.Duration(policy.TimeoutMinutes)*time.Minute {
		return nil
	}
	return m.startStallWatchdogRecoveryLocked(ctx, cfg, workspace, record, rt, session, client)
}

// advanceStallWatchdogState completes durable bookkeeping after Stop/Resume
// work has progressed through the ordinary mailbox reconciler. It never issues
// an AgentHub action.
func (m *agentManager) advanceStallWatchdogState(workspace serveWorkspace, observed generationRecord, rt *agentRuntime, session agentHubSession) error {
	record := rt.snapshotGeneration()
	state := record.StallWatchdog
	if state == nil || state.GenerationID != record.GenerationID || state.SessionID != session.ID {
		return nil
	}
	message, found, err := mailboxMessageByID(workspace.Path, state.RecoveryMessageID)
	if err != nil {
		return err
	}
	if found && state.RecoveryTurnID == "" && message.Status == resourceMessageDelivered && strings.TrimSpace(message.TurnID) != "" {
		_, err = rt.mutateGeneration(func(current *generationRecord) {
			if current.StallWatchdog != nil && current.StallWatchdog.RecoveryMessageID == state.RecoveryMessageID {
				current.StallWatchdog.RecoveryTurnID = strings.TrimSpace(message.TurnID)
			}
		})
		if err != nil {
			return err
		}
		state = rt.snapshotGeneration().StallWatchdog
	}
	if state == nil {
		return nil
	}
	terminalMessage := found && (message.Status == resourceMessageCancelled || message.Status == resourceMessageUndeliverable || message.Status == resourceMessageDeliveryUnknown)
	if terminalMessage || (state.RecoveryTurnID != "" && session.State != "running" && session.State != "waiting_approval") {
		_, err = rt.mutateGeneration(func(current *generationRecord) {
			if current.StallWatchdog != nil && current.StallWatchdog.RecoveryMessageID == state.RecoveryMessageID {
				current.StallWatchdog = nil
			}
		})
		return err
	}
	return nil
}

func (m *agentManager) startStallWatchdogRecoveryLocked(ctx context.Context, cfg config, workspace serveWorkspace, record generationRecord, rt *agentRuntime, session agentHubSession, client *agentHubClient) error {
	turnID := strings.TrimSpace(activeAgentHubTurnID(session))
	if turnID == "" {
		return nil
	}
	rt.turnActionMu.Lock()
	defer rt.turnActionMu.Unlock()

	latest := rt.snapshotGeneration()
	if latest.ID != record.ID || latest.GenerationID != record.GenerationID || latest.AgentHubSessionID != record.AgentHubSessionID || latest.StallWatchdog != nil {
		return nil
	}
	current, err := client.GetSession(ctx, latest.AgentHubSessionID)
	if err != nil {
		return fmt.Errorf("read current Session before stall recovery: %w", err)
	}
	if current.State != "running" || strings.TrimSpace(activeAgentHubTurnID(current)) != turnID || len(current.PendingApprovalIDs) > 0 {
		return nil
	}
	if !agentHubSessionExactlyMatchesGeneration(cfg, latest, current) {
		return errors.New("AgentHub Session does not match the current generation for stall recovery")
	}
	lastActivity := agentHubActivityTime(current, latest)
	now := m.resourceNow()
	if lastActivity.IsZero() || now.Before(lastActivity) {
		return nil
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		return err
	}
	runtimeConfig, err := puaWorkspace.RuntimeConfig()
	if err != nil {
		return err
	}
	if !runtimeConfig.StallWatchdogPolicy.Enabled || now.Sub(lastActivity) < time.Duration(runtimeConfig.StallWatchdogPolicy.TimeoutMinutes)*time.Minute {
		return nil
	}
	instanceID := strings.TrimSpace(runtimeConfig.InstanceID)
	resourceID := normalizedResourceID(latest.ResourceID)
	messageID := notificationMessageID(resourceMessageTypeTurnStallRecovery, instanceID, resourceID, latest.GenerationID, current.ID, turnID, "1")
	language, err := puaWorkspace.Language()
	if err != nil {
		return err
	}
	recoveryText, err := frozenStallWatchdogRecoveryText(workspace.Path, messageID, language)
	if err != nil {
		return err
	}
	message := resourceMailboxMessage{
		ID: messageID, ResourceID: resourceID, Text: recoveryText,
		Role: "system", RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue,
		ModeFrozen: true, Type: resourceMessageTypeTurnStallRecovery,
		Causation: &resourceMessageCausation{
			Type: resourceMessageTypeTurnStallRecovery, SourceWorkspaceInstanceID: instanceID,
			SourceResourceID: resourceID, GenerationID: latest.GenerationID, TurnID: turnID,
			Reason: "no_effective_activity",
		},
	}
	accepted, err := acceptGeneratedMailboxMessage(workspace.Path, message)
	if err != nil {
		return fmt.Errorf("persist stall recovery message: %w", err)
	}
	if accepted.Status == resourceMessageDelivered && strings.TrimSpace(accepted.TurnID) != "" {
		_, err = rt.mutateGeneration(func(currentRecord *generationRecord) {
			if currentRecord.StallWatchdog == nil && currentRecord.GenerationID == latest.GenerationID {
				currentRecord.StallWatchdog = &stallWatchdogState{
					GenerationID: latest.GenerationID, SessionID: current.ID, TurnID: turnID,
					RecoveryTurnID: strings.TrimSpace(accepted.TurnID), RecoveryMessageID: messageID,
					DetectedAt: now.Format(time.RFC3339Nano), Attempt: 1, StopRequested: true,
				}
			}
		})
		return err
	}
	_, err = rt.mutateRuntime(func(runtime *agentRuntime) {
		if runtime.record.StallWatchdog != nil || runtime.record.GenerationID != latest.GenerationID || runtime.record.AgentHubSessionID != current.ID {
			return
		}
		runtime.record.StallWatchdog = &stallWatchdogState{
			GenerationID: latest.GenerationID, SessionID: current.ID, TurnID: turnID,
			RecoveryMessageID: messageID, DetectedAt: now.Format(time.RFC3339Nano), Attempt: 1,
			StopRequested: true,
		}
		runtime.record.Status = "stopping"
		runtime.record.UpdatedAt = now.Format(time.RFC3339Nano)
		runtime.agentHub = client
		runtime.agentHubStopRequested = true
	})
	if err != nil {
		return fmt.Errorf("persist stall recovery Stop guard: %w", err)
	}
	stopped, stopErr := client.Stop(ctx, current.ID)
	if stopErr != nil {
		rt.addPUANotice(m, "warning", "agenthub/turn-stall-watchdog", "Automatic stall recovery Stop outcome is unknown; it will not be retried automatically: "+stopErr.Error())
		return nil
	}
	if !agentHubSessionExactlyMatchesGeneration(cfg, latest, stopped) {
		return errors.New("AgentHub Stop response did not match the current generation for stall recovery")
	}
	rt.applyAgentHubSessionState(m, stopped)
	_ = m.enqueueResourceController(workspace, resourceID, func() error {
		if err := m.reconcileResourceMailboxLocked(context.Background(), workspace, resourceID); err != nil {
			rt.addPUANotice(m, "warning", "resource/turn-stall-watchdog", "Resume stalled Turn Session: "+err.Error())
			return err
		}
		return nil
	})
	rt.addPUANotice(m, "warning", "agenthub/turn-stall-watchdog", fmt.Sprintf("Turn %s had no effective activity and was stopped for automatic recovery.", turnID))
	return nil
}
