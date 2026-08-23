//go:build darwin

package serve

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

func readServiceProcessIdentity(pid int) (serviceProcessIdentity, error) {
	if pid <= 0 {
		return serviceProcessIdentity{}, errProcessIdentityUnavailable
	}
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return serviceProcessIdentity{}, err
	}
	arguments, environment, err := parseDarwinProcessArguments(data)
	if err != nil {
		return serviceProcessIdentity{}, err
	}
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return serviceProcessIdentity{}, err
	}
	if process.Proc.P_pid != int32(pid) {
		return serviceProcessIdentity{}, fmt.Errorf("%w: process changed during inspection", errProcessIdentityUnavailable)
	}
	return serviceProcessIdentity{
		command:      strings.Join(arguments, " "),
		environment:  environment,
		processGroup: int(process.Eproc.Pgid),
		startID:      fmt.Sprintf("%d:%06d", process.Proc.P_starttime.Sec, process.Proc.P_starttime.Usec),
	}, nil
}

func platformServiceProcessIdentityMatches(identity serviceProcessIdentity, startID, token, digest string) bool {
	// macOS may withhold envp from kern.procargs2 for another process. The
	// kernel-assigned creation timestamp identifies the exact PID incarnation
	// without parsing ps output or trusting wall-clock
	// proximity. Token and digest must still be present in the persisted launch
	// record, while the opaque start ID is the native proof checked here.
	return startID != "" && identity.startID == startID && token != "" && digest != ""
}

func serviceProcessIdentityRequired() bool { return true }

// kern.procargs2 returns argc, the executable path, argv, and envp in one
// NUL-delimited buffer. Parsing it directly avoids depending on ps output,
// shell quoting, locale, or command-line truncation.
func parseDarwinProcessArguments(data []byte) ([]string, []string, error) {
	if len(data) < 4 {
		return nil, nil, fmt.Errorf("%w: short kern.procargs2 response", errProcessIdentityUnavailable)
	}
	argumentCount := int(binary.NativeEndian.Uint32(data[:4]))
	if argumentCount <= 0 || argumentCount > len(data) {
		return nil, nil, fmt.Errorf("%w: invalid argument count", errProcessIdentityUnavailable)
	}
	cursor := 4
	_, cursor, ok := nextDarwinProcessField(data, cursor)
	if !ok {
		return nil, nil, fmt.Errorf("%w: missing executable path", errProcessIdentityUnavailable)
	}
	for cursor < len(data) && data[cursor] == 0 {
		cursor++
	}
	arguments := make([]string, 0, argumentCount)
	for len(arguments) < argumentCount {
		field, next, ok := nextDarwinProcessField(data, cursor)
		if !ok || field == "" {
			return nil, nil, fmt.Errorf("%w: truncated process arguments", errProcessIdentityUnavailable)
		}
		arguments = append(arguments, field)
		cursor = next
	}
	environment := make([]string, 0)
	for cursor < len(data) {
		if data[cursor] == 0 {
			cursor++
			continue
		}
		field, next, ok := nextDarwinProcessField(data, cursor)
		if !ok || !strings.ContainsRune(field, '=') {
			return nil, nil, fmt.Errorf("%w: malformed process environment", errProcessIdentityUnavailable)
		}
		environment = append(environment, field)
		cursor = next
	}
	return arguments, environment, nil
}

func nextDarwinProcessField(data []byte, cursor int) (string, int, bool) {
	if cursor < 0 || cursor >= len(data) {
		return "", cursor, false
	}
	end := bytes.IndexByte(data[cursor:], 0)
	if end < 0 {
		return "", cursor, false
	}
	end += cursor
	return string(data[cursor:end]), end + 1, true
}
