package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/disksing/pua/agenthub/protocol"
	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/localize"
	"github.com/disksing/pua/internal/workspacepath"
)

// All mutations are serialized by the resource controller. This is PUA work
// state, not another copy of AgentHub's canonical Turn history.
type turnWork struct {
	ID                   string                            `json:"id"`
	GenerationID         string                            `json:"generationId"`
	SessionID            string                            `json:"sessionId"`
	TurnID               string                            `json:"turnId,omitempty"`
	Context              providerMessageContext            `json:"context"`
	State                string                            `json:"state"`
	History              string                            `json:"history,omitempty"`
	ParentID             string                            `json:"parentId,omitempty"`
	TargetID             string                            `json:"targetId,omitempty"`
	Checkpoint           string                            `json:"checkpoint,omitempty"`
	ResumeID             string                            `json:"resumeId,omitempty"`
	Outcome              string                            `json:"outcome,omitempty"`
	LastError            string                            `json:"lastError,omitempty"`
	OutputDone           bool                              `json:"outputDone,omitempty"`
	CreatedAt            string                            `json:"createdAt,omitempty"`
	TerminalObservedAt   string                            `json:"terminalObservedAt,omitempty"`
	LastPublishedAt      string                            `json:"lastPublishedAt,omitempty"`
	PublishedIDs         []string                          `json:"publishedIds,omitempty"`
	NotificationAccepted bool                              `json:"notificationAccepted,omitempty"`
	Command              *turnWorkCommand                  `json:"command,omitempty"`
	Notification         *resourceMailboxMessage           `json:"notification,omitempty"`
	Previews             map[string]bool                   `json:"previews,omitempty"`
	GroupedPreviews      map[string]string                 `json:"groupedPreviews,omitempty"`
	PreviewAttempts      map[string]agentHubInboundMessage `json:"previewAttempts,omitempty"`
	PreviewErrors        map[string]string                 `json:"previewErrors,omitempty"`
}

type turnWorkFile struct {
	Version    int            `json:"version"`
	ResourceID string         `json:"resourceId"`
	Works      []turnWork     `json:"works"`
	Advisories []turnAdvisory `json:"advisories,omitempty"`
}

type turnWorkCommand struct {
	Action     string `json:"action"`
	WorkID     string `json:"workId"`
	MessageID  string `json:"messageId,omitempty"`
	Checkpoint string `json:"checkpoint,omitempty"`
	Text       string `json:"text,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

func turnWorkPath(root, resource string) string {
	return filepath.Join(workspacepath.ControlDir(root), "turn-work", sharedChatID(resource)+".json")
}

func loadTurnWork(root, resource string) (turnWorkFile, error) {
	file := turnWorkFile{Version: 1, ResourceID: resource, Works: []turnWork{}}
	data, err := os.ReadFile(turnWorkPath(root, resource))
	if errors.Is(err, os.ErrNotExist) {
		return file, nil
	}
	if err != nil {
		return file, err
	}
	if err = json.Unmarshal(data, &file); err == nil && file.Version != 1 {
		err = errors.New("unsupported Turn work version")
	}
	return file, err
}

func saveTurnWork(root, resource string, file turnWorkFile) error {
	path := turnWorkPath(root, resource)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return saveJSONStateFile(path, ".turn-work-*.tmp", file)
}

func (f turnWorkFile) find(id string) *turnWork {
	for i := range f.Works {
		if f.Works[i].ID == id {
			return &f.Works[i]
		}
	}
	return nil
}

func (f *turnWorkFile) active(session, turn string) *turnWork {
	for i := len(f.Works) - 1; i >= 0; i-- {
		w := &f.Works[i]
		if w.SessionID == session && w.TurnID == turn && turn != "" && !w.OutputDone {
			return w
		}
	}
	return nil
}

func (f turnWorkFile) canPreview(work *turnWork, messageID string) bool {
	if work == nil {
		return false
	}
	for _, advisory := range f.Advisories {
		if advisory.Type == "queue_preview" && advisory.MessageID == messageID {
			return false
		}
	}
	for _, previous := range f.Works {
		if previous.Previews[messageID] || previous.GroupedPreviews[messageID] != "" {
			return false
		}
		if _, attempted := previous.PreviewAttempts[messageID]; attempted && previous.ID != work.ID {
			return false
		}
	}
	return true
}

func prepareTurnWork(root, resource, generation, session string, message resourceMailboxMessage) error {
	if message.ProviderContext == nil || message.ProviderContext.Protocol != 1 || message.ActualMode == resourceMessageModeSteer {
		return nil
	}
	file, err := loadTurnWork(root, resource)
	if err != nil {
		return err
	}
	if existing := file.find(message.ID); existing != nil {
		if existing.TurnID == "" && message.Status == resourceMessageQueued &&
			(existing.SessionID != session || !reflect.DeepEqual(existing.Context, *message.ProviderContext)) {
			existing.GenerationID, existing.SessionID = generation, session
			existing.Context = *message.ProviderContext
			return saveTurnWork(root, resource, file)
		}
		return nil
	}
	ws, err := app.OpenWorkspace(root)
	if err != nil {
		return err
	}
	if err := ws.RefreshTurnGuidance(resource); err != nil {
		return err
	}
	work := turnWork{ID: message.ID, GenerationID: generation, SessionID: session,
		Context: *message.ProviderContext, State: "running", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	for _, parent := range file.Works {
		if parent.TargetID == message.ID && parent.State == "suspended" {
			work.ParentID = parent.ID
			break
		}
	}
	file.Works = append(file.Works, work)
	return saveTurnWork(root, resource, file)
}

func bindTurnWork(root, resource, id, turn string) error {
	file, err := loadTurnWork(root, resource)
	if err != nil {
		return err
	}
	w := file.find(id)
	if w == nil || w.TurnID == turn || turn == "" {
		return nil
	}
	w.TurnID = turn
	return saveTurnWork(root, resource, file)
}

func syncTurnChat(root, resource string, work *turnWork, turn agentHubTurn) error {
	if !work.Context.ChatVisible {
		return nil
	}
	chatMu.Lock()
	defer chatMu.Unlock()
	chat, err := loadChatLocked(root, sharedChatID(resource))
	if err != nil {
		return err
	}
	if !chat.Open || chat.Epoch != work.Context.ChatEpoch {
		work.LastError = "Chat epoch closed; output retained only in execution history"
		return nil
	}
	changed := false
	sender, senderResolved := "", false
	var published []string
	for i, item := range turn.Items {
		if item.Type != "message" || item.Role != "assistant" || strings.TrimSpace(item.Text) == "" {
			continue
		}
		// A completed block, a subsequent item, or the terminal Turn seals the
		// text. Re-reading the projection handles merged same-cursor deltas.
		if !item.Complete && !turn.Closed && i == len(turn.Items)-1 {
			continue
		}
		id := "auto-" + chatID(work.SessionID, fmt.Sprintf("%s:%d", work.TurnID, item.StartEventID))
		oldLen := len(chat.Messages)
		if err := chat.appendMessage(id, "agent", item.Text); err != nil {
			return err
		}
		if len(chat.Messages) != oldLen {
			if !senderResolved {
				sender = turnChatSender(root, resource, *work)
				senderResolved = true
			}
			chat.Messages[len(chat.Messages)-1].Sender = sender
		}
		changed = changed || len(chat.Messages) != oldLen
		published = append(published, id)
	}
	if changed {
		if err := saveChatLocked(root, chat); err != nil {
			return err
		}
		work.LastPublishedAt = chat.Messages[len(chat.Messages)-1].CreatedAt
	}
	work.PublishedIDs = published
	work.LastError = ""
	return nil
}

func turnWorkReference(root, resource string, work turnWork) (string, error) {
	instance, err := workspaceInstanceID(root)
	if err != nil {
		return "", err
	}
	return encodeResourceHistoryReference(resourceHistoryReference{
		Kind: "turn", InstanceID: instance, ResourceID: resource,
		GenerationID: work.GenerationID, TurnID: work.TurnID,
	})
}

func isMailboxReceiptExpired(err error) bool {
	var apiErr *resourceAPIError
	return errors.As(err, &apiErr) && apiErr.Code == "message_receipt_expired"
}

func (w *turnWork) recordExpiredInput(err error) {
	if w.State != "cancelled" {
		w.State, w.Outcome = "blocked", "mailbox receipt expired"
	}
	w.LastError = err.Error()
}

func recoverDeliveredWorkTurn(ctx context.Context, root string, client *agentHubClient, work *turnWork, message resourceMailboxMessage) (string, error) {
	if message.AgentHubSessionID != "" && message.AgentHubSessionID != work.SessionID {
		return "", fmt.Errorf("delivery belongs to another AgentHub session: %s", message.AgentHubSessionID)
	}
	if message.TurnID != "" {
		return message.TurnID, nil
	}
	turnID, err := findMailboxOpenerTurn(ctx, client, work.SessionID, work.ID)
	if err != nil || turnID == "" {
		return "", err
	}
	_, err = updateMailboxMessage(root, work.ID, func(current *resourceMailboxMessage) {
		current.TurnID = turnID
	})
	return turnID, err
}

func (m *agentManager) reconcileTurnWorkLocked(ctx context.Context, workspace serveWorkspace, resource string, client *agentHubClient) error {
	file, err := loadTurnWork(workspace.Path, resource)
	if err != nil {
		return err
	}
	defer m.refreshTurnOutputTarget(workspace, resource, false)
	if len(file.Works) == 0 && len(file.Advisories) == 0 {
		return nil
	}
	before, _ := json.Marshal(file)
	_, archived, _, err := resourceExistsAndArchived(workspace.Path, resource)
	if err != nil {
		return err
	}
	paused := false
	if detail, task, e := taskDetail(workspace.Path, resource); e == nil && task {
		paused = detail.State == app.TaskStatePaused
	}
	if archived || paused {
		if err := cancelSuspendedTurnWork(workspace.Path, resource, "resource paused or archived"); err != nil {
			return err
		}
		file, err = loadTurnWork(workspace.Path, resource)
		if err != nil {
			return err
		}
	}
	if err := m.reconcileAdvisories(ctx, workspace, resource, &file, client, !archived && !paused); err != nil {
		return err
	}
	for i := range file.Works {
		work := &file.Works[i]
		if archived || paused {
			if work.State == "yield_requested" || work.State == "suspended" {
				work.State, work.Outcome = "cancelled", "resource paused or archived"
			}
		}
		if work.OutputDone {
			continue
		}
		if work.TurnID == "" {
			message, found, e := mailboxMessageByID(workspace.Path, work.ID)
			if isMailboxReceiptExpired(e) {
				// Only terminal receipts expire. With no recorded Turn ID,
				// this work has no output to reconcile through the mailbox.
				work.recordExpiredInput(e)
				work.OutputDone = true
				continue
			}
			if e != nil {
				return e
			}
			if found {
				if message.Status == resourceMessageDelivered {
					turnID, err := recoverDeliveredWorkTurn(ctx, workspace.Path, client, work, message)
					if err != nil {
						work.LastError = err.Error()
						continue
					}
					work.TurnID = turnID
				}
				if work.TurnID == "" && (message.Status == resourceMessageCancelled ||
					message.Status == resourceMessageUndeliverable || message.Status == resourceMessageDeliveryUnknown) {
					work.OutputDone, work.State, work.LastError = true, "blocked", "input ended without a confirmed Turn: "+message.Status
					continue
				}
			}
			if work.TurnID == "" {
				continue
			}
		}
		queryStarted := time.Now()
		turn, _, e := client.SessionTurn(ctx, work.SessionID, work.TurnID)
		queryElapsed := time.Since(queryStarted)
		if e != nil {
			work.LastError = e.Error()
			log.Printf("Turn output query workspace=%s resource=%s work=%s query_ms=%d error=%v",
				workspace.ID, resource, work.ID, queryElapsed.Milliseconds(), e)
			continue
		}
		if queryElapsed >= time.Second {
			log.Printf("Turn output query workspace=%s resource=%s work=%s query_ms=%d",
				workspace.ID, resource, work.ID, queryElapsed.Milliseconds())
		}
		if turn.Closed && work.TerminalObservedAt == "" {
			work.TerminalObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
		work.History, e = turnWorkReference(workspace.Path, resource, *work)
		if e != nil {
			return e
		}
		if !archived {
			previousPublishedAt := work.LastPublishedAt
			if e = syncTurnChat(workspace.Path, resource, work, turn); e != nil {
				work.LastError = e.Error()
				continue
			}
			if work.LastPublishedAt != previousPublishedAt {
				lag := time.Duration(0)
				if terminalAt := generationTime(turn.CompletedAt); turn.Closed && !terminalAt.IsZero() {
					lag = generationTime(work.LastPublishedAt).Sub(terminalAt)
				}
				log.Printf("Turn output published workspace=%s resource=%s work=%s turn=%s query_ms=%d terminal_at=%s observed_at=%s published_at=%s terminal_lag_ms=%d",
					workspace.ID, resource, work.ID, work.TurnID, queryElapsed.Milliseconds(),
					turn.CompletedAt, work.TerminalObservedAt, work.LastPublishedAt, lag.Milliseconds())
			}
		}
		if !turn.Closed {
			continue
		}
		work.OutputDone = true
		switch {
		case work.State == "cancelled":
		case turn.Status != "completed":
			work.State, work.Outcome = "blocked", turn.Status
		case work.Context.ChatVisible && work.LastError != "":
			work.State, work.Outcome = "blocked", "Chat output unavailable"
		case work.State == "yield_requested":
			work.State = "suspended"
		default:
			work.State = "completed"
			if work.Outcome == "" {
				work.Outcome = "completed"
			}
		}
	}
	// Persist terminal facts before enqueueing side effects. Each generated
	// input has a stable id, so a crash after acceptance is safe to replay.
	if after, _ := json.Marshal(file); string(before) != string(after) {
		if err = saveTurnWork(workspace.Path, resource, file); err != nil {
			return err
		}
		m.requestReconcile(reconcileMailboxes | reconcileNotifications | reconcileScheduler)
	}
	for i := range file.Works {
		parent := &file.Works[i]
		if parent.State == "resuming" {
			resumed := file.find(parent.ResumeID)
			if resumed == nil {
				_, _, e := mailboxMessageByID(workspace.Path, parent.ResumeID)
				if isMailboxReceiptExpired(e) {
					parent.recordExpiredInput(e)
				} else if e != nil {
					return e
				}
			} else if resumed.OutputDone && (resumed.State == "completed" || resumed.State == "cancelled" || resumed.State == "blocked") {
				parent.State, parent.Outcome = resumed.State, resumed.Outcome
			}
		}
		if parent.Notification != nil && !parent.NotificationAccepted && (parent.State == "suspended" || parent.State == "completed") {
			if parent.State == "suspended" {
				parent.Notification.ProviderContext.ParentWork = parent.ID
				parent.Notification.ProviderContext.ParentHistory = parent.History
			}
			if _, err = acceptGeneratedMailboxMessage(workspace.Path, *parent.Notification); err != nil {
				return err
			}
			parent.NotificationAccepted = true
		}
		if parent.State != "suspended" {
			continue
		}
		child := file.find(parent.TargetID)
		if child == nil {
			message, found, e := mailboxMessageByID(workspace.Path, parent.TargetID)
			if isMailboxReceiptExpired(e) {
				parent.recordExpiredInput(e)
				continue
			}
			if e != nil {
				return e
			}
			if found && (message.Status == resourceMessageCancelled || message.Status == resourceMessageUndeliverable || message.Status == resourceMessageDeliveryUnknown) {
				parent.State, parent.LastError = "blocked", "selected input was not delivered: "+message.Status
			}
			continue
		}
		if child.State == "blocked" || child.State == "cancelled" {
			parent.State, parent.LastError = "blocked", "selected input ended without successful completion"
			continue
		}
		if child.State != "completed" || !child.OutputDone {
			continue
		}
		resumeID := "resume-" + chatID(parent.ID, child.ID)
		resumeContext := parent.Context
		resumeContext.WorkID = resumeID
		text := localize.MustRender(parent.Context.Language, "turn-resume.md", map[string]string{
			"Checkpoint": parent.Checkpoint, "ParentHistory": parent.History, "ChildHistory": child.History,
		})
		resume := resourceMailboxMessage{ID: resumeID, ResourceID: resource, Text: text, Type: "turn_resume",
			RequestedMode: resourceMessageModeEnqueue, ProviderContext: &resumeContext,
			Causation: &resourceMessageCausation{Type: "turn_resume", SourceResourceID: resource, MessageID: parent.ID, TurnReference: parent.History}}
		if _, err = acceptGeneratedMailboxMessage(workspace.Path, resume); err != nil {
			return err
		}
		parent.State, parent.ResumeID = "resuming", resumeID
	}
	if after, _ := json.Marshal(file); string(before) != string(after) {
		if err := saveTurnWork(workspace.Path, resource, file); err != nil {
			return err
		}
	}
	return settleTurnTaskState(workspace.Path, resource, file)
}

func selectedTurnWorkInput(root, resource string, mailbox resourceMailbox) (string, error) {
	file, err := loadTurnWork(root, resource)
	if err != nil {
		return "", err
	}
	for i := len(file.Works) - 1; i >= 0; i-- {
		work := file.Works[i]
		if work.State == "suspended" {
			return work.TargetID, nil
		}
		if work.State == "resuming" {
			for _, message := range mailbox.Messages {
				if message.ID == work.ResumeID && message.Status == resourceMessageQueued {
					return work.ResumeID, nil
				}
			}
		}
	}
	return "", nil
}

func (m *agentManager) previewQueuedInput(ctx context.Context, workspace serveWorkspace, session agentHubSession, client *agentHubClient, message resourceMailboxMessage) error {
	file, err := loadTurnWork(workspace.Path, message.ResourceID)
	if err != nil {
		return err
	}
	work := file.active(session.ID, session.CurrentTurnID)
	if session.Provider == "pi" && session.InputCapabilities.Steer && !session.InputCapabilities.TurnScopedSteer {
		if !file.canPreview(work, message.ID) || file.advisoryUnowned() {
			return nil
		}
		current, err := client.GetSession(ctx, session.ID)
		if err != nil {
			return err
		}
		if current.CurrentTurnID != session.CurrentTurnID {
			return nil
		}
		mailbox, err := loadHotResourceMailbox(workspace.Path, message.ResourceID)
		if err != nil {
			return err
		}
		if err := m.reserveAdvisory(ctx, workspace, message.ResourceID, &file, current, client, *work,
			"preview-"+chatID(work.ID, message.ID), "queue_preview", message.ID,
			queuePreviewRenderer(&file, work, current, message, mailbox)); err != nil {
			return err
		}
		return saveTurnWork(workspace.Path, message.ResourceID, file)
	}
	if !session.InputCapabilities.TurnScopedSteer || !file.canPreview(work, message.ID) {
		return nil
	}
	status, err := client.Status(ctx)
	if err != nil {
		return err
	}
	scoped := false
	for _, capability := range status.Capabilities {
		scoped = scoped || capability == protocol.CapabilityTurnScopedSteer
	}
	if !scoped {
		return nil
	}
	input, attempted := work.PreviewAttempts[message.ID]
	if !attempted {
		mailbox, err := loadHotResourceMailbox(workspace.Path, message.ResourceID)
		if err != nil {
			return err
		}
		text, err := renderWorkspaceAdvisory(workspace, queuePreviewRenderer(&file, work, session, message, mailbox))
		if err != nil {
			return err
		}
		payload, err := marshalPUAMessagePayload(puaMessagePayload{
			Schema: puaMessagePayloadSchema, Text: text, Role: "system", Type: "queue_preview",
			Causation: &resourceMessageCausation{Type: "queue_preview", SourceResourceID: message.ResourceID, MessageID: message.ID},
		})
		if err != nil {
			return err
		}
		input = agentHubInboundMessage{
			SchemaVersion: 2, Steer: true, ExpectedTurnID: session.CurrentTurnID,
			MessageID: "preview-" + chatID(work.ID, message.ID), Text: text, Payload: payload,
		}
		if work.PreviewAttempts == nil {
			work.PreviewAttempts = map[string]agentHubInboundMessage{}
		}
		// Reserve before sending. An unknown outcome may retry this exact input
		// in its original Turn, but cannot replay the queue into a later Turn.
		work.PreviewAttempts[message.ID] = input
		if err := saveTurnWork(workspace.Path, message.ResourceID, file); err != nil {
			return err
		}
	}
	_, err = client.Message(ctx, session.ID, input)
	if err != nil {
		if work.PreviewErrors == nil {
			work.PreviewErrors = map[string]string{}
		}
		work.PreviewErrors[message.ID] = err.Error()
		return saveTurnWork(workspace.Path, message.ResourceID, file)
	}
	if work.Previews == nil {
		work.Previews = map[string]bool{}
	}
	work.Previews[message.ID] = true
	delete(work.PreviewErrors, message.ID)
	return saveTurnWork(workspace.Path, message.ResourceID, file)
}

func (m *agentManager) handleTurnWork(w http.ResponseWriter, r *http.Request, workspaceID, resource string) {
	workspace, err := m.server.workspace(workspaceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	if err := m.server.requireWorkspaceOwnership(workspace.Path); err != nil {
		writeError(w, err, http.StatusConflict)
		return
	}
	if exists, archived, _, err := resourceExistsAndArchived(workspace.Path, resource); err != nil || !exists || archived {
		writeError(w, errors.New("resource is unavailable or archived"), http.StatusConflict)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var command turnWorkCommand
	if r.Method == http.MethodPost {
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&command); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
	}
	var result any
	err = m.withResourceController(r.Context(), workspace, resource, func() error {
		file, e := loadTurnWork(workspace.Path, resource)
		if e != nil {
			return e
		}
		if r.Method == http.MethodGet {
			if r.URL.Query().Get("chat") == "true" {
				work := file.find(r.URL.Query().Get("work"))
				if work == nil || !work.Context.ChatVisible {
					return errors.New("work has no shared Chat audience")
				}
				chatMu.Lock()
				defer chatMu.Unlock()
				chat, err := loadChatLocked(workspace.Path, sharedChatID(resource))
				if err != nil {
					return err
				}
				if !chat.Open || chat.Epoch != work.Context.ChatEpoch {
					return errors.New("Chat epoch closed")
				}
				result, err = chat.publicPage(40, "", "", "")
				return err
			}
			chat, err := readTurnChatSnapshot(workspace.Path, resource)
			if err != nil {
				return err
			}
			publicFile := file
			publicFile.Advisories = append([]turnAdvisory(nil), file.Advisories...)
			for index := range publicFile.Advisories {
				publicFile.Advisories[index].Input = nil
			}
			result = struct {
				turnWorkFile
				Chat           turnChatSnapshot   `json:"chat"`
				CurrentControl turnCurrentControl `json:"currentControl"`
			}{publicFile, chat, m.currentTurnControl(r.Context(), workspace, resource, file)}
			return nil
		}
		result, e = m.controlTurnWork(r.Context(), workspace, resource, &file, command)
		return e
	})
	if err != nil {
		writeError(w, err, http.StatusConflict)
		return
	}
	m.requestReconcile(reconcileMailboxes | reconcileAgentHub)
	writeJSON(w, result)
}

func yieldInputSupported(message resourceMailboxMessage) bool {
	return (message.ProviderContext == nil || message.ProviderContext.Protocol == 1) &&
		(message.Type != "user_chat_input" || message.Causation != nil && message.Causation.AutomaticChat)
}

func (m *agentManager) controlTurnWork(ctx context.Context, workspace serveWorkspace, resource string, file *turnWorkFile, command turnWorkCommand) (any, error) {
	work := file.find(command.WorkID)
	if work == nil {
		return nil, errors.New("unknown work; use the work id from this Turn's opener")
	}
	record, found, err := currentResourceGeneration(workspace.Path, resource)
	if err != nil {
		return nil, err
	}
	if !found || record.AgentHubSessionID != work.SessionID || record.GenerationID != work.GenerationID {
		return nil, errors.New("work is no longer in the current generation")
	}
	_, client, err := m.agentHubRuntimeConfig()
	if err != nil {
		return nil, err
	}
	session, err := client.GetSession(ctx, work.SessionID)
	if err != nil {
		return nil, err
	}
	if work.TurnID == "" || session.CurrentTurnID != work.TurnID || work.OutputDone ||
		(session.State != "running" && session.State != "waiting_approval") {
		return nil, errors.New("work is not the active Turn")
	}
	turn, _, err := client.SessionTurn(ctx, work.SessionID, work.TurnID)
	if err != nil || turn.Closed {
		return nil, errors.New("work is not a verified active Turn")
	}
	if work.Command != nil {
		if reflect.DeepEqual(*work.Command, command) {
			return map[string]string{"status": "accepted", "next": "End this Turn now; the server will schedule the next input."}, nil
		}
		return nil, errors.New("this Turn already has a different control request")
	}
	if len(session.PendingApprovalIDs) != 0 {
		return nil, errors.New("resolve outstanding approvals before ending this work")
	}
	switch command.Action {
	case "yield":
		if work.ParentID != "" {
			return nil, errors.New("nested yields are not supported; finish this input before returning to suspended work")
		}
		if strings.TrimSpace(command.Checkpoint) == "" {
			return nil, errors.New("yield requires a checkpoint")
		}
		message, exists, e := mailboxMessageByID(workspace.Path, command.MessageID)
		if e != nil {
			return nil, e
		}
		if !exists || message.ResourceID != resource || message.Status != resourceMessageQueued ||
			message.RequestedMode == resourceMessageModeInterrupt {
			return nil, errors.New("select a queued message belonging to this resource")
		}
		if !yieldInputSupported(message) {
			return nil, errors.New("legacy input cannot own a continuation; finish current work before processing it")
		}
		work.State, work.TargetID, work.Checkpoint = "yield_requested", message.ID, command.Checkpoint
	case "notify":
		if work.Context.ChatVisible || work.ParentID != "" || strings.TrimSpace(command.Text) == "" {
			return nil, errors.New("notify requires an internal root Turn and a notification intent")
		}
		chatMu.Lock()
		chat, e := loadChatLocked(workspace.Path, sharedChatID(resource))
		chatMu.Unlock()
		if e != nil {
			return nil, e
		}
		if !chat.Open {
			return nil, errors.New("Chat is closed")
		}
		id := "notify-" + chatID(work.ID, "notify")
		c := providerMessageContext{Protocol: 1, WorkID: id, Language: work.Context.Language, OpenerRole: "system", ChatVisible: true, ChatEpoch: chat.Epoch}
		work.Notification = &resourceMailboxMessage{ID: id, ResourceID: resource, Type: "chat_notification",
			Text:          "Proactive notification to the shared Chat. Explain this update:\n" + command.Text,
			RequestedMode: resourceMessageModeEnqueue, ProviderContext: &c,
			Causation: &resourceMessageCausation{Type: "chat_notification", SourceResourceID: resource, MessageID: work.ID, ChatEpoch: chat.Epoch, AutomaticChat: true}}
		if strings.TrimSpace(command.Checkpoint) != "" {
			work.State, work.TargetID, work.Checkpoint = "yield_requested", id, command.Checkpoint
		}
	case "finish":
		if command.Reason != "completed" && command.Reason != "cancelled" {
			return nil, errors.New("finish reason must be completed or cancelled")
		}
		work.Outcome = command.Reason
		if command.Reason == "cancelled" {
			work.State = "cancelled"
		}
	case "cancel-resume":
		parent := file.find(work.ParentID)
		if parent == nil || parent.State != "suspended" {
			return nil, errors.New("no suspended parent work")
		}
		parent.State, parent.Outcome = "cancelled", command.Reason
	default:
		return nil, errors.New("unknown Turn action")
	}
	work.Command = &command
	if err := saveTurnWork(workspace.Path, resource, *file); err != nil {
		return nil, err
	}
	return map[string]string{"status": "accepted", "next": "End this Turn now; the server will schedule the next input."}, nil
}

// Poll work independently of the hot mailbox: a running Turn may have consumed
// its last input, and a suspended parent must survive resource generation changes.
func (m *agentManager) reconcileWorkspaceTurnWork(ctx context.Context, workspace serveWorkspace, client *agentHubClient) error {
	paths, err := filepath.Glob(filepath.Join(workspacepath.ControlDir(workspace.Path), "turn-work", "*.json"))
	if err != nil {
		return err
	}
	var failures []error
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		// Resource identity comes from persisted mailbox inputs, never filenames.
		data, e := os.ReadFile(path)
		if e != nil {
			failures = append(failures, fmt.Errorf("read Turn work %s: %w", path, e))
			continue
		}
		var file turnWorkFile
		if e = json.Unmarshal(data, &file); e != nil {
			failures = append(failures, fmt.Errorf("decode Turn work %s: %w", path, e))
			continue
		}
		if len(file.Works) == 0 && len(file.Advisories) == 0 {
			continue
		}
		if file.ResourceID == "" {
			continue
		}
		resource := file.ResourceID
		if e = m.withResourceController(ctx, workspace, resource, func() error {
			if e := m.reconcileTurnWorkLocked(ctx, workspace, resource, client); e != nil {
				return e
			}
			return m.reconcileResourceMailboxLocked(ctx, workspace, resource)
		}); e != nil {
			failures = append(failures, fmt.Errorf("%s: %w", resource, e))
		}
	}
	return errors.Join(failures...)
}

func cancelSuspendedTurnWork(root, resource, reason string) error {
	file, err := loadTurnWork(root, resource)
	if err != nil {
		return err
	}
	changed := false
	for i := range file.Works {
		w := &file.Works[i]
		if w.State == "suspended" || w.State == "yield_requested" || w.State == "resuming" {
			w.State, w.Outcome = "cancelled", reason
			changed = true
			if w.ResumeID != "" {
				if _, err := updateMailboxMessage(root, w.ResumeID, func(message *resourceMailboxMessage) {
					if message.Status == resourceMessageQueued {
						message.Status, message.TerminalAt = resourceMessageCancelled, time.Now().Format(time.RFC3339Nano)
					}
				}); err != nil && !isMailboxReceiptExpired(err) {
					return err
				}
			}
		}
	}
	if changed {
		return saveTurnWork(root, resource, file)
	}
	return nil
}

func settleTurnTaskState(root, resource string, file turnWorkFile) error {
	if file.advisoryBarrier() {
		return nil
	}
	if len(file.Works) > 0 {
		lastWork := file.Works[len(file.Works)-1]
		for _, advisory := range file.Advisories {
			if advisory.Standalone && advisory.CreatedAt >= lastWork.CreatedAt {
				return nil
			}
		}
	}
	if !strings.Contains(resource, ".task") {
		return nil
	}
	detail, task, err := taskDetail(root, resource)
	if err != nil || !task {
		return err
	}
	if detail.Archived || detail.State != app.TaskStateInProgress {
		return nil
	}
	for _, work := range file.Works {
		if !work.OutputDone || work.State == "suspended" || work.State == "resuming" {
			return nil
		}
	}
	pending, err := mailboxPendingForResource(root, resource)
	if err != nil || pending {
		return err
	}
	ws, err := app.OpenWorkspace(root)
	if err != nil {
		return err
	}
	state, note := app.TaskStateCompleted, ""
	if len(file.Works) > 0 && file.Works[len(file.Works)-1].State == "blocked" {
		state, note = app.TaskStateBlocked, "Turn work requires attention; inspect pua turn status"
	}
	_, err = ws.SetTaskState(resource, state, note)
	return err
}
