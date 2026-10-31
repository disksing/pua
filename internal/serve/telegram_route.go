package serve

import (
	"os"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
)

const (
	telegramPauseManual   = "manual"
	telegramPauseCreating = "creating"
	telegramPauseUnpaired = "unpaired"
	telegramPauseUser     = "user_unavailable"
	telegramPauseResource = "resource_unavailable"
	telegramPauseArchived = "resource_archived"
	telegramPauseChat     = "chat_unavailable"
	telegramPauseClosed   = "chat_closed"
	telegramPauseEpoch    = "epoch_boundary_unknown"
	telegramPauseDeleted  = "user_deleted"
)

func (r *telegramRoute) pause(reason string) bool {
	if r.Paused && r.PauseReason == reason {
		return false
	}
	r.Paused = true
	r.PauseReason = reason
	r.PausedAt = time.Now().UTC().Format(time.RFC3339)
	return true
}

func (r *telegramRoute) unpause() bool {
	if !r.Paused && r.PauseReason == "" && r.PausedAt == "" {
		return false
	}
	r.Paused = false
	r.PauseReason = ""
	r.PausedAt = ""
	return true
}

func telegramAutoRecheck(reason string) bool {
	switch reason {
	case telegramPauseUnpaired, telegramPauseUser, telegramPauseResource, telegramPauseArchived,
		telegramPauseChat, telegramPauseClosed, telegramPauseEpoch:
		return true
	default:
		return false
	}
}

func telegramPauseHint(reason string) string {
	switch reason {
	case telegramPauseManual:
		return "Paused with /stop. Send /resume in this topic to continue."
	case telegramPauseCreating:
		return "Topic creation is not confirmed. Use /bind in the intended topic."
	case telegramPauseUnpaired:
		return "The account is not paired with this user. Check Workspace Telegram settings."
	case telegramPauseUser:
		return "The Workspace user could not be verified. Check the user and refresh status."
	case telegramPauseResource:
		return "The resource could not be verified. Check that it is available."
	case telegramPauseArchived:
		return "The resource is archived and cannot receive messages. Choose an active resource in PUA."
	case telegramPauseChat:
		return "The PUA chat could not be read. Check the chat and refresh status."
	case telegramPauseClosed:
		return "The PUA chat is closed. Reopen it in PUA to continue."
	case telegramPauseEpoch:
		return "The previous chat period cannot be verified. Send /resume for recovery options."
	case telegramPauseDeleted:
		return "The Workspace user was deleted. Pair again, then send /resume for recovery options."
	default:
		return "The pause cause is unknown. Send /resume for recovery options."
	}
}

func telegramForwardable(message chatMessage, route *telegramRoute) bool {
	if message.LegacyChatID != "" && (route.LegacyChat != message.LegacyChatID || message.LegacyIndex < route.LegacyCursor) {
		return false
	}
	switch message.Role {
	case "user":
		if message.OriginRoute != "" {
			return message.Text != "" && message.OriginRoute != route.key()
		}
		id := message.ID
		if message.LegacyID != "" {
			id = message.LegacyID
		}
		return message.Text != "" && !(strings.HasPrefix(id, "telegram-") && message.Sender == route.User)
	case "agent":
		return message.Text != ""
	default:
		return false
	}
}

// Keep unresolved delivery state and an explicit notice for older messages;
// neither can be forwarded into a new chat period without a user decision.
func (r *telegramRoute) holdUntil(c resourceChat, end int, reason string) bool {
	if r.Cursor < 0 || end < r.Cursor || end > len(c.Messages) {
		return false
	}
	count := 0
	for _, message := range c.Messages[r.Cursor:end] {
		if telegramForwardable(message, r) {
			count++
		}
	}
	if r.Pending != nil && r.Pending.Status == "sending" {
		r.Pending.Status = "unknown"
		r.Pending.Error = "Delivery may have succeeded before restart. Check Telegram before deciding."
	}
	if count > 0 || r.Pending != nil {
		r.Held = append(r.Held, telegramHeldDelivery{
			ID: newGenerationRecordID(), Epoch: r.Epoch, Reason: reason,
			Count: count, HeldAt: time.Now().UTC().Format(time.RFC3339), Pending: r.Pending,
		})
	}
	r.Pending = nil
	r.Cursor = end
	return true
}

func (r *telegramRoute) migrateSharedChat(c resourceChat) bool {
	if r.SharedChat {
		return true
	}
	id := chatID(r.Resource, r.User)
	source, found := c.LegacySources[id]
	if !found || !source.Open || source.Epoch != r.Epoch || r.Cursor < 0 || r.Cursor > len(source.MessageIDs) {
		return false
	}
	position := func(oldIndex int) (int, bool) {
		if oldIndex == len(source.MessageIDs) {
			return c.MigrationEnd, true
		}
		if oldIndex < 0 || oldIndex >= len(source.MessageIDs) {
			return 0, false
		}
		for i, message := range c.Messages {
			if message.ID == source.MessageIDs[oldIndex] {
				return i, true
			}
		}
		return 0, false
	}
	cursor, ok := position(r.Cursor)
	if !ok {
		return false
	}
	if r.Pending != nil {
		if r.Pending.NextCursor <= r.Cursor || r.Pending.NextCursor > len(source.MessageIDs) {
			return false
		}
		last, exists := position(r.Pending.NextCursor - 1)
		if !exists {
			return false
		}
		r.Pending.NextCursor = last + 1
	}
	r.LegacyChat, r.LegacyCursor = id, r.Cursor
	r.Cursor, r.Epoch, r.SharedChat = cursor, c.Epoch, true
	return true
}

// checkRoute only automatically releases pauses whose cause this version
// recorded. Legacy and explicit pauses require an explicit user action.
func (rt *telegramRuntime) checkRoute(st *telegramState, r *telegramRoute, ws *app.Workspace, allowCached bool) (resourceChat, time.Time, bool, bool) {
	var empty resourceChat
	if r.ThreadID == 0 || (r.Paused && !telegramAutoRecheck(r.PauseReason)) {
		return empty, time.Time{}, false, false
	}
	if st.Accounts[r.ChatID] != r.User {
		return empty, time.Time{}, false, r.pause(telegramPauseUnpaired)
	}
	if _, err := ws.User(r.User); err != nil {
		return empty, time.Time{}, false, r.pause(telegramPauseUser)
	}
	exists, archived, _, err := resourceExistsAndArchived(rt.workspace.Path, r.Resource)
	if err != nil || !exists {
		return empty, time.Time{}, false, r.pause(telegramPauseResource)
	}
	if archived {
		return empty, time.Time{}, false, r.pause(telegramPauseArchived)
	}
	id := sharedChatID(r.Resource)
	chatMu.Lock()
	c, err := ensureChatLocked(rt.workspace.Path, r.Resource, r.User)
	chatMu.Unlock()
	if err != nil {
		return empty, time.Time{}, false, r.pause(telegramPauseChat)
	}
	info, err := os.Stat(chatPath(rt.workspace.Path, id))
	if err != nil {
		return empty, time.Time{}, false, r.pause(telegramPauseChat)
	}
	if allowCached && r.SharedChat && !r.Paused && r.Pending == nil && rt.lastFiles[r.key()].Equal(info.ModTime()) {
		return empty, info.ModTime(), true, false
	}
	if !c.Open {
		return c, info.ModTime(), false, r.pause(telegramPauseClosed)
	}
	changed := false
	if !r.SharedChat {
		if !r.migrateSharedChat(c) {
			return c, info.ModTime(), false, r.pause(telegramPauseEpoch)
		}
		changed = true
	}
	if r.Cursor < 0 || r.Cursor > len(c.Messages) {
		return c, info.ModTime(), false, r.pause(telegramPauseEpoch)
	}
	if c.Epoch != r.Epoch {
		if c.Epoch <= r.Epoch || c.EpochStart == nil || !r.holdUntil(c, *c.EpochStart, "chat_reopened") {
			return c, info.ModTime(), false, r.pause(telegramPauseEpoch)
		}
		r.Epoch = c.Epoch
		delete(rt.lastFiles, r.key())
		changed = true
	}
	changed = r.unpause() || changed
	return c, info.ModTime(), true, changed
}
