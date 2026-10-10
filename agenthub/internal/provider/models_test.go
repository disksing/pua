package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/agenthub/internal/config"
)

// The tests never spawn real provider CLIs. Short-lived commands (kimi,
// opencode) are faked with shell scripts; long-running protocol servers
// (codex, pi) are faked by re-executing the test binary itself through
// TestHelperProcess.

func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("AGENTHUB_TEST_HELPER")
	if mode == "" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	switch mode {
	case "pi-native-missing":
		fmt.Fprintln(os.Stderr, "No session found matching 'missing-native-session'")
		os.Exit(1)
	case "jsonrpc-output-then-exit":
		fmt.Println(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"final"}}}}`)
	case "jsonrpc-large-response":
		for scanner.Scan() {
			var request struct {
				ID json.RawMessage `json:"id"`
			}
			if json.Unmarshal(scanner.Bytes(), &request) != nil || len(request.ID) == 0 {
				continue
			}
			payload := strings.Repeat("x", 17*1024*1024)
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"payload":%q}}`+"\n", request.ID, payload)
			break
		}
	case "pi-output-then-exit":
		for scanner.Scan() {
			var request struct {
				ID   json.RawMessage `json:"id"`
				Type string          `json:"type"`
			}
			if json.Unmarshal(scanner.Bytes(), &request) != nil || request.Type != "get_state" {
				continue
			}
			fmt.Printf(`{"id":%s,"type":"response","command":"get_state","success":true,"data":{"sessionId":"terminal-order"}}`+"\n", request.ID)
			fmt.Println(`{"type":"agent_settled"}`)
			break
		}
	case "codex", "codex-session-environment":
		for scanner.Scan() {
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(scanner.Bytes(), &request) != nil || request.Method == "" || len(request.ID) == 0 {
				continue
			}
			result := json.RawMessage(`{}`)
			if request.Method == "model/list" {
				result = json.RawMessage(`{"data":[{"id":"gpt-a","displayName":"GPT A","hidden":false,"isDefault":true},{"id":"gpt-b","displayName":"GPT B","hidden":false,"isDefault":false},{"id":"gpt-hidden","displayName":"GPT Hidden","hidden":true,"isDefault":false},{"id":"gpt-a","displayName":"GPT A dup","hidden":false,"isDefault":false}]}`)
			}
			if mode == "codex-session-environment" && (request.Method == "thread/start" || request.Method == "thread/resume") {
				expected := os.Getenv("AGENTHUB_EXPECTED_LAUNCH_ENV")
				var params struct {
					Config       map[string]any `json:"config"`
					ExcludeTurns bool           `json:"excludeTurns"`
				}
				_ = json.Unmarshal(request.Params, &params)
				if os.Getenv("AGENTHUB_PROCESS_ENV") != expected ||
					params.Config["shell_environment_policy.set.AGENTHUB_PROCESS_ENV"] != expected {
					fmt.Printf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"launch environment missing"}}`+"\n", request.ID)
					continue
				}
				if request.Method == "thread/resume" && !params.ExcludeTurns {
					fmt.Printf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"resume did not exclude turns"}}`+"\n", request.ID)
					continue
				}
				if request.Method == "thread/start" && params.ExcludeTurns {
					fmt.Printf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"start unexpectedly excluded turns"}}`+"\n", request.ID)
					continue
				}
				result, _ = json.Marshal(map[string]any{"thread": map[string]any{"id": expected}})
			}
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", request.ID, result)
		}
	case "pi", "pi-session-environment", "pi-delayed-prompt":
		for scanner.Scan() {
			var request struct {
				ID   json.RawMessage `json:"id"`
				Type string          `json:"type"`
			}
			if json.Unmarshal(scanner.Bytes(), &request) != nil || request.Type == "" {
				continue
			}
			var data json.RawMessage
			switch request.Type {
			case "get_state":
				sessionID := "s1"
				if mode == "pi-session-environment" {
					sessionID = os.Getenv("AGENTHUB_PROCESS_ENV")
				}
				data, _ = json.Marshal(map[string]any{
					"model":     map[string]any{"provider": "xai", "id": "grok-9"},
					"sessionId": sessionID,
				})
			case "get_available_models":
				data = json.RawMessage(`{"models":[{"provider":"xai","id":"grok-9","name":"Grok 9"},{"provider":"kimi-coding","id":"k3","name":"Kimi K3"},{"provider":"xai","id":"grok-9","name":"Grok 9 dup"}]}`)
			case "prompt", "steer":
				if mode == "pi-delayed-prompt" {
					delay, _ := time.ParseDuration(os.Getenv("AGENTHUB_TEST_PROMPT_DELAY"))
					time.Sleep(delay)
				}
				data = json.RawMessage(`{}`)
			default:
				data = json.RawMessage(`{}`)
			}
			fmt.Printf(`{"id":%s,"type":"response","command":%q,"success":true,"data":%s}`+"\n", request.ID, request.Type, data)
			if mode == "pi-delayed-prompt" && (request.Type == "prompt" || request.Type == "steer") {
				fmt.Println(`{"type":"agent_settled"}`)
			}
		}
	case "acp", "acp-session-environment", "acp-hang-session-new", "acp-init-error", "acp-prompt-exit", "acp-delayed-prompt":
		for scanner.Scan() {
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(scanner.Bytes(), &request) != nil || request.Method == "" || len(request.ID) == 0 {
				continue
			}
			if mode == "acp-hang-session-new" && request.Method == "session/new" {
				// Never respond: simulates a provider stuck inside session/new
				// (the Kimi Code failure mode behind the session creation hang).
				continue
			}
			if mode == "acp-init-error" && request.Method == "initialize" {
				fmt.Printf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"auth required: run the provider login flow"}}`+"\n", request.ID)
				continue
			}
			var result json.RawMessage
			switch request.Method {
			case "initialize":
				result = json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"sessionCapabilities":{}}}`)
			case "session/new":
				sessionID := "session-test"
				if mode == "acp-session-environment" {
					sessionID = os.Getenv("AGENTHUB_PROCESS_ENV")
				}
				result, _ = json.Marshal(map[string]any{
					"sessionId": sessionID,
					"configOptions": []any{
						map[string]any{"id": "model", "category": "model", "currentValue": "kimi-code/k3", "options": []any{map[string]any{"value": "kimi-code/k3"}}},
						map[string]any{"id": "mode", "category": "mode", "currentValue": "yolo", "options": []any{map[string]any{"value": "yolo"}}},
					},
				})
			default:
				if mode == "acp-delayed-prompt" && request.Method == "session/prompt" {
					delay, _ := time.ParseDuration(os.Getenv("AGENTHUB_TEST_PROMPT_DELAY"))
					time.Sleep(delay)
				}
				result = json.RawMessage(`{}`)
			}
			fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", request.ID, result)
			if mode == "acp-prompt-exit" && request.Method == "session/prompt" {
				return
			}
		}
	case "sleep":
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

// helperCLI writes a wrapper script that re-executes the test binary as the
// fake provider CLI. The `--` separator keeps provider arguments like
// "--mode" from being parsed as test flags.
func helperCLI(t *testing.T, mode string) string {
	t.Helper()
	t.Setenv("AGENTHUB_TEST_HELPER", mode)
	return writeScript(t, fmt.Sprintf("exec %q -test.run='^TestHelperProcess$' -- \"$@\"", os.Args[0]))
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fakes require a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "fake-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustModels(t *testing.T, ctx context.Context, provider config.Provider) []Model {
	t.Helper()
	models, err := ListModels(ctx, provider)
	if err != nil {
		t.Fatalf("ListModels(%s): %v", provider.Type, err)
	}
	return models
}

func TestListCodexModels(t *testing.T) {
	models := mustModels(t, context.Background(), config.Provider{ID: "codex", Type: "codex", Command: helperCLI(t, "codex")})
	if len(models) != 2 {
		t.Fatalf("models = %+v, want 2 entries (hidden skipped, dup removed)", models)
	}
	if models[0].ID != "gpt-a" || models[0].Label != "GPT A" || !models[0].Default {
		t.Fatalf("models[0] = %+v", models[0])
	}
	if models[1].ID != "gpt-b" || models[1].Default {
		t.Fatalf("models[1] = %+v", models[1])
	}
}

func TestListPiModels(t *testing.T) {
	models := mustModels(t, context.Background(), config.Provider{ID: "pi", Type: "pi", Command: helperCLI(t, "pi")})
	if len(models) != 2 {
		t.Fatalf("models = %+v, want 2 entries (dup removed)", models)
	}
	if models[0].ID != "xai/grok-9" || models[0].Label != "Grok 9" || !models[0].Default {
		t.Fatalf("models[0] = %+v, want current model marked default", models[0])
	}
	if models[1].ID != "kimi-coding/k3" || models[1].Default {
		t.Fatalf("models[1] = %+v", models[1])
	}
}

func TestListKimiModels(t *testing.T) {
	command := writeScript(t, `cat <<'EOF'
{"providers":{"managed:kimi-code":{"type":"kimi"}},"models":{
  "kimi-code/kimi-for-coding":{"displayName":"K2.7 Coding"},
  "kimi-code/k3":{"displayName":"K3"},
  "kimi-code/k3-256k":{}
}}
EOF
`)
	models := mustModels(t, context.Background(), config.Provider{ID: "kimi", Type: "kimi", Command: command})
	if len(models) != 3 {
		t.Fatalf("models = %+v", models)
	}
	// Configuration order is preserved, and a missing displayName falls back
	// to the ID.
	want := []Model{
		{ID: "kimi-code/kimi-for-coding", Label: "K2.7 Coding"},
		{ID: "kimi-code/k3", Label: "K3"},
		{ID: "kimi-code/k3-256k", Label: "kimi-code/k3-256k"},
	}
	for index, model := range want {
		if models[index] != model {
			t.Fatalf("models[%d] = %+v, want %+v", index, models[index], model)
		}
	}
}

func TestListKimiModelsInvalidJSON(t *testing.T) {
	command := writeScript(t, `echo 'not json'`)
	_, err := ListModels(context.Background(), config.Provider{ID: "kimi", Type: "kimi", Command: command})
	var modelErr *ModelError
	if err == nil || !errors.As(err, &modelErr) || modelErr.Kind != ModelErrUpstream {
		t.Fatalf("err = %v, want upstream ModelError", err)
	}
}

func TestListOpenCodeModels(t *testing.T) {
	command := writeScript(t, `cat <<'EOF'
opencode/big-pickle
{
  "id": "big-pickle",
  "name": "Big Pickle"
}
kimi-for-coding/k3
{
  "id": "k3",
  "name": "Kimi For Coding/Kimi K3"
}
opencode/broken
{not json
EOF
`)
	models := mustModels(t, context.Background(), config.Provider{ID: "opencode", Type: "opencode", Command: command})
	want := []Model{
		{ID: "opencode/big-pickle", Label: "Big Pickle"},
		{ID: "kimi-for-coding/k3", Label: "Kimi For Coding/Kimi K3"},
		{ID: "opencode/broken", Label: "opencode/broken"},
	}
	if len(models) != len(want) {
		t.Fatalf("models = %+v", models)
	}
	for index, model := range want {
		if models[index] != model {
			t.Fatalf("models[%d] = %+v, want %+v", index, models[index], model)
		}
	}
}

func TestParseOpenCodeModelsPlainOutput(t *testing.T) {
	// The non-verbose layout (IDs only) also parses, with ID labels.
	models := parseOpenCodeModels("opencode/a\nkimi-for-coding/k3\n\n")
	if len(models) != 2 || models[0].ID != "opencode/a" || models[1].Label != "kimi-for-coding/k3" {
		t.Fatalf("models = %+v", models)
	}
}

func TestListModelsMissingCLI(t *testing.T) {
	_, err := ListModels(context.Background(), config.Provider{ID: "codex", Type: "codex", Command: "/nonexistent/agenthub-test/codex"})
	var modelErr *ModelError
	if err == nil || !errors.As(err, &modelErr) || modelErr.Kind != ModelErrUnavailable {
		t.Fatalf("err = %v, want unavailable ModelError", err)
	}
}

func TestListModelsUpstreamFailure(t *testing.T) {
	command := writeScript(t, `echo 'boom: everything is broken' >&2
exit 3`)
	_, err := ListModels(context.Background(), config.Provider{ID: "opencode", Type: "opencode", Command: command})
	var modelErr *ModelError
	if err == nil || !errors.As(err, &modelErr) || modelErr.Kind != ModelErrUpstream {
		t.Fatalf("err = %v, want upstream ModelError", err)
	}
	if !strings.Contains(err.Error(), "boom: everything is broken") {
		t.Fatalf("stderr diagnostics missing from error: %v", err)
	}
}

func TestListModelsTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := ListModels(ctx, config.Provider{ID: "codex", Type: "codex", Command: helperCLI(t, "sleep")})
	var modelErr *ModelError
	if err == nil || !errors.As(err, &modelErr) || modelErr.Kind != ModelErrTimeout {
		t.Fatalf("err = %v, want timeout ModelError", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout took too long: %s", elapsed)
	}
}

func TestListModelsUnsupportedType(t *testing.T) {
	// ResolveProviderCommand does not know the type, so use a command via the
	// supported-type switch only after resolution: an unknown type with a
	// resolvable command fails as unavailable/unsupported.
	_, err := ListModels(context.Background(), config.Provider{ID: "x", Type: "unknown", Command: "sh"})
	if err == nil {
		t.Fatal("want error for unsupported provider type")
	}
}
