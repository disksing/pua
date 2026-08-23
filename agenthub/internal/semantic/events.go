package semantic

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/disksing/pua/agenthub/internal/session"
)

const (
	EventsSchema      = "agenthub.semantic-events.v1"
	EventDetailSchema = "agenthub.event-detail.v1"
	maxSummary        = 120
	maxOutput         = 12000
)

type Source struct {
	EventID   int64  `json:"eventId"`
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId,omitempty"`
	Time      string `json:"time"`
	StartTime string `json:"startTime,omitempty"`
}

type Event struct {
	ID            string `json:"id"`
	SourceEventID int64  `json:"sourceEventId"`
	Index         int    `json:"index"`
	Time          string `json:"time"`
	StartTime     string `json:"startTime,omitempty"`
	Type          string `json:"type"`
	SessionID     string `json:"sessionId"`
	TurnID        string `json:"turnId,omitempty"`
	Data          any    `json:"data,omitempty"`
}

type Frame struct {
	Schema string  `json:"schema"`
	Cursor int64   `json:"cursor"`
	Mode   string  `json:"mode"`
	Source Source  `json:"source"`
	Events []Event `json:"events"`
}

type Detail struct {
	Schema      string        `json:"schema"`
	SourceEvent session.Event `json:"sourceEvent"`
	Frame       Frame         `json:"frame"`
}

// FrameFor projects one durable source event into the provider-neutral public
// protocol. appendMode is used only for a live folded-delta patch; durable
// reads and reconnect replay always use replace.
func FrameFor(source session.Event, appendMode bool) Frame {
	mode := "replace"
	if appendMode {
		mode = "append"
	}
	timeValue := source.Time.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
	startTime := ""
	if source.StartTime != nil {
		startTime = source.StartTime.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
	}
	frame := Frame{
		Schema: EventsSchema,
		Cursor: source.ID,
		Mode:   mode,
		Source: Source{EventID: source.ID, Type: source.Type, SessionID: source.SessionID, TurnID: source.TurnID, Time: timeValue, StartTime: startTime},
		Events: []Event{},
	}
	for index, value := range normalize(source) {
		value.ID = fmt.Sprintf("sem_%d_%d", source.ID, index)
		value.SourceEventID = source.ID
		value.Index = index
		value.Time = timeValue
		value.StartTime = startTime
		value.SessionID = source.SessionID
		value.TurnID = source.TurnID
		frame.Events = append(frame.Events, value)
	}
	return frame
}

func FramesFor(events []session.Event) []Frame {
	frames := make([]Frame, 0, len(events))
	for _, event := range events {
		frames = append(frames, FrameFor(event, false))
	}
	return frames
}

func normalize(source session.Event) []Event {
	data := object(source.Data)
	switch source.Type {
	case "provider.event", "provider.metadata", "provider.stderr", "provider.turn.started", "provider.turn.completed", "message.delivery", "plan.event":
		return nil
	case "tool.event":
		if legacyToolEnvelopeIsNoise(data) {
			return nil
		}
		value := toolCallFromLegacy(data)
		if value == nil {
			return unknown(source.Type, "normalization_failed", "invalid_provider_payload")
		}
		ensureCallID(value, source.ID)
		return []Event{{Type: "tool.call", Data: value}}
	case "tool.call":
		value := publicToolCall(data)
		if value == nil {
			return unknown(source.Type, "normalization_failed", "invalid_tool_call")
		}
		ensureCallID(value, source.ID)
		return []Event{{Type: "tool.call", Data: value}}
	case "message.user", "message.user.steer":
		return []Event{{Type: "message.input", Data: map[string]any{
			"role": "user", "text": textValue(data["text"]), "steer": source.Type == "message.user.steer",
		}}}
	case "message.input":
		return []Event{{Type: source.Type, Data: messageInput(data)}}
	case "message.assistant.delta", "message.reasoning.delta":
		return []Event{{Type: source.Type, Data: map[string]any{"text": textValue(data["text"])}}}
	case "approval.requested":
		return []Event{{Type: source.Type, Data: approvalRequest(data)}}
	case "approval.resolved":
		return []Event{{Type: source.Type, Data: selectFields(data, "approvalId", "decision", "optionId", "text", "reason")}}
	case "provider.error":
		return []Event{{Type: source.Type, Data: providerError(data)}}
	case "turn.started", "turn.completed", "turn.failed", "turn.cancelled",
		"session.created", "session.provider", "session.state", "session.archived",
		"session.agent", "session.launch-environment", session.EventEphemeralEnvironmentRequired:
		return []Event{{Type: source.Type, Data: canonicalData(source.Type, data)}}
	default:
		if strings.HasPrefix(source.Type, "provider.process.") {
			return nil
		}
		return unknown(source.Type, "unsupported_event_type", "")
	}
}

func legacyToolEnvelopeIsNoise(data map[string]any) bool {
	raw := object(data["raw"])
	item := raw
	if nested := object(raw["item"]); len(nested) > 0 {
		item = nested
	}
	switch stringValue(item["type"]) {
	case "userMessage", "agentMessage", "reasoning":
		return true
	default:
		return false
	}
}

func unknown(sourceType, reason, code string) []Event {
	data := map[string]any{"sourceType": sourceType, "reason": reason}
	if code != "" {
		data["code"] = code
	}
	return []Event{{Type: "unknown", Data: data}}
}

func messageInput(data map[string]any) map[string]any {
	result := selectFields(data, "schemaVersion", "text", "payload", "role", "sender", "steer", "messageId", "replyTo", "correlationId")
	if stringValue(result["role"]) == "" {
		result["role"] = "user"
	}
	return result
}

func providerError(data map[string]any) map[string]any {
	result := make(map[string]any)
	for _, field := range []string{"message", "details", "reason", "code"} {
		putNullableString(result, data, field)
	}
	if value, ok := data["retryable"].(bool); ok {
		result["retryable"] = value
	} else if value, ok := data["willRetry"].(bool); ok {
		result["retryable"] = value
	}
	return result
}

func canonicalData(eventType string, data map[string]any) map[string]any {
	switch eventType {
	case "turn.failed":
		result := make(map[string]any)
		for _, field := range []string{"error", "message", "reason"} {
			putNullableString(result, data, field)
		}
		return result
	case "turn.cancelled":
		return selectFields(data, "reason")
	case "session.provider":
		return selectFields(data, "agentName", "provider", "providerSessionId", "inputCapabilities")
	case "session.state":
		return selectFields(data, "state", "reason")
	case "session.agent":
		return selectFields(data, "agentName")
	case "session.launch-environment":
		return selectFields(data, "environment")
	case session.EventEphemeralEnvironmentRequired:
		return selectFields(data, "required")
	case "session.created":
		return withoutRaw(data)
	default:
		return map[string]any{}
	}
}

// ToolCallData builds the new durable provider-neutral tool.call payload. Raw
// input is retained only as a diagnostic sidecar; publicToolCall strips it
// from every SemanticFrame.
func ToolCallData(method string, raw json.RawMessage) map[string]any {
	payload := object(raw)
	result := toolCall(method, payload)
	if result == nil {
		result = map[string]any{"schemaVersion": 1, "operation": operationFor(method), "name": "Tool", "status": statusFor(method, "")}
	}
	result["raw"] = map[string]any{"method": method, "payload": payload}
	return result
}

func toolCallFromLegacy(data map[string]any) map[string]any {
	method := stringValue(data["method"])
	raw := object(data["raw"])
	return toolCall(method, raw)
}

func toolCall(method string, raw map[string]any) map[string]any {
	if strings.HasPrefix(method, "item/") || strings.HasPrefix(method, "command/") {
		if method == "item/commandExecution/outputDelta" || method == "command/exec/outputDelta" {
			callID := firstString(raw["itemId"], raw["callId"], raw["id"])
			if callID == "" {
				return nil
			}
			return map[string]any{
				"schemaVersion": 1, "callId": callID, "operation": "update",
				"output": map[string]any{"mode": "append", "text": textValue(raw["delta"])},
			}
		}
		item := raw
		if nested := object(raw["item"]); len(nested) > 0 {
			item = nested
		}
		itemType := stringValue(item["type"])
		if itemType == "userMessage" || itemType == "agentMessage" || itemType == "reasoning" {
			return nil
		}
		callID := firstString(item["id"], raw["itemId"])
		kind, name, summary, output, errorMessage := codexTool(itemType, item)
		status := normalizeStatus(stringValue(item["status"]))
		operation := operationFor(method)
		if method == "item/started" {
			status = "running"
		}
		if operation == "finish" && status == "running" {
			status = "completed"
		}
		if errorMessage != "" {
			status = "failed"
		}
		result := map[string]any{"schemaVersion": 1, "callId": callID, "operation": operation, "toolKind": kind, "name": name, "status": status}
		putString(result, "summary", truncate(strings.Join(strings.Fields(summary), " "), maxSummary))
		putOutput(result, "replace", output)
		putError(result, errorMessage)
		return result
	}

	update := raw
	if nested := object(raw["update"]); len(nested) > 0 {
		update = nested
	}
	kind := stringValue(update["sessionUpdate"])
	if kind == "tool_call" || kind == "tool_call_update" {
		callID := firstString(update["toolCallId"], update["id"])
		input := object(update["rawInput"])
		name := humanize(stringValue(update["kind"]))
		if name == "" {
			name = "Tool"
		}
		summary := firstString(update["title"], command(input["command"]), input["path"], input["filePath"], humanize(stringValue(update["kind"])))
		status := normalizeStatus(stringValue(update["status"]))
		operation := "update"
		if kind == "tool_call" {
			operation = "start"
		}
		if status == "completed" || status == "failed" {
			operation = "finish"
		}
		result := map[string]any{"schemaVersion": 1, "callId": callID, "operation": operation, "toolKind": toolKind(stringValue(update["kind"])), "name": name, "status": status}
		putString(result, "summary", truncate(strings.Join(strings.Fields(summary), " "), maxSummary))
		putOutput(result, "replace", contentText(update["content"]))
		return result
	}

	if method == "tool_execution_start" || method == "tool_execution_end" {
		toolName := firstString(raw["toolName"], raw["name"], raw["tool"])
		args := object(raw["args"])
		summary := firstString(command(args["command"]), args["path"], args["filePath"])
		errorMessage := firstString(raw["error"])
		failed, _ := raw["isError"].(bool)
		if errorMessage != "" {
			failed = true
		}
		status := "running"
		operation := "start"
		if method == "tool_execution_end" {
			operation, status = "finish", "completed"
			if failed {
				status = "failed"
			}
		}
		result := map[string]any{"schemaVersion": 1, "callId": firstString(raw["toolCallId"], raw["callId"]), "operation": operation, "toolKind": toolKind(toolName), "name": firstString(humanize(toolName), "Tool"), "status": status}
		putString(result, "summary", truncate(strings.Join(strings.Fields(summary), " "), maxSummary))
		output := textValue(raw["result"])
		if output == "" {
			output = contentText(object(raw["result"])["content"])
		}
		putOutput(result, "replace", output)
		putError(result, errorMessage)
		return result
	}

	return map[string]any{
		"schemaVersion": 1,
		"callId":        firstString(raw["toolCallId"], raw["itemId"], raw["id"]),
		"operation":     operationFor(method),
		"toolKind":      "other",
		"name":          "Tool",
		"summary":       method,
		"status":        statusFor(method, ""),
	}
}

func publicToolCall(data map[string]any) map[string]any {
	if len(data) == 0 {
		return nil
	}
	result := map[string]any{"schemaVersion": 1, "callId": stringValue(data["callId"])}
	operation := stringValue(data["operation"])
	if operation != "start" && operation != "finish" {
		operation = "update"
	}
	result["operation"] = operation
	for _, field := range []string{"toolKind", "name", "summary"} {
		putNullableString(result, data, field)
	}
	if value, exists := data["status"]; exists {
		if value == nil {
			result["status"] = nil
		} else {
			result["status"] = normalizeStatus(stringValue(value))
		}
	}
	if value, exists := data["output"]; exists {
		if value == nil {
			result["output"] = nil
		} else if output := object(value); len(output) > 0 {
			mode := stringValue(output["mode"])
			if mode != "append" {
				mode = "replace"
			}
			public := map[string]any{"mode": mode, "text": textValue(output["text"])}
			if truncated, ok := output["truncated"].(bool); ok && truncated {
				public["truncated"] = true
			}
			result["output"] = public
		}
	}
	if value, exists := data["error"]; exists {
		if value == nil {
			result["error"] = nil
		} else if source := object(value); len(source) > 0 {
			public := map[string]any{"message": textValue(source["message"])}
			if code := stringValue(source["code"]); code != "" {
				public["code"] = code
			}
			result["error"] = public
		}
	}
	return result
}

func putNullableString(target, source map[string]any, field string) {
	value, exists := source[field]
	if !exists {
		return
	}
	if value == nil {
		target[field] = nil
		return
	}
	if text, ok := value.(string); ok {
		target[field] = text
	}
}

func ensureCallID(data map[string]any, sourceID int64) {
	if stringValue(data["callId"]) == "" {
		data["callId"] = fmt.Sprintf("call_%d", sourceID)
	}
}

// ApprovalRequestData creates the new durable approval payload while keeping
// the provider request as a raw diagnostic sidecar.
func ApprovalRequestData(approvalID, method string, params json.RawMessage) map[string]any {
	data := map[string]any{"approvalId": approvalID, "method": method, "params": json.RawMessage(params)}
	result := approvalRequest(data)
	result["raw"] = map[string]any{"method": method, "params": object(params)}
	return result
}

func approvalRequest(data map[string]any) map[string]any {
	if stringValue(data["kind"]) != "" || stringValue(data["title"]) != "" {
		result := make(map[string]any)
		for _, field := range []string{"approvalId", "kind", "title", "detail", "question"} {
			putNullableString(result, data, field)
		}
		result["options"] = publicApprovalOptions(data["options"])
		return result
	}
	method := stringValue(data["method"])
	params := object(data["params"])
	result := map[string]any{"approvalId": stringValue(data["approvalId"]), "kind": "tool", "title": "Approval requested", "detail": "", "question": "", "options": []any{}}
	if value := firstString(command(params["command"]), command(object(params["rawInput"])["command"])); value != "" {
		result["title"], result["detail"] = "Run command", truncate(value, 160)
	}
	if changes, ok := params["changes"].([]any); ok {
		paths := make([]string, 0, len(changes))
		for _, change := range changes {
			if path := stringValue(object(change)["path"]); path != "" {
				paths = append(paths, path)
			}
		}
		if len(paths) > 0 {
			result["title"], result["detail"] = "Apply file changes", truncate(strings.Join(paths, ", "), 160)
		}
	}
	if tool := object(params["toolCall"]); len(tool) > 0 {
		result["kind"] = "question"
		result["title"] = firstString(tool["title"], humanize(stringValue(tool["kind"])), "Permission requested")
		result["question"] = contentText(tool["content"])
	}
	if strings.Contains(method, "permissions") {
		result["title"], result["detail"] = "Grant permissions", stringValue(params["reason"])
	}
	result["options"] = publicApprovalOptions(params["options"])
	return result
}

func publicApprovalOptions(value any) []any {
	values, _ := value.([]any)
	options := make([]any, 0, len(values))
	for _, value := range values {
		option := object(value)
		id := stringValue(option["optionId"])
		if id == "" {
			continue
		}
		options = append(options, map[string]any{"optionId": id, "label": firstString(option["label"], option["name"]), "kind": stringValue(option["kind"])})
	}
	return options
}

func codexTool(itemType string, item map[string]any) (kind, name, summary, output, errorMessage string) {
	switch itemType {
	case "commandExecution":
		kind, name = "command", "Command"
		summary = firstString(command(item["command"]), item["cmd"])
		output = firstText(item["aggregatedOutput"], item["output"])
		if exitCode, ok := number(item["exitCode"]); ok && exitCode != 0 {
			errorMessage = fmt.Sprintf("Exit code %d", exitCode)
		}
	case "fileChange":
		kind, name = "file_change", "File change"
		if changes, ok := item["changes"].([]any); ok {
			paths := make([]string, 0, len(changes))
			for _, change := range changes {
				if path := stringValue(object(change)["path"]); path != "" {
					paths = append(paths, path)
				}
			}
			summary = strings.Join(paths, ", ")
		}
	case "mcpToolCall":
		kind, name = "mcp", "MCP"
		summary = strings.Join(nonEmpty(stringValue(item["server"]), stringValue(item["tool"])), " / ")
		output = safePreview(item["result"])
		errorMessage = firstString(object(item["error"])["message"], item["error"])
	case "webSearch":
		kind, name, summary = "web_search", "Web search", stringValue(item["query"])
	default:
		kind, name = toolKind(itemType), firstString(humanize(itemType), "Tool")
		summary = firstString(item["title"], item["name"], command(item["command"]), item["path"])
		output = firstText(item["output"], item["aggregatedOutput"])
	}
	return
}

func operationFor(method string) string {
	value := strings.ToLower(method)
	if strings.Contains(value, "started") || strings.HasSuffix(value, "_start") || strings.HasSuffix(value, "/start") {
		return "start"
	}
	if strings.Contains(value, "completed") || strings.Contains(value, "_end") || strings.HasSuffix(value, "/end") {
		return "finish"
	}
	return "update"
}

func statusFor(method, status string) string {
	value := normalizeStatus(status)
	if value != "running" || operationFor(method) != "finish" {
		return value
	}
	return "completed"
}

func normalizeStatus(status string) string {
	switch strings.ToLower(status) {
	case "completed", "complete", "done", "success", "succeeded":
		return "completed"
	case "failed", "failure", "error", "declined", "denied", "cancelled", "canceled":
		return "failed"
	default:
		return "running"
	}
}

func toolKind(value string) string {
	normalized := strings.ToLower(value)
	switch {
	case strings.Contains(normalized, "command"), strings.Contains(normalized, "exec"), strings.Contains(normalized, "bash"), strings.Contains(normalized, "shell"):
		return "command"
	case strings.Contains(normalized, "filechange"), strings.Contains(normalized, "edit"), strings.Contains(normalized, "patch"):
		return "file_change"
	case strings.Contains(normalized, "mcp"):
		return "mcp"
	case strings.Contains(normalized, "web"), strings.Contains(normalized, "search"):
		return "web_search"
	case strings.Contains(normalized, "read"):
		return "read"
	case strings.Contains(normalized, "write"):
		return "write"
	default:
		return "other"
	}
}

func putOutput(target map[string]any, mode, text string) {
	if text == "" {
		return
	}
	value, truncated := truncateFlag(text, maxOutput)
	output := map[string]any{"mode": mode, "text": value}
	if truncated {
		output["truncated"] = true
	}
	target["output"] = output
}

func putError(target map[string]any, message string) {
	if message != "" {
		target["error"] = map[string]any{"message": message}
	}
}

func putString(target map[string]any, key, value string) {
	if value != "" {
		target[key] = value
	}
}

func object(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	case json.RawMessage:
		var result map[string]any
		_ = json.Unmarshal(typed, &result)
		return result
	case []byte:
		var result map[string]any
		_ = json.Unmarshal(typed, &result)
		return result
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return map[string]any{}
		}
		var result map[string]any
		_ = json.Unmarshal(encoded, &result)
		return result
	}
}

func selectFields(source map[string]any, fields ...string) map[string]any {
	result := make(map[string]any)
	for _, field := range fields {
		if value, ok := source[field]; ok {
			result[field] = value
		}
	}
	return result
}

func withoutRaw(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		if key == "raw" || key == "rawInput" {
			continue
		}
		result[key] = value
	}
	return result
}

func firstString(values ...any) string {
	for _, value := range values {
		if text := stringValue(value); text != "" {
			return text
		}
	}
	return ""
}

func stringValue(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func textValue(value any) string {
	text, _ := value.(string)
	return text
}

func firstText(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && text != "" {
			return text
		}
	}
	return ""
}

func safePreview(value any) string {
	if text := textValue(value); text != "" {
		return text
	}
	if value == nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil || string(encoded) == "null" || string(encoded) == "{}" || string(encoded) == "[]" {
		return ""
	}
	return string(encoded)
}

func command(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, value := range typed {
			if text, ok := value.(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

func contentText(value any) string {
	values, ok := value.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		block := object(value)
		if text := textValue(block["text"]); text != "" {
			parts = append(parts, text)
		} else if text := textValue(object(block["content"])["text"]); text != "" {
			parts = append(parts, text)
		} else if stringValue(block["type"]) == "diff" && stringValue(block["path"]) != "" {
			parts = append(parts, "Edit "+stringValue(block["path"]))
		}
	}
	return strings.Join(parts, "\n")
}

func humanize(value string) string {
	if value == "" {
		return ""
	}
	var builder strings.Builder
	for index, current := range value {
		if current == '_' || current == '-' {
			builder.WriteByte(' ')
			continue
		}
		if index > 0 && unicode.IsUpper(current) {
			builder.WriteByte(' ')
		}
		builder.WriteRune(current)
	}
	text := strings.TrimSpace(builder.String())
	if text == "" {
		return ""
	}
	runes := []rune(text)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

func truncate(value string, maximum int) string {
	result, _ := truncateFlag(value, maximum)
	return result
}

func truncateFlag(value string, maximum int) (string, bool) {
	runes := []rune(value)
	if len(runes) <= maximum {
		return value, false
	}
	return string(runes[:maximum-1]) + "…", true
}

func number(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	default:
		return 0, false
	}
}

func nonEmpty(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}
