//go:build linux

package serve

import "testing"

func TestLinuxProcessIdentityRejectsEnvironmentSpoofing(t *testing.T) {
	const (
		processGroup = 41
		startID      = "12345"
		token        = "instance-token"
		digest       = "command-digest"
	)
	tests := []struct {
		name        string
		environment []string
	}{
		{name: "missing token", environment: []string{serviceCommandDigestEnvironment + "=" + digest}},
		{name: "prefix spoof", environment: []string{"X_" + serviceInstanceTokenEnvironment + "=" + token, serviceCommandDigestEnvironment + "=" + digest}},
		{name: "duplicate token", environment: []string{serviceInstanceTokenEnvironment + "=" + token, serviceInstanceTokenEnvironment + "=" + token, serviceCommandDigestEnvironment + "=" + digest}},
		{name: "wrong digest", environment: []string{serviceInstanceTokenEnvironment + "=" + token, serviceCommandDigestEnvironment + "=wrong"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity := serviceProcessIdentity{command: "/bin/service", environment: test.environment, processGroup: processGroup, startID: startID}
			if serviceProcessIdentityMatches(identity, processGroup, startID, token, digest) {
				t.Fatal("spoofed Linux identity matched")
			}
		})
	}
}
