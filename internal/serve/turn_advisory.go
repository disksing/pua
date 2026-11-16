package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/disksing/pua/agenthub/protocol"
)

type turnAdvisory struct {
	ID                 string                  `json:"id"`
	Type               string                  `json:"type"`
	MessageID          string                  `json:"messageId,omitempty"`
	OriginWorkID       string                  `json:"originWorkId"`
	OriginTurnID       string                  `json:"originTurnId"`
	GenerationID       string                  `json:"generationId"`
	SessionID          string                  `json:"sessionId"`
	TurnID             string                  `json:"turnId,omitempty"`
	HistoryRef         string                  `json:"historyRef,omitempty"`
	State              string                  `json:"state"`
	Standalone         bool                    `json:"standalone"`
	CreatedAt          string                  `json:"createdAt"`
	TerminalObservedAt string                  `json:"terminalObservedAt,omitempty"`
	LastError          string                  `json:"lastError,omitempty"`
	Input              *agentHubInboundMessage `json:"input,omitempty"`
}

type turnChatSnapshot struct {
	Open       bool   `json:"open"`
	Epoch      uint64 `json:"epoch"`
	Revision   uint64 `json:"revision"`
	ObservedAt string `json:"observedAt"`
	ParentID   string `json:"parentId,omitempty"`
}

type chatStateNotice struct {
	Revision      uint64 `json:"revision"`
	OriginWorkID  string `json:"originWorkId"`
	AllowBoundary bool   `json:"allowBoundary,omitempty"`
}

type turnCurrentControl struct {
	Kind         string   `json:"kind"`
	GenerationID string   `json:"generationId,omitempty"`
	SessionID    string   `json:"sessionId,omitempty"`
	TurnID       string   `json:"turnId,omitempty"`
	WorkID       string   `json:"workId,omitempty"`
	Available    []string `json:"available"`
	Reason       string   `json:"reason,omitempty"`
}

func stableParentResource(resource string) string {
	if resource == "workspace" {
		return ""
	}
	if index := strings.LastIndex(resource, "."); index > 0 {
		return resource[:index]
	}
	return "workspace"
}

func chatSnapshotLocked(chat resourceChat) turnChatSnapshot {
	return turnChatSnapshot{Open: chat.Open, Epoch: chat.Epoch, Revision: chat.Revision,
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), ParentID: stableParentResource(chat.ResourceID)}
}

func readTurnChatSnapshot(root, resource string) (turnChatSnapshot, error) {
	chatMu.Lock()
	defer chatMu.Unlock()
	chat, err := loadChatLocked(root, sharedChatID(resource))
	if errors.Is(err, os.ErrNotExist) {
		chat, err = resourceChat{ResourceID: resource}, nil
	}
	if err != nil {
		return turnChatSnapshot{}, err
	}
	return chatSnapshotLocked(chat), nil
}

func (manager *agentManager) chatNoticeWork(ctx context.Context, workspace serveWorkspace, resource string) (string, bool) {
	file, err := loadTurnWork(workspace.Path, resource)
	if err != nil || len(file.Works) == 0 {
		return "", false
	}
	record, found, err := currentResourceGeneration(workspace.Path, resource)
	if err != nil || !found {
		return "", false
	}
	candidate := ""
	for index := len(file.Works) - 1; index >= 0; index-- {
		work := file.Works[index]
		if work.GenerationID == record.GenerationID && work.SessionID == record.AgentHubSessionID && work.TurnID != "" && !work.OutputDone {
			candidate = work.ID
			break
		}
	}
	if candidate == "" {
		return "", false
	}
	_, client, err := manager.agentHubRuntimeConfig()
	if err != nil {
		return candidate, false
	}
	session, err := client.GetSession(ctx, record.AgentHubSessionID)
	if err != nil {
		return candidate, false
	}
	if session.State != "running" && session.State != "waiting_approval" {
		return "", false
	}
	work := file.active(session.ID, session.CurrentTurnID)
	if work == nil || work.GenerationID != record.GenerationID {
		return "", false
	}
	turn, _, err := client.SessionTurn(ctx, session.ID, session.CurrentTurnID)
	if err != nil {
		return work.ID, false
	}
	if turn.Closed {
		return "", false
	}
	return work.ID, true
}

func (file turnWorkFile) advisory(id string) *turnAdvisory {
	for index := range file.Advisories {
		if file.Advisories[index].ID == id {
			return &file.Advisories[index]
		}
	}
	return nil
}

func advisoryPending(advisory turnAdvisory) bool {
	return advisory.State != "completed" && advisory.State != "failed" && advisory.State != "expired"
}

func (file turnWorkFile) advisoryBarrier() bool {
	for _, advisory := range file.Advisories {
		if advisoryPending(advisory) {
			return true
		}
	}
	return false
}

func (file turnWorkFile) advisoryUnowned() bool {
	for _, advisory := range file.Advisories {
		if advisoryPending(advisory) && (advisory.TurnID == "" || advisory.Standalone) {
			return true
		}
	}
	return false
}

func (manager *agentManager) advisoryMode(ctx context.Context, client *agentHubClient, session agentHubSession) (bool, bool, error) {
	if session.InputCapabilities.TurnScopedSteer {
		status, err := client.Status(ctx)
		if err != nil {
			return false, false, err
		}
		for _, capability := range status.Capabilities {
			if capability == protocol.CapabilityTurnScopedSteer {
				return true, true, nil
			}
		}
	}
	return session.Provider == "pi" && session.InputCapabilities.Steer, false, nil
}

func (manager *agentManager) reserveAdvisory(ctx context.Context, workspace serveWorkspace, resource string, file *turnWorkFile,
	session agentHubSession, client *agentHubClient, work turnWork, id, kind, messageID string, render advisoryRenderer) error {
	if file.advisory(id) != nil {
		return nil
	}
	available, scoped, err := manager.advisoryMode(ctx, client, session)
	if err != nil || !available {
		return err
	}
	text, err := renderWorkspaceAdvisory(workspace, render)
	if err != nil {
		return err
	}
	payload, err := marshalPUAMessagePayload(puaMessagePayload{
		Schema: puaMessagePayloadSchema, Role: "system", Type: kind, Text: text,
		Causation: &resourceMessageCausation{Type: kind, SourceResourceID: resource, MessageID: messageID},
	})
	if err != nil {
		return err
	}
	input := &agentHubInboundMessage{SchemaVersion: 2, Steer: true, MessageID: id, Text: text, Payload: payload}
	if scoped {
		input.ExpectedTurnID = work.TurnID
	}
	file.Advisories = append(file.Advisories, turnAdvisory{
		ID: id, Type: kind, MessageID: messageID, OriginWorkID: work.ID, OriginTurnID: work.TurnID,
		GenerationID: work.GenerationID, SessionID: work.SessionID, State: "reserved",
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Input: input,
	})
	if err := saveTurnWork(workspace.Path, resource, *file); err != nil {
		return err
	}
	manager.requestTurnOutput(workspace, resource)
	return manager.advanceAdvisory(ctx, workspace, resource, file, &file.Advisories[len(file.Advisories)-1], client, true)
}

func canonicalAdvisory(ctx context.Context, client *agentHubClient, advisory *turnAdvisory) (string, bool, error) {
	cursor := int64(0)
	for {
		frames, latest, err := client.SessionFrames(ctx, advisory.SessionID, cursor, agentHubEventMaxCount)
		if err != nil {
			return "", false, err
		}
		for _, frame := range frames {
			for _, event := range frame.Events {
				if event.Type != "message.input" {
					continue
				}
				var input agentHubInboundMessage
				if json.Unmarshal(event.Data, &input) != nil || input.MessageID != advisory.ID {
					continue
				}
				actual, actualOK := decodePUAMessagePayload(input.Payload)
				expected, expectedOK := decodePUAMessagePayload(advisory.Input.Payload)
				if !actualOK || !expectedOK || !reflect.DeepEqual(actual, expected) ||
					input.Text != advisory.Input.Text || input.Steer != advisory.Input.Steer ||
					input.ExpectedTurnID != advisory.Input.ExpectedTurnID || input.SchemaVersion != advisory.Input.SchemaVersion {
					return "", false, errors.New("advisory stable ID conflicts with canonical input")
				}
				if event.TurnID == "" {
					return "", false, errors.New("advisory canonical input has no Turn identity")
				}
				return event.TurnID, true, nil
			}
		}
		if len(frames) == 0 || frames[len(frames)-1].Cursor <= cursor || frames[len(frames)-1].Cursor >= latest {
			return "", false, nil
		}
		cursor = frames[len(frames)-1].Cursor
	}
}

func (manager *agentManager) advanceAdvisory(ctx context.Context, workspace serveWorkspace, resource string,
	file *turnWorkFile, advisory *turnAdvisory, client *agentHubClient, allowSend bool) error {
	if !advisoryPending(*advisory) {
		return nil
	}
	if advisory.Input == nil {
		return errors.New("pending advisory has no durable input")
	}
	if advisory.TurnID == "" {
		turnID, found, err := canonicalAdvisory(ctx, client, advisory)
		if err != nil {
			advisory.LastError = err.Error()
			return nil
		}
		if found {
			advisory.TurnID, advisory.State, advisory.LastError = turnID, "accepted", ""
		} else {
			if !allowSend {
				advisory.State, advisory.LastError = "expired", "resource paused or archived before canonical acceptance"
				return nil
			}
			record, current, err := currentResourceGeneration(workspace.Path, resource)
			if err != nil {
				return err
			}
			if !current || record.GenerationID != advisory.GenerationID || record.AgentHubSessionID != advisory.SessionID {
				advisory.State, advisory.LastError = "expired", "origin generation ended before canonical acceptance"
				return nil
			}
			session, err := client.GetSession(ctx, advisory.SessionID)
			if err != nil {
				advisory.LastError = err.Error()
				return nil
			}
			if session.CurrentTurnID != "" && session.CurrentTurnID != advisory.OriginTurnID {
				advisory.State, advisory.LastError = "expired", "another Turn now owns the Session"
				return nil
			}
			if session.State == "stopped" || session.State == "stopping" || session.State == "archived" {
				advisory.State, advisory.LastError = "expired", "origin Session is no longer running"
				return nil
			}
			advisory.State = "unknown"
			if err := saveTurnWork(workspace.Path, resource, *file); err != nil {
				return err
			}
			_, sendErr := client.Message(ctx, advisory.SessionID, *advisory.Input)
			turnID, found, lookupErr := canonicalAdvisory(ctx, client, advisory)
			if found && lookupErr == nil {
				advisory.TurnID, advisory.State, advisory.LastError = turnID, "accepted", ""
			} else {
				advisory.LastError = "canonical acceptance is not yet known; formal input is blocked"
				if lookupErr != nil {
					advisory.LastError = lookupErr.Error()
				} else if sendErr != nil {
					advisory.LastError = sendErr.Error()
					var upstream *agentHubAPIError
					if errors.As(sendErr, &upstream) && upstream.StatusCode >= 400 && upstream.StatusCode < 500 {
						advisory.State = "failed"
					}
				}
				return nil
			}
		}
		advisory.Standalone = advisory.TurnID != advisory.OriginTurnID
		advisory.HistoryRef, err = turnWorkReference(workspace.Path, resource, turnWork{
			GenerationID: advisory.GenerationID, SessionID: advisory.SessionID, TurnID: advisory.TurnID,
		})
		if err != nil {
			return err
		}
		if advisory.Type == "queue_preview" {
			if work := file.find(advisory.OriginWorkID); work != nil {
				if work.Previews == nil {
					work.Previews = map[string]bool{}
				}
				work.Previews[advisory.MessageID] = true
			}
		}
	}
	turn, _, err := client.SessionTurn(ctx, advisory.SessionID, advisory.TurnID)
	if err != nil {
		advisory.LastError = err.Error()
		return nil
	}
	if turn.Closed {
		advisory.State, advisory.TerminalObservedAt = "completed", time.Now().UTC().Format(time.RFC3339Nano)
		if turn.Status != "completed" {
			advisory.State, advisory.LastError = "failed", turn.Status
		} else {
			advisory.LastError = ""
		}
	}
	return nil
}

func (manager *agentManager) reconcileAdvisories(ctx context.Context, workspace serveWorkspace, resource string,
	file *turnWorkFile, client *agentHubClient, allowSend bool) error {
	for index := range file.Advisories {
		if err := manager.advanceAdvisory(ctx, workspace, resource, file, &file.Advisories[index], client, allowSend); err != nil {
			return err
		}
	}
	chatMu.Lock()
	chat, err := loadChatLocked(workspace.Path, sharedChatID(resource))
	chatMu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || chat.StateNotice == nil {
		return err
	}
	notice := *chat.StateNotice
	work := file.find(notice.OriginWorkID)
	if allowSend && work != nil && !file.advisoryUnowned() {
		record, current, err := currentResourceGeneration(workspace.Path, resource)
		if err != nil {
			return err
		}
		if current && record.GenerationID == work.GenerationID && record.AgentHubSessionID == work.SessionID {
			session, err := client.GetSession(ctx, work.SessionID)
			if err != nil {
				return err
			}
			matches := session.CurrentTurnID == work.TurnID && (session.State == "running" || session.State == "waiting_approval")
			if matches && !notice.AllowBoundary {
				turn, _, err := client.SessionTurn(ctx, session.ID, work.TurnID)
				if err != nil {
					return err
				}
				matches = !turn.Closed
			}
			if matches || (notice.AllowBoundary && session.CurrentTurnID == "") {
				snapshot := chatSnapshotLocked(chat)
				if err := manager.reserveAdvisory(ctx, workspace, resource, file, session, client, *work,
					"chat-state-"+chatID(work.ID, fmt.Sprint(notice.Revision)), "chat_state", "",
					func(language string, compact bool) (string, error) {
						return localizedChatState(language, compact, snapshot, *work)
					}); err != nil {
					return err
				}
			}
		}
	} else if allowSend && file.advisoryUnowned() {
		return nil
	}
	chatMu.Lock()
	defer chatMu.Unlock()
	latest, err := loadChatLocked(workspace.Path, sharedChatID(resource))
	if err != nil {
		return err
	}
	if latest.StateNotice != nil && latest.StateNotice.Revision == notice.Revision {
		latest.StateNotice = nil
		return saveChatLocked(workspace.Path, latest)
	}
	return nil
}

func (manager *agentManager) currentTurnControl(ctx context.Context, workspace serveWorkspace, resource string, file turnWorkFile) turnCurrentControl {
	control := turnCurrentControl{Kind: "unknown", Available: []string{}}
	record, found, err := currentResourceGeneration(workspace.Path, resource)
	if err != nil {
		control.Reason = err.Error()
		return control
	}
	if !found {
		control.Kind, control.Reason = "idle", "no current generation"
		return control
	}
	control.GenerationID, control.SessionID = record.GenerationID, record.AgentHubSessionID
	_, client, err := manager.agentHubRuntimeConfig()
	if err != nil {
		control.Reason = err.Error()
		return control
	}
	session, err := client.GetSession(ctx, record.AgentHubSessionID)
	if err != nil {
		control.Reason = err.Error()
		return control
	}
	control.TurnID = session.CurrentTurnID
	if session.CurrentTurnID == "" {
		control.Kind, control.Reason = "idle", "no active Turn"
		return control
	}
	for _, advisory := range file.Advisories {
		if advisory.Standalone && advisory.SessionID == session.ID && advisory.TurnID == session.CurrentTurnID {
			control.Kind, control.Reason = "advisory", "no formal work; end this advisory Turn and await formal input"
			return control
		}
	}
	work := file.active(session.ID, session.CurrentTurnID)
	if work == nil || work.GenerationID != record.GenerationID {
		control.Reason = "current Turn has no matching controllable formal work"
		return control
	}
	turn, _, err := client.SessionTurn(ctx, session.ID, session.CurrentTurnID)
	if err != nil || turn.Closed || (session.State != "running" && session.State != "waiting_approval") {
		control.Reason = "formal Turn is terminal or cannot be verified"
		return control
	}
	control.Kind, control.WorkID = "work", work.ID
	if work.Command != nil || work.State != "running" {
		control.Reason = "work control was already requested; end this Turn"
		return control
	}
	if len(session.PendingApprovalIDs) != 0 {
		control.Reason = "resolve outstanding approvals before using work controls"
		return control
	}
	control.Available = []string{"finish"}
	if work.ParentID == "" {
		control.Available = append(control.Available, "yield")
		if !work.Context.ChatVisible {
			snapshot, err := readTurnChatSnapshot(workspace.Path, resource)
			if err == nil && snapshot.Open {
				control.Available = append(control.Available, "notify")
			}
		}
	} else {
		control.Available = append(control.Available, "cancel-resume")
	}
	return control
}

func annotateHistoryAdvisories(root, resource string, summary *resourceHistoryTurnSummary) error {
	file, err := loadTurnWork(root, resource)
	if err != nil {
		return err
	}
	for _, advisory := range file.Advisories {
		if advisory.TurnID == summary.TurnID && advisory.GenerationID == summary.Generation.GenerationID {
			advisory.Input = nil
			summary.Advisories = append(summary.Advisories, advisory)
		}
	}
	return nil
}
