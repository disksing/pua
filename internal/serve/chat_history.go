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
	key      chatTurnKey
	messages []chatMessage
}

type chatHistoryCache struct {
	mu      sync.Mutex
	entries map[chatTurnKey]*list.Element
	recent  list.List
}

func (cache *chatHistoryCache) get(key chatTurnKey) ([]chatMessage, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry := cache.entries[key]
	if entry == nil {
		return nil, false
	}
	cache.recent.MoveToFront(entry)
	return entry.Value.(chatHistoryEntry).messages, true
}

func (cache *chatHistoryCache) put(key chatTurnKey, messages []chatMessage) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.entries == nil {
		cache.entries = make(map[chatTurnKey]*list.Element)
	}
	if cache.entries[key] != nil {
		return
	}
	cache.entries[key] = cache.recent.PushFront(chatHistoryEntry{key, messages})
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
			CreatedAt: firstNonEmpty(item.EndedAt, item.StartedAt, turn.CompletedAt, work.CreatedAt),
		})
	}
	return messages
}

func (m *agentManager) chatHistory(ctx context.Context, root string, chat resourceChat) (resourceChat, error) {
	return m.chatHistoryWindow(ctx, root, chat, 0, "")
}

func (m *agentManager) chatHistoryWindow(ctx context.Context, root string, chat resourceChat, limit int, user string) (resourceChat, error) {
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
			when := generationTime(message.CreatedAt)
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
	projected := make([][]chatMessage, len(file.Works))
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
		if messages, ok := m.chatHistoryCache.get(key); ok {
			if archived {
				messages = messages[:min(work.ChatTextCount, len(messages))]
			}
			projected[index] = messages
			remember(messages)
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
		messages := historyChatTexts(work, turn, work.ChatSender)
		if turn.Closed && work.OutputDone {
			m.chatHistoryCache.put(key, messages)
		}
		if archived {
			messages = messages[:min(work.ChatTextCount, len(messages))]
		}
		projected[index] = messages
		remember(messages)
	}
	for _, messages := range projected {
		view.Messages = append(view.Messages, messages...)
	}
	if len(view.Messages) != len(chat.Messages) {
		sort.SliceStable(view.Messages, func(left, right int) bool {
			return generationTime(view.Messages[left].CreatedAt).Before(generationTime(view.Messages[right].CreatedAt))
		})
	}
	return view, nil
}

func (m *agentManager) historyPublicPage(ctx context.Context, root string, chat resourceChat, limit int, before, after, user string) (publicChat, error) {
	window := limit
	if before != "" {
		window = 0
	}
	view, err := m.chatHistoryWindow(ctx, root, chat, window, user)
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
			view, err = m.chatHistory(ctx, root, chat)
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
