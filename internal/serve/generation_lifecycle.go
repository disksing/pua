package serve

import (
	"errors"
	"strings"
	"time"
)

// GenerationLifecyclePhase is the canonical phase of one generation. It is
// deliberately independent from the legacy generationRecord status strings and from
// any AgentHub schema version.
type GenerationLifecyclePhase string

const (
	GenerationPhaseAbsent     GenerationLifecyclePhase = "absent"
	GenerationPhaseCreating   GenerationLifecyclePhase = "creating"
	GenerationPhaseReady      GenerationLifecyclePhase = "ready"
	GenerationPhaseActive     GenerationLifecyclePhase = "active"
	GenerationPhaseStopping   GenerationLifecyclePhase = "stopping"
	GenerationPhaseStopped    GenerationLifecyclePhase = "stopped"
	GenerationPhaseArchived   GenerationLifecyclePhase = "archived"
	GenerationPhaseRecovering GenerationLifecyclePhase = "recovering"
)

// GenerationLifecycleIntent describes why the current generation must leave
// its mutable state. An intent is durable state, while idle deadlines and
// binding comparisons are input facts that may create an intent during plan
// preparation.
type GenerationLifecycleIntent string

const (
	GenerationIntentNone        GenerationLifecycleIntent = "none"
	GenerationIntentIdle        GenerationLifecycleIntent = "idle"
	GenerationIntentReplacement GenerationLifecycleIntent = "replacement"
	GenerationIntentArchive     GenerationLifecycleIntent = "archive"
	GenerationIntentRecovery    GenerationLifecycleIntent = "recovery"
)

// GenerationLifecycleOperation is the only vocabulary understood by the
// planner. The executor owns the meaning of each operation and is responsible
// for running network effects outside store/controller locks.
type GenerationLifecycleOperation string

const (
	GenerationOperationNone                    GenerationLifecycleOperation = "none"
	GenerationOperationCreateGeneration        GenerationLifecycleOperation = "create_generation"
	GenerationOperationWaitForSession          GenerationLifecycleOperation = "wait_for_session"
	GenerationOperationDeliverMessage          GenerationLifecycleOperation = "deliver_message"
	GenerationOperationWaitForMessageReceipt   GenerationLifecycleOperation = "wait_for_message_receipt"
	GenerationOperationWaitForTurnTerminal     GenerationLifecycleOperation = "wait_for_turn_terminal"
	GenerationOperationInterruptTurn           GenerationLifecycleOperation = "interrupt_turn"
	GenerationOperationStopSession             GenerationLifecycleOperation = "stop_session"
	GenerationOperationResumeSession           GenerationLifecycleOperation = "resume_session"
	GenerationOperationWaitForStopped          GenerationLifecycleOperation = "wait_for_stopped"
	GenerationOperationArchiveSession          GenerationLifecycleOperation = "archive_session"
	GenerationOperationRetireGeneration        GenerationLifecycleOperation = "retire_generation"
	GenerationOperationObserveSession          GenerationLifecycleOperation = "observe_session"
	GenerationOperationFinalizeArchivedMailbox GenerationLifecycleOperation = "finalize_archived_mailbox"
)

// GenerationLifecycleReceiptState records the last durable boundary for a
// side effect. Unknown means the request may have reached AgentHub; retrying
// is therefore allowed only for operations with an explicit idempotent
// contract, while a later observation may advance the receipt without a new
// network call.
type GenerationLifecycleReceiptState string

const (
	GenerationReceiptNone      GenerationLifecycleReceiptState = "none"
	GenerationReceiptRequested GenerationLifecycleReceiptState = "requested"
	GenerationReceiptUnknown   GenerationLifecycleReceiptState = "unknown"
	GenerationReceiptSucceeded GenerationLifecycleReceiptState = "succeeded"
	GenerationReceiptRetryable GenerationLifecycleReceiptState = "retryable"
	GenerationReceiptTerminal  GenerationLifecycleReceiptState = "terminal"
)

const (
	GenerationMessageModeSteer     = "steer"
	GenerationMessageModeEnqueue   = "enqueue"
	GenerationMessageModeInterrupt = "interrupt"

	GenerationMessageStatusQueued       = "queued"
	GenerationMessageStatusDelivering   = "delivering"
	GenerationMessageStatusInterrupting = "interrupting"
)

// GenerationLifecycleReceipt identifies one operation attempt without using
// a process-local pointer or a clock. The operation ID is stable across
// retries and can be used by an executor to deduplicate effects.
type GenerationLifecycleReceipt struct {
	Operation    GenerationLifecycleOperation    `json:"operation,omitempty"`
	State        GenerationLifecycleReceiptState `json:"state,omitempty"`
	OperationID  string                          `json:"operationId,omitempty"`
	GenerationID string                          `json:"generationId,omitempty"`
	SessionID    string                          `json:"sessionId,omitempty"`
	TurnID       string                          `json:"turnId,omitempty"`
	MessageID    string                          `json:"messageId,omitempty"`
	Revision     string                          `json:"revision,omitempty"`
}

// GenerationLifecycleState is the canonical lifecycle projection. Store
// adapters may serialize this value, but the planner never interprets a
// storage version or a legacy field name.
type GenerationLifecycleState struct {
	Intent  GenerationLifecycleIntent  `json:"intent,omitempty"`
	Phase   GenerationLifecyclePhase   `json:"phase,omitempty"`
	Reason  string                     `json:"reason,omitempty"`
	Receipt GenerationLifecycleReceipt `json:"receipt,omitempty"`
}

// GenerationBindingFacts keep the resolved binding visible to the planner so
// a replacement plan is tied to the desired Agent binding, not merely to a
// boolean from one caller.
type GenerationBindingFacts struct {
	Kind            string `json:"kind,omitempty"`
	Name            string `json:"name,omitempty"`
	ResolvedAgent   string `json:"resolvedAgent,omitempty"`
	ProfileRevision string `json:"profileRevision,omitempty"`
}

// GenerationMessageFacts is the minimal mailbox fact needed to choose the
// next operation. The message body is intentionally absent: planning must not
// depend on payload content.
type GenerationMessageFacts struct {
	ID               string `json:"id,omitempty"`
	Status           string `json:"status,omitempty"`
	RequestedMode    string `json:"requestedMode,omitempty"`
	ActualMode       string `json:"actualMode,omitempty"`
	ModeFrozen       bool   `json:"modeFrozen,omitempty"`
	GenerationID     string `json:"generationId,omitempty"`
	SessionID        string `json:"sessionId,omitempty"`
	TurnID           string `json:"turnId,omitempty"`
	InterruptTurnID  string `json:"interruptTurnId,omitempty"`
	AgentHubAccepted bool   `json:"agentHubAccepted,omitempty"`
	// ProviderDeliveryPending means AgentHub durably accepted the stable input
	// but still needs an explicit same-ID confirmation after Provider recovery.
	ProviderDeliveryPending bool `json:"providerDeliveryPending,omitempty"`
}

// GenerationLifecycleFacts is a snapshot assembled by the store/runtime
// adapter. PlanGeneration is a pure function over this value: it performs no
// I/O, reads no clock, and never mutates the input.
type GenerationLifecycleFacts struct {
	WorkspaceInstanceID string `json:"workspaceInstanceId,omitempty"`
	ResourceID          string `json:"resourceId"`
	Revision            string `json:"revision,omitempty"`

	CurrentGeneration bool                     `json:"currentGeneration"`
	GenerationID      string                   `json:"generationId,omitempty"`
	GenerationNumber  int                      `json:"generation,omitempty"`
	Phase             GenerationLifecyclePhase `json:"phase,omitempty"`
	Binding           GenerationBindingFacts   `json:"binding,omitempty"`
	BindingChanged    bool                     `json:"bindingChanged,omitempty"`
	ResourceArchived  bool                     `json:"resourceArchived,omitempty"`

	SessionKnown bool   `json:"sessionKnown"`
	SessionID    string `json:"sessionId,omitempty"`
	SessionState string `json:"sessionState,omitempty"`
	// SessionResumable is an observation about the exact current Session, not
	// permission to create a replacement. A stopped Session remains resumable
	// until AgentHub reports an explicit terminal resume failure.
	SessionResumable         bool   `json:"sessionResumable"`
	SessionResumeUnavailable bool   `json:"sessionResumeUnavailable"`
	ResumeBackoffActive      bool   `json:"resumeBackoffActive"`
	TurnID                   string `json:"turnId,omitempty"`
	TurnActive               bool   `json:"turnActive"`
	ApprovalPending          bool   `json:"approvalPending"`
	SteerSupported           bool   `json:"steerSupported"`

	MailboxPending bool                    `json:"mailboxPending"`
	NextMessage    *GenerationMessageFacts `json:"nextMessage,omitempty"`

	IdleDeadlineDue bool                     `json:"idleDeadlineDue"`
	Lifecycle       GenerationLifecycleState `json:"lifecycle,omitempty"`
}

// GenerationLifecycleGuard is copied into a plan and checked again immediately
// before a local commit. A network response must never be committed when any
// identity or revision it was planned against has changed.
type GenerationLifecycleGuard struct {
	WorkspaceInstanceID string                    `json:"workspaceInstanceId,omitempty"`
	ResourceID          string                    `json:"resourceId"`
	Revision            string                    `json:"revision,omitempty"`
	LifecycleIntent     GenerationLifecycleIntent `json:"lifecycleIntent,omitempty"`
	GenerationID        string                    `json:"generationId,omitempty"`
	SessionID           string                    `json:"sessionId,omitempty"`
	TurnID              string                    `json:"turnId,omitempty"`
	MessageID           string                    `json:"messageId,omitempty"`
}

// GenerationLifecyclePlan describes exactly one next step. A plan with a
// non-empty BlockedReason has no executable operation.
type GenerationLifecyclePlan struct {
	Operation     GenerationLifecycleOperation `json:"operation"`
	Intent        GenerationLifecycleIntent    `json:"intent"`
	Reason        string                       `json:"reason,omitempty"`
	OperationID   string                       `json:"operationId,omitempty"`
	GenerationID  string                       `json:"generationId,omitempty"`
	SessionID     string                       `json:"sessionId,omitempty"`
	TurnID        string                       `json:"turnId,omitempty"`
	MessageID     string                       `json:"messageId,omitempty"`
	MessageMode   string                       `json:"messageMode,omitempty"`
	Guard         GenerationLifecycleGuard     `json:"guard"`
	BlockedReason string                       `json:"blockedReason,omitempty"`
}

// PlanGeneration deterministically chooses the safest next operation for one
// resource. Its priority is:
//
//  1. finalize archived mailbox items;
//  2. converge archive/replacement/idle lifecycle intent;
//  3. recover an in-flight lifecycle/mailbox receipt;
//  4. deliver or interrupt the next message;
//  5. create a generation for pending work;
//  6. start idle retirement only at a proven ready boundary.
//
// Lifecycle intent always wins over delivery. In particular, no plan sends a
// message to a stopping or archived Session; a stopped resumable Session first
// receives the explicit ResumeSession operation.
func PlanGeneration(facts GenerationLifecycleFacts) GenerationLifecyclePlan {
	facts = normalizeGenerationFacts(facts)
	plan := GenerationLifecyclePlan{
		Operation:    GenerationOperationNone,
		Intent:       facts.Lifecycle.Intent,
		GenerationID: facts.GenerationID,
		SessionID:    facts.SessionID,
		TurnID:       facts.TurnID,
		Guard:        generationLifecycleGuard(facts),
	}
	if facts.ResourceID == "" {
		plan.BlockedReason = "resource id is required"
		return plan
	}

	intent, reason := generationLifecycleIntent(facts)
	plan.Intent = intent
	plan.Reason = reason
	plan.Guard.LifecycleIntent = intent

	if facts.ResourceArchived && facts.MailboxPending {
		return finishGenerationPlan(plan, GenerationOperationFinalizeArchivedMailbox, reasonOr(reason, "resource_archived"), facts.NextMessage)
	}
	if !facts.CurrentGeneration {
		if facts.ResourceArchived {
			return plan
		}
		if facts.MailboxPending {
			return finishGenerationPlan(plan, GenerationOperationCreateGeneration, reasonOr(reason, "message_waiting"), facts.NextMessage)
		}
		return plan
	}
	if facts.MailboxPending && facts.NextMessage != nil &&
		(facts.NextMessage.Status == GenerationMessageStatusDelivering || facts.NextMessage.Status == GenerationMessageStatusInterrupting || facts.NextMessage.AgentHubAccepted) {
		return planMessage(plan, facts)
	}

	if intent != GenerationIntentNone {
		return planLifecycleIntent(plan, facts, intent, reason)
	}

	if receiptPlan, ok := planReceiptRecovery(plan, facts); ok {
		return receiptPlan
	}

	phase := observedGenerationPhase(facts)
	if phase == GenerationPhaseArchived {
		return finishGenerationPlan(plan, GenerationOperationRetireGeneration, "session_archived", nil)
	}
	if phase == GenerationPhaseStopped && facts.MailboxPending && facts.NextMessage != nil &&
		facts.SessionKnown && facts.SessionResumable && !facts.SessionResumeUnavailable {
		if facts.ResumeBackoffActive {
			return finishGenerationPlan(plan, GenerationOperationWaitForSession, "resume_backoff", facts.NextMessage)
		}
		return finishGenerationPlan(plan, GenerationOperationResumeSession, "resume_stopped_session", facts.NextMessage)
	}
	if phase == GenerationPhaseStopped && facts.SessionResumeUnavailable {
		return finishGenerationPlan(plan, GenerationOperationArchiveSession, "session_resume_unavailable", nil)
	}
	if phase == GenerationPhaseStopped {
		// A stopped current generation is intentionally retained. It is resumed
		// only when a mailbox item creates demand; no-message observations do not
		// start provider work or retire the generation.
		return plan
	}
	if phase == GenerationPhaseCreating || phase == GenerationPhaseRecovering || !facts.SessionKnown {
		return finishGenerationPlan(plan, GenerationOperationWaitForSession, "session_state_not_ready", nil)
	}

	if facts.MailboxPending && facts.NextMessage != nil {
		return planMessage(plan, facts)
	}
	if facts.IdleDeadlineDue && phase == GenerationPhaseReady && !facts.TurnActive && !facts.ApprovalPending {
		plan.Intent = GenerationIntentIdle
		return finishGenerationPlan(plan, GenerationOperationStopSession, "idle_deadline", nil)
	}
	return plan
}

func planLifecycleIntent(plan GenerationLifecyclePlan, facts GenerationLifecycleFacts, intent GenerationLifecycleIntent, reason string) GenerationLifecyclePlan {
	phase := observedGenerationPhase(facts)
	if intent == GenerationIntentIdle && phase == GenerationPhaseStopped && !facts.SessionResumeUnavailable {
		// Idle sleep is a reversible current-generation state. Once Stop has
		// converged, hold the exact Session until a mailbox item asks for Resume.
		return plan
	}
	if intent == GenerationIntentReplacement && facts.TurnActive && facts.NextMessage != nil &&
		facts.NextMessage.RequestedMode == GenerationMessageModeInterrupt &&
		facts.NextMessage.Status == GenerationMessageStatusQueued {
		return planMessage(plan, facts)
	}
	if facts.TurnActive || facts.ApprovalPending || phase == GenerationPhaseActive {
		return finishGenerationPlan(plan, GenerationOperationWaitForTurnTerminal, reasonOr(reason, "active_turn"), nil)
	}
	if !facts.SessionKnown || phase == GenerationPhaseCreating || phase == GenerationPhaseRecovering {
		return finishGenerationPlan(plan, GenerationOperationWaitForSession, reasonOr(reason, "session_state_not_ready"), nil)
	}
	switch phase {
	case GenerationPhaseReady:
		return finishGenerationPlan(plan, GenerationOperationStopSession, reasonOr(reason, string(intent)), nil)
	case GenerationPhaseStopping:
		return finishGenerationPlan(plan, GenerationOperationWaitForStopped, reasonOr(reason, "stop_in_flight"), nil)
	case GenerationPhaseStopped:
		return finishGenerationPlan(plan, GenerationOperationArchiveSession, reasonOr(reason, "stopped"), nil)
	case GenerationPhaseArchived:
		return finishGenerationPlan(plan, GenerationOperationRetireGeneration, reasonOr(reason, "archived"), nil)
	default:
		return finishGenerationPlan(plan, GenerationOperationObserveSession, reasonOr(reason, "unknown_session_state"), nil)
	}
}

func planReceiptRecovery(plan GenerationLifecyclePlan, facts GenerationLifecycleFacts) (GenerationLifecyclePlan, bool) {
	receipt := facts.Lifecycle.Receipt
	if receipt.Operation == GenerationOperationNone || receipt.State == GenerationReceiptNone || receipt.State == GenerationReceiptSucceeded || receipt.State == GenerationReceiptTerminal {
		return GenerationLifecyclePlan{}, false
	}
	if receipt.Operation == GenerationOperationDeliverMessage || receipt.Operation == GenerationOperationInterruptTurn {
		if facts.MailboxPending {
			return finishGenerationPlan(plan, GenerationOperationWaitForMessageReceipt, "message_receipt_pending", facts.NextMessage), true
		}
		return GenerationLifecyclePlan{}, false
	}
	if receipt.Operation == GenerationOperationStopSession || receipt.Operation == GenerationOperationArchiveSession {
		intent := facts.Lifecycle.Intent
		if intent == GenerationIntentNone {
			intent = GenerationIntentRecovery
		}
		plan.Intent = intent
		plan.Reason = reasonOr(facts.Lifecycle.Reason, "lifecycle_receipt_pending")
		if receipt.Operation == GenerationOperationStopSession && receipt.State == GenerationReceiptRetryable && observedGenerationPhase(facts) == GenerationPhaseReady {
			return finishGenerationPlan(plan, GenerationOperationStopSession, "retry_stop", nil), true
		}
		if receipt.Operation == GenerationOperationArchiveSession && observedGenerationPhase(facts) == GenerationPhaseStopped {
			return finishGenerationPlan(plan, GenerationOperationArchiveSession, "retry_archive", nil), true
		}
		return planLifecycleIntent(plan, facts, intent, plan.Reason), true
	}
	if receipt.Operation == GenerationOperationResumeSession {
		if facts.SessionResumeUnavailable {
			plan.Intent = GenerationIntentRecovery
			return planLifecycleIntent(plan, facts, GenerationIntentRecovery, "resume_terminal_failure"), true
		}
		if facts.MailboxPending && facts.NextMessage != nil && observedGenerationPhase(facts) == GenerationPhaseStopped &&
			facts.SessionKnown && facts.SessionResumable {
			if facts.ResumeBackoffActive {
				return finishGenerationPlan(plan, GenerationOperationWaitForSession, "resume_backoff", facts.NextMessage), true
			}
			return finishGenerationPlan(plan, GenerationOperationResumeSession, "retry_resume", facts.NextMessage), true
		}
		return GenerationLifecyclePlan{}, false
	}
	return GenerationLifecyclePlan{}, false
}

func planMessage(plan GenerationLifecyclePlan, facts GenerationLifecycleFacts) GenerationLifecyclePlan {
	message := facts.NextMessage
	if message == nil || strings.TrimSpace(message.ID) == "" {
		return finishGenerationPlan(plan, GenerationOperationObserveSession, "mailbox_fact_incomplete", nil)
	}
	plan.MessageID = strings.TrimSpace(message.ID)
	plan.MessageMode = normalizedGenerationMessageMode(message)
	phase := observedGenerationPhase(facts)
	if message.Status == GenerationMessageStatusDelivering && message.ProviderDeliveryPending {
		if !facts.SessionKnown || phase == GenerationPhaseCreating || phase == GenerationPhaseRecovering || phase == GenerationPhaseAbsent {
			return finishGenerationPlan(plan, GenerationOperationWaitForSession, "session_state_not_ready", message)
		}
		switch phase {
		case GenerationPhaseStopping, GenerationPhaseArchived:
			return finishGenerationPlan(plan, GenerationOperationWaitForStopped, "generation_not_deliverable", message)
		case GenerationPhaseStopped:
			if facts.SessionKnown && facts.SessionResumable && !facts.SessionResumeUnavailable {
				if facts.ResumeBackoffActive {
					return finishGenerationPlan(plan, GenerationOperationWaitForSession, "resume_backoff", message)
				}
				return finishGenerationPlan(plan, GenerationOperationResumeSession, "resume_pending_provider_delivery", message)
			}
			return finishGenerationPlan(plan, GenerationOperationWaitForStopped, "generation_not_deliverable", message)
		default:
			return finishGenerationPlan(plan, GenerationOperationDeliverMessage, "confirm_pending_provider_delivery", message)
		}
	}
	if message.Status == GenerationMessageStatusDelivering || message.Status == GenerationMessageStatusInterrupting || message.AgentHubAccepted {
		return finishGenerationPlan(plan, GenerationOperationWaitForMessageReceipt, "message_receipt_pending", message)
	}
	if phase == GenerationPhaseStopping || phase == GenerationPhaseArchived {
		return finishGenerationPlan(plan, GenerationOperationWaitForStopped, "generation_not_deliverable", message)
	}
	if phase == GenerationPhaseStopped {
		if facts.SessionKnown && facts.SessionResumable && !facts.SessionResumeUnavailable {
			if facts.ResumeBackoffActive {
				return finishGenerationPlan(plan, GenerationOperationWaitForSession, "resume_backoff", message)
			}
			return finishGenerationPlan(plan, GenerationOperationResumeSession, "resume_stopped_session", message)
		}
		return finishGenerationPlan(plan, GenerationOperationWaitForStopped, "generation_not_deliverable", message)
	}
	active := facts.TurnActive || facts.ApprovalPending || phase == GenerationPhaseActive || facts.SessionState == "running" || facts.SessionState == "waiting_approval"
	if message.RequestedMode == GenerationMessageModeInterrupt && active {
		plan.TurnID = firstNonEmpty(strings.TrimSpace(message.InterruptTurnID), facts.TurnID)
		return finishGenerationPlan(plan, GenerationOperationInterruptTurn, "interrupt_requested", message)
	}
	if active {
		if plan.MessageMode == GenerationMessageModeSteer && facts.SteerSupported {
			return finishGenerationPlan(plan, GenerationOperationDeliverMessage, "steer_requested", message)
		}
		return finishGenerationPlan(plan, GenerationOperationWaitForTurnTerminal, "enqueue_waits_for_turn", message)
	}
	if phase != GenerationPhaseReady {
		return finishGenerationPlan(plan, GenerationOperationWaitForSession, "session_not_ready", message)
	}
	return finishGenerationPlan(plan, GenerationOperationDeliverMessage, "message_waiting", message)
}

func finishGenerationPlan(plan GenerationLifecyclePlan, operation GenerationLifecycleOperation, reason string, message *GenerationMessageFacts) GenerationLifecyclePlan {
	plan.Operation = operation
	plan.Reason = reasonOr(plan.Reason, reason)
	if message != nil {
		plan.MessageID = strings.TrimSpace(message.ID)
		plan.MessageMode = normalizedGenerationMessageMode(message)
		if plan.Guard.MessageID == "" {
			plan.Guard.MessageID = plan.MessageID
		}
		if plan.TurnID == "" {
			plan.TurnID = firstNonEmpty(strings.TrimSpace(message.TurnID), strings.TrimSpace(message.InterruptTurnID), plan.TurnID)
		}
	}
	plan.OperationID = lifecycleOperationID(plan)
	return plan
}

func lifecycleOperationID(plan GenerationLifecyclePlan) string {
	return strings.Join([]string{
		string(plan.Operation), plan.GenerationID, plan.SessionID, plan.TurnID, plan.MessageID,
	}, "\x00")
}

func generationLifecycleIntent(facts GenerationLifecycleFacts) (GenerationLifecycleIntent, string) {
	intent := facts.Lifecycle.Intent
	reason := strings.TrimSpace(facts.Lifecycle.Reason)
	if facts.ResourceArchived {
		return GenerationIntentArchive, reasonOr(reason, "resource_archived")
	}
	if facts.BindingChanged && intent != GenerationIntentArchive {
		return GenerationIntentReplacement, reasonOr(reason, "binding_changed")
	}
	if facts.SessionResumeUnavailable && intent != GenerationIntentArchive && intent != GenerationIntentReplacement {
		return GenerationIntentRecovery, reasonOr(reason, "session_resume_unavailable")
	}
	if intent == GenerationIntentIdle && facts.MailboxPending {
		return GenerationIntentNone, ""
	}
	if intent == GenerationIntentIdle && (facts.TurnActive || facts.ApprovalPending) {
		return GenerationIntentNone, ""
	}
	if intent == GenerationIntentNone && facts.IdleDeadlineDue && !facts.MailboxPending && !facts.TurnActive && !facts.ApprovalPending {
		return GenerationIntentIdle, "idle_deadline"
	}
	return intent, reason
}

func generationLifecycleGuard(facts GenerationLifecycleFacts) GenerationLifecycleGuard {
	intent, _ := generationLifecycleIntent(facts)
	return GenerationLifecycleGuard{
		WorkspaceInstanceID: strings.TrimSpace(facts.WorkspaceInstanceID),
		ResourceID:          strings.TrimSpace(facts.ResourceID),
		Revision:            strings.TrimSpace(facts.Revision),
		LifecycleIntent:     intent,
		GenerationID:        strings.TrimSpace(facts.GenerationID),
		SessionID:           strings.TrimSpace(facts.SessionID),
		TurnID:              strings.TrimSpace(facts.TurnID),
	}
}

func normalizeGenerationFacts(facts GenerationLifecycleFacts) GenerationLifecycleFacts {
	facts.WorkspaceInstanceID = strings.TrimSpace(facts.WorkspaceInstanceID)
	facts.ResourceID = strings.TrimSpace(facts.ResourceID)
	facts.Revision = strings.TrimSpace(facts.Revision)
	facts.GenerationID = strings.TrimSpace(facts.GenerationID)
	facts.SessionID = strings.TrimSpace(facts.SessionID)
	facts.SessionState = strings.TrimSpace(facts.SessionState)
	facts.TurnID = strings.TrimSpace(facts.TurnID)
	if facts.SessionKnown && facts.SessionState == "stopped" && facts.SessionID != "" && !facts.SessionResumeUnavailable {
		// A present, non-archived stopped Session is resumable by default. The
		// adapter may set this fact explicitly, but deriving it here keeps the
		// pure planner safe for all store adapters until AgentHub reports an
		// explicit terminal resume failure.
		facts.SessionResumable = true
	}
	facts.Lifecycle.Reason = strings.TrimSpace(facts.Lifecycle.Reason)
	if facts.Lifecycle.Intent == "" {
		facts.Lifecycle.Intent = GenerationIntentNone
	}
	if facts.Lifecycle.Phase == "" {
		facts.Lifecycle.Phase = facts.Phase
	}
	if facts.Lifecycle.Receipt.State == "" {
		facts.Lifecycle.Receipt.State = GenerationReceiptNone
	}
	if facts.NextMessage != nil {
		message := *facts.NextMessage
		message.ID = strings.TrimSpace(message.ID)
		message.Status = strings.TrimSpace(message.Status)
		message.RequestedMode = strings.TrimSpace(message.RequestedMode)
		message.ActualMode = strings.TrimSpace(message.ActualMode)
		message.GenerationID = strings.TrimSpace(message.GenerationID)
		message.SessionID = strings.TrimSpace(message.SessionID)
		message.TurnID = strings.TrimSpace(message.TurnID)
		message.InterruptTurnID = strings.TrimSpace(message.InterruptTurnID)
		facts.NextMessage = &message
	}
	if facts.GenerationID != "" {
		facts.CurrentGeneration = true
	}
	return facts
}

func observedGenerationPhase(facts GenerationLifecycleFacts) GenerationLifecyclePhase {
	if facts.SessionKnown {
		switch facts.SessionState {
		case "starting":
			return GenerationPhaseCreating
		case "ready":
			return GenerationPhaseReady
		case "running", "waiting_approval":
			return GenerationPhaseActive
		case "stopping":
			return GenerationPhaseStopping
		case "stopped":
			return GenerationPhaseStopped
		case "archived":
			return GenerationPhaseArchived
		}
	}
	if facts.Phase != "" {
		return facts.Phase
	}
	return facts.Lifecycle.Phase
}

func normalizedGenerationMessageMode(message *GenerationMessageFacts) string {
	if message == nil {
		return ""
	}
	if strings.TrimSpace(message.ActualMode) != "" {
		return strings.TrimSpace(message.ActualMode)
	}
	mode := strings.TrimSpace(message.RequestedMode)
	if mode == "" {
		return GenerationMessageModeEnqueue
	}
	return mode
}

func reasonOr(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(fallback)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// LifecycleGuardMatchesFacts verifies that a plan still owns the exact
// resource/generation/session/turn/message snapshot it was created from.
// Empty optional guard fields mean the operation did not depend on that
// identity. This function is safe to call after re-reading facts under the
// resource controller; it does not acquire any lock itself.
func LifecycleGuardMatchesFacts(plan GenerationLifecyclePlan, facts GenerationLifecycleFacts) bool {
	guard := plan.Guard
	current := generationLifecycleGuard(normalizeGenerationFacts(facts))
	return equalGuardField(guard.WorkspaceInstanceID, current.WorkspaceInstanceID) &&
		equalGuardField(guard.ResourceID, current.ResourceID) &&
		equalGuardField(guard.Revision, current.Revision) &&
		equalGuardField(string(guard.LifecycleIntent), string(current.LifecycleIntent)) &&
		equalGuardField(guard.GenerationID, current.GenerationID) &&
		equalGuardField(guard.SessionID, current.SessionID) &&
		equalGuardField(guard.TurnID, current.TurnID) &&
		equalGuardField(guard.MessageID, nextMessageID(facts))
}

// GuardedLifecycleCommit invokes commit only when the plan still matches the
// newly-read facts. A false result is a normal stale-result outcome: callers
// must discard the network result and re-plan.
func GuardedLifecycleCommit(plan GenerationLifecyclePlan, facts GenerationLifecycleFacts, commit func() error) (bool, error) {
	if !LifecycleGuardMatchesFacts(plan, facts) {
		return false, nil
	}
	if commit == nil {
		return false, errors.New("lifecycle commit is nil")
	}
	return true, commit()
}

// legacyLifecyclePlanStillCurrent re-reads the current generation store
// boundary after a
// network effect. It is intentionally small and read-only; the caller decides
// how to re-plan when the result is stale.
func legacyLifecyclePlanStillCurrent(workspace serveWorkspace, plan GenerationLifecyclePlan, session *agentHubSession) (bool, error) {
	record, found, err := currentGenerationRecordByID(workspace.Path, plan.Guard.ResourceID, plan.GenerationID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	mailbox, err := loadHotResourceMailbox(workspace.Path, plan.Guard.ResourceID)
	if err != nil {
		return false, err
	}
	facts := AdaptLegacyGenerationFacts(LegacyGenerationLifecycleInput{
		Generation: record, Session: session, Mailbox: mailbox, Revision: record.UpdatedAt,
	})
	return LifecycleGuardMatchesFacts(plan, facts), nil
}

func equalGuardField(expected, actual string) bool {
	expected = strings.TrimSpace(expected)
	return expected == "" || expected == strings.TrimSpace(actual)
}

func nextMessageID(facts GenerationLifecycleFacts) string {
	if facts.NextMessage == nil {
		return ""
	}
	return strings.TrimSpace(facts.NextMessage.ID)
}

// LegacyGenerationLifecycleInput is the compatibility boundary for the
// legacy generation/mailbox projection. It is intentionally the only place
// where old flags and old status values are translated into canonical
// lifecycle facts. The resource-scoped generation store owns persistence;
// changing its adapter does not change PlanGeneration.
type LegacyGenerationLifecycleInput struct {
	Generation            generationRecord
	Session               *agentHubSession
	ResourceArchived      bool
	BindingChanged        bool
	Mailbox               resourceMailbox
	WorkspaceInstanceID   string
	Revision              string
	Now                   time.Time
	AgentHubStopRequested bool
	LifecycleStopInFlight bool
}

// AdaptLegacyGenerationFacts converts the old in-memory/durable projection at
// the store boundary. It does not read files or call time.Now; callers supply
// all observations explicitly.
func AdaptLegacyGenerationFacts(input LegacyGenerationLifecycleInput) GenerationLifecycleFacts {
	record := input.Generation
	facts := GenerationLifecycleFacts{
		WorkspaceInstanceID: strings.TrimSpace(input.WorkspaceInstanceID),
		ResourceID:          normalizedResourceID(record.ResourceID),
		Revision:            strings.TrimSpace(input.Revision),
		CurrentGeneration:   strings.TrimSpace(record.GenerationID) != "" || strings.TrimSpace(record.AgentHubSessionID) != "",
		GenerationID:        strings.TrimSpace(record.GenerationID),
		GenerationNumber:    record.Generation,
		Phase:               legacyGenerationPhase(record.Status),
		Binding: GenerationBindingFacts{
			Kind:            strings.TrimSpace(record.BindingKind),
			Name:            strings.TrimSpace(record.BindingName),
			ResolvedAgent:   strings.TrimSpace(record.AgentHubAgentName),
			ProfileRevision: strings.TrimSpace(record.ProfileRevision),
		},
		BindingChanged:   input.BindingChanged,
		ResourceArchived: input.ResourceArchived,
		SessionID:        strings.TrimSpace(record.AgentHubSessionID),
		Lifecycle: GenerationLifecycleState{
			Intent: legacyLifecycleIntent(record, input.ResourceArchived),
			Phase:  legacyGenerationPhase(record.Status),
			Reason: legacyLifecycleReason(record, input.ResourceArchived),
		},
	}
	if facts.Revision == "" {
		facts.Revision = strings.TrimSpace(record.UpdatedAt)
	}
	if input.Session != nil {
		session := input.Session
		facts.SessionKnown = true
		facts.SessionID = firstNonEmpty(session.ID, facts.SessionID)
		facts.SessionState = strings.TrimSpace(session.State)
		facts.TurnID = activeLegacyTurnID(*session)
		facts.TurnActive = session.State == "running" || session.State == "waiting_approval"
		facts.ApprovalPending = len(session.PendingApprovalIDs) > 0 || session.State == "waiting_approval"
		facts.SteerSupported = session.InputCapabilities.Steer
		facts.Phase = legacyGenerationPhase(session.State)
		facts.Lifecycle.Phase = facts.Phase
		facts.SessionResumable = session.State == "stopped" && strings.TrimSpace(facts.SessionID) != ""
	}
	facts.SessionResumeUnavailable = record.SessionResumeUnavailable
	if !input.Now.IsZero() && strings.TrimSpace(record.ResumeRetryAt) != "" {
		if retryAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(record.ResumeRetryAt)); err == nil {
			facts.ResumeBackoffActive = input.Now.Before(retryAt)
		}
	}
	if record.LifecycleReceipt != nil {
		facts.Lifecycle.Receipt = *record.LifecycleReceipt
	}
	if facts.TurnID == "" && !facts.SessionKnown {
		facts.TurnID = strings.TrimSpace(record.CurrentTurnID)
	}
	facts.MailboxPending, facts.NextMessage = mailboxFacts(input.Mailbox, facts.ResourceID)
	if facts.TurnID == "" && facts.NextMessage != nil {
		facts.TurnID = firstNonEmpty(facts.NextMessage.InterruptTurnID, facts.NextMessage.TurnID)
	}
	if !input.Now.IsZero() && strings.TrimSpace(record.IdleDeadlineAt) != "" {
		if deadline, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(record.IdleDeadlineAt)); err == nil {
			facts.IdleDeadlineDue = !input.Now.Before(deadline)
		}
	}
	if input.AgentHubStopRequested || input.LifecycleStopInFlight {
		facts.Lifecycle.Receipt = GenerationLifecycleReceipt{
			Operation:    GenerationOperationStopSession,
			State:        GenerationReceiptUnknown,
			GenerationID: facts.GenerationID,
			SessionID:    facts.SessionID,
			Revision:     facts.Revision,
		}
	}
	return normalizeGenerationFacts(facts)
}

// ApplyLegacyLifecyclePlan is the reverse compatibility mapping. It is for a
// guarded store commit only; the planner never calls it and never sees these
// field names.
func ApplyLegacyLifecyclePlan(record *generationRecord, plan GenerationLifecyclePlan) {
	if record == nil {
		return
	}
	switch plan.Intent {
	case GenerationIntentArchive:
		record.ArchivedTaskStopRequested = true
	case GenerationIntentReplacement:
		record.ReplacementPending = true
	case GenerationIntentIdle:
		record.IdleSleepStopRequested = true
	}
	switch plan.Operation {
	case GenerationOperationStopSession, GenerationOperationWaitForStopped:
		record.Status = "stopping"
	case GenerationOperationResumeSession:
		record.Status = "starting"
		record.AgentHubStoppedObserved = false
	case GenerationOperationArchiveSession, GenerationOperationRetireGeneration:
		record.Status = "stopped"
	}
	if plan.Operation == GenerationOperationStopSession || plan.Operation == GenerationOperationResumeSession || plan.Operation == GenerationOperationArchiveSession {
		receipt := GenerationLifecycleReceipt{
			Operation:    plan.Operation,
			State:        GenerationReceiptRequested,
			OperationID:  plan.OperationID,
			GenerationID: plan.GenerationID,
			SessionID:    plan.SessionID,
			TurnID:       plan.TurnID,
			MessageID:    plan.MessageID,
			Revision:     plan.Guard.Revision,
		}
		record.LifecycleReceipt = &receipt
	}
	if plan.Operation == GenerationOperationRetireGeneration {
		record.ReplacementPending = false
		record.IdleSleepStopRequested = false
		record.ArchivedTaskStopRequested = false
		record.AgentHubStoppedObserved = true
	}
}

func legacyGenerationPhase(status string) GenerationLifecyclePhase {
	switch strings.TrimSpace(status) {
	case "starting":
		return GenerationPhaseCreating
	case "idle", "ready":
		return GenerationPhaseReady
	case "idle-suspended":
		return GenerationPhaseStopped
	case "running", "waiting_approval":
		return GenerationPhaseActive
	case "stopping":
		return GenerationPhaseStopping
	case "stopped":
		return GenerationPhaseStopped
	case "archived":
		return GenerationPhaseArchived
	case "recovering", "failed":
		return GenerationPhaseRecovering
	default:
		return GenerationPhaseAbsent
	}
}

func legacyLifecycleIntent(record generationRecord, resourceArchived bool) GenerationLifecycleIntent {
	if resourceArchived || record.ArchivedTaskStopRequested {
		return GenerationIntentArchive
	}
	if record.ReplacementPending {
		return GenerationIntentReplacement
	}
	if record.SessionResumeUnavailable {
		return GenerationIntentRecovery
	}
	if record.IdleSleepStopRequested {
		return GenerationIntentIdle
	}
	return GenerationIntentNone
}

func legacyLifecycleReason(record generationRecord, resourceArchived bool) string {
	if resourceArchived || record.ArchivedTaskStopRequested {
		return "resource_archived"
	}
	if record.ReplacementPending {
		if record.ManualStopRequested {
			return "manual_generation_stop"
		}
		if reason := strings.TrimSpace(record.RetireReason); reason != "" {
			return reason
		}
		return "binding_changed"
	}
	if record.SessionResumeUnavailable {
		return "session_resume_unavailable"
	}
	if record.IdleSleepStopRequested {
		return "idle_deadline"
	}
	return ""
}

func activeLegacyTurnID(session agentHubSession) string {
	if session.State != "running" && session.State != "waiting_approval" {
		return ""
	}
	return strings.TrimSpace(session.CurrentTurnID)
}

func mailboxFacts(mailbox resourceMailbox, resourceID string) (bool, *GenerationMessageFacts) {
	message, found := selectPendingMailboxMessage(mailbox, resourceID)
	if found {
		mode := strings.TrimSpace(message.RequestedMode)
		if mode == "" {
			mode = GenerationMessageModeEnqueue
		}
		return true, &GenerationMessageFacts{
			ID:                      message.ID,
			Status:                  message.Status,
			RequestedMode:           mode,
			ActualMode:              message.ActualMode,
			ModeFrozen:              message.ModeFrozen,
			GenerationID:            message.GenerationID,
			SessionID:               message.AgentHubSessionID,
			TurnID:                  message.TurnID,
			InterruptTurnID:         message.InterruptTurnID,
			AgentHubAccepted:        message.Status == resourceMessageDelivered,
			ProviderDeliveryPending: message.ProviderDeliveryPending,
		}
	}
	return false, nil
}
