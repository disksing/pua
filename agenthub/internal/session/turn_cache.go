package session

import (
	"container/list"
	"os"
	"sync"
)

const (
	turnCacheBudget     = 8 << 20
	turnCacheMaxEntries = 64
)

type turnCacheEntry struct {
	id       string
	revision uint64
	file     os.FileInfo
	turns    []TurnSummary
	bytes    int64
}

// turnCache bounds retained projection estimates across the whole Store, not
// per session. Oversized results bypass admission; their persisted revision
// still prevents repeated rebuilds. It never acquires a session or Store lock.
type turnCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   list.List
	bytes   int64
}

func sameProjectionFile(left, right os.FileInfo) bool {
	return left != nil && right != nil && left.Size() == right.Size() &&
		left.ModTime().Equal(right.ModTime()) && os.SameFile(left, right)
}

func (cache *turnCache) get(id string, revision uint64, file os.FileInfo) ([]TurnSummary, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry := cache.entries[id]
	if entry == nil {
		return nil, false
	}
	value := entry.Value.(*turnCacheEntry)
	if value.revision != revision || !sameProjectionFile(value.file, file) {
		cache.removeLocked(entry)
		return nil, false
	}
	cache.order.MoveToFront(entry)
	return value.turns, true
}

func (cache *turnCache) put(id string, revision uint64, file os.FileInfo, turns []TurnSummary) {
	bytes := estimateTurnCacheBytes(turns)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if entry := cache.entries[id]; entry != nil {
		cache.removeLocked(entry)
	}
	if file == nil || bytes > turnCacheBudget {
		return
	}
	if cache.entries == nil {
		cache.entries = make(map[string]*list.Element)
	}
	for cache.bytes+bytes > turnCacheBudget || cache.order.Len() >= turnCacheMaxEntries {
		cache.removeLocked(cache.order.Back())
	}
	value := &turnCacheEntry{id: id, revision: revision, file: file, turns: turns, bytes: bytes}
	cache.entries[id] = cache.order.PushFront(value)
	cache.bytes += bytes
}

func (cache *turnCache) remove(id string) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if entry := cache.entries[id]; entry != nil {
		cache.removeLocked(entry)
	}
}

func (cache *turnCache) removeLocked(entry *list.Element) {
	value := entry.Value.(*turnCacheEntry)
	delete(cache.entries, value.id)
	cache.bytes -= value.bytes
	cache.order.Remove(entry)
}

func (cache *turnCache) size() (int, int64) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.order.Len(), cache.bytes
}

// Estimates deliberately overcount shared strings and include backing slice
// capacity, raw payload capacity, and per-record overhead. This is an admission
// budget, not a measurement or hard limit on the process's Go heap/RSS.
func estimateTurnCacheBytes(turns []TurnSummary) int64 {
	bytes := int64(256 + cap(turns)*512)
	for _, turn := range turns {
		bytes += int64(len(turn.ID) + len(turn.TurnID) + len(turn.Status) + len(turn.TriggerPreview) +
			len(turn.TriggerRole) + len(turn.TriggerMessageID) + len(turn.FinalReplyPreview) + cap(turn.TriggerPayload))
		bytes += estimateSenderBytes(turn.TriggerSender)
		bytes += int64(cap(turn.Items) * 384)
		for _, item := range turn.Items {
			bytes += int64(len(item.Type) + len(item.Role) + len(item.Text) + len(item.MessageID) +
				len(item.activityTail) + cap(item.Payload) + cap(item.Data))
			bytes += estimateSenderBytes(item.Sender)
		}
	}
	return bytes
}

func estimateSenderBytes(sender *MessageSender) int64 {
	if sender == nil {
		return 0
	}
	return int64(64 + len(sender.ID) + len(sender.Name) + len(sender.SessionID))
}
