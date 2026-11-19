package serve

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/workspacepath"
)

type chatRequest struct {
	Position int              `json:"position,omitempty"`
	ID       string           `json:"id"`
	Text     string           `json:"text"`
	Target   string           `json:"target"`
	Seconds  int              `json:"seconds"`
	Binding  app.AgentBinding `json:"binding"`
}

func (m *agentManager) handleChat(w http.ResponseWriter, r *http.Request, workspaceID, resourceID string, parts []string) {
	workspace, err := m.server.workspace(workspaceID)
	if err != nil {
		writeError(w, err, 404)
		return
	}
	if err = m.server.requireWorkspaceOwnership(workspace.Path); err != nil {
		writeError(w, err, 409)
		return
	}
	exists, archived, _, err := resourceExistsAndArchived(workspace.Path, resourceID)
	if err != nil || !exists {
		writeError(w, errors.New("resource not found"), 404)
		return
	}
	operation := ""
	if len(parts) > 0 {
		operation = parts[0]
	}
	var pageLimit int
	var before, after string
	if operation != "runtime" {
		before, after = r.URL.Query().Get("before"), r.URL.Query().Get("after")
		if rawLimit := r.URL.Query().Get("limit"); rawLimit != "" {
			pageLimit, err = strconv.Atoi(rawLimit)
			if err != nil || pageLimit < 1 || pageLimit > 100 {
				writeError(w, errors.New("chat limit must be between 1 and 100"), 400)
				return
			}
		}
		if (before != "" && after != "") || (pageLimit == 0 && (before != "" || after != "")) ||
			(r.Method != http.MethodGet && (before != "" || after != "")) {
			writeError(w, errors.New("invalid chat page cursor"), 400)
			return
		}
	}
	agent := r.Header.Get("X-PUA-Chat-Token") != ""
	var user string
	var identity resourceChat
	if agent {
		chatID := r.Header.Get("X-PUA-Chat")
		if chatID == "" {
			chatID = sharedChatID(resourceID)
		}
		c, loadErr := loadDialogue(workspace.Path, dialoguePrefix+chatID)
		if loadErr != nil || c.ResourceID != normalizedResourceID(resourceID) {
			writeError(w, errors.New("invalid chat identity"), 403)
			return
		}
		identity = c
		if c.Version < 4 {
			user = c.User
			ws, openErr := app.OpenWorkspace(workspace.Path)
			if openErr != nil {
				writeError(w, openErr, 500)
				return
			}
			if _, userErr := ws.User(user); userErr != nil {
				writeError(w, errors.New("chat user no longer exists"), 403)
				return
			}
		}
	} else {
		// Native EventSource cannot set X-PUA-User. Accept the selected user
		// only on this read-only stream, with the same validation as headers.
		if r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "runtime" && parts[1] == "stream" && r.Header.Get(workspaceUserHeader) == "" {
			r = r.Clone(r.Context())
			r.Header.Set(workspaceUserHeader, r.URL.Query().Get("user"))
		}
		user, err = m.server.workspaceUserName(r, workspace.Path)
		if err != nil {
			writeError(w, err, 400)
			return
		}
	}
	noticeWorkID := ""
	noticeBoundary := false
	if r.Method == http.MethodPost && (operation == "open" || operation == "close") {
		noticeContext, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		noticeWorkID, noticeBoundary = m.chatNoticeWork(noticeContext, workspace, resourceID)
		cancel()
		lifecycle := chatLifecycleLock(workspace.Path, sharedChatID(resourceID))
		lifecycle.Lock()
		defer lifecycle.Unlock()
	}
	chatMu.Lock()
	if !agent {
		ws, openErr := app.OpenWorkspace(workspace.Path)
		if openErr == nil {
			_, openErr = ws.User(user)
		}
		if openErr != nil {
			chatMu.Unlock()
			writeError(w, errors.New("chat user no longer exists"), 400)
			return
		}
	}
	c, err := ensureChatLocked(workspace.Path, resourceID, user)
	if err != nil {
		chatMu.Unlock()
		writeError(w, err, 500)
		return
	}
	if agent {
		token := c.Token
		if identity.ID != c.ID {
			source, found := c.LegacySources[identity.ID]
			if !found || !source.Open || source.User != user {
				token = ""
			} else {
				token = source.Token
			}
		}
		if !c.Open || archived || token == "" || r.Header.Get("X-PUA-Chat-Generation") != "" || subtle.ConstantTimeCompare([]byte(token), []byte(r.Header.Get("X-PUA-Chat-Token"))) != 1 {
			chatMu.Unlock()
			writeError(w, errors.New("chat is closed or reply capability has expired"), 409)
			return
		}
	}
	if resourceID == "workspace" && !c.Open {
		c.open()
		err = saveChatLocked(workspace.Path, c)
	}
	if err != nil {
		chatMu.Unlock()
		writeError(w, err, 500)
		return
	}
	if operation == "runtime" {
		chatMu.Unlock()
		if r.Method != http.MethodGet && !(len(parts) == 2 && parts[1] == "read") {
			writeError(w, errors.New("legacy dialogue runtime is read-only"), 403)
			return
		}
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		legacyAddress := c.address()
		legacyID := chatID(resourceID, user)
		if _, found := c.LegacySources[legacyID]; found {
			legacyAddress = dialoguePrefix + legacyID
		}
		switch parts[1] {
		case "read":
			m.server.handleSessionRead(w, r, workspace.Path, legacyAddress)
		case "history":
			m.handleResourceHistory(w, r, workspaceID, legacyAddress, parts[2:])
		case "status":
			m.handleResourceStatus(w, r, workspaceID, legacyAddress)
		case "events":
			m.handleResourceEvents(w, r, workspaceID, legacyAddress)
		case "stream":
			m.handleResourceStream(w, r, workspaceID, legacyAddress)
		case "approval":
			m.handleResourceApproval(w, r, workspaceID, legacyAddress)
		case "turn":
			if len(parts) == 3 && parts[2] == "end" {
				m.handleResourceEndTurn(w, r, workspaceID, legacyAddress)
			} else {
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
		return
	}
	if r.Method == http.MethodGet {
		chatMu.Unlock()
		if operation == "notes" && agent {
			writeJSON(w, map[string]string{"text": c.Notes})
			return
		}
		if operation != "" {
			http.NotFound(w, r)
			return
		}
		page, pageErr := m.historyPublicPage(r.Context(), workspace.Path, c, pageLimit, before, after, user)
		if position := r.URL.Query().Get("afterPosition"); after != "" && position != "" {
			previous, parseErr := strconv.Atoi(position)
			if parseErr != nil || previous < 0 {
				writeError(w, errors.New("invalid chat cursor position"), 400)
				return
			}
			if errors.Is(pageErr, errChatCursor) || (pageErr == nil && page.EndPosition-len(page.Messages) != previous) {
				page, pageErr = m.historyPublicPage(r.Context(), workspace.Path, c, pageLimit, "", "", user)
				page.Reset = true
			}
		}
		if pageErr != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(pageErr, errChatCursor) {
				status = http.StatusBadRequest
			}
			writeError(w, pageErr, status)
			return
		}
		c.addQueuedMessages(workspace.Path, &page)
		writeJSON(w, page)
		return
	}
	if r.Method != http.MethodPost {
		chatMu.Unlock()
		w.WriteHeader(405)
		return
	}
	if archived {
		chatMu.Unlock()
		writeError(w, errors.New("resource is archived"), 409)
		return
	}
	var body chatRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&body); err != nil {
		chatMu.Unlock()
		writeError(w, err, 400)
		return
	}
	agentOperation := operation == "publish" || operation == "dispatch" || operation == "notes" || operation == "timer"
	if agentOperation != agent {
		chatMu.Unlock()
		writeError(w, errors.New("operation not available to this caller"), 403)
		return
	}
	previousOpen := c.Open
	switch operation {
	case "open":
		if !c.Open {
			c.open()
		}
	case "close":
		if resourceID == "workspace" {
			err = errors.New("Workspace chat remains open")
		} else {
			c.Open = false
			c.Token = ""
			for id, source := range c.LegacySources {
				source.Token = ""
				source.Open = false
				c.LegacySources[id] = source
			}
			c.IntervalSeconds = 0
			c.NextCheckAt = ""
		}
	case "read":
		chatMu.Unlock()
		view, readErr := m.chatHistory(r.Context(), workspace.Path, c)
		if readErr != nil {
			writeError(w, readErr, http.StatusServiceUnavailable)
			return
		}
		chatMu.Lock()
		c, err = loadChatLocked(workspace.Path, c.ID)
		if err != nil {
			break
		}
		view.ReadCursors, view.LegacyUnread, view.HistoryReads = c.ReadCursors, c.LegacyUnread, c.HistoryReads
		positionMatches := body.Position == 0 || (body.Position > 0 && body.Position <= len(view.Messages) && view.Messages[body.Position-1].ID == body.ID)
		if positionMatches && view.markRead(user, body.ID) {
			c.ReadCursors, c.LegacyUnread, c.HistoryReads = view.ReadCursors, view.LegacyUnread, view.HistoryReads
			err = saveChatLocked(workspace.Path, c)
		}
		chatMu.Unlock()
		if err != nil {
			writeError(w, err, 500)
			return
		}
		view.Open, view.Epoch = c.Open, c.Epoch
		page, _ := view.publicPage(pageLimit, "", "", user)
		writeJSON(w, page)
		return
	case "messages":
		err = c.appendUserInput(body.ID, body.Text, user)
	case "publish":
		works, workErr := loadTurnWork(workspace.Path, resourceID)
		if workErr != nil {
			err = workErr
			break
		}
		for _, work := range works.Works {
			if work.Context.ChatVisible && work.Context.ChatEpoch == c.Epoch && work.TurnID != "" && !work.OutputDone {
				err = errors.New("this Turn publishes ordinary text automatically; do not call chat publish")
				break
			}
		}
		if err != nil {
			break
		}
		publishID := body.ID
		if identity.ID != c.ID {
			// Preserve retries from a pre-migration capability even when IDs
			// from different users collided during the merge.
			matched := false
			for _, message := range c.Messages {
				if (message.LegacyChatID == identity.ID && message.LegacyID == body.ID) ||
					(message.PublishOrigin == identity.ID && message.ClientID == body.ID) {
					body.ID = message.ID
					matched = true
					break
				}
			}
			if !matched {
				for _, message := range c.Messages {
					if message.ID == body.ID && message.LegacyChatID != identity.ID {
						body.ID = "legacy-" + chatID(identity.ID, body.ID)
						break
					}
				}
			}
		}
		previousLength := len(c.Messages)
		if strings.HasPrefix(body.ID, "auto-") {
			err = errors.New("auto- message IDs are reserved for Turn output")
			break
		}
		err = c.appendMessage(body.ID, "agent", body.Text)
		if err == nil && identity.ID != c.ID && len(c.Messages) > previousLength {
			c.Messages[len(c.Messages)-1].PublishOrigin = identity.ID
			c.Messages[len(c.Messages)-1].ClientID = publishID
		}
	case "notes":
		c.Notes = body.Text
	case "dispatch", "timer", "binding":
		err = errors.New("independent dialogue runtime has been retired")

	default:
		err = errors.New("unknown chat operation")
	}
	if err == nil && previousOpen != c.Open {
		c.Revision++
		c.StateNotice = nil
		if noticeWorkID != "" {
			c.StateNotice = &chatStateNotice{Revision: c.Revision, OriginWorkID: noticeWorkID, AllowBoundary: noticeBoundary}
		}
	}
	if err == nil {
		err = saveChatLocked(workspace.Path, c)
	}
	chatMu.Unlock()
	if err != nil {
		writeError(w, err, 409)
		return
	}
	if operation == "messages" || operation == "close" || operation == "open" {
		m.requestChatInput(workspace, resourceID, chatInputTransfer)
	}
	if previousOpen != c.Open {
		m.requestTurnOutput(workspace, resourceID)
	}
	// All effects are replayed from durable state by the reconciler. Closing
	// revokes publication without interrupting execution.
	m.requestReconcile(reconcileMailboxes | reconcileAgentHub)
	page, _ := c.publicPage(pageLimit, "", "", user)
	c.addQueuedMessages(workspace.Path, &page)
	if unread, _, activityErr := chatHistoryActivity(workspace.Path, c, user); activityErr == nil {
		page.UnreadCount += unread
	}
	writeJSON(w, page)
}

func (m *agentManager) reconcileWorkspaceChats(ctx context.Context, workspace serveWorkspace) error {
	// Explicit migration policy: old Inbox contents are discarded, never
	// interpreted as public chat or used to reconstruct a conversation.
	if err := discardLegacyInbox(workspace.Path); err != nil {
		return err
	}
	ws, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		return err
	}
	users, err := ws.Users()
	if err != nil {
		return err
	}
	registered := make(map[string]bool, len(users))
	for _, user := range users {
		registered[user.Name] = true
	}
	chatMu.Lock()
	if err = migrateExistingChatsLocked(workspace.Path); err != nil {
		chatMu.Unlock()
		return err
	}
	c, e := ensureChatLocked(workspace.Path, "workspace", "")
	if e != nil {
		chatMu.Unlock()
		return e
	}
	if !c.Open {
		c.open()
		if e = saveChatLocked(workspace.Path, c); e != nil {
			chatMu.Unlock()
			return e
		}
	}
	chatMu.Unlock()
	paths, err := filepath.Glob(filepath.Join(workspacepath.ControlDir(workspace.Path), "chats", "*.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err = ctx.Err(); err != nil {
			return err
		}
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		_, loadErr := m.reconcileChatOutbox(ctx, workspace, id, registered)
		if loadErr != nil {
			return loadErr
		}
	}
	return nil
}

func (m *agentManager) reconcileChatOutbox(ctx context.Context, workspace serveWorkspace, id string, registered map[string]bool) (transferred int, resultErr error) {
	destinations := make(map[string]bool)
	defer func() {
		for resource := range destinations {
			m.requestChatInput(workspace, resource, chatInputMailbox)
		}
	}()
	chatMu.Lock()
	defer chatMu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	chat, err := loadChatLocked(workspace.Path, id)
	if err != nil {
		return 0, err
	}
	if chat.Version >= 4 {
		before, _ := json.Marshal(chat)
		chat.IntervalSeconds, chat.NextCheckAt = 0, ""
		for sourceID, source := range chat.LegacySources {
			if !registered[source.User] && (source.Open || source.Token != "") {
				source.Open, source.Token = false, ""
				chat.LegacySources[sourceID] = source
			}
		}
		_, archived, _, resourceErr := resourceExistsAndArchived(workspace.Path, chat.ResourceID)
		if resourceErr != nil {
			return 0, resourceErr
		}
		savedAt := make(map[string]string)
		for _, message := range chat.Messages {
			if message.Role == "user" {
				savedAt["input-"+message.ID] = message.CreatedAt
			}
		}
		for index := range chat.Outbox {
			delivery := &chat.Outbox[index]
			if delivery.Accepted {
				continue
			}
			if err := ctx.Err(); err != nil {
				return transferred, err
			}
			legacySourceValid := true
			if delivery.userInput() && delivery.SourceChatID != "" {
				source, found := chat.LegacySources[delivery.SourceChatID]
				legacySourceValid = found && source.Open && registered[source.User]
			}
			if delivery.Target == chat.address() || (delivery.userInput() && (!chat.Open || archived || delivery.Epoch != chat.Epoch || !legacySourceValid)) {
				delivery.Accepted = true
				continue
			}
			messageType := "chat_input"
			if delivery.userInput() {
				messageType = "user_chat_input"
			}
			sourceID := chat.ID
			if delivery.SourceChatID != "" {
				sourceID = delivery.SourceChatID
			}
			var sender *agentHubMessageSender
			if delivery.Sender != "" {
				sender = &agentHubMessageSender{Name: delivery.Sender}
			}
			cause := &resourceMessageCausation{Type: "chat", SourceResourceID: chat.ResourceID, MessageID: delivery.ID}
			if delivery.Automatic {
				cause.ChatEpoch, cause.AutomaticChat = delivery.Epoch, true
			}
			started := time.Now()
			message, acceptErr := acceptGeneratedMailboxMessage(workspace.Path, resourceMailboxMessage{
				ID: "chat-" + chatID(sourceID, delivery.ID), ResourceID: delivery.Target, Text: delivery.Text,
				Sender: sender, Type: messageType, RequestedMode: resourceMessageModeSteer, Causation: cause,
			})
			if acceptErr != nil {
				return transferred, acceptErr
			}
			delivery.Accepted = true
			destinations[delivery.Target] = true
			transferred++
			log.Printf("Chat input accepted workspace=%s resource=%s input=%s message=%s saved_at=%s accepted_at=%s accept_ms=%d",
				workspace.ID, delivery.Target, delivery.ID, message.ID, savedAt[delivery.ID], message.AcceptedAt, time.Since(started).Milliseconds())
		}
		after, _ := json.Marshal(chat)
		if !bytes.Equal(before, after) {
			if err := saveChatLocked(workspace.Path, chat); err != nil {
				return transferred, err
			}
		}
	}
	pending, err := mailboxPendingForResource(workspace.Path, chat.address())
	if err != nil {
		return transferred, err
	}
	if pending {
		return transferred, markResourceMailboxArchived(workspace.Path, chat.address())
	}
	return transferred, nil
}

func (m *agentManager) handleChats(w http.ResponseWriter, r *http.Request, workspaceID string) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	workspace, err := m.server.workspace(workspaceID)
	if err != nil {
		writeError(w, err, 404)
		return
	}
	user, err := m.server.workspaceUserName(r, workspace.Path)
	if err != nil {
		writeError(w, err, 400)
		return
	}
	type entry struct {
		ResourceID string `json:"resourceId"`
		Open       bool   `json:"open"`
		Unread     int    `json:"unread"`
		UpdatedAt  string `json:"updatedAt"`
	}
	entries := []entry{}
	chatMu.Lock()
	if err = migrateExistingChatsLocked(workspace.Path); err != nil {
		chatMu.Unlock()
		writeError(w, err, 500)
		return
	}
	paths, err := filepath.Glob(filepath.Join(workspacepath.ControlDir(workspace.Path), "chats", "*.json"))
	if err != nil {
		chatMu.Unlock()
		writeError(w, err, 500)
		return
	}
	for _, path := range paths {
		c, e := loadChatLocked(workspace.Path, strings.TrimSuffix(filepath.Base(path), ".json"))
		if e != nil {
			err = e
			break
		}
		if c.Version < 4 || c.Epoch == 0 {
			continue
		}
		item := entry{ResourceID: c.ResourceID, Open: c.Open}
		legacyUnread := c.legacyUnreadSet(user)
		for i, message := range c.Messages {
			item.UpdatedAt = message.CreatedAt
			if c.unread(user, i, message, legacyUnread) {
				item.Unread++
			}
		}
		unread, updated, activityErr := chatHistoryActivity(workspace.Path, c, user)
		if activityErr != nil {
			err = activityErr
			break
		}
		item.Unread += unread
		if generationTime(updated).After(generationTime(item.UpdatedAt)) {
			item.UpdatedAt = updated
		}
		entries = append(entries, item)
	}
	chatMu.Unlock()
	if err != nil {
		writeError(w, err, 500)
		return
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].UpdatedAt > entries[j].UpdatedAt })
	writeJSON(w, map[string]any{"chats": entries})
}

func discardLegacyInbox(root string) error {
	paths, err := filepath.Glob(filepath.Join(workspacepath.ControlDir(root), "users", "*", "inbox.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
