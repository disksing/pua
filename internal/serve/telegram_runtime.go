package serve

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/disksing/pua/internal/app"
)

// The supervisor follows Workspace registration; workers never run without
// ownership and terminate before the service releases its Workspace locks.
func (s *server) startTelegram(ctx context.Context) {
	s.agents.runBackground(func() {
		workers := map[string]context.CancelFunc{}
		defer func() {
			for _, cancel := range workers {
				cancel()
			}
		}()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			cfg, err := s.loadConfig()
			if err == nil {
				present := map[string]bool{}
				for _, w := range cfg.Workspaces {
					present[w.ID] = true
					if workers[w.ID] != nil {
						continue
					}
					workerCtx, cancel := context.WithCancel(ctx)
					workers[w.ID] = cancel
					rt := s.telegramFor(w)
					s.agents.runBackground(func() { rt.run(workerCtx) })
				}
				for id, cancel := range workers {
					if !present[id] {
						cancel()
						delete(workers, id)
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}
func telegramWait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func (rt *telegramRuntime) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		if rt.server.requireWorkspaceOwnership(rt.workspace.Path) != nil {
			if !telegramWait(ctx, time.Second) {
				return
			}
			continue
		}
		rt.mu.Lock()
		st, err := rt.load()
		rt.mu.Unlock()
		if err != nil || !st.Enabled || st.Token == "" {
			if !telegramWait(ctx, time.Second) {
				return
			}
			continue
		}
		if len(st.Inbox) == 0 {
			var updates []telegramUpdate
			err = rt.call(ctx, st.Token, "getUpdates", map[string]any{"offset": st.Offset, "timeout": 5, "limit": 50, "allowed_updates": []string{"message", "callback_query"}}, &updates)
			rt.mu.Lock()
			current, e := rt.load()
			if e != nil || current.Revision != st.Revision || !current.Enabled {
				rt.mu.Unlock()
				continue
			}
			if err != nil {
				current.LastError = err.Error()
				_ = rt.save(current)
				rt.mu.Unlock()
				if !telegramWait(ctx, backoff) {
					return
				}
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
			backoff = time.Second
			current.LastPoll = time.Now().UTC().Format(time.RFC3339)
			current.LastError = ""
			for _, u := range updates {
				if u.ID >= current.Offset {
					current.Inbox = append(current.Inbox, u)
					current.Offset = max(current.Offset, u.ID+1)
				}
			}
			err = rt.save(current)
			rt.mu.Unlock()
			if err != nil {
				if !telegramWait(ctx, time.Second) {
					return
				}
				continue
			}
		}
		rt.mu.Lock()
		st, err = rt.load()
		if err == nil && st.Enabled {
			// Persist the remaining inbox after each item. Replayed user messages have
			// stable Chat IDs, so a crash after Chat commit cannot repeat execution.

			// Release the lock between inputs so an inbox burst cannot starve
			// Settings actions such as disabling or unlinking the connection.
			if len(st.Inbox) > 0 && ctx.Err() == nil {
				err = rt.process(ctx, &st, st.Inbox[0])
				if err == nil {
					st.Inbox = st.Inbox[1:]
					err = rt.save(st)
				} else {
					st.LastError = "Cannot process Telegram input; will retry"
					_ = rt.save(st)
				}
			}

			if err == nil {
				err = rt.flush(ctx, &st)
			}
		}
		rt.mu.Unlock()
		if err != nil && !telegramWait(ctx, time.Second) {
			return
		}
	}
}

// flush reads only chats whose files changed (or have queued delivery). It never
// reads execution history. Cursors count every Chat entry, but only agent entries
// can be sent. Startup deliberately audits all subscribed chats.
func (rt *telegramRuntime) flush(ctx context.Context, st *telegramState) error {
	ws, err := app.OpenWorkspace(rt.workspace.Path)
	if err != nil {
		return err
	}
	changed := false
	for i := range st.Routes {
		route := &st.Routes[i]
		if route.Paused || route.ThreadID == 0 {
			continue
		}
		_, userErr := ws.User(route.User)
		exists, archived, _, resourceErr := resourceExistsAndArchived(rt.workspace.Path, route.Resource)
		if userErr != nil || resourceErr != nil || !exists || archived || st.Accounts[route.ChatID] != route.User {
			route.Paused = true
			route.Pending = nil
			changed = true
			continue
		}
		id := chatID(route.Resource, route.User)
		path := chatPath(rt.workspace.Path, id)
		info, e := os.Stat(path)
		if e != nil {
			continue
		}
		if route.Pending == nil && rt.lastFiles[id].Equal(info.ModTime()) {
			continue
		}
		chatMu.Lock()
		chat, e := loadChatLocked(rt.workspace.Path, id)
		chatMu.Unlock()
		if e != nil {
			return e
		}
		if !chat.Open || chat.Epoch != route.Epoch {
			route.Paused = true
			route.Pending = nil
			changed = true
			continue
		}
		if route.Pending == nil {
			for route.Cursor < len(chat.Messages) {
				message := chat.Messages[route.Cursor]
				if message.Role != "agent" {
					route.Cursor++
					changed = true
					continue
				}
				route.Pending = &telegramDelivery{Text: message.Text, Chunks: telegramChunks(message.Text), NextCursor: route.Cursor + 1, Status: "queued"}
				changed = true
				break
			}
			if route.Pending == nil {
				rt.lastFiles[id] = info.ModTime()
				continue
			}
		}
		pending := route.Pending
		if pending.Status == "sending" {
			pending.Status = "unknown"
			pending.Error = "Delivery may have succeeded before restart. Check Telegram before retrying."
			changed = true
		}
		if pending.Status != "queued" || time.Now().Before(pending.RetryAt) || time.Now().Before(rt.nextSend[route.ChatID]) {
			continue
		}
		if len(pending.Chunks) == 0 {
			route.Cursor = pending.NextCursor
			route.Pending = nil
			changed = true
			continue
		}
		pending.Status = "sending"
		if err = rt.save(*st); err != nil {
			return err
		}
		// Recheck cancellation immediately before any external write.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err = rt.send(ctx, *st, route.ChatID, route.ThreadID, pending.Chunks[pending.Part], nil)
		rt.nextSend[route.ChatID] = time.Now().Add(time.Second)
		if err == nil {
			pending.Part++
			pending.Status = "queued"
			if pending.Part == len(pending.Chunks) {
				route.Cursor = pending.NextCursor
				route.Pending = nil
			}
		} else {
			var apiErr *telegramAPIError
			if errors.As(err, &apiErr) {
				if apiErr.Code == 429 {
					pending.Status = "queued"
					pending.RetryAt = time.Now().Add(time.Duration(max(apiErr.RetryAfter, 1)) * time.Second)
					rt.nextSend[route.ChatID] = pending.RetryAt
				} else {
					pending.Status = "failed"
					pending.Error = err.Error() + ". Check the bot and topic, then retry or skip."
				}
			} else {
				pending.Status = "unknown"
				pending.Error = "Delivery outcome unknown. Check Telegram before retrying."
			}
		}
		if err = rt.save(*st); err != nil {
			return err
		}
		// Bound external work while holding the Workspace connection lock.
		return nil
	}
	if changed {
		return rt.save(*st)
	}
	return nil
}
