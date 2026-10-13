package serve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/workspacepath"
)

// A dialogue address is a runtime partition, never a workspace resource.
// Keeping it distinct lets the existing durable controllers and mailboxes
// order each channel independently without running Task workflow hooks.
const dialoguePrefix = "dialogue-"

var chatMu sync.Mutex

type chatMessage struct {
	ID        string `json:"id"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`
}
type chatDelivery struct {
	ID       string `json:"id"`
	Target   string `json:"target"`
	Text     string `json:"text"`
	Accepted bool   `json:"accepted"`
	Epoch    uint64 `json:"epoch"`
}
type resourceChat struct {
	ReadThrough     int              `json:"readThrough"`
	Version         int              `json:"version"`
	ID              string           `json:"id"`
	ResourceID      string           `json:"resourceId"`
	User            string           `json:"user"`
	Open            bool             `json:"open"`
	Epoch           uint64           `json:"epoch"`
	Token           string           `json:"token"`
	Binding         app.AgentBinding `json:"binding"`
	Notes           string           `json:"notes"`
	IntervalSeconds int              `json:"intervalSeconds"`
	NextCheckAt     string           `json:"nextCheckAt,omitempty"`
	Messages        []chatMessage    `json:"messages"`
	Outbox          []chatDelivery   `json:"outbox"`
}

func chatID(resourceID, user string) string {
	sum := sha256.Sum256([]byte(normalizedResourceID(resourceID) + "\x00" + user))
	return hex.EncodeToString(sum[:16])
}
func chatPath(root, id string) string {
	return filepath.Join(workspacepath.ControlDir(root), "chats", id+".json")
}
func validChatID(id string) bool { b, err := hex.DecodeString(id); return err == nil && len(b) == 16 }
func loadChatLocked(root, id string) (resourceChat, error) {
	if !validChatID(id) {
		return resourceChat{}, errors.New("invalid chat id")
	}
	data, err := os.ReadFile(chatPath(root, id))
	if err != nil {
		return resourceChat{}, err
	}
	var c resourceChat
	if err = json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	if (c.Version != 1 && c.Version != 2) || c.ID != id || c.ID != chatID(c.ResourceID, c.User) {
		return c, errors.New("invalid chat record")
	}
	if c.Messages == nil {
		c.Messages = []chatMessage{}
	}
	return c, nil
}
func saveChatLocked(root string, c resourceChat) error {
	c.Version = 2
	return saveJSONStateFile(chatPath(root, c.ID), ".chat-*.tmp", c)
}
func loadDialogue(root, address string) (resourceChat, error) {
	if !strings.HasPrefix(address, dialoguePrefix) {
		return resourceChat{}, errors.New("not a dialogue address")
	}
	chatMu.Lock()
	defer chatMu.Unlock()
	return loadChatLocked(root, strings.TrimPrefix(address, dialoguePrefix))
}
func ensureChatLocked(root, resourceID, user string) (resourceChat, error) {
	id := chatID(resourceID, user)
	c, err := loadChatLocked(root, id)
	if !os.IsNotExist(err) {
		return c, err
	}
	c = resourceChat{Version: 2, ID: id, ResourceID: normalizedResourceID(resourceID), User: user, Messages: []chatMessage{}, Binding: app.AgentBinding{Kind: "profile", Name: "default"}}
	return c, saveChatLocked(root, c)
}
func (c resourceChat) address() string { return dialoguePrefix + c.ID }

type chatSnapshot struct {
	Open        bool `json:"open"`
	UnreadCount int  `json:"unreadCount"`
}

type sessionReadSnapshot struct {
	LatestTurnNumber int `json:"latestTurnNumber"`
	ReadTurnNumber   int `json:"readTurnNumber"`
	UnreadCount      int `json:"unreadCount"`
}

func (c resourceChat) snapshot() *chatSnapshot {
	unread := 0
	for i, message := range c.Messages {
		if i >= c.ReadThrough && message.Role == "agent" {
			unread++
		}
	}
	return &chatSnapshot{Open: c.Open, UnreadCount: unread}
}

func (c resourceChat) public() any {
	return struct {
		ID              string           `json:"id"`
		ResourceID      string           `json:"resourceId"`
		Open            bool             `json:"open"`
		Epoch           uint64           `json:"epoch"`
		Binding         app.AgentBinding `json:"binding"`
		Messages        []chatMessage    `json:"messages"`
		IntervalSeconds int              `json:"intervalSeconds"`
		NextCheckAt     string           `json:"nextCheckAt,omitempty"`
		TimerMode       string           `json:"timerMode"`
		UnreadCount     int              `json:"unreadCount"`
	}{c.ID, c.ResourceID, c.Open, c.Epoch, c.Binding, c.Messages, c.IntervalSeconds, c.NextCheckAt, "once", c.snapshot().UnreadCount}
}
func (c *resourceChat) appendMessage(id, role, text string) error {
	if strings.TrimSpace(id) == "" || len(id) > 200 || strings.TrimSpace(text) == "" {
		return errors.New("id and text are required")
	}
	for _, existing := range c.Messages {
		if existing.ID == id {
			if existing.Role != role || existing.Text != text {
				return errors.New("message id conflicts with existing content")
			}
			return nil
		}
	}
	c.Messages = append(c.Messages, chatMessage{ID: id, Role: role, Text: text, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	return nil
}
func (c *resourceChat) queue(id, target, text string) error {
	for _, d := range c.Outbox {
		if d.ID == id {
			if d.Target != target || d.Text != text {
				return errors.New("operation id conflicts with existing content")
			}
			return nil
		}
	}
	c.Outbox = append(c.Outbox, chatDelivery{ID: id, Target: target, Text: text, Epoch: c.Epoch})
	return nil
}
func (c resourceChat) prompt(text string) string {
	return fmt.Sprintf(`You are the dialogue Agent for resource %s and user %s. This is a separate communication channel, not the execution Agent. Read the resource instructions and user preferences for context, but do not run its Task state completion hooks. Discuss requirements, query resource status/history/artifacts, and delegate actual work using pua chat dispatch. Do not do long execution work here. Only pua chat publish writes to the user's chat; your ordinary final/analysis and tools are private events. Use pua chat show to read the durable chat and pua chat notes to read/update your own persistent notes. Maintain tasks you care about and notification decisions in those notes; PUA has no watch list. Timers are OFF by default; opening or resuming chat is not a reason to schedule one. Use pua chat timer <seconds> only for a concrete user-requested follow-up (0 cancels, 60..86400 seconds). Each timer is ONE-SHOT and turns off when its check is queued, including timers saved by older versions. Read your notes and reassess whether another check is necessary; do not blindly renew a fixed one-minute loop. Choose a delay based on expected progress or the next known schedule, and back off when nothing changes. A long-term request for updates alone does not justify checking idle daily/weekly tasks every minute. Cancel with pua chat timer 0 when no follow-up remains. Avoid duplicate notifications. All publish and dispatch calls require a stable --id for retry safety. Reuse that id when retrying. You may query any authorized resource, but do not assume a task has completed without evidence.

Trigger:
%s`, c.ResourceID, c.User, text)
}
