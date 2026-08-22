package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/localize"
)

const (
	agentHubOpaqueMessageSchema = 2
	puaMessagePayloadSchema     = "pua.resource-message.v1"

	providerResponseProgressFinal = "progress_final"
	providerResponseFinalOnly     = "final_only"
	providerResponseNone          = "none"
)

// puaMessagePayload is PUA-owned application metadata. AgentHub persists the
// JSON value but does not interpret any field in it.
type puaMessagePayload struct {
	Schema                    string                    `json:"schema"`
	Text                      string                    `json:"text"`
	Role                      string                    `json:"role"`
	Sender                    *agentHubMessageSender    `json:"sender,omitempty"`
	SenderWorkspaceInstanceID string                    `json:"senderWorkspaceInstanceId,omitempty"`
	Type                      string                    `json:"type,omitempty"`
	Causation                 *resourceMessageCausation `json:"causation,omitempty"`
}

func providerMessageText(text, role string, sender *agentHubMessageSender, steer bool) string {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		role = "user"
	}
	sender = normalizedMessageSender(sender)
	if role == "user" && sender == nil && !steer {
		return text
	}
	header := "Message from " + role
	if name := providerMessageSenderName(sender); name != "" {
		header += " " + strconv.QuoteToGraphic(name)
	}
	if steer {
		header += " (steer)"
	}
	return header + ":\n" + text
}

func normalizedMessageSender(sender *agentHubMessageSender) *agentHubMessageSender {
	if sender == nil {
		return nil
	}
	normalized := &agentHubMessageSender{
		ID: strings.TrimSpace(sender.ID), Name: strings.TrimSpace(sender.Name), SessionID: strings.TrimSpace(sender.SessionID),
	}
	if normalized.ID == "" && normalized.Name == "" && normalized.SessionID == "" {
		return nil
	}
	return normalized
}

func providerMessageSenderName(sender *agentHubMessageSender) string {
	if sender == nil {
		return ""
	}
	for _, value := range []string{sender.Name, sender.ID, sender.SessionID} {
		if value != "" {
			return value
		}
	}
	return ""
}

func normalizedProviderMessageRole(role string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		return "user"
	}
	return role
}

func providerMessageParty(role string, sender *agentHubMessageSender, language string) string {
	role = normalizedProviderMessageRole(role)
	label := role
	if language == localize.SimplifiedChinese {
		switch role {
		case "user":
			label = "用户"
		case "agent":
			label = "Agent"
		case "system":
			label = "系统"
		}
	}
	if name := providerMessageSenderName(normalizedMessageSender(sender)); name != "" {
		label += " " + strconv.QuoteToGraphic(name)
	}
	return label
}

func providerResponseLabel(language, response string) string {
	if language == localize.SimplifiedChinese {
		switch response {
		case providerResponseProgressFinal:
			return "[发送方接收：进度 + 最终回复]"
		case providerResponseFinalOnly:
			return "[发送方接收：仅最终回复]"
		default:
			return "[发送方接收：无]"
		}
	}
	switch response {
	case providerResponseProgressFinal:
		return "[sender receives: progress + final reply]"
	case providerResponseFinalOnly:
		return "[sender receives: final reply only]"
	default:
		return "[sender receives: no progress or final reply]"
	}
}

func chineseProviderMessageSource(party, kind string) string {
	if strings.HasPrefix(party, "Agent") {
		return "来自 " + party + " 的" + kind
	}
	return "来自" + party + " 的" + kind
}

func providerMessageReplyTarget(role string, sender *agentHubMessageSender) (string, bool) {
	role = normalizedProviderMessageRole(role)
	sender = normalizedMessageSender(sender)
	if sender == nil {
		return "", false
	}
	switch role {
	case "agent":
		if isStablePUAResourceID(sender.ID) {
			return sender.ID, true
		}
	case "user":
		if app.ValidateUserName(sender.Name) == nil {
			return sender.Name, true
		}
	}
	return "", false
}

func sameProviderMessageParty(leftRole string, left *agentHubMessageSender, rightRole string, right *agentHubMessageSender) bool {
	leftRole, rightRole = normalizedProviderMessageRole(leftRole), normalizedProviderMessageRole(rightRole)
	if leftRole != rightRole {
		return false
	}
	leftTarget, leftOK := providerMessageReplyTarget(leftRole, left)
	rightTarget, rightOK := providerMessageReplyTarget(rightRole, right)
	if leftOK || rightOK {
		return leftOK && rightOK && leftTarget == rightTarget
	}
	return reflect.DeepEqual(normalizedMessageSender(left), normalizedMessageSender(right))
}

func providerMessageCanDeliverAgentResult(message resourceMailboxMessage) bool {
	if normalizedProviderMessageRole(message.Role) != "agent" || !message.SubscribeResult ||
		message.Sender == nil || !isStablePUAResourceID(message.Sender.ID) || strings.TrimSpace(message.SenderWorkspaceInstanceID) == "" {
		return false
	}
	switch message.ResultSubscriptionStatus {
	case resourceResultSubscriptionDisabled, resourceResultSubscriptionNone:
		return false
	default:
		return true
	}
}

func providerOpenerResponse(role string, message *resourceMailboxMessage) string {
	switch normalizedProviderMessageRole(role) {
	case "user":
		return providerResponseProgressFinal
	case "agent":
		if message != nil && providerMessageCanDeliverAgentResult(*message) {
			return providerResponseFinalOnly
		}
	}
	return providerResponseNone
}

func providerMessageTextWithContext(message resourceMailboxMessage, steer bool) string {
	context := message.ProviderContext
	if context == nil {
		return providerMessageText(message.Text, message.Role, message.Sender, steer)
	}
	language := context.Language
	from := providerMessageParty(message.Role, message.Sender, language)
	if !steer {
		if language == localize.SimplifiedChinese {
			return chineseProviderMessageSource(from, "消息") + " " + providerResponseLabel(language, context.OpenerResponse) + "：\n" + message.Text
		}
		return "Message from " + from + " " + providerResponseLabel(language, context.OpenerResponse) + ":\n" + message.Text
	}
	if sameProviderMessageParty(message.Role, message.Sender, context.OpenerRole, context.OpenerSender) {
		if language == localize.SimplifiedChinese {
			return chineseProviderMessageSource(from, "插入消息") + "：\n" + message.Text
		}
		return "Inserted message from " + from + ":\n" + message.Text
	}
	opener := providerMessageParty(context.OpenerRole, context.OpenerSender, language) + " " + providerResponseLabel(language, context.OpenerResponse)
	replyTarget, canReply := providerMessageReplyTarget(message.Role, message.Sender)
	if language == localize.SimplifiedChinese {
		clauses := make([]string, 0, 2)
		if canReply {
			clauses = append(clauses, "回复请使用 `pua message send --to="+replyTarget+" '<reply>'`")
		}
		clauses = append(clauses, "当前会话："+opener)
		return chineseProviderMessageSource(from, "插入消息") + "（" + strings.Join(clauses, "。") + "）：\n" + message.Text
	}
	clauses := make([]string, 0, 2)
	if canReply {
		clauses = append(clauses, "Reply via `pua message send --to="+replyTarget+" '<reply>'`")
	}
	clauses = append(clauses, "Current conversation: "+opener)
	return "Inserted message from " + from + " (" + strings.Join(clauses, ". ") + "):\n" + message.Text
}

func providerContextOpenerMessage(workspacePath, resourceID, sessionID, turnID, messageID string) *resourceMailboxMessage {
	message, found, err := mailboxMessageByID(workspacePath, messageID)
	if err != nil || !found || normalizedResourceID(message.ResourceID) != normalizedResourceID(resourceID) {
		return nil
	}
	if value := strings.TrimSpace(message.AgentHubSessionID); value != "" && value != strings.TrimSpace(sessionID) {
		return nil
	}
	if value := strings.TrimSpace(message.TurnID); value != "" && value != strings.TrimSpace(turnID) {
		return nil
	}
	return &message
}

func providerContextMailboxOpener(workspacePath, resourceID, generationID, sessionID, turnID string, beforeSequence uint64) *resourceMailboxMessage {
	mailbox, err := loadResourceMailboxForResource(workspacePath, resourceID)
	if err != nil {
		return nil
	}
	var selected resourceMailboxMessage
	found := false
	for _, candidate := range mailbox.Messages {
		if candidate.Sequence >= beforeSequence || candidate.ActualMode == resourceMessageModeSteer ||
			normalizedResourceID(candidate.ResourceID) != normalizedResourceID(resourceID) ||
			(candidate.Status != resourceMessageDelivered && candidate.Status != resourceMessageDelivering) {
			continue
		}
		if generationID != "" && candidate.GenerationID != generationID {
			continue
		}
		if value := strings.TrimSpace(candidate.AgentHubSessionID); value != "" && value != strings.TrimSpace(sessionID) {
			continue
		}
		if value := strings.TrimSpace(candidate.TurnID); value != "" && strings.TrimSpace(turnID) != "" && value != strings.TrimSpace(turnID) {
			continue
		}
		if !found || candidate.Sequence > selected.Sequence {
			selected, found = candidate, true
		}
	}
	if !found {
		return nil
	}
	return &selected
}

func (m *agentManager) ensureProviderMessageContext(ctx context.Context, workspace serveWorkspace, client *agentHubClient, session agentHubSession, generationID string, message resourceMailboxMessage) (resourceMailboxMessage, error) {
	if message.ProviderContext != nil {
		return message, nil
	}
	language, err := workspaceContentLanguage(workspace.Path)
	if err != nil {
		return resourceMailboxMessage{}, err
	}
	contextValue := providerMessageContext{
		Language: language, OpenerRole: normalizedProviderMessageRole(message.Role),
		OpenerSender: normalizedMessageSender(message.Sender), OpenerResponse: providerOpenerResponse(message.Role, &message),
	}
	if message.ActualMode == resourceMessageModeSteer {
		turnID := strings.TrimSpace(message.TurnID)
		if turnID == "" {
			turnID = strings.TrimSpace(session.CurrentTurnID)
		}
		if turnID == "" {
			return resourceMailboxMessage{}, fmt.Errorf("prepare inserted message %s: active Turn id is empty", message.ID)
		}
		contextValue.TurnID = turnID
		openerMessage := providerContextMailboxOpener(workspace.Path, message.ResourceID, generationID, session.ID, turnID, message.Sequence)
		if openerMessage == nil {
			turn, _, turnErr := client.SessionTurn(ctx, session.ID, turnID)
			if turnErr != nil {
				return resourceMailboxMessage{}, fmt.Errorf("prepare inserted message %s Turn context: %w", message.ID, turnErr)
			}
			_, openerRole, openerSender := puaMessagePresentation(turn.TriggerPreview, turn.TriggerRole, turn.TriggerSender, turn.TriggerPayload)
			contextValue.OpenerRole = normalizedProviderMessageRole(openerRole)
			contextValue.OpenerSender = normalizedMessageSender(openerSender)
			openerMessage = providerContextOpenerMessage(workspace.Path, message.ResourceID, session.ID, turnID, turn.TriggerMessageID)
		} else {
			contextValue.OpenerRole = normalizedProviderMessageRole(openerMessage.Role)
			contextValue.OpenerSender = normalizedMessageSender(openerMessage.Sender)
		}
		contextValue.OpenerResponse = providerOpenerResponse(contextValue.OpenerRole, openerMessage)
	}
	return updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
		if current.ProviderContext == nil {
			current.ProviderContext = cloneProviderMessageContext(&contextValue)
		}
	})
}

func puaPayloadForMailboxMessage(message resourceMailboxMessage) puaMessagePayload {
	return puaMessagePayload{
		Schema: puaMessagePayloadSchema, Text: message.Text, Role: message.Role,
		Sender: normalizedMessageSender(message.Sender), SenderWorkspaceInstanceID: message.SenderWorkspaceInstanceID,
		Type: message.Type, Causation: message.Causation,
	}
}

func marshalPUAMessagePayload(payload puaMessagePayload) (json.RawMessage, error) {
	encoded, err := json.Marshal(payload)
	return json.RawMessage(encoded), err
}

func agentHubMailboxMessage(message resourceMailboxMessage) (agentHubInboundMessage, error) {
	payload, err := marshalPUAMessagePayload(puaPayloadForMailboxMessage(message))
	if err != nil {
		return agentHubInboundMessage{}, err
	}
	steer := message.ActualMode == resourceMessageModeSteer
	return agentHubInboundMessage{
		SchemaVersion: agentHubOpaqueMessageSchema,
		Text:          providerMessageTextWithContext(message, steer),
		Payload:       payload,
		Steer:         steer,
		MessageID:     message.ID,
	}, nil
}

func decodePUAMessagePayload(raw json.RawMessage) (puaMessagePayload, bool) {
	if len(raw) == 0 {
		return puaMessagePayload{}, false
	}
	var payload puaMessagePayload
	if json.Unmarshal(raw, &payload) != nil || payload.Schema != puaMessagePayloadSchema {
		return puaMessagePayload{}, false
	}
	return payload, true
}

// canonicalAgentHubMessageMatches accepts both the v2 representation and an
// equivalent v1 input that may have been persisted before a rolling upgrade.
func canonicalAgentHubMessageMatches(canonical agentHubInboundMessage, expected resourceMailboxMessage) bool {
	expectedWire, err := agentHubMailboxMessage(expected)
	if err != nil {
		return false
	}
	if canonical.SchemaVersion == agentHubOpaqueMessageSchema {
		actualPayload, ok := decodePUAMessagePayload(canonical.Payload)
		expectedPayload, expectedOK := decodePUAMessagePayload(expectedWire.Payload)
		expectedText := providerMessageTextWithContext(expected, canonical.Steer)
		legacyText := providerMessageText(expected.Text, expected.Role, expected.Sender, canonical.Steer)
		return ok && expectedOK && (canonical.Text == expectedText || canonical.Text == legacyText) &&
			canonical.MessageID == expectedWire.MessageID && reflect.DeepEqual(actualPayload, expectedPayload)
	}
	role := canonical.Role
	if role == "" {
		role = "user"
	}
	return canonical.Text == expected.Text && role == expected.Role && reflect.DeepEqual(canonical.Sender, normalizedMessageSender(expected.Sender)) &&
		canonical.MessageID == expected.ID
}

func puaMessagePresentation(text, role string, sender *agentHubMessageSender, payload json.RawMessage) (string, string, *agentHubMessageSender) {
	if decoded, ok := decodePUAMessagePayload(payload); ok {
		return decoded.Text, decoded.Role, decoded.Sender
	}
	if role == "" {
		role = "user"
	}
	return text, role, sender
}
