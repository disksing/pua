package serve

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func testGenerationFacts() GenerationLifecycleFacts {
	return GenerationLifecycleFacts{
		WorkspaceInstanceID: "ws-1",
		ResourceID:          "project1.task1",
		Revision:            "rev-1",
		CurrentGeneration:   true,
		GenerationID:        "gen-1",
		Phase:               GenerationPhaseReady,
		SessionKnown:        true,
		SessionID:           "ses-1",
		SessionState:        "ready",
	}
}

func generationMessage(mode, status string) *GenerationMessageFacts {
	return &GenerationMessageFacts{ID: "msg-1", RequestedMode: mode, Status: status}
}

func TestPlanGenerationLifecycleTable(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*GenerationLifecycleFacts)
		operation GenerationLifecycleOperation
		intent    GenerationLifecycleIntent
		reason    string
	}{
		{
			name:      "missing resource is blocked",
			mutate:    func(f *GenerationLifecycleFacts) { f.ResourceID = "" },
			operation: GenerationOperationNone,
		},
		{
			name: "first pending message creates generation",
			mutate: func(f *GenerationLifecycleFacts) {
				f.CurrentGeneration = false
				f.GenerationID = ""
				f.SessionKnown = false
				f.NextMessage = generationMessage(GenerationMessageModeEnqueue, GenerationMessageStatusQueued)
				f.MailboxPending = true
			},
			operation: GenerationOperationCreateGeneration,
			reason:    "message_waiting",
		},
		{
			name: "archived resource with no generation does not create",
			mutate: func(f *GenerationLifecycleFacts) {
				f.CurrentGeneration = false
				f.GenerationID = ""
				f.SessionKnown = false
				f.ResourceArchived = true
			},
			operation: GenerationOperationNone,
		},
		{
			name: "archived mailbox is finalized before lifecycle network work",
			mutate: func(f *GenerationLifecycleFacts) {
				f.ResourceArchived = true
				f.MailboxPending = true
				f.NextMessage = generationMessage(GenerationMessageModeEnqueue, GenerationMessageStatusQueued)
			},
			operation: GenerationOperationFinalizeArchivedMailbox,
			intent:    GenerationIntentArchive,
			reason:    "resource_archived",
		},
		{
			name: "archive waits for active turn",
			mutate: func(f *GenerationLifecycleFacts) {
				f.ResourceArchived = true
				f.SessionState = "running"
				f.TurnActive = true
				f.TurnID = "turn-1"
			},
			operation: GenerationOperationWaitForTurnTerminal,
			intent:    GenerationIntentArchive,
			reason:    "resource_archived",
		},
		{
			name:      "replacement stops ready generation",
			mutate:    func(f *GenerationLifecycleFacts) { f.BindingChanged = true },
			operation: GenerationOperationStopSession,
			intent:    GenerationIntentReplacement,
			reason:    "binding_changed",
		},
		{
			name:      "replacement waits while stopping",
			mutate:    func(f *GenerationLifecycleFacts) { f.BindingChanged = true; f.SessionState = "stopping" },
			operation: GenerationOperationWaitForStopped,
			intent:    GenerationIntentReplacement,
		},
		{
			name:      "replacement archives stopped generation",
			mutate:    func(f *GenerationLifecycleFacts) { f.BindingChanged = true; f.SessionState = "stopped" },
			operation: GenerationOperationArchiveSession,
			intent:    GenerationIntentReplacement,
		},
		{
			name: "stopped current generation resumes for pending message",
			mutate: func(f *GenerationLifecycleFacts) {
				f.SessionState = "stopped"
				f.MailboxPending = true
				f.NextMessage = generationMessage(GenerationMessageModeEnqueue, GenerationMessageStatusQueued)
			},
			operation: GenerationOperationResumeSession,
			reason:    "resume_stopped_session",
		},
		{
			name: "stopped current generation honors resume backoff",
			mutate: func(f *GenerationLifecycleFacts) {
				f.SessionState = "stopped"
				f.MailboxPending = true
				f.NextMessage = generationMessage(GenerationMessageModeEnqueue, GenerationMessageStatusQueued)
				f.ResumeBackoffActive = true
			},
			operation: GenerationOperationWaitForSession,
			reason:    "resume_backoff",
		},
		{
			name: "stopped current generation waits without demand",
			mutate: func(f *GenerationLifecycleFacts) {
				f.SessionState = "stopped"
				f.SessionResumable = true
			},
			operation: GenerationOperationNone,
		},
		{
			name: "idle suspended generation stays stopped without demand",
			mutate: func(f *GenerationLifecycleFacts) {
				f.SessionState = "stopped"
				f.SessionResumable = true
				f.Lifecycle.Intent = GenerationIntentIdle
			},
			operation: GenerationOperationNone,
			intent:    GenerationIntentIdle,
		},
		{
			name: "terminal resume failure archives for replacement",
			mutate: func(f *GenerationLifecycleFacts) {
				f.SessionState = "stopped"
				f.SessionResumeUnavailable = true
				f.MailboxPending = true
				f.NextMessage = generationMessage(GenerationMessageModeEnqueue, GenerationMessageStatusQueued)
			},
			operation: GenerationOperationArchiveSession,
			intent:    GenerationIntentRecovery,
		},
		{
			name:      "archived session is retired",
			mutate:    func(f *GenerationLifecycleFacts) { f.SessionState = "archived" },
			operation: GenerationOperationRetireGeneration,
		},
		{
			name: "ready generation delivers queued message",
			mutate: func(f *GenerationLifecycleFacts) {
				f.MailboxPending = true
				f.NextMessage = generationMessage(GenerationMessageModeEnqueue, GenerationMessageStatusQueued)
			},
			operation: GenerationOperationDeliverMessage,
			reason:    "message_waiting",
		},
		{
			name: "active steer delivers without waiting",
			mutate: func(f *GenerationLifecycleFacts) {
				f.SessionState = "running"
				f.TurnActive = true
				f.TurnID = "turn-1"
				f.SteerSupported = true
				f.MailboxPending = true
				f.NextMessage = generationMessage(GenerationMessageModeSteer, GenerationMessageStatusQueued)
			},
			operation: GenerationOperationDeliverMessage,
			reason:    "steer_requested",
		},
		{
			name: "active enqueue waits for terminal turn",
			mutate: func(f *GenerationLifecycleFacts) {
				f.SessionState = "running"
				f.TurnActive = true
				f.TurnID = "turn-1"
				f.MailboxPending = true
				f.NextMessage = generationMessage(GenerationMessageModeEnqueue, GenerationMessageStatusQueued)
			},
			operation: GenerationOperationWaitForTurnTerminal,
			reason:    "enqueue_waits_for_turn",
		},
		{
			name: "active interrupt gets its own operation",
			mutate: func(f *GenerationLifecycleFacts) {
				f.SessionState = "waiting_approval"
				f.TurnActive = true
				f.ApprovalPending = true
				f.TurnID = "turn-1"
				f.MailboxPending = true
				f.NextMessage = generationMessage(GenerationMessageModeInterrupt, GenerationMessageStatusQueued)
			},
			operation: GenerationOperationInterruptTurn,
			reason:    "interrupt_requested",
		},
		{
			name: "in-flight delivery waits for receipt",
			mutate: func(f *GenerationLifecycleFacts) {
				f.MailboxPending = true
				f.NextMessage = generationMessage(GenerationMessageModeEnqueue, GenerationMessageStatusDelivering)
			},
			operation: GenerationOperationWaitForMessageReceipt,
			reason:    "message_receipt_pending",
		},
		{
			name: "stopped Provider-pending delivery resumes exact Session",
			mutate: func(f *GenerationLifecycleFacts) {
				f.SessionState = "stopped"
				f.SessionResumable = true
				f.MailboxPending = true
				f.NextMessage = generationMessage(GenerationMessageModeEnqueue, GenerationMessageStatusDelivering)
				f.NextMessage.ProviderDeliveryPending = true
			},
			operation: GenerationOperationResumeSession,
			reason:    "resume_pending_provider_delivery",
		},
		{
			name: "active Provider-pending enqueue confirms same input",
			mutate: func(f *GenerationLifecycleFacts) {
				f.SessionState = "running"
				f.TurnActive = true
				f.TurnID = "turn-1"
				f.MailboxPending = true
				f.NextMessage = generationMessage(GenerationMessageModeEnqueue, GenerationMessageStatusDelivering)
				f.NextMessage.ProviderDeliveryPending = true
			},
			operation: GenerationOperationDeliverMessage,
			reason:    "confirm_pending_provider_delivery",
		},
		{
			name: "Provider-pending delivery waits for Session recovery",
			mutate: func(f *GenerationLifecycleFacts) {
				f.SessionKnown = false
				f.SessionState = ""
				f.MailboxPending = true
				f.NextMessage = generationMessage(GenerationMessageModeEnqueue, GenerationMessageStatusDelivering)
				f.NextMessage.ProviderDeliveryPending = true
			},
			operation: GenerationOperationWaitForSession,
			reason:    "session_state_not_ready",
		},
		{
			name:      "idle deadline stops only an empty ready generation",
			mutate:    func(f *GenerationLifecycleFacts) { f.IdleDeadlineDue = true },
			operation: GenerationOperationStopSession,
			intent:    GenerationIntentIdle,
			reason:    "idle_deadline",
		},
		{
			name: "pending mailbox prevents idle stop",
			mutate: func(f *GenerationLifecycleFacts) {
				f.IdleDeadlineDue = true
				f.MailboxPending = true
				f.NextMessage = generationMessage(GenerationMessageModeEnqueue, GenerationMessageStatusQueued)
			},
			operation: GenerationOperationDeliverMessage,
		},
		{
			name: "approval prevents idle stop",
			mutate: func(f *GenerationLifecycleFacts) {
				f.IdleDeadlineDue = true
				f.SessionState = "waiting_approval"
				f.TurnActive = true
				f.ApprovalPending = true
			},
			operation: GenerationOperationNone,
		},
		{
			name:      "unknown session waits for observation",
			mutate:    func(f *GenerationLifecycleFacts) { f.SessionKnown = false; f.SessionState = "" },
			operation: GenerationOperationWaitForSession,
			reason:    "session_state_not_ready",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facts := testGenerationFacts()
			test.mutate(&facts)
			plan := PlanGeneration(facts)
			if plan.Operation != test.operation {
				t.Fatalf("operation = %q, want %q; plan = %#v", plan.Operation, test.operation, plan)
			}
			if test.intent != "" && plan.Intent != test.intent {
				t.Fatalf("intent = %q, want %q; plan = %#v", plan.Intent, test.intent, plan)
			}
			if test.reason != "" && plan.Reason != test.reason {
				t.Fatalf("reason = %q, want %q; plan = %#v", plan.Reason, test.reason, plan)
			}
			if plan.Operation != GenerationOperationNone && plan.OperationID == "" {
				t.Fatal("executable plan has no stable operation id")
			}
		})
	}
}

func TestPlanGenerationIsDeterministic(t *testing.T) {
	facts := testGenerationFacts()
	facts.MailboxPending = true
	facts.NextMessage = generationMessage(GenerationMessageModeInterrupt, GenerationMessageStatusQueued)
	facts.SessionState = "running"
	facts.TurnActive = true
	facts.TurnID = "turn-1"
	first := PlanGeneration(facts)
	second := PlanGeneration(facts)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same facts produced different plans: %#v vs %#v", first, second)
	}
}

func TestLifecycleGuardRejectsStaleResults(t *testing.T) {
	facts := testGenerationFacts()
	facts.MailboxPending = true
	facts.NextMessage = generationMessage(GenerationMessageModeEnqueue, GenerationMessageStatusQueued)
	plan := PlanGeneration(facts)
	if !LifecycleGuardMatchesFacts(plan, facts) {
		t.Fatal("plan did not match its source facts")
	}

	stale := facts
	stale.GenerationID = "gen-2"
	if LifecycleGuardMatchesFacts(plan, stale) {
		t.Fatal("generation replacement was not detected")
	}
	stale = facts
	stale.Revision = "rev-2"
	if LifecycleGuardMatchesFacts(plan, stale) {
		t.Fatal("revision change was not detected")
	}
	stale = facts
	stale.NextMessage = generationMessage(GenerationMessageModeEnqueue, GenerationMessageStatusQueued)
	stale.NextMessage.ID = "msg-2"
	if LifecycleGuardMatchesFacts(plan, stale) {
		t.Fatal("mailbox replacement was not detected")
	}
}

func TestGuardedLifecycleCommitDropsStaleResult(t *testing.T) {
	facts := testGenerationFacts()
	plan := PlanGeneration(facts)
	commits := 0
	committed, err := GuardedLifecycleCommit(plan, facts, func() error { commits++; return nil })
	if err != nil || !committed || commits != 1 {
		t.Fatalf("matching commit = (%v, %v), commits=%d", committed, err, commits)
	}

	stale := facts
	stale.SessionID = "ses-new"
	committed, err = GuardedLifecycleCommit(plan, stale, func() error { commits++; return nil })
	if err != nil || committed || commits != 1 {
		t.Fatalf("stale commit = (%v, %v), commits=%d", committed, err, commits)
	}
}

func TestAdaptLegacyGenerationFacts(t *testing.T) {
	now := time.Date(2026, 8, 14, 4, 0, 0, 0, time.UTC)
	record := generationRecord{
		ResourceID:         "project1.task1",
		Generation:         3,
		GenerationID:       "gen-3",
		AgentHubSessionID:  "ses-3",
		BindingKind:        "agent",
		BindingName:        "worker",
		AgentHubAgentName:  "worker",
		ProfileRevision:    "profile-rev",
		Status:             "idle",
		UpdatedAt:          "2026-08-14T03:59:00Z",
		IdleDeadlineAt:     "2026-08-14T03:30:00Z",
		ReplacementPending: true,
	}
	mailbox := resourceMailbox{Messages: []resourceMailboxMessage{{
		ID: "msg-3", ResourceID: "project1.task1", Status: resourceMessageQueued,
		RequestedMode: resourceMessageModeEnqueue,
	}}}
	session := &agentHubSession{ID: "ses-3", State: "ready", InputCapabilities: agentHubInputCapabilities{Steer: true}}
	facts := AdaptLegacyGenerationFacts(LegacyGenerationLifecycleInput{
		Generation: record, Session: session, Mailbox: mailbox, WorkspaceInstanceID: "ws-3", Now: now,
		Revision: "store-rev-3", BindingChanged: true,
	})
	if facts.ResourceID != "project1.task1" || facts.GenerationID != "gen-3" || facts.GenerationNumber != 3 {
		t.Fatalf("identity was not adapted: %#v", facts)
	}
	if facts.Lifecycle.Intent != GenerationIntentReplacement || facts.Binding.ResolvedAgent != "worker" {
		t.Fatalf("binding/lifecycle was not adapted: %#v", facts)
	}
	if !facts.MailboxPending || facts.NextMessage == nil || facts.NextMessage.ID != "msg-3" {
		t.Fatalf("mailbox was not adapted: %#v", facts)
	}
	if !facts.IdleDeadlineDue {
		t.Fatal("deadline should be due")
	}
	plan := PlanGeneration(facts)
	if plan.Operation != GenerationOperationStopSession || plan.Intent != GenerationIntentReplacement {
		t.Fatalf("adapted facts produced %#v", plan)
	}
	stopped := record
	stopped.ReplacementPending = false
	stopped.Status = "stopped"
	stoppedFacts := AdaptLegacyGenerationFacts(LegacyGenerationLifecycleInput{Generation: stopped, Session: &agentHubSession{ID: "ses-3", State: "stopped"}, Mailbox: mailbox, Now: now})
	if !stoppedFacts.SessionResumable {
		t.Fatalf("stopped Session should be marked resumable by the adapter: %#v", stoppedFacts)
	}
}

func TestApplyLegacyLifecyclePlan(t *testing.T) {
	record := &generationRecord{Status: "idle"}
	ApplyLegacyLifecyclePlan(record, GenerationLifecyclePlan{Intent: GenerationIntentIdle, Operation: GenerationOperationStopSession})
	if !record.IdleSleepStopRequested || record.Status != "stopping" || record.LifecycleReceipt == nil || record.LifecycleReceipt.Operation != GenerationOperationStopSession {
		t.Fatalf("idle stop was not adapted: %#v", record)
	}
	ApplyLegacyLifecyclePlan(record, GenerationLifecyclePlan{Operation: GenerationOperationResumeSession, OperationID: "resume-1"})
	if record.Status != "starting" || record.LifecycleReceipt == nil || record.LifecycleReceipt.Operation != GenerationOperationResumeSession {
		t.Fatalf("resume was not adapted: %#v", record)
	}
	ApplyLegacyLifecyclePlan(record, GenerationLifecyclePlan{Intent: GenerationIntentArchive, Operation: GenerationOperationRetireGeneration})
	if record.Status != "stopped" || record.IdleSleepStopRequested || record.ArchivedTaskStopRequested || !record.AgentHubStoppedObserved {
		t.Fatalf("retirement was not adapted: %#v", record)
	}
}

func TestExecuteGenerationLifecyclePlanUsesOneEffectAndReturnsReceipt(t *testing.T) {
	facts := testGenerationFacts()
	facts.IdleDeadlineDue = true
	plan := PlanGeneration(facts)
	calls := 0
	result, err := ExecuteGenerationLifecyclePlan(context.Background(), plan, GenerationLifecycleEffects{
		StopSession: func(_ context.Context, received GenerationLifecyclePlan) (agentHubSession, error) {
			calls++
			if received.OperationID != plan.OperationID {
				t.Fatalf("operation id changed across effect boundary: %q != %q", received.OperationID, plan.OperationID)
			}
			return agentHubSession{ID: "ses-1", State: "stopping"}, nil
		},
		ArchiveSession: func(_ context.Context, _ GenerationLifecyclePlan) (agentHubSession, error) {
			calls++
			return agentHubSession{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result.Receipt.State != GenerationReceiptSucceeded || result.Session == nil || result.Session.State != "stopping" {
		t.Fatalf("unexpected execution result: calls=%d result=%#v", calls, result)
	}
}

func TestExecuteGenerationLifecyclePlanTreatsNetworkErrorAsUnknown(t *testing.T) {
	facts := testGenerationFacts()
	facts.IdleDeadlineDue = true
	plan := PlanGeneration(facts)
	result, err := ExecuteGenerationLifecyclePlan(context.Background(), plan, GenerationLifecycleEffects{
		StopSession: func(context.Context, GenerationLifecyclePlan) (agentHubSession, error) {
			return agentHubSession{}, errors.New("transport failed after request")
		},
	})
	if err == nil || result.Receipt.State != GenerationReceiptUnknown {
		t.Fatalf("network error was not preserved as unknown: result=%#v err=%v", result, err)
	}
}

func TestExecuteGenerationLifecyclePlanResumesWithOneEffect(t *testing.T) {
	facts := testGenerationFacts()
	facts.SessionState = "stopped"
	facts.SessionResumable = true
	facts.MailboxPending = true
	facts.NextMessage = generationMessage(GenerationMessageModeEnqueue, GenerationMessageStatusQueued)
	plan := PlanGeneration(facts)
	if plan.Operation != GenerationOperationResumeSession {
		t.Fatalf("unexpected resume plan: %#v", plan)
	}
	calls := 0
	result, err := ExecuteGenerationLifecyclePlan(context.Background(), plan, GenerationLifecycleEffects{
		ResumeSession: func(_ context.Context, received GenerationLifecyclePlan) (agentHubSession, error) {
			calls++
			if received.SessionID != "ses-1" || received.MessageID != "msg-1" {
				t.Fatalf("resume plan identity changed: %#v", received)
			}
			return agentHubSession{ID: "ses-1", State: "ready"}, nil
		},
	})
	if err != nil || calls != 1 || result.Receipt.State != GenerationReceiptSucceeded || result.Session == nil || result.Session.State != "ready" {
		t.Fatalf("unexpected resume execution: calls=%d result=%#v err=%v", calls, result, err)
	}
}

func TestExecuteGenerationLifecyclePlanDoesNotEffectWait(t *testing.T) {
	facts := testGenerationFacts()
	facts.SessionState = "stopping"
	facts.Lifecycle.Intent = GenerationIntentReplacement
	plan := PlanGeneration(facts)
	if plan.Operation != GenerationOperationWaitForStopped {
		t.Fatalf("unexpected wait plan: %#v", plan)
	}
	calls := 0
	result, err := ExecuteGenerationLifecyclePlan(context.Background(), plan, GenerationLifecycleEffects{
		StopSession: func(context.Context, GenerationLifecyclePlan) (agentHubSession, error) {
			calls++
			return agentHubSession{}, nil
		},
	})
	if err != nil || calls != 0 || result.Receipt.State != GenerationReceiptNone {
		t.Fatalf("wait unexpectedly executed effect: calls=%d result=%#v err=%v", calls, result, err)
	}
}
