//go:build integration && !windows

package integration_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestPUAGatePendingStartupFailureAndStopSurviveRestart(t *testing.T) {
	for _, stop := range []bool{false, true} {
		name := "budget"
		if stop {
			name = "requested stop"
		}
		t.Run(name, func(t *testing.T) {
			gate := newGate(t)
			trigger := filepath.Join(gate.root, "fail-startup")
			code, body := gate.create(map[string]string{"FAKE_MODE": "startup-crash-on-trigger", "FAKE_STARTUP_CRASH_TRIGGER": trigger}, nil)
			if code != http.StatusCreated {
				t.Fatalf("create: %d %v", code, body)
			}
			value := decodeSession(t, body)
			path := "/v1/sessions/" + value.ID
			code, body = gate.request(http.MethodPost, path+"/stop", map[string]any{})
			if code != http.StatusOK {
				t.Fatalf("initial stop: %d %v", code, body)
			}
			if err := os.WriteFile(trigger, []byte("fail"), 0600); err != nil {
				t.Fatal(err)
			}
			input := map[string]any{"text": "scheduler tick", "messageId": "pending-tick"}
			code, body = gate.request(http.MethodPost, path+"/messages", input)
			if code != http.StatusAccepted {
				t.Fatalf("message: %d %v", code, body)
			}
			if stop {
				code, body = gate.request(http.MethodPost, path+"/stop", map[string]any{})
				if code != http.StatusOK {
					t.Fatalf("stop pending: %d %v", code, body)
				}
			}
			value = gate.waitSession(value.ID, func(current sessionValue) bool { return current.State == "stopped" && current.CurrentTurnID == "" })
			wantReason, wantTerminal := "startup_error", "turn.failed"
			if stop {
				wantReason, wantTerminal = "requested", "turn.cancelled"
			}
			if value.StopReason != wantReason {
				t.Fatalf("settled session = %+v", value)
			}
			before := gate.events(value.ID)
			if countType(before, wantTerminal) != 1 {
				t.Fatalf("terminal events = %v", eventTypes(before))
			}
			// Semantic event frames intentionally omit process and delivery
			// bookkeeping; inspect this isolated daemon's canonical log.
			countStarts := func() int {
				data, err := os.ReadFile(filepath.Join(gate.root, "data", "sessions", value.ID, "events.jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				for _, line := range bytes.Split(data, []byte{'\n'}) {
					var event struct {
						Type string `json:"type"`
					}
					if json.Unmarshal(line, &event) == nil && event.Type == "provider.process.started" {
						count++
					}
				}
				return count
			}
			starts := countStarts()
			if !stop && starts != 4 {
				t.Fatalf("startup budget: starts=%d, want initial start + 3 attempts", starts)
			}
			gate.stop()
			gate = startGate(t, gate.root, gate.cwd)
			code, body = gate.request(http.MethodPost, path+"/messages", input)
			if code != http.StatusAccepted {
				t.Fatalf("stable replay: %d %v", code, body)
			}
			after := gate.events(value.ID)
			if countStarts() != starts || countType(after, "message.input") != 1 || countType(after, wantTerminal) != 1 {
				t.Fatalf("restart resurrected terminal input: %v", eventTypes(after))
			}
			code, body = gate.request(http.MethodDelete, path, map[string]any{})
			if code != http.StatusOK {
				t.Fatalf("archive: %d %v", code, body)
			}
		})
	}
}
