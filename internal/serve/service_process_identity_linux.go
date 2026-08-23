//go:build linux

package serve

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func readServiceProcessIdentity(pid int) (serviceProcessIdentity, error) {
	if pid <= 0 {
		return serviceProcessIdentity{}, errProcessIdentityUnavailable
	}
	base := filepath.Join("/proc", strconv.Itoa(pid))
	environmentData, err := os.ReadFile(filepath.Join(base, "environ"))
	if err != nil {
		return serviceProcessIdentity{}, err
	}
	commandData, err := os.ReadFile(filepath.Join(base, "cmdline"))
	if err != nil {
		return serviceProcessIdentity{}, err
	}
	stat, err := os.ReadFile(filepath.Join(base, "stat"))
	if err != nil {
		return serviceProcessIdentity{}, err
	}
	closeParen := bytes.LastIndexByte(stat, ')')
	if closeParen < 0 || closeParen+2 >= len(stat) {
		return serviceProcessIdentity{}, fmt.Errorf("%w: malformed proc stat", errProcessIdentityUnavailable)
	}
	fields := strings.Fields(string(stat[closeParen+2:]))
	if len(fields) < 20 {
		return serviceProcessIdentity{}, fmt.Errorf("%w: malformed proc stat", errProcessIdentityUnavailable)
	}
	processGroup, err := strconv.Atoi(fields[2])
	if err != nil {
		return serviceProcessIdentity{}, fmt.Errorf("%w: malformed process group", errProcessIdentityUnavailable)
	}
	return serviceProcessIdentity{
		command:      strings.ReplaceAll(string(commandData), "\x00", " "),
		environment:  splitNullTerminated(environmentData),
		processGroup: processGroup,
		startID:      fields[19],
	}, nil
}

func platformServiceProcessIdentityMatches(identity serviceProcessIdentity, startID, token, digest string) bool {
	// Older Linux state has no start ID, so the existing token/digest contract
	// remains sufficient there. New state also checks the kernel start tick.
	if startID != "" && identity.startID != startID {
		return false
	}
	return environmentHasSingleValue(identity.environment, serviceInstanceTokenEnvironment, token) &&
		environmentHasSingleValue(identity.environment, serviceCommandDigestEnvironment, digest)
}

func serviceProcessIdentityRequired() bool { return true }

func splitNullTerminated(data []byte) []string {
	parts := bytes.Split(data, []byte{0})
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) > 0 {
			result = append(result, string(part))
		}
	}
	return result
}
