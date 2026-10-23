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

// Legacy dialogue addresses remain stable for history and migration only.
// They are never destinations for new user input or runnable resources.
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

func (d chatDelivery) userInput() bool {
	return strings.HasPrefix(d.ID, "input-") || strings.HasPrefix(d.ID, "migrated-input-")
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
	if (c.Version < 1 || c.Version > 3) || c.ID != id || c.ID != chatID(c.ResourceID, c.User) {
		return c, errors.New("invalid chat record")
	}
	if c.Messages == nil {
		c.Messages = []chatMessage{}
	}
	return c, nil
}
func saveChatLocked(root string, c resourceChat) error {
	c.Version = 3
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
	if err == nil && c.Version < 3 {
		err = migrateChatLocked(root, &c)
	}
	if !os.IsNotExist(err) {
		return c, err
	}
	c = resourceChat{Version: 3, ID: id, ResourceID: normalizedResourceID(resourceID), User: user, Messages: []chatMessage{}, Binding: app.AgentBinding{Kind: "profile", Name: "default"}}
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

// The reply capability is scoped to one resource/user/open epoch. Carry it in
// each input rather than session environment: a busy execution session may
// receive inputs from multiple users and survives chat close/reopen.
func (c resourceChat) prompt(text string) string {
	return fmt.Sprintf(`User Chat input for resource %s, user %s. You are this resource's execution Agent: handle the request directly and follow its normal work/state instructions. This routing replaces legacy instructions that prohibit execution Agents from publishing or require a dialogue relay. This user may be inserting a message into an existing Turn; keep each user's reply target separate.
Only explicit publish writes to Chat. Ordinary final, commentary, tools, internal collaboration and system notifications are execution events, NEVER automatic public replies. Before finishing, publish the answer to this input using the command below, even if it opened the Turn. Do not delegate to a dialogue Agent. Reuse a stable --id when retrying; do not publish the same answer twice. Do not expose the reply capability in public messages. Closure revokes it; if publication is rejected, retain the result in execution history and do not redirect to another chat.
Read this user's history: pua chat show --reply-to=%s.%s
Reply: pua chat publish --reply-to=%s.%s --id=reply-%s '<answer>'

%s`, c.ResourceID, c.User, c.ID, c.Token, c.ID, c.Token, chatID(c.ID, text), text)
}

// Retire legacy dialogue inputs without replaying work already delivered to a
// provider. A queued, never-attempted input is safe to move to execution. Old
// mailbox entries and outbox records remain available for audit.
func migrateChatLocked(root string, c *resourceChat) error {
	if c.Version >= 3 {
		return nil
	}
	mailbox, err := loadResourceMailboxForResource(root, c.address())
	if err != nil {
		return err
	}
	c.Token = ""
	if c.Open {
		c.Token = newGenerationRecordID()
	}
	c.IntervalSeconds, c.NextCheckAt = 0, ""
	var migrated []chatDelivery
	for i := range c.Outbox {
		d := &c.Outbox[i]
		if d.Target != c.address() {
			continue
		}
		safe := !d.Accepted
		for _, message := range mailbox.Messages {
			if message.ID == "chat-"+chatID(c.ID, d.ID) {
				// Retirement may have reached this mailbox before a failed
				// migration write was retried. Never-attempted cancellation
				// is still safe; delivery_unknown is deliberately excluded.
				safe = message.AttemptCount == 0 && (message.Status == resourceMessageQueued ||
					(message.Status == resourceMessageUndeliverable && message.LastErrorCode == "resource_archived"))
				break
			}
		}
		if safe && c.Open && d.Epoch == c.Epoch && strings.HasPrefix(d.ID, "input-") {
			for _, message := range c.Messages {
				if message.Role == "user" && "input-"+message.ID == d.ID {
					migrated = append(migrated, chatDelivery{ID: "migrated-input-" + chatID(c.ID, message.ID), Target: c.ResourceID, Text: c.prompt("User message " + message.ID + " from " + c.User + ":\n" + message.Text), Epoch: c.Epoch})
					break
				}
			}
		}
		d.Accepted = true
	}
	c.Outbox = append(c.Outbox, migrated...)

	c.Version = 3
	return saveChatLocked(root, *c)
}

// appendUserInput is shared by the web and Telegram transports. Callers hold
// chatMu and commit Chat plus outbox together before acknowledging the input.
func (c *resourceChat) appendUserInput(id, text string) error {
	if !c.Open {
		return errors.New("chat is closed")
	}
	for _, message := range c.Messages {
		if message.ID == id {
			return c.appendMessage(id, "user", text)
		}
	}
	if err := c.appendMessage(id, "user", text); err != nil {
		return err
	}
	return c.queue("input-"+id, c.ResourceID, c.prompt("User message "+id+" from "+c.User+":\n"+text))
}
