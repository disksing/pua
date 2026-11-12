package serve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
)

const (
	generationBudgetRetireReason     = "turn_limit"
	generationInactivityRetireReason = "inactivity_limit"
	generationUsagePageSize          = 500
)

type generationUsage struct {
	CompletedTurns int
	TurnDurationMS int64
	LatestEventID  int64
}

func generationBudgetReached(policy app.GenerationPolicy, usage generationUsage) bool {
	if !policy.BudgetEnabled {
		return false
	}
	if policy.MaxTurns > 0 && usage.CompletedTurns >= policy.MaxTurns {
		return true
	}
	return policy.MaxAccumulatedTurnMinutes > 0 &&
		usage.TurnDurationMS >= int64(policy.MaxAccumulatedTurnMinutes)*int64(time.Minute/time.Millisecond)
}

func generationInactivityReached(policy app.GenerationPolicy, session agentHubSession, now time.Time) bool {
	if !policy.InactivityEnabled || policy.MaxInactivityMinutes < 1 || now.IsZero() {
		return false
	}
	lastActivityAt := generationTime(session.LastActivityAt)
	if lastActivityAt.IsZero() || now.Before(lastActivityAt) {
		return false
	}
	return now.Sub(lastActivityAt) >= time.Duration(policy.MaxInactivityMinutes)*time.Minute
}

func generationUsageFromTurns(turns []agentHubTurn) generationUsage {
	usage := generationUsage{}
	seen := make(map[string]struct{}, len(turns))
	for _, turn := range turns {
		turnID := strings.TrimSpace(turn.TurnID)
		if turnID == "" {
			turnID = strings.TrimSpace(turn.ID)
		}
		if turnID == "" {
			continue
		}
		if _, exists := seen[turnID]; exists {
			continue
		}
		seen[turnID] = struct{}{}
		if !turn.Closed || !generationBudgetTerminalStatus(turn.Status) {
			continue
		}
		usage.CompletedTurns++
		duration := turn.DurationMS
		if duration <= 0 {
			started := generationTime(turn.StartedAt)
			ended := generationTime(firstNonEmpty(turn.EndedAt, turn.CompletedAt))
			if !started.IsZero() && ended.After(started) {
				duration = ended.Sub(started).Milliseconds()
			}
		}
		if duration > 0 {
			usage.TurnDurationMS += duration
		}
	}
	return usage
}

func generationBudgetTerminalStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "failed", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func fetchGenerationUsage(ctx context.Context, client *agentHubClient, sessionID string) (generationUsage, error) {
	if client == nil || strings.TrimSpace(sessionID) == "" {
		return generationUsage{}, errors.New("generation usage requires an AgentHub Session")
	}
	before := int64(0)
	latest := true
	turns := make([]agentHubTurn, 0)
	latestEventID := int64(0)
	for {
		page, err := client.SessionTurns(ctx, sessionID, before, latest, generationUsagePageSize)
		if err != nil {
			return generationUsage{}, err
		}
		turns = append(turns, page.Turns...)
		if page.LatestEventID > latestEventID {
			latestEventID = page.LatestEventID
		}
		if !page.Page.HasMoreBefore {
			break
		}
		if page.Page.NextBefore <= 0 || page.Page.NextBefore == before {
			return generationUsage{}, errors.New("AgentHub Turn pagination did not advance")
		}
		before = page.Page.NextBefore
		latest = false
	}
	usage := generationUsageFromTurns(turns)
	usage.LatestEventID = latestEventID
	return usage, nil
}

// prepareGenerationPolicyForNewTurnLocked evaluates the independent usage and
// inactivity rotation policies only when queued input is about to start a new
// Turn. A terminal Turn can therefore finish all completion bookkeeping before
// replacement, and an idle generation does no work until new input arrives. The
// caller owns the resource controller and has verified the exact inactive
// AgentHub Session.
func (m *agentManager) prepareGenerationPolicyForNewTurnLocked(ctx context.Context, workspace serveWorkspace, record generationRecord, observedSession agentHubSession, rt *agentRuntime, client *agentHubClient) (bool, error) {
	if m == nil || rt == nil || client == nil ||
		(observedSession.State != "ready" && observedSession.State != "stopped") {
		return false, nil
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		return false, err
	}
	runtimeConfig, err := puaWorkspace.RuntimeConfig()
	if err != nil || (!runtimeConfig.GenerationPolicy.BudgetEnabled && !runtimeConfig.GenerationPolicy.InactivityEnabled) {
		return false, err
	}
	current, found, err := currentResourceGeneration(workspace.Path, record.ResourceID)
	if err != nil || !found || current.GenerationID != record.GenerationID || current.AgentHubSessionID != observedSession.ID ||
		current.Retired || current.ReplacementPending || current.ArchivedTaskStopRequested {
		return false, err
	}
	usage := generationUsage{
		CompletedTurns: current.GenerationCompletedTurns,
		TurnDurationMS: current.GenerationTurnDurationMS,
		LatestEventID:  current.GenerationUsageEventID,
	}
	refreshUsage := func() (bool, error) {
		if current.GenerationUsageReady && current.GenerationUsageEventID >= observedSession.LastEventID {
			return true, nil
		}
		usage, err = fetchGenerationUsage(ctx, client, current.AgentHubSessionID)
		if err != nil {
			return false, fmt.Errorf("inspect generation Turn usage: %w", err)
		}
		if usage.LatestEventID < observedSession.LastEventID {
			return false, fmt.Errorf("AgentHub Turn projection cursor %d trails Session cursor %d", usage.LatestEventID, observedSession.LastEventID)
		}
		updated, persistErr := rt.mutateGeneration(func(candidate *generationRecord) {
			if candidate.GenerationID != current.GenerationID || candidate.AgentHubSessionID != observedSession.ID || candidate.Retired {
				return
			}
			candidate.GenerationCompletedTurns = usage.CompletedTurns
			candidate.GenerationTurnDurationMS = usage.TurnDurationMS
			candidate.GenerationUsageEventID = usage.LatestEventID
			candidate.GenerationUsageReady = true
		})
		if persistErr != nil {
			return false, fmt.Errorf("persist generation Turn usage: %w", persistErr)
		}
		if updated.GenerationID != current.GenerationID || updated.AgentHubSessionID != observedSession.ID {
			return false, nil
		}
		current = updated
		return true, nil
	}
	if runtimeConfig.GenerationPolicy.BudgetEnabled {
		currentStillMatches, refreshErr := refreshUsage()
		if refreshErr != nil || !currentStillMatches {
			return false, refreshErr
		}
	}

	// Re-read the policy after the AgentHub projection request so a concurrent
	// settings update takes effect at this same Turn boundary.
	runtimeConfig, err = puaWorkspace.RuntimeConfig()
	if err != nil {
		return false, err
	}
	if runtimeConfig.GenerationPolicy.BudgetEnabled {
		currentStillMatches, refreshErr := refreshUsage()
		if refreshErr != nil || !currentStillMatches {
			return false, refreshErr
		}
	}
	retireReason := ""
	if generationBudgetReached(runtimeConfig.GenerationPolicy, usage) {
		retireReason = generationBudgetRetireReason
	} else if generationInactivityReached(runtimeConfig.GenerationPolicy, observedSession, m.resourceNow()) {
		retireReason = generationInactivityRetireReason
	}
	if retireReason == "" {
		return false, nil
	}
	updated, err := rt.mutateGeneration(func(current *generationRecord) {
		if current.GenerationID != record.GenerationID || current.AgentHubSessionID != observedSession.ID ||
			current.Retired || current.ReplacementPending || current.ArchivedTaskStopRequested {
			return
		}
		current.ReplacementPending = true
		current.ManualStopRequested = false
		current.RetireReason = retireReason
		current.IdleSleepStopRequested = false
		current.ResumeFailureCount = 0
		current.ResumeRetryAt = ""
		current.ResumeLastError = ""
		current.UpdatedAt = m.resourceNow().Format(time.RFC3339Nano)
	})
	if err != nil || updated.GenerationID != record.GenerationID || !updated.ReplacementPending {
		return false, err
	}
	_ = m.enqueueResourceController(workspace, record.ResourceID, func() error {
		m.retireResourceGenerationLocked(context.WithoutCancel(ctx), rt)
		return nil
	})
	return true, nil
}
