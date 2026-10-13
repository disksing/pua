package serve

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/disksing/pua/internal/app"
)

const (
	resourceHistoryReferenceVersion = 1
	resourceHistoryDefaultLimit     = 20
	resourceHistoryMaxLimit         = 100
)

type resourceHistoryReference struct {
	Version      int    `json:"v"`
	Kind         string `json:"k"`
	InstanceID   string `json:"w"`
	ResourceID   string `json:"r"`
	GenerationID string `json:"g"`
	TurnID       string `json:"t,omitempty"`
	EventID      int64  `json:"e,omitempty"`
	Before       int64  `json:"b,omitempty"`
}

type resourceHistoryGeneration struct {
	Generation         int              `json:"generation"`
	GenerationID       string           `json:"generationId"`
	Title              string           `json:"title"`
	Binding            app.AgentBinding `json:"binding"`
	ResolvedProfile    string           `json:"resolvedProfile,omitempty"`
	AgentName          string           `json:"agentName,omitempty"`
	Provider           string           `json:"provider,omitempty"`
	ProviderID         string           `json:"providerId,omitempty"`
	Model              string           `json:"model,omitempty"`
	Status             string           `json:"status"`
	CreatedAt          string           `json:"createdAt"`
	UpdatedAt          string           `json:"updatedAt"`
	AgentHubSessionID  string           `json:"agentHubSessionId,omitempty"`
	ReplacementPending bool             `json:"replacementPending,omitempty"`
}

type resourceHistoryDelivery struct {
	MessageID                 string                    `json:"messageId"`
	RequestedMode             string                    `json:"requestedMode"`
	ActualMode                string                    `json:"actualMode"`
	DowngradeReason           string                    `json:"downgradeReason,omitempty"`
	Role                      string                    `json:"role"`
	Sender                    *agentHubMessageSender    `json:"sender,omitempty"`
	SenderWorkspaceInstanceID string                    `json:"senderWorkspaceInstanceId,omitempty"`
	Type                      string                    `json:"type,omitempty"`
	Causation                 *resourceMessageCausation `json:"causation,omitempty"`
	AcceptedAt                string                    `json:"acceptedAt"`
	DeliveredAt               string                    `json:"deliveredAt,omitempty"`
}

type resourceHistoryTurnSummary struct {
	Reference         string                    `json:"reference"`
	TurnID            string                    `json:"turnId"`
	Status            string                    `json:"status"`
	Closed            bool                      `json:"closed"`
	StartedAt         string                    `json:"startedAt"`
	EndedAt           string                    `json:"endedAt,omitempty"`
	DurationMS        int64                     `json:"durationMs"`
	TriggerPreview    string                    `json:"triggerPreview,omitempty"`
	TriggerRole       string                    `json:"triggerRole,omitempty"`
	TriggerSender     *agentHubMessageSender    `json:"triggerSender,omitempty"`
	FinalReplyPreview string                    `json:"finalReplyPreview,omitempty"`
	EventCount        int                       `json:"eventCount"`
	ToolEventCount    int                       `json:"toolEventCount"`
	StartEventID      int64                     `json:"startEventId"`
	LastEventID       int64                     `json:"lastEventId"`
	EndEventID        int64                     `json:"endEventId,omitempty"`
	TriggerDelivery   *resourceHistoryDelivery  `json:"triggerDelivery,omitempty"`
	Generation        resourceHistoryGeneration `json:"generation"`
}

type resourceHistoryTurnItem struct {
	Type                 string                 `json:"type"`
	Role                 string                 `json:"role,omitempty"`
	Sender               *agentHubMessageSender `json:"sender,omitempty"`
	Steer                bool                   `json:"steer,omitempty"`
	Text                 string                 `json:"text,omitempty"`
	StartEventID         int64                  `json:"startEventId"`
	EndEventID           int64                  `json:"endEventId"`
	StartEventRef        string                 `json:"startEventRef"`
	EndEventRef          string                 `json:"endEventRef"`
	StartedAt            string                 `json:"startedAt"`
	EndedAt              string                 `json:"endedAt"`
	DurationMS           int64                  `json:"durationMs"`
	Count                int                    `json:"count"`
	ThinkingCount        int                    `json:"thinkingCount,omitempty"`
	ReasoningUpdateCount int                    `json:"reasoningUpdateCount,omitempty"`
	ToolCallCount        int                    `json:"toolCallCount,omitempty"`
	Data                 json.RawMessage        `json:"data,omitempty"`
}

type resourceHistoryTurnDetail struct {
	Turn               resourceHistoryTurnSummary `json:"turn"`
	Items              []resourceHistoryTurnItem  `json:"items"`
	Deliveries         []resourceHistoryDelivery  `json:"deliveries"`
	LatestEventID      int64                      `json:"latestEventId"`
	LatestEventRef     string                     `json:"latestEventRef,omitempty"`
	TurnStartedEventID int64                      `json:"turnStartedEventId,omitempty"`
	CompletedAt        string                     `json:"completedAt,omitempty"`
}

type resourceHistoryGap struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type resourceHistorySegment struct {
	Generation resourceHistoryGeneration    `json:"generation"`
	Turns      []resourceHistoryTurnSummary `json:"turns"`
	Gap        *resourceHistoryGap          `json:"gap,omitempty"`
}

type resourceHistoryPage struct {
	CompletedTurnNumber int                      `json:"completedTurnNumber"`
	ResourceID          string                   `json:"resourceId"`
	Segments            []resourceHistorySegment `json:"segments"`
	Page                struct {
		Limit      int    `json:"limit"`
		NextCursor string `json:"nextCursor,omitempty"`
		HasMore    bool   `json:"hasMore"`
	} `json:"page"`
}

type resourceHistoryEventDetail struct {
	Reference   string                    `json:"reference"`
	Generation  resourceHistoryGeneration `json:"generation"`
	Schema      string                    `json:"schema"`
	SourceEvent agentHubEvent             `json:"sourceEvent"`
	Frame       agentHubSemanticFrame     `json:"frame"`
}

func encodeResourceHistoryReference(reference resourceHistoryReference) (string, error) {
	reference.Version = resourceHistoryReferenceVersion
	data, err := json.Marshal(reference)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeResourceHistoryReference(value, expectedKind, instanceID, resourceID string) (resourceHistoryReference, error) {
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || len(data) == 0 {
		return resourceHistoryReference{}, &resourceAPIError{Code: "invalid_history_reference", Message: "history reference is malformed"}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var reference resourceHistoryReference
	if err := decoder.Decode(&reference); err != nil {
		return resourceHistoryReference{}, &resourceAPIError{Code: "invalid_history_reference", Message: "history reference is malformed"}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return resourceHistoryReference{}, &resourceAPIError{Code: "invalid_history_reference", Message: "history reference contains trailing data"}
	}
	if reference.Version != resourceHistoryReferenceVersion || reference.Kind != expectedKind ||
		reference.InstanceID != instanceID || normalizedResourceID(reference.ResourceID) != normalizedResourceID(resourceID) ||
		strings.TrimSpace(reference.GenerationID) == "" {
		return resourceHistoryReference{}, &resourceAPIError{Code: "invalid_history_reference", Message: "history reference does not belong to this resource"}
	}
	return reference, nil
}

func resourceHistoryInstanceID(workspace serveWorkspace) (string, error) {
	opened, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		return "", err
	}
	configuration, err := opened.RuntimeConfig()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(configuration.InstanceID) == "" {
		return "", errors.New("Workspace instance id is unavailable")
	}
	return configuration.InstanceID, nil
}

func resourceHistoryGenerations(workspacePath, resourceID string) ([]generationRecord, error) {
	records, err := loadGenerationRecords(workspacePath)
	if err != nil {
		return nil, err
	}
	filtered := make([]generationRecord, 0)
	for _, record := range records {
		if generationMatchesResource(record, resourceID) && strings.TrimSpace(record.GenerationID) != "" {
			filtered = append(filtered, record)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].Generation != filtered[j].Generation {
			return filtered[i].Generation > filtered[j].Generation
		}
		return filtered[i].GenerationID > filtered[j].GenerationID
	})
	return filtered, nil
}

func historyGeneration(record generationRecord) resourceHistoryGeneration {
	return resourceHistoryGeneration{
		Generation: record.Generation, GenerationID: record.GenerationID, Title: record.Title,
		Binding:         app.AgentBinding{Kind: record.BindingKind, Name: record.BindingName},
		ResolvedProfile: record.ResolvedProfile, AgentName: record.AgentHubAgentName,
		Provider: record.AgentHubProviderName, ProviderID: record.AgentHubProviderID, Model: record.AgentHubModel,
		Status: record.Status, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
		AgentHubSessionID: record.AgentHubSessionID, ReplacementPending: record.ReplacementPending,
	}
}

func historyDeliveries(mailbox resourceMailbox, generationID, turnID string) []resourceHistoryDelivery {
	matches := make([]resourceMailboxMessage, 0)
	for _, message := range mailbox.Messages {
		if message.GenerationID == generationID && message.TurnID == turnID {
			matches = append(matches, message)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Sequence < matches[j].Sequence })
	result := make([]resourceHistoryDelivery, 0, len(matches))
	for _, message := range matches {
		result = append(result, resourceHistoryDelivery{
			MessageID: message.ID, RequestedMode: message.RequestedMode, ActualMode: message.ActualMode,
			DowngradeReason: message.DowngradeReason, Role: message.Role, Sender: message.Sender,
			SenderWorkspaceInstanceID: message.SenderWorkspaceInstanceID, Type: message.Type, Causation: message.Causation,
			AcceptedAt: message.AcceptedAt, DeliveredAt: message.DeliveredAt,
		})
	}
	return result
}

func historyTurnSummary(instanceID, resourceID string, record generationRecord, turn agentHubTurn, mailbox resourceMailbox) (resourceHistoryTurnSummary, error) {
	turnID := strings.TrimSpace(turn.TurnID)
	if turnID == "" {
		turnID = strings.TrimSpace(turn.ID)
	}
	reference, err := encodeResourceHistoryReference(resourceHistoryReference{
		Kind: "turn", InstanceID: instanceID, ResourceID: resourceID, GenerationID: record.GenerationID, TurnID: turnID,
	})
	if err != nil {
		return resourceHistoryTurnSummary{}, err
	}
	deliveries := historyDeliveries(mailbox, record.GenerationID, turnID)
	var trigger *resourceHistoryDelivery
	if len(deliveries) > 0 {
		value := deliveries[0]
		trigger = &value
	}
	triggerPreview, triggerRole, triggerSender := turn.TriggerPreview, turn.TriggerRole, turn.TriggerSender
	if payload, ok := decodePUAMessagePayload(turn.TriggerPayload); ok {
		triggerPreview = resourceMessagePreview(payload.Text)
		triggerRole, triggerSender = payload.Role, payload.Sender
	}
	return resourceHistoryTurnSummary{
		Reference: reference, TurnID: turnID, Status: turn.Status, Closed: turn.Closed,
		StartedAt: turn.StartedAt, EndedAt: turn.EndedAt, DurationMS: turn.DurationMS,
		TriggerPreview: triggerPreview, TriggerRole: triggerRole, TriggerSender: triggerSender,
		FinalReplyPreview: turn.FinalReplyPreview, EventCount: turn.EventCount, ToolEventCount: turn.ToolEventCount,
		StartEventID: turn.StartEventID, LastEventID: turn.LastEventID, EndEventID: turn.EndEventID,
		TriggerDelivery: trigger, Generation: historyGeneration(record),
	}, nil
}

func resourceMessagePreview(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= 160 {
		return value
	}
	return string(runes[:160]) + "…"
}

func historyGapFor(record generationRecord, err error) *resourceHistoryGap {
	gap := &resourceHistoryGap{Code: "session_unreadable", Message: err.Error(), Retryable: true}
	if strings.TrimSpace(record.AgentHubSessionID) == "" {
		if record.Status == "starting" {
			gap.Code, gap.Message, gap.Retryable = "session_starting", "generation is waiting for its AgentHub Session to start", true
			return gap
		}
		gap.Code, gap.Message, gap.Retryable = "session_missing", "generation has no AgentHub Session reference", false
		return gap
	}
	var upstream *agentHubAPIError
	if errors.As(err, &upstream) {
		switch {
		case upstream.StatusCode == http.StatusNotFound:
			gap.Code, gap.Retryable = "session_missing", false
		case upstream.Code == "session_store_failed":
			gap.Code = "history_corrupt"
		case upstream.StatusCode >= 500:
			gap.Code = "agenthub_unavailable"
		}
	} else {
		gap.Code = "agenthub_unavailable"
	}
	return gap
}

func parseResourceHistoryLimit(value string) (int, error) {
	if strings.TrimSpace(value) == "" {
		return resourceHistoryDefaultLimit, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit <= 0 || limit > resourceHistoryMaxLimit {
		return 0, &resourceAPIError{Code: "invalid_request", Message: fmt.Sprintf("limit must be between 1 and %d", resourceHistoryMaxLimit)}
	}
	return limit, nil
}

func validateResourceHistoryTarget(workspace serveWorkspace, resourceID string) error {
	exists, _, _, err := resourceExistsAndArchived(workspace.Path, resourceID)
	if err != nil || !exists {
		if err == nil {
			err = fmt.Errorf("resource not found: %s", resourceID)
		}
		return &resourceAPIError{Code: "resource_not_found", Message: err.Error()}
	}
	return nil
}

func (m *agentManager) resourceHistoryPage(ctx context.Context, workspace serveWorkspace, resourceID, cursorValue string, limit int) (resourceHistoryPage, error) {
	resourceID = normalizedResourceID(resourceID)
	if err := validateResourceHistoryTarget(workspace, resourceID); err != nil {
		return resourceHistoryPage{}, err
	}
	instanceID, err := resourceHistoryInstanceID(workspace)
	if err != nil {
		return resourceHistoryPage{}, err
	}
	records, err := resourceHistoryGenerations(workspace.Path, resourceID)
	if err != nil {
		return resourceHistoryPage{}, &resourceAPIError{Code: "history_index_corrupt", Message: err.Error()}
	}
	page := resourceHistoryPage{ResourceID: resourceID, Segments: []resourceHistorySegment{}}
	page.CompletedTurnNumber = latestCompletedTurnByResource(records)[resourceID].TurnNumber
	page.Page.Limit = limit
	if len(records) == 0 {
		return page, nil
	}
	start, before := 0, int64(0)
	if strings.TrimSpace(cursorValue) != "" {
		cursor, decodeErr := decodeResourceHistoryReference(cursorValue, "cursor", instanceID, resourceID)
		if decodeErr != nil {
			if apiErr, ok := decodeErr.(*resourceAPIError); ok {
				apiErr.Code = "invalid_history_cursor"
			}
			return resourceHistoryPage{}, decodeErr
		}
		before = cursor.Before
		found := false
		for index := range records {
			if records[index].GenerationID == cursor.GenerationID {
				start, found = index, true
				break
			}
		}
		if !found {
			return resourceHistoryPage{}, &resourceAPIError{Code: "invalid_history_cursor", Message: "history cursor generation no longer belongs to this resource"}
		}
	}
	mailbox, err := loadResourceMailboxForResource(workspace.Path, resourceID)
	if err != nil {
		return resourceHistoryPage{}, err
	}
	_, client, err := m.agentHubRuntimeConfig()
	if err != nil {
		return resourceHistoryPage{}, &resourceAPIError{Code: "history_unavailable", Message: err.Error()}
	}
	remaining := limit
	setNext := func(record generationRecord, nextBefore int64) error {
		encoded, encodeErr := encodeResourceHistoryReference(resourceHistoryReference{
			Kind: "cursor", InstanceID: instanceID, ResourceID: resourceID, GenerationID: record.GenerationID, Before: nextBefore,
		})
		if encodeErr != nil {
			return encodeErr
		}
		page.Page.NextCursor, page.Page.HasMore = encoded, true
		return nil
	}
	for index := start; index < len(records); index++ {
		record := records[index]
		if remaining == 0 {
			if err := setNext(record, func() int64 {
				if index == start {
					return before
				}
				return 0
			}()); err != nil {
				return resourceHistoryPage{}, err
			}
			break
		}
		segment := resourceHistorySegment{Generation: historyGeneration(record), Turns: []resourceHistoryTurnSummary{}}
		if strings.TrimSpace(record.AgentHubSessionID) == "" {
			segment.Gap = historyGapFor(record, errors.New("generation has no AgentHub Session reference"))
			page.Segments = append(page.Segments, segment)
			remaining--
			if remaining == 0 && index+1 < len(records) {
				if err := setNext(records[index+1], 0); err != nil {
					return resourceHistoryPage{}, err
				}
				break
			}
			before = 0
			continue
		}
		turnPage, fetchErr := client.SessionTurns(ctx, record.AgentHubSessionID, before, before == 0, remaining)
		if fetchErr != nil {
			segment.Gap = historyGapFor(record, fetchErr)
			page.Segments = append(page.Segments, segment)
			remaining--
			if remaining == 0 && index+1 < len(records) {
				if err := setNext(records[index+1], 0); err != nil {
					return resourceHistoryPage{}, err
				}
				break
			}
			before = 0
			continue
		}
		for _, turn := range turnPage.Turns {
			summary, summaryErr := historyTurnSummary(instanceID, resourceID, record, turn, mailbox)
			if summaryErr != nil {
				return resourceHistoryPage{}, summaryErr
			}
			segment.Turns = append(segment.Turns, summary)
			remaining--
		}
		page.Segments = append(page.Segments, segment)
		if turnPage.Page.HasMoreBefore {
			if turnPage.Page.NextBefore <= 0 {
				return resourceHistoryPage{}, &resourceAPIError{Code: "history_corrupt", Message: "AgentHub Turn page did not provide a backward cursor"}
			}
			if err := setNext(record, turnPage.Page.NextBefore); err != nil {
				return resourceHistoryPage{}, err
			}
			break
		}
		before = 0
		if remaining == 0 && index+1 < len(records) {
			if err := setNext(records[index+1], 0); err != nil {
				return resourceHistoryPage{}, err
			}
			break
		}
	}
	return page, nil
}

func resourceHistoryGenerationByReference(workspace serveWorkspace, resourceID string, reference resourceHistoryReference) (generationRecord, error) {
	records, err := resourceHistoryGenerations(workspace.Path, resourceID)
	if err != nil {
		return generationRecord{}, err
	}
	for _, record := range records {
		if record.GenerationID == reference.GenerationID {
			if strings.TrimSpace(record.AgentHubSessionID) == "" {
				return generationRecord{}, &resourceAPIError{Code: "session_missing", Message: "generation has no AgentHub Session reference"}
			}
			return record, nil
		}
	}
	return generationRecord{}, &resourceAPIError{Code: "history_reference_not_found", Message: "history reference generation was not found"}
}

// resourceHistoryGenerationByID resolves any generation recorded in the
// resource History, not just the current one, so read-only event paging can
// expand compact Turn ranges from older generations. An unknown generation
// keeps the generation_changed semantics live clients already handle.
func resourceHistoryGenerationByID(workspace serveWorkspace, resourceID, generationID string) (generationRecord, error) {
	resourceID = normalizedResourceID(resourceID)
	if err := validateResourceHistoryTarget(workspace, resourceID); err != nil {
		return generationRecord{}, err
	}
	records, err := resourceHistoryGenerations(workspace.Path, resourceID)
	if err != nil {
		return generationRecord{}, err
	}
	for _, record := range records {
		if record.GenerationID == generationID {
			if strings.TrimSpace(record.AgentHubSessionID) == "" {
				return generationRecord{}, &resourceAPIError{Code: "session_missing", Message: "generation has no AgentHub Session reference"}
			}
			return record, nil
		}
	}
	return generationRecord{}, &resourceAPIError{Code: "generation_changed", Message: "resource current generation changed; refresh resource status and history head"}
}

func (m *agentManager) resourceHistoryTurn(ctx context.Context, workspace serveWorkspace, resourceID, value string) (resourceHistoryTurnDetail, error) {
	resourceID = normalizedResourceID(resourceID)
	if err := validateResourceHistoryTarget(workspace, resourceID); err != nil {
		return resourceHistoryTurnDetail{}, err
	}
	instanceID, err := resourceHistoryInstanceID(workspace)
	if err != nil {
		return resourceHistoryTurnDetail{}, err
	}
	reference, err := decodeResourceHistoryReference(value, "turn", instanceID, resourceID)
	if err != nil {
		return resourceHistoryTurnDetail{}, err
	}
	record, err := resourceHistoryGenerationByReference(workspace, resourceID, reference)
	if err != nil {
		return resourceHistoryTurnDetail{}, err
	}
	return m.resourceHistoryTurnForRecord(ctx, workspace, resourceID, instanceID, record, reference.TurnID)
}

// resourceHistoryTurnByID resolves a Turn named by the live event identity
// instead of requiring the caller to already have its opaque History
// reference. Terminal events can outlive the small History head page in the
// web client, so recovery must not depend on the Turn still being among the
// latest summaries.
func (m *agentManager) resourceHistoryTurnByID(ctx context.Context, workspace serveWorkspace, resourceID, generationID, turnID string) (resourceHistoryTurnDetail, error) {
	resourceID = normalizedResourceID(resourceID)
	generationID, turnID = strings.TrimSpace(generationID), strings.TrimSpace(turnID)
	if generationID == "" || turnID == "" {
		return resourceHistoryTurnDetail{}, &resourceAPIError{Code: "invalid_request", Message: "generationId and turnId are required"}
	}
	record, err := resourceHistoryGenerationByID(workspace, resourceID, generationID)
	if err != nil {
		return resourceHistoryTurnDetail{}, err
	}
	instanceID, err := resourceHistoryInstanceID(workspace)
	if err != nil {
		return resourceHistoryTurnDetail{}, err
	}
	return m.resourceHistoryTurnForRecord(ctx, workspace, resourceID, instanceID, record, turnID)
}

func (m *agentManager) resourceHistoryTurnForRecord(ctx context.Context, workspace serveWorkspace, resourceID, instanceID string, record generationRecord, turnID string) (resourceHistoryTurnDetail, error) {
	_, client, err := m.agentHubRuntimeConfig()
	if err != nil {
		return resourceHistoryTurnDetail{}, &resourceAPIError{Code: "history_unavailable", Message: err.Error()}
	}
	turn, latestEventID, err := client.SessionTurn(ctx, record.AgentHubSessionID, turnID)
	if err != nil {
		var upstream *agentHubAPIError
		if errors.As(err, &upstream) && upstream.StatusCode == http.StatusNotFound {
			return resourceHistoryTurnDetail{}, &resourceAPIError{Code: "history_turn_not_found", Message: err.Error()}
		}
		return resourceHistoryTurnDetail{}, &resourceAPIError{Code: "history_unavailable", Message: err.Error()}
	}
	mailbox, err := loadResourceMailboxForResource(workspace.Path, resourceID)
	if err != nil {
		return resourceHistoryTurnDetail{}, err
	}
	summary, err := historyTurnSummary(instanceID, resourceID, record, turn, mailbox)
	if err != nil {
		return resourceHistoryTurnDetail{}, err
	}
	if summary.TurnID != turnID {
		return resourceHistoryTurnDetail{}, &resourceAPIError{Code: "history_corrupt", Message: "AgentHub Turn identity does not match the requested Turn"}
	}
	detail := resourceHistoryTurnDetail{
		Turn: summary, Items: []resourceHistoryTurnItem{}, Deliveries: historyDeliveries(mailbox, record.GenerationID, summary.TurnID),
		LatestEventID: latestEventID, TurnStartedEventID: turn.TurnStartedEventID, CompletedAt: turn.CompletedAt,
	}
	for _, item := range turn.Items {
		itemText, itemRole, itemSender := puaMessagePresentation(item.Text, item.Role, item.Sender, item.Payload)
		startRef, encodeErr := encodeResourceHistoryReference(resourceHistoryReference{
			Kind: "event", InstanceID: instanceID, ResourceID: resourceID, GenerationID: record.GenerationID, EventID: item.StartEventID,
		})
		if encodeErr != nil {
			return resourceHistoryTurnDetail{}, encodeErr
		}
		endRef, encodeErr := encodeResourceHistoryReference(resourceHistoryReference{
			Kind: "event", InstanceID: instanceID, ResourceID: resourceID, GenerationID: record.GenerationID, EventID: item.EndEventID,
		})
		if encodeErr != nil {
			return resourceHistoryTurnDetail{}, encodeErr
		}
		detail.Items = append(detail.Items, resourceHistoryTurnItem{
			Type: item.Type, Role: itemRole, Sender: itemSender, Steer: item.Steer, Text: itemText,
			StartEventID: item.StartEventID, EndEventID: item.EndEventID, StartEventRef: startRef, EndEventRef: endRef,
			StartedAt: item.StartedAt, EndedAt: item.EndedAt, DurationMS: item.DurationMS, Count: item.Count, Data: item.Data,
			ThinkingCount: item.ThinkingCount, ReasoningUpdateCount: item.ReasoningUpdateCount, ToolCallCount: item.ToolCallCount,
		})
	}
	if latestEventID > 0 {
		detail.LatestEventRef, _ = encodeResourceHistoryReference(resourceHistoryReference{
			Kind: "event", InstanceID: instanceID, ResourceID: resourceID, GenerationID: record.GenerationID, EventID: latestEventID,
		})
	}
	return detail, nil
}

func (m *agentManager) resourceHistoryEvent(ctx context.Context, workspace serveWorkspace, resourceID, value string) (resourceHistoryEventDetail, error) {
	resourceID = normalizedResourceID(resourceID)
	if err := validateResourceHistoryTarget(workspace, resourceID); err != nil {
		return resourceHistoryEventDetail{}, err
	}
	instanceID, err := resourceHistoryInstanceID(workspace)
	if err != nil {
		return resourceHistoryEventDetail{}, err
	}
	reference, err := decodeResourceHistoryReference(value, "event", instanceID, resourceID)
	if err != nil || reference.EventID <= 0 {
		if err == nil {
			err = &resourceAPIError{Code: "invalid_history_reference", Message: "event reference is invalid"}
		}
		return resourceHistoryEventDetail{}, err
	}
	record, err := resourceHistoryGenerationByReference(workspace, resourceID, reference)
	if err != nil {
		return resourceHistoryEventDetail{}, err
	}
	_, client, err := m.agentHubRuntimeConfig()
	if err != nil {
		return resourceHistoryEventDetail{}, &resourceAPIError{Code: "history_unavailable", Message: err.Error()}
	}
	detail, err := client.SessionEvent(ctx, record.AgentHubSessionID, reference.EventID)
	if err != nil {
		var upstream *agentHubAPIError
		if errors.As(err, &upstream) && upstream.StatusCode == http.StatusNotFound {
			return resourceHistoryEventDetail{}, &resourceAPIError{Code: "history_event_not_found", Message: err.Error()}
		}
		return resourceHistoryEventDetail{}, &resourceAPIError{Code: "history_unavailable", Message: err.Error()}
	}
	if detail.SourceEvent.SessionID != "" && detail.SourceEvent.SessionID != record.AgentHubSessionID {
		return resourceHistoryEventDetail{}, &resourceAPIError{Code: "history_corrupt", Message: "AgentHub Event belongs to a different Session"}
	}
	return resourceHistoryEventDetail{
		Reference: value, Generation: historyGeneration(record), Schema: detail.Schema,
		SourceEvent: detail.SourceEvent, Frame: detail.Frame,
	}, nil
}

func (m *agentManager) handleResourceHistory(w http.ResponseWriter, r *http.Request, workspaceID, resourceID string, parts []string) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	workspace, err := m.server.workspace(workspaceID)
	if err != nil {
		writeError(w, &resourceAPIError{Code: "workspace_not_owned", Message: err.Error()}, http.StatusNotFound)
		return
	}
	if len(parts) == 1 && parts[0] == "turns" {
		limit, parseErr := parseResourceHistoryLimit(r.URL.Query().Get("limit"))
		if parseErr != nil {
			writeError(w, parseErr, resourceErrorStatus(parseErr))
			return
		}
		page, historyErr := m.resourceHistoryPage(r.Context(), workspace, resourceID, r.URL.Query().Get("cursor"), limit)
		if historyErr != nil {
			writeError(w, historyErr, resourceErrorStatus(historyErr))
			return
		}
		writeJSON(w, page)
		return
	}
	if len(parts) == 2 && parts[0] == "turns" {
		if parts[1] == "by-id" {
			detail, historyErr := m.resourceHistoryTurnByID(r.Context(), workspace, resourceID, r.URL.Query().Get("generationId"), r.URL.Query().Get("turnId"))
			if historyErr != nil {
				writeError(w, historyErr, resourceErrorStatus(historyErr))
				return
			}
			writeJSON(w, detail)
			return
		}
		detail, historyErr := m.resourceHistoryTurn(r.Context(), workspace, resourceID, parts[1])
		if historyErr != nil {
			writeError(w, historyErr, resourceErrorStatus(historyErr))
			return
		}
		writeJSON(w, detail)
		return
	}
	if len(parts) == 2 && parts[0] == "events" {
		detail, historyErr := m.resourceHistoryEvent(r.Context(), workspace, resourceID, parts[1])
		if historyErr != nil {
			writeError(w, historyErr, resourceErrorStatus(historyErr))
			return
		}
		writeJSON(w, detail)
		return
	}
	http.NotFound(w, r)
}
