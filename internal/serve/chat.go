package serve

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/workspacepath"
)

type chatRequest struct {
	ID      string           `json:"id"`
	Text    string           `json:"text"`
	Target  string           `json:"target"`
	Seconds int              `json:"seconds"`
	Binding app.AgentBinding `json:"binding"`
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
	agent := r.Header.Get("X-PUA-Chat-Token") != ""
	var user string
	if agent {
		c, loadErr := loadDialogue(workspace.Path, dialoguePrefix+r.Header.Get("X-PUA-Chat"))
		if loadErr != nil || c.ResourceID != normalizedResourceID(resourceID) {
			writeError(w, errors.New("invalid dialogue identity"), 403)
			return
		}
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
	chatMu.Lock()
	c, err := ensureChatLocked(workspace.Path, resourceID, user)
	if err != nil {
		chatMu.Unlock()
		writeError(w, err, 500)
		return
	}
	if agent {
		rec, found, loadErr := currentResourceGeneration(workspace.Path, c.address())
		if loadErr != nil || !found || !c.Open || archived || rec.ChatEpoch != c.Epoch || rec.GenerationID != r.Header.Get("X-PUA-Chat-Generation") || subtle.ConstantTimeCompare([]byte(c.Token), []byte(r.Header.Get("X-PUA-Chat-Token"))) != 1 {
			chatMu.Unlock()
			writeError(w, errors.New("dialogue is closed or generation has changed"), 409)
			return
		}
	}
	if resourceID == "workspace" && c.Epoch == 0 {
		c.Open = true
		c.Epoch = 1
		c.Token = newGenerationRecordID()
		err = saveChatLocked(workspace.Path, c)
	}
	if err != nil {
		chatMu.Unlock()
		writeError(w, err, 500)
		return
	}
	if operation == "runtime" {
		chatMu.Unlock()
		if agent && r.Method != http.MethodGet {
			writeError(w, errors.New("runtime controls require a user"), 403)
			return
		}
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		switch parts[1] {
		case "read":
			m.server.handleSessionRead(w, r, workspace.Path, c.address(), user)
		case "history":
			m.handleResourceHistory(w, r, workspaceID, c.address(), parts[2:])
		case "status":
			m.handleResourceStatus(w, r, workspaceID, c.address())
		case "events":
			m.handleResourceEvents(w, r, workspaceID, c.address())
		case "stream":
			m.handleResourceStream(w, r, workspaceID, c.address())
		case "approval":
			m.handleResourceApproval(w, r, workspaceID, c.address())
		case "turn":
			if len(parts) == 3 && parts[2] == "end" {
				m.handleResourceEndTurn(w, r, workspaceID, c.address())
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
		writeJSON(w, c.public())
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
	switch operation {
	case "open":
		if !c.Open {
			c.Open = true
			c.Epoch++
			c.Token = newGenerationRecordID()
			c.NextCheckAt = ""
			c.IntervalSeconds = 0
		}
	case "close":
		if resourceID == "workspace" {
			err = errors.New("Workspace chat remains open")
		} else {
			c.Open = false
			c.Token = ""
			c.IntervalSeconds = 0
			c.NextCheckAt = ""
		}
	case "read":
		previous := c.ReadThrough
		for i, message := range c.Messages {
			if message.ID == body.ID && i+1 > c.ReadThrough {
				c.ReadThrough = i + 1
				break
			}
		}
		if previous == c.ReadThrough {
			chatMu.Unlock()
			writeJSON(w, c.public())
			return
		}
	case "messages":
		if !c.Open {
			err = errors.New("chat is closed")
			break
		}
		err = c.appendMessage(body.ID, "user", body.Text)
		if err == nil {
			err = c.queue("input-"+body.ID, c.address(), c.prompt("User message "+body.ID+" from "+c.User+":\n"+body.Text))
		}
	case "publish":
		err = c.appendMessage(body.ID, "agent", body.Text)
	case "dispatch":
		target := normalizedResourceID(body.Target)
		if body.Target == "" {
			target = c.ResourceID
		}
		if strings.HasPrefix(target, dialoguePrefix) {
			err = errors.New("dispatch requires an execution resource")
			break
		}
		var ok, closed bool
		ok, closed, _, err = resourceExistsAndArchived(workspace.Path, target)
		if err == nil && (!ok || closed) {
			err = errors.New("target is unavailable")
		}
		if err == nil && (body.ID == "" || strings.TrimSpace(body.Text) == "") {
			err = errors.New("id and text are required")
		}
		if err == nil {
			err = c.queue("dispatch-"+body.ID, target, fmt.Sprintf("Work relayed by the dialogue Agent for %s, user %s. Preserve the user's scope and constraints.\n\n%s", c.ResourceID, c.User, body.Text))
		}
	case "notes":
		c.Notes = body.Text
	case "timer":
		if body.Seconds != 0 && (body.Seconds < 60 || body.Seconds > 86400) {
			err = errors.New("timer must be 0 or 60..86400 seconds")
			break
		}
		c.IntervalSeconds = body.Seconds
		c.NextCheckAt = ""
		if body.Seconds > 0 {
			c.NextCheckAt = time.Now().Add(time.Duration(body.Seconds) * time.Second).UTC().Format(time.RFC3339Nano)
		}
	case "binding":
		var binding app.AgentBinding
		binding, err = app.NormalizeAgentBinding(body.Binding)
		if err == nil {
			c.Binding = binding
		}

	default:
		err = errors.New("unknown chat operation")
	}
	if err == nil {
		err = saveChatLocked(workspace.Path, c)
	}
	if err == nil && operation == "close" {
		err = markResourceMailboxArchived(workspace.Path, c.address())
	}
	chatMu.Unlock()
	if err != nil {
		writeError(w, err, 409)
		return
	}
	// All effects are replayed from durable state by the reconciler. Closing
	// revokes publication before the provider can observe the stop request.
	m.requestReconcile(reconcileMailboxes | reconcileAgentHub)
	if operation == "binding" {
		_ = m.enqueueResourceController(workspace, c.address(), func() error {
			return m.resourceBindingChangedLocked(context.Background(), workspace, c.address(), c.Binding)
		})
	}
	writeJSON(w, c.public())
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
	chatMu.Lock()
	for _, user := range users {
		registered[user.Name] = true
		c, e := ensureChatLocked(workspace.Path, "workspace", user.Name)
		if e != nil {
			chatMu.Unlock()
			return e
		}
		if c.Epoch == 0 {
			c.Open = true
			c.Epoch = 1
			c.Token = newGenerationRecordID()
			if e = saveChatLocked(workspace.Path, c); e != nil {
				chatMu.Unlock()
				return e
			}
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
		chatMu.Lock()
		c, loadErr := loadChatLocked(workspace.Path, id)
		if loadErr != nil {
			chatMu.Unlock()
			return loadErr
		}
		before, _ := json.Marshal(c)
		// Version 1 repeating timers retain their next appointment, but are
		// consumed once under the new semantics; no blanket model wakeup.
		c.Version = 2
		if !registered[c.User] {
			c.Open = false
			c.Token = ""
			c.IntervalSeconds = 0
			c.NextCheckAt = ""
		}
		_, archived, _, loadErr := resourceExistsAndArchived(workspace.Path, c.ResourceID)
		if loadErr != nil {
			chatMu.Unlock()
			return loadErr
		}
		if c.Open && !archived && c.IntervalSeconds > 0 && c.NextCheckAt != "" {
			due, parseErr := time.Parse(time.RFC3339Nano, c.NextCheckAt)
			if parseErr != nil {
				chatMu.Unlock()
				return parseErr
			}
			if !time.Now().Before(due) {
				pending, pendingErr := mailboxPendingForResource(workspace.Path, c.address())
				if pendingErr != nil {
					chatMu.Unlock()
					return pendingErr
				}
				for _, d := range c.Outbox {
					if d.Target == c.address() && !d.Accepted && d.Epoch == c.Epoch {
						pending = true
					}
				}
				rec, found, generationErr := currentResourceGeneration(workspace.Path, c.address())
				if generationErr != nil {
					chatMu.Unlock()
					return generationErr
				}
				if !pending && (!found || (rec.Status != "running" && rec.Status != "waiting_approval" && rec.Status != "starting")) {
					_ = c.queue("tick-"+c.NextCheckAt, c.address(), c.prompt("Scheduled one-time check. This timer is now off. Read your notes, query relevant work, and publish only useful new information. Arrange another check only if a concrete follow-up remains, choosing a delay based on expected progress. Otherwise leave the timer off."))
					c.IntervalSeconds = 0
					c.NextCheckAt = ""
				}
				// Keep a due appointment while busy; retry after the Turn/input
				// finishes instead of repeatedly adding steer messages.
			}
		}
		for i := range c.Outbox {
			d := &c.Outbox[i]
			if d.Accepted {
				continue
			}
			// Work durably dispatched before closure still belongs to execution.
			if d.Target == c.address() && (!c.Open || archived || d.Epoch != c.Epoch) {
				d.Accepted = true
				continue
			}
			sum := chatID(c.ID, d.ID)
			_, err = acceptGeneratedMailboxMessage(workspace.Path, resourceMailboxMessage{ID: "chat-" + sum, ResourceID: d.Target, Text: d.Text, Type: "chat_input", RequestedMode: resourceMessageModeSteer, Causation: &resourceMessageCausation{Type: "chat", SourceResourceID: c.ResourceID, MessageID: d.ID}})
			if err != nil {
				chatMu.Unlock()
				return err
			}
			d.Accepted = true
		}
		after, _ := json.Marshal(c)
		if !bytes.Equal(before, after) {
			err = saveChatLocked(workspace.Path, c)
		}
		chatMu.Unlock()
		if err != nil {
			return err
		}
		// Once queued, join this short controller operation even on shutdown.
		// The effects still use ctx for network cancellation; returning before
		// the closure finishes would leave untracked writes after shutdown.
		if err = m.withResourceController(context.WithoutCancel(ctx), workspace, c.address(), func() error {
			latest, loadErr := loadDialogue(workspace.Path, c.address())
			if loadErr != nil {
				return loadErr
			}
			rec, found, loadErr := currentResourceGeneration(workspace.Path, c.address())
			if loadErr != nil || !found {
				return loadErr
			}
			if latest.Open && !archived && rec.ChatEpoch == latest.Epoch {
				return nil
			}
			cfg, client, cfgErr := m.agentHubRuntimeConfig()
			if cfgErr != nil {
				return cfgErr
			}
			rt := m.ensureRuntime(workspace, rec, client)
			updated, persistErr := rt.mutateGeneration(func(record *generationRecord) { record.ArchivedTaskStopRequested = true })
			if persistErr != nil {
				return persistErr
			}
			if updated.AgentHubSessionID != "" {
				session, fetchErr := client.GetSession(ctx, updated.AgentHubSessionID)
				if fetchErr != nil {
					return fetchErr
				}
				m.stopAgentHubSessionForArchivedResource(ctx, cfg, workspace, updated, session, client)
			}
			return nil
		}); err != nil {
			return err
		}
		if !c.Open || archived {
			_ = markResourceMailboxArchived(workspace.Path, c.address())
		}
	}
	return nil
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
	paths, err := filepath.Glob(filepath.Join(workspacepath.ControlDir(workspace.Path), "chats", "*.json"))
	if err != nil {
		writeError(w, err, 500)
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
	for _, path := range paths {
		c, e := loadChatLocked(workspace.Path, strings.TrimSuffix(filepath.Base(path), ".json"))
		if e != nil {
			err = e
			break
		}
		if c.User != user || c.Epoch == 0 {
			continue
		}
		item := entry{ResourceID: c.ResourceID, Open: c.Open}
		for i, message := range c.Messages {
			item.UpdatedAt = message.CreatedAt
			if i >= c.ReadThrough && message.Role == "agent" {
				item.Unread++
			}
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
