package serve

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/disksing/pua/internal/security"
)

const serviceStartupLogBufferBytes = 1 << 20

const serviceCommandWaitDelay = 250 * time.Millisecond

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

func reapOrphanProcessGroup(pgid int) {
	if pgid <= 0 {
		return
	}
	_ = terminateProcessGroup(pgid, false)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = terminateProcessGroup(pgid, true)
}

func terminateProcessGroup(pgid int, force bool) error {
	if pgid <= 0 {
		return nil
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	err := syscall.Kill(-pgid, signal)
	if err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

var _ io.Writer = (*serviceLogSink)(nil)
