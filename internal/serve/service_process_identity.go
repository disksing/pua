package serve

import (
	"errors"
	"strings"
)

const (
	serviceInstanceTokenEnvironment = "PUA_SERVICE_INSTANCE_TOKEN"
	serviceCommandDigestEnvironment = "PUA_SERVICE_COMMAND_DIGEST"
)

type serviceProcessIdentity struct {
	command      string
	environment  []string
	processGroup int
	startID      string
}

// processIdentityMatches is deliberately fail-closed. A persisted PID is
// owned only when native process metadata still identifies the recorded
// launch and the process remains the expected process-group leader.
func processIdentityMatches(pid, processGroup int, startID, token, digest string) bool {
	if pid <= 0 || processGroup <= 0 || pid != processGroup || token == "" || digest == "" ||
		strings.ContainsRune(token, '\x00') || strings.ContainsRune(digest, '\x00') {
		return false
	}
	identity, err := readServiceProcessIdentity(pid)
	if err != nil {
		return false
	}
	return serviceProcessIdentityMatches(identity, processGroup, startID, token, digest)
}

func serviceProcessIdentityMatches(identity serviceProcessIdentity, processGroup int, startID, token, digest string) bool {
	if strings.TrimSpace(identity.command) == "" || identity.processGroup != processGroup {
		return false
	}
	return platformServiceProcessIdentityMatches(identity, startID, token, digest)
}

func environmentHasSingleValue(environment []string, name, want string) bool {
	prefix := name + "="
	found := false
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			continue
		}
		if found || strings.TrimPrefix(entry, prefix) != want {
			return false
		}
		found = true
	}
	return found
}

// readProcessCommand is kept small and side-effect free for diagnostics and
// tests that need to verify PID-reuse protection.
func readProcessCommand(pid int) string {
	identity, err := readServiceProcessIdentity(pid)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(identity.command)
}

var errProcessIdentityUnavailable = errors.New("process identity is unavailable")
