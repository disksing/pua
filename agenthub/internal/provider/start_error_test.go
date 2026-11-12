package provider

import (
	"errors"
	"testing"
)

func TestPiMissingNativeSessionIsTerminalStartupError(t *testing.T) {
	value := newPi(helperCLI(t, "pi-native-missing"), Options{Cwd: t.TempDir()})
	err := value.Start("missing-native-session")
	if !errors.Is(err, ErrNativeSessionMissing) {
		t.Fatalf("missing session error = %v", err)
	}
	if err := value.Close(); err != nil {
		t.Fatal(err)
	}
}
