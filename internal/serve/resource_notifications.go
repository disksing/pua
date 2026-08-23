package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/localize"
)

func notificationMessageID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "msg-notify-" + hex.EncodeToString(sum[:16])
}

func workspaceInstanceID(workspacePath string) (string, error) {
	puaWorkspace, err := app.OpenWorkspace(workspacePath)
	if err != nil {
		return "", err
	}
	runtime, err := puaWorkspace.RuntimeConfig()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(runtime.InstanceID), nil
}

func (m *agentManager) managedWorkspaceByInstanceID(instanceID string) (serveWorkspace, bool, error) {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return serveWorkspace{}, false, nil
	}
	cfg, err := m.server.loadConfig()
	if err != nil {
		return serveWorkspace{}, false, err
	}
	for _, workspace := range cfg.Workspaces {
		if !m.server.ownsWorkspace(workspace.Path) {
			continue
		}
		current, readErr := workspaceInstanceID(workspace.Path)
		if readErr != nil {
			continue
		}
		if current == instanceID {
			return workspace, true, nil
		}
	}
	return serveWorkspace{}, false, nil
}

func lastAssistantText(turn agentHubTurn) string {
	for index := len(turn.Items) - 1; index >= 0; index-- {
		item := turn.Items[index]
		if item.Role == "assistant" && strings.TrimSpace(item.Text) != "" {
			return strings.TrimSpace(item.Text)
		}
	}
	return ""
}

const maxTurnResultOutputBytes = 16 * 1024

func boundedTurnResultOutput(language, value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= maxTurnResultOutputBytes {
		return value
	}
	marker := strings.TrimSpace(localize.MustRender(language, "turn-response-truncated.txt", nil))
	return value[:maxTurnResultOutputBytes] + "\n" + marker
}

func turnResultMessage(language, resourceID string, generationID string, turn agentHubTurn, reference string, sourceIDs []string, historyUnavailable bool) string {
	status := strings.TrimSpace(turn.Status)
	if status == "" {
		status = "unknown"
	}
	turnID := strings.TrimSpace(turn.TurnID)
	if turnID == "" {
		turnID = strings.TrimSpace(turn.ID)
	}
	return strings.TrimSuffix(localize.MustRender(language, "turn-result.md", map[string]any{
		"ResourceID": resourceID, "GenerationID": generationID, "TurnID": turnID, "Status": status,
		"SourceMessageIDs": strings.Join(sourceIDs, "`, `"), "Reference": reference,
		"Reply": boundedTurnResultOutput(language, lastAssistantText(turn)), "HistoryUnavailable": historyUnavailable,
	}), "\n")
}

func terminalDeliveryMessage(language string, message resourceMailboxMessage) string {
	status := publicResourceMessageStatus(message.Status)
	code := strings.TrimSpace(message.LastErrorCode)
	if code == "" {
		code = "delivery_failed"
	}
	return strings.TrimSuffix(localize.MustRender(language, "delivery-notice.md", map[string]string{
		"MessageID": message.ID, "ResourceID": message.ResourceID, "Status": status,
		"Code": code, "Detail": strings.TrimSpace(message.LastError),
	}), "\n")
}

func ensureNotificationReceipt(workspacePath, messageID, notificationType, targetInstanceID, targetResourceID, receiptID string) (resourceMailboxMessage, error) {
	return updateMailboxMessage(workspacePath, messageID, func(message *resourceMailboxMessage) {
		if message.Notification != nil {
			return
		}
		now := time.Now().Format(time.RFC3339Nano)
		message.Notification = &resourceNotificationReceipt{
			ID: receiptID, Type: notificationType, Status: resourceNotificationWaiting,
			TargetWorkspaceInstanceID: targetInstanceID, TargetResourceID: targetResourceID,
			CreatedAt: now, UpdatedAt: now,
		}
	})
}

func updateNotificationReceipt(workspacePath, sourceMessageID string, mutate func(*resourceNotificationReceipt)) error {
	_, err := updateMailboxMessage(workspacePath, sourceMessageID, func(message *resourceMailboxMessage) {
		if message.Notification == nil {
			return
		}
		mutate(message.Notification)
		message.Notification.UpdatedAt = time.Now().Format(time.RFC3339Nano)
	})
	return err
}

func updateOperationNotificationReceipts(workspacePath string, operation resourceMailboxNotificationOp, mutate func(*resourceNotificationReceipt)) error {
	ids := mailboxOperationSourceIDs(operation)
	if len(ids) == 0 {
		return nil
	}
	source, found, err := mailboxMessageByID(workspacePath, ids[0])
	if err != nil || !found {
		return err
	}
	_, err = mutateResourceMailboxForResource(workspacePath, source.ResourceID, func(mailbox *resourceMailbox) error {
		for index := range mailbox.Messages {
			message := &mailbox.Messages[index]
			for _, sourceID := range ids {
				if message.ID != sourceID {
					continue
				}
				if message.Notification == nil {
					message.Notification = &resourceNotificationReceipt{ID: operation.ID, Type: operation.Type, Status: operation.Status, TargetWorkspaceInstanceID: operation.TargetWorkspaceInstanceID, TargetResourceID: operation.TargetResourceID, CreatedAt: operation.UpdatedAt, UpdatedAt: operation.UpdatedAt}
				}
				mutate(message.Notification)
				message.Notification.UpdatedAt = time.Now().Format(time.RFC3339Nano)
				if operation.Type == resourceMessageTypeTurnResult {
					message.ResultOperationID = operation.ID
					message.ResultSubscriptionStatus = resourceResultSubscriptionComplete
				}
				break
			}
		}
		return nil
	})
	return err
}

func terminalNotificationReceipt(workspacePath, sourceMessageID, code, detail string) error {
	if err := updateNotificationReceipt(workspacePath, sourceMessageID, func(receipt *resourceNotificationReceipt) {
		receipt.Status = resourceNotificationTerminal
		receipt.LastErrorCode = code
		receipt.LastError = detail
	}); err != nil {
		return err
	}
	return removeMailboxNotificationOperation(workspacePath, sourceMessageID)
}

func terminalNotificationOperation(workspacePath string, operation resourceMailboxNotificationOp, code, detail string) error {
	ids := mailboxOperationSourceIDs(operation)
	if len(ids) == 0 {
		return errors.New("notification operation has no source mailbox message")
	}
	if err := updateOperationNotificationReceipts(workspacePath, operation, func(receipt *resourceNotificationReceipt) {
		receipt.Status = resourceNotificationTerminal
		receipt.LastErrorCode = code
		receipt.LastError = detail
	}); err != nil {
		return err
	}
	return removeMailboxNotificationOperation(workspacePath, ids[0])
}

func mirrorNotificationDelivery(receipt *resourceNotificationReceipt, message resourceMailboxMessage) {
	receipt.DeliveryStatus = publicResourceMessageStatus(message.Status)
	receipt.DeliveredAt = message.DeliveredAt
	receipt.TerminalAt = message.TerminalAt
	receipt.LastError = message.LastError
	receipt.LastErrorCode = message.LastErrorCode
	switch message.Status {
	case resourceMessageDelivered:
		receipt.Status = resourceNotificationDelivered
	case resourceMessageCancelled, resourceMessageUndeliverable, resourceMessageDeliveryUnknown:
		receipt.Status = resourceNotificationTerminal
	}
}

func (m *agentManager) routeNotification(ctx context.Context, source serveWorkspace, sourceMessage resourceMailboxMessage, generated resourceMailboxMessage) error {
	receipt := sourceMessage.Notification
	if receipt == nil {
		return nil
	}
	if generated.ID != "" {
		if err := upsertMailboxNotificationOperation(source.Path, sourceMessage.ID, mailboxNotificationOperationFromGenerated(sourceMessage, generated)); err != nil {
			return err
		}
		latest, found, err := mailboxMessageByID(source.Path, sourceMessage.ID)
		if err != nil || !found {
			return err
		}
		sourceMessage = latest
		receipt = sourceMessage.Notification
	}
	operation, operationFound, err := mailboxNotificationOperation(source.Path, sourceMessage.ID)
	if err != nil {
		return err
	}
	if !operationFound {
		operation = mailboxNotificationOperationFromGenerated(sourceMessage, generated)
		if operation.ID == "" {
			operation.ID = receipt.ID
		}
		if err := upsertMailboxNotificationOperation(source.Path, sourceMessage.ID, operation); err != nil {
			return err
		}
	}
	if generated.ID == "" && operation.GeneratedMessageID != "" {
		generated = resourceMailboxOperationGeneratedMessage(operation)
	}
	if operation.Status == resourceNotificationTerminal || operation.Status == resourceNotificationDelivered {
		return removeMailboxNotificationOperation(source.Path, sourceMessage.ID)
	}
	target, found, err := m.managedWorkspaceByInstanceID(operation.TargetWorkspaceInstanceID)
	if err != nil {
		return err
	}
	if !found {
		if operation.Status == resourceNotificationAccepted {
			return updateOperationNotificationReceipts(source.Path, operation, func(current *resourceNotificationReceipt) {
				current.LastErrorCode = "target_workspace_unavailable"
				current.LastError = "the target Workspace is no longer registered with and owned by this PUA Server; prior mailbox acceptance is retained"
			})
		}
		return terminalNotificationOperation(source.Path, operation, "target_workspace_unavailable", "the target Workspace is not registered with and owned by this PUA Server")
	}
	targetResourceID := normalizedResourceID(operation.TargetResourceID)
	if operation.Status == resourceNotificationAccepted {
		var latest resourceMailboxMessage
		var messageFound bool
		var targetTerminalCode, targetTerminalDetail string
		controllerErr := m.withResourceController(ctx, target, targetResourceID, func() error {
			var loadErr error
			latest, messageFound, loadErr = mailboxMessageByID(target.Path, operation.GeneratedMessageID)
			if loadErr != nil {
				var apiErr *resourceAPIError
				if errors.As(loadErr, &apiErr) && apiErr.Code == "message_receipt_expired" {
					targetTerminalCode = "target_message_expired"
					targetTerminalDetail = loadErr.Error()
					return nil
				}
				return loadErr
			}
			if !messageFound {
				targetTerminalCode = "target_message_missing"
				targetTerminalDetail = "the previously accepted target mailbox message is missing"
			}
			return nil
		})
		if controllerErr != nil {
			return controllerErr
		}
		if targetTerminalCode != "" {
			return terminalNotificationOperation(source.Path, operation, targetTerminalCode, targetTerminalDetail)
		}
		if err := updateOperationNotificationReceipts(source.Path, operation, func(current *resourceNotificationReceipt) {
			mirrorNotificationDelivery(current, latest)
		}); err != nil {
			return err
		}
		if latest.Status == resourceMessageDelivered || latest.Status == resourceMessageCancelled || latest.Status == resourceMessageUndeliverable || latest.Status == resourceMessageDeliveryUnknown {
			return removeMailboxNotificationOperation(source.Path, mailboxOperationSourceIDs(operation)[0])
		}
		return updateMailboxNotificationOperation(source.Path, mailboxOperationSourceIDs(operation)[0], func(current *resourceMailboxNotificationOp) {
			current.DeliveryStatus = publicResourceMessageStatus(latest.Status)
			current.LastError = latest.LastError
			current.LastErrorCode = latest.LastErrorCode
		})
	}
	var accepted, latest resourceMailboxMessage
	var targetTerminalCode, targetTerminalDetail string
	controllerErr := m.withResourceController(ctx, target, targetResourceID, func() error {
		exists, archived, _, inspectErr := resourceExistsAndArchived(target.Path, targetResourceID)
		if inspectErr != nil || !exists {
			targetTerminalCode = "target_resource_not_found"
			targetTerminalDetail = fmt.Sprintf("target resource not found: %s", operation.TargetResourceID)
			if inspectErr != nil {
				targetTerminalDetail = inspectErr.Error()
			}
			return nil
		}
		if archived {
			targetTerminalCode = "target_resource_archived"
			targetTerminalDetail = fmt.Sprintf("target resource is archived: %s", operation.TargetResourceID)
			return nil
		}
		var err error
		accepted, err = acceptGeneratedMailboxMessage(target.Path, generated)
		if err != nil {
			var apiErr *resourceAPIError
			if errors.As(err, &apiErr) && apiErr.Code == "message_conflict" {
				targetTerminalCode = apiErr.Code
				targetTerminalDetail = apiErr.Message
				return nil
			}
			return err
		}
		if accepted.Status == resourceMessageQueued || accepted.Status == resourceMessageDelivering || accepted.Status == resourceMessageInterrupting {
			if err := m.reconcileResourceMailboxLocked(ctx, target, accepted.ResourceID); err != nil {
				recordMailboxFailure(target.Path, accepted.ID, err)
			}
		}
		var found bool
		var loadErr error
		latest, found, loadErr = mailboxMessageByID(target.Path, accepted.ID)
		if loadErr != nil {
			var apiErr *resourceAPIError
			if errors.As(loadErr, &apiErr) && apiErr.Code == "message_receipt_expired" {
				targetTerminalCode = "target_message_expired"
				targetTerminalDetail = loadErr.Error()
				return nil
			}
			return loadErr
		}
		if !found {
			targetTerminalCode = "target_message_missing"
			targetTerminalDetail = "the accepted target mailbox message is missing"
		}
		return nil
	})
	if controllerErr != nil {
		return controllerErr
	}
	if targetTerminalCode != "" {
		return terminalNotificationOperation(source.Path, operation, targetTerminalCode, targetTerminalDetail)
	}
	if err := updateOperationNotificationReceipts(source.Path, operation, func(current *resourceNotificationReceipt) {
		current.Status = resourceNotificationAccepted
		current.AcceptedAt = accepted.AcceptedAt
		current.DeliveryStatus = publicResourceMessageStatus(accepted.Status)
		current.DeliveredAt = accepted.DeliveredAt
		current.TerminalAt = accepted.TerminalAt
		current.LastError = accepted.LastError
		current.LastErrorCode = accepted.LastErrorCode
	}); err != nil {
		return err
	}
	if err := updateMailboxNotificationOperation(source.Path, mailboxOperationSourceIDs(operation)[0], func(current *resourceMailboxNotificationOp) {
		current.Status = resourceNotificationAccepted
		current.AcceptedAt = accepted.AcceptedAt
		current.GeneratedText = ""
		current.GeneratedRequestedMode = accepted.RequestedMode
		current.GeneratedActualMode = accepted.ActualMode
		current.GeneratedModeFrozen = accepted.ModeFrozen
		current.GeneratedDowngradeReason = accepted.DowngradeReason
		current.DeliveryStatus = publicResourceMessageStatus(accepted.Status)
		current.DeliveredAt = accepted.DeliveredAt
		current.TerminalAt = accepted.TerminalAt
		current.LastError = accepted.LastError
		current.LastErrorCode = accepted.LastErrorCode
	}); err != nil {
		return err
	}
	if err := updateOperationNotificationReceipts(source.Path, operation, func(current *resourceNotificationReceipt) {
		mirrorNotificationDelivery(current, latest)
	}); err != nil {
		return err
	}
	if latest.Status == resourceMessageDelivered || latest.Status == resourceMessageCancelled || latest.Status == resourceMessageUndeliverable || latest.Status == resourceMessageDeliveryUnknown {
		return removeMailboxNotificationOperation(source.Path, mailboxOperationSourceIDs(operation)[0])
	}
	return nil
}

func terminalTurnStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "failed", "cancelled", "canceled", "interrupted", "turn.completed", "turn.failed", "turn.cancelled", "turn.interrupted":
		return true
	default:
		return false
	}
}

type turnResultSubscriptionGroup struct {
	SourceResourceID          string
	SourceWorkspaceInstanceID string
	GenerationID              string
	TurnID                    string
	SubscriberResourceID      string
	SubscriberInstanceID      string
	Messages                  []resourceMailboxMessage
}

func turnResultSubscriptionGroupKey(message resourceMailboxMessage) string {
	return strings.Join([]string{
		normalizedResourceID(message.ResourceID), message.GenerationID, message.TurnID,
		strings.TrimSpace(message.SenderWorkspaceInstanceID), strings.TrimSpace(message.Sender.ID),
	}, "\x00")
}

func (m *agentManager) reconcileTurnResultSubscriptions(ctx context.Context, workspace serveWorkspace, instanceID string, client *agentHubClient) error {
	hotMailboxes, err := loadAllHotResourceMailboxes(workspace.Path)
	if err != nil {
		return err
	}
	groups := make(map[string]*turnResultSubscriptionGroup)
	for _, mailbox := range hotMailboxes {
		for _, message := range mailbox.Messages {
			if message.Type != "" || message.Status != resourceMessageDelivered || message.ActualMode == resourceMessageModeSteer || !message.SubscribeResult ||
				(message.ResultSubscriptionStatus != resourceResultSubscriptionPending && message.ResultSubscriptionStatus != "") ||
				message.Sender == nil || !isStablePUAResourceID(message.Sender.ID) ||
				strings.TrimSpace(message.SenderWorkspaceInstanceID) == "" || strings.TrimSpace(message.GenerationID) == "" {
				continue
			}
			if strings.TrimSpace(message.TurnID) == "" {
				generation, generationFound, generationErr := generationRecordByID(workspace.Path, message.GenerationID)
				if generationErr != nil {
					return generationErr
				}
				if !generationFound || strings.TrimSpace(generation.AgentHubSessionID) == "" || client == nil {
					continue
				}
				canonical, canonicalFound, canonicalErr := findCanonicalAgentHubMessage(ctx, client, generation.AgentHubSessionID, message, 0)
				if canonicalErr != nil || !canonicalFound || strings.TrimSpace(canonical.TurnID) == "" {
					continue
				}
				message, err = updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
					current.TurnID = strings.TrimSpace(canonical.TurnID)
					bindMailboxResultSubscription(current, current.TurnID)
				})
				if err != nil {
					return err
				}
			}
			if strings.TrimSpace(message.TurnID) == "" {
				continue
			}
			key := turnResultSubscriptionGroupKey(message)
			group := groups[key]
			if group == nil {
				group = &turnResultSubscriptionGroup{
					SourceResourceID: normalizedResourceID(message.ResourceID), SourceWorkspaceInstanceID: instanceID,
					GenerationID: message.GenerationID, TurnID: message.TurnID,
					SubscriberResourceID: strings.TrimSpace(message.Sender.ID), SubscriberInstanceID: strings.TrimSpace(message.SenderWorkspaceInstanceID),
				}
				groups[key] = group
			}
			group.Messages = append(group.Messages, cloneMailboxMessage(message))
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		group := groups[key]
		sort.SliceStable(group.Messages, func(i, j int) bool {
			if group.Messages[i].Sequence != group.Messages[j].Sequence {
				return group.Messages[i].Sequence < group.Messages[j].Sequence
			}
			return group.Messages[i].ID < group.Messages[j].ID
		})
		sourceIDs := make([]string, 0, len(group.Messages))
		for _, message := range group.Messages {
			sourceIDs = append(sourceIDs, message.ID)
		}

		record, found, err := generationRecordByID(workspace.Path, group.GenerationID)
		if err != nil {
			return err
		}
		historyUnavailable := false
		turn := agentHubTurn{}
		if !found {
			turn = agentHubTurn{TurnID: group.TurnID, Status: "unknown", Closed: true}
			historyUnavailable = true
		} else {
			var turnErr error
			if strings.TrimSpace(record.AgentHubSessionID) == "" {
				turnErr = errors.New("generation has no AgentHub session")
			} else {
				turn, _, turnErr = client.SessionTurn(ctx, record.AgentHubSessionID, group.TurnID)
			}
			if turnErr != nil {
				if record.CompletionMarker != "" && record.CompletionTurnID == group.TurnID {
					turn = agentHubTurn{TurnID: group.TurnID, Status: record.CompletionState, Closed: true, EndedAt: record.CompletionAt}
					historyUnavailable = true
				} else if !isLiveAgentStatus(record.Status) {
					turn = agentHubTurn{TurnID: group.TurnID, Status: record.Status, Closed: true, EndedAt: record.UpdatedAt}
					historyUnavailable = true
				} else {
					continue
				}
			}
		}
		if !turn.Closed && !terminalTurnStatus(turn.Status) {
			continue
		}
		if strings.TrimSpace(turn.Status) == "" {
			turn.Status = "unknown"
		}
		turnID := strings.TrimSpace(turn.TurnID)
		if turnID == "" {
			turnID = strings.TrimSpace(turn.ID)
			if turnID == "" {
				turnID = group.TurnID
			}
			turn.TurnID = turnID
		}
		reference := ""
		if !historyUnavailable {
			reference, err = encodeResourceHistoryReference(resourceHistoryReference{
				Kind: "turn", InstanceID: instanceID, ResourceID: group.SourceResourceID, GenerationID: group.GenerationID, TurnID: turnID,
			})
			if err != nil {
				return err
			}
		}
		existingOperation, existingFound, err := mailboxNotificationOperation(workspace.Path, sourceIDs[0])
		if err != nil {
			return err
		}
		operationID := notificationMessageID(resourceMessageTypeTurnResult, instanceID, group.SourceResourceID, group.GenerationID, turnID, group.SubscriberInstanceID, group.SubscriberResourceID)
		generatedMessageID := operationID
		targetWorkspaceInstanceID := group.SubscriberInstanceID
		targetResourceID := group.SubscriberResourceID
		operationStatus := resourceNotificationWaiting
		acceptedAt := ""
		generatedRequestedMode := resourceMessageModeSteer
		generatedActualMode := resourceMessageModeSteer
		generatedModeFrozen := false
		generatedDowngradeReason := ""
		if existingFound && existingOperation.Type == resourceMessageTypeTurnResult {
			// A pre-subscribeResult server may already have durably created the
			// callback operation under the legacy type. Reuse its operation and
			// generated message IDs (and target) so migration recovery cannot
			// create a second result for the same source message.
			if strings.TrimSpace(existingOperation.ID) != "" {
				operationID = existingOperation.ID
			}
			if strings.TrimSpace(existingOperation.GeneratedMessageID) != "" {
				generatedMessageID = existingOperation.GeneratedMessageID
			}
			if strings.TrimSpace(existingOperation.TargetWorkspaceInstanceID) != "" {
				targetWorkspaceInstanceID = existingOperation.TargetWorkspaceInstanceID
			}
			if strings.TrimSpace(existingOperation.TargetResourceID) != "" {
				targetResourceID = existingOperation.TargetResourceID
			}
			if strings.TrimSpace(existingOperation.Status) != "" {
				operationStatus = existingOperation.Status
			}
			acceptedAt = existingOperation.AcceptedAt
			if existingOperation.GeneratedRequestedMode != "" {
				generatedRequestedMode = existingOperation.GeneratedRequestedMode
			}
			if existingOperation.GeneratedActualMode != "" {
				generatedActualMode = existingOperation.GeneratedActualMode
			}
			generatedModeFrozen = existingOperation.GeneratedModeFrozen
			generatedDowngradeReason = existingOperation.GeneratedDowngradeReason
		}
		language, err := m.notificationContentLanguage(workspace, targetWorkspaceInstanceID)
		if err != nil {
			return err
		}
		causation := &resourceMessageCausation{
			Type: resourceMessageTypeTurnResult, SourceWorkspaceInstanceID: instanceID, SourceResourceID: group.SourceResourceID,
			MessageID: sourceIDs[0], SourceMessageIDs: sourceIDs, GenerationID: group.GenerationID, TurnID: turnID, TurnReference: reference,
			TurnStatus: strings.TrimSpace(turn.Status), HistoryUnavailable: historyUnavailable,
		}
		generated := resourceMailboxMessage{
			ID: generatedMessageID, ResourceID: targetResourceID,
			Text:   turnResultMessage(language, group.SourceResourceID, group.GenerationID, turn, reference, sourceIDs, historyUnavailable),
			Sender: &agentHubMessageSender{ID: group.SourceResourceID, Name: group.SourceResourceID}, SenderWorkspaceInstanceID: instanceID,
			RequestedMode: generatedRequestedMode, ActualMode: generatedActualMode, ModeFrozen: generatedModeFrozen, DowngradeReason: generatedDowngradeReason,
			SubscribeResult: false, ResultSubscriptionStatus: resourceResultSubscriptionDisabled,
			Type: resourceMessageTypeTurnResult, Causation: causation,
		}
		operation := resourceMailboxNotificationOp{
			ID: operationID, Type: resourceMessageTypeTurnResult, SourceMessageID: sourceIDs[0], SourceMessageIDs: sourceIDs,
			SourceResourceID: group.SourceResourceID, SourceWorkspaceInstanceID: instanceID,
			TargetWorkspaceInstanceID: targetWorkspaceInstanceID, TargetResourceID: targetResourceID,
			GeneratedMessageID: generatedMessageID, GeneratedText: generated.Text, GeneratedSender: generated.Sender, GeneratedCausation: causation,
			GeneratedRequestedMode: generatedRequestedMode, GeneratedActualMode: generatedActualMode, GeneratedModeFrozen: generatedModeFrozen, GeneratedDowngradeReason: generatedDowngradeReason,
			Status: operationStatus, AcceptedAt: acceptedAt, UpdatedAt: time.Now().Format(time.RFC3339Nano),
			GenerationID: group.GenerationID, TurnID: turnID, TurnReference: reference, TurnStatus: turn.Status, HistoryUnavailable: historyUnavailable,
		}
		if err := upsertMailboxNotificationOperationForSources(workspace.Path, sourceIDs, operation); err != nil {
			return err
		}
		latest, found, err := mailboxMessageByID(workspace.Path, sourceIDs[0])
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if err := m.routeNotification(ctx, workspace, latest, resourceMailboxMessage{}); err != nil {
			return err
		}
	}
	return nil
}

func (m *agentManager) reconcileTerminalNotice(ctx context.Context, workspace serveWorkspace, instanceID string, message resourceMailboxMessage) error {
	if message.Type != "" || message.Role != "agent" || message.Sender == nil || strings.TrimSpace(message.Sender.ID) == "" ||
		strings.TrimSpace(message.SenderWorkspaceInstanceID) == "" ||
		(message.Status != resourceMessageCancelled && message.Status != resourceMessageUndeliverable && message.Status != resourceMessageDeliveryUnknown) {
		return nil
	}
	receiptID := notificationMessageID(resourceMessageTypeDeliveryTerminal, instanceID, message.ResourceID, message.ID, message.Status)
	updated, err := ensureNotificationReceipt(workspace.Path, message.ID, resourceMessageTypeDeliveryTerminal, message.SenderWorkspaceInstanceID, message.Sender.ID, receiptID)
	if err != nil {
		return err
	}
	language, err := m.notificationContentLanguage(workspace, message.SenderWorkspaceInstanceID)
	if err != nil {
		return err
	}
	generated := resourceMailboxMessage{
		ID: receiptID, ResourceID: message.Sender.ID, Text: terminalDeliveryMessage(language, message),
		Sender: &agentHubMessageSender{ID: message.ResourceID, Name: message.ResourceID}, SenderWorkspaceInstanceID: instanceID,
		RequestedMode: resourceMessageModeSteer, ActualMode: resourceMessageModeSteer,
		Type: resourceMessageTypeDeliveryTerminal,
		Causation: &resourceMessageCausation{
			Type: resourceMessageTypeDeliveryTerminal, SourceWorkspaceInstanceID: instanceID, SourceResourceID: message.ResourceID,
			MessageID: message.ID, GenerationID: message.GenerationID, TurnID: message.TurnID, TerminalCode: message.LastErrorCode,
		},
	}
	return m.routeNotification(ctx, workspace, updated, generated)
}

func (m *agentManager) reconcileWorkspaceNotifications(ctx context.Context, workspace serveWorkspace, client *agentHubClient) error {
	instanceID, err := workspaceInstanceID(workspace.Path)
	if err != nil {
		return err
	}
	hotMailboxes, err := loadAllHotResourceMailboxes(workspace.Path)
	if err != nil {
		return err
	}
	var failures []string
	for _, mailbox := range hotMailboxes {
		messages := append([]resourceMailboxMessage(nil), mailbox.Messages...)
		sort.SliceStable(messages, func(i, j int) bool { return messages[i].Sequence < messages[j].Sequence })
		for _, message := range messages {
			if message.Notification != nil && (message.Notification.Status == resourceNotificationAccepted || message.Notification.Status == resourceNotificationDelivered || message.Notification.Status == resourceNotificationTerminal) {
				if err := m.routeNotification(ctx, workspace, message, resourceMailboxMessage{}); err != nil {
					failures = append(failures, fmt.Sprintf("message %s notification receipt: %v", message.ID, err))
				}
				continue
			}
			if err := m.reconcileTerminalNotice(ctx, workspace, instanceID, message); err != nil {
				failures = append(failures, fmt.Sprintf("message %s terminal notice: %v", message.ID, err))
				continue
			}
		}
	}
	if err := m.reconcileTurnResultSubscriptions(ctx, workspace, instanceID, client); err != nil {
		failures = append(failures, fmt.Sprintf("turn result subscriptions: %v", err))
	}
	operations, err := pendingResourceMailboxNotificationOperations(workspace.Path)
	if err != nil {
		return err
	}
	for _, operation := range operations {
		sourceIDs := mailboxOperationSourceIDs(operation)
		if len(sourceIDs) == 0 {
			continue
		}
		source, found, sourceErr := mailboxMessageByID(workspace.Path, sourceIDs[0])
		if sourceErr != nil || !found {
			if sourceErr != nil {
				failures = append(failures, fmt.Sprintf("operation %s source: %v", operation.ID, sourceErr))
			}
			continue
		}
		if err := m.routeNotification(ctx, workspace, source, resourceMailboxOperationGeneratedMessage(operation)); err != nil {
			failures = append(failures, fmt.Sprintf("operation %s: %v", operation.ID, err))
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}
