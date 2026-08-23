// Command fakeprovider is a deterministic ACP subprocess used by AgentHub's
// black-box integration gate. It intentionally supports fault injection via
// the per-session launch environment; it is not a user-facing provider.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type envelope struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

type fake struct {
	encoder       *json.Encoder
	promptID      json.RawMessage
	nativeID      string
	resumed       bool
	approvalAsked bool
}

func main() {
	if len(os.Args) < 2 || os.Args[1] != "acp" {
		fmt.Fprintln(os.Stderr, "fakeprovider: expected acp mode")
		os.Exit(2)
	}
	instance := &fake{
		encoder:  json.NewEncoder(os.Stdout),
		nativeID: getenv("FAKE_NATIVE_ID", "fake-native-session"),
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		var message envelope
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			continue
		}
		if message.Method == "" {
			instance.handleResponse(message)
			continue
		}
		instance.handleRequest(message)
	}
}

func (f *fake) handleRequest(message envelope) {
	mode := os.Getenv("FAKE_MODE")
	switch message.Method {
	case "initialize":
		if mode == "startup-crash" {
			os.Exit(23)
		}
		f.respond(message.ID, map[string]any{
			"protocolVersion": 1,
			"agentCapabilities": map[string]any{
				"loadSession": true,
				"sessionCapabilities": map[string]any{
					"resume": map[string]any{},
					"close":  map[string]any{},
				},
			},
		})
	case "session/new":
		f.resumed = false
		f.respond(message.ID, map[string]any{"sessionId": f.nativeID})
	case "session/resume", "session/load":
		f.resumed = true
		var params struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(message.Params, &params)
		if params.SessionID != "" {
			f.nativeID = params.SessionID
		}
		f.respond(message.ID, map[string]any{"sessionId": f.nativeID})
	case "session/prompt":
		f.promptID = append(json.RawMessage(nil), message.ID...)
		f.emitEnvironment()
		switch mode {
		case "approval-hold":
			f.askApproval()
		case "crash":
			f.askApproval()
			waitForCrashTrigger()
			os.Exit(17)
		case "hold":
			return
		case "complete-exit":
			f.completePrompt()
			os.Exit(0)
		case "burst":
			count := getenvInt("FAKE_BURST_COUNT", 400)
			size := getenvInt("FAKE_BURST_BYTES", 65536)
			text := strings.Repeat("x", size)
			for i := 0; i < count; i++ {
				f.notify("session/update", map[string]any{
					"sessionId": f.nativeID,
					"update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       map[string]any{"type": "text", "text": text},
					},
				})
			}
			f.completePrompt()
		default:
			f.completePrompt()
		}
	case "session/cancel":
		if len(f.promptID) > 0 {
			f.respond(f.promptID, map[string]any{"stopReason": "cancelled"})
			f.promptID = nil
		}
	case "session/close":
		f.respond(message.ID, map[string]any{})
		time.Sleep(10 * time.Millisecond)
		os.Exit(0)
	default:
		f.respondError(message.ID, -32601, "unsupported fake provider method")
	}
}

func (f *fake) handleResponse(message envelope) {
	if !f.approvalAsked || string(message.ID) != `"approval-1"` {
		return
	}
	f.approvalAsked = false
	f.completePrompt()
}

func (f *fake) emitEnvironment() {
	text := fmt.Sprintf(
		"context=%s instance=%s resumed=%t native=%s",
		os.Getenv("SESSION_CONTEXT_ID"),
		os.Getenv("FAKE_INSTANCE"),
		f.resumed,
		f.nativeID,
	)
	if os.Getenv("FAKE_REPORT_EPHEMERAL") == "1" {
		value := os.Getenv("FAKE_EPHEMERAL_SECRET")
		if value == "" {
			value = "missing"
		}
		text += " ephemeral=" + value
	}
	f.notify("session/update", map[string]any{
		"sessionId": f.nativeID,
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": text},
		},
	})
}

func (f *fake) askApproval() {
	f.approvalAsked = true
	f.request("approval-1", "session/request_permission", map[string]any{
		"sessionId": f.nativeID,
		"options": []map[string]any{
			{"optionId": "once", "kind": "allow_once"},
			{"optionId": "always", "kind": "allow_always"},
			{"optionId": "deny", "kind": "reject_once"},
		},
	})
}

func (f *fake) completePrompt() {
	if len(f.promptID) == 0 {
		return
	}
	f.respond(f.promptID, map[string]any{"stopReason": "end_turn"})
	f.promptID = nil
}

func (f *fake) respond(id json.RawMessage, result any) {
	_ = f.encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (f *fake) respondError(id json.RawMessage, code int, message string) {
	_ = f.encoder.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

func (f *fake) request(id, method string, params any) {
	_ = f.encoder.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
}

func (f *fake) notify(method string, params any) {
	_ = f.encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(key))
	if err != nil || value < 1 {
		return fallback
	}
	return value
}

func waitForCrashTrigger() {
	path := os.Getenv("FAKE_CRASH_TRIGGER")
	if path == "" {
		time.Sleep(500 * time.Millisecond)
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "fakeprovider: crash trigger timed out")
	os.Exit(24)
}
