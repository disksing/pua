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
	processGroup, startID, err := parseLinuxServiceProcessStat(stat)
	if err != nil {
		return serviceProcessIdentity{}, err
	}
	return serviceProcessIdentity{
		pid:          pid,
		command:      strings.ReplaceAll(string(commandData), "\x00", " "),
		environment:  splitNullTerminated(environmentData),
		processGroup: processGroup,
		startID:      startID,
	}, nil
}

func readServiceProcessGroupMembers(processGroup int) ([]serviceProcessIdentity, error) {
	if processGroup <= 0 {
		return nil, errProcessIdentityUnavailable
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	members := make([]serviceProcessIdentity, 0)
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || pid <= 0 {
			continue
		}
		statPath := filepath.Join("/proc", entry.Name(), "stat")
		stat, statErr := os.ReadFile(statPath)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return nil, statErr
		}
		candidateGroup, _, statErr := parseLinuxServiceProcessStat(stat)
		if statErr != nil {
			return nil, fmt.Errorf("inspect service process group %d candidate %d: %w", processGroup, pid, statErr)
		}
		if candidateGroup != processGroup {
			continue
		}
		identity, identityErr := readServiceProcessIdentity(pid)
		if os.IsNotExist(identityErr) {
			continue
		}
		if identityErr != nil {
			return nil, fmt.Errorf("inspect service process group %d member %d: %w", processGroup, pid, identityErr)
		}
		if identity.processGroup == processGroup {
			members = append(members, identity)
		}
	}
	return members, nil
}

func parseLinuxServiceProcessStat(stat []byte) (int, string, error) {
	closeParen := bytes.LastIndexByte(stat, ')')
	if closeParen < 0 || closeParen+2 >= len(stat) {
		return 0, "", fmt.Errorf("%w: malformed proc stat", errProcessIdentityUnavailable)
	}
	fields := strings.Fields(string(stat[closeParen+2:]))
	if len(fields) < 20 {
		return 0, "", fmt.Errorf("%w: malformed proc stat", errProcessIdentityUnavailable)
	}
	processGroup, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, "", fmt.Errorf("%w: malformed process group", errProcessIdentityUnavailable)
	}
	return processGroup, fields[19], nil
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

func serviceProcessIdentityInspectionAvailable() bool { return true }

func serviceProcessIdentityMarkerRequired() bool { return false }

func platformServiceProcessGroupMemberMatches(identity serviceProcessIdentity, token, markerPath string) (bool, error) {
	return environmentHasSingleValue(identity.environment, serviceInstanceTokenEnvironment, token), nil
}

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
