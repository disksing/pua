package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/disksing/pua/agenthub/internal/config"
	"github.com/disksing/pua/agenthub/internal/session"
)

type writeCloser struct{ io.Writer }

func (writeCloser) Close() error { return nil }

func TestCodexTranslatesStreamingTurnAndApproval(t *testing.T) {
	var events []Event
	var approvalID string
	options := Options{
		Agent: config.Agent{Options: map[string]string{"approval": "on-request"}},
		Hooks: Hooks{
			Event:    func(event Event) { events = append(events, event) },
			Approval: func(id, _ string, _ json.RawMessage) { approvalID = id },
		},
	}
	value := newCodex("unused", options)
	var output bytes.Buffer
	value.rpc.stdin = writeCloser{&output}
	value.notification("turn/started", json.RawMessage(`{"turn":{"id":"turn-native"}}`))
	value.notification("item/agentMessage/delta", json.RawMessage(`{"delta":"hello"}`))
	value.inbound(json.RawMessage(`42`), "item/commandExecution/requestApproval", json.RawMessage(`{"command":"go test ./..."}`))
	if approvalID != "42" {
		t.Fatalf("approval id = %q", approvalID)
	}
	if err := value.Approve("42", ApprovalResolution{Decision: "accept"}); err != nil {
		t.Fatal(err)
	}
	value.notification("turn/completed", json.RawMessage(`{"turn":{"id":"turn-native"}}`))
	if len(events) != 3 || events[1].Type != "message.assistant.delta" || !events[2].TurnDone {
		t.Fatalf("unexpected events: %+v", events)
	}
	if !strings.Contains(output.String(), `"decision":"accept"`) {
		t.Fatalf("unexpected approval response: %s", output.String())
	}
}

func TestCodexClassifiesItemLifecycleNotifications(t *testing.T) {
	var events []Event
	value := newCodex("unused", Options{Hooks: Hooks{
		Event: func(event Event) { events = append(events, event) },
	}})

	tests := []struct {
		method string
		params string
		want   string
	}{
		{method: "item/started", params: `{"item":{"id":"user-1","type":"userMessage"}}`, want: "provider.event"},
		{method: "item/completed", params: `{"item":{"id":"agent-1","type":"agentMessage"}}`, want: "message.assistant.completed"},
		{method: "item/updated", params: `{"item":{"id":"reasoning-1","type":"reasoning"}}`, want: "provider.event"},
		{method: "item/started", params: `{"item":{"id":"search-1","type":"webSearch"}}`, want: "tool.call"},
		{method: "item/completed", params: `{"item":{"id":"command-1","type":"commandExecution"}}`, want: "tool.call"},
		{method: "item/commandExecution/outputDelta", params: `{"itemId":"command-1","delta":"ok"}`, want: "tool.call"},
		{method: "command/exec/outputDelta", params: `{"callId":"command-2","delta":"ok"}`, want: "tool.call"},
	}

	for _, test := range tests {
		value.notification(test.method, json.RawMessage(test.params))
		if got := events[len(events)-1].Type; got != test.want {
			t.Fatalf("%s event type = %q, want %q", test.method, got, test.want)
		}
	}
}

func TestCodexRetryableErrorKeepsTurnOpenAndNormalizesData(t *testing.T) {
	var events []Event
	value := newCodex("unused", Options{Hooks: Hooks{
		Event: func(event Event) { events = append(events, event) },
	}})
	value.notification("turn/started", json.RawMessage(`{"turn":{"id":"turn-native"}}`))
	value.notification("error", json.RawMessage(`{
		"error": {
			"message": "Reconnecting... 2/5",
			"additionalDetails": "stream disconnected before completion: tls handshake eof"
		},
		"willRetry": true
	}`))

	if value.turn != "turn-native" {
		t.Fatalf("retryable error cleared native turn: %q", value.turn)
	}
	retry := events[1]
	if retry.Type != "provider.error" || retry.TurnDone || retry.TurnFailed {
		t.Fatalf("retryable error became terminal: %+v", retry)
	}
	data, ok := retry.Data.(map[string]any)
	if !ok {
		t.Fatalf("retry data type = %T", retry.Data)
	}
	if data["message"] != "Reconnecting... 2/5" ||
		data["details"] != "stream disconnected before completion: tls handshake eof" ||
		data["willRetry"] != true {
		t.Fatalf("retry data was not normalized: %+v", data)
	}

	value.notification("item/agentMessage/delta", json.RawMessage(`{"delta":"recovered"}`))
	value.notification("turn/completed", json.RawMessage(`{"turn":{"id":"turn-native"}}`))
	if events[2].Type != "message.assistant.delta" || !events[3].TurnDone || events[3].TurnFailed {
		t.Fatalf("recovered turn did not complete normally: %+v", events)
	}
}

func TestCodexNonRetryableErrorFailsTurnWithNormalizedMessage(t *testing.T) {
	var events []Event
	value := newCodex("unused", Options{Hooks: Hooks{
		Event: func(event Event) { events = append(events, event) },
	}})
	value.notification("turn/started", json.RawMessage(`{"turn":{"id":"turn-native"}}`))
	value.notification("error", json.RawMessage(`{
		"error": {
			"message": "stream disconnected",
			"additionalDetails": "retry budget exhausted"
		},
		"willRetry": false
	}`))

	if value.turn != "" {
		t.Fatalf("terminal error kept native turn open: %q", value.turn)
	}
	failure := events[1]
	if failure.Type != "provider.error" || !failure.TurnDone || !failure.TurnFailed {
		t.Fatalf("non-retryable error was not terminal: %+v", failure)
	}
	data := failure.Data.(map[string]any)
	if data["message"] != "stream disconnected" || data["details"] != "retry budget exhausted" ||
		data["willRetry"] != false {
		t.Fatalf("terminal error data was not normalized: %+v", data)
	}
}

func TestCodexParsesModelList(t *testing.T) {
	raw := json.RawMessage(`{"data":[{"id":"gpt-5.6-sol","isDefault":true,"supportedReasoningEfforts":[{"reasoningEffort":"low"},{"reasoningEffort":"high"}]},{"id":"gpt-5.5","supportedReasoningEfforts":[{"reasoningEffort":"medium"}]}]}`)
	models := parseCodexModels(raw)
	if len(models) != 2 || !models[0].isDefault || models[0].id != "gpt-5.6-sol" {
		t.Fatalf("unexpected models: %+v", models)
	}
	if len(models[0].efforts) != 2 || models[0].efforts[1] != "high" {
		t.Fatalf("unexpected efforts: %+v", models[0].efforts)
	}
	if got := parseCodexModels(json.RawMessage(`null`)); len(got) != 0 {
		t.Fatalf("expected no models, got %+v", got)
	}
}

func TestCodexChecksReasoningEffort(t *testing.T) {
	models := []codexModel{
		{id: "gpt-5.6-sol", isDefault: true, efforts: []string{"low", "medium", "high"}},
		{id: "gpt-5.5", efforts: []string{"medium"}},
	}
	if err := checkReasoningEffort("high", "", models); err != nil {
		t.Fatalf("default model should accept high: %v", err)
	}
	if err := checkReasoningEffort("medium", "gpt-5.5", models); err != nil {
		t.Fatalf("requested model should accept medium: %v", err)
	}
	err := checkReasoningEffort("bogus", "gpt-5.6-sol", models)
	if err == nil || !strings.Contains(err.Error(), "low, medium, high") {
		t.Fatalf("expected supported values in error, got %v", err)
	}
	if err := checkReasoningEffort("bogus", "unknown-model", models); err != nil {
		t.Fatalf("unknown model should pass through: %v", err)
	}
	if err := checkReasoningEffort("bogus", "", nil); err != nil {
		t.Fatalf("empty catalog should pass through: %v", err)
	}
}

func TestCodexChecksReasoningEffortEcho(t *testing.T) {
	if err := checkReasoningEffortEcho(json.RawMessage(`{"reasoningEffort":"high"}`), "high"); err != nil {
		t.Fatalf("matching echo should pass: %v", err)
	}
	if err := checkReasoningEffortEcho(json.RawMessage(`{"model":"gpt-5.6-sol"}`), "high"); err != nil {
		t.Fatalf("missing echo should pass: %v", err)
	}
	err := checkReasoningEffortEcho(json.RawMessage(`{"reasoningEffort":"medium"}`), "high")
	if err == nil || !strings.Contains(err.Error(), "medium") {
		t.Fatalf("expected echo mismatch error, got %v", err)
	}
}

func TestACPTranslatesMessageAndPermission(t *testing.T) {
	var events []Event
	var approvalID string
	value := newACP("unused", Options{
		Provider: config.Provider{Type: "kimi"},
		Hooks: Hooks{
			Event:    func(event Event) { events = append(events, event) },
			Approval: func(id, _ string, _ json.RawMessage) { approvalID = id },
		},
	})
	var output bytes.Buffer
	value.rpc.stdin = writeCloser{&output}
	value.notification("session/update", json.RawMessage(`{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}}`))
	value.inbound(json.RawMessage(`"req-1"`), "session/request_permission", json.RawMessage(`{"options":[{"optionId":"once","kind":"allow_once"}]}`))
	if approvalID != "req-1" {
		t.Fatalf("approval id = %q", approvalID)
	}
	if err := value.Approve("req-1", ApprovalResolution{Decision: "accept"}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "message.assistant.delta" {
		t.Fatalf("unexpected events: %+v", events)
	}
	if !strings.Contains(output.String(), `"optionId":"once"`) {
		t.Fatalf("unexpected permission response: %s", output.String())
	}
}

func TestACPSelectsExplicitPermissionOption(t *testing.T) {
	var approvalID string
	value := newACP("unused", Options{
		Provider: config.Provider{Type: "kimi"},
		Hooks:    Hooks{Approval: func(id, _ string, _ json.RawMessage) { approvalID = id }},
	})
	var output bytes.Buffer
	value.rpc.stdin = writeCloser{&output}
	params := json.RawMessage(`{"toolCall":{"toolCallId":"t1","title":"AskUserQuestion"},"options":[{"optionId":"q0_opt_0","name":"red","kind":"allow_once"},{"optionId":"q0_opt_1","name":"blue","kind":"allow_once"},{"optionId":"q0_skip","name":"Skip","kind":"reject_once"}]}`)
	value.inbound(json.RawMessage(`"req-2"`), "session/request_permission", params)
	if approvalID != "req-2" {
		t.Fatalf("approval id = %q", approvalID)
	}
	// An explicit option selection wins over the kind-based default, which
	// would otherwise pick the first allow_once option.
	if err := value.Approve("req-2", ApprovalResolution{Decision: "accept", OptionID: "q0_opt_1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"optionId":"q0_opt_1"`) {
		t.Fatalf("unexpected permission response: %s", output.String())
	}
}

func TestACPRejectsUnknownPermissionOptionAndStaysPending(t *testing.T) {
	value := newACP("unused", Options{Provider: config.Provider{Type: "kimi"}})
	var output bytes.Buffer
	value.rpc.stdin = writeCloser{&output}
	params := json.RawMessage(`{"options":[{"optionId":"q0_opt_0","kind":"allow_once"},{"optionId":"q0_skip","kind":"reject_once"}]}`)
	value.inbound(json.RawMessage(`"req-3"`), "session/request_permission", params)
	err := value.Approve("req-3", ApprovalResolution{Decision: "accept", OptionID: "bogus"})
	if err == nil || !strings.Contains(err.Error(), `"bogus"`) {
		t.Fatalf("expected unknown option error, got %v", err)
	}
	if output.String() != "" {
		t.Fatalf("unknown option must not reach the provider: %s", output.String())
	}
	// The failed selection leaves the request pending so the user can retry.
	if err := value.Approve("req-3", ApprovalResolution{Decision: "accept", OptionID: "q0_opt_0"}); err != nil {
		t.Fatalf("retry after unknown option failed: %v", err)
	}
	if !strings.Contains(output.String(), `"optionId":"q0_opt_0"`) {
		t.Fatalf("unexpected permission response: %s", output.String())
	}
}

func TestPiTranslatesDeltaAndSettled(t *testing.T) {
	var events []Event
	value := newPi("unused", Options{Hooks: Hooks{Event: func(event Event) { events = append(events, event) }}})
	value.event("message_update", json.RawMessage(`{"assistantMessageEvent":{"type":"text_delta","delta":"ok"}}`))
	value.event("agent_settled", json.RawMessage(`{"type":"agent_settled"}`))
	if len(events) != 2 || events[0].Type != "message.assistant.delta" || !events[1].TurnDone {
		t.Fatalf("unexpected events: %+v", events)
	}
}

func TestPiNormalizedDeltasUseStoreMerge(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(session.CreateInput{Title: "Pi delta merge", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(created.ID, "turn.started", "turn_1", nil); err != nil {
		t.Fatal(err)
	}
	var appendErr error
	value := newPi("unused", Options{Hooks: Hooks{Event: func(event Event) {
		data, err := json.Marshal(event.Data)
		if err == nil {
			_, err = store.Append(created.ID, event.Type, "turn_1", data)
		}
		if err != nil && appendErr == nil {
			appendErr = err
		}
	}}})
	value.event("message_update", json.RawMessage(`{"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"Hello"},"message":{"content":[{"text":"large cumulative snapshot"}]}}`))
	value.event("message_update", json.RawMessage(`{"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":" world"},"message":{"content":[{"text":"larger cumulative snapshot"}]}}`))
	if appendErr != nil {
		t.Fatal(appendErr)
	}

	events, err := store.EventsAfter(created.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("durable events = %d, want created + turn + one merged Pi message: %+v", len(events), events)
	}
	var data map[string]any
	if err := json.Unmarshal(events[2].Data, &data); err != nil {
		t.Fatal(err)
	}
	if events[2].Type != "message.assistant.delta" || data["text"] != "Hello world" || data["method"] != "message_update" {
		t.Fatalf("merged Pi event = %+v data=%+v", events[2], data)
	}
	if bytes.Contains(events[2].Data, []byte("cumulative snapshot")) {
		t.Fatalf("merged Pi event retained provider snapshots: %s", events[2].Data)
	}
}

func TestPiNormalizesSnapshotOnlyMessageUpdates(t *testing.T) {
	var events []Event
	value := newPi("unused", Options{Hooks: Hooks{Event: func(event Event) { events = append(events, event) }}})
	value.event("message_update", json.RawMessage(`{
		"type":"message_update",
		"assistantMessageEvent":{"type":"thinking_start","contentIndex":0,"partial":{"content":[{"type":"thinking","thinking":"initial"}]}},
		"message":{"content":[{"type":"thinking","thinking":"large repeated snapshot"}]}
	}`))
	value.event("message_update", json.RawMessage(`{
		"type":"message_update",
		"assistantMessageEvent":{"type":"thinking_end","contentIndex":0,"content":"reasoning result","partial":{"content":[{"type":"thinking","thinking":"reasoning result"}]}},
		"message":{"content":[{"type":"thinking","thinking":"reasoning result"}]}
	}`))
	value.event("message_update", json.RawMessage(`{
		"type":"message_update",
		"assistantMessageEvent":{"type":"text_start","contentIndex":1,"partial":{"content":[{"type":"thinking","thinking":"reasoning result"},{"type":"text","text":"initial"}]}},
		"message":{"content":[{"type":"thinking","thinking":"reasoning result"},{"type":"text","text":"large repeated snapshot"}]}
	}`))
	value.event("message_update", json.RawMessage(`{
		"type":"message_update",
		"assistantMessageEvent":{"type":"text_end","contentIndex":1,"content":"answer result","partial":{"content":[{"type":"thinking","thinking":"reasoning result"},{"type":"text","text":"answer result"}]}},
		"message":{"content":[{"type":"thinking","thinking":"reasoning result"},{"type":"text","text":"answer result"}]}
	}`))

	if len(events) != 3 || events[2].Type != "message.assistant.completed" {
		t.Fatalf("snapshot-only updates must emit reasoning, answer and boundary: %+v", events)
	}
	want := []struct {
		typeName string
		text     string
	}{
		{typeName: "message.reasoning.delta", text: "reasoning result"},
		{typeName: "message.assistant.delta", text: "answer result"},
	}
	for i, expected := range want {
		if events[i].Type != expected.typeName {
			t.Fatalf("event %d type = %q, want %q", i, events[i].Type, expected.typeName)
		}
		data, ok := events[i].Data.(map[string]any)
		if !ok || data["text"] != expected.text || data["method"] != "message_update" {
			t.Fatalf("event %d data = %#v", i, events[i].Data)
		}
		encoded, err := json.Marshal(events[i].Data)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encoded, []byte("repeated snapshot")) {
			t.Fatalf("event %d retained the cumulative Pi snapshot: %s", i, encoded)
		}
	}
}

func TestPiDoesNotDuplicateMessageEndAfterDeltas(t *testing.T) {
	var events []Event
	value := newPi("unused", Options{Hooks: Hooks{Event: func(event Event) { events = append(events, event) }}})
	value.event("message_update", json.RawMessage(`{"assistantMessageEvent":{"type":"text_start","contentIndex":2,"partial":{"content":[]}}}`))
	value.event("message_update", json.RawMessage(`{"assistantMessageEvent":{"type":"text_delta","contentIndex":2,"delta":"Hello"},"message":{"content":[{"text":"large snapshot"}]}}`))
	value.event("message_update", json.RawMessage(`{"assistantMessageEvent":{"type":"text_delta","contentIndex":2,"delta":" world"},"message":{"content":[{"text":"larger snapshot"}]}}`))
	value.event("message_update", json.RawMessage(`{"assistantMessageEvent":{"type":"text_end","contentIndex":2,"content":"Hello world","partial":{"content":[]}},"message":{"content":[{"text":"Hello world"}]}}`))

	if len(events) != 3 || events[2].Type != "message.assistant.completed" {
		t.Fatalf("stream must emit two fragments and a boundary: %+v", events)
	}
	for i, expected := range []string{"Hello", " world"} {
		data, ok := events[i].Data.(map[string]any)
		if events[i].Type != "message.assistant.delta" || !ok || data["text"] != expected {
			t.Fatalf("event %d = %+v, want assistant fragment %q", i, events[i], expected)
		}
	}
}

func TestPiDropsRedundantToolCallAssemblyDeltas(t *testing.T) {
	var events []Event
	value := newPi("unused", Options{Hooks: Hooks{Event: func(event Event) { events = append(events, event) }}})
	value.event("message_update", json.RawMessage(`{
		"type":"message_update",
		"assistantMessageEvent":{"type":"toolcall_delta","contentIndex":3,"delta":"{\"path\":","partial":{"content":[{"type":"toolCall","id":"call_1","name":"read","partialJson":"{\"path\":","arguments":{}}]}},
		"message":{"content":[{"type":"toolCall","id":"call_1","name":"read","partialJson":"{\"path\":","arguments":{}}]}
	}`))
	if len(events) != 0 {
		t.Fatalf("redundant tool-call assembly delta must not reach persistence: %+v", events)
	}
	value.event("message_update", json.RawMessage(`{
		"assistantMessageEvent":{"type":"toolcall_start","contentIndex":3,"partial":{"content":[{"type":"toolCall","id":"call_1","name":"read","arguments":{}}]}},
		"message":{"content":[{"type":"toolCall","id":"call_1","name":"read","arguments":{}}]}
	}`))
	value.event("message_update", json.RawMessage(`{
		"assistantMessageEvent":{"type":"toolcall_end","contentIndex":3,"toolCall":{"type":"toolCall","id":"call_1","name":"read","arguments":{"path":"README.md"}},"partial":{"content":[{"type":"toolCall","id":"call_1","name":"read","arguments":{"path":"README.md"}}]}},
		"message":{"content":[{"type":"toolCall","id":"call_1","name":"read","arguments":{"path":"README.md"}}]}
	}`))
	if len(events) != 2 {
		t.Fatalf("tool-call boundaries = %+v, want two compact metadata events", events)
	}
	for i, event := range events {
		data, ok := event.Data.(map[string]any)
		_, hasRaw := data["raw"]
		if event.Type != "provider.metadata" || !ok || hasRaw {
			t.Fatalf("tool-call boundary %d retained a cumulative snapshot: type=%s data=%#v", i, event.Type, event.Data)
		}
	}

	value.event("tool_execution_start", json.RawMessage(`{"type":"tool_execution_start","toolCallId":"call_1","toolName":"read","args":{"path":"README.md"}}`))
	value.event("tool_execution_end", json.RawMessage(`{"type":"tool_execution_end","toolCallId":"call_1","toolName":"read","result":{"content":[{"type":"text","text":"contents"}]}}`))
	if len(events) != 4 || events[2].Type != "tool.call" || events[3].Type != "tool.call" {
		t.Fatalf("tool execution visibility changed after dropping assembly deltas: %+v", events)
	}
}

func TestPiKeepsUnknownMessageUpdatesRaw(t *testing.T) {
	var events []Event
	value := newPi("unused", Options{Hooks: Hooks{Event: func(event Event) { events = append(events, event) }}})
	value.event("message_update", json.RawMessage(`{"assistantMessageEvent":{"type":"future_delta","value":42},"message":{"future":true}}`))
	if len(events) != 1 || events[0].Type != "provider.event" {
		t.Fatalf("unknown Pi update lost its raw fallback: %+v", events)
	}
}

func TestCodexLaunchEnvironmentReachesProcessAndStartOrResumeConfig(t *testing.T) {
	t.Setenv("AGENTHUB_PROCESS_ENV", "daemon-value")
	t.Setenv("AGENTHUB_EXPECTED_LAUNCH_ENV", "session-value")
	for _, resumeID := range []string{"", "thread-existing"} {
		t.Run(map[bool]string{true: "resume", false: "start"}[resumeID != ""], func(t *testing.T) {
			var nativeID string
			value := newCodex(helperCLI(t, "codex-session-environment"), Options{
				Cwd:         t.TempDir(),
				Environment: map[string]string{"AGENTHUB_PROCESS_ENV": "session-value"},
				Hooks: Hooks{
					NativeID: func(id string) { nativeID = id },
				},
			})
			if err := value.Start(resumeID); err != nil {
				t.Fatalf("Start(%q): %v", resumeID, err)
			}
			if nativeID != "session-value" {
				t.Fatalf("native id = %q, launch environment did not reach fake Codex", nativeID)
			}
			if err := value.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestACPLaunchEnvironmentOverridesDaemonEnvironment(t *testing.T) {
	t.Setenv("AGENTHUB_PROCESS_ENV", "daemon-value")
	var nativeID string
	options := acpTestOptions(t, &nativeID)
	options.Environment = map[string]string{"AGENTHUB_PROCESS_ENV": "acp-session-value"}
	value := newACP(helperCLI(t, "acp-session-environment"), options)
	if err := value.Start(""); err != nil {
		t.Fatal(err)
	}
	if nativeID != "acp-session-value" {
		t.Fatalf("native id = %q, launch environment did not reach fake ACP", nativeID)
	}
	if err := value.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPiLaunchEnvironmentOverridesDaemonEnvironment(t *testing.T) {
	t.Setenv("AGENTHUB_PROCESS_ENV", "daemon-value")
	var nativeID string
	value := newPi(helperCLI(t, "pi-session-environment"), Options{
		Cwd:         t.TempDir(),
		Environment: map[string]string{"AGENTHUB_PROCESS_ENV": "pi-session-value"},
		Hooks: Hooks{
			NativeID: func(id string) { nativeID = id },
		},
	})
	if err := value.Start(""); err != nil {
		t.Fatal(err)
	}
	if nativeID != "pi-session-value" {
		t.Fatalf("native id = %q, launch environment did not reach fake Pi", nativeID)
	}
	if err := value.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONRPCDrainsFinalOutputBeforeProcessEnd(t *testing.T) {
	notificationStarted := make(chan struct{})
	releaseNotification := make(chan struct{})
	processEnded := make(chan struct{})
	var notificationOnce sync.Once
	var processEndOnce sync.Once
	value := newJSONRPC(
		helperCLI(t, "jsonrpc-output-then-exit"),
		nil,
		t.TempDir(),
		nil,
		Hooks{ProcessEnd: func(error) { processEndOnce.Do(func() { close(processEnded) }) }},
	)
	value.notify = func(string, json.RawMessage) {
		notificationOnce.Do(func() { close(notificationStarted) })
		<-releaseNotification
	}
	if err := value.start(); err != nil {
		t.Fatal(err)
	}
	waitForSignal(t, notificationStarted, "final JSON-RPC notification")
	select {
	case <-processEnded:
		t.Fatal("ProcessEnd ran before the final JSON-RPC notification completed")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseNotification)
	waitForSignal(t, processEnded, "JSON-RPC ProcessEnd")
	if err := value.close(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONRPCAcceptsResponseLargerThanScannerLimit(t *testing.T) {
	value := newJSONRPC(helperCLI(t, "jsonrpc-large-response"), nil, t.TempDir(), nil, Hooks{})
	if err := value.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = value.close() })

	raw, err := value.requestWithTimeout("large/result", map[string]any{}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Payload) != 17*1024*1024 {
		t.Fatalf("payload bytes = %d, want %d", len(result.Payload), 17*1024*1024)
	}
}

type unexpectedEOFReader struct{}

func (unexpectedEOFReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestJSONRPCReadFailureImmediatelyFailsPendingRequest(t *testing.T) {
	events := make(chan Event, 1)
	value := newJSONRPC("unused", nil, "", nil, Hooks{Event: func(event Event) { events <- event }})
	var input bytes.Buffer
	value.stdin = writeCloser{&input}

	requestDone := make(chan error, 1)
	go func() {
		_, err := value.requestLongRunning("thread/resume", map[string]any{})
		requestDone <- err
	}()
	waitForWaitingRequest(t, func() int {
		value.mu.Lock()
		defer value.mu.Unlock()
		return len(value.waiting)
	})

	value.consumeStdout(unexpectedEOFReader{})
	select {
	case err := <-requestDone:
		if err == nil || !strings.Contains(err.Error(), "read provider stdout") ||
			!strings.Contains(err.Error(), "unexpected EOF") {
			t.Fatalf("request error = %v, want stdout read failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request remained blocked after stdout read failure")
	}
	select {
	case event := <-events:
		if event.Type != "provider.error" || !strings.Contains(fmt.Sprint(event.Data), "unexpected EOF") {
			t.Fatalf("transport event = %#v, want provider.error with stdout failure", event)
		}
	default:
		t.Fatal("stdout read failure did not emit provider.error")
	}
	if err := value.close(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONRPCCloseReleasesLongRunningRequest(t *testing.T) {
	value := newJSONRPC(helperCLI(t, "sleep"), nil, t.TempDir(), nil, Hooks{})
	if err := value.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = value.close() })

	requestDone := make(chan error, 1)
	go func() {
		_, err := value.requestLongRunning("session/prompt", map[string]any{})
		requestDone <- err
	}()
	waitForWaitingRequest(t, func() int {
		value.mu.Lock()
		defer value.mu.Unlock()
		return len(value.waiting)
	})

	closeDone := make(chan error, 1)
	go func() { closeDone <- value.close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close deadlocked behind a long-running request")
	}
	select {
	case err := <-requestDone:
		if err == nil || !strings.Contains(err.Error(), "provider exited") {
			t.Fatalf("request error = %v, want provider exit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long-running request remained blocked after Close")
	}
}

func TestPiDrainsFinalOutputBeforeProcessEnd(t *testing.T) {
	eventStarted := make(chan struct{})
	releaseEvent := make(chan struct{})
	processEnded := make(chan struct{})
	var eventOnce sync.Once
	var processEndOnce sync.Once
	value := newPi(helperCLI(t, "pi-output-then-exit"), Options{
		Cwd: t.TempDir(),
		Hooks: Hooks{
			Event: func(Event) {
				eventOnce.Do(func() { close(eventStarted) })
				<-releaseEvent
			},
			ProcessEnd: func(error) { processEndOnce.Do(func() { close(processEnded) }) },
		},
	})
	if err := value.Start(""); err != nil {
		t.Fatal(err)
	}
	waitForSignal(t, eventStarted, "final Pi event")
	select {
	case <-processEnded:
		t.Fatal("ProcessEnd ran before the final Pi event completed")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseEvent)
	waitForSignal(t, processEnded, "Pi ProcessEnd")
	if err := value.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestACPPromptCompletionPrecedesImmediateProcessEnd(t *testing.T) {
	var nativeID string
	var mu sync.Mutex
	var order []string
	processEnded := make(chan struct{})
	options := acpTestOptions(t, &nativeID)
	options.Hooks.Event = func(event Event) {
		if !event.TurnDone {
			return
		}
		mu.Lock()
		order = append(order, "turn")
		mu.Unlock()
	}
	options.Hooks.ProcessEnd = func(error) {
		mu.Lock()
		order = append(order, "process")
		mu.Unlock()
		close(processEnded)
	}
	value := newACP(helperCLI(t, "acp-prompt-exit"), options)
	if err := value.Start(""); err != nil {
		t.Fatal(err)
	}
	if err := value.Prompt("finish", false); err != nil {
		t.Fatal(err)
	}
	waitForSignal(t, processEnded, "ACP ProcessEnd")
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ",") != "turn,process" {
		t.Fatalf("terminal order = %v, want turn before process", order)
	}
}

func TestACPPromptCanOutliveControlRequestTimeout(t *testing.T) {
	previous := controlRequestTimeout
	controlRequestTimeout = 40 * time.Millisecond
	t.Cleanup(func() { controlRequestTimeout = previous })
	t.Setenv("AGENTHUB_TEST_PROMPT_DELAY", "150ms")

	terminal := make(chan Event, 1)
	var nativeID string
	options := acpTestOptions(t, &nativeID)
	options.Hooks.Event = func(event Event) {
		if event.TurnDone {
			terminal <- event
		}
	}
	value := newACP(helperCLI(t, "acp-delayed-prompt"), options)
	if err := value.Start(""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = value.Close() })
	if err := value.Prompt("take your time", false); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-terminal:
		if event.TurnFailed || event.Type != "provider.turn.completed" {
			t.Fatalf("long ACP prompt was treated as failed: %+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delayed ACP prompt")
	}
}

func TestPiPromptCanOutliveControlRequestTimeout(t *testing.T) {
	previous := controlRequestTimeout
	controlRequestTimeout = 40 * time.Millisecond
	t.Cleanup(func() { controlRequestTimeout = previous })
	t.Setenv("AGENTHUB_TEST_PROMPT_DELAY", "150ms")

	terminal := make(chan Event, 1)
	value := newPi(helperCLI(t, "pi-delayed-prompt"), Options{
		Cwd: t.TempDir(),
		Hooks: Hooks{Event: func(event Event) {
			if event.TurnDone {
				terminal <- event
			}
		}},
	})
	if err := value.Start(""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = value.Close() })
	if err := value.Prompt("take your time", false); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-terminal:
		if event.TurnFailed || event.Type != "provider.turn.completed" {
			t.Fatalf("long Pi prompt was treated as failed: %+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delayed Pi prompt")
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitForWaitingRequest(t *testing.T, count func() int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for provider request registration")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCloseEliminatesDescendantsAfterGroupLeaderExited(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "writes")
	cmd := exec.Command("sh", "-c", `(trap '' TERM; while :; do printf x >> "$1"; sleep 0.02; done) & exit 0`, "sh", marker)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	<-done
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	if !processGroupExists(pgid) {
		t.Fatal("test descendant did not survive its group leader")
	}
	if err := terminateChildProcess(cmd, pgid, nil, done); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(marker)
	time.Sleep(100 * time.Millisecond)
	after, _ := os.Stat(marker)
	if before != nil && after != nil && before.Size() != after.Size() {
		t.Fatal("provider descendant wrote after Close returned")
	}
}
