package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAgentHubEndpoint = "http://127.0.0.1:4646/agenthub"
	agentHubAPIVersion      = "1"
	agentHubRequestTimeout  = 30 * time.Second
)

// agentHubMaxResponseBytes caps how much of an AgentHub response body the
// client will buffer while decoding. AgentHub is a local trusted service and
// event pages can legitimately reach tens of megabytes, so the cap only
// guards against runaway memory usage. It is a variable so tests can lower it.
var agentHubMaxResponseBytes int64 = 256 << 20

var requiredAgentHubCapabilities = []string{
	"session.source",
	"session.source-metadata",
	"session.idempotent-create",
	"session.input-capabilities",
	"messages.idempotent",
	"messages.at-least-once",
	"messages.opaque-payload-v2",
	"turns.stable-index",
	"turns.materialized",
	"session.launch-environment",
	"session.launch-environment-update",
	"session.strict-stopped",
	"events.lossless-replay",
	"events.canonical-turn-terminals",
	"events.semantic-v1",
	"event.raw-v1",
	"recovery.closed-turns",
}

type agentHubClient struct {
	endpoint   string
	httpClient *http.Client
}

type agentHubAPIError struct {
	StatusCode int
	Code       string
	Message    string
	Retryable  bool
	Details    json.RawMessage
	RequestID  string
}

func (e *agentHubAPIError) Error() string {
	if e == nil {
		return ""
	}
	message := strings.TrimSpace(e.Message)
	if message == "" {
		message = http.StatusText(e.StatusCode)
	}
	if e.Code == "" {
		return fmt.Sprintf("AgentHub returned %d: %s", e.StatusCode, message)
	}
	if e.RequestID == "" {
		return fmt.Sprintf("AgentHub %s: %s", e.Code, message)
	}
	return fmt.Sprintf("AgentHub %s: %s (request %s)", e.Code, message, e.RequestID)
}

type agentHubStatus struct {
	APIVersion   string          `json:"apiVersion"`
	Capabilities []string        `json:"capabilities"`
	Version      string          `json:"version"`
	StartedAt    string          `json:"startedAt,omitempty"`
	Uptime       int64           `json:"uptimeSeconds,omitempty"`
	Paths        agentHubPaths   `json:"paths"`
	Runtime      json.RawMessage `json:"runtime,omitempty"`
}

type agentHubPaths struct {
	Config   string `json:"config"`
	Sessions string `json:"sessions"`
	Archive  string `json:"archive"`
	Logs     string `json:"logs"`
}

type agentHubProvider struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
}

type agentHubAgent struct {
	Name              string            `json:"name"`
	ProviderID        string            `json:"providerId"`
	Options           map[string]string `json:"options,omitempty"`
	Available         bool              `json:"available"`
	UnavailableReason string            `json:"unavailableReason,omitempty"`
}

type agentHubProbe struct {
	ProviderID string `json:"providerId"`
	Type       string `json:"type"`
	Command    string `json:"command,omitempty"`
	Available  bool   `json:"available"`
}

type agentHubCatalog struct {
	Providers []agentHubProvider `json:"providers"`
	Agents    []agentHubAgent    `json:"agents"`
	Probes    []agentHubProbe    `json:"probes"`
}

// agentHubConfiguredProvider and agentHubConfiguredAgent mirror the editable
// part of AgentHub's /v1/config contract. They deliberately live in the PUA
// client package so the PUA settings endpoint never invents a second source
// of truth for provider or agent definitions.
type agentHubConfiguredProvider struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
	Command string `json:"command,omitempty"`
}

type agentHubConfiguredAgent struct {
	Name        string            `json:"name"`
	ProviderID  string            `json:"providerId"`
	Options     map[string]string `json:"options,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
}

type agentHubConfiguredOnWatch struct {
	Enabled                bool   `json:"enabled"`
	ServerURL              string `json:"serverUrl"`
	AuthMode               string `json:"authMode"`
	Username               string `json:"username,omitempty"`
	Password               string `json:"password"`
	RefreshIntervalSeconds int    `json:"refreshIntervalSeconds"`
}

type agentHubConfiguredConfig struct {
	Version         int                          `json:"version"`
	AgentProviders  []agentHubConfiguredProvider `json:"agentProviders"`
	Agents          []agentHubConfiguredAgent    `json:"agents"`
	OnWatch         agentHubConfiguredOnWatch    `json:"onWatch"`
	LegacyCompanion json.RawMessage              `json:"companion,omitempty"`
}

type agentHubSource struct {
	App        string            `json:"app,omitempty"`
	InstanceID string            `json:"instanceId,omitempty"`
	ExternalID string            `json:"externalId,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

type agentHubInputCapabilities struct {
	Steer bool `json:"steer"`
}

type agentHubSession struct {
	ID                 string                    `json:"id"`
	Title              string                    `json:"title"`
	Cwd                string                    `json:"cwd"`
	AgentName          string                    `json:"agentName,omitempty"`
	LaunchEnvironment  map[string]string         `json:"launchEnvironment,omitempty"`
	Source             *agentHubSource           `json:"source,omitempty"`
	Provider           string                    `json:"provider,omitempty"`
	InputCapabilities  agentHubInputCapabilities `json:"inputCapabilities"`
	ProviderSessionID  string                    `json:"providerSessionId,omitempty"`
	State              string                    `json:"state"`
	StopReason         string                    `json:"stopReason,omitempty"`
	CurrentTurnID      string                    `json:"currentTurnId,omitempty"`
	PendingApprovalIDs []string                  `json:"pendingApprovalIds,omitempty"`
	LastEventID        int64                     `json:"lastEventId"`
	LastActivityAt     string                    `json:"lastActivityAt,omitempty"`
	LastActivityTurnID string                    `json:"lastActivityTurnId,omitempty"`
	CreatedAt          string                    `json:"createdAt"`
	UpdatedAt          string                    `json:"updatedAt"`
}

// agentHubMessageSender belongs to PUA's own message payload. The top-level
// legacy fields use the same shape only while reading old AgentHub inputs.
type agentHubMessageSender struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

// agentHubInboundMessage is the schema-v2 wire shape plus deprecated schema-v1
// fields used to recognize existing durable inputs during rolling upgrades.
type agentHubInboundMessage struct {
	SchemaVersion int                    `json:"schemaVersion,omitempty"`
	Text          string                 `json:"text"`
	Payload       json.RawMessage        `json:"payload,omitempty"`
	Role          string                 `json:"role,omitempty"`
	Sender        *agentHubMessageSender `json:"sender,omitempty"`
	Steer         bool                   `json:"steer,omitempty"`
	MessageID     string                 `json:"messageId,omitempty"`
	TurnID        string                 `json:"-"`
}

type agentHubCreateSessionRequest struct {
	Title             string                  `json:"title,omitempty"`
	Cwd               string                  `json:"cwd"`
	AgentName         string                  `json:"agentName"`
	LaunchEnvironment map[string]string       `json:"launchEnvironment,omitempty"`
	Source            *agentHubSource         `json:"source,omitempty"`
	IdempotencyKey    string                  `json:"idempotencyKey,omitempty"`
	InitialMessage    *agentHubInboundMessage `json:"initialMessage,omitempty"`
}

type agentHubApprovalReply struct {
	Decision string `json:"decision,omitempty"`
	OptionID string `json:"optionId,omitempty"`
	Text     string `json:"text,omitempty"`
}

type agentHubSessionFilter struct {
	IncludeArchived  bool
	Archived         bool
	States           []string
	SourceApp        string
	SourceInstanceID string
	SourceExternalID string
}

type agentHubEvent struct {
	ID        int64           `json:"id"`
	Time      string          `json:"time"`
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	TurnID    string          `json:"turnId,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

type agentHubSemanticEvent struct {
	ID            string          `json:"id"`
	SourceEventID int64           `json:"sourceEventId"`
	Index         int             `json:"index"`
	Time          string          `json:"time"`
	StartTime     string          `json:"startTime,omitempty"`
	Type          string          `json:"type"`
	SessionID     string          `json:"sessionId"`
	TurnID        string          `json:"turnId,omitempty"`
	Data          json.RawMessage `json:"data,omitempty"`
}

type agentHubSemanticFrame struct {
	Schema string `json:"schema"`
	Cursor int64  `json:"cursor"`
	Mode   string `json:"mode"`
	Source struct {
		EventID   int64  `json:"eventId"`
		Type      string `json:"type"`
		SessionID string `json:"sessionId"`
		TurnID    string `json:"turnId,omitempty"`
		Time      string `json:"time"`
		StartTime string `json:"startTime,omitempty"`
	} `json:"source"`
	Events []agentHubSemanticEvent `json:"events"`
}

type agentHubEventDetail struct {
	Schema      string                `json:"schema"`
	SourceEvent agentHubEvent         `json:"sourceEvent"`
	Frame       agentHubSemanticFrame `json:"frame"`
}

func semanticAgentHubEvent(event agentHubSemanticEvent) agentHubEvent {
	return agentHubEvent{
		ID: event.SourceEventID, Time: event.Time, Type: event.Type,
		SessionID: event.SessionID, TurnID: event.TurnID, Data: event.Data,
	}
}

type agentHubTurnItem struct {
	Type                 string                 `json:"type"`
	Role                 string                 `json:"role,omitempty"`
	Sender               *agentHubMessageSender `json:"sender,omitempty"`
	Steer                bool                   `json:"steer,omitempty"`
	Text                 string                 `json:"text,omitempty"`
	Payload              json.RawMessage        `json:"payload,omitempty"`
	MessageID            string                 `json:"messageId,omitempty"`
	StartEventID         int64                  `json:"startEventId"`
	EndEventID           int64                  `json:"endEventId"`
	StartedAt            string                 `json:"startedAt"`
	EndedAt              string                 `json:"endedAt"`
	DurationMS           int64                  `json:"durationMs"`
	Count                int                    `json:"count"`
	ThinkingCount        int                    `json:"thinkingCount,omitempty"`
	ReasoningUpdateCount int                    `json:"reasoningUpdateCount,omitempty"`
	ToolCallCount        int                    `json:"toolCallCount,omitempty"`
	Data                 json.RawMessage        `json:"data,omitempty"`
}

type agentHubTurn struct {
	ID                 string                 `json:"id"`
	TurnID             string                 `json:"turnId"`
	Status             string                 `json:"status"`
	Closed             bool                   `json:"closed"`
	StartedAt          string                 `json:"startedAt"`
	EndedAt            string                 `json:"endedAt,omitempty"`
	DurationMS         int64                  `json:"durationMs"`
	StartEventID       int64                  `json:"startEventId"`
	TurnStartedEventID int64                  `json:"turnStartedEventId,omitempty"`
	EndEventID         int64                  `json:"endEventId,omitempty"`
	CompletedAt        string                 `json:"completedAt,omitempty"`
	FirstEventID       int64                  `json:"firstEventId"`
	LastEventID        int64                  `json:"lastEventId"`
	TriggerEventID     int64                  `json:"triggerEventId,omitempty"`
	FinalReplyEventID  int64                  `json:"finalReplyEventId,omitempty"`
	TriggerPreview     string                 `json:"triggerPreview,omitempty"`
	TriggerRole        string                 `json:"triggerRole,omitempty"`
	TriggerSender      *agentHubMessageSender `json:"triggerSender,omitempty"`
	TriggerPayload     json.RawMessage        `json:"triggerPayload,omitempty"`
	TriggerMessageID   string                 `json:"triggerMessageId,omitempty"`
	FinalReplyPreview  string                 `json:"finalReplyPreview,omitempty"`
	EventCount         int                    `json:"eventCount"`
	ToolEventCount     int                    `json:"toolEventCount"`
	Items              []agentHubTurnItem     `json:"items,omitempty"`
}

type agentHubTurnPage struct {
	Turns []agentHubTurn `json:"turns"`
	Page  struct {
		NextBefore    int64 `json:"nextBefore"`
		HasMoreBefore bool  `json:"hasMoreBefore"`
	} `json:"page"`
	LatestCursor  int64 `json:"latestCursor"`
	LatestEventID int64 `json:"latestEventId"`
}

func newAgentHubClient(endpoint string, httpClient *http.Client) (*agentHubClient, error) {
	normalized, err := normalizeAgentHubEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &agentHubClient{endpoint: normalized, httpClient: httpClient}, nil
}

func normalizeAgentHubEndpoint(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		endpoint = defaultAgentHubEndpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid AgentHub endpoint: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", errors.New("AgentHub endpoint must be an absolute http or https URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("AgentHub endpoint must not contain credentials, query parameters, or a fragment")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if path != "" && path != "/agenthub" {
		return "", errors.New("AgentHub endpoint path must be /agenthub")
	}
	parsed.Path = path
	return strings.TrimRight(parsed.String(), "/"), nil
}

func (c *agentHubClient) Status(ctx context.Context) (agentHubStatus, error) {
	var response agentHubStatus
	err := c.doJSON(ctx, http.MethodGet, "/v1/status", nil, &response)
	return response, err
}

func validateAgentHubStatus(status agentHubStatus) error {
	if status.APIVersion != agentHubAPIVersion {
		return fmt.Errorf("incompatible AgentHub apiVersion %q; PUA requires %q", status.APIVersion, agentHubAPIVersion)
	}
	available := make(map[string]bool, len(status.Capabilities))
	for _, capability := range status.Capabilities {
		available[capability] = true
	}
	var missing []string
	for _, capability := range requiredAgentHubCapabilities {
		if !available[capability] {
			missing = append(missing, capability)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("AgentHub is missing required capabilities: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (c *agentHubClient) Agents(ctx context.Context) (agentHubCatalog, error) {
	var response agentHubCatalog
	err := c.doJSON(ctx, http.MethodGet, "/v1/agents", nil, &response)
	return response, err
}

func (c *agentHubClient) Config(ctx context.Context) (agentHubConfiguredConfig, error) {
	var response struct {
		Config agentHubConfiguredConfig `json:"config"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/v1/config", nil, &response)
	return response.Config, err
}

func (c *agentHubClient) SaveConfig(ctx context.Context, config agentHubConfiguredConfig) (agentHubConfiguredConfig, error) {
	var response struct {
		Config agentHubConfiguredConfig `json:"config"`
	}
	err := c.doJSON(ctx, http.MethodPut, "/v1/config", struct {
		Config agentHubConfiguredConfig `json:"config"`
	}{Config: config}, &response)
	return response.Config, err
}

func (c *agentHubClient) SetProviderEnabled(ctx context.Context, id string, enabled bool) (agentHubConfiguredProvider, error) {
	var response struct {
		Provider agentHubConfiguredProvider `json:"provider"`
	}
	err := c.doJSON(ctx, http.MethodPut, "/v1/config/providers/"+url.PathEscape(strings.TrimSpace(id)), struct {
		Enabled bool `json:"enabled"`
	}{Enabled: enabled}, &response)
	return response.Provider, err
}

func (c *agentHubClient) CreateSession(ctx context.Context, request agentHubCreateSessionRequest) (agentHubSession, error) {
	var response struct {
		Session agentHubSession `json:"session"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/v1/sessions", request, &response)
	return response.Session, err
}

func (c *agentHubClient) GetSession(ctx context.Context, sessionID string) (agentHubSession, error) {
	var response struct {
		Session agentHubSession `json:"session"`
	}
	err := c.doJSON(ctx, http.MethodGet, sessionPath(sessionID), nil, &response)
	return response.Session, err
}

// SessionFrames returns one page of a session's durable semantic history along
// with the latest cursor. It is used both to record canonical turn terminals
// and to prove that an archived session passed through a durable stopped state.
func (c *agentHubClient) SessionFrames(ctx context.Context, sessionID string, after int64, limit int) ([]agentHubSemanticFrame, int64, error) {
	query := make(url.Values)
	query.Set("after", strconv.FormatInt(after, 10))
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var response struct {
		Schema       string                  `json:"schema"`
		Frames       []agentHubSemanticFrame `json:"frames"`
		LatestCursor int64                   `json:"latestCursor"`
	}
	err := c.doJSON(ctx, http.MethodGet, sessionPath(sessionID)+"/events?"+query.Encode(), nil, &response)
	if err == nil && response.Schema != "agenthub.semantic-events.v1" {
		err = fmt.Errorf("AgentHub returned unsupported events schema %q", response.Schema)
	}
	if err == nil {
		for _, frame := range response.Frames {
			if frame.Schema != response.Schema || frame.Cursor <= 0 || frame.Source.EventID != frame.Cursor || (frame.Source.SessionID != "" && frame.Source.SessionID != sessionID) {
				err = fmt.Errorf("AgentHub returned an invalid semantic frame at cursor %d", frame.Cursor)
				break
			}
		}
	}
	return response.Frames, response.LatestCursor, err
}

func (c *agentHubClient) SessionTurns(ctx context.Context, sessionID string, before int64, latest bool, limit int) (agentHubTurnPage, error) {
	query := make(url.Values)
	if latest {
		query.Set("latest", "true")
	} else if before > 0 {
		query.Set("before", strconv.FormatInt(before, 10))
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var response agentHubTurnPage
	path := sessionPath(sessionID) + "/turns"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	err := c.doJSON(ctx, http.MethodGet, path, nil, &response)
	return response, err
}

func (c *agentHubClient) SessionTurn(ctx context.Context, sessionID, turnID string) (agentHubTurn, int64, error) {
	var response struct {
		Turn          agentHubTurn `json:"turn"`
		LatestEventID int64        `json:"latestEventId"`
	}
	err := c.doJSON(ctx, http.MethodGet, sessionPath(sessionID)+"/turns/"+url.PathEscape(turnID), nil, &response)
	return response.Turn, response.LatestEventID, err
}

func (c *agentHubClient) SessionEvent(ctx context.Context, sessionID string, eventID int64) (agentHubEventDetail, error) {
	if eventID <= 0 {
		return agentHubEventDetail{}, errors.New("event id must be positive")
	}
	var result agentHubEventDetail
	err := c.doJSON(ctx, http.MethodGet, sessionPath(sessionID)+"/event/"+strconv.FormatInt(eventID, 10), nil, &result)
	if err != nil {
		return agentHubEventDetail{}, err
	}
	if result.Schema != "agenthub.event-detail.v1" || result.SourceEvent.ID != eventID ||
		result.Frame.Schema != "agenthub.semantic-events.v1" || result.Frame.Cursor != eventID ||
		result.Frame.Source.EventID != eventID ||
		(result.SourceEvent.SessionID != "" && result.SourceEvent.SessionID != sessionID) ||
		(result.Frame.Source.SessionID != "" && result.Frame.Source.SessionID != sessionID) {
		return agentHubEventDetail{}, errors.New("AgentHub returned an invalid event detail")
	}
	return result, nil
}

func (c *agentHubClient) ListSessions(ctx context.Context, filter agentHubSessionFilter) ([]agentHubSession, error) {
	query := make(url.Values)
	if filter.IncludeArchived {
		query.Set("includeArchived", "true")
	}
	if filter.Archived {
		query.Set("archived", "true")
	}
	if len(filter.States) > 0 {
		query.Set("state", strings.Join(filter.States, ","))
	}
	query.Set("sourceApp", strings.TrimSpace(filter.SourceApp))
	query.Set("sourceInstanceId", strings.TrimSpace(filter.SourceInstanceID))
	query.Set("sourceExternalId", strings.TrimSpace(filter.SourceExternalID))
	for key, values := range query {
		if len(values) == 0 || values[0] == "" {
			query.Del(key)
		}
	}
	path := "/v1/sessions"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var response struct {
		Sessions []agentHubSession `json:"sessions"`
	}
	err := c.doJSON(ctx, http.MethodGet, path, nil, &response)
	return response.Sessions, err
}

func (c *agentHubClient) Message(ctx context.Context, sessionID string, message agentHubInboundMessage) (agentHubSession, error) {
	var response struct {
		Session agentHubSession `json:"session"`
	}
	err := c.doJSON(ctx, http.MethodPost, sessionPath(sessionID)+"/messages", message, &response)
	return response.Session, err
}

func (c *agentHubClient) Approval(ctx context.Context, sessionID, approvalID string, reply agentHubApprovalReply) (agentHubSession, error) {
	var response struct {
		Session agentHubSession `json:"session"`
	}
	path := sessionPath(sessionID) + "/approvals/" + url.PathEscape(approvalID)
	err := c.doJSON(ctx, http.MethodPost, path, reply, &response)
	return response.Session, err
}

func (c *agentHubClient) Interrupt(ctx context.Context, sessionID string) (agentHubSession, error) {
	return c.sessionAction(ctx, sessionID, "interrupt")
}

func (c *agentHubClient) Stop(ctx context.Context, sessionID string) (agentHubSession, error) {
	return c.sessionAction(ctx, sessionID, "stop")
}

// agentHubResumeRequest carries the optional launchEnvironment overlay that
// replaces selected launch environment entries when a stopped session resumes.
type agentHubResumeRequest struct {
	LaunchEnvironment map[string]string `json:"launchEnvironment,omitempty"`
}

// Resume reactivates the exact stopped AgentHub Session. The persisted
// providerSessionId and history remain owned by AgentHub; PUA supplies no
// new source tuple and therefore never creates a replacement here.
func (c *agentHubClient) Resume(ctx context.Context, sessionID string, launchEnvironment map[string]string) (agentHubSession, error) {
	var response struct {
		Session agentHubSession `json:"session"`
	}
	err := c.doJSON(ctx, http.MethodPost, sessionPath(sessionID)+"/resume", agentHubResumeRequest{
		LaunchEnvironment: launchEnvironment,
	}, &response)
	return response.Session, err
}

func (c *agentHubClient) Archive(ctx context.Context, sessionID string) (agentHubSession, error) {
	var response struct {
		Session agentHubSession `json:"session"`
	}
	err := c.doJSON(ctx, http.MethodDelete, sessionPath(sessionID), struct{}{}, &response)
	return response.Session, err
}

func (c *agentHubClient) sessionAction(ctx context.Context, sessionID, action string) (agentHubSession, error) {
	var response struct {
		Session agentHubSession `json:"session"`
	}
	err := c.doJSON(ctx, http.MethodPost, sessionPath(sessionID)+"/"+action, struct{}{}, &response)
	return response.Session, err
}

func (c *agentHubClient) doJSON(ctx context.Context, method, path string, body, output any) error {
	requestContext := ctx
	cancel := func() {}
	if _, ok := ctx.Deadline(); !ok {
		requestContext, cancel = context.WithTimeout(ctx, agentHubRequestTimeout)
	}
	defer cancel()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(requestContext, method, c.endpoint+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeAgentHubError(response)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, agentHubMaxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read AgentHub response: %w", err)
	}
	if int64(len(data)) > agentHubMaxResponseBytes {
		return fmt.Errorf("AgentHub response exceeds %d MiB limit", agentHubMaxResponseBytes>>20)
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode AgentHub response: %w", err)
	}
	return nil
}

func decodeAgentHubError(response *http.Response) error {
	var envelope struct {
		Error struct {
			Code      string          `json:"code"`
			Message   string          `json:"message"`
			Retryable bool            `json:"retryable"`
			Details   json.RawMessage `json:"details"`
			RequestID string          `json:"requestId"`
		} `json:"error"`
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr == nil {
		_ = json.Unmarshal(data, &envelope)
	}
	return &agentHubAPIError{
		StatusCode: response.StatusCode,
		Code:       envelope.Error.Code,
		Message:    envelope.Error.Message,
		Retryable:  envelope.Error.Retryable,
		Details:    envelope.Error.Details,
		RequestID:  envelope.Error.RequestID,
	}
}

func sessionPath(sessionID string) string {
	return "/v1/sessions/" + url.PathEscape(strings.TrimSpace(sessionID))
}
