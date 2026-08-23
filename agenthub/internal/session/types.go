package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	StateStarting        = "starting"
	StateReady           = "ready"
	StateRunning         = "running"
	StateWaitingApproval = "waiting_approval"
	StateStopping        = "stopping"
	StateStopped         = "stopped"
	StateArchived        = "archived"
)

const (
	StopReasonRequested      = "requested"
	StopReasonCompleted      = "completed"
	StopReasonProviderError  = "provider_error"
	StopReasonStartupError   = "startup_error"
	StopReasonDaemonRecovery = "daemon_recovery"
)

const (
	EventTurnCompleted   = "turn.completed"
	EventTurnFailed      = "turn.failed"
	EventTurnCancelled   = "turn.cancelled"
	EventMessageInput    = "message.input"
	EventMessageDelivery = "message.delivery"
)

const MessageSchemaOpaquePayload = 2

// MessageRole is retained only for schema-v1 input and materialized-history
// compatibility. Schema-v2 callers keep application-specific provenance in
// Payload, which AgentHub stores without interpreting.
type MessageRole string

const (
	MessageRoleUser      MessageRole = "user"
	MessageRoleSystem    MessageRole = "system"
	MessageRoleAgent     MessageRole = "agent"
	MessageRoleAssistant MessageRole = "assistant"
)

// MessageSender is the legacy schema-v1 provenance shape.
type MessageSender struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

// MessageInput is the canonical durable input event payload. Schema v2 owns
// only provider-facing text, opaque caller payload, and AgentHub delivery
// controls. The remaining provenance and correlation fields are accepted and
// replayed only for schema-v1 compatibility.
type MessageInput struct {
	SchemaVersion int             `json:"schemaVersion,omitempty"`
	Text          string          `json:"text"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	Steer         bool            `json:"steer"`
	MessageID     string          `json:"messageId,omitempty"`
	Role          MessageRole     `json:"role,omitempty"`
	Sender        *MessageSender  `json:"sender,omitempty"`
	ReplyTo       string          `json:"replyTo,omitempty"`
	CorrelationID string          `json:"correlationId,omitempty"`
}

// MessageDeliveryEventData records attempts to hand one durable canonical
// input to its provider. Accepted means the provider call returned success;
// a crash between the provider accepting the input and this event being
// fsynced is intentionally recovered by another attempt (at-least-once).
type MessageDeliveryEventData struct {
	MessageEventID int64  `json:"messageEventId"`
	MessageID      string `json:"messageId,omitempty"`
	State          string `json:"state"`
	Attempt        int    `json:"attempt"`
	Error          string `json:"error,omitempty"`
}

const (
	MessageDeliveryAttempting = "attempting"
	MessageDeliveryPending    = "pending"
	MessageDeliveryAccepted   = "accepted"
)

// DurableMessage is reconstructed exclusively from canonical Events.
type DurableMessage struct {
	EventID   int64
	TurnID    string
	Input     MessageInput
	Attempt   int
	Delivered bool
}

// MessageInputError is returned when an inbound message cannot be accepted.
// Code and Field are suitable for mapping to the public API error envelope.
type MessageInputError struct {
	Code    string
	Field   string
	Message string
}

func (e *MessageInputError) Error() string {
	return e.Message
}

// NormalizeMessageInput validates either the opaque schema-v2 contract or the
// legacy provenance contract. Schema v2 never inspects Payload contents beyond
// requiring one valid JSON value.
func NormalizeMessageInput(value MessageInput) (MessageInput, error) {
	if strings.TrimSpace(value.Text) == "" {
		return MessageInput{}, &MessageInputError{
			Code: "invalid_message_text", Field: "text", Message: "message text is required",
		}
	}
	value.MessageID = strings.TrimSpace(value.MessageID)
	if value.SchemaVersion == MessageSchemaOpaquePayload {
		if value.Role != "" || value.Sender != nil || value.ReplyTo != "" || value.CorrelationID != "" {
			return MessageInput{}, &MessageInputError{
				Code: "mixed_message_schema", Field: "schemaVersion",
				Message: "schemaVersion 2 messages must keep application metadata inside payload",
			}
		}
		if len(value.Payload) > 0 {
			if !json.Valid(value.Payload) {
				return MessageInput{}, &MessageInputError{
					Code: "invalid_message_payload", Field: "payload", Message: "payload must be valid JSON",
				}
			}
			var compact bytes.Buffer
			_ = json.Compact(&compact, value.Payload)
			value.Payload = append(json.RawMessage(nil), compact.Bytes()...)
		}
		if err := validateMessageReference("messageId", value.MessageID); err != nil {
			return MessageInput{}, err
		}
		return value, nil
	}
	if value.SchemaVersion != 0 {
		return MessageInput{}, &MessageInputError{
			Code: "invalid_message_schema", Field: "schemaVersion",
			Message: fmt.Sprintf("unsupported message schemaVersion %d; expected 2 or omitted legacy schema", value.SchemaVersion),
		}
	}
	if len(value.Payload) > 0 {
		return MessageInput{}, &MessageInputError{
			Code: "mixed_message_schema", Field: "payload",
			Message: "payload requires schemaVersion 2",
		}
	}
	return normalizeLegacyMessageInput(value)
}

func normalizeLegacyMessageInput(value MessageInput) (MessageInput, error) {
	value.Role = MessageRole(strings.ToLower(strings.TrimSpace(string(value.Role))))
	if value.Role == "" {
		value.Role = MessageRoleUser
	}
	switch value.Role {
	case MessageRoleUser, MessageRoleSystem, MessageRoleAgent:
	case MessageRoleAssistant:
		return MessageInput{}, &MessageInputError{
			Code: "assistant_message_forbidden", Field: "role",
			Message: "role assistant is reserved for output generated by the current session provider",
		}
	default:
		return MessageInput{}, &MessageInputError{
			Code: "invalid_message_role", Field: "role",
			Message: fmt.Sprintf("unsupported message role %q; expected user, system, or agent", value.Role),
		}
	}
	if value.Sender != nil {
		sender := *value.Sender
		sender.ID = strings.TrimSpace(sender.ID)
		sender.Name = strings.TrimSpace(sender.Name)
		sender.SessionID = strings.TrimSpace(sender.SessionID)
		if sender.ID == "" && sender.Name == "" && sender.SessionID == "" {
			return MessageInput{}, &MessageInputError{
				Code: "invalid_message_sender", Field: "sender",
				Message: "sender must contain at least one of id, name, or sessionId",
			}
		}
		for _, field := range []struct {
			name  string
			value string
		}{
			{name: "sender.id", value: sender.ID},
			{name: "sender.name", value: sender.Name},
			{name: "sender.sessionId", value: sender.SessionID},
		} {
			entry := field.value
			if strings.ContainsRune(entry, '\x00') {
				return MessageInput{}, &MessageInputError{
					Code: "invalid_message_sender", Field: field.name,
					Message: fmt.Sprintf("%s must not contain NUL", field.name),
				}
			}
			if len(entry) > 4096 {
				return MessageInput{}, &MessageInputError{
					Code: "invalid_message_sender", Field: field.name,
					Message: fmt.Sprintf("%s is too long", field.name),
				}
			}
		}
		value.Sender = &sender
	}
	for _, field := range []struct{ name, value string }{
		{name: "messageId", value: value.MessageID},
		{name: "replyTo", value: value.ReplyTo},
		{name: "correlationId", value: value.CorrelationID},
	} {
		if err := validateMessageReference(field.name, field.value); err != nil {
			return MessageInput{}, err
		}
	}
	value.ReplyTo = strings.TrimSpace(value.ReplyTo)
	value.CorrelationID = strings.TrimSpace(value.CorrelationID)
	return value, nil
}

func validateMessageReference(name, value string) error {
	if strings.ContainsRune(value, '\x00') {
		return &MessageInputError{
			Code: "invalid_message_reference", Field: name,
			Message: fmt.Sprintf("%s must not contain NUL", name),
		}
	}
	if len(value) > 4096 {
		return &MessageInputError{
			Code: "invalid_message_reference", Field: name,
			Message: fmt.Sprintf("%s is too long", name),
		}
	}
	return nil
}

// TurnTerminalEventData is the provider-independent payload of a canonical
// turn terminal event. A successful completion has an empty payload. Failed
// and cancelled turns may carry a human-readable error or stable reason.
// Provider-native completion payloads remain available on their preceding
// diagnostic event and are never copied into this public payload.
type TurnTerminalEventData struct {
	Error  string `json:"error,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type Session struct {
	ID                 string            `json:"id"`
	Title              string            `json:"title"`
	Cwd                string            `json:"cwd"`
	AgentName          string            `json:"agentName,omitempty"`
	IdempotencyKey     string            `json:"idempotencyKey,omitempty"`
	Source             *Source           `json:"source,omitempty"`
	LaunchEnvironment  map[string]string `json:"launchEnvironment,omitempty"`
	Provider           string            `json:"provider,omitempty"`
	InputCapabilities  InputCapabilities `json:"inputCapabilities"`
	ProviderSessionID  string            `json:"providerSessionId,omitempty"`
	State              string            `json:"state"`
	StopReason         string            `json:"stopReason,omitempty"`
	CurrentTurnID      string            `json:"currentTurnId,omitempty"`
	PendingApprovalIDs []string          `json:"pendingApprovalIds,omitempty"`
	LastEventID        int64             `json:"lastEventId"`
	// LastActivityAt is updated only by semantic work events within a Turn.
	// Session lifecycle changes, provider metadata, and stderr do not refresh it.
	LastActivityAt     *time.Time `json:"lastActivityAt,omitempty"`
	LastActivityTurnID string     `json:"lastActivityTurnId,omitempty"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
}

// Source is caller-supplied metadata for correlating sessions with the
// application that created them. AgentHub stores these values verbatim and
// does not authenticate them or impose uniqueness.
type Source struct {
	App        string            `json:"app,omitempty"`
	InstanceID string            `json:"instanceId,omitempty"`
	ExternalID string            `json:"externalId,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// InputCapabilities describes provider-independent input behavior for this
// Session. It is captured with the Session so archived Sessions remain
// self-describing even when the daemon configuration later changes.
type InputCapabilities struct {
	Steer bool `json:"steer"`
}

// Event is a durable canonical session event. StartTime is populated for a
// delta event folded with at least one following fragment; it preserves the
// first fragment timestamp while Time continues to track the newest fragment.
type Event struct {
	ID        int64           `json:"id"`
	Time      time.Time       `json:"time"`
	StartTime *time.Time      `json:"startTime,omitempty"`
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	TurnID    string          `json:"turnId,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// EventPage is a stable snapshot page of a session's durable event log.
// After and NextAfter use exclusive cursor semantics: a subsequent request
// passes NextAfter as its After value. Before and NextBefore are the
// backward counterpart, populated only by backward pages (EventsPageBefore):
// a subsequent backward request passes NextBefore as its Before value. They
// are zero on forward pages and omitted from their JSON encoding.
type EventPage struct {
	Events        []Event `json:"events"`
	After         int64   `json:"after"`
	Limit         int     `json:"limit"`
	NextAfter     int64   `json:"nextAfter"`
	HasMore       bool    `json:"hasMore"`
	Before        int64   `json:"before,omitempty"`
	NextBefore    int64   `json:"nextBefore,omitempty"`
	HasMoreBefore bool    `json:"hasMoreBefore,omitempty"`
	LatestCursor  int64   `json:"latestCursor"`
}

type CreateInput struct {
	Title             string
	Cwd               string
	AgentName         string
	IdempotencyKey    string
	Source            *Source
	LaunchEnvironment map[string]string
	Provider          string
	InputCapabilities InputCapabilities
}

// TurnSummary is a rebuildable index entry over the durable event log. Every
// reference is a stable event ID; no filesystem path or byte offset escapes
// through the public API.
type TurnSummary struct {
	ID                 string          `json:"id"`
	TurnID             string          `json:"turnId"`
	Status             string          `json:"status"`
	Closed             bool            `json:"closed"`
	StartedAt          time.Time       `json:"startedAt"`
	EndedAt            *time.Time      `json:"endedAt,omitempty"`
	DurationMS         int64           `json:"durationMs"`
	StartEventID       int64           `json:"startEventId"`
	TurnStartedEventID int64           `json:"turnStartedEventId,omitempty"`
	EndEventID         int64           `json:"endEventId,omitempty"`
	CompletedAt        *time.Time      `json:"completedAt,omitempty"`
	FirstEventID       int64           `json:"firstEventId"`
	LastEventID        int64           `json:"lastEventId"`
	TriggerEventID     int64           `json:"triggerEventId,omitempty"`
	FinalReplyEventID  int64           `json:"finalReplyEventId,omitempty"`
	TriggerPreview     string          `json:"triggerPreview,omitempty"`
	TriggerRole        MessageRole     `json:"triggerRole,omitempty"`
	TriggerSender      *MessageSender  `json:"triggerSender,omitempty"`
	TriggerPayload     json.RawMessage `json:"triggerPayload,omitempty"`
	TriggerMessageID   string          `json:"triggerMessageId,omitempty"`
	FinalReplyPreview  string          `json:"finalReplyPreview,omitempty"`
	EventCount         int             `json:"eventCount"`
	ToolEventCount     int             `json:"toolEventCount"`
	Items              []TurnItem      `json:"items"`
}

// TurnItem is a compact visible projection over a stable Event range.
// Message items retain complete text and provenance. Activity items combine
// uninterrupted thinking and tool work, retaining only deterministic
// count/timing metadata; callers use the bounded Event range endpoint to
// expand their raw details.
type TurnItem struct {
	Type                 string          `json:"type"`
	Role                 MessageRole     `json:"role,omitempty"`
	Sender               *MessageSender  `json:"sender,omitempty"`
	Steer                bool            `json:"steer,omitempty"`
	Text                 string          `json:"text,omitempty"`
	Payload              json.RawMessage `json:"payload,omitempty"`
	MessageID            string          `json:"messageId,omitempty"`
	StartEventID         int64           `json:"startEventId"`
	EndEventID           int64           `json:"endEventId"`
	StartedAt            time.Time       `json:"startedAt"`
	EndedAt              time.Time       `json:"endedAt"`
	DurationMS           int64           `json:"durationMs"`
	Count                int             `json:"count"`
	ThinkingCount        int             `json:"thinkingCount,omitempty"`
	ReasoningUpdateCount int             `json:"reasoningUpdateCount,omitempty"`
	ToolCallCount        int             `json:"toolCallCount,omitempty"`
	Data                 json.RawMessage `json:"data,omitempty"`
	activityTail         string
}

// TurnPage uses exclusive stable event-ID cursors in both directions. A
// Turn's FirstEventID is its cursor key.
type TurnPage struct {
	Turns         []TurnSummary `json:"turns"`
	After         int64         `json:"after"`
	Before        int64         `json:"before,omitempty"`
	Limit         int           `json:"limit"`
	NextAfter     int64         `json:"nextAfter"`
	NextBefore    int64         `json:"nextBefore,omitempty"`
	HasMore       bool          `json:"hasMore"`
	HasMoreBefore bool          `json:"hasMoreBefore,omitempty"`
	LatestCursor  int64         `json:"latestCursor"`
	LatestEventID int64         `json:"latestEventId"`
}

// ValidateLaunchEnvironment checks values before they are persisted and
// passed to an operating-system process. Environment variable names cannot
// be empty or contain '=' or NUL, and values cannot contain NUL.
func ValidateLaunchEnvironment(environment map[string]string) error {
	for key, value := range environment {
		if key == "" {
			return errors.New("environment variable name cannot be empty")
		}
		if strings.ContainsAny(key, "=\x00") {
			return fmt.Errorf("invalid environment variable name %q", key)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("environment variable %q contains NUL", key)
		}
	}
	return nil
}

type StateEventData struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// ProviderProcessEventData is durable evidence that an adapter started an OS
// process group. It lets a replacement daemon terminate and confirm the old
// group after an ungraceful daemon exit before publishing stopped.
type ProviderProcessEventData struct {
	PID            int `json:"pid"`
	ProcessGroupID int `json:"processGroupId"`
}

type ApprovalEventData struct {
	ApprovalID string `json:"approvalId"`
}

// AgentRenameEventData is the payload of the session.agent event, appended
// when a configured agent is renamed so the session follows the new name.
type AgentRenameEventData struct {
	AgentName string `json:"agentName"`
}

// LaunchEnvironmentEventData is the payload of the
// session.launch-environment event. It carries the session's full effective
// launch environment after an overlay was merged, so replay replaces the
// projected map with the payload verbatim instead of re-applying the
// overlay. The historical session.created snapshot is never rewritten.
type LaunchEnvironmentEventData struct {
	Environment map[string]string `json:"environment"`
}

// ProviderEventData is the payload of the session.provider event. AgentName
// names the agent configuration the session runs with.
type ProviderEventData struct {
	AgentName         string            `json:"agentName,omitempty"`
	Provider          string            `json:"provider"`
	ProviderSessionID string            `json:"providerSessionId,omitempty"`
	InputCapabilities InputCapabilities `json:"inputCapabilities"`
}
