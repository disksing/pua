package provider

import (
	"encoding/json"
	"testing"
)

func TestPiSettledAssistantOutcome(t *testing.T) {
	const capacity = `{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"The model is currently at capacity due to high demand","content":[],"usage":{"input":0,"output":0,"totalTokens":0}}}`
	const success = `{"type":"message_end","message":{"role":"assistant","stopReason":"stop"}}`
	const retry = `{"type":"agent_end","willRetry":true}`
	const noRetry = `{"type":"agent_end","willRetry":false}`
	const retryStart = `{"type":"auto_retry_start","attempt":1}`
	const retrySuccess = `{"type":"auto_retry_end","success":true,"attempt":1}`
	const retryFailure = `{"type":"auto_retry_end","success":false,"finalError":"retry budget exhausted"}`
	const aborted = `{"type":"message_end","message":{"role":"assistant","stopReason":"aborted","errorMessage":"Request aborted"}}`
	const text = `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"I will inspect the files"}}`
	const toolStart = `{"type":"tool_execution_start","toolCallId":"synthetic-tool","toolName":"bash","args":{"command":"true"}}`
	const toolEnd = `{"type":"tool_execution_end","toolCallId":"synthetic-tool","toolName":"bash","isError":false}`
	const capacityMessage = "The model is currently at capacity due to high demand"
	for _, test := range []struct {
		name      string
		input     []string
		wantError string
		wantText  int
		wantTools int
	}{
		{name: "capacity without output", input: []string{capacity, noRetry}, wantError: capacityMessage},
		{name: "text and tools before failure", input: []string{text, toolStart, toolEnd, capacity, noRetry}, wantError: capacityMessage, wantText: 1, wantTools: 2},
		{name: "retry succeeds", input: []string{capacity, retry, retryStart, `{"type":"agent_start"}`, text, success, retrySuccess, noRetry}, wantText: 1},
		{name: "retry exhausted", input: []string{capacity, retry, retryStart, `{"type":"agent_start"}`, capacity, noRetry, retryFailure}, wantError: capacityMessage},
		{name: "retry ends without a new assistant", input: []string{capacity, retry, retryStart, `{"type":"agent_start"}`, retryFailure}, wantError: capacityMessage},
		{name: "partial retry output still fails", input: []string{capacity, retry, retryStart, text, capacity, retryFailure}, wantError: capacityMessage, wantText: 1},
		{name: "other roles do not clear failure", input: []string{capacity, `{"type":"message_start","message":{"role":"user"}}`, `{"type":"message_end","message":{"role":"user"}}`, `{"type":"message_end","message":{"role":"toolResult","isError":false}}`}, wantError: capacityMessage},
		{name: "missing error message", input: []string{`{"type":"message_end","message":{"role":"assistant","stopReason":"error"}}`}, wantError: "Pi assistant response failed"},
		{name: "normal completion", input: []string{text, success, noRetry}, wantText: 1},
		{name: "legacy settled only"},
		{name: "aborted response", input: []string{aborted, noRetry}},
		{name: "retry aborted", input: []string{capacity, retry, retryStart, aborted, `{"type":"auto_retry_end","success":false,"finalError":"Retry cancelled"}`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var events []Event
			value := newPi("unused", Options{Hooks: Hooks{Event: func(event Event) { events = append(events, event) }}})
			for _, input := range test.input {
				emitPiTestEvent(t, value, input)
			}
			var textCount, toolCount int
			for _, event := range events {
				if event.TurnDone || event.TurnFailed {
					t.Fatalf("intermediate event closed the Turn: %+v", event)
				}
				switch event.Type {
				case "message.assistant.delta":
					textCount++
				case "tool.call":
					toolCount++
				}
			}
			if textCount != test.wantText || toolCount != test.wantTools {
				t.Fatalf("output counts: text=%d tools=%d", textCount, toolCount)
			}
			emitPiTestEvent(t, value, `{"type":"agent_settled"}`)
			terminal := events[len(events)-1]
			wantType := "provider.turn.completed"
			if test.wantError != "" {
				wantType = "provider.error"
			}
			if terminal.Type != wantType || !terminal.TurnDone || terminal.TurnFailed != (test.wantError != "") {
				t.Fatalf("terminal = %+v, want %s", terminal, wantType)
			}
			data := terminal.Data.(map[string]any)
			if data["method"] != "agent_settled" || (test.wantError != "" && data["message"] != test.wantError) {
				t.Fatalf("terminal data = %+v", data)
			}
			// Reuse the native session: the previous failure must not leak into
			// a subsequent Turn, including one with no assistant message.
			emitPiTestEvent(t, value, `{"type":"agent_start"}`)
			emitPiTestEvent(t, value, `{"type":"agent_settled"}`)
			next := events[len(events)-1]
			if next.Type != "provider.turn.completed" || !next.TurnDone || next.TurnFailed {
				t.Fatalf("failure leaked into next Turn: %+v", next)
			}
		})
	}
}

func TestPiExtensionErrorKeepsExistingTerminalContract(t *testing.T) {
	var events []Event
	value := newPi("unused", Options{Hooks: Hooks{Event: func(event Event) { events = append(events, event) }}})
	emitPiTestEvent(t, value, `{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"model failed"}}`)
	emitPiTestEvent(t, value, `{"type":"extension_error","error":"extension failed"}`)
	terminal := events[len(events)-1]
	if terminal.Type != "provider.error" || !terminal.TurnDone || !terminal.TurnFailed {
		t.Fatalf("extension failure = %+v", terminal)
	}
	emitPiTestEvent(t, value, `{"type":"agent_settled"}`)
	if events[len(events)-1].TurnFailed {
		t.Fatal("assistant failure survived extension terminal")
	}
}

func emitPiTestEvent(t *testing.T, value *piSession, input string) {
	t.Helper()
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(input), &envelope); err != nil {
		t.Fatal(err)
	}
	value.event(envelope.Type, json.RawMessage(input))
}
