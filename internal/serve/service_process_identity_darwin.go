//go:build darwin

package serve

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unsafe"

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
		pid:          pid,
		command:      strings.Join(arguments, " "),
		environment:  environment,
		processGroup: int(process.Eproc.Pgid),
		startID:      fmt.Sprintf("%d:%06d", process.Proc.P_starttime.Sec, process.Proc.P_starttime.Usec),
	}, nil
}

func readServiceProcessGroupMembers(processGroup int) ([]serviceProcessIdentity, error) {
	if processGroup <= 0 {
		return nil, errProcessIdentityUnavailable
	}
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", processGroup)
	if err != nil {
		return nil, err
	}
	members := make([]serviceProcessIdentity, 0, len(processes))
	for _, process := range processes {
		pid := int(process.Proc.P_pid)
		if pid <= 0 || int(process.Eproc.Pgid) != processGroup {
			continue
		}
		identity, identityErr := readServiceProcessIdentity(pid)
		if identityErr != nil {
			present, probeErr := processPresent(pid)
			if probeErr != nil {
				return nil, probeErr
			}
			if !present {
				continue
			}
			return nil, fmt.Errorf("inspect service process group %d member %d: %w", processGroup, pid, identityErr)
		}
		if identity.processGroup == processGroup {
			members = append(members, identity)
		}
	}
	return members, nil
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

func serviceProcessIdentityMarkerRequired() bool { return true }

func platformServiceProcessGroupMemberMatches(identity serviceProcessIdentity, token, markerPath string) (bool, error) {
	if environmentHasSingleValue(identity.environment, serviceInstanceTokenEnvironment, token) {
		return true, nil
	}
	if markerPath == "" {
		return false, nil
	}
	return darwinProcessHasIdentityMarker(identity.pid, markerPath)
}

func darwinProcessHasIdentityMarker(pid int, markerPath string) (bool, error) {
	const (
		procInfoCallPIDFDInfo      = 3
		procPIDFDVnodePathInfo     = 2
		darwinVnodePathBufferBytes = 2048
		darwinMaxPathBytes         = 1024
	)
	if pid <= 0 || markerPath == "" {
		return false, nil
	}
	buffer := make([]byte, darwinVnodePathBufferBytes)
	count, _, errno := unix.Syscall6(
		unix.SYS_PROC_INFO,
		procInfoCallPIDFDInfo,
		uintptr(pid),
		procPIDFDVnodePathInfo,
		serviceProcessIdentityMarkerFD,
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)),
	)
	if errno != 0 {
		if errors.Is(errno, unix.EBADF) || errors.Is(errno, unix.ESRCH) {
			return false, nil
		}
		return false, errno
	}
	if count < darwinMaxPathBytes || count > uintptr(len(buffer)) {
		return false, fmt.Errorf("%w: malformed vnode path response", errProcessIdentityUnavailable)
	}
	pathData := buffer[int(count)-darwinMaxPathBytes : int(count)]
	if end := bytes.IndexByte(pathData, 0); end >= 0 {
		pathData = pathData[:end]
	}
	return string(pathData) == markerPath, nil
}

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
