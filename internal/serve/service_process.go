package serve

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/disksing/pua/internal/security"
)

const serviceStartupLogBufferBytes = 1 << 20

type serviceLogSink struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	maxFiles int
	file     *os.File
	size     int64
}

// serviceLogWriter keeps startup output in a bounded private buffer until the
// initial export handshake has registered every secret exported by the
// service. This prevents a service that prints an exported secret before its
// first export from leaking that value into the durable log.
type serviceLogWriter struct {
	mu       sync.Mutex
	sink     *serviceLogSink
	redactor *security.Redactor
	gated    bool
	buffer   []byte
}

func newServiceLogWriter(sink *serviceLogSink, redactor *security.Redactor, gated bool) *serviceLogWriter {
	return &serviceLogWriter{sink: sink, redactor: redactor, gated: gated}
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
	return w.writeUnlocked(data)
}

func (w *serviceLogWriter) writeUnlocked(data []byte) (int, error) {
	if w.sink == nil || len(data) == 0 {
		return len(data), nil
	}
	if w.redactor != nil {
		data = w.redactor.Redact(data)
	}
	return w.sink.Write(data)
}

// Release publishes the bounded startup buffer after the caller has loaded and
// registered the service's initial export secrets.
func (w *serviceLogWriter) Release(redactor *security.Redactor) error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.gated {
		return nil
	}
	w.gated = false
	if redactor != nil {
		w.redactor = redactor
	}
	buffer := w.buffer
	w.buffer = nil
	if len(buffer) == 0 {
		return nil
	}
	_, err := w.writeUnlocked(buffer)
	return err
}

func (w *serviceLogWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	// If the startup handshake never succeeded, discard all gated output rather
	// than flushing bytes whose secrets were not discoverable from an export.
	w.buffer = nil
	w.gated = false
	w.mu.Unlock()
	if w.sink != nil {
		return w.sink.Close()
	}
	return nil
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
