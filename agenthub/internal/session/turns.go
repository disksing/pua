package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const turnPreviewLimit = 280

// TurnsPage rebuilds a compact Turn index from the durable event source of
// truth. after and before are exclusive FirstEventID cursors; latest selects
// the newest page. Archived Sessions use the same path and remain readable.
func (s *Store) TurnsPage(id string, after, before int64, latest bool, limit int) (TurnPage, error) {
	if after < 0 || before < 0 || (after > 0 && (before > 0 || latest)) || (before > 0 && latest) {
		return TurnPage{}, errors.New("invalid turn cursor")
	}
	if limit <= 0 {
		limit = DefaultEventPageSize
	}
	if limit > MaxEventPageSize {
		limit = MaxEventPageSize
	}
	state, err := s.state(id)
	if err != nil {
		return TurnPage{}, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	turns, err := s.turnRecordsLocked(state, id)
	if err != nil {
		return TurnPage{}, err
	}
	latestCursor := int64(0)
	if len(turns) > 0 {
		latestCursor = turns[len(turns)-1].FirstEventID
	}
	page := TurnPage{Turns: []TurnSummary{}, After: after, Before: before, Limit: limit, LatestCursor: latestCursor, LatestEventID: state.session.LastEventID, NextAfter: after}
	if latest {
		before = latestCursor + 1
		page.Before = before
	}
	if before > 0 {
		end := sort.Search(len(turns), func(i int) bool { return turns[i].FirstEventID >= before })
		start := end - limit
		if start < 0 {
			start = 0
		}
		page.Turns = cloneTurnSummaries(turns[start:end])
		page.HasMoreBefore = start > 0
		if len(page.Turns) > 0 {
			page.NextBefore = page.Turns[0].FirstEventID
			page.NextAfter = page.Turns[len(page.Turns)-1].FirstEventID
		}
		page.HasMore = end < len(turns)
		return page, nil
	}
	start := sort.Search(len(turns), func(i int) bool { return turns[i].FirstEventID > after })
	end := start + limit
	if end > len(turns) {
		end = len(turns)
	}
	page.Turns = cloneTurnSummaries(turns[start:end])
	page.HasMore = end < len(turns)
	if len(page.Turns) > 0 {
		page.NextAfter = page.Turns[len(page.Turns)-1].FirstEventID
	}
	return page, nil
}

// Turn returns one compact materialized Turn, repairing a missing or stale
// projection from canonical Events before reporting not found.
func (s *Store) Turn(id, turnID string) (TurnSummary, error) {
	state, err := s.state(id)
	if err != nil {
		return TurnSummary{}, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	turns, err := s.turnRecordsLocked(state, id)
	if err != nil {
		return TurnSummary{}, err
	}
	for _, turn := range turns {
		if turn.ID == turnID || turn.TurnID == turnID {
			return cloneTurnSummaries([]TurnSummary{turn})[0], nil
		}
	}
	return TurnSummary{}, ErrNotFound
}

func (s *Store) projectTurnEventLocked(state *sessionState, id string, event Event) error {
	if event.TurnID == "" || (event.Type != "turn.started" && event.Type != EventTurnCompleted && event.Type != EventTurnFailed && event.Type != EventTurnCancelled) {
		return nil
	}
	turns := buildTurnSummaries(state.events)
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].ID != event.TurnID {
			continue
		}
		encoded, err := json.Marshal(turns[i])
		if err != nil {
			return err
		}
		return appendDurable(s.turnsPath(id), append(encoded, '\n'))
	}
	return nil
}

func (s *Store) turnRecordsLocked(state *sessionState, id string) ([]TurnSummary, error) {
	path := s.turnsPathFor(state, id)
	_, statErr := os.Stat(path)
	turns, err := readTurnRecordsRepairTail(path)
	needsRebuild := err != nil || errors.Is(statErr, os.ErrNotExist) || (len(turns) == 0 && state.session.CurrentTurnID != "")
	if !needsRebuild {
		for _, turn := range turns {
			if !turn.Closed && state.session.CurrentTurnID == "" {
				needsRebuild = true
				break
			}
		}
	}
	if state.session.CurrentTurnID != "" {
		// The opened record is intentionally minimal. Active content is folded
		// from its stable Event range so callers see current messages and tools.
		needsRebuild = true
	}
	if !needsRebuild {
		return turns, nil
	}
	if err := s.ensureEventsLocked(state, id); err != nil {
		return nil, err
	}
	turns = buildTurnSummaries(state.events)
	if err := writeTurnRecordsAtomic(s.turnsPathFor(state, id), turns); err != nil {
		return nil, fmt.Errorf("rebuild turn projection: %w", err)
	}
	return turns, nil
}

func readTurnRecordsRepairTail(path string) ([]TurnSummary, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lastNewline := bytes.LastIndexByte(data, '\n')
		validLength := lastNewline + 1
		if err := os.Truncate(path, int64(validLength)); err != nil {
			return nil, err
		}
		data = data[:validLength]
	}
	latest := make(map[string]TurnSummary)
	order := make(map[string]int64)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var turn TurnSummary
		if err := json.Unmarshal(scanner.Bytes(), &turn); err != nil {
			return nil, err
		}
		if turn.ID == "" {
			turn.ID = turn.TurnID
		}
		if turn.TurnID == "" {
			turn.TurnID = turn.ID
		}
		if turn.ID == "" || turn.StartEventID <= 0 {
			return nil, errors.New("invalid turn projection record")
		}
		hasMaterializedItems := turn.Items != nil
		turn.Items = normalizeTurnItems(turn.Items)
		if hasMaterializedItems {
			deriveFinalReply(&turn)
		}
		latest[turn.ID] = turn
		order[turn.ID] = turn.StartEventID
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	turns := make([]TurnSummary, 0, len(latest))
	for _, turn := range latest {
		turns = append(turns, turn)
	}
	sort.Slice(turns, func(i, j int) bool { return order[turns[i].ID] < order[turns[j].ID] })
	return turns, nil
}

func writeTurnRecordsAtomic(path string, turns []TurnSummary) error {
	var data []byte
	for _, turn := range turns {
		encoded, err := json.Marshal(turn)
		if err != nil {
			return err
		}
		data = append(data, encoded...)
		data = append(data, '\n')
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	file, err := os.OpenFile(temp, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

func buildTurnSummaries(events []Event) []TurnSummary {
	turns := make([]TurnSummary, 0)
	byID := make(map[string]int)
	toolIdentities := make(map[string]map[string]struct{})
	for _, event := range events {
		if event.TurnID == "" {
			continue
		}
		index, ok := byID[event.TurnID]
		if !ok {
			index = len(turns)
			byID[event.TurnID] = index
			turns = append(turns, TurnSummary{
				ID: event.TurnID, TurnID: event.TurnID, Status: "active", StartedAt: eventStartTime(event),
				StartEventID: event.ID, FirstEventID: event.ID, LastEventID: event.ID, Items: []TurnItem{},
			})
		}
		turn := &turns[index]
		turn.LastEventID = event.ID
		turn.EventCount++
		switch event.Type {
		case "turn.started":
			turn.Status = "active"
			turn.Closed = false
			turn.CompletedAt = nil
			turn.EndedAt = nil
			turn.EndEventID = 0
			turn.TurnStartedEventID = event.ID
			appendLifecycleItem(turn, event)
		case EventMessageInput:
			var input MessageInput
			if json.Unmarshal(event.Data, &input) == nil {
				appendInputMessageItem(turn, event, input)
			}
			if turn.TriggerEventID == 0 {
				if json.Unmarshal(event.Data, &input) == nil {
					turn.TriggerEventID = event.ID
					turn.TriggerPreview = preview(input.Text)
					turn.TriggerRole = input.Role
					turn.TriggerSender = cloneMessageSender(input.Sender)
					turn.TriggerPayload = cloneRawMessage(input.Payload)
					turn.TriggerMessageID = input.MessageID
				}
			}
		case "message.assistant.delta":
			var data struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(event.Data, &data) == nil {
				appendAssistantItem(turn, event, data.Text)
			}
		case "message.reasoning.delta":
			appendThinkingActivity(turn, event)
		case "tool.event", "tool.call":
			turn.ToolEventCount++
			appendToolActivity(turn, event, toolIdentities)
		case "approval.requested", "approval.resolved":
			appendStructuredItem(turn, event, "approval")
		case "provider.error":
			appendStructuredItem(turn, event, "error")
		case EventTurnCompleted, EventTurnFailed, EventTurnCancelled:
			turn.Status = strings.TrimPrefix(event.Type, "turn.")
			turn.Closed = true
			completed := event.Time
			turn.CompletedAt = &completed
			turn.EndedAt = &completed
			turn.EndEventID = event.ID
			appendLifecycleItem(turn, event)
		default:
			if strings.HasPrefix(event.Type, "session.") {
				appendLifecycleItem(turn, event)
			} else if !invisibleTurnEvent(event.Type) {
				appendUnknownItem(turn, event)
			}
		}
		turn.DurationMS = durationMilliseconds(turn.StartedAt, event.Time)
	}
	for index := range turns {
		deriveFinalReply(&turns[index])
	}
	return turns
}

func invisibleTurnEvent(eventType string) bool {
	return eventType == EventMessageDelivery || eventType == "provider.event" || eventType == "provider.metadata" ||
		eventType == "provider.stderr" || eventType == "plan.event" || eventType == "provider.turn.started" ||
		eventType == "provider.turn.completed" || strings.HasPrefix(eventType, "provider.process.")
}

func appendToolActivity(turn *TurnSummary, event Event, identities map[string]map[string]struct{}) {
	item := appendActivityItem(turn, event)
	identity := toolIdentity(event.Data)
	key := turn.ID + ":" + fmt.Sprint(item.StartEventID)
	seen := identities[key]
	if seen == nil {
		seen = make(map[string]struct{})
		identities[key] = seen
	}
	if identity == "" {
		item.ToolCallCount++
	} else if _, exists := seen[identity]; !exists {
		seen[identity] = struct{}{}
		item.ToolCallCount++
	}
	item.activityTail = "tool"
	updateActivityItem(item, event)
}

func appendThinkingActivity(turn *TurnSummary, event Event) {
	item := appendActivityItem(turn, event)
	if item.activityTail != "thinking" {
		item.ThinkingCount++
	}
	item.ReasoningUpdateCount++
	item.activityTail = "thinking"
	updateActivityItem(item, event)
}

func appendActivityItem(turn *TurnSummary, event Event) *TurnItem {
	if len(turn.Items) == 0 || turn.Items[len(turn.Items)-1].Type != "activity" {
		item := newTurnItem(event, "activity")
		item.Count = 0
		turn.Items = append(turn.Items, item)
		return &turn.Items[len(turn.Items)-1]
	}
	return &turn.Items[len(turn.Items)-1]
}

func updateActivityItem(item *TurnItem, event Event) {
	item.EndEventID = event.ID
	item.EndedAt = event.Time
	item.DurationMS = durationMilliseconds(item.StartedAt, item.EndedAt)
	item.Count = item.ThinkingCount + item.ToolCallCount
}

func normalizeTurnItems(items []TurnItem) []TurnItem {
	normalized := make([]TurnItem, 0, len(items))
	for _, item := range items {
		if item.Type == "thinking" || item.Type == "tool" {
			legacyCount := item.Count
			if legacyCount <= 0 {
				legacyCount = 1
			}
			legacyType := item.Type
			item.Type = "activity"
			item.ThinkingCount = 0
			item.ReasoningUpdateCount = 0
			item.ToolCallCount = 0
			if legacyType == "thinking" {
				item.ThinkingCount = 1
				item.ReasoningUpdateCount = legacyCount
			} else {
				item.ToolCallCount = legacyCount
			}
		}
		if item.Type != "activity" {
			normalized = append(normalized, item)
			continue
		}
		if item.ThinkingCount == 0 && item.ToolCallCount == 0 {
			item.ThinkingCount = 1
			item.ReasoningUpdateCount = max(1, item.Count)
		}
		item.Count = item.ThinkingCount + item.ToolCallCount
		if len(normalized) > 0 && normalized[len(normalized)-1].Type == "activity" {
			previous := &normalized[len(normalized)-1]
			previous.EndEventID = item.EndEventID
			previous.EndedAt = item.EndedAt
			previous.DurationMS = durationMilliseconds(previous.StartedAt, previous.EndedAt)
			previous.ThinkingCount += item.ThinkingCount
			previous.ReasoningUpdateCount += item.ReasoningUpdateCount
			previous.ToolCallCount += item.ToolCallCount
			previous.Count = previous.ThinkingCount + previous.ToolCallCount
			continue
		}
		normalized = append(normalized, item)
	}
	return normalized
}

func toolIdentity(data json.RawMessage) string {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return ""
	}
	keys := map[string]bool{"toolCallId": true, "callId": true, "itemId": true}
	var find func(any) string
	find = func(current any) string {
		switch typed := current.(type) {
		case map[string]any:
			for key, entry := range typed {
				if keys[key] {
					if text, ok := entry.(string); ok && strings.TrimSpace(text) != "" {
						return strings.TrimSpace(text)
					}
				}
			}
			if item, ok := typed["item"].(map[string]any); ok {
				if text, ok := item["id"].(string); ok && strings.TrimSpace(text) != "" {
					return strings.TrimSpace(text)
				}
			}
			for _, entry := range typed {
				if found := find(entry); found != "" {
					return found
				}
			}
		case []any:
			for _, entry := range typed {
				if found := find(entry); found != "" {
					return found
				}
			}
		}
		return ""
	}
	return find(value)
}

func eventStartTime(event Event) time.Time {
	if event.StartTime != nil {
		return *event.StartTime
	}
	return event.Time
}

func durationMilliseconds(start, end time.Time) int64 {
	if start.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start).Milliseconds()
}

func newTurnItem(event Event, itemType string) TurnItem {
	started := eventStartTime(event)
	return TurnItem{
		Type: itemType, StartEventID: event.ID, EndEventID: event.ID,
		StartedAt: started, EndedAt: event.Time, DurationMS: durationMilliseconds(started, event.Time), Count: 1,
	}
}

func appendInputMessageItem(turn *TurnSummary, event Event, input MessageInput) {
	item := newTurnItem(event, "message")
	item.Role, item.Sender = input.Role, cloneMessageSender(input.Sender)
	item.Steer, item.Text = input.Steer, input.Text
	item.Payload, item.MessageID = cloneRawMessage(input.Payload), input.MessageID
	turn.Items = append(turn.Items, item)
}

func appendMessageItem(turn *TurnSummary, event Event, role MessageRole, text string) {
	item := newTurnItem(event, "message")
	item.Role, item.Text = role, text
	turn.Items = append(turn.Items, item)
}

func appendAssistantItem(turn *TurnSummary, event Event, text string) {
	if len(turn.Items) > 0 {
		last := &turn.Items[len(turn.Items)-1]
		if last.Type == "message" && last.Role == MessageRoleAssistant {
			last.Text += text
			last.EndEventID = event.ID
			last.EndedAt = event.Time
			last.DurationMS = durationMilliseconds(last.StartedAt, last.EndedAt)
			last.Count++
			return
		}
	}
	appendMessageItem(turn, event, MessageRoleAssistant, text)
}

// deriveFinalReply keeps the Turn summary aligned with the conversation
// projection: progress updates are assistant messages too, while the last
// non-empty assistant message is the reply that closes or currently tails the
// Turn. Items may combine multiple durable delta events from one message.
func deriveFinalReply(turn *TurnSummary) {
	turn.FinalReplyEventID = 0
	turn.FinalReplyPreview = ""
	for index := len(turn.Items) - 1; index >= 0; index-- {
		item := turn.Items[index]
		if item.Type != "message" || item.Role != MessageRoleAssistant || strings.TrimSpace(item.Text) == "" {
			continue
		}
		turn.FinalReplyEventID = item.EndEventID
		turn.FinalReplyPreview = preview(item.Text)
		return
	}
}

func appendStructuredItem(turn *TurnSummary, event Event, itemType string) {
	item := newTurnItem(event, itemType)
	item.Data = append(json.RawMessage(nil), event.Data...)
	if itemType == "error" {
		var data struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(event.Data, &data) == nil {
			item.Text = data.Message
		}
	}
	turn.Items = append(turn.Items, item)
}

func appendLifecycleItem(turn *TurnSummary, event Event) {
	item := newTurnItem(event, "lifecycle")
	item.Text = event.Type
	item.Data = append(json.RawMessage(nil), event.Data...)
	turn.Items = append(turn.Items, item)
}

func appendUnknownItem(turn *TurnSummary, event Event) {
	item := newTurnItem(event, "unknown")
	item.Text = event.Type
	item.Data = append(json.RawMessage(nil), event.Data...)
	turn.Items = append(turn.Items, item)
}

func preview(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= turnPreviewLimit {
		return value
	}
	return string(runes[:turnPreviewLimit]) + "…"
}

func cloneMessageSender(value *MessageSender) *MessageSender {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneRawMessage(value json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), value...)
}

func cloneTurnSummaries(values []TurnSummary) []TurnSummary {
	cloned := append([]TurnSummary(nil), values...)
	for i := range cloned {
		cloned[i].TriggerSender = cloneMessageSender(cloned[i].TriggerSender)
		cloned[i].TriggerPayload = cloneRawMessage(cloned[i].TriggerPayload)
		if cloned[i].CompletedAt != nil {
			value := *cloned[i].CompletedAt
			cloned[i].CompletedAt = &value
		}
		if cloned[i].EndedAt != nil {
			value := *cloned[i].EndedAt
			cloned[i].EndedAt = &value
		}
		cloned[i].Items = append([]TurnItem(nil), cloned[i].Items...)
		for itemIndex := range cloned[i].Items {
			cloned[i].Items[itemIndex].Sender = cloneMessageSender(cloned[i].Items[itemIndex].Sender)
			cloned[i].Items[itemIndex].Payload = cloneRawMessage(cloned[i].Items[itemIndex].Payload)
			cloned[i].Items[itemIndex].Data = append(json.RawMessage(nil), cloned[i].Items[itemIndex].Data...)
		}
	}
	return cloned
}

// HasMessageID reports whether a canonical input with the stable message ID
// is already durable. It supports safe delivery retry after a lost response.
func (s *Store) HasMessageID(id, messageID string) (bool, error) {
	_, found, err := s.MessageByID(id, messageID)
	return found, err
}

// DurableMessageByID returns both the canonical input and its provider
// delivery state. A durable input without an accepted delivery event remains
// pending and must not be acknowledged as already delivered.
func (s *Store) DurableMessageByID(id, messageID string) (DurableMessage, bool, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return DurableMessage{}, false, nil
	}
	messages, err := s.DurableMessages(id)
	if err != nil {
		return DurableMessage{}, false, err
	}
	for _, message := range messages {
		if message.Input.MessageID == messageID {
			return message, true, nil
		}
	}
	return DurableMessage{}, false, nil
}

// DurableMessages returns canonical inputs in event order with delivery state
// folded from later message.delivery facts.
func (s *Store) DurableMessages(id string) ([]DurableMessage, error) {
	state, err := s.state(id)
	if err != nil {
		return nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := s.ensureEventsLocked(state, id); err != nil {
		return nil, err
	}
	return buildDurableMessages(state.events), nil
}

func buildDurableMessages(events []Event) []DurableMessage {
	messages := make([]DurableMessage, 0)
	byEventID := make(map[int64]int)
	byTurnID := make(map[string][]int)
	for _, event := range events {
		switch event.Type {
		case EventMessageInput:
			var input MessageInput
			if json.Unmarshal(event.Data, &input) != nil {
				continue
			}
			byEventID[event.ID] = len(messages)
			byTurnID[event.TurnID] = append(byTurnID[event.TurnID], len(messages))
			messages = append(messages, DurableMessage{EventID: event.ID, TurnID: event.TurnID, Input: input})
		case EventMessageDelivery:
			var delivery MessageDeliveryEventData
			if json.Unmarshal(event.Data, &delivery) != nil {
				continue
			}
			index, ok := byEventID[delivery.MessageEventID]
			if !ok {
				continue
			}
			if messages[index].Terminal && !(messages[index].State == MessageDeliveryClosed && delivery.State == MessageDeliveryAccepted) {
				continue
			}
			if delivery.Attempt > messages[index].Attempt {
				messages[index].Attempt = delivery.Attempt
			}
			if delivery.State == MessageDeliveryAccepted {
				messages[index].Delivered = true
				messages[index].Terminal = true
			}
			messages[index].State = delivery.State
			messages[index].Error = delivery.Error
			messages[index].StartupAttempts = delivery.StartupAttempts
			messages[index].RetryAt = delivery.RetryAt
			if delivery.State == MessageDeliveryFailed || delivery.State == MessageDeliveryCancelled {
				messages[index].Terminal = true
			}
		case EventTurnCompleted, EventTurnFailed, EventTurnCancelled:
			// A closed Turn must never be reopened by replaying an unconfirmed
			// input, including old logs predating delivery terminal events.
			var terminal TurnTerminalEventData
			_ = json.Unmarshal(event.Data, &terminal)
			for _, index := range byTurnID[event.TurnID] {
				if !messages[index].Terminal {
					messages[index].Terminal = true
					messages[index].RetryAt = ""
					messages[index].Error = terminal.Error
					if event.Type == EventTurnCompleted {
						messages[index].State = MessageDeliveryClosed
					} else if event.Type == EventTurnFailed {
						messages[index].State = MessageDeliveryFailed
					} else {
						messages[index].State = MessageDeliveryCancelled
					}
				}
			}
		}
	}
	return messages
}

// MessageByID returns the canonical durable input for a caller-stable ID.
func (s *Store) MessageByID(id, messageID string) (MessageInput, bool, error) {
	message, found, err := s.DurableMessageByID(id, messageID)
	return message.Input, found, err
}
