package serve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceLogSinkReportsRotationRemoveFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "stdout.log")
	mustWriteServiceLogFixture(t, path, "live")
	mustWriteServiceLogFixture(t, path+".1", "one")
	mustWriteServiceLogFixture(t, path+".2", "two")

	sink := newServiceLogSink(path, ServiceLogRotationConfig{MaxBytes: 4, MaxFiles: 2})
	wantErr := errors.New("injected remove failure")
	sink.fileOps.remove = func(candidate string) error {
		if candidate == path+".2" {
			return wantErr
		}
		return os.Remove(candidate)
	}

	n, err := sink.Write([]byte("x"))
	if n != 0 || !errors.Is(err, wantErr) {
		t.Fatalf("Write() = (%d, %v), want (0, injected remove failure)", n, err)
	}
	assertServiceLogFixture(t, path, "live")
	assertServiceLogFixture(t, path+".1", "one")
	assertServiceLogFixture(t, path+".2", "two")
}

func TestServiceLogSinkReportsRotationRenameFailureAtOrOverLimit(t *testing.T) {
	for _, initial := range []string{"full", "over-limit"} {
		t.Run(initial, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stdout.log")
			mustWriteServiceLogFixture(t, path, initial)

			sink := newServiceLogSink(path, ServiceLogRotationConfig{MaxBytes: 4, MaxFiles: 2})
			wantErr := errors.New("injected rename failure")
			sink.fileOps.rename = func(old, next string) error {
				if old == path && next == path+".1" {
					return wantErr
				}
				return os.Rename(old, next)
			}

			n, err := sink.Write([]byte("x"))
			if n != 0 || !errors.Is(err, wantErr) {
				t.Fatalf("Write() = (%d, %v), want (0, injected rename failure)", n, err)
			}
			assertServiceLogFixture(t, path, initial)
			if sink.size != int64(len(initial)) {
				t.Fatalf("sink size = %d, want %d", sink.size, len(initial))
			}
		})
	}
}

func TestServiceLogSinkKeepsSizeAndBackupOnOpenFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout.log")
	mustWriteServiceLogFixture(t, path, "live")

	sink := newServiceLogSink(path, ServiceLogRotationConfig{MaxBytes: 4, MaxFiles: 2})
	sink.mu.Lock()
	openErr := sink.ensureFileLocked()
	sink.mu.Unlock()
	if openErr != nil {
		t.Fatalf("open initial service log: %v", openErr)
	}
	wantErr := errors.New("injected open failure")
	openFile := sink.fileOps.openFile
	failed := false
	sink.fileOps.openFile = func(name string, flag int, mode os.FileMode) (*os.File, error) {
		if name == path && !failed {
			failed = true
			return nil, wantErr
		}
		return openFile(name, flag, mode)
	}

	n, err := sink.Write([]byte("x"))
	if n != 0 || !errors.Is(err, wantErr) {
		t.Fatalf("first Write() = (%d, %v), want (0, injected open failure)", n, err)
	}
	if sink.size != 4 {
		t.Fatalf("sink size after failed reopen = %d, want 4", sink.size)
	}
	assertServiceLogFixture(t, path+".1", "live")

	n, err = sink.Write([]byte("x"))
	if n != 1 || err != nil {
		t.Fatalf("second Write() = (%d, %v), want (1, nil)", n, err)
	}
	assertServiceLogFixture(t, path, "x")
}

func TestServiceLogSinkReturnsPartialWriteOnRotationFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout.log")
	sink := newServiceLogSink(path, ServiceLogRotationConfig{MaxBytes: 4, MaxFiles: 2})
	wantErr := errors.New("injected rename failure")
	sink.fileOps.rename = func(old, next string) error {
		if old == path && next == path+".1" {
			return wantErr
		}
		return os.Rename(old, next)
	}

	n, err := sink.Write([]byte("abcdef"))
	if n != 4 || !errors.Is(err, wantErr) {
		t.Fatalf("Write() = (%d, %v), want (4, injected rename failure)", n, err)
	}
	assertServiceLogFixture(t, path, "abcd")
}

func TestServiceLogSinkRejectsRecreatedFullLogAfterRotation(t *testing.T) {
	for _, replacement := range []string{"full", "over-limit"} {
		t.Run(replacement, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stdout.log")
			mustWriteServiceLogFixture(t, path, "live")

			sink := newServiceLogSink(path, ServiceLogRotationConfig{MaxBytes: 4, MaxFiles: 2})
			rename := sink.fileOps.rename
			sink.fileOps.rename = func(old, next string) error {
				if err := rename(old, next); err != nil {
					return err
				}
				if old == path {
					return os.WriteFile(path, []byte(replacement), 0o600)
				}
				return nil
			}

			n, err := sink.Write([]byte("x"))
			if n != 0 || err == nil || !strings.Contains(err.Error(), "remains at size") {
				t.Fatalf("Write() = (%d, %v), want (0, size-after-rotation error)", n, err)
			}
			assertServiceLogFixture(t, path, replacement)
			assertServiceLogFixture(t, path+".1", "live")
		})
	}
}

func TestServiceLogSinkKeepsBoundedBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout.log")
	sink := newServiceLogSink(path, ServiceLogRotationConfig{MaxBytes: 4, MaxFiles: 2})

	n, err := sink.Write([]byte("abcdefghij"))
	if n != 10 || err != nil {
		t.Fatalf("Write() = (%d, %v), want (10, nil)", n, err)
	}
	assertServiceLogFixture(t, path, "ij")
	assertServiceLogFixture(t, path+".1", "efgh")
	assertServiceLogFixture(t, path+".2", "abcd")
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("unexpected excess service log backup: %v", err)
	}
}

func mustWriteServiceLogFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", filepath.Base(path), err)
	}
}

func assertServiceLogFixture(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", filepath.Base(path), data, want)
	}
}
