package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/disksing/pua/agenthub/internal/config"
	"github.com/disksing/pua/agenthub/internal/provider"
	"github.com/disksing/pua/agenthub/internal/semantic"
	"github.com/disksing/pua/agenthub/internal/session"
	"github.com/disksing/pua/internal/security"
)

type Manager struct {
	store *session.Store

	mu       sync.Mutex
	cfg      config.Config
	running  map[string]*active
	inputs   map[string]*sync.Mutex
	retrying map[string]bool
	factory  func(provider.Options) (provider.Session, error)
}

var ErrEphemeralEnvironmentRequired = errors.New("session requires a non-empty ephemeral environment for every provider start")

type active struct {
	mu      sync.Mutex
	adapter provider.Session
	turnID  string
	// interruptRequested is set before calling the provider so a provider
	// completion notification caused by that call cannot be mistaken for a
	// naturally completed Turn.
	interruptRequested  bool
	ready               chan struct{}
	startErr            error
	stopReason          string
	finalized           bool
	finalize            sync.Once
	redactor            *security.Redactor
	providerIdentityErr error
	// replies holds custom text replies queued while the owning turn is still
	// open. Providers cannot accept free text inside an approval response, so
	// each reply dismisses its question immediately and is delivered as a
	// regular user message once the turn closes.
	replies []string
}

func (a *active) redactError(err error) error {
	if err == nil || a == nil || a.redactor == nil {
		return err
	}
	return errors.New(a.redactor.RedactString(err.Error()))
}

func (a *active) redactData(value any) any {
	if a == nil || a.redactor == nil {
		return value
	}
	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	data = a.redactor.Redact(data)
	var result any
	if json.Unmarshal(data, &result) != nil {
		return string(data)
	}
	return result
}

func (a *active) turn() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turnID
}

func (a *active) setTurn(value string) {
	a.mu.Lock()
	a.turnID = value
	a.mu.Unlock()
}

func (a *active) requestInterrupt() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finalized || a.turnID == "" {
		return false
	}
	a.interruptRequested = true
	return true
}

func (a *active) clearInterruptRequest() {
	a.mu.Lock()
	if a.turnID != "" {
		a.interruptRequested = false
	}
	a.mu.Unlock()
}

func (a *active) waitReady() error {
	<-a.ready
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.startErr
}

func (a *active) finishStart(err error) {
	a.mu.Lock()
	a.startErr = err
	a.mu.Unlock()
	close(a.ready)
}

func New(store *session.Store, cfg config.Config) *Manager {
	manager := &Manager{
		store: store, cfg: cfg, running: make(map[string]*active),
		inputs: make(map[string]*sync.Mutex), retrying: make(map[string]bool), factory: provider.New,
	}
	for _, value := range store.List(false) {
		manager.recover(value)
	}
	return manager
}

func (a *active) markStopping(reason string) {
	a.mu.Lock()
	if a.stopReason == "" {
		a.stopReason = reason
	}
	a.mu.Unlock()
}

func (a *active) outcome(processErr error) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.stopReason != "":
		return a.stopReason, nil
	case a.startErr != nil:
		return session.StopReasonStartupError, a.startErr
	case processErr != nil:
		return session.StopReasonProviderError, processErr
	default:
		return session.StopReasonCompleted, nil
	}
}

func (a *active) withEvent(fn func(turnID string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finalized {
		return
	}
	fn(a.turnID)
}

func (a *active) withProviderIdentity(nativeID string, fn func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finalized || a.providerIdentityErr != nil {
		return
	}
	if a.redactor != nil && a.redactor.ContainsSecret([]byte(nativeID)) {
		a.providerIdentityErr = errors.New("provider session identity contains a registered secret")
		return
	}
	fn()
}

func (a *active) providerIdentityError() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.providerIdentityErr
}

func (a *active) beginFinalizing() {
	a.mu.Lock()
	a.finalized = true
	a.mu.Unlock()
}

func (m *Manager) Config() config.Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneConfig(m.cfg)
}

func (m *Manager) SetConfig(cfg config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	m.cfg = cloneConfig(cfg)
	m.mu.Unlock()
	return nil
}

func (m *Manager) Start(id string) (session.Session, error) {
	return m.StartWithEnvironment(id, nil)
}

// StartWithEnvironment starts a Provider with an in-memory environment
// overlay. The overlay is never passed to the Session store.
func (m *Manager) StartWithEnvironment(id string, ephemeral map[string]string) (session.Session, error) {
	if err := session.ValidateLaunchEnvironment(ephemeral); err != nil {
		return session.Session{}, err
	}
	if _, err := m.ensureWithEphemeral(id, ephemeral); err != nil {
		return session.Session{}, err
	}
	lock := m.inputLock(id)
	lock.Lock()
	m.deliverPendingLocked(id)
	lock.Unlock()
	return m.store.Get(id)
}

func (m *Manager) inputLock(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock := m.inputs[id]
	if lock == nil {
		lock = &sync.Mutex{}
		m.inputs[id] = lock
	}
	return lock
}

func (m *Manager) Send(id, text string, steer bool) (session.Session, error) {
	return m.SendMessage(id, session.MessageInput{
		SchemaVersion: session.MessageSchemaOpaquePayload, Text: text, Steer: steer,
	})
}

// MessageSendResult separates durable request acceptance from Provider
// delivery. Callers with a stable MessageID can safely retry a pending result
// without appending another canonical input.
type MessageSendResult = session.MessageSendResult

// SendMessage persists and delivers one canonical inbound message. The
// schema-v2 text is delivered byte-for-byte. Schema-v1 prompt construction is
// retained by provider.PromptText only for old requests and durable replay.
func (m *Manager) SendMessage(id string, input session.MessageInput) (session.Session, error) {
	result, err := m.SendMessageResult(id, input)
	return result.Session, err
}

// SendMessageResult persists one canonical inbound message and reports
// whether the Provider accepted it during this or an earlier attempt.
func (m *Manager) SendMessageResult(id string, input session.MessageInput) (MessageSendResult, error) {
	input, err := session.NormalizeMessageInput(input)
	if err != nil {
		return MessageSendResult{}, err
	}
	lock := m.inputLock(id)
	lock.Lock()
	defer lock.Unlock()
	value, err := m.store.Get(id)
	if err != nil {
		return MessageSendResult{}, err
	}
	if value.State == session.StateArchived {
		return MessageSendResult{}, session.ErrArchived
	}
	if value.State == session.StateStopping {
		return MessageSendResult{}, errors.New("session provider is stopping")
	}
	if input.MessageID != "" {
		previous, accepted, err := m.store.DurableMessageByID(id, input.MessageID)
		if err != nil {
			return MessageSendResult{}, err
		}
		if accepted {
			if !reflect.DeepEqual(previous.Input, input) {
				return MessageSendResult{}, session.ErrMessageIDConflict
			}
			delivered := previous.Delivered
			if !delivered {
				delivered = m.deliverMessageLocked(id, previous)
			}
			return m.messageSendResult(id, input.MessageID, delivered)
		}
	}
	current := value.CurrentTurnID
	if current != "" && input.Steer && !value.InputCapabilities.Steer {
		return MessageSendResult{}, errors.New("session provider does not support steering an active turn")
	}
	if current != "" && !input.Steer {
		return MessageSendResult{}, errors.New("session already has an active turn; set steer=true or wait")
	}
	turnID := current
	delivered := false
	if turnID == "" {
		turnID, err = session.NewID("turn")
		if err != nil {
			return MessageSendResult{}, err
		}
		messageEvent, err := m.store.Append(id, session.EventMessageInput, turnID, marshal(input))
		if err != nil {
			return MessageSendResult{}, err
		}
		// turn.started is lifecycle-only. The canonical message.input event is
		// the sole durable source for message text and provenance.
		if _, err := m.store.Append(id, "turn.started", turnID, nil); err != nil {
			return MessageSendResult{}, err
		}
		delivered = m.deliverMessageLocked(id, session.DurableMessage{EventID: messageEvent.ID, TurnID: turnID, Input: input})
	} else {
		messageEvent, err := m.store.Append(id, session.EventMessageInput, turnID, marshal(input))
		if err != nil {
			return MessageSendResult{}, err
		}
		delivered = m.deliverMessageLocked(id, session.DurableMessage{EventID: messageEvent.ID, TurnID: turnID, Input: input})
	}
	return m.messageSendResult(id, input.MessageID, delivered)
}

func (m *Manager) messageSendResult(id, messageID string, delivered bool) (MessageSendResult, error) {
	value, err := m.store.Get(id)
	if err != nil {
		return MessageSendResult{}, err
	}
	state := session.MessageProviderDeliveryPending
	if delivered {
		state = session.MessageProviderDeliveryDelivered
	}
	return MessageSendResult{
		Session:  value,
		Delivery: session.MessageProviderDelivery{MessageID: messageID, State: state},
	}, nil
}

// deliverMessageLocked attempts one pending durable input while the caller
// holds the Session input lock. Provider errors keep the input pending: the
// accepted HTTP request has transferred retry responsibility to AgentHub.
func (m *Manager) deliverMessageLocked(id string, message session.DurableMessage) bool {
	value, err := m.store.Get(id)
	if err != nil {
		return false
	}
	if value.CurrentTurnID == "" {
		if _, err := m.store.Append(id, "turn.started", message.TurnID, nil); err != nil {
			m.schedulePendingRetry(id)
			return false
		}
	} else if value.CurrentTurnID != message.TurnID {
		m.schedulePendingRetry(id)
		return false
	}
	run, err := m.ensure(id)
	if err != nil {
		m.recordDelivery(id, message, session.MessageDeliveryPending, err)
		// A fresh ephemeral overlay can only arrive through an explicit start
		// or resume. Keep the durable message pending without spinning a timer
		// that is incapable of satisfying that precondition.
		if !errors.Is(err, ErrEphemeralEnvironmentRequired) {
			m.schedulePendingRetry(id)
		}
		return false
	}
	run.setTurn(message.TurnID)
	attempt := message.Attempt + 1
	message.Attempt = attempt
	if _, err := m.store.Append(id, session.EventMessageDelivery, message.TurnID, marshal(session.MessageDeliveryEventData{
		MessageEventID: message.EventID, MessageID: message.Input.MessageID,
		State: session.MessageDeliveryAttempting, Attempt: attempt,
	})); err != nil {
		m.schedulePendingRetry(id)
		return false
	}
	if err := promptAdapter(run.adapter, message.Input); err != nil {
		m.recordDelivery(id, message, session.MessageDeliveryPending, run.redactError(err))
		m.schedulePendingRetry(id)
		return false
	}
	if _, err := m.store.Append(id, session.EventMessageDelivery, message.TurnID, marshal(session.MessageDeliveryEventData{
		MessageEventID: message.EventID, MessageID: message.Input.MessageID,
		State: session.MessageDeliveryAccepted, Attempt: attempt,
	})); err != nil {
		// The provider may already have accepted the prompt. Retrying after an
		// ambiguous acknowledgement is the deliberate at-least-once window.
		m.schedulePendingRetry(id)
		return false
	}
	return true
}

func (m *Manager) recordDelivery(id string, message session.DurableMessage, state string, cause error) {
	data := session.MessageDeliveryEventData{
		MessageEventID: message.EventID, MessageID: message.Input.MessageID,
		State: state, Attempt: message.Attempt,
	}
	if cause != nil {
		data.Error = cause.Error()
	}
	_, _ = m.store.Append(id, session.EventMessageDelivery, message.TurnID, marshal(data))
}

func (m *Manager) deliverPendingLocked(id string) {
	messages, err := m.store.DurableMessages(id)
	if err != nil {
		return
	}
	for _, message := range messages {
		if message.Delivered {
			continue
		}
		if !m.deliverMessageLocked(id, message) {
			return
		}
	}
}

func (m *Manager) schedulePendingRetry(id string) {
	m.mu.Lock()
	if m.retrying[id] {
		m.mu.Unlock()
		return
	}
	m.retrying[id] = true
	m.mu.Unlock()
	go func() {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		<-timer.C
		m.mu.Lock()
		delete(m.retrying, id)
		m.mu.Unlock()
		m.retryPending(id)
	}()
}

// retryPending is the deterministic callback behind the delayed retry. It
// deliberately uses the implicit ensure path: Sessions that have consumed an
// ephemeral environment stay stopped until an explicit caller supplies the
// next overlay.
func (m *Manager) retryPending(id string) {
	lock := m.inputLock(id)
	lock.Lock()
	defer lock.Unlock()
	value, err := m.store.Get(id)
	if err == nil && value.State != session.StateStopping && value.State != session.StateArchived &&
		!(value.State == session.StateStopped && value.StopReason == session.StopReasonRequested) {
		m.deliverPendingLocked(id)
	}
}

func promptAdapter(adapter provider.Session, input session.MessageInput) error {
	text, err := provider.PromptText(input)
	if err != nil {
		return err
	}
	return adapter.Prompt(text, input.Steer)
}

func (m *Manager) Interrupt(id string) error {
	m.mu.Lock()
	run := m.running[id]
	m.mu.Unlock()
	if run == nil {
		return errors.New("session provider is not running")
	}
	if !run.requestInterrupt() {
		return errors.New("session has no active turn to interrupt")
	}
	if err := run.adapter.Interrupt(); err != nil {
		run.clearInterruptRequest()
		return run.redactError(err)
	}
	// Providers differ in whether Interrupt emits a terminal notification. If
	// one did not, close the canonical Turn here; withEvent makes this idempotent
	// with the providerEvent path and serializes the race with a late provider
	// notification.
	run.withEvent(func(turnID string) {
		if turnID == "" || !run.interruptRequested {
			return
		}
		_, _ = m.store.Append(id, session.EventTurnCancelled, turnID, marshal(session.TurnTerminalEventData{Reason: "interrupted"}))
		run.interruptRequested = false
		run.turnID = ""
	})
	return nil
}

func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	run := m.running[id]
	value, err := m.store.Get(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if value.State == session.StateArchived {
		m.mu.Unlock()
		// Archived sessions are read-only; stopping is a no-op.
		return nil
	}
	if value.State == session.StateStopped && run == nil {
		m.mu.Unlock()
		return nil
	}
	if run != nil {
		run.markStopping(session.StopReasonRequested)
	}
	if value.State != session.StateStopping {
		if _, err = m.store.Append(id, "session.state", "", marshal(session.StateEventData{State: session.StateStopping})); err != nil {
			m.mu.Unlock()
			return err
		}
	}
	if run == nil {
		m.convergeStored(id, session.StopReasonRequested, nil)
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	closeErr := run.redactError(run.adapter.Close())
	_ = run.waitReady()
	if closeErr == nil {
		m.convergeActive(id, run, nil)
	} else {
		_, _ = m.store.Append(id, "provider.error", run.turn(), marshal(map[string]any{
			"message": run.redactError(closeErr).Error(), "reason": "process_cleanup_error",
		}))
	}
	return closeErr
}

// IsRunning reports whether the provider process for a session is
// currently running under this daemon.
func (m *Manager) IsRunning(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running[id] != nil
}

// ApprovalReply is the public reply to a pending approval. Exactly one mode
// applies: Text sends a custom free-text reply (the question is dismissed and
// the text is delivered as the next user message once the current turn
// closes), OptionID selects one of the options offered by the request, and
// Decision selects a coarse outcome.
type ApprovalReply struct {
	Decision string
	OptionID string
	Text     string
}

func (m *Manager) Approve(id, approvalID string, reply ApprovalReply) error {
	m.mu.Lock()
	run := m.running[id]
	m.mu.Unlock()
	if run == nil {
		return errors.New("session provider is not running; approval cannot survive daemon restart")
	}
	if reply.Text != "" {
		// No provider protocol carries free text inside an approval response,
		// so a custom reply dismisses the question and is queued for delivery
		// as a regular user message when the turn closes (see providerEvent).
		if err := run.adapter.Approve(approvalID, provider.ApprovalResolution{Decision: "cancel"}); err != nil {
			return run.redactError(err)
		}
		run.mu.Lock()
		run.replies = append(run.replies, reply.Text)
		run.mu.Unlock()
		_, err := m.store.Append(id, "approval.resolved", run.turn(), marshal(run.redactData(map[string]any{
			"approvalId": approvalID, "decision": "text", "text": reply.Text,
		})))
		return err
	}
	resolution := provider.ApprovalResolution{Decision: reply.Decision, OptionID: reply.OptionID}
	if err := run.adapter.Approve(approvalID, resolution); err != nil {
		return run.redactError(err)
	}
	data := map[string]any{"approvalId": approvalID, "decision": reply.Decision}
	if reply.OptionID != "" {
		data["optionId"] = reply.OptionID
	}
	_, err := m.store.Append(id, "approval.resolved", run.turn(), marshal(run.redactData(data)))
	return err
}

func (m *Manager) Close() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.running))
	for id := range m.running {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.Stop(id)
	}
}

func (m *Manager) ensure(id string) (*active, error) {
	return m.ensureWithEphemeral(id, nil)
}

func (m *Manager) ensureWithEphemeral(id string, ephemeral map[string]string) (*active, error) {
	m.mu.Lock()
	if run := m.running[id]; run != nil {
		m.mu.Unlock()
		if err := run.waitReady(); err != nil {
			return nil, err
		}
		return run, nil
	}
	value, err := m.store.Get(id)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if value.State == session.StateArchived {
		m.mu.Unlock()
		return nil, session.ErrArchived
	}
	if value.State == session.StateStopping {
		m.mu.Unlock()
		return nil, errors.New("session provider is stopping")
	}
	if len(ephemeral) > 0 && !value.EphemeralEnvironmentRequired {
		if _, err := m.store.Append(id, session.EventEphemeralEnvironmentRequired, "", marshal(session.EphemeralEnvironmentRequiredEventData{Required: true})); err != nil {
			m.mu.Unlock()
			return nil, err
		}
		value.EphemeralEnvironmentRequired = true
	}
	if value.EphemeralEnvironmentRequired && len(ephemeral) == 0 {
		m.mu.Unlock()
		return nil, ErrEphemeralEnvironmentRequired
	}
	_, _ = m.store.Append(id, "session.state", "", marshal(session.StateEventData{State: session.StateStarting}))
	cfg := cloneConfig(m.cfg)
	if value.AgentName == "" {
		err := fmt.Errorf("session %s has no agent: it was created before explicit agent selection and cannot be started; create a new session with an explicit agent", id)
		m.convergeStored(id, session.StopReasonStartupError, err)
		m.mu.Unlock()
		return nil, err
	}
	agent, providerConfig, err := resolveAgent(cfg, value.AgentName)
	if err != nil {
		err = fmt.Errorf("session %s: %w", id, err)
		m.convergeStored(id, session.StopReasonStartupError, err)
		m.mu.Unlock()
		return nil, err
	}
	run := &active{turnID: value.CurrentTurnID, ready: make(chan struct{}), redactor: ephemeralEnvironmentRedactor(ephemeral)}
	adapter, err := m.factory(provider.Options{
		ID: id, Cwd: value.Cwd, Title: value.Title, Agent: agent, Provider: providerConfig,
		Environment: mergeEnvironmentWithEphemeral(agent.Environment, value.LaunchEnvironment, ephemeral),
		Hooks: provider.Hooks{
			NativeID: func(nativeID string) {
				run.withProviderIdentity(nativeID, func() {
					_, _ = m.store.Append(id, "session.provider", "", marshal(session.ProviderEventData{
						AgentName: agent.Name, Provider: providerConfig.Type, ProviderSessionID: nativeID,
						InputCapabilities: provider.InputCapabilities(providerConfig.Type),
					}))
				})
			},
			Event: func(event provider.Event) { m.providerEvent(id, run, event) },
			Approval: func(approvalID, method string, params json.RawMessage) {
				run.withEvent(func(turnID string) {
					data := semantic.ApprovalRequestData(approvalID, method, params)
					_, _ = m.store.Append(id, "approval.requested", turnID, marshal(run.redactData(data)))
				})
			},
			ProcessStart: func(info provider.ProcessInfo) error {
				_, err := m.store.Append(id, "provider.process.started", "", marshal(session.ProviderProcessEventData{
					PID: info.PID, ProcessGroupID: info.ProcessGroupID,
				}))
				return err
			},
			ProcessEnd: func(processErr error) {
				_ = run.waitReady()
				// Wait confirms the group leader. Close also eliminates and
				// probes the full process group before stopped is publishable.
				if cleanupErr := run.adapter.Close(); cleanupErr != nil {
					_, _ = m.store.Append(id, "session.state", "", marshal(session.StateEventData{State: session.StateStopping}))
					_, _ = m.store.Append(id, "provider.error", run.turn(), marshal(map[string]any{
						"message": run.redactError(cleanupErr).Error(), "reason": "process_cleanup_error",
					}))
					return
				}
				m.convergeActive(id, run, processErr)
			},
		},
	})
	if err != nil {
		err = run.redactError(err)
		m.convergeStored(id, session.StopReasonStartupError, err)
		m.mu.Unlock()
		return nil, err
	}
	run.adapter = adapter
	m.running[id] = run
	m.mu.Unlock()

	startErr := adapter.Start(value.ProviderSessionID)
	if identityErr := run.providerIdentityError(); identityErr != nil {
		startErr = identityErr
	}
	if startErr != nil {
		startErr = run.redactError(startErr)
		run.finishStart(startErr)
		_ = adapter.Close()
		m.convergeActive(id, run, startErr)
		return nil, startErr
	}
	readyState := session.StateReady
	if value.CurrentTurnID != "" {
		readyState = session.StateRunning
	}
	_, _ = m.store.Append(id, "session.state", "", marshal(session.StateEventData{State: readyState}))
	run.finishStart(nil)
	return run, nil
}

func (m *Manager) providerEvent(id string, run *active, event provider.Event) {
	var replies []string
	run.withEvent(func(turnID string) {
		if event.Type != "" {
			_, _ = m.store.Append(id, event.Type, turnID, marshal(run.redactData(event.Data)))
		}
		if !event.TurnDone || turnID == "" {
			return
		}
		eventType := session.EventTurnCompleted
		terminal := session.TurnTerminalEventData{}
		approvalReason := session.StopReasonCompleted
		interrupted := run.interruptRequested
		if interrupted {
			eventType = session.EventTurnCancelled
			terminal.Reason = "interrupted"
			approvalReason = "interrupted"
		} else if event.TurnFailed {
			eventType = session.EventTurnFailed
			terminal.Error = providerEventMessage(event.Data)
			if run.redactor != nil {
				terminal.Error = run.redactor.RedactString(terminal.Error)
			}
			approvalReason = session.StopReasonProviderError
		}
		// A canonical turn terminal is also the closure boundary for every
		// approval belonging to that turn. In particular, an RPC waiter can
		// observe a crashed provider before the process Wait callback runs.
		// Close approvals here so clients never see turn.failed followed by
		// a still-pending approval while ProcessEnd catches up.
		if value, err := m.store.Get(id); err == nil {
			for _, approvalID := range value.PendingApprovalIDs {
				_, _ = m.store.Append(id, "approval.resolved", turnID, marshal(map[string]any{
					"approvalId": approvalID, "decision": "cancel", "reason": approvalReason,
				}))
			}
		}
		_, _ = m.store.Append(id, eventType, turnID, marshal(terminal))
		run.interruptRequested = false
		run.turnID = ""
		replies = run.replies
		run.replies = nil
	})
	// Replies are delivered from a separate goroutine: providerEvent runs on
	// the provider read loop, and some adapters send prompts synchronously,
	// which would deadlock the loop that feeds their own responses.
	if len(replies) > 0 {
		go func() {
			for _, reply := range replies {
				m.deliverReply(id, reply)
			}
		}()
	}
}

// deliverReply sends a queued custom approval reply as a regular user
// message. The turn that carried the question has already closed, so the
// reply starts a fresh turn; a session that stopped in the meantime is not
// resurrected, and the recorded approval.resolved event keeps the text
// visible for a manual resend.
func (m *Manager) deliverReply(id, text string) {
	value, err := m.store.Get(id)
	if err != nil {
		return
	}
	if value.State == session.StateStopping || value.State == session.StateStopped || value.State == session.StateArchived {
		return
	}
	if _, err := m.Send(id, text, false); err != nil {
		_, _ = m.store.Append(id, "provider.error", "", marshal(map[string]any{
			"message": "could not deliver queued reply: " + err.Error(),
		}))
	}
}

func providerEventMessage(data any) string {
	switch value := data.(type) {
	case map[string]any:
		message, _ := value["message"].(string)
		return message
	case struct{ Message string }:
		return value.Message
	default:
		return ""
	}
}

func (m *Manager) convergeActive(id string, run *active, processErr error) {
	run.finalize.Do(func() {
		run.beginFinalizing()
		processErr = run.redactError(processErr)
		reason, cause := run.outcome(processErr)
		m.mu.Lock()
		if m.running[id] == run {
			delete(m.running, id)
		}
		m.convergeStored(id, reason, cause)
		m.mu.Unlock()
		run.setTurn("")
	})
}

// convergeStored is the single terminal path for every confirmed provider
// exit. The event order is stable: error, pending approvals, open turn, and
// finally the stopped boundary with a machine-readable reason.
func (m *Manager) convergeStored(id, reason string, cause error) {
	value, err := m.store.Get(id)
	if err != nil || value.State == session.StateArchived {
		return
	}
	if value.State == session.StateStopped && value.CurrentTurnID == "" && len(value.PendingApprovalIDs) == 0 {
		return
	}
	if cause != nil {
		_, _ = m.store.Append(id, "provider.error", value.CurrentTurnID, marshal(map[string]any{
			"message": cause.Error(), "reason": reason,
		}))
	}
	for _, approvalID := range value.PendingApprovalIDs {
		_, _ = m.store.Append(id, "approval.resolved", value.CurrentTurnID, marshal(map[string]any{
			"approvalId": approvalID, "decision": "cancel", "reason": reason,
		}))
	}
	pendingTurn := false
	if messages, pendingErr := m.store.DurableMessages(id); pendingErr == nil {
		for _, message := range messages {
			if !message.Delivered && message.TurnID == value.CurrentTurnID {
				pendingTurn = true
				break
			}
		}
	}
	if value.CurrentTurnID != "" && !pendingTurn {
		eventType := session.EventTurnCancelled
		data := session.TurnTerminalEventData{Reason: reason}
		if reason == session.StopReasonProviderError || reason == session.StopReasonStartupError {
			eventType = session.EventTurnFailed
			if cause != nil {
				data.Error = cause.Error()
			}
		}
		_, _ = m.store.Append(id, eventType, value.CurrentTurnID, marshal(data))
	}
	_, _ = m.store.Append(id, "session.state", "", marshal(session.StateEventData{
		State: session.StateStopped, Reason: reason,
	}))
}

func (m *Manager) recover(value session.Session) {
	if value.State == session.StateArchived {
		return
	}
	process, open, processErr := m.store.OpenProviderProcess(value.ID)
	needsRecovery := value.State == session.StateStarting ||
		value.State == session.StateReady ||
		value.State == session.StateRunning ||
		value.State == session.StateWaitingApproval ||
		value.State == session.StateStopping ||
		open ||
		value.CurrentTurnID != "" ||
		len(value.PendingApprovalIDs) > 0
	if !needsRecovery {
		return
	}
	if value.State != session.StateStopping {
		_, _ = m.store.Append(value.ID, "session.state", "", marshal(session.StateEventData{State: session.StateStopping}))
	}
	err := processErr
	if err == nil && open {
		err = provider.TerminateProcessGroup(process.PID, process.ProcessGroupID)
	}
	if err != nil {
		_, _ = m.store.Append(value.ID, "provider.error", value.CurrentTurnID, marshal(map[string]any{
			"message": "daemon recovery could not confirm provider exit: " + err.Error(),
			"reason":  session.StopReasonDaemonRecovery,
		}))
		return
	}
	m.convergeStored(value.ID, session.StopReasonDaemonRecovery, errors.New("provider work was interrupted by daemon restart"))
	if messages, err := m.store.DurableMessages(value.ID); err == nil {
		for _, message := range messages {
			if !message.Delivered {
				m.schedulePendingRetry(value.ID)
				break
			}
		}
	}
}

func resolveAgent(cfg config.Config, reference string) (config.Agent, config.Provider, error) {
	return cfg.Agent(reference)
}

func marshal(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

func cloneEnvironment(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	cloned := make(map[string]string, len(value))
	for key, entry := range value {
		cloned[key] = entry
	}
	return cloned
}

// mergeEnvironment combines an agent's configured environment with the
// session's launch environment. The session overlay wins for same-named
// entries, so a per-session value (for example a PUA-injected resource
// identity) overrides the agent default. An empty result stays nil so the
// provider inherits the daemon environment unchanged.
func mergeEnvironment(agent, session map[string]string) map[string]string {
	if len(agent) == 0 && len(session) == 0 {
		return nil
	}
	merged := make(map[string]string, len(agent)+len(session))
	for key, value := range agent {
		merged[key] = value
	}
	for key, value := range session {
		merged[key] = value
	}
	return merged
}

func mergeEnvironmentWithEphemeral(agent, session, ephemeral map[string]string) map[string]string {
	merged := mergeEnvironment(agent, session)
	if len(ephemeral) == 0 {
		return merged
	}
	if merged == nil {
		merged = make(map[string]string, len(ephemeral))
	}
	for key, value := range ephemeral {
		merged[key] = value
	}
	return merged
}

func ephemeralEnvironmentRedactor(environment map[string]string) *security.Redactor {
	redactor := security.NewRedactor()
	for key, value := range environment {
		redactor.Register(key)
		redactor.Register(value)
	}
	return redactor
}

func cloneConfig(value config.Config) config.Config {
	data, _ := json.Marshal(value)
	var result config.Config
	_ = json.Unmarshal(data, &result)
	return result
}

func (m *Manager) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fmt.Sprintf("%d running sessions", len(m.running))
}
