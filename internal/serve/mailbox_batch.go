package serve

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
)

const mailboxBatchCapability = "mailbox_batch_v1"
const resourceMessageHandled = "handled"
const mailboxReadDefaultBytes = 64 * 1024
const mailboxReadMaxBytes = 1024 * 1024

type mailboxQueueSnapshot struct {
	SampledAt               string `json:"sampledAt"`
	WorkspaceInstanceID     string `json:"workspaceInstanceId"`
	ResourceID              string `json:"resourceId"`
	WaitingTotal            int    `json:"waitingTotal"`
	SameSourceWaiting       int    `json:"sameSourceWaiting"`
	BatchEligibleFromSource int    `json:"batchEligibleFromSource"`
	Delivering              int    `json:"delivering"`
	Interrupting            int    `json:"interrupting"`
	DeliveryUnknown         int    `json:"deliveryUnknown"`
	CurrentExcluded         bool   `json:"currentExcluded"`
	IncludesPreviewMessage  bool   `json:"includesPreviewMessage"`
}

func ordinaryBatchMessage(message resourceMailboxMessage) bool {
	return message.Role == "agent" && message.Type == "" && message.Causation == nil &&
		message.Sender != nil && isStablePUAResourceID(message.Sender.ID) &&
		message.SenderWorkspaceInstanceID != "" && message.RequestedMode != resourceMessageModeInterrupt
}

func batchIneligibleReason(message resourceMailboxMessage, works turnWorkFile) string {
	if message.Status != resourceMessageQueued {
		return "message_not_waiting"
	}
	if !ordinaryBatchMessage(message) {
		return "not_ordinary_agent_message"
	}
	if message.AttemptCount > 0 || message.TurnID != "" {
		return "delivery_already_attempted"
	}
	for _, work := range works.Works {
		if message.ID == work.TargetID || message.ID == work.ResumeID {
			return "continuation_dependency"
		}
	}
	return ""
}

func queueSnapshot(instanceID, resource string, mailbox resourceMailbox, source *resourceMailboxMessage, exclude string, works turnWorkFile) *mailboxQueueSnapshot {
	snapshot := &mailboxQueueSnapshot{
		SampledAt: time.Now().Format(time.RFC3339Nano), WorkspaceInstanceID: instanceID,
		ResourceID: resource, CurrentExcluded: exclude != "",
	}
	for _, message := range mailbox.Messages {
		if message.ResourceID != resource || message.ID == exclude {
			continue
		}
		switch message.Status {
		case resourceMessageQueued:
			snapshot.WaitingTotal++
			if source != nil && ordinaryBatchMessage(message) && source.Sender != nil &&
				message.Sender.ID == source.Sender.ID && message.SenderWorkspaceInstanceID == source.SenderWorkspaceInstanceID {
				snapshot.SameSourceWaiting++
				if batchIneligibleReason(message, works) == "" {
					snapshot.BatchEligibleFromSource++
				}
			}
		case resourceMessageDelivering:
			snapshot.Delivering++
		case resourceMessageInterrupting:
			snapshot.Interrupting++
		case resourceMessageDeliveryUnknown:
			snapshot.DeliveryUnknown++
		}
	}
	return snapshot
}

func queueSnapshotText(snapshot *mailboxQueueSnapshot) string {
	if snapshot == nil {
		return ""
	}
	return fmt.Sprintf("\nQueue snapshot at %s: waiting=%d, same_source_waiting=%d, batch_eligible_from_source=%d, delivering=%d, interrupting=%d, delivery_unknown=%d; current_excluded=%t, includes_preview_message=%t. Inspect `pua message list`; read complete bodies with `pua message read --from=<resource>`, then explicitly ack selected IDs only after completing their work.",
		snapshot.SampledAt, snapshot.WaitingTotal, snapshot.SameSourceWaiting, snapshot.BatchEligibleFromSource,
		snapshot.Delivering, snapshot.Interrupting, snapshot.DeliveryUnknown, snapshot.CurrentExcluded, snapshot.IncludesPreviewMessage)
}

func sampleMailboxQueue(workspace serveWorkspace, message resourceMailboxMessage) (*mailboxQueueSnapshot, error) {
	mailbox, err := loadHotResourceMailbox(workspace.Path, message.ResourceID)
	if err != nil {
		return nil, err
	}
	works, err := loadTurnWork(workspace.Path, message.ResourceID)
	if err != nil {
		return nil, err
	}
	return queueSnapshot(mailboxInstanceID(workspace.Path), message.ResourceID, mailbox, &message, message.ID, works), nil
}

type mailboxHandling struct {
	OperationID  string `json:"operationId"`
	WorkID       string `json:"workId"`
	GenerationID string `json:"generationId"`
	HandledAt    string `json:"handledAt"`
	Summary      string `json:"summary"`
	History      string `json:"history"`
	BodyDigest   string `json:"bodyDigest"`
}

type mailboxReadRequest struct {
	IDs      []string `json:"ids"`
	WorkID   string   `json:"workId,omitempty"`
	MaxBytes int      `json:"maxBytes,omitempty"`
}

type mailboxReadItem struct {
	resourceMessageResponse
	Complete    bool   `json:"complete"`
	CanAck      bool   `json:"canAck"`
	Reason      string `json:"reason,omitempty"`
	BodyDigest  string `json:"bodyDigest,omitempty"`
	ReadReceipt string `json:"readReceipt,omitempty"`
}

type mailboxReadResponse struct {
	Capability   string                `json:"capability"`
	Items        []mailboxReadItem     `json:"items"`
	RemainingIDs []string              `json:"remainingIds"`
	Queue        *mailboxQueueSnapshot `json:"queue"`
}

type mailboxAckRequest struct {
	OperationID string            `json:"operationId"`
	WorkID      string            `json:"workId"`
	IDs         []string          `json:"ids"`
	Receipts    map[string]string `json:"receipts,omitempty"`
	ReceiptRef  string            `json:"receiptRef,omitempty"`
	Summary     string            `json:"summary"`
}

type mailboxAckResult struct {
	Capability  string                `json:"capability"`
	OperationID string                `json:"operationId"`
	ResourceID  string                `json:"resourceId"`
	WorkID      string                `json:"workId"`
	IDs         []string              `json:"ids"`
	Handled     int                   `json:"handled"`
	HandledAt   string                `json:"handledAt"`
	Summary     string                `json:"summary"`
	History     string                `json:"history"`
	Queue       *mailboxQueueSnapshot `json:"queue"`
}

type mailboxHandledOperation struct {
	Version       int                      `json:"version"`
	RequestDigest string                   `json:"requestDigest"`
	Result        mailboxAckResult         `json:"result"`
	Messages      []resourceMailboxMessage `json:"messages"`
}

type mailboxReadClaims struct {
	Version   int                      `json:"version"`
	WorkID    string                   `json:"workId"`
	Receipts  map[string]string        `json:"receipts"`
	Messages  []resourceMailboxMessage `json:"messages"`
	Remaining []string                 `json:"remainingIds,omitempty"`
	UpdatedAt string                   `json:"updatedAt"`
}

func batchHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func mailboxOperationPath(directory, operationID string) string {
	return filepath.Join(directory, "handled", "operations", batchHash(operationID)+".json")
}

func mailboxHandledIndexPath(directory, messageID string) string {
	return filepath.Join(directory, "handled", "messages", batchHash(messageID)+".json")
}

func mailboxReadClaimsPath(directory, workID string) string {
	return filepath.Join(directory, "read-claims", batchHash(workID)+".json")
}

func readMailboxOperation(directory, operationID string) (mailboxHandledOperation, bool, error) {
	var operation mailboxHandledOperation
	found, err := readResourceMailboxJSON(mailboxOperationPath(directory, operationID), &operation)
	if found && err == nil && (operation.Version != 1 || operation.Result.OperationID != operationID) {
		err = errors.New("invalid handled operation")
	}
	return operation, found, err
}

func mailboxHandledByID(directory, messageID string) (resourceMailboxMessage, bool, error) {
	var index struct {
		MessageID   string `json:"messageId"`
		OperationID string `json:"operationId"`
	}
	found, err := readResourceMailboxJSON(mailboxHandledIndexPath(directory, messageID), &index)
	if err != nil || !found {
		return resourceMailboxMessage{}, false, err
	}
	if index.MessageID != messageID || index.OperationID == "" {
		return resourceMailboxMessage{}, false, errors.New("invalid handled message reference")
	}
	operation, found, err := readMailboxOperation(directory, index.OperationID)
	if err != nil {
		return resourceMailboxMessage{}, false, err
	}
	if found {
		for _, message := range operation.Messages {
			if message.ID == messageID && message.Status == resourceMessageHandled && message.Handling != nil {
				return cloneMailboxMessage(message), true, nil
			}
		}
	}
	return resourceMailboxMessage{}, false, errors.New("handled message audit is unavailable")
}

func readMailboxClaims(directory, workID string) (mailboxReadClaims, error) {
	claims := mailboxReadClaims{Version: 1, WorkID: workID, Receipts: map[string]string{}, Messages: []resourceMailboxMessage{}}
	found, err := readResourceMailboxJSON(mailboxReadClaimsPath(directory, workID), &claims)
	if !found || err != nil {
		return claims, err
	}
	if claims.Version != 1 || claims.WorkID != workID {
		return mailboxReadClaims{}, errors.New("invalid mailbox read claims")
	}
	if claims.Receipts == nil {
		claims.Receipts = map[string]string{}
	}
	return claims, nil
}

func mailboxHandledDocuments(store resourceMailboxStore) ([]resourceMailboxDocument, error) {
	var documents []resourceMailboxDocument
	if store.BatchCommit != nil {
		operation := *store.BatchCommit
		documents = append(documents, resourceMailboxDocument{value: operation, path: mailboxOperationPath(store.Directory, operation.Result.OperationID)})
		for _, message := range operation.Messages {
			index := map[string]string{"messageId": message.ID, "operationId": operation.Result.OperationID}
			documents = append(documents, resourceMailboxDocument{value: index, path: mailboxHandledIndexPath(store.Directory, message.ID)})
		}
	}
	documents = append(documents, store.BatchExtra...)
	if len(documents) == 0 {
		return nil, nil
	}
	for _, part := range []string{"handled", "handled/operations", "handled/messages", "read-claims"} {
		directory := filepath.Join(store.Directory, part)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, err
		}
		if err := syncResourceMailboxDirectory(filepath.Dir(directory)); err != nil {
			return nil, err
		}
	}
	return documents, nil
}

func batchSecret(directory string) ([]byte, error) {
	path := filepath.Join(directory, "batch-secret")
	secret, err := os.ReadFile(path)
	if err == nil {
		if len(secret) != 32 {
			return nil, errors.New("invalid mailbox batch key")
		}
		return secret, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	secret = make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	if err := writeResourceMailboxFile(path, secret, 0o600); err != nil {
		return nil, err
	}
	return secret, nil
}

func batchReceipt(secret []byte, instance, resource, work, generation string, message resourceMailboxMessage) string {
	claim, _ := json.Marshal([]string{instance, resource, work, generation, message.ID, batchHash(message.Text)})
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(claim)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func validateBatchIDs(ids []string) error {
	if len(ids) == 0 || len(ids) > 100 {
		return &resourceAPIError{Code: "invalid_request", Message: "select between 1 and 100 explicit message IDs"}
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || len(id) > 256 || seen[id] {
			return &resourceAPIError{Code: "invalid_request", Message: "message IDs must be nonempty and unique"}
		}
		seen[id] = true
	}
	return nil
}

func (m *agentManager) activeBatchWork(ctx context.Context, workspace serveWorkspace, resource, workID string, internal bool) (turnWork, turnWorkFile, error) {
	works, err := loadTurnWork(workspace.Path, resource)
	if err != nil {
		return turnWork{}, works, err
	}
	work := works.find(workID)
	if work == nil || work.OutputDone || work.State != "running" || work.Command != nil ||
		work.Context.Protocol != 1 || (internal && work.Context.ChatVisible) {
		return turnWork{}, works, &resourceAPIError{Code: "batch_work_unavailable", Message: "an active fixed-audience work is required; ack requires an internal Turn"}
	}
	exists, archived, _, err := resourceExistsAndArchived(workspace.Path, resource)
	if err != nil || !exists || archived {
		return turnWork{}, works, &resourceAPIError{Code: "batch_work_unavailable", Message: "target resource is unavailable"}
	}
	if detail, task, err := taskDetail(workspace.Path, resource); err != nil {
		return turnWork{}, works, err
	} else if task && detail.State == app.TaskStatePaused {
		return turnWork{}, works, &resourceAPIError{Code: "batch_work_unavailable", Message: "target task is paused"}
	}
	record, found, err := currentResourceGeneration(workspace.Path, resource)
	if err != nil {
		return turnWork{}, works, err
	}
	if !found || record.ReplacementPending || record.GenerationID != work.GenerationID || record.AgentHubSessionID != work.SessionID {
		return turnWork{}, works, &resourceAPIError{Code: "batch_work_unavailable", Message: "work generation is no longer current"}
	}
	_, client, err := m.agentHubRuntimeConfig()
	if err != nil {
		return turnWork{}, works, err
	}
	session, err := client.GetSession(ctx, work.SessionID)
	if err != nil {
		return turnWork{}, works, err
	}
	if work.TurnID == "" || session.CurrentTurnID != work.TurnID ||
		(session.State != "running" && session.State != "waiting_approval") || (internal && len(session.PendingApprovalIDs) != 0) {
		return turnWork{}, works, &resourceAPIError{Code: "batch_work_unavailable", Message: "work is no longer active or approvals are outstanding"}
	}
	return *work, works, nil
}

func (m *agentManager) readBatch(ctx context.Context, workspace serveWorkspace, resource string, request mailboxReadRequest) (mailboxReadResponse, error) {
	response := mailboxReadResponse{Capability: mailboxBatchCapability, Items: []mailboxReadItem{}, RemainingIDs: []string{}}
	if err := validateBatchIDs(request.IDs); err != nil {
		return response, err
	}
	if request.MaxBytes == 0 {
		request.MaxBytes = mailboxReadDefaultBytes
	}
	if request.MaxBytes < 1 || request.MaxBytes > mailboxReadMaxBytes {
		return response, &resourceAPIError{Code: "invalid_request", Message: "maxBytes must be between 1 and 1048576"}
	}
	works, err := loadTurnWork(workspace.Path, resource)
	if err != nil {
		return response, err
	}
	var work turnWork
	if request.WorkID != "" {
		work, works, err = m.activeBatchWork(ctx, workspace, resource, request.WorkID, false)
		if err != nil {
			return response, err
		}
	}
	lock := resourceMailboxLock(workspace.Path, resource)
	lock.Lock()
	store, err := loadResourceMailboxStoreInternal(workspace.Path, resource)
	if err != nil {
		lock.Unlock()
		return response, err
	}
	var claims mailboxReadClaims
	var secret []byte
	if request.WorkID != "" {
		secret, err = batchSecret(store.Directory)
		if err != nil {
			lock.Unlock()
			return response, err
		}
		claims, err = readMailboxClaims(store.Directory, work.ID)
		if err != nil {
			lock.Unlock()
			return response, err
		}
		claims.Messages = []resourceMailboxMessage{}
		claims.Receipts = map[string]string{}
	}
	claimsPath := ""
	if request.WorkID != "" {
		claimsPath = mailboxReadClaimsPath(store.Directory, work.ID)
	}
	byID := map[string]resourceMailboxMessage{}
	for _, message := range store.Mailbox.Messages {
		byID[message.ID] = message
	}
	var source *resourceMailboxMessage
	remaining := request.MaxBytes
	for _, id := range request.IDs {
		message, found := byID[id]
		if !found || message.Status == resourceMessageHandled {
			archived, exists, archiveErr := mailboxHandledByID(store.Directory, id)
			if archiveErr != nil {
				lock.Unlock()
				return response, archiveErr
			}
			if exists {
				message, found = archived, true
			}
		}
		item := mailboxReadItem{resourceMessageResponse: resourceMessageResponse{MessageID: id, ResourceID: resource}}
		if !found {
			item.Reason = "message_not_found"
		} else {
			item.resourceMessageResponse = mailboxMessageResponse(message)
			item.Text = ""
			if message.receipt {
				item.Reason = "body_unavailable_in_receipt"
			} else if len(message.Text) > remaining {
				item.Reason = "body_exceeds_budget"
				response.RemainingIDs = append(response.RemainingIDs, id)
			} else {
				item.Text, item.Complete, item.BodyDigest = message.Text, true, batchHash(message.Text)
				remaining -= len(message.Text)
				item.Reason = batchIneligibleReason(message, works)
				if item.Reason == "" && request.WorkID != "" && !work.Context.ChatVisible {
					item.CanAck = true
					item.ReadReceipt = batchReceipt(secret, store.InstanceID, resource, work.ID, work.GenerationID, message)
					claims.Receipts[message.ID] = item.ReadReceipt
					claims.Messages = append(claims.Messages, cloneMailboxMessage(message))
				}
			}
			if source == nil && message.Sender != nil {
				source = &message
			}
		}
		response.Items = append(response.Items, item)
	}
	response.Queue = queueSnapshot(store.InstanceID, resource, store.Mailbox, source, "", works)
	if request.WorkID != "" {
		claims.UpdatedAt = time.Now().Format(time.RFC3339Nano)
		claims.Remaining = append([]string(nil), response.RemainingIDs...)
		before := cloneResourceMailboxStore(store)
		before.BatchExtra = nil
		store.BatchExtra = []resourceMailboxDocument{{value: claims, path: claimsPath}}
		if err := persistResourceMailboxStore(workspace.Path, store, before); err != nil {
			lock.Unlock()
			return response, err
		}
		store.BatchExtra = nil
	}
	lock.Unlock()
	return response, nil
}

type mailboxAckConflict struct {
	Items map[string]string `json:"items"`
}

func (e *mailboxAckConflict) Error() string {
	return "batch confirmation rejected; no IDs were handled"
}

func (m *agentManager) ackBatch(ctx context.Context, workspace serveWorkspace, resource string, request mailboxAckRequest) (mailboxAckResult, error) {
	var result mailboxAckResult
	if err := validateBatchIDs(request.IDs); err != nil {
		return result, err
	}
	if strings.TrimSpace(request.OperationID) == "" || len(request.OperationID) > 128 || strings.TrimSpace(request.Summary) == "" ||
		len(request.Summary) > 4096 || request.WorkID == "" {
		return result, &resourceAPIError{Code: "invalid_request", Message: "operationId, workId, summary (up to 4096 bytes) and receipts are required"}
	}
	if request.Receipts == nil {
		request.Receipts = map[string]string{}
	}
	if len(request.Receipts) != 0 && len(request.Receipts) != len(request.IDs) {
		return result, &resourceAPIError{Code: "invalid_request", Message: "one receipt per explicit ID is required"}
	}
	for _, id := range request.IDs {
		if len(request.Receipts) != 0 && strings.TrimSpace(request.Receipts[id]) == "" {
			return result, &resourceAPIError{Code: "invalid_request", Message: "one receipt per explicit ID is required"}
		}
	}
	requestData, _ := json.Marshal(request)
	digest := batchHash(string(requestData))
	store, err := loadResourceMailboxStoreForRead(workspace.Path, resource)
	if err != nil {
		return result, err
	}
	if previous, found, err := readMailboxOperation(store.Directory, request.OperationID); err != nil {
		return result, err
	} else if found {
		if previous.RequestDigest != digest {
			return result, &resourceAPIError{Code: "message_conflict", Message: "operationId already committed with different input"}
		}
		return previous.Result, nil
	}
	work, works, err := m.activeBatchWork(ctx, workspace, resource, request.WorkID, true)
	if err != nil {
		return result, err
	}
	history, err := turnWorkReference(workspace.Path, resource, work)
	if err != nil {
		return result, err
	}
	_, err = mutateResourceMailboxStoreForResource(workspace.Path, resource, func(store *resourceMailboxStore) error {
		secret, err := batchSecret(store.Directory)
		if err != nil {
			return err
		}
		claims, claimsErr := readMailboxClaims(store.Directory, work.ID)
		if claimsErr != nil {
			return claimsErr
		}
		conflicts := map[string]string{}
		indexes := map[string]int{}
		for index, message := range store.Mailbox.Messages {
			indexes[message.ID] = index
		}
		for _, id := range request.IDs {
			index, found := indexes[id]
			if !found {
				conflicts[id] = "message_not_waiting"
				if archived, exists, err := mailboxHandledByID(store.Directory, id); err != nil {
					return err
				} else if exists {
					conflicts[id] = "already_handled_by:" + archived.Handling.OperationID
				}
				continue
			}
			message := store.Mailbox.Messages[index]
			if reason := batchIneligibleReason(message, works); reason != "" {
				conflicts[id] = reason
				if message.Handling != nil {
					conflicts[id] = "already_handled_by:" + message.Handling.OperationID
				}
				continue
			}
			expected := batchReceipt(secret, store.InstanceID, resource, work.ID, work.GenerationID, message)
			receipt := request.Receipts[id]
			if receipt == "" {
				receipt = claims.Receipts[id]
			}
			if !hmac.Equal([]byte(expected), []byte(receipt)) {
				conflicts[id] = "complete_body_receipt_required"
			}
		}
		if len(conflicts) != 0 {
			return &mailboxAckConflict{Items: conflicts}
		}
		now := time.Now().Format(time.RFC3339Nano)
		operation := mailboxHandledOperation{Version: 1, RequestDigest: digest}
		for _, id := range request.IDs {
			message := &store.Mailbox.Messages[indexes[id]]
			message.Status, message.TerminalAt, message.UpdatedAt = resourceMessageHandled, now, now
			message.Handling = &mailboxHandling{OperationID: request.OperationID, WorkID: work.ID, GenerationID: work.GenerationID,
				HandledAt: now, Summary: request.Summary, History: history, BodyDigest: batchHash(message.Text)}
			operation.Messages = append(operation.Messages, cloneMailboxMessage(*message))
		}
		first := store.Mailbox.Messages[indexes[request.IDs[0]]]
		result = mailboxAckResult{Capability: mailboxBatchCapability, OperationID: request.OperationID, ResourceID: resource,
			WorkID: work.ID, IDs: append([]string(nil), request.IDs...), Handled: len(request.IDs), HandledAt: now,
			Summary: request.Summary, History: history, Queue: queueSnapshot(store.InstanceID, resource, store.Mailbox, &first, "", works)}
		operation.Result = result
		store.BatchCommit = &operation
		return nil
	})
	return result, err
}

type mailboxListCursor struct {
	Version       int    `json:"version"`
	InstanceID    string `json:"instanceId"`
	ResourceID    string `json:"resourceId"`
	FromResource  string `json:"fromResource,omitempty"`
	FromWorkspace string `json:"fromWorkspaceInstanceId,omitempty"`
	Upper         uint64 `json:"upper"`
	After         uint64 `json:"after"`
	Limit         int    `json:"limit"`
}

type mailboxListItem struct {
	MessageID                 string                 `json:"messageId"`
	Sequence                  uint64                 `json:"sequence"`
	Status                    string                 `json:"status"`
	Role                      string                 `json:"role"`
	Type                      string                 `json:"type,omitempty"`
	Sender                    *agentHubMessageSender `json:"sender,omitempty"`
	SenderWorkspaceInstanceID string                 `json:"senderWorkspaceInstanceId,omitempty"`
	AcceptedAt                string                 `json:"acceptedAt"`
	BodyBytes                 int                    `json:"bodyBytes"`
	CanAck                    bool                   `json:"canAck"`
	Reason                    string                 `json:"reason,omitempty"`
}

type mailboxListResponse struct {
	Capability    string                `json:"capability"`
	Items         []mailboxListItem     `json:"items"`
	NextCursor    string                `json:"nextCursor,omitempty"`
	UpperSequence uint64                `json:"upperSequence"`
	Queue         *mailboxQueueSnapshot `json:"queue"`
}

func listBatch(workspace serveWorkspace, resource string, request *http.Request) (mailboxListResponse, error) {
	response := mailboxListResponse{Capability: mailboxBatchCapability, Items: []mailboxListItem{}}
	query := request.URL.Query()
	for key := range query {
		if key != "cursor" && key != "limit" && key != "fromResource" && key != "fromWorkspaceInstanceId" &&
			key != "status" && key != "role" {
			return response, &resourceAPIError{Code: "invalid_request", Message: "unknown queue query parameter"}
		}
	}
	if (query.Get("status") != "" && query.Get("status") != "waiting") || (query.Get("role") != "" && query.Get("role") != "agent") {
		return response, &resourceAPIError{Code: "invalid_request", Message: "queue list supports waiting Agent messages"}
	}
	store, err := loadResourceMailboxStoreForRead(workspace.Path, resource)
	if err != nil {
		return response, err
	}
	cursor := mailboxListCursor{Version: 1, InstanceID: store.InstanceID, ResourceID: resource,
		FromResource: query.Get("fromResource"), FromWorkspace: query.Get("fromWorkspaceInstanceId"), Upper: store.Mailbox.NextSequence, Limit: 20}
	if value := query.Get("cursor"); value != "" {
		data, decodeErr := base64.RawURLEncoding.DecodeString(value)
		if len(value) > 4096 || decodeErr != nil || json.Unmarshal(data, &cursor) != nil || cursor.Version != 1 ||
			cursor.InstanceID != store.InstanceID || cursor.ResourceID != resource || cursor.Upper > store.Mailbox.NextSequence ||
			cursor.After > cursor.Upper || cursor.Limit < 1 || cursor.Limit > 100 {
			return response, &resourceAPIError{Code: "invalid_request", Message: "invalid queue cursor"}
		}
		for key, value := range map[string]string{"fromResource": cursor.FromResource, "fromWorkspaceInstanceId": cursor.FromWorkspace, "limit": strconv.Itoa(cursor.Limit)} {
			if query.Has(key) && query.Get(key) != value {
				return response, &resourceAPIError{Code: "invalid_request", Message: "queue cursor filters cannot change"}
			}
		}
	} else {
		if value := query.Get("limit"); value != "" {
			cursor.Limit, err = strconv.Atoi(value)
			if err != nil || cursor.Limit < 1 || cursor.Limit > 100 {
				return response, &resourceAPIError{Code: "invalid_request", Message: "queue limit must be between 1 and 100"}
			}
		}
		if cursor.FromResource != "" {
			if !isStablePUAResourceID(cursor.FromResource) {
				return response, &resourceAPIError{Code: "invalid_request", Message: "fromResource must be a stable resource ID"}
			}
			if cursor.FromWorkspace == "" {
				cursor.FromWorkspace = store.InstanceID
			}
		}
	}
	works, err := loadTurnWork(workspace.Path, resource)
	if err != nil {
		return response, err
	}
	source := &resourceMailboxMessage{Sender: &agentHubMessageSender{ID: cursor.FromResource}, SenderWorkspaceInstanceID: cursor.FromWorkspace}
	response.Queue = queueSnapshot(store.InstanceID, resource, store.Mailbox, source, "", works)
	response.UpperSequence = cursor.Upper
	sort.Slice(store.Mailbox.Messages, func(first, second int) bool {
		return store.Mailbox.Messages[first].Sequence < store.Mailbox.Messages[second].Sequence
	})
	for _, message := range store.Mailbox.Messages {
		if message.Sequence <= cursor.After || message.Sequence > cursor.Upper || message.Status != resourceMessageQueued || message.Role != "agent" ||
			(cursor.FromResource != "" && (message.Sender == nil || message.Sender.ID != cursor.FromResource)) ||
			(cursor.FromWorkspace != "" && message.SenderWorkspaceInstanceID != cursor.FromWorkspace) {
			continue
		}
		reason := batchIneligibleReason(message, works)
		response.Items = append(response.Items, mailboxListItem{MessageID: message.ID, Sequence: message.Sequence, Status: "waiting",
			Role: message.Role, Type: message.Type, Sender: message.Sender, SenderWorkspaceInstanceID: message.SenderWorkspaceInstanceID,
			AcceptedAt: message.AcceptedAt, BodyBytes: len(message.Text), CanAck: reason == "", Reason: reason})
		cursor.After = message.Sequence
		if len(response.Items) == cursor.Limit {
			for _, remaining := range store.Mailbox.Messages {
				if remaining.Sequence <= cursor.After || remaining.Sequence > cursor.Upper || remaining.Status != resourceMessageQueued || remaining.Role != "agent" ||
					(cursor.FromResource != "" && (remaining.Sender == nil || remaining.Sender.ID != cursor.FromResource)) ||
					(cursor.FromWorkspace != "" && remaining.SenderWorkspaceInstanceID != cursor.FromWorkspace) {
					continue
				}
				data, _ := json.Marshal(cursor)
				response.NextCursor = base64.RawURLEncoding.EncodeToString(data)
				break
			}
			break
		}
	}
	return response, nil
}

func writeBatchError(w http.ResponseWriter, err error) {
	var conflict *mailboxAckConflict
	if errors.As(err, &conflict) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "batch_conflict", "message": conflict.Error(), "items": conflict.Items}})
		return
	}
	writeError(w, err, resourceErrorStatus(err))
}

func (m *agentManager) handleMailboxBatch(w http.ResponseWriter, r *http.Request, workspaceID, resource, action string) {
	workspace, err := m.server.workspace(workspaceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	if !isStablePUAResourceID(resource) {
		writeError(w, errors.New("invalid resource"), http.StatusBadRequest)
		return
	}
	var response any
	if action == "list" && r.Method == http.MethodGet {
		response, err = listBatch(workspace, resource, r)
	} else if (action == "read" || action == "ack") && r.Method == http.MethodPost {
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128*1024))
		decoder.DisallowUnknownFields()
		if action == "read" {
			var body mailboxReadRequest
			if err = decoder.Decode(&body); err == nil {
				err = m.withResourceController(r.Context(), workspace, resource, func() error {
					var callErr error
					response, callErr = m.readBatch(r.Context(), workspace, resource, body)
					return callErr
				})
			}
		} else {
			var body mailboxAckRequest
			if err = decoder.Decode(&body); err == nil {
				err = m.withResourceController(r.Context(), workspace, resource, func() error {
					var callErr error
					response, callErr = m.ackBatch(r.Context(), workspace, resource, body)
					return callErr
				})
			}
		}
	} else if strings.HasPrefix(action, "operation:") && r.Method == http.MethodGet {
		var store resourceMailboxStore
		store, err = loadResourceMailboxStoreForRead(workspace.Path, resource)
		if err == nil {
			var operation mailboxHandledOperation
			var found bool
			operation, found, err = readMailboxOperation(store.Directory, strings.TrimPrefix(action, "operation:"))
			if err == nil && !found {
				err = &resourceAPIError{Code: "message_not_found", Message: "batch operation not found"}
			}
			response = operation.Result
		}
	} else {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		var apiErr *resourceAPIError
		var conflict *mailboxAckConflict
		if !errors.As(err, &apiErr) && !errors.As(err, &conflict) && strings.Contains(err.Error(), "invalid character") {
			err = &resourceAPIError{Code: "invalid_request", Message: "invalid batch request"}
		}
		writeBatchError(w, err)
		return
	}
	writeJSON(w, response)
}
