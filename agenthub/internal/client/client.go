package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/disksing/pua/agenthub/internal/daemon"
	"github.com/disksing/pua/agenthub/internal/paths"
	"github.com/disksing/pua/agenthub/internal/semantic"
	"github.com/disksing/pua/agenthub/internal/session"
)

type Client struct {
	endpoint string
	http     *http.Client
}

// APIError is the stable error envelope returned by every non-2xx API
// response. Callers may use errors.As to inspect Code and Retryable.
type APIError struct {
	StatusCode int
	Code       string          `json:"code"`
	Message    string          `json:"message"`
	Retryable  bool            `json:"retryable"`
	Details    json.RawMessage `json:"details,omitempty"`
	RequestID  string          `json:"requestId,omitempty"`
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("agenthub API returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// IncompatibleDaemonError reports an API version or capability mismatch
// before a client creates a session.
type IncompatibleDaemonError struct {
	APIVersion          string
	SupportedAPIVersion string
	MissingCapabilities []string
}

func (e *IncompatibleDaemonError) Error() string {
	if e.APIVersion != e.SupportedAPIVersion {
		return fmt.Sprintf("incompatible AgentHub API version %q (client requires %q)", e.APIVersion, e.SupportedAPIVersion)
	}
	return fmt.Sprintf("AgentHub daemon is missing required capabilities: %s", strings.Join(e.MissingCapabilities, ", "))
}

type EventPage struct {
	Schema string           `json:"schema"`
	Frames []semantic.Frame `json:"frames"`
	Page   struct {
		After     int64 `json:"after"`
		Limit     int   `json:"limit"`
		NextAfter int64 `json:"nextAfter"`
		HasMore   bool  `json:"hasMore"`
		// Backward pagination fields, populated only on pages requested
		// with before/latest. NextBefore is the exclusive cursor for the
		// next older page; HasMoreBefore reports whether older events
		// remain.
		Before        int64 `json:"before"`
		NextBefore    int64 `json:"nextBefore"`
		HasMoreBefore bool  `json:"hasMoreBefore"`
	} `json:"page"`
	LatestCursor int64 `json:"latestCursor"`
}

type EventCursorGapError struct {
	Expected int64
	Got      int64
}

func (e *EventCursorGapError) Error() string {
	return fmt.Sprintf("event cursor gap: expected %d, got %d; projection stopped", e.Expected, e.Got)
}

func Discover() (*Client, error) {
	if endpoint := strings.TrimRight(strings.TrimSpace(os.Getenv("AGENTHUB_ENDPOINT")), "/"); endpoint != "" {
		return New(endpoint), nil
	}
	resolved, err := paths.Resolve()
	if err != nil {
		return nil, err
	}
	state, err := daemon.ReadState(resolved.ServerFile)
	if err != nil {
		return nil, fmt.Errorf("discover agenthub daemon: %w", err)
	}
	return New(state.Endpoint), nil
}

func New(endpoint string) *Client {
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		http:     &http.Client{Timeout: 30 * time.Minute},
	}
}

func (c *Client) Status() (map[string]any, error) {
	var result map[string]any
	if err := c.request(http.MethodGet, "/v1/status", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// RequireCapabilities rejects old or incomplete daemons before the caller
// creates a session. Unknown additional capabilities are ignored.
func (c *Client) RequireCapabilities(apiVersion string, required ...string) error {
	var status struct {
		APIVersion   string   `json:"apiVersion"`
		Capabilities []string `json:"capabilities"`
	}
	if err := c.request(http.MethodGet, "/v1/status", nil, &status); err != nil {
		return err
	}
	if status.APIVersion != apiVersion {
		return &IncompatibleDaemonError{
			APIVersion:          status.APIVersion,
			SupportedAPIVersion: apiVersion,
		}
	}
	available := make(map[string]bool, len(status.Capabilities))
	for _, capability := range status.Capabilities {
		available[capability] = true
	}
	var missing []string
	for _, capability := range required {
		if !available[capability] {
			missing = append(missing, capability)
		}
	}
	if len(missing) > 0 {
		return &IncompatibleDaemonError{
			APIVersion:          status.APIVersion,
			SupportedAPIVersion: apiVersion,
			MissingCapabilities: missing,
		}
	}
	return nil
}

func (c *Client) CreateSession(title, cwd, agentName string) (session.Session, error) {
	return c.CreateSessionWithMessage(title, cwd, agentName, "")
}

func (c *Client) CreateSessionWithMessage(title, cwd, agentName, message string) (session.Session, error) {
	body := map[string]any{
		"title":     title,
		"cwd":       cwd,
		"agentName": agentName,
	}
	if strings.TrimSpace(message) != "" {
		body["initialMessage"] = map[string]any{"schemaVersion": session.MessageSchemaOpaquePayload, "text": message}
	}
	var result struct {
		Session session.Session `json:"session"`
	}
	if err := c.request(http.MethodPost, "/v1/sessions", body, &result); err != nil {
		return session.Session{}, err
	}
	return result.Session, nil
}

func (c *Client) Agents() (map[string]any, error) {
	var result map[string]any
	if err := c.request(http.MethodGet, "/v1/agents", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) SendMessage(id, text string, steer bool) (session.Session, error) {
	return c.SendMessageInput(id, session.MessageInput{
		SchemaVersion: session.MessageSchemaOpaquePayload, Text: text, Steer: steer,
	})
}

// SendMessageInput sends one canonical input. Schema-v2 payload is opaque to
// AgentHub; schema-v1 provenance fields remain available for compatibility.
func (c *Client) SendMessageInput(id string, input session.MessageInput) (session.Session, error) {
	result, err := c.SendMessageInputResult(id, input)
	return result.Session, err
}

// SendMessageInputResult distinguishes durable input acceptance from Provider
// delivery. The older SendMessageInput method retains its Session-only return
// shape for source compatibility.
func (c *Client) SendMessageInputResult(id string, input session.MessageInput) (session.MessageSendResult, error) {
	var result session.MessageSendResult
	if err := c.request(http.MethodPost, "/v1/sessions/"+id+"/messages", input, &result); err != nil {
		return session.MessageSendResult{}, err
	}
	return result, nil
}

func (c *Client) SessionAction(id, action string) (session.Session, error) {
	var result struct {
		Session session.Session `json:"session"`
	}
	if err := c.request(http.MethodPost, "/v1/sessions/"+id+"/"+action, map[string]any{}, &result); err != nil {
		return session.Session{}, err
	}
	return result.Session, nil
}

// Resume restarts a stopped session's provider with the launch environment
// recorded on the session.
func (c *Client) Resume(id string) (session.Session, error) {
	return c.ResumeWithEnvironment(id, nil)
}

// ResumeWithEnvironment overlays launchEnvironment onto the session's
// durable launch environment (same-named entries are replaced, others are
// kept) and persists the merged result before the provider restarts, so a
// provider resume picks up the new values. A nil or empty map behaves
// exactly like Resume.
func (c *Client) ResumeWithEnvironment(id string, launchEnvironment map[string]string) (session.Session, error) {
	var result struct {
		Session session.Session `json:"session"`
	}
	body := map[string]any{}
	if len(launchEnvironment) > 0 {
		body["launchEnvironment"] = launchEnvironment
	}
	if err := c.request(http.MethodPost, "/v1/sessions/"+id+"/resume", body, &result); err != nil {
		return session.Session{}, err
	}
	return result.Session, nil
}

func (c *Client) ResolveApproval(id, approvalID, decision string) (session.Session, error) {
	var result struct {
		Session session.Session `json:"session"`
	}
	path := "/v1/sessions/" + id + "/approvals/" + approvalID
	if err := c.request(http.MethodPost, path, map[string]any{"decision": decision}, &result); err != nil {
		return session.Session{}, err
	}
	return result.Session, nil
}

func (c *Client) EventsPage(id string, after int64, limit int) (EventPage, error) {
	var result EventPage
	path := fmt.Sprintf("/v1/sessions/%s/events?after=%d&limit=%d", id, after, limit)
	if err := c.request(http.MethodGet, path, nil, &result); err != nil {
		return EventPage{}, err
	}
	if err := validateEventPage(result); err != nil {
		return EventPage{}, err
	}
	return result, nil
}

// EventsPageBefore returns the last limit events before an exclusive cursor,
// in ascending id order. A cursor past the durable head is clamped to
// head+1, so math.MaxInt64 reads the current tail (the latest=true form).
// Page backward by passing Page.NextBefore while Page.HasMoreBefore is true.
func (c *Client) EventsPageBefore(id string, before int64, limit int) (EventPage, error) {
	var result EventPage
	path := fmt.Sprintf("/v1/sessions/%s/events?before=%d&limit=%d", id, before, limit)
	if err := c.request(http.MethodGet, path, nil, &result); err != nil {
		return EventPage{}, err
	}
	if err := validateEventPage(result); err != nil {
		return EventPage{}, err
	}
	return result, nil
}

func validateEventPage(page EventPage) error {
	if page.Schema != semantic.EventsSchema {
		return fmt.Errorf("unsupported events schema %q", page.Schema)
	}
	for _, frame := range page.Frames {
		if frame.Schema != semantic.EventsSchema {
			return fmt.Errorf("unsupported semantic frame schema %q at cursor %d", frame.Schema, frame.Cursor)
		}
	}
	return nil
}

// EventsAfter catches up through the durable head reported by the first REST
// page. It stops before projecting any non-contiguous event.
func (c *Client) EventsAfter(id string, after int64) ([]semantic.Frame, error) {
	cursor := after
	var target int64 = -1
	var frames []semantic.Frame
	for {
		page, err := c.EventsPage(id, cursor, session.MaxEventPageSize)
		if err != nil {
			return nil, err
		}
		if target < 0 {
			target = page.LatestCursor
		}
		for _, frame := range page.Frames {
			if frame.Cursor > target {
				break
			}
			if frame.Cursor != cursor+1 {
				return nil, &EventCursorGapError{Expected: cursor + 1, Got: frame.Cursor}
			}
			frames = append(frames, frame)
			cursor = frame.Cursor
		}
		if cursor >= target {
			return frames, nil
		}
		if len(page.Frames) == 0 {
			return nil, &EventCursorGapError{Expected: cursor + 1, Got: 0}
		}
	}
}

func (c *Client) ListSessions(includeArchived bool) ([]session.Session, error) {
	path := "/v1/sessions"
	if includeArchived {
		path += "?includeArchived=true"
	}
	var result struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := c.request(http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result.Sessions, nil
}

// ListArchivedSessions returns only archived sessions.
func (c *Client) ListArchivedSessions() ([]session.Session, error) {
	var result struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := c.request(http.MethodGet, "/v1/sessions?archived=true", nil, &result); err != nil {
		return nil, err
	}
	return result.Sessions, nil
}

func (c *Client) GetSession(id string) (session.Session, error) {
	var result struct {
		Session session.Session `json:"session"`
	}
	if err := c.request(http.MethodGet, "/v1/sessions/"+id, nil, &result); err != nil {
		return session.Session{}, err
	}
	return result.Session, nil
}

func (c *Client) ArchiveSession(id string) (session.Session, error) {
	var result struct {
		Session session.Session `json:"session"`
	}
	if err := c.request(http.MethodDelete, "/v1/sessions/"+id, map[string]any{}, &result); err != nil {
		return session.Session{}, err
	}
	return result.Session, nil
}

func (c *Client) request(method, path string, body any, target any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, c.endpoint+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
		var apiError struct {
			Error APIError `json:"error"`
		}
		if json.Unmarshal(data, &apiError) == nil && apiError.Error.Message != "" {
			apiError.Error.StatusCode = response.StatusCode
			return &apiError.Error
		}
		return &APIError{StatusCode: response.StatusCode, Message: response.Status}
	}
	if target == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}
