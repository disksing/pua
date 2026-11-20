package pua

import (
	"strings"
	"testing"
)

func TestCLIRejectsRetiredChatCommands(t *testing.T) {
	t.Setenv(puaResourceIDEnvironment, "workspace")
	t.Setenv("PUA_CHANNEL", "internal")
	for _, operation := range []string{"show", "notes", "publish"} {
		_, err := runErr(t, "chat", operation, "--reply-to=old-chat.old-token")
		if err == nil || !strings.Contains(err.Error(), "pua chat show --work=") {
			t.Fatalf("legacy chat %s returned %v", operation, err)
		}
	}
	t.Setenv("PUA_CHANNEL", "dialogue")
	if dialogueCommandAllowed([]string{"chat", "show", "--work=old-work"}) ||
		dialogueCommandAllowed([]string{"message", "send"}) ||
		!dialogueCommandAllowed([]string{"history", "turn", "show"}) {
		t.Fatal("retired dialogue command allowlist changed write or history access")
	}
}
