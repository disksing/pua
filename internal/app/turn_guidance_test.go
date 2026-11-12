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
		strings.Count(string(data), puaPromptStart) != 1 || !strings.Contains(string(data), "pua turn yield") {
		t.Fatalf("bad guidance refresh: %s", data)
	}
}
