package session

import (
	"context"
	"sort"
	"time"
)

// publishMetadata publishes an owned, immutable read model after the canonical
// log update. Callers serialize writers with state.mu (or have not exposed the
// state yet). No metadata reader acquires the history/write lock.
func (state *sessionState) publishMetadata() {
	value := cloneSession(state.session)
	state.metadata.Store(&value)
}

// ListCursor is the exclusive descending (updatedAt, id) page boundary.
type ListCursor struct {
	UpdatedAt time.Time
	ID        string
}

type ListOptions struct {
	ArchivedOnly bool
	States       []string
	Cursor       *ListCursor
	// Limit <= 0 preserves the historical unbounded response.
	Limit int
}

type SessionListPage struct {
	Sessions []Session
	HasMore  bool
}

// Query filters immutable metadata before sorting/paging and only deep-copies
// the returned rows. Its O(N) scan never waits for a session's history or disk
// writes. The page is a collection of per-session snapshots, not a transaction
// across all sessions, matching Filter's existing consistency contract.
func (s *Store) Query(ctx context.Context, filter ListFilter, options ListOptions) (SessionListPage, error) {
	started := time.Now()
	s.metrics.metadataQueries.Add(1)
	defer func() { s.metrics.metadataQueryNanos.Add(time.Since(started).Nanoseconds()) }()
	if err := ctx.Err(); err != nil {
		return SessionListPage{}, err
	}
	s.mu.RLock()
	values := make([]*Session, 0, len(s.sessions))
	for _, state := range s.sessions {
		if err := ctx.Err(); err != nil {
			s.mu.RUnlock()
			return SessionListPage{}, err
		}
		value := state.metadata.Load()
		if value == nil || (!filter.IncludeArchived && value.State == StateArchived) ||
			(options.ArchivedOnly && value.State != StateArchived) || !matchesSource(value.Source, filter) {
			continue
		}
		if len(options.States) > 0 {
			matched := false
			for _, allowed := range options.States {
				if value.State == allowed {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		if cursor := options.Cursor; cursor != nil {
			if !value.UpdatedAt.Before(cursor.UpdatedAt) &&
				!(value.UpdatedAt.Equal(cursor.UpdatedAt) && value.ID < cursor.ID) {
				continue
			}
		}
		values = append(values, value)
	}
	s.mu.RUnlock()
	sort.Slice(values, func(i, j int) bool {
		if values[i].UpdatedAt.Equal(values[j].UpdatedAt) {
			return values[i].ID > values[j].ID
		}
		return values[i].UpdatedAt.After(values[j].UpdatedAt)
	})
	page := SessionListPage{}
	if options.Limit > 0 && len(values) > options.Limit {
		values = values[:options.Limit]
		page.HasMore = true
	}
	page.Sessions = make([]Session, 0, len(values))
	for _, value := range values {
		if err := ctx.Err(); err != nil {
			return SessionListPage{}, err
		}
		page.Sessions = append(page.Sessions, cloneSession(*value))
	}
	if err := ctx.Err(); err != nil {
		return SessionListPage{}, err
	}
	return page, nil
}
