package session

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"syscall"
)

const turnOffsetVersion = 1

type turnDiskRecord struct {
	TurnSummary
	Range      *turnDiskRange  `json:"rawRange,omitempty"`
	Checkpoint *turnCheckpoint `json:"rawCheckpoint,omitempty"`
}

type turnDiskRange struct {
	Start int64  `json:"startOffset"`
	End   *int64 `json:"endOffset,omitempty"`
}

type turnCheckpoint struct {
	Version    int      `json:"version"`
	Device     uint64   `json:"device"`
	Inode      uint64   `json:"inode"`
	Modified   int64    `json:"modified"`
	Size       int64    `json:"size"`
	TailOffset int64    `json:"tailOffset"`
	LastID     int64    `json:"lastEventId"`
	Prefix     [32]byte `json:"prefix"`
	Tail       [32]byte `json:"tail"`
}

type turnLocation struct {
	first, last int64
	start, end  int64
	closed      bool
}

type turnOffsetIndex struct {
	locations map[string]turnLocation
	dirty     map[string]struct{}
	source    os.FileInfo
	size      int64
	lastID    int64
	tail      int64
	pageID    int64
	pageStart int64
}

func newTurnOffsetIndex() *turnOffsetIndex {
	return &turnOffsetIndex{locations: make(map[string]turnLocation), dirty: make(map[string]struct{})}
}

func (index *turnOffsetIndex) record(event Event, start, end int64) {
	if index.pageStart >= start {
		index.pageID, index.pageStart = 0, 0
	}
	if event.TurnID != "" {
		location, exists := index.locations[event.TurnID]
		if !exists {
			location.first, location.start = event.ID, start
		}
		switch event.Type {
		case "turn.started":
			location.closed = false
		case EventTurnCompleted, EventTurnFailed, EventTurnCancelled:
			location.closed = true
		}
		location.last, location.end = event.ID, end
		index.locations[event.TurnID] = location
		index.dirty[event.TurnID] = struct{}{}
	}
	index.lastID, index.tail, index.size = event.ID, start, end
}

func (index *turnOffsetIndex) diskRecord(summary TurnSummary) turnDiskRecord {
	location := index.locations[summary.ID]
	rawRange := &turnDiskRange{Start: location.start}
	if summary.Closed {
		rawRange.End = &location.end
	}
	return turnDiskRecord{TurnSummary: summary, Range: rawRange}
}

func turnFileIdentity(info os.FileInfo) (uint64, uint64) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Dev), uint64(stat.Ino)
	}
	return 0, 0
}

func turnPrefixDigest(reader io.ReaderAt, end int64) ([32]byte, error) {
	const sampleSize = 512
	head := min(end, sampleSize)
	tail := min(end-head, sampleSize)
	buffer := make([]byte, head+tail)
	if head > 0 {
		if _, err := reader.ReadAt(buffer[:head], 0); err != nil {
			return [32]byte{}, err
		}
	}
	if tail > 0 {
		if _, err := reader.ReadAt(buffer[head:], end-tail); err != nil {
			return [32]byte{}, err
		}
	}
	return sha256.Sum256(buffer), nil
}

func turnTailDigest(reader io.ReaderAt, start, end int64) ([32]byte, error) {
	hash := sha256.New()
	copied, err := io.Copy(hash, io.NewSectionReader(reader, start, end-start))
	if err != nil || copied != end-start {
		return [32]byte{}, io.ErrUnexpectedEOF
	}
	return [32]byte(hash.Sum(nil)), nil
}

func (index *turnOffsetIndex) checkpoint(path string) (*turnCheckpoint, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !sameProjectionFile(index.source, info) {
		return nil, errors.New("event log changed before writing Turn projection")
	}
	device, inode := turnFileIdentity(info)
	prefix, err := turnPrefixDigest(file, index.tail)
	if err != nil {
		return nil, err
	}
	tail, err := turnTailDigest(file, index.tail, index.size)
	if err != nil {
		return nil, err
	}
	return &turnCheckpoint{
		Version: turnOffsetVersion, Device: device, Inode: inode, Modified: info.ModTime().UnixNano(),
		Size: index.size, TailOffset: index.tail, LastID: index.lastID, Prefix: prefix, Tail: tail,
	}, nil
}

func scanTurnEventBytes(file *os.File, id string, start, end, firstID int64, visit func(Event, int64, int64) error) error {
	if end < start || start < 0 {
		return errors.New("invalid event byte range")
	}
	if end > start {
		var last [1]byte
		if _, err := file.ReadAt(last[:], end-1); err != nil || last[0] != '\n' {
			return io.ErrUnexpectedEOF
		}
	}
	scanner := newEventScanner(io.NewSectionReader(file, start, end-start))
	position, nextID := start, firstID
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return err
		}
		if event.ID != nextID || event.SessionID != id {
			return fmt.Errorf("invalid event history at %d: expected %s event %d", position, id, nextID)
		}
		next := position + int64(len(scanner.Bytes())) + 1
		if err := visit(event, position, next); err != nil {
			return err
		}
		position, nextID = next, nextID+1
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if position != end {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func restoreTurnOffsets(path, id string, lastID int64, records []turnDiskRecord, stamp *turnCheckpoint) (*turnOffsetIndex, error) {
	if stamp == nil || stamp.Version != turnOffsetVersion || stamp.LastID < 1 || stamp.LastID > lastID || stamp.TailOffset < 0 ||
		stamp.TailOffset >= stamp.Size || stamp.Size-stamp.TailOffset > eventRecordLimit {
		return nil, errors.New("invalid Turn checkpoint")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	device, inode := turnFileIdentity(info)
	if info.Size() < stamp.Size || inode == 0 || device != stamp.Device || inode != stamp.Inode {
		return nil, errors.New("event log was truncated or replaced")
	}
	prefix, err := turnPrefixDigest(file, stamp.TailOffset)
	if err != nil || prefix != stamp.Prefix {
		return nil, errors.New("event log prefix changed")
	}
	index := newTurnOffsetIndex()
	index.source, index.size, index.tail, index.lastID = info, stamp.Size, stamp.TailOffset, stamp.LastID
	var previousTailTurn string
	for _, record := range records {
		if record.Range == nil || record.FirstEventID < 1 || record.FirstEventID > record.LastEventID ||
			record.LastEventID > stamp.LastID || record.Range.Start < 0 || record.Range.Start >= stamp.Size ||
			record.Closed != (record.Range.End != nil) {
			return nil, errors.New("invalid Turn range")
		}
		location := turnLocation{first: record.FirstEventID, last: record.LastEventID, start: record.Range.Start, closed: record.Closed}
		if record.Range.End != nil {
			location.end = *record.Range.End
			if location.end <= location.start || location.end > stamp.Size {
				return nil, errors.New("invalid closed Turn range")
			}
		}
		if record.LastEventID == stamp.LastID {
			previousTailTurn = record.ID
		}
		index.locations[record.ID] = location
	}
	tail, tailErr := turnTailDigest(file, stamp.TailOffset, stamp.Size)
	if tailErr == nil && tail == stamp.Tail && lastID == stamp.LastID &&
		info.Size() == stamp.Size && info.ModTime().UnixNano() == stamp.Modified {
		return index, nil
	}
	if err := scanTurnEventBytes(file, id, stamp.TailOffset, info.Size(), stamp.LastID, func(event Event, start, end int64) error {
		if event.ID == stamp.LastID {
			if event.TurnID != previousTailTurn {
				return errors.New("event log checkpoint no longer matches its Turn")
			}
			if tailErr == nil && tail == stamp.Tail && end == stamp.Size {
				return nil
			}
		}
		index.record(event, start, end)
		return nil
	}); err != nil {
		return nil, err
	}
	if index.lastID != lastID {
		return nil, errors.New("event log cursor differs from session cursor")
	}
	return index, nil
}

func (s *Store) prepareTurnOffsetsLocked(state *sessionState, id string, load bool) {
	if state.turnOffsets == nil && !state.turnOffsetsChecked && load {
		path := s.turnsPathFor(state, id)
		records, stamp, err := readTurnDiskRecords(path)
		if err == nil && stamp != nil {
			state.turnOffsets, _ = restoreTurnOffsets(s.eventsPathFor(state, id), id, state.session.LastEventID, records, stamp)
			state.turnOffsetsInvalid = state.turnOffsets == nil
		} else if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			state.turnOffsetsInvalid = true
		}
		state.turnOffsetsChecked = true
		state.turnOffsetFile, _ = os.Stat(path)
	}
	if index := state.turnOffsets; index != nil && index.source != nil {
		info, _ := os.Stat(s.eventsPathFor(state, id))
		if !sameProjectionFile(index.source, info) {
			state.turnOffsets = nil
			state.turnOffsetsInvalid = true
		}
	}
}

func (s *Store) recordTurnOffsetLocked(state *sessionState, id string, event Event, length int64, merged bool) {
	index := state.turnOffsets
	if index == nil {
		return
	}
	start := index.size
	if merged {
		start = index.tail
	}
	end := start + length
	info, err := os.Stat(s.eventsPathFor(state, id))
	if err != nil || info.Size() != end || (index.source != nil && !os.SameFile(index.source, info)) {
		state.turnOffsets = nil
		state.turnOffsetsInvalid = true
		return
	}
	index.source = info
	index.record(event, start, end)
}

func (s *Store) scanIndexedTurnLocked(state *sessionState, id, turnID string, visit func(Event)) error {
	index := state.turnOffsets
	location, exists := index.locations[turnID]
	if !exists {
		return nil
	}
	file, err := os.Open(s.eventsPathFor(state, id))
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !sameProjectionFile(index.source, info) {
		return errors.New("event log changed before reading Turn range")
	}
	end := location.end
	if !location.closed {
		end = index.size
	}
	var lastID int64
	err = scanTurnEventBytes(file, id, location.start, end, location.first, func(event Event, _, next int64) error {
		if event.TurnID == turnID {
			lastID = event.ID
			visit(event)
		} else if event.ID == location.first || (location.closed && next == end) {
			return errors.New("Turn range boundary does not belong to the Turn")
		}
		return nil
	})
	if err == nil && lastID != location.last {
		return errors.New("Turn range does not match the last event")
	}
	return err
}

func (s *Store) indexedTurnEventPageLocked(state *sessionState, id string, after, highWater int64, limit int) ([]Event, bool) {
	if state.eventIndex != nil || after >= highWater {
		return nil, false
	}
	s.prepareTurnOffsetsLocked(state, id, true)
	index := state.turnOffsets
	if index == nil {
		return nil, false
	}
	var first, start int64
	var anchorTurn string
	for turnID, location := range index.locations {
		if location.first <= after+1 && location.first > first && (!location.closed || location.last >= after+1) {
			first, start, anchorTurn = location.first, location.start, turnID
		}
	}
	if index.pageID == after+1 {
		first, start, anchorTurn = index.pageID, index.pageStart, ""
	}
	if first == 0 {
		return nil, false
	}
	file, err := os.Open(s.eventsPathFor(state, id))
	if err != nil {
		return nil, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !sameProjectionFile(index.source, info) {
		return nil, false
	}
	events := make([]Event, 0, min(int64(limit), highWater-after))
	var nextID, nextOffset int64
	err = scanTurnEventBytes(file, id, start, index.size, first, func(event Event, _, end int64) error {
		if event.ID == first && anchorTurn != "" && event.TurnID != anchorTurn {
			return errors.New("event range anchor does not match its Turn")
		}
		if event.ID > after {
			events = append(events, event)
		}
		if len(events) == limit || event.ID == highWater {
			nextID, nextOffset = event.ID+1, end
			return errEventPageComplete
		}
		return nil
	})
	if !errors.Is(err, errEventPageComplete) {
		return nil, false
	}
	index.pageID, index.pageStart = nextID, nextOffset
	return events, true
}

var errEventPageComplete = errors.New("event page complete")

func (s *Store) persistDirtyTurnsLocked(state *sessionState, id string) ([]turnDiskRecord, error) {
	index := state.turnOffsets
	if len(index.dirty) == 0 {
		return nil, nil
	}
	path := s.turnsPathFor(state, id)
	info, statErr := os.Stat(path)
	if !(state.turnOffsetFile == nil && errors.Is(statErr, os.ErrNotExist)) && !sameProjectionFile(state.turnOffsetFile, info) {
		return nil, errors.New("Turn projection changed before appending")
	}
	dirty := make([]string, 0, len(index.dirty))
	for turnID := range index.dirty {
		dirty = append(dirty, turnID)
	}
	sort.Slice(dirty, func(left, right int) bool {
		return index.locations[dirty[left]].first < index.locations[dirty[right]].first
	})
	records := make([]turnDiskRecord, 0, len(dirty))
	for _, turnID := range dirty {
		summaries, err := s.buildStoredTurnsLocked(state, id, turnID)
		if err != nil {
			return nil, err
		}
		if len(summaries) != 1 {
			return nil, errors.New("indexed Turn is missing from event log")
		}
		records = append(records, index.diskRecord(summaries[0]))
	}
	checkpoint, err := index.checkpoint(s.eventsPathFor(state, id))
	if err != nil {
		return nil, err
	}
	records[len(records)-1].Checkpoint = checkpoint
	var data []byte
	for _, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}
		data = append(data, encoded...)
		data = append(data, '\n')
	}
	if err := appendDurable(path, data); err != nil {
		return nil, err
	}
	clear(index.dirty)
	state.turnOffsetFile, _ = os.Stat(path)
	s.metrics.turnProjectionWrites.Add(1)
	return records, nil
}

func (s *Store) rebuildTurnOffsetsLocked(state *sessionState, id string) ([]turnDiskRecord, error) {
	return s.rebuildTurnOffsets(context.Background(), state, id, false)
}

func (s *Store) rebuildTurnOffsets(ctx context.Context, state *sessionState, id string, backfill bool) ([]turnDiskRecord, error) {
	eventPath := s.eventsPathFor(state, id)
	file, err := os.Open(eventPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	index := newTurnOffsetIndex()
	index.source = info
	path := s.turnsPathFor(state, id)
	projection, _ := os.Stat(path)
	turns, err := buildTurnSummariesFrom(func(visit func(Event)) error {
		return scanTurnEventBytes(file, id, 0, info.Size(), 1, func(event Event, start, end int64) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			index.record(event, start, end)
			visit(event)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if index.lastID != state.session.LastEventID {
		return nil, errors.New("event log cursor differs from session cursor")
	}
	if backfill && len(turns) == 0 && (projection == nil || projection.Size() == 0) {
		return nil, nil
	}
	records := make([]turnDiskRecord, 0, len(turns))
	for _, turn := range turns {
		records = append(records, index.diskRecord(turn))
	}
	if len(records) != 0 {
		checkpoint, err := index.checkpoint(eventPath)
		if err != nil {
			return nil, err
		}
		records[len(records)-1].Checkpoint = checkpoint
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	currentEvents, _ := os.Stat(eventPath)
	currentProjection, statErr := os.Stat(path)
	if !sameProjectionFile(info, currentEvents) ||
		!(projection == nil && errors.Is(statErr, os.ErrNotExist)) && !sameProjectionFile(projection, currentProjection) {
		return nil, errors.New("session files changed during Turn rebuild")
	}
	if err := writeTurnRecordsAtomic(path, records); err != nil {
		return nil, fmt.Errorf("rebuild turn projection: %w", err)
	}
	clear(index.dirty)
	state.turnOffsets, state.turnOffsetsChecked, state.turnOffsetsInvalid = index, true, false
	state.turnOffsetFile, _ = os.Stat(path)
	s.metrics.turnBuilds.Add(1)
	s.metrics.turnRebuilds.Add(1)
	s.metrics.turnProjectionWrites.Add(1)
	return records, nil
}

var ErrTurnBackfillInProgress = errors.New("another Turn backfill is in progress")

type TurnBackfillResult struct {
	SessionID string `json:"sessionId"`
	Turns     int    `json:"turns"`
	Changed   bool   `json:"changed"`
}

func (s *Store) BackfillTurnOffsets(ctx context.Context, id string) (TurnBackfillResult, error) {
	result := TurnBackfillResult{SessionID: id}
	state, err := s.state(id)
	if err != nil {
		return result, err
	}
	if !s.turnBackfillMu.TryLock() {
		return result, ErrTurnBackfillInProgress
	}
	defer s.turnBackfillMu.Unlock()
	state.mu.Lock()
	defer state.mu.Unlock()
	idle := (state.session.State == StateReady || state.session.State == StateStopped) &&
		state.session.CurrentTurnID == "" && len(state.session.PendingApprovalIDs) == 0
	if state.session.State != StateArchived && !idle {
		return result, ErrSessionActive
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	existing, stamp, err := readTurnDiskRecords(s.turnsPathFor(state, id))
	if err == nil && stamp != nil {
		index, restoreErr := restoreTurnOffsets(s.eventsPathFor(state, id), id, state.session.LastEventID, existing, stamp)
		if restoreErr == nil && len(index.dirty) == 0 {
			result.Turns = len(existing)
			return result, nil
		}
	}
	records, err := s.rebuildTurnOffsets(ctx, state, id, true)
	if err != nil {
		return result, err
	}
	s.turnCache.remove(id)
	result.Turns, result.Changed = len(records), records != nil
	return result, nil
}
