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

// A Telegram send holds a per-chat read lock until the external request
// finishes. Close and reopen take the write lock to fence old sends.
var chatLifecycleLocks sync.Map

func chatLifecycleLock(root, id string) *sync.RWMutex {
	lock, _ := chatLifecycleLocks.LoadOrStore(filepath.Clean(root)+"\x00"+id, &sync.RWMutex{})
	return lock.(*sync.RWMutex)
}

type chatMessage struct {
	ID            string `json:"id"`
	Role          string `json:"role"`
	Text          string `json:"text"`
	CreatedAt     string `json:"createdAt"`
	Sender        string `json:"sender,omitempty"`
	ClientID      string `json:"clientId,omitempty"`
	OriginRoute   string `json:"originRoute,omitempty"`
	PublishOrigin string `json:"publishOrigin,omitempty"`
	LegacyChatID  string `json:"legacyChatId,omitempty"`
	LegacyID      string `json:"legacyId,omitempty"`
	LegacyIndex   int    `json:"legacyIndex,omitempty"`
}
type chatDelivery struct {
	ID           string `json:"id"`
	Target       string `json:"target"`
	Text         string `json:"text"`
	Sender       string `json:"sender,omitempty"`
	Accepted     bool   `json:"accepted"`
	Epoch        uint64 `json:"epoch"`
	SourceChatID string `json:"sourceChatId,omitempty"`
}

func (d chatDelivery) userInput() bool {
	return strings.HasPrefix(d.ID, "input-") || strings.HasPrefix(d.ID, "migrated-input-")
}

type resourceChat struct {
	ReadThrough     int                         `json:"readThrough"`
	ReadCursors     map[string]int              `json:"readCursors,omitempty"`
	LegacyUnread    map[string][]string         `json:"legacyUnread,omitempty"`
	Version         int                         `json:"version"`
	ID              string                      `json:"id"`
	ResourceID      string                      `json:"resourceId"`
	User            string                      `json:"user"`
	Open            bool                        `json:"open"`
	Epoch           uint64                      `json:"epoch"`
	EpochStart      *int                        `json:"epochStart,omitempty"`
	Token           string                      `json:"token"`
	Binding         app.AgentBinding            `json:"binding"`
	Notes           string                      `json:"notes"`
	IntervalSeconds int                         `json:"intervalSeconds"`
	NextCheckAt     string                      `json:"nextCheckAt,omitempty"`
	Messages        []chatMessage               `json:"messages"`
	Outbox          []chatDelivery              `json:"outbox"`
	LegacySources   map[string]legacyChatSource `json:"legacySources,omitempty"`
	MigrationEnd    int                         `json:"migrationEnd,omitempty"`
}

type legacyChatSource struct {
	User       string   `json:"user"`
	Epoch      uint64   `json:"epoch"`
	Open       bool     `json:"open"`
	Token      string   `json:"token,omitempty"`
	MessageIDs []string `json:"messageIds,omitempty"`
}

func (c *resourceChat) open() {
	c.Open = true
	c.Epoch++
	start := len(c.Messages)
	c.EpochStart = &start
	c.Token = newGenerationRecordID()
	for id, source := range c.LegacySources {
		source.Token = ""
		source.Open = false
		c.LegacySources[id] = source
	}
	c.IntervalSeconds = 0
	c.NextCheckAt = ""
}

func chatID(resourceID, user string) string {
	sum := sha256.Sum256([]byte(normalizedResourceID(resourceID) + "\x00" + user))
	return hex.EncodeToString(sum[:16])
}
func sharedChatID(resourceID string) string { return chatID(resourceID, "") }
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
	expectedID := chatID(c.ResourceID, c.User)
	if c.Version == 4 {
		expectedID = sharedChatID(c.ResourceID)
	}
	if (c.Version < 1 || c.Version > 4) || c.ID != id || c.ID != expectedID {
		return c, errors.New("invalid chat record")
	}
	if c.Messages == nil {
		c.Messages = []chatMessage{}
	}
	return c, nil
}
func saveChatLocked(root string, c resourceChat) error {
	if c.ID == sharedChatID(c.ResourceID) {
		c.Version = 4
	} else {
		c.Version = 3
	}
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
	id := sharedChatID(resourceID)
	c, err := loadChatLocked(root, id)
	if !os.IsNotExist(err) {
		return c, err
	}
	return migrateSharedChatLocked(root, resourceID)
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

func (c resourceChat) legacyUnreadSet(user string) map[string]bool {
	set := make(map[string]bool, len(c.LegacyUnread[user]))
	for _, id := range c.LegacyUnread[user] {
		set[id] = true
	}
	return set
}

func (c resourceChat) unread(user string, i int, message chatMessage, legacyUnread map[string]bool) bool {
	if message.Role != "agent" {
		return false
	}
	if c.Version < 4 {
		return i >= c.ReadThrough
	}
	return legacyUnread[message.ID] || i >= c.ReadCursors[user]
}

func (c resourceChat) snapshot(users ...string) *chatSnapshot {
	user := ""
	if len(users) > 0 {
		user = users[0]
	}
	unread := 0
	legacyUnread := c.legacyUnreadSet(user)
	for i, message := range c.Messages {
		if c.unread(user, i, message, legacyUnread) {
			unread++
		}
	}
	return &chatSnapshot{Open: c.Open, UnreadCount: unread}
}

type publicChat struct {
	ID              string              `json:"id"`
	ResourceID      string              `json:"resourceId"`
	Open            bool                `json:"open"`
	Epoch           uint64              `json:"epoch"`
	Binding         app.AgentBinding    `json:"binding"`
	Messages        []publicChatMessage `json:"messages"`
	IntervalSeconds int                 `json:"intervalSeconds"`
	NextCheckAt     string              `json:"nextCheckAt,omitempty"`
	TimerMode       string              `json:"timerMode"`
	UnreadCount     int                 `json:"unreadCount"`
	HasOlder        *bool               `json:"hasOlder,omitempty"`
	HasNewer        *bool               `json:"hasNewer,omitempty"`
}

type publicChatMessage struct {
	ID        string `json:"id"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`
	Sender    string `json:"sender,omitempty"`
}

func (c resourceChat) public(users ...string) publicChat {
	messages := make([]publicChatMessage, 0, len(c.Messages))
	for _, message := range c.Messages {
		messages = append(messages, publicChatMessage{
			ID: message.ID, Role: message.Role, Text: message.Text,
			CreatedAt: message.CreatedAt, Sender: message.Sender,
		})
	}
	return publicChat{
		ID: c.ID, ResourceID: c.ResourceID, Open: c.Open, Epoch: c.Epoch,
		Binding: c.Binding, Messages: messages, IntervalSeconds: c.IntervalSeconds,
		NextCheckAt: c.NextCheckAt, TimerMode: "once", UnreadCount: c.snapshot(users...).UnreadCount,
	}
}

// Message IDs are stable across appends and chat epochs, unlike offsets from
// the end of the history when new messages arrive between page requests.
func (c resourceChat) publicPage(limit int, before, after string, users ...string) (publicChat, error) {
	result := c.public(users...)
	if limit == 0 {
		return result, nil
	}
	start, end := 0, len(c.Messages)
	if before != "" || after != "" {
		cursor := before
		if after != "" {
			cursor = after
		}
		index := -1
		for i, message := range c.Messages {
			if message.ID == cursor {
				index = i
				break
			}
		}
		if index < 0 {
			return publicChat{}, errors.New("chat message cursor not found")
		}
		if before != "" {
			end = index
		} else {
			start = index + 1
		}
	}
	if after != "" {
		end = min(end, start+limit)
	} else {
		start = max(start, end-limit)
	}
	result.Messages = result.Messages[start:end]
	older, newer := start > 0, end < len(c.Messages)
	result.HasOlder, result.HasNewer = &older, &newer
	return result, nil
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

func (c *resourceChat) markRead(user, id string) bool {
	index := -1
	for i, message := range c.Messages {
		if message.ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return false
	}
	if c.Version < 4 {
		previous := c.ReadThrough
		c.ReadThrough = max(c.ReadThrough, index+1)
		return previous != c.ReadThrough
	}
	if c.ReadCursors == nil {
		c.ReadCursors = make(map[string]int)
	}
	previous := c.ReadCursors[user]
	c.ReadCursors[user] = max(previous, index+1)
	remaining := make([]string, 0, len(c.LegacyUnread[user]))
	legacyUnread := c.legacyUnreadSet(user)
	for _, message := range c.Messages[index+1:] {
		if legacyUnread[message.ID] {
			remaining = append(remaining, message.ID)
		}
	}
	changed := previous != c.ReadCursors[user] || len(remaining) != len(c.LegacyUnread[user])
	if c.LegacyUnread != nil {
		c.LegacyUnread[user] = remaining
	}
	return changed
}
func (c *resourceChat) queue(id, target, text string, users ...string) error {
	sender := ""
	if len(users) > 0 {
		sender = users[0]
	}
	for _, d := range c.Outbox {
		if d.ID == id && d.SourceChatID == "" {
			if d.Target != target || d.Text != text || d.Sender != sender {
				return errors.New("operation id conflicts with existing content")
			}
			return nil
		}
	}
	c.Outbox = append(c.Outbox, chatDelivery{ID: id, Target: target, Text: text, Sender: sender, Epoch: c.Epoch})
	return nil
}

// The capability is scoped to one resource and open epoch. Carry it in each
// input because an execution session can outlive a chat close/reopen.
func (c resourceChat) prompt(text string, users ...string) string {
	user := c.User
	if len(users) > 0 {
		user = users[0]
	}
	capability := c.ID + "." + c.Token
	if c.Version >= 4 {
		capability = c.Token
	}
	return fmt.Sprintf(`User Chat input for resource %s, user %s. This resource has one shared Chat visible to all Workspace users. You are this resource's execution Agent: handle the request directly and follow its normal work/state instructions. This user may be inserting a message into an existing Turn; attribute requests to their sender.
Only explicit publish writes to Chat. Ordinary final, commentary, tools, internal collaboration and system notifications are execution events, NEVER automatic public replies. Before finishing, publish the answer to this input using the command below, even if it opened the Turn. Do not delegate to a dialogue Agent. Reuse a stable --id when retrying; do not publish the same answer twice. Do not expose the reply capability in public messages. Closure revokes it; if publication is rejected, retain the result in execution history and do not redirect to another chat.
Read this resource's chat.md open items, including after context recovery. Record actionable commitments and this source message's ID/History reference without storing reply capabilities. Keep work status separate from notification status; progress does not discharge the final reply. Before ending, check every user's pending replies and due notifications. Mark published only after publish succeeds; if publication is rejected, keep the item blocked. chat.md does not schedule wake-ups: register Scheduler conditions when needed. Do not mirror these items into legacy chat notes.
Read the shared history: pua chat show --reply-to=%s
Reply: pua chat publish --reply-to=%s --id=reply-%s '<answer>'

%s`, c.ResourceID, user, capability, capability, chatID(c.ID, text), text)
}

// Retire legacy dialogue inputs without replaying work already delivered to a
// provider. A queued, never-attempted input is safe to move to execution. Old
// mailbox entries and outbox records remain available for audit.
func migrateChatLocked(root string, c *resourceChat) error {
	if err := migrateChatDataLocked(root, c); err != nil {
		return err
	}
	return saveChatLocked(root, *c)
}

func migrateChatDataLocked(root string, c *resourceChat) error {
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
	return nil
}

// appendUserInput is shared by the web and Telegram transports. Callers hold
// chatMu and commit Chat plus outbox together before acknowledging the input.
func (c *resourceChat) appendUserInput(id, text string, context ...string) error {
	if !c.Open {
		return errors.New("chat is closed")
	}
	user, originRoute := c.User, ""
	if len(context) > 0 {
		user = context[0]
	}
	if len(context) > 1 {
		originRoute = context[1]
	}
	if c.Version >= 4 && user == "" {
		return errors.New("user is required")
	}
	for _, message := range c.Messages {
		if message.Role == "user" && message.Sender == user && (message.ClientID == id || (message.ClientID == "" && message.ID == id)) {
			if message.Text != text {
				return errors.New("message id conflicts with existing content")
			}
			return nil
		}
	}
	publicID := id
	for _, message := range c.Messages {
		if message.ID == publicID {
			publicID = "user-" + chatID(user, id)
			break
		}
	}
	if strings.TrimSpace(id) == "" || len(id) > 200 || strings.TrimSpace(text) == "" {
		return errors.New("id and text are required")
	}
	for _, message := range c.Messages {
		if message.ID == publicID {
			return errors.New("message id conflicts with existing content")
		}
	}
	c.Messages = append(c.Messages, chatMessage{ID: publicID, ClientID: id, Role: "user", Sender: user, OriginRoute: originRoute, Text: text, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	return c.queue("input-"+publicID, c.ResourceID, c.prompt("User message "+id+" from "+user+":\n"+text, user), user)
}
