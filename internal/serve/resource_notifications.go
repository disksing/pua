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
	if isLegacyTurnResultType(receipt.Type) || isLegacyTurnResultType(generated.Type) {
		operation, found, err := mailboxNotificationOperation(source.Path, sourceMessage.ID)
		if err != nil {
			return err
		}
		if found {
			if operation.SourceResourceID == "" {
				operation.SourceResourceID = sourceMessage.ResourceID
			}
			return retireLegacyResultOperation(source.Path, operation)
		}
		if receipt.Status != resourceNotificationDelivered && receipt.Status != resourceNotificationTerminal {
			return terminalNotificationReceipt(source.Path, sourceMessage.ID, "result_subscription_removed", "Automatic Turn results are no longer sent.")
		}
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
	if operationFound && isLegacyTurnResultType(operation.Type) {
		if operation.SourceResourceID == "" {
			operation.SourceResourceID = sourceMessage.ResourceID
		}
		return retireLegacyResultOperation(source.Path, operation)
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

// Retire an old outbox operation without delivering it, retaining source receipts.
func retireLegacyResultOperation(workspacePath string, operation resourceMailboxNotificationOp) error {
	_, err := mutateResourceMailboxStoreForResource(workspacePath, operation.SourceResourceID, func(store *resourceMailboxStore) error {
		for index := range store.Mailbox.Messages {
			message := &store.Mailbox.Messages[index]
			if !mailboxOperationHasSource(operation, message.ID) {
				continue
			}
			if message.Notification == nil {
				message.Notification = &resourceNotificationReceipt{ID: operation.ID, Type: operation.Type, Status: operation.Status, TargetWorkspaceInstanceID: operation.TargetWorkspaceInstanceID, TargetResourceID: operation.TargetResourceID, CreatedAt: operation.UpdatedAt}
			}
			if message.Notification.Status != resourceNotificationDelivered && message.Notification.Status != resourceNotificationTerminal {
				now := time.Now().Format(time.RFC3339Nano)
				message.Notification.Status = resourceNotificationTerminal
				message.Notification.TerminalAt = now
				message.Notification.UpdatedAt = now
				message.Notification.LastErrorCode = "result_subscription_removed"
				message.Notification.LastError = "Automatic Turn results are no longer sent."
			}
		}
		for index := range store.Outbox.Operations {
			current := &store.Outbox.Operations[index]
			if current.ID == operation.ID &&
				(current.Status != resourceNotificationTerminal || current.LastErrorCode != "result_subscription_removed") {
				current.Status = resourceNotificationTerminal
				current.TerminalAt = time.Now().Format(time.RFC3339Nano)
				current.UpdatedAt = current.TerminalAt
				current.LastErrorCode = "result_subscription_removed"
				current.LastError = "Automatic Turn results are no longer sent."
			}
		}
		return nil
	})
	return err
}

func (m *agentManager) reconcileWorkspaceNotifications(ctx context.Context, workspace serveWorkspace, _ *agentHubClient) error {
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
			if message.Notification != nil && isLegacyTurnResultType(message.Notification.Type) {
				if err := m.routeNotification(ctx, workspace, message, resourceMailboxMessage{}); err != nil {
					failures = append(failures, fmt.Sprintf("message %s legacy result: %v", message.ID, err))
				}
				continue
			}
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
	operations, err := pendingResourceMailboxNotificationOperations(workspace.Path)
	if err != nil {
		return err
	}
	for _, operation := range operations {
		if isLegacyTurnResultType(operation.Type) {
			if err := retireLegacyResultOperation(workspace.Path, operation); err != nil {
				failures = append(failures, fmt.Sprintf("operation %s legacy result: %v", operation.ID, err))
			}
			continue
		}
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
