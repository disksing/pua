package session

import (
	"sync/atomic"
	"time"
)

type storeMetrics struct {
	openNanos               atomic.Int64
	snapshotCheckNanos      atomic.Int64
	trustedSnapshots        atomic.Uint64
	fallbackReplays         atomic.Uint64
	historyLoads            atomic.Uint64
	historyLoadErrors       atomic.Uint64
	historyLoadNanos        atomic.Int64
	cachedEventPayloadBytes atomic.Int64
	metadataQueries         atomic.Uint64
	metadataQueryNanos      atomic.Int64
	turnCacheHits           atomic.Uint64
	turnProjectionReads     atomic.Uint64
	turnBuilds              atomic.Uint64
	turnBuildNanos          atomic.Int64
	turnRebuilds            atomic.Uint64
	turnProjectionWrites    atomic.Uint64
}

// StoreDiagnostics contains process-local, aggregate counters. Durations are
// cumulative nanoseconds; gauges are estimates, not Go heap or RSS attribution.
// No session IDs, event contents, or environment variables are exposed.
type StoreDiagnostics struct {
	OpenNanos               int64  `json:"openNanos"`
	SnapshotCheckNanos      int64  `json:"snapshotCheckNanos"`
	TrustedSnapshots        uint64 `json:"trustedSnapshots"`
	FallbackReplays         uint64 `json:"fallbackReplays"`
	HistoryLoads            uint64 `json:"historyLoads"`
	HistoryLoadErrors       uint64 `json:"historyLoadErrors"`
	HistoryLoadNanos        int64  `json:"historyLoadNanos"`
	CachedEventPayloadBytes int64  `json:"cachedEventPayloadBytes"`
	MetadataQueries         uint64 `json:"metadataQueries"`
	MetadataQueryNanos      int64  `json:"metadataQueryNanos"`
	TurnCacheHits           uint64 `json:"turnCacheHits"`
	TurnCacheEntries        int    `json:"turnCacheEntries"`
	TurnCacheEstimatedBytes int64  `json:"turnCacheEstimatedBytes"`
	TurnCacheBudgetBytes    int64  `json:"turnCacheBudgetBytes"`
	TurnProjectionReads     uint64 `json:"turnProjectionReads"`
	TurnBuilds              uint64 `json:"turnBuilds"`
	TurnBuildNanos          int64  `json:"turnBuildNanos"`
	TurnRebuilds            uint64 `json:"turnRebuilds"`
	TurnProjectionWrites    uint64 `json:"turnProjectionWrites"`
}

func (s *Store) Diagnostics() StoreDiagnostics {
	entries, bytes := s.turnCache.size()
	return StoreDiagnostics{
		OpenNanos: s.metrics.openNanos.Load(), SnapshotCheckNanos: s.metrics.snapshotCheckNanos.Load(),
		TrustedSnapshots: s.metrics.trustedSnapshots.Load(), FallbackReplays: s.metrics.fallbackReplays.Load(),
		HistoryLoads: s.metrics.historyLoads.Load(), HistoryLoadErrors: s.metrics.historyLoadErrors.Load(),
		HistoryLoadNanos: s.metrics.historyLoadNanos.Load(), CachedEventPayloadBytes: s.metrics.cachedEventPayloadBytes.Load(),
		MetadataQueries: s.metrics.metadataQueries.Load(), MetadataQueryNanos: s.metrics.metadataQueryNanos.Load(),
		TurnCacheHits: s.metrics.turnCacheHits.Load(), TurnCacheEntries: entries, TurnCacheEstimatedBytes: bytes, TurnCacheBudgetBytes: turnCacheBudget,
		TurnProjectionReads: s.metrics.turnProjectionReads.Load(), TurnBuilds: s.metrics.turnBuilds.Load(), TurnBuildNanos: s.metrics.turnBuildNanos.Load(),
		TurnRebuilds: s.metrics.turnRebuilds.Load(), TurnProjectionWrites: s.metrics.turnProjectionWrites.Load(),
	}
}

func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

func (s *Store) readHistory(path string) ([]Event, error) {
	start := time.Now()
	events, err := readEventsRepairTail(path)
	s.metrics.historyLoadNanos.Add(time.Since(start).Nanoseconds())
	if err != nil {
		s.metrics.historyLoadErrors.Add(1)
		return nil, err
	}
	s.metrics.historyLoads.Add(1)
	var bytes int64
	for _, event := range events {
		bytes += int64(len(event.Data))
	}
	s.metrics.cachedEventPayloadBytes.Add(bytes)
	return events, nil
}

func (s *Store) buildTurns(events []Event) []TurnSummary {
	start := time.Now()
	turns := buildTurnSummaries(events)
	s.metrics.turnBuilds.Add(1)
	s.metrics.turnBuildNanos.Add(time.Since(start).Nanoseconds())
	return turns
}
