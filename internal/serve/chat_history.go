package serve

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type chatTurnKey struct{ root, resource, session, turn, work string }

type chatHistoryEntry struct {
	key  chatTurnKey
	view chatTurnProjection
}

type chatTurnProjection struct {
	messages []chatMessage
	previews map[string]string
	retry    bool
}

type chatHistoryCache struct {
	mu      sync.Mutex
	entries map[chatTurnKey]*list.Element
	recent  list.List
}

func (cache *chatHistoryCache) get(key chatTurnKey) (chatTurnProjection, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry := cache.entries[key]
	if entry == nil {
		return chatTurnProjection{}, false
	}
	cache.recent.MoveToFront(entry)
	return entry.Value.(chatHistoryEntry).view, true
}

func (cache *chatHistoryCache) put(key chatTurnKey, view chatTurnProjection) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.entries == nil {
		cache.entries = make(map[chatTurnKey]*list.Element)
	}
	if cache.entries[key] != nil {
		return
	}
	cache.entries[key] = cache.recent.PushFront(chatHistoryEntry{key, view})
	if cache.recent.Len() > 512 {
		last := cache.recent.Back()
		delete(cache.entries, last.Value.(chatHistoryEntry).key)
		cache.recent.Remove(last)
	}
}

func historyChatTexts(work turnWork, turn agentHubTurn, sender string) []chatMessage {
	var messages []chatMessage
	for index, item := range turn.Items {
		if item.Type != "message" || item.Role != "assistant" || strings.TrimSpace(item.Text) == "" ||
			(!item.Complete && !turn.Closed && index == len(turn.Items)-1) {
			continue
		}
		messages = append(messages, chatMessage{
			ID:   "auto-" + chatID(work.SessionID, fmt.Sprintf("%s:%d", work.TurnID, item.StartEventID)),
			Role: "agent", Text: item.Text, Sender: sender, workID: work.ID, workEpoch: work.Context.ChatEpoch, workOffset: int64(len(messages) + 1),
			turnID: work.TurnID, startEventID: item.StartEventID,
			CreatedAt: firstNonEmpty(item.EndedAt, item.StartedAt, turn.CompletedAt, work.CreatedAt),
		})
	}
	return messages
}

func projectChatTurn(work turnWork, turn agentHubTurn, streamContinues func(int64, int64) (bool, error)) chatTurnProjection {
	view := chatTurnProjection{messages: historyChatTexts(work, turn, work.ChatSender)}
	if len(view.messages) == 0 {
		return view
	}
	var first chatMessage
	var text strings.Builder
	var unfinished bool
	var previewStart, previewCount int64
	next := 0
	for _, item := range turn.Items {
		if id := chatQueuePreview(item); id != "" {
			if view.previews == nil {
				view.previews = make(map[string]string)
			}
			if _, exists := view.previews[id]; !exists {
				view.previews[id] = firstNonEmpty(item.StartedAt, item.EndedAt)
			}
			if unfinished {
				if previewCount == 0 {
					previewStart = item.StartEventID
				}
				previewCount++
			}
			continue
		}
		if next == len(view.messages) || item.StartEventID != view.messages[next].startEventID {
			if previewCount > 0 && item.Type == "message" && item.Role == "assistant" && !item.Complete {
				text.WriteString(item.Text)
				continue
			}
			unfinished, previewCount = false, 0
			continue
		}
		message := &view.messages[next]
		next++
		continuing := previewCount > 0
		if continuing && item.StartEventID > previewStart+previewCount {
			continuing = false
			if streamContinues != nil {
				var err error
				continuing, err = streamContinues(previewStart, item.StartEventID)
				view.retry = view.retry || err != nil
			}
		}
		if continuing {
			text.WriteString(message.Text)
			message.timelineAt = first.CreatedAt
			message.continuation = &chatContinuation{ID: first.ID, Text: text.String()}
		} else {
			first = *message
			text.Reset()
			text.WriteString(message.Text)
		}
		unfinished, previewCount = !item.Complete, 0
	}
	return view
}

func chatStreamContinues(ctx context.Context, client *agentHubClient, work turnWork, after, before int64) (bool, error) {
	for after+1 < before {
		frames, _, err := client.SessionFrames(ctx, work.SessionID, after, int(min(100, before-after-1)))
		if err != nil {
			return false, err
		}
		if len(frames) == 0 {
			return false, nil
		}
		for _, frame := range frames {
			if frame.Cursor != after+1 || frame.Cursor >= before {
				return false, nil
			}
			if frame.Source.Type == "message.assistant.completed" && (frame.Source.TurnID == "" || frame.Source.TurnID == work.TurnID) {
				return false, nil
			}
			after = frame.Cursor
		}
	}
	return true, nil
}

func chatQueuePreview(item agentHubTurnItem) string {
	if item.Type != "message" || !item.Steer {
		return ""
	}
	payload, ok := decodePUAMessagePayload(item.Payload)
	if !ok || payload.Role != "system" || payload.Type != "queue_preview" ||
		payload.Causation == nil || payload.Causation.Type != "queue_preview" {
		return ""
	}
	return payload.Causation.MessageID
}

func (m *agentManager) chatHistory(ctx context.Context, root string, chat resourceChat) (resourceChat, error) {
	return m.chatHistoryWindow(ctx, root, chat, 0, "", false)
}

func chatMessageTime(message chatMessage, timeline bool) time.Time {
	if timeline && message.timelineAt != "" {
		return generationTime(message.timelineAt)
	}
	return generationTime(message.CreatedAt)
}

func (m *agentManager) chatHistoryWindow(ctx context.Context, root string, chat resourceChat, limit int, user string, timeline bool) (resourceChat, error) {
	file, err := loadTurnWork(root, chat.ResourceID)
	if err != nil {
		return chat, err
	}
	view := chat
	view.Messages = append([]chatMessage(nil), chat.Messages...)
	for index := range view.Messages {
		view.Messages[index].rawPosition = index + 1
	}
	archived := false
	if len(file.Works) > 0 {
		_, archived, _, err = resourceExistsAndArchived(root, chat.ResourceID)
		if err != nil {
			return chat, err
		}
	}
	var client *agentHubClient
	var newest []time.Time
	remember := func(messages []chatMessage) {
		if limit == 0 {
			return
		}
		for _, message := range messages {
			when := chatMessageTime(message, timeline)
			index := sort.Search(len(newest), func(i int) bool { return !newest[i].After(when) })
			if index == limit {
				continue
			}
			newest = append(newest, time.Time{})
			copy(newest[index+1:], newest[index:])
			newest[index] = when
			newest = newest[:min(limit, len(newest))]
		}
	}
	remember(view.Messages)
	projected := make([]chatTurnProjection, len(file.Works))
	for index := len(file.Works) - 1; index >= 0; index-- {
		work := file.Works[index]
		if !work.HistoryChat || !work.Context.ChatVisible || work.SessionID == "" || (archived && work.ChatTextCount == 0) {
			continue
		}
		if limit > 0 && len(newest) == limit && work.OutputDone && work.ChatTextCount > 0 &&
			work.LastPublishedAt != "" && generationTime(work.LastPublishedAt).Before(newest[limit-1]) {
			view.omitted += work.ChatTextCount
			view.omittedUnread += max(0, work.ChatTextCount-int(chat.HistoryReads[user][work.ID]))
			continue
		}
		key := chatTurnKey{root, chat.ResourceID, work.SessionID, work.TurnID, work.ID}
		if projection, ok := m.chatHistoryCache.get(key); ok {
			if archived {
				projection.messages = projection.messages[:min(work.ChatTextCount, len(projection.messages))]
			}
			projected[index] = projection
			remember(projection.messages)
			continue
		}
		if client == nil {
			_, client, err = m.agentHubRuntimeConfig()
			if err != nil {
				return chat, err
			}
		}
		if work.TurnID == "" {
			work.TurnID, err = findMailboxOpenerTurn(ctx, client, work.SessionID, work.ID)
			if err != nil {
				return chat, err
			}
			if work.TurnID == "" {
				continue
			}
			key.turn = work.TurnID
		}
		turn, _, err := client.SessionTurn(ctx, work.SessionID, work.TurnID)
		if err != nil {
			return chat, err
		}
		if firstNonEmpty(turn.TurnID, turn.ID) != work.TurnID || (turn.TriggerMessageID != "" && turn.TriggerMessageID != work.ID) {
			return chat, errors.New("Chat Turn does not match its work")
		}
		projection := projectChatTurn(work, turn, func(after, before int64) (bool, error) {
			return chatStreamContinues(ctx, client, work, after, before)
		})
		if turn.Closed && work.OutputDone && !projection.retry {
			m.chatHistoryCache.put(key, projection)
		}
		if archived {
			projection.messages = projection.messages[:min(work.ChatTextCount, len(projection.messages))]
		}
		projected[index] = projection
		remember(projection.messages)
	}
	if timeline {
		view.placePreviewedInputs(file.Works, projected)
	}
	for _, projection := range projected {
		view.Messages = append(view.Messages, projection.messages...)
	}
	if len(view.Messages) != len(chat.Messages) {
		sort.SliceStable(view.Messages, func(left, right int) bool {
			first, second := view.Messages[left], view.Messages[right]
			firstTime, secondTime := chatMessageTime(first, timeline), chatMessageTime(second, timeline)
			if timeline && firstTime.Equal(secondTime) {
				return !(first.Role == "user" && first.timelineAt != "") && second.Role == "user" && second.timelineAt != ""
			}
			return firstTime.Before(secondTime)
		})
	}
	return view, nil
}

func (view *resourceChat) placePreviewedInputs(works []turnWork, projected []chatTurnProjection) {
	previews := make(map[string]string)
	for index, projection := range projected {
		if len(projection.messages) == 0 || len(projection.previews) == 0 {
			continue
		}
		latest := projection.messages[len(projection.messages)-1].CreatedAt
		for id, at := range projection.previews {
			if generationTime(at).After(generationTime(latest)) {
				at = latest
			}
			if _, exists := previews[id]; !exists {
				previews[id] = at
			}
		}
		for id, parent := range works[index].GroupedPreviews {
			if at, ok := previews[parent]; ok {
				previews[id] = at
			}
		}
	}
	if len(previews) == 0 {
		return
	}
	view.timelineInputs = make(map[string]string)
	for index := range view.Messages {
		message := &view.Messages[index]
		for _, id := range view.executionInputIDs(*message) {
			at := previews[id]
			if generationTime(at).IsZero() {
				continue
			}
			if generationTime(at).Before(generationTime(message.CreatedAt)) {
				at = message.CreatedAt
			}
			message.timelineAt = at
			view.timelineInputs[message.ID] = at
			break
		}
	}
}

func (m *agentManager) historyPublicPage(ctx context.Context, root string, chat resourceChat, limit int, before, after, user string) (publicChat, error) {
	window := limit
	if before != "" {
		window = 0
	}
	view, err := m.chatHistoryWindow(ctx, root, chat, window, user, true)
	if err != nil {
		return publicChat{}, err
	}
	if view.omitted > 0 && after != "" {
		start := max(0, len(view.Messages)-limit)
		found := false
		for _, message := range view.Messages[start:] {
			found = found || message.ID == after
		}
		if !found {
			view, err = m.chatHistoryWindow(ctx, root, chat, 0, user, true)
			if err != nil {
				return publicChat{}, err
			}
		}
	}
	return view.publicPage(limit, before, after, user)
}

func chatHistoryActivity(root string, chat resourceChat, user string) (unread int, latest string, err error) {
	file, err := loadTurnWork(root, chat.ResourceID)
	if err != nil {
		return 0, "", err
	}
	for _, work := range file.Works {
		if work.HistoryChat && work.Context.ChatVisible {
			unread += max(0, work.ChatTextCount-int(chat.HistoryReads[user][work.ID]))
			if generationTime(work.LastPublishedAt).After(generationTime(latest)) {
				latest = work.LastPublishedAt
			}
		}
	}
	return unread, latest, nil
}
