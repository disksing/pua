package semantic

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/agenthub/internal/session"
)

func sourceEvent(t *testing.T, id int64, eventType string, data any) session.Event {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return session.Event{ID: id, Time: time.Date(2026, 8, 22, 1, 2, 3, 0, time.UTC), Type: eventType, SessionID: "ses_test", TurnID: "turn_test", Data: encoded}
}

func eventData(t *testing.T, frame Frame) map[string]any {
	t.Helper()
	if len(frame.Events) != 1 {
		t.Fatalf("semantic events = %d, want 1", len(frame.Events))
	}
	value, ok := frame.Events[0].Data.(map[string]any)
	if !ok {
		t.Fatalf("semantic data = %#v", frame.Events[0].Data)
	}
	return value
}

func TestFrameForPreservesCursorAndFiltersProviderNoise(t *testing.T) {
	frame := FrameFor(sourceEvent(t, 42, "provider.event", map[string]any{"method": "secret", "raw": map[string]any{"token": "do-not-return"}}), false)
	if frame.Schema != EventsSchema || frame.Cursor != 42 || frame.Mode != "replace" || frame.Source.EventID != 42 || frame.Source.Type != "provider.event" {
		t.Fatalf("frame = %+v", frame)
	}
	if len(frame.Events) != 0 {
		t.Fatalf("noise frame events = %#v", frame.Events)
	}
	legacyEnvelope := FrameFor(sourceEvent(t, 43, "tool.event", map[string]any{
		"method": "item/completed", "raw": map[string]any{"item": map[string]any{"id": "message_1", "type": "agentMessage"}},
	}), false)
	if len(legacyEnvelope.Events) != 0 {
		t.Fatalf("legacy message envelope events = %#v", legacyEnvelope.Events)
	}
}

func TestLegacyToolEventsNormalizeAcrossProviders(t *testing.T) {
	tests := []struct {
		name   string
		method string
		raw    any
		want   map[string]any
	}{
		{
			name: "codex command", method: "item/started",
			raw:  map[string]any{"item": map[string]any{"id": "call_codex", "type": "commandExecution", "command": []string{"go", "test", "./..."}, "status": "inProgress"}},
			want: map[string]any{"callId": "call_codex", "operation": "start", "toolKind": "command", "name": "Command", "summary": "go test ./...", "status": "running"},
		},
		{
			name: "codex output", method: "item/commandExecution/outputDelta",
			raw:  map[string]any{"itemId": "call_codex", "delta": "ok\n"},
			want: map[string]any{"callId": "call_codex", "operation": "update", "output": map[string]any{"mode": "append", "text": "ok\n"}},
		},
		{
			name: "acp tool", method: "session/update",
			raw:  map[string]any{"update": map[string]any{"sessionUpdate": "tool_call", "toolCallId": "call_acp", "kind": "read", "title": "Read task.md", "status": "in_progress"}},
			want: map[string]any{"callId": "call_acp", "operation": "start", "toolKind": "read", "name": "Read", "summary": "Read task.md", "status": "running"},
		},
		{
			name: "pi tool", method: "tool_execution_end",
			raw:  map[string]any{"toolCallId": "call_pi", "toolName": "bash", "result": "done"},
			want: map[string]any{"callId": "call_pi", "operation": "finish", "toolKind": "command", "name": "Bash", "status": "completed", "output": map[string]any{"mode": "replace", "text": "done"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frame := FrameFor(sourceEvent(t, 7, "tool.event", map[string]any{"method": test.method, "raw": test.raw}), false)
			if frame.Events[0].Type != "tool.call" {
				t.Fatalf("type = %q", frame.Events[0].Type)
			}
			data := eventData(t, frame)
			if data["schemaVersion"] != float64(1) && data["schemaVersion"] != 1 {
				t.Fatalf("schemaVersion = %#v", data["schemaVersion"])
			}
			for key, want := range test.want {
				if !reflect.DeepEqual(data[key], want) {
					t.Errorf("%s = %#v, want %#v (data=%#v)", key, data[key], want, data)
				}
			}
			if _, exists := data["raw"]; exists {
				t.Fatalf("public data leaked raw: %#v", data)
			}
		})
	}
}

func TestNewToolCallRetainsRawOnlyInSourceEvent(t *testing.T) {
	data := ToolCallData("item/completed", json.RawMessage(`{"item":{"id":"call_1","type":"mcpToolCall","server":"docs","tool":"lookup","status":"completed","result":{"content":"safe"}}}`))
	if _, ok := data["raw"]; !ok {
		t.Fatalf("durable data omitted raw: %#v", data)
	}
	frame := FrameFor(sourceEvent(t, 9, "tool.call", data), false)
	public := eventData(t, frame)
	if _, ok := public["raw"]; ok {
		t.Fatalf("public data leaked raw: %#v", public)
	}
	if got := public["output"].(map[string]any)["text"]; got != `{"content":"safe"}` {
		t.Fatalf("MCP output = %#v", got)
	}
}

func TestPublicToolAndApprovalSanitizeNestedObjects(t *testing.T) {
	tool := FrameFor(sourceEvent(t, 14, "tool.call", map[string]any{
		"callId": "call_1", "operation": "finish", "toolKind": "mcp", "name": "MCP", "status": "completed",
		"output": map[string]any{"mode": "replace", "text": "ok", "raw": map[string]any{"secret": "tool-secret"}},
		"error":  map[string]any{"message": "failed", "code": "E_TEST", "raw": "error-secret"},
	}), false)
	approval := FrameFor(sourceEvent(t, 15, "approval.requested", map[string]any{
		"approvalId": "apr_1", "kind": "question", "title": "Choose", "options": []any{
			map[string]any{"optionId": "yes", "label": "Yes", "kind": "allow", "raw": "approval-secret"},
		},
	}), false)
	encoded, _ := json.Marshal([]Frame{tool, approval})
	for _, secret := range []string{"tool-secret", "error-secret", "approval-secret", `"raw"`} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("public frames leaked %q: %s", secret, encoded)
		}
	}
}

func TestLegacyCanonicalFieldsAreSanitized(t *testing.T) {
	message := FrameFor(sourceEvent(t, 10, "message.user.steer", map[string]any{"text": "continue", "raw": "secret"}), false)
	if got := eventData(t, message); got["role"] != "user" || got["steer"] != true || got["text"] != "continue" {
		t.Fatalf("message = %#v", got)
	}
	errorFrame := FrameFor(sourceEvent(t, 11, "provider.error", map[string]any{"message": "closed", "willRetry": true, "raw": map[string]any{"token": "secret"}}), false)
	if got := eventData(t, errorFrame); got["retryable"] != true || got["message"] != "closed" {
		t.Fatalf("provider error = %#v", got)
	}
	unknown := FrameFor(sourceEvent(t, 12, "future.provider.payload", map[string]any{"raw": "secret"}), false)
	encoded, _ := json.Marshal(unknown)
	if strings.Contains(string(encoded), "secret") || eventData(t, unknown)["sourceType"] != "future.provider.payload" {
		t.Fatalf("unknown frame = %s", encoded)
	}
	requirement := FrameFor(sourceEvent(t, 13, session.EventEphemeralEnvironmentRequired, map[string]any{
		"required": true, "key": "secret-name", "value": "secret-value",
	}), false)
	requirementData := eventData(t, requirement)
	if !reflect.DeepEqual(requirementData, map[string]any{"required": true}) {
		t.Fatalf("ephemeral requirement frame = %#v", requirementData)
	}
}

func TestFrameProjectionIsDeterministicAndAppendModeIsExplicit(t *testing.T) {
	source := sourceEvent(t, 13, "message.assistant.delta", map[string]any{"text": "hello", "raw": "ignored"})
	first := FrameFor(source, false)
	second := FrameFor(source, false)
	if !reflect.DeepEqual(first, second) || first.Events[0].ID != "sem_13_0" {
		t.Fatalf("projection is not stable: %#v %#v", first, second)
	}
	appendFrame := FrameFor(source, true)
	if appendFrame.Mode != "append" || appendFrame.Cursor != first.Cursor || appendFrame.Events[0].ID != first.Events[0].ID {
		t.Fatalf("append frame = %#v", appendFrame)
	}
}
