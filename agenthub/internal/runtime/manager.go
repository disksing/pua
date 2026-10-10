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
)

type Manager struct {
	store *session.Store

	mu        sync.Mutex
	cfg       config.Config
	running   map[string]*active
	inputs    map[string]*sync.Mutex
	retrying  map[string]bool
	factory   func(provider.Options) (provider.Session, error)
	done      chan struct{}
	closeOnce sync.Once
}

const maxPendingStartupAttempts = 3

type active struct {
	mu      sync.Mutex
	adapter provider.Session
	turnID  string
	// interruptRequested is set before calling the provider so a provider
	// completion notification caused by that call cannot be mistaken for a
	// naturally completed Turn.
	interruptRequested bool
	ready              chan struct{}
	startErr           error
	stopReason         string
	finalized          bool
	finalize           sync.Once
	// replies holds custom text replies queued while the owning turn is still
	// open. Providers cannot accept free text inside an approval response, so
	// each reply dismisses its question immediately and is delivered as a
	// regular user message once the turn closes.
	replies []string
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
		done: make(chan struct{}),
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
	if _, err := m.ensure(id); err != nil {
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

// SendMessage persists and delivers one canonical inbound message. The
// schema-v2 text is delivered byte-for-byte. Schema-v1 prompt construction is
// retained by provider.PromptText only for old requests and durable replay.
func (m *Manager) SendMessage(id string, input session.MessageInput) (session.Session, error) {
	input, err := session.NormalizeMessageInput(input)
	if err != nil {
		return session.Session{}, err
	}
	lock := m.inputLock(id)
	lock.Lock()
	defer lock.Unlock()
	value, err := m.store.Get(id)
	if err != nil {
		return session.Session{}, err
	}
	if value.State == session.StateArchived {
		return session.Session{}, session.ErrArchived
	}
	if value.State == session.StateStopping {
		return session.Session{}, errors.New("session provider is stopping")
	}
	if value.State == session.StateStopped && value.StopReason == session.StopReasonRequested {
		m.mu.Lock()
		m.convergeStored(id, session.StopReasonRequested, nil)
		m.mu.Unlock()
		value, err = m.store.Get(id)
		if err != nil {
			return session.Session{}, err
		}
	}
	if input.MessageID != "" {
		previous, accepted, err := m.store.DurableMessageByID(id, input.MessageID)
		if err != nil {
			return session.Session{}, err
		}
		if accepted {
			if !reflect.DeepEqual(previous.Input, input) {
				return session.Session{}, session.ErrMessageIDConflict
			}
			if !previous.Terminal {
				m.deliverMessageLocked(id, previous)
			}
			return m.store.Get(id)
		}
	}
	current := value.CurrentTurnID
	if current != "" && input.Steer && !value.InputCapabilities.Steer {
		return session.Session{}, errors.New("session provider does not support steering an active turn")
	}
	if current != "" && !input.Steer {
		return session.Session{}, errors.New("session already has an active turn; set steer=true or wait")
	}
	turnID := current
	if turnID == "" {
		turnID, err = session.NewID("turn")
		if err != nil {
			return session.Session{}, err
		}
		messageEvent, err := m.store.Append(id, session.EventMessageInput, turnID, marshal(input))
		if err != nil {
			return session.Session{}, err
		}
		// turn.started is lifecycle-only. The canonical message.input event is
		// the sole durable source for message text and provenance.
		if _, err := m.store.Append(id, "turn.started", turnID, nil); err != nil {
			return session.Session{}, err
		}
		m.deliverMessageLocked(id, session.DurableMessage{EventID: messageEvent.ID, TurnID: turnID, Input: input})
	} else {
		messageEvent, err := m.store.Append(id, session.EventMessageInput, turnID, marshal(input))
		if err != nil {
			return session.Session{}, err
		}
		m.deliverMessageLocked(id, session.DurableMessage{EventID: messageEvent.ID, TurnID: turnID, Input: input})
	}
	return m.store.Get(id)
}

// deliverMessageLocked attempts one pending durable input while the caller
// holds the Session input lock. Transient startup errors retain the input
// within a durable budget; permanent failure and Stop settle it without replay.
func (m *Manager) deliverMessageLocked(id string, message session.DurableMessage) bool {
	if message.Terminal {
		return true
	}
	if retryAt, err := time.Parse(time.RFC3339Nano, message.RetryAt); err == nil && time.Now().Before(retryAt) {
		m.schedulePendingRetry(id)
		return false
	}
	value, err := m.store.Get(id)
	if err != nil {
		return false
	}
	if value.State == session.StateStopping || value.State == session.StateArchived {
		return false
	}
	if message.StartupAttempts >= maxPendingStartupAttempts {
		m.mu.Lock()
		// An explicit Resume may already have started a healthy provider.
		// Never publish stopped while that process is still alive.
		noRuntime := m.running[id] == nil
		if noRuntime {
			m.convergeStored(id, session.StopReasonStartupError, errors.New("pending input exhausted provider startup attempts"))
		}
		m.mu.Unlock()
		if noRuntime {
			return false
		}
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
	// Persist the attempt before starting the provider. A restart during a
	// failed handshake must not reset the startup budget to zero.
	message.Attempt++
	message.StartupAttempts++
	message.RetryAt = ""
	if err := m.recordDelivery(id, message, session.MessageDeliveryAttempting, nil); err != nil {
		m.schedulePendingRetry(id)
		return false
	}
	run, err := m.ensureInput(id, message.EventID)
	// Stop and process finalization can settle this input while ensure waits.
	// Do not overwrite that terminal fact or schedule another startup.
	current, readErr := m.store.DurableMessages(id)
	if readErr != nil {
		return false
	}
	for _, item := range current {
		if item.EventID == message.EventID && item.Terminal {
			return true
		}
	}
	value, readErr = m.store.Get(id)
	if readErr != nil || value.State == session.StateStopping || value.State == session.StateArchived ||
		(value.State == session.StateStopped && value.StopReason == session.StopReasonRequested) {
		return false
	}
	if err != nil {
		message.RetryAt = time.Now().Add(time.Second << (message.StartupAttempts - 1)).Format(time.RFC3339Nano)
		if m.recordDelivery(id, message, session.MessageDeliveryPending, err) != nil {
			return false
		}
		m.schedulePendingRetry(id)
		return false
	}
	run.setTurn(message.TurnID)
	message.StartupAttempts = 0
	if err := m.recordDelivery(id, message, session.MessageDeliveryAttempting, nil); err != nil {
		m.schedulePendingRetry(id)
		return false
	}
	if err := promptAdapter(run.adapter, message.Input); err != nil {
		m.recordDelivery(id, message, session.MessageDeliveryPending, err)
		m.schedulePendingRetry(id)
		return false
	}
	if _, err := m.store.Append(id, session.EventMessageDelivery, message.TurnID, marshal(session.MessageDeliveryEventData{
		MessageEventID: message.EventID, MessageID: message.Input.MessageID,
		State: session.MessageDeliveryAccepted, Attempt: message.Attempt,
	})); err != nil {
		// The provider may already have accepted the prompt. Retrying after an
		// ambiguous acknowledgement is the deliberate at-least-once window.
		m.schedulePendingRetry(id)
		return false
	}
	return true
}

func (m *Manager) recordDelivery(id string, message session.DurableMessage, state string, cause error) error {
	data := session.MessageDeliveryEventData{
		MessageEventID: message.EventID, MessageID: message.Input.MessageID,
		State: state, Attempt: message.Attempt,
		StartupAttempts: message.StartupAttempts, RetryAt: message.RetryAt,
	}
	if cause != nil {
		data.Error = cause.Error()
	}
	_, err := m.store.Append(id, session.EventMessageDelivery, message.TurnID, marshal(data))
	return err
}

func (m *Manager) deliverPendingLocked(id string) {
	messages, err := m.store.DurableMessages(id)
	if err != nil {
		return
	}
	for _, message := range messages {
		if message.Terminal {
			continue
		}
		if !m.deliverMessageLocked(id, message) {
			return
		}
	}
}

func (m *Manager) schedulePendingRetry(id string) {
	delay := time.Second
	if messages, err := m.store.DurableMessages(id); err == nil {
		for _, message := range messages {
			if !message.Terminal {
				if retryAt, err := time.Parse(time.RFC3339Nano, message.RetryAt); err == nil && time.Until(retryAt) > delay {
					delay = time.Until(retryAt)
				}
				break
			}
		}
	}
	m.mu.Lock()
	if m.retrying[id] {
		m.mu.Unlock()
		return
	}
	m.retrying[id] = true
	m.mu.Unlock()
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-m.done:
			return
		}
		m.mu.Lock()
		delete(m.retrying, id)
		m.mu.Unlock()
		lock := m.inputLock(id)
		lock.Lock()
		select {
		case <-m.done:
			lock.Unlock()
			return
		default:
		}
		value, err := m.store.Get(id)
		if err == nil && value.State != session.StateStopping && value.State != session.StateArchived &&
			!(value.State == session.StateStopped && value.StopReason == session.StopReasonRequested) {
			m.deliverPendingLocked(id)
		}
		lock.Unlock()
	}()
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
		return err
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
		m.convergeStored(id, session.StopReasonRequested, nil)
		m.mu.Unlock()
		return nil
	}
	if run != nil {
		run.markStopping(session.StopReasonRequested)
	}
	if value.State != session.StateStopping || value.StopReason != session.StopReasonRequested {
		if _, err = m.store.Append(id, "session.state", "", marshal(session.StateEventData{State: session.StateStopping, Reason: session.StopReasonRequested})); err != nil {
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
	closeErr := run.adapter.Close()
	_ = run.waitReady()
	if closeErr == nil {
		m.convergeActive(id, run, nil)
	} else {
		_, _ = m.store.Append(id, "provider.error", run.turn(), marshal(map[string]any{
			"message": closeErr.Error(), "reason": "process_cleanup_error",
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
			return err
		}
		run.mu.Lock()
		run.replies = append(run.replies, reply.Text)
		run.mu.Unlock()
		_, err := m.store.Append(id, "approval.resolved", run.turn(), marshal(map[string]any{
			"approvalId": approvalID, "decision": "text", "text": reply.Text,
		}))
		return err
	}
	resolution := provider.ApprovalResolution{Decision: reply.Decision, OptionID: reply.OptionID}
	if err := run.adapter.Approve(approvalID, resolution); err != nil {
		return err
	}
	data := map[string]any{"approvalId": approvalID, "decision": reply.Decision}
	if reply.OptionID != "" {
		data["optionId"] = reply.OptionID
	}
	_, err := m.store.Append(id, "approval.resolved", run.turn(), marshal(data))
	return err
}

func (m *Manager) Close() {
	m.closeOnce.Do(func() { close(m.done) })
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
	return m.ensureInput(id, 0)
}

func (m *Manager) ensureInput(id string, messageEventID int64) (*active, error) {
	m.mu.Lock()
	if messageEventID != 0 {
		messages, err := m.store.DurableMessages(id)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		for _, message := range messages {
			if message.EventID == messageEventID && message.Terminal {
				m.mu.Unlock()
				return nil, errors.New("input is already terminal")
			}
		}
	}
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
	run := &active{turnID: value.CurrentTurnID, ready: make(chan struct{})}
	adapter, err := m.factory(provider.Options{
		ID: id, Cwd: value.Cwd, Title: value.Title, Agent: agent, Provider: providerConfig,
		Environment: mergeEnvironment(agent.Environment, value.LaunchEnvironment),
		Hooks: provider.Hooks{
			NativeID: func(nativeID string) {
				run.withEvent(func(_ string) {
					_, _ = m.store.Append(id, "session.provider", "", marshal(session.ProviderEventData{
						AgentName: agent.Name, Provider: providerConfig.Type, ProviderSessionID: nativeID,
						InputCapabilities: provider.InputCapabilities(providerConfig.Type),
					}))
				})
			},
			Event: func(event provider.Event) { m.providerEvent(id, run, event) },
			Approval: func(approvalID, method string, params json.RawMessage) {
				run.withEvent(func(turnID string) {
					_, _ = m.store.Append(id, "approval.requested", turnID, marshal(semantic.ApprovalRequestData(approvalID, method, params)))
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
						"message": cleanupErr.Error(), "reason": "process_cleanup_error",
					}))
					return
				}
				m.convergeActive(id, run, processErr)
			},
		},
	})
	if err != nil {
		m.convergeStored(id, session.StopReasonStartupError, err)
		m.mu.Unlock()
		return nil, err
	}
	run.adapter = adapter
	m.running[id] = run
	m.mu.Unlock()

	if err := adapter.Start(value.ProviderSessionID); err != nil {
		run.finishStart(err)
		_ = adapter.Close()
		m.convergeActive(id, run, err)
		return nil, err
	}
	readyState := session.StateReady
	if value.CurrentTurnID != "" {
		readyState = session.StateRunning
	}
	run.mu.Lock()
	if run.stopReason == "" && !run.finalized {
		_, _ = m.store.Append(id, "session.state", "", marshal(session.StateEventData{State: readyState}))
	}
	run.mu.Unlock()
	run.finishStart(nil)
	return run, nil
}

func (m *Manager) providerEvent(id string, run *active, event provider.Event) {
	var replies []string
	run.withEvent(func(turnID string) {
		if event.Type != "" {
			_, _ = m.store.Append(id, event.Type, turnID, marshal(event.Data))
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
	messages, err := m.store.DurableMessages(id)
	if err != nil {
		return
	}
	hasPending := false
	for _, message := range messages {
		if !message.Terminal {
			hasPending = true
			break
		}
	}
	if value.State == session.StateStopped && value.CurrentTurnID == "" && len(value.PendingApprovalIDs) == 0 && !hasPending {
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
	for _, message := range messages {
		if message.Terminal {
			continue
		}
		state := ""
		if reason == session.StopReasonRequested {
			state = session.MessageDeliveryCancelled
		} else if reason == session.StopReasonStartupError &&
			(errors.Is(cause, provider.ErrNativeSessionMissing) || message.StartupAttempts >= maxPendingStartupAttempts) {
			state = session.MessageDeliveryFailed
		}
		if state != "" {
			message.RetryAt = ""
			if err := m.recordDelivery(id, message, state, cause); err != nil {
				return // Keep the open Turn until its delivery outcome is durable.
			}
		} else if message.TurnID == value.CurrentTurnID {
			pendingTurn = true
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
		value.State == session.StateRunning ||
		value.State == session.StateWaitingApproval ||
		value.State == session.StateStopping ||
		open ||
		value.CurrentTurnID != "" ||
		len(value.PendingApprovalIDs) > 0
	if !needsRecovery {
		return
	}
	stopRequested, intentErr := m.store.StopWasRequested(value.ID)
	if intentErr != nil {
		return // Do not resume when the durable stop intent cannot be read.
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
	reason := session.StopReasonDaemonRecovery
	cause := errors.New("provider work was interrupted by daemon restart")
	if stopRequested {
		reason, cause = session.StopReasonRequested, nil
	} else if value.State == session.StateStopped && value.StopReason == session.StopReasonStartupError {
		reason = session.StopReasonStartupError
	}
	m.convergeStored(value.ID, reason, cause)
	if messages, err := m.store.DurableMessages(value.ID); err == nil {
		for _, message := range messages {
			if !message.Terminal && reason != session.StopReasonRequested {
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
