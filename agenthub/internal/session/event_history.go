package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const (
	eventHistorySessionBudget = 4 << 20
	eventHistoryStoreBudget   = 32 << 20
	eventIndexStride          = 128
	eventRecordLimit          = 8 << 20
)

type eventLogIndex struct {
	offsets        []int64
	messageOffsets []int64
	turns          map[string]eventByteRange
	count          int64
	size           int64
	file           os.FileInfo
}

type eventByteRange struct {
	start int64
	end   int64
}

func (index *eventLogIndex) append(event Event, offset, length int64) {
	if (event.ID-1)%eventIndexStride == 0 {
		index.offsets = append(index.offsets, offset)
	}
	switch event.Type {
	case EventMessageInput, EventMessageDelivery, EventTurnCompleted, EventTurnFailed, EventTurnCancelled:
		index.messageOffsets = append(index.messageOffsets, offset)
	}
	if event.TurnID != "" {
		if index.turns == nil {
			index.turns = make(map[string]eventByteRange)
		}
		location, exists := index.turns[event.TurnID]
		if !exists {
			location.start = offset
		}
		location.end = offset + length
		index.turns[event.TurnID] = location
	}
	index.count = event.ID
	index.size = offset + length
}

func (index *eventLogIndex) merge(event Event, delta int64) {
	index.size += delta
	if location, exists := index.turns[event.TurnID]; exists {
		location.end += delta
		index.turns[event.TurnID] = location
	}
}

func estimateEventCacheBytes(event Event) int64 {
	return int64(256 + cap(event.Data) + len(event.Type) + len(event.SessionID) + len(event.TurnID))
}

func (s *Store) setEventsLocked(state *sessionState, events []Event, loaded bool) {
	s.metrics.cachedEventPayloadBytes.Add(-state.eventPayloadBytes)
	s.metrics.cachedEventBytes.Add(-state.eventCacheBytes)
	state.events = events
	state.eventsLoaded = loaded
	state.eventPayloadBytes = 0
	state.eventCacheBytes = 0
	for _, event := range events {
		state.eventPayloadBytes += int64(len(event.Data))
		state.eventCacheBytes += estimateEventCacheBytes(event)
	}
	s.metrics.cachedEventPayloadBytes.Add(state.eventPayloadBytes)
	s.metrics.cachedEventBytes.Add(state.eventCacheBytes)
}

func (s *Store) trimEventsLocked(state *sessionState) {
	if len(state.events) == 0 {
		return
	}
	if state.eventsLoaded && state.eventCacheBytes <= eventHistorySessionBudget &&
		s.metrics.cachedEventBytes.Load() <= eventHistoryStoreBudget &&
		state.session.State != StateStopped && state.session.State != StateArchived {
		return
	}
	last := state.events[len(state.events)-1]
	s.setEventsLocked(state, []Event{last}, false)
}

func (s *Store) ensureAppendTailLocked(state *sessionState, id string) error {
	if state.eventIndex != nil {
		if err := s.ensureEventIndexLocked(state, id); err != nil {
			return err
		}
	}
	if len(state.events) > 0 || state.session.LastEventID == 0 {
		return nil
	}
	last, ok := readLastEventRecord(s.eventsPathFor(state, id), snapshotTailWindow)
	if !ok {
		last, ok = readLastEventRecord(s.eventsPathFor(state, id), eventRecordLimit)
	}
	if !ok || last.ID != state.session.LastEventID || last.SessionID != id {
		return s.ensureEventsLocked(state, id)
	}
	s.setEventsLocked(state, []Event{last}, false)
	return nil
}

func newEventScanner(reader io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), eventRecordLimit)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			return newline + 1, data[:newline], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	return scanner
}

func repairEventTail(file *os.File) error {
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return err
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], info.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	position := info.Size()
	buffer := make([]byte, 64<<10)
	for position > 0 {
		length := min(position, int64(len(buffer)))
		position -= length
		if _, err := file.ReadAt(buffer[:length], position); err != nil {
			return err
		}
		if newline := bytes.LastIndexByte(buffer[:length], '\n'); newline >= 0 {
			return os.Truncate(file.Name(), position+int64(newline)+1)
		}
	}
	return os.Truncate(file.Name(), 0)
}

func walkEventLog(path string, visit func(Event, int64, int64) error) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	if err := repairEventTail(file); err != nil {
		return fmt.Errorf("repair event log tail: %w", err)
	}
	scanner := newEventScanner(file)
	var previousID, offset int64
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return fmt.Errorf("invalid event log record: %w", err)
		}
		if event.ID != previousID+1 {
			return fmt.Errorf("non-contiguous event id: got %d after %d", event.ID, previousID)
		}
		previousID = event.ID
		length := int64(len(scanner.Bytes()) + 1)
		if err := visit(event, offset, length); err != nil {
			return err
		}
		offset += length
	}
	return scanner.Err()
}

func (s *Store) ensureEventIndexLocked(state *sessionState, id string) error {
	info, err := os.Stat(s.eventsPathFor(state, id))
	if err != nil {
		return err
	}
	if state.eventIndex != nil && !sameProjectionFile(state.eventIndex.file, info) {
		state.eventIndex = nil
	}
	if state.eventIndex == nil {
		started := time.Now()
		index := &eventLogIndex{}
		err := walkEventLog(s.eventsPathFor(state, id), func(event Event, offset, length int64) error {
			index.append(event, offset, length)
			return nil
		})
		s.metrics.eventIndexBuildNanos.Add(time.Since(started).Nanoseconds())
		if err != nil {
			return err
		}
		if index.count != state.session.LastEventID {
			return fmt.Errorf("event log cursor %d differs from session cursor %d", index.count, state.session.LastEventID)
		}
		state.eventIndex = index
		state.eventIndex.file, _ = os.Stat(s.eventsPathFor(state, id))
		s.metrics.eventIndexBuilds.Add(1)
	}
	return nil
}

func (s *Store) historyPageLocked(state *sessionState, id string, after, highWater int64, limit int) ([]Event, error) {
	if err := s.ensureEventIndexLocked(state, id); err != nil {
		return nil, err
	}
	index := state.eventIndex
	after = max(after, 0)
	highWater = min(highWater, index.count)
	events := make([]Event, 0, min(int64(limit), max(highWater-after, 0)))
	if after >= highWater {
		return events, nil
	}
	slot := after / eventIndexStride
	file, err := os.Open(s.eventsPathFor(state, id))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if _, err := file.Seek(index.offsets[slot], io.SeekStart); err != nil {
		return nil, err
	}
	scanner := newEventScanner(file)
	previousID := slot * eventIndexStride
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("invalid event log record: %w", err)
		}
		if event.ID != previousID+1 {
			return nil, fmt.Errorf("non-contiguous event id: got %d after %d", event.ID, previousID)
		}
		previousID = event.ID
		if event.ID > highWater {
			break
		}
		if event.ID > after {
			events = append(events, event)
			if len(events) == limit {
				break
			}
		}
	}
	return events, scanner.Err()
}

func (s *Store) scanMessageEventsLocked(state *sessionState, id string, visit func(Event)) error {
	if state.eventsLoaded {
		for _, event := range state.events {
			visit(event)
		}
		return nil
	}
	if err := s.ensureEventIndexLocked(state, id); err != nil {
		return err
	}
	file, err := os.Open(s.eventsPathFor(state, id))
	if err != nil {
		return err
	}
	defer file.Close()
	buffer := make([]byte, 64<<10)
	for _, offset := range state.eventIndex.messageOffsets {
		scanner := bufio.NewScanner(io.NewSectionReader(file, offset, state.eventIndex.size-offset))
		scanner.Buffer(buffer, eventRecordLimit)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return err
			}
			return io.ErrUnexpectedEOF
		}
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return err
		}
		visit(event)
	}
	return nil
}

func (s *Store) buildStoredTurnsLocked(state *sessionState, id, turnID string) ([]TurnSummary, error) {
	started := time.Now()
	defer func() {
		s.metrics.turnBuilds.Add(1)
		s.metrics.turnBuildNanos.Add(time.Since(started).Nanoseconds())
	}()
	return buildTurnSummariesFrom(func(visit func(Event)) error {
		if turnID != "" && state.turnOffsets != nil {
			return s.scanIndexedTurnLocked(state, id, turnID, visit)
		}
		if state.eventsLoaded {
			for _, event := range state.events {
				if turnID == "" || event.TurnID == turnID {
					visit(event)
				}
			}
			return nil
		}
		if turnID == "" {
			return walkEventLog(s.eventsPathFor(state, id), func(event Event, _, _ int64) error {
				visit(event)
				return nil
			})
		}
		if err := s.ensureEventIndexLocked(state, id); err != nil {
			return err
		}
		location, exists := state.eventIndex.turns[turnID]
		if !exists {
			return nil
		}
		file, err := os.Open(s.eventsPathFor(state, id))
		if err != nil {
			return err
		}
		defer file.Close()
		scanner := newEventScanner(io.NewSectionReader(file, location.start, location.end-location.start))
		for scanner.Scan() {
			var event Event
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				return err
			}
			if event.TurnID == turnID {
				visit(event)
			}
		}
		return scanner.Err()
	})
}

func (s *Store) useHistoryPagesLocked(state *sessionState, id string) (bool, error) {
	if state.eventsLoaded {
		return false, nil
	}
	if state.eventIndex != nil || len(state.events) > 0 {
		return true, nil
	}
	info, err := os.Stat(s.eventsPathFor(state, id))
	if err != nil {
		return false, err
	}
	if info.Size() > eventHistorySessionBudget {
		return true, nil
	}
	return false, s.ensureEventsLocked(state, id)
}
