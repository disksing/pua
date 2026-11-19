package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefreshTurnGuidancePreservesCustomInstructions(t *testing.T) {
	ws, err := Initialize(t.TempDir(), "en")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ws.Root(), "AGENTS.md")
	if err := os.WriteFile(path, []byte("Custom instructions.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ws.RefreshTurnGuidance("workspace"); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "Custom instructions.") ||
		strings.Count(string(data), puaPromptStart) != 1 || !strings.Contains(string(data), "pua turn switch --message=") ||
		!strings.Contains(string(data), "pua turn switch --to=chat") {
		t.Fatalf("bad guidance refresh: %s", data)
	}
}

func TestCurrentTurnGuidanceGateAndRefresh(t *testing.T) {
	for _, language := range []string{"en", "zh-CN"} {
		for _, state := range []string{"custom", "missing", "stale", "duplicate", "incomplete", "reversed", "directory"} {
			t.Run(language+"/"+state, func(t *testing.T) {
				ws, err := Initialize(t.TempDir(), language)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(ws.Root(), "AGENTS.md")
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				contents := "Custom prefix.\n" + string(original) + "\nCustom suffix.\n"
				switch state {
				case "missing":
					err = os.Remove(path)
				case "stale":
					contents = "Custom prefix.\n" + puaPromptStart + "\nlegacy rules\n" + puaPromptEnd + "\nCustom suffix.\n"
				case "duplicate":
					contents += puaPromptStart
				case "incomplete":
					contents = puaPromptStart
				case "reversed":
					contents = puaPromptEnd + "\n" + puaPromptStart
				case "directory":
					if err = os.Remove(path); err == nil {
						err = os.Mkdir(path, 0755)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				if state != "missing" && state != "directory" {
					if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
						t.Fatal(err)
					}
				}
				before, _ := os.ReadFile(path)
				current, gateErr := ws.HasCurrentTurnGuidance()
				if current != (state == "custom") {
					t.Fatalf("guidance gate: current=%t err=%v", current, gateErr)
				}
				after, _ := os.ReadFile(path)
				if string(before) != string(after) {
					t.Fatal("read-only gate changed guidance")
				}
				refreshErr := ws.RefreshTurnGuidance("workspace")
				repairable := state == "custom" || state == "missing" || state == "stale"
				if (refreshErr == nil) != repairable {
					t.Fatalf("refresh: %v", refreshErr)
				}
				if repairable {
					current, err = ws.HasCurrentTurnGuidance()
					if err != nil || !current {
						t.Fatalf("refreshed guidance not usable: %t %v", current, err)
					}
					if state != "missing" {
						refreshed, _ := os.ReadFile(path)
						if !strings.HasPrefix(string(refreshed), "Custom prefix.") ||
							!strings.HasSuffix(string(refreshed), "Custom suffix.\n") {
							t.Fatal("custom instructions were overwritten")
						}
					}
				}
			})
		}
	}
}
