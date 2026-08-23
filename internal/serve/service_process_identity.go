package serve

import (
	"errors"
	"strings"
)

const (
	serviceInstanceTokenEnvironment = "PUA_SERVICE_INSTANCE_TOKEN"
	serviceCommandDigestEnvironment = "PUA_SERVICE_COMMAND_DIGEST"
	serviceProcessIdentityMarkerFD  = 3
)

type serviceProcessIdentity struct {
	pid          int
	command      string
	environment  []string
	processGroup int
	startID      string
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

var errProcessIdentityUnavailable = errors.New("process identity is unavailable")
