package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/disksing/pua/internal/security"
)

const serviceStartupLogBufferBytes = 1 << 20

const serviceCommandWaitDelay = 250 * time.Millisecond

const (
	serviceProcessGroupPollInterval = 10 * time.Millisecond
	serviceProcessGroupKillWait     = time.Second
)

type serviceProcessGroupIdentity struct {
	leaderPID       int
	processGroup    int
	startID         string
	instanceToken   string
	commandDigest   string
	markerPath      string
	ownership       serviceProcessOwnership
	processPlatform *serviceProcessPlatform
}

// serviceProcessOwnership records whether ownership comes from the current
// manager's uninterrupted launch or must be reconstructed from durable state.
// The zero value is deliberately the fail-closed reconstructed case.
type serviceProcessOwnership uint8

const (
	serviceProcessOwnershipReconstructed serviceProcessOwnership = iota
	serviceProcessOwnershipCurrentManager
)

// serviceProcessPlatform keeps process inspection and signaling injectable so
// unsupported-platform ownership behavior can be verified without ever
// signaling a real numeric PID or process group.
type serviceProcessPlatform struct {
	identityInspectionAvailable bool
	identityMarkerRequired      bool
	processGroupPresent         func(int) (bool, error)
	processPresent              func(int) (bool, error)
	readProcessIdentity         func(int) (serviceProcessIdentity, error)
	readProcessGroupMembers     func(int) ([]serviceProcessIdentity, error)
	processGroupMemberMatches   func(serviceProcessIdentity, string, string) (bool, error)
	signalProcessGroup          func(int, syscall.Signal) error
}

func nativeServiceProcessPlatform() *serviceProcessPlatform {
	return &serviceProcessPlatform{
		identityInspectionAvailable: serviceProcessIdentityInspectionAvailable(),
		identityMarkerRequired:      serviceProcessIdentityMarkerRequired(),
		processGroupPresent:         processGroupPresent,
		processPresent:              processPresent,
		readProcessIdentity:         readServiceProcessIdentity,
		readProcessGroupMembers:     readServiceProcessGroupMembers,
		processGroupMemberMatches:   platformServiceProcessGroupMemberMatches,
		signalProcessGroup:          signalProcessGroup,
	}
}

func (identity serviceProcessGroupIdentity) platform() *serviceProcessPlatform {
	if identity.processPlatform != nil {
		return identity.processPlatform
	}
	return nativeServiceProcessPlatform()
}

func serviceProcessIdentityMarkerPath(root, serviceID, token string) string {
	if root == "" || serviceID == "" || token == "" || strings.ContainsRune(token, filepath.Separator) {
		return ""
	}
	return filepath.Join(serviceRuntimePath(root, serviceID), ".identity-"+token)
}

func openServiceProcessIdentityMarker(root, serviceID, token string) (*os.File, string, error) {
	path := serviceProcessIdentityMarkerPath(root, serviceID, token)
	if path == "" {
		return nil, "", errors.New("service process identity marker path is unavailable")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("create service process identity marker: %w", err)
	}
	return file, path, nil
}

type serviceLogSink struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	maxFiles int
	file     *os.File
	size     int64
}

// runServiceGroupCommand runs a bounded supervisor hook in its own process
// group. A hook is complete only after both its leader and captured output are
// finished: a background descendant that retains the output descriptor must
// therefore be reaped when the command context expires.
func runServiceGroupCommand(ctx context.Context, command []string, dir string, env []string, output io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = serviceCommandWaitDelay
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return terminateProcessGroup(cmd.Process.Pid, true)
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return err
	}
	_ = writer.Close()

	copied := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(output, reader)
		copied <- copyErr
	}()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	var waitErr, copyErr error
	waitComplete, copyComplete := false, false
	cancelled := false
	cancelledCh := ctx.Done()
	var forceTimer *time.Timer
	var forceCh <-chan time.Time
	for !waitComplete || !copyComplete {
		select {
		case waitErr = <-waited:
			waitComplete = true
			waited = nil
		case copyErr = <-copied:
			copyComplete = true
			copied = nil
		case <-cancelledCh:
			cancelled = true
			cancelledCh = nil
			// Kill the whole group before waiting for the leader or pipe reader.
			_ = terminateProcessGroup(cmd.Process.Pid, true)
			forceTimer = time.NewTimer(serviceCommandWaitDelay)
			forceCh = forceTimer.C
		case <-forceCh:
			forceCh = nil
			_ = cmd.Process.Kill()
			_ = reader.Close()
		}
	}
	if forceTimer != nil && !forceTimer.Stop() {
		select {
		case <-forceTimer.C:
		default:
		}
	}
	_ = terminateProcessGroup(cmd.Process.Pid, true)
	_ = reader.Close()
	if waitErr != nil {
		return waitErr
	}
	if cancelled {
		return ctx.Err()
	}
	return copyErr
}

// serviceLogWriter keeps a declared exporter's startup output in a bounded
// private buffer until the initial hand-off has registered every exported
// secret. This prevents a service that prints an exported secret before its
// first export from leaking that value into the durable log.
type serviceLogWriter struct {
	mu          sync.Mutex
	sink        *serviceLogSink
	stream      *security.Stream
	beforeWrite func() error
	gated       bool
	blocked     error
	buffer      []byte
}

func newServiceLogWriter(sink *serviceLogSink, redactor *security.Redactor, gated bool, beforeWrite func() error) *serviceLogWriter {
	return &serviceLogWriter{sink: sink, stream: redactor.NewStream(sink), beforeWrite: beforeWrite, gated: gated}
}

func (w *serviceLogWriter) Write(data []byte) (int, error) {
	if w == nil {
		return 0, io.ErrClosedPipe
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.gated {
		remaining := serviceStartupLogBufferBytes - len(w.buffer)
		if remaining > 0 {
			if remaining > len(data) {
				remaining = len(data)
			}
			w.buffer = append(w.buffer, data[:remaining]...)
		}
		// Always report the input as consumed. Dropping excess startup output is
		// safer than allowing an unregistered secret into a durable sink.
		return len(data), nil
	}
	if w.blocked != nil {
		return 0, w.blocked
	}
	if w.beforeWrite != nil {
		if err := w.beforeWrite(); err != nil {
			w.blocked = err
			return 0, err
		}
	}
	return w.stream.Write(data)
}

// Release publishes the bounded startup buffer after the caller has loaded and
// registered the service's initial export secrets. The export guard runs while
// holding the writer lock so an atomic replacement that raced with the initial
// hand-off is observed before any later bytes can reach the streaming redactor.
func (w *serviceLogWriter) Release() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.gated {
		return w.blocked
	}
	if w.beforeWrite != nil {
		if err := w.beforeWrite(); err != nil {
			w.buffer = nil
			w.gated = false
			w.blocked = err
			return err
		}
	}
	w.gated = false
	buffer := w.buffer
	w.buffer = nil
	if len(buffer) == 0 {
		return nil
	}
	_, err := w.stream.Write(buffer)
	return err
}

func (w *serviceLogWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	// If the startup handshake never succeeded, discard all gated output rather
	// than flushing bytes whose secrets were not discoverable from an export.
	if w.gated || w.blocked != nil {
		w.buffer = nil
		w.gated = false
		err := w.sink.Close()
		w.mu.Unlock()
		return err
	}
	if w.beforeWrite != nil {
		if err := w.beforeWrite(); err != nil {
			// Do not flush the stream's retained suffix when the current export is
			// invalid. Its secret contents cannot be proven safe to persist.
			w.blocked = err
			closeErr := w.sink.Close()
			w.mu.Unlock()
			if closeErr != nil {
				return closeErr
			}
			return err
		}
	}
	err := w.stream.Close()
	w.mu.Unlock()
	return err
}

func newServiceLogSink(path string, rotation ServiceLogRotationConfig) *serviceLogSink {
	if rotation.MaxBytes <= 0 {
		rotation.MaxBytes = defaultLogMaxBytes
	}
	if rotation.MaxFiles <= 0 {
		rotation.MaxFiles = defaultLogMaxFiles
	}
	return &serviceLogSink{path: path, maxBytes: rotation.MaxBytes, maxFiles: rotation.MaxFiles}
}

func (s *serviceLogSink) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(data) == 0 {
		return 0, nil
	}
	written := 0
	for written < len(data) {
		if err := s.ensureFileLocked(); err != nil {
			return written, err
		}
		if s.maxBytes > 0 && s.size >= s.maxBytes {
			if err := s.rotateLocked(); err != nil {
				return written, err
			}
		}
		chunk := data[written:]
		if s.maxBytes > 0 {
			remaining := s.maxBytes - s.size
			if int64(len(chunk)) > remaining {
				chunk = chunk[:remaining]
			}
		}
		n, err := s.file.Write(chunk)
		s.size += int64(n)
		written += n
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (s *serviceLogSink) ensureFileLocked() error {
	if s.file != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(s.path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlinked service log %s", s.path)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, statErr := file.Stat()
	if statErr == nil {
		s.size = info.Size()
	}
	s.file = file
	return nil
}

func (s *serviceLogSink) rotateLocked() error {
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
	for index := s.maxFiles - 1; index >= 1; index-- {
		old := fmt.Sprintf("%s.%d", s.path, index)
		next := fmt.Sprintf("%s.%d", s.path, index+1)
		if index+1 >= s.maxFiles {
			_ = os.Remove(next)
		}
		_ = os.Rename(old, next)
	}
	if s.maxFiles > 1 {
		_ = os.Rename(s.path, s.path+".1")
	} else {
		_ = os.Remove(s.path)
	}
	s.size = 0
	return s.ensureFileLocked()
}

func (s *serviceLogSink) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Sync()
	closeErr := s.file.Close()
	s.file = nil
	if err != nil {
		return err
	}
	return closeErr
}

func terminateProcessGroup(pgid int, force bool) error {
	if pgid <= 0 {
		return nil
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	return signalProcessGroup(pgid, signal)
}

func signalProcessGroup(pgid int, signal syscall.Signal) error {
	if pgid <= 0 {
		return nil
	}
	err := syscall.Kill(-pgid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("signal process group %d: %w", pgid, err)
	}
	return nil
}

func processGroupPresent(pgid int) (bool, error) {
	if pgid <= 0 {
		return false, nil
	}
	err := syscall.Kill(-pgid, 0)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return true, fmt.Errorf("probe process group %d: %w", pgid, err)
}

func processPresent(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return true, fmt.Errorf("probe process %d: %w", pid, err)
}

// ownedServiceProcessGroupPresent checks the group before every graceful or
// forceful signal. A current manager may rely on uninterrupted launch
// continuity when native inspection is unavailable. Reconstructed ownership
// always requires native proof: a live leader must match its durable identity,
// and every residual member must carry the persisted launch token. Once proof
// or launch continuity has been established, the leader may disappear while
// descendants drain. If the numeric PID or process group was reused,
// verification fails before a signal is sent.
func ownedServiceProcessGroupPresent(identity serviceProcessGroupIdentity, allowLeaderExit bool) (bool, error) {
	platform := identity.platform()
	present, err := platform.processGroupPresent(identity.processGroup)
	if err != nil || !present {
		return present, err
	}
	leaderPresent, err := platform.processPresent(identity.leaderPID)
	if err != nil {
		return true, err
	}
	if !leaderPresent {
		if allowLeaderExit || identity.ownership == serviceProcessOwnershipCurrentManager {
			return true, nil
		}
		return ownedResidualServiceProcessGroupPresent(identity)
	}
	if identity.ownership == serviceProcessOwnershipCurrentManager && !platform.identityInspectionAvailable {
		return true, nil
	}
	if !platform.identityInspectionAvailable {
		return true, fmt.Errorf("service process group %d ownership identity inspection is unavailable", identity.processGroup)
	}
	var current serviceProcessIdentity
	for attempt := 0; attempt < 3; attempt++ {
		current, err = platform.readProcessIdentity(identity.leaderPID)
		if err == nil {
			break
		}
		leaderPresent, probeErr := platform.processPresent(identity.leaderPID)
		if probeErr != nil {
			return true, probeErr
		}
		if !leaderPresent {
			if allowLeaderExit {
				return true, nil
			}
			return true, fmt.Errorf("service process group %d remains after its leader exited before ownership was confirmed", identity.processGroup)
		}
		if attempt < 2 {
			time.Sleep(serviceProcessGroupPollInterval)
		}
	}
	if err != nil {
		return true, fmt.Errorf("verify service process group %d leader: %w", identity.processGroup, err)
	}
	if !serviceProcessIdentityMatches(current, identity.processGroup, identity.startID, identity.instanceToken, identity.commandDigest) {
		return true, fmt.Errorf("service process group %d leader identity changed", identity.processGroup)
	}
	return true, nil
}

func ownedResidualServiceProcessGroupPresent(identity serviceProcessGroupIdentity) (bool, error) {
	platform := identity.platform()
	if !platform.identityInspectionAvailable {
		return true, fmt.Errorf("service process group %d residual ownership identity inspection is unavailable", identity.processGroup)
	}
	if identity.instanceToken == "" || strings.ContainsRune(identity.instanceToken, '\x00') {
		return true, fmt.Errorf("service process group %d residual ownership token is unavailable", identity.processGroup)
	}
	members, err := platform.readProcessGroupMembers(identity.processGroup)
	if err != nil {
		return true, fmt.Errorf("inspect residual service process group %d: %w", identity.processGroup, err)
	}
	verified := 0
	for _, member := range members {
		if member.pid <= 0 || member.processGroup != identity.processGroup || strings.TrimSpace(member.command) == "" {
			return true, fmt.Errorf("service process group %d residual member identity is unavailable", identity.processGroup)
		}
		if member.pid == identity.leaderPID {
			if !serviceProcessIdentityMatches(member, identity.processGroup, identity.startID, identity.instanceToken, identity.commandDigest) {
				return true, fmt.Errorf("service process group %d leader identity changed", identity.processGroup)
			}
		} else {
			matches, matchErr := platform.processGroupMemberMatches(member, identity.instanceToken, identity.markerPath)
			if matchErr != nil {
				return true, fmt.Errorf("inspect service process group %d residual member %d: %w", identity.processGroup, member.pid, matchErr)
			}
			if !matches {
				return true, fmt.Errorf("service process group %d residual member %d ownership is unresolved", identity.processGroup, member.pid)
			}
		}
		verified++
	}
	if verified > 0 {
		return true, nil
	}
	present, err := platform.processGroupPresent(identity.processGroup)
	if err != nil || !present {
		return present, err
	}
	return true, fmt.Errorf("service process group %d remains without inspectable members", identity.processGroup)
}

func signalOwnedServiceProcessGroup(identity serviceProcessGroupIdentity, allowLeaderExit bool, signal syscall.Signal) (bool, error) {
	present, err := ownedServiceProcessGroupPresent(identity, allowLeaderExit)
	if errors.Is(err, syscall.EPERM) {
		// Darwin can briefly report EPERM while a dying group is being reaped.
		// Give that transition a bounded chance to resolve, but retain the error
		// when the group remains inaccessible rather than treating it as gone.
		gone, waitErr := waitForOwnedServiceProcessGroup(context.Background(), identity, allowLeaderExit, time.Now().Add(5*serviceProcessGroupPollInterval))
		if gone {
			return false, nil
		}
		if waitErr != nil {
			return true, waitErr
		}
		present, err = ownedServiceProcessGroupPresent(identity, allowLeaderExit)
	}
	if err != nil || !present {
		return present, err
	}
	if err := identity.platform().signalProcessGroup(identity.processGroup, signal); err != nil {
		return true, err
	}
	return true, nil
}

func waitForOwnedServiceProcessGroup(ctx context.Context, identity serviceProcessGroupIdentity, allowLeaderExit bool, deadline time.Time) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		present, err := ownedServiceProcessGroupPresent(identity, allowLeaderExit)
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return false, err
		}
		if err == nil && !present {
			return true, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, err
		}
		if remaining > serviceProcessGroupPollInterval {
			remaining = serviceProcessGroupPollInterval
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return false, ctx.Err()
		case <-timer.C:
		}
	}
}

// terminateOwnedServiceProcessGroup gives the complete owned group a graceful
// TERM window even when its leader exits first. Remaining descendants are
// killed as a group, and success is reported only after the group no longer
// exists. Context cancellation shortens the graceful window but does not skip
// the bounded SIGKILL verification needed to avoid silent orphans.
func terminateOwnedServiceProcessGroup(ctx context.Context, identity serviceProcessGroupIdentity, grace time.Duration) error {
	if identity.leaderPID <= 0 || identity.processGroup <= 0 || identity.leaderPID != identity.processGroup {
		return fmt.Errorf("invalid service process group identity: leader=%d group=%d", identity.leaderPID, identity.processGroup)
	}
	present, err := signalOwnedServiceProcessGroup(identity, false, syscall.SIGTERM)
	if err != nil || !present {
		return err
	}
	if grace < 0 {
		grace = 0
	}
	graceErr := error(nil)
	gone, err := waitForOwnedServiceProcessGroup(ctx, identity, true, time.Now().Add(grace))
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if gone {
		return nil
	}
	graceErr = err
	present, err = signalOwnedServiceProcessGroup(identity, true, syscall.SIGKILL)
	if err != nil {
		if graceErr != nil {
			return fmt.Errorf("force service process group %d after %v: %w", identity.processGroup, graceErr, err)
		}
		return err
	}
	if !present {
		return nil
	}
	gone, err = waitForOwnedServiceProcessGroup(context.Background(), identity, true, time.Now().Add(serviceProcessGroupKillWait))
	if err != nil {
		return err
	}
	if !gone {
		if graceErr != nil {
			return fmt.Errorf("service process group %d remains after SIGKILL following %v", identity.processGroup, graceErr)
		}
		return fmt.Errorf("service process group %d remains after SIGKILL", identity.processGroup)
	}
	return nil
}

var _ io.Writer = (*serviceLogSink)(nil)
