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
var errChatCursor = errors.New("chat message cursor not found")

// A Telegram send holds a per-chat read lock until the external request
// finishes. Close and reopen take the write lock to fence old sends.
var chatLifecycleLocks sync.Map

func chatLifecycleLock(root, id string) *sync.RWMutex {
	lock, _ := chatLifecycleLocks.LoadOrStore(filepath.Clean(root)+"\x00"+id, &sync.RWMutex{})
	return lock.(*sync.RWMutex)
}

type chatMessage struct {
	workID        string
	workEpoch     uint64
	workOffset    int64
	rawPosition   int
	turnID        string
	startEventID  int64
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
	InputPosition int    `json:"inputPosition,omitempty"`
	CreatedAt     string `json:"createdAt,omitempty"`
	ClientID      string `json:"clientId,omitempty"`
	OriginRoute   string `json:"originRoute,omitempty"`
	Automatic     bool   `json:"automatic,omitempty"`
	ID            string `json:"id"`
	Target        string `json:"target"`
	Text          string `json:"text"`
	Sender        string `json:"sender,omitempty"`
	Accepted      bool   `json:"accepted"`
	Epoch         uint64 `json:"epoch"`
	SourceChatID  string `json:"sourceChatId,omitempty"`
}

func (d chatDelivery) userInput() bool {
	return strings.HasPrefix(d.ID, "input-") || strings.HasPrefix(d.ID, "migrated-input-")
}

type resourceChat struct {
	omitted         int
	omittedUnread   int
	HistoryReads    map[string]map[string]int64 `json:"historyReads,omitempty"`
	ReadThrough     int                         `json:"readThrough"`
	ReadCursors     map[string]int              `json:"readCursors,omitempty"`
	LegacyUnread    map[string][]string         `json:"legacyUnread,omitempty"`
	Version         int                         `json:"version"`
	ID              string                      `json:"id"`
	ResourceID      string                      `json:"resourceId"`
	User            string                      `json:"user"`
	Open            bool                        `json:"open"`
	Epoch           uint64                      `json:"epoch"`
	Revision        uint64                      `json:"revision,omitempty"`
	StateNotice     *chatStateNotice            `json:"stateNotice,omitempty"`
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
	if c.Version >= 4 {
		expectedID = sharedChatID(c.ResourceID)
	}
	if (c.Version < 1 || c.Version > 5) || c.ID != id || c.ID != expectedID {
		return c, errors.New("invalid chat record")
	}
	if c.Messages == nil {
		c.Messages = []chatMessage{}
	}
	for _, delivery := range c.Outbox {
		if delivery.InputPosition == 0 {
			continue
		}
		index := delivery.InputPosition - 1
		if index < 0 || index > len(c.Messages) {
			return c, errors.New("invalid chat input position")
		}
		c.Messages = append(c.Messages, chatMessage{})
		copy(c.Messages[index+1:], c.Messages[index:])
		c.Messages[index] = chatMessage{ID: strings.TrimPrefix(delivery.ID, "input-"), Role: "user",
			Text: delivery.Text, Sender: delivery.Sender, CreatedAt: delivery.CreatedAt,
			ClientID: delivery.ClientID, OriginRoute: delivery.OriginRoute}
	}
	return c, nil
}
func saveChatLocked(root string, c resourceChat) error {
	inputs := make(map[int]bool)
	for _, delivery := range c.Outbox {
		if delivery.InputPosition > 0 {
			inputs[delivery.InputPosition] = true
		}
	}
	if len(inputs) > 0 {
		stored := make([]chatMessage, 0, len(c.Messages))
		for index, message := range c.Messages {
			if !inputs[index+1] {
				stored = append(stored, message)
			}
		}
		c.Messages = stored
	}
	if c.ID == sharedChatID(c.ResourceID) {
		c.Version = 4
		if len(inputs) > 0 || len(c.HistoryReads) > 0 {
			c.Version = 5
		}
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
	if message.workID != "" {
		return message.workOffset > c.HistoryReads[user][message.workID]
	}
	if message.rawPosition > 0 {
		i = message.rawPosition - 1
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
	unread := c.omittedUnread
	legacyUnread := c.legacyUnreadSet(user)
	for i, message := range c.Messages {
		if c.unread(user, i, message, legacyUnread) {
			unread++
		}
	}
	return &chatSnapshot{Open: c.Open, UnreadCount: unread}
}

type publicChat struct {
	ID              string               `json:"id"`
	ResourceID      string               `json:"resourceId"`
	Open            bool                 `json:"open"`
	Epoch           uint64               `json:"epoch"`
	Binding         app.AgentBinding     `json:"binding"`
	Messages        []publicChatMessage  `json:"messages"`
	QueuedMessages  *[]publicChatMessage `json:"queuedMessages,omitempty"`
	IntervalSeconds int                  `json:"intervalSeconds"`
	NextCheckAt     string               `json:"nextCheckAt,omitempty"`
	TimerMode       string               `json:"timerMode"`
	UnreadCount     int                  `json:"unreadCount"`
	HasOlder        *bool                `json:"hasOlder,omitempty"`
	HasNewer        *bool                `json:"hasNewer,omitempty"`
	EndPosition     int                  `json:"endPosition,omitempty"`
	Reset           bool                 `json:"reset,omitempty"`
}

type publicChatMessage struct {
	ID              string   `json:"id"`
	Role            string   `json:"role"`
	Text            string   `json:"text"`
	CreatedAt       string   `json:"createdAt"`
	Sender          string   `json:"sender,omitempty"`
	InputMessageIDs []string `json:"inputMessageIds,omitempty"`
	TurnID          string   `json:"turnId,omitempty"`
	StartEventID    int64    `json:"startEventId,omitempty"`
}

func (c resourceChat) public(users ...string) publicChat {
	return c.publicRange(0, len(c.Messages), users...)
}

func (c resourceChat) publicRange(start, end int, users ...string) publicChat {
	messages := make([]publicChatMessage, 0, end-start)
	for _, message := range c.Messages[start:end] {
		messages = append(messages, c.publicMessage(message))
	}
	return publicChat{
		ID: c.ID, ResourceID: c.ResourceID, Open: c.Open, Epoch: c.Epoch,
		Binding: c.Binding, Messages: messages, IntervalSeconds: c.IntervalSeconds,
		NextCheckAt: c.NextCheckAt, TimerMode: "once", UnreadCount: c.snapshot(users...).UnreadCount,
	}
}

func (c resourceChat) publicMessage(message chatMessage) publicChatMessage {
	return publicChatMessage{
		ID: message.ID, Role: message.Role, Text: message.Text,
		CreatedAt: message.CreatedAt, Sender: message.Sender,
		InputMessageIDs: c.executionInputIDs(message), TurnID: message.turnID, StartEventID: message.startEventID,
	}
}

func (c resourceChat) addQueuedMessages(workspacePath string, page *publicChat) {
	queued := make([]publicChatMessage, 0)
	if !c.Open || len(c.Outbox) == 0 {
		page.QueuedMessages = &queued
		return
	}
	mailbox, err := loadResourceMailboxForResource(workspacePath, c.ResourceID)
	if err != nil {
		return
	}
	statuses := make(map[string]string)
	for _, message := range mailbox.Messages {
		statuses[message.ID] = message.Status
	}
	pending := make(map[string]publicChatMessage)
	order := make([]string, 0)
	for _, delivery := range c.Outbox {
		if !delivery.userInput() || delivery.Target != c.ResourceID || delivery.Epoch != c.Epoch {
			continue
		}
		id := "chat-" + chatID(firstNonEmpty(delivery.SourceChatID, c.ID), delivery.ID)
		status, found := statuses[id]
		if status == resourceMessageQueued || status == resourceMessageDelivering || status == resourceMessageInterrupting ||
			(!found && !delivery.Accepted) {
			pending[id] = publicChatMessage{}
			order = append(order, id)
		}
	}
	if len(pending) != 0 {
		for _, message := range c.Messages {
			for _, id := range c.executionInputIDs(message) {
				if _, found := pending[id]; found {
					pending[id] = c.publicMessage(message)
				}
			}
		}
		seen := make(map[string]bool)
		for _, id := range order {
			if message := pending[id]; message.ID != "" && !seen[message.ID] {
				queued = append(queued, message)
				seen[message.ID] = true
			}
		}
	}
	page.QueuedMessages = &queued
}

func (c resourceChat) executionInputIDs(message chatMessage) []string {
	if message.Role != "user" {
		return nil
	}
	if message.LegacyChatID != "" && message.LegacyID != "" {
		return []string{
			"chat-" + chatID(message.LegacyChatID, "input-"+message.LegacyID),
			"chat-" + chatID(message.LegacyChatID, "migrated-input-"+chatID(message.LegacyChatID, message.LegacyID)),
		}
	}
	return []string{"chat-" + chatID(c.ID, "input-"+message.ID)}
}

// Message IDs are stable across appends and chat epochs, unlike offsets from
// the end of the history when new messages arrive between page requests.
func (c resourceChat) publicPage(limit int, before, after string, users ...string) (publicChat, error) {
	if limit == 0 {
		return c.public(users...), nil
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
			return publicChat{}, errChatCursor
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
	result := c.publicRange(start, end, users...)
	result.EndPosition = end + c.omitted
	older, newer := start+c.omitted > 0, end < len(c.Messages)
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
	rawPosition := 0
	historyChanged := false
	for i, message := range c.Messages[:index+1] {
		if message.workID == "" {
			position := message.rawPosition
			if position == 0 {
				position = i + 1
			}
			rawPosition = max(rawPosition, position)
			continue
		}
		if message.workOffset > c.HistoryReads[user][message.workID] {
			if c.HistoryReads == nil {
				c.HistoryReads = make(map[string]map[string]int64)
			}
			if c.HistoryReads[user] == nil {
				c.HistoryReads[user] = make(map[string]int64)
			}
			c.HistoryReads[user][message.workID] = message.workOffset
			historyChanged = true
		}
	}
	previous := c.ReadCursors[user]
	c.ReadCursors[user] = max(previous, rawPosition)
	remaining := make([]string, 0, len(c.LegacyUnread[user]))
	legacyUnread := c.legacyUnreadSet(user)
	for _, message := range c.Messages[index+1:] {
		if legacyUnread[message.ID] {
			remaining = append(remaining, message.ID)
		}
	}
	changed := historyChanged || previous != c.ReadCursors[user] || len(remaining) != len(c.LegacyUnread[user])
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
At Turn start and after recovery, read spec.md and memory.md (scheduler.md for Scheduler) and check open items. Record actionable commitments and this source message's ID/History reference in memory.md (scheduler.md for Scheduler) without storing reply capabilities. Change spec.md only within the exact scope of a verifiable direct user instruction or approval. Keep work and notification statuses separate. Check pending replies and due notifications before ending. Mark published only after publish succeeds. If rejected, keep the item blocked. Register Scheduler conditions for wake-ups. Keep follow-ups in the current document.
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
	if strings.HasPrefix(publicID, "auto-") {
		publicID = "user-" + chatID(user, id)
	}
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
	if err := c.queue("input-"+publicID, c.ResourceID, text, user); err != nil {
		return err
	}
	delivery := &c.Outbox[len(c.Outbox)-1]
	delivery.Automatic = true
	if c.Version >= 4 {
		delivery.InputPosition, delivery.ClientID, delivery.OriginRoute = len(c.Messages), id, originRoute
		delivery.CreatedAt = c.Messages[len(c.Messages)-1].CreatedAt
	}
	return nil
}
