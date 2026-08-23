package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	resumeRetryBase = 5 * time.Second
	resumeRetryMax  = 5 * time.Minute
)

func resumeRetryDelay(failureCount int) time.Duration {
	if failureCount <= 1 {
		return resumeRetryBase
	}
	delay := resumeRetryBase
	for attempt := 1; attempt < failureCount && delay < resumeRetryMax; attempt++ {
		delay *= 2
		if delay >= resumeRetryMax {
			return resumeRetryMax
		}
	}
	return delay
}

// resumeStoppedGenerationLocked is the runtime adapter for the canonical
// ResumeSession operation. The resource controller has already serialized the
// resource, while turnActionMu serializes this Session with direct Turn
// actions. No store write is held across the AgentHub request.
func (m *agentManager) resumeStoppedGenerationLocked(ctx context.Context, workspace serveWorkspace, record generationRecord, rt *agentRuntime, client *agentHubClient, plan GenerationLifecyclePlan) (bool, bool, error) {
	if rt == nil || client == nil || plan.Operation != GenerationOperationResumeSession || strings.TrimSpace(record.AgentHubSessionID) == "" {
		return false, false, nil
	}
	rt.turnActionMu.Lock()
	defer rt.turnActionMu.Unlock()

	latest := rt.snapshotGeneration()
	if latest.ID != record.ID || latest.GenerationID != record.GenerationID || latest.AgentHubSessionID != record.AgentHubSessionID ||
		latest.ReplacementPending || latest.ArchivedTaskStopRequested || latest.SessionResumeUnavailable {
		return false, false, nil
	}
	if strings.TrimSpace(plan.Guard.Revision) != "" && strings.TrimSpace(latest.UpdatedAt) != strings.TrimSpace(plan.Guard.Revision) {
		// Another poll/action changed the generation after planning. Discard the
		// plan; the next controller pass will observe the new boundary.
		return false, false, nil
	}

	cfg, _, err := m.agentHubRuntimeConfig()
	if err != nil {
		return false, false, err
	}
	observed, err := client.GetSession(ctx, latest.AgentHubSessionID)
	if err != nil {
		return false, isTerminalResumeError(err), err
	}
	if observed.State != "stopped" {
		return false, false, nil
	}
	if !agentHubSessionExactlyMatchesGeneration(cfg, latest, observed) {
		return false, true, fmt.Errorf("AgentHub Session %s does not match generation %s for Resume", observed.ID, latest.GenerationID)
	}

	receipt := lifecycleResumeReceipt(plan)
	if _, err := rt.mutateGeneration(func(current *generationRecord) {
		if current.ID != latest.ID || current.GenerationID != latest.GenerationID || current.AgentHubSessionID != latest.AgentHubSessionID {
			return
		}
		current.LifecycleReceipt = &receipt
		current.Status = "starting"
		current.AgentHubStoppedObserved = false
	}); err != nil {
		return false, false, err
	}

	var launchEnvironment map[string]string
	var ephemeralEnvironment map[string]string
	if m.server != nil {
		boundVariables, boundSecrets, bindingErr := m.server.serviceEnvironment(workspace)
		if bindingErr != nil {
			return false, false, bindingErr
		}
		launchEnvironment = boundVariables
		if len(boundSecrets) > 0 {
			status, statusErr := client.Status(ctx)
			if statusErr != nil {
				return false, false, statusErr
			}
			if !agentHubHasCapability(status, "session.ephemeral-environment") {
				return false, false, errors.New("AgentHub does not support ephemeral service secrets")
			}
			ephemeralEnvironment = boundSecrets
		}
	}
	resumed, resumeErr := client.ResumeWithEnvironment(ctx, observed.ID, launchEnvironment, ephemeralEnvironment)
	if resumeErr != nil {
		terminal := isTerminalResumeError(resumeErr)
		receipt.State = GenerationReceiptUnknown
		if terminal {
			receipt.State = GenerationReceiptTerminal
		}
		_, persistErr := rt.mutateGeneration(func(current *generationRecord) {
			if current.ID != latest.ID || current.GenerationID != latest.GenerationID || current.AgentHubSessionID != latest.AgentHubSessionID {
				return
			}
			current.LifecycleReceipt = &receipt
			current.Status = "stopped"
			current.AgentHubStoppedObserved = !current.IdleSleepStopRequested
			if current.IdleSleepStopRequested && !current.ReplacementPending && !current.ArchivedTaskStopRequested {
				current.Status = "idle-suspended"
			}
			if terminal {
				current.SessionResumeUnavailable = true
				current.ResumeFailureCount = 0
				current.ResumeRetryAt = ""
				current.ResumeLastError = ""
			} else {
				current.ResumeFailureCount++
				current.ResumeRetryAt = m.resourceNow().Add(resumeRetryDelay(current.ResumeFailureCount)).Format(time.RFC3339Nano)
				current.ResumeLastError = strings.TrimSpace(resumeErr.Error())
			}
		})
		if persistErr != nil {
			return false, terminal, persistErr
		}
		return false, terminal, resumeErr
	}
	if strings.TrimSpace(resumed.ID) == "" {
		return false, true, errors.New("AgentHub Resume returned no Session identity")
	}
	if !agentHubSessionExactlyMatchesGeneration(cfg, latest, resumed) {
		terminalErr := fmt.Errorf("Resume response for generation %s did not match its AgentHub source", latest.GenerationID)
		receipt.State = GenerationReceiptTerminal
		_, persistErr := rt.mutateGeneration(func(current *generationRecord) {
			if current.ID == latest.ID && current.GenerationID == latest.GenerationID {
				current.LifecycleReceipt = &receipt
				current.SessionResumeUnavailable = true
				current.Status = "stopped"
			}
		})
		if persistErr != nil {
			return false, true, persistErr
		}
		return false, true, terminalErr
	}

	current := rt.snapshotGeneration()
	if current.ID != latest.ID || current.GenerationID != latest.GenerationID || current.AgentHubSessionID != latest.AgentHubSessionID ||
		(strings.TrimSpace(plan.Guard.Revision) != "" && strings.TrimSpace(current.UpdatedAt) != strings.TrimSpace(plan.Guard.Revision)) {
		return false, false, nil
	}
	// Check the guarded boundary before projecting the response. A successful
	// Resume normally advances Session.updatedAt; applying that authoritative
	// response first would make the old plan revision look stale even though no
	// competing generation mutation occurred.
	stillCurrent, err := legacyLifecyclePlanStillCurrent(workspace, plan, &resumed)
	if err != nil {
		return false, false, err
	}
	if !stillCurrent {
		return false, false, nil
	}
	rt.applyAgentHubSessionState(m, resumed)
	receipt.State = GenerationReceiptSucceeded
	if _, err := rt.mutateGeneration(func(current *generationRecord) {
		if current.ID == latest.ID && current.GenerationID == latest.GenerationID && current.AgentHubSessionID == latest.AgentHubSessionID {
			current.LifecycleReceipt = &receipt
			current.SessionResumeUnavailable = false
			current.ResumeFailureCount = 0
			current.ResumeRetryAt = ""
			current.ResumeLastError = ""
		}
	}); err != nil {
		return false, false, err
	}
	return true, false, nil
}

func lifecycleResumeReceipt(plan GenerationLifecyclePlan) GenerationLifecycleReceipt {
	return GenerationLifecycleReceipt{
		Operation:    GenerationOperationResumeSession,
		State:        GenerationReceiptRequested,
		OperationID:  plan.OperationID,
		GenerationID: plan.GenerationID,
		SessionID:    plan.SessionID,
		TurnID:       plan.TurnID,
		MessageID:    plan.MessageID,
		Revision:     plan.Guard.Revision,
	}
}

func isTerminalResumeError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *agentHubAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode == http.StatusNotFound {
		return true
	}
	code := strings.ToLower(strings.TrimSpace(apiErr.Code))
	message := strings.ToLower(strings.TrimSpace(apiErr.Message))
	switch code {
	case "session_not_found", "session_archived", "session_source_mismatch", "resume_unavailable", "provider_resume_unavailable":
		return true
	}
	if apiErr.Retryable {
		return false
	}
	for _, phrase := range []string{
		"does not support session resume",
		"does not support session load",
		"resume/load",
		"native session resume",
		"native session load",
		"provider session cannot be resumed",
	} {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}

// retireUnresumableGenerationLocked is entered only after the Resume effect
// has reached an explicit terminal boundary. A transient failure never calls
// this function and leaves the mailbox queued for receipt/replan retry.
func (m *agentManager) retireUnresumableGenerationLocked(ctx context.Context, rt *agentRuntime, client *agentHubClient, reason error) error {
	if rt == nil {
		return nil
	}
	latest := rt.snapshotGeneration()
	if latest.Retired {
		return nil
	}
	_, err := rt.mutateGeneration(func(record *generationRecord) {
		record.SessionResumeUnavailable = true
		record.ReplacementPending = true
	})
	if err != nil {
		return err
	}

	session, sessionErr := client.GetSession(ctx, latest.AgentHubSessionID)
	if sessionErr != nil {
		if !isMissingAgentHubSessionError(sessionErr) {
			return sessionErr
		}
		updated, persistErr := rt.mutateGeneration(func(record *generationRecord) {
			record.Status = "stopped"
			record.AgentHubStoppedObserved = true
			record.ReplacementPending = false
			record.RetireReason = resumeRetireReason(reason)
		})
		if persistErr != nil {
			return persistErr
		}
		return retireStoredGeneration(rt, updated, resumeRetireReason(reason))
	}
	if session.State == "archived" {
		// An archived bound Session is already a terminal AgentHub boundary for
		// this Resume demand. Preserve the normal generation retirement path but
		// do not attempt to Resume or wait for a second provider proof; the
		// archived Session cannot accept a new Turn.
		retireReason := resumeRetireReason(reason)
		updated, persistErr := rt.mutateGeneration(func(record *generationRecord) {
			record.Status = "stopped"
			record.AgentHubStoppedObserved = true
			record.ReplacementPending = false
			record.IdleSleepStopRequested = false
			record.RetireReason = retireReason
		})
		if persistErr != nil {
			return persistErr
		}
		return retireStoredGeneration(rt, updated, retireReason)
	}
	return func() error {
		m.retireResourceGenerationLocked(ctx, rt)
		return nil
	}()
}

func isMissingAgentHubSessionError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *agentHubAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == http.StatusNotFound || strings.EqualFold(strings.TrimSpace(apiErr.Code), "session_not_found") ||
		strings.EqualFold(strings.TrimSpace(apiErr.Code), "session_archived")
}

func resumeRetireReason(err error) string {
	if err == nil {
		return "session_resume_unavailable"
	}
	return "session_resume_unavailable: " + strings.TrimSpace(err.Error())
}

// retireGenerationWithoutSession releases a generation after AgentHub has
// proved that its persisted Session identity cannot be recovered. Mailbox
// ownership remains durable and will create a fresh generation on demand.
func retireGenerationWithoutSession(rt *agentRuntime, reason string) error {
	if rt == nil {
		return nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "session_unavailable"
	}
	updated, err := rt.mutateGeneration(func(record *generationRecord) {
		record.Status = "stopped"
		record.AgentHubStoppedObserved = true
		record.SessionResumeUnavailable = true
		record.ReplacementPending = false
		record.IdleSleepStopRequested = false
		record.RetireReason = reason
	})
	if err != nil {
		return err
	}
	return retireStoredGeneration(rt, updated, reason)
}
