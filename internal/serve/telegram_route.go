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
	default:
		return "The pause cause is unknown. Send /resume for recovery options."
	}
}

func telegramForwardable(message chatMessage) bool {
	switch message.Role {
	case "user":
		return message.Text != "" && !strings.HasPrefix(message.ID, "telegram-")
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
		if telegramForwardable(message) {
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
	id := chatID(r.Resource, r.User)
	info, err := os.Stat(chatPath(rt.workspace.Path, id))
	if err != nil {
		return empty, time.Time{}, false, r.pause(telegramPauseChat)
	}
	if allowCached && !r.Paused && r.Pending == nil && rt.lastFiles[id].Equal(info.ModTime()) {
		return empty, info.ModTime(), true, false
	}
	chatMu.Lock()
	c, err := loadChatLocked(rt.workspace.Path, id)
	chatMu.Unlock()
	if err != nil {
		return empty, time.Time{}, false, r.pause(telegramPauseChat)
	}
	if !c.Open {
		return c, info.ModTime(), false, r.pause(telegramPauseClosed)
	}
	if r.Cursor < 0 || r.Cursor > len(c.Messages) {
		return c, info.ModTime(), false, r.pause(telegramPauseEpoch)
	}
	changed := false
	if c.Epoch != r.Epoch {
		if c.Epoch <= r.Epoch || c.EpochStart == nil || !r.holdUntil(c, *c.EpochStart, "chat_reopened") {
			return c, info.ModTime(), false, r.pause(telegramPauseEpoch)
		}
		r.Epoch = c.Epoch
		delete(rt.lastFiles, id)
		changed = true
	}
	changed = r.unpause() || changed
	return c, info.ModTime(), true, changed
}
