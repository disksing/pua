package provider

import "testing"

func TestInputCapabilitiesByProvider(t *testing.T) {
	for _, test := range []struct {
		provider string
		steer    bool
	}{{"codex", true}, {"pi", true}, {"kimi", false}, {"opencode", false}} {
		if got := InputCapabilities(test.provider).Steer; got != test.steer {
			t.Fatalf("%s steer = %v, want %v", test.provider, got, test.steer)
		}
		if got := InputCapabilities(test.provider).TurnScopedSteer; got != (test.provider == "codex") {
			t.Fatalf("%s unexpected turn-scoped capability: %v", test.provider, got)
		}
	}
}
