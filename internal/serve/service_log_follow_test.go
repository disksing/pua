package serve

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServiceManagerFollowLogsAcrossRotationAndTruncation(t *testing.T) {
	manager, path := newFollowLogTestManager(t, "first\n")
	ctx, cancel := context.WithCancel(context.Background())
	reader, err := manager.LogsContext(ctx, "worker", "stdout", true)
	if err != nil {
		t.Fatalf("open followed log: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	follower := reader.(*followLogReader)
	follower.pollInterval = 5 * time.Millisecond

	readFollowLogChunk(t, reader, "first\n")
	sink := newServiceLogSink(path, ServiceLogRotationConfig{MaxBytes: 6, MaxFiles: 3})
	t.Cleanup(func() { _ = sink.Close() })

	firstFile := follower.file
	if n, err := sink.Write([]byte("other\n")); n != 6 || err != nil {
		t.Fatalf("first rotating write = (%d, %v), want (6, nil)", n, err)
	}
	readFollowLogChunk(t, reader, "other\n")
	assertFollowLogFileClosed(t, firstFile)

	secondFile := follower.file
	if n, err := sink.Write([]byte("third\n")); n != 6 || err != nil {
		t.Fatalf("second rotating write = (%d, %v), want (6, nil)", n, err)
	}
	readFollowLogChunk(t, reader, "third\n")
	assertFollowLogFileClosed(t, secondFile)

	if err := sink.Close(); err != nil {
		t.Fatalf("close rotating sink: %v", err)
	}
	if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
		t.Fatalf("truncate active log: %v", err)
	}
	readFollowLogChunk(t, reader, "x\n")

	readDone := make(chan struct {
		n   int
		err error
	}, 1)
	go func() {
		n, readErr := reader.Read(make([]byte, 1))
		readDone <- struct {
			n   int
			err error
		}{n: n, err: readErr}
	}()
	cancel()
	select {
	case result := <-readDone:
		if result.n != 0 || !errors.Is(result.err, io.EOF) {
			t.Fatalf("read after cancellation = (%d, %v), want (0, EOF)", result.n, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("followed log did not stop after cancellation")
	}
	activeFile := follower.file
	if err := reader.Close(); err != nil {
		t.Fatalf("close followed log: %v", err)
	}
	assertFollowLogFileClosed(t, activeFile)
}

func TestServiceManagerFollowLogsRejectsSymlinkReplacement(t *testing.T) {
	manager, path := newFollowLogTestManager(t, "safe\n")
	reader, err := manager.LogsContext(context.Background(), "worker", "stdout", true)
	if err != nil {
		t.Fatalf("open followed log: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	reader.(*followLogReader).pollInterval = 5 * time.Millisecond
	readFollowLogChunk(t, reader, "safe\n")

	outside := filepath.Join(t.TempDir(), "outside.log")
	if err := os.WriteFile(outside, []byte("do-not-read\n"), 0o600); err != nil {
		t.Fatalf("write outside log: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove active log: %v", err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("create log symlink: %v", err)
	}

	buffer := make([]byte, len("do-not-read\n"))
	n, err := reader.Read(buffer)
	if n != 0 || err == nil || !strings.Contains(err.Error(), "unsafe service log") {
		t.Fatalf("read symlink replacement = (%d, %v), want unsafe-log error", n, err)
	}
}

func newFollowLogTestManager(t *testing.T, contents string) (*ServiceManager, string) {
	t.Helper()
	root := t.TempDir()
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Command:       []string{"true"},
	})
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatalf("create service manager: %v", err)
	}
	path := filepath.Join(serviceRuntimePath(root, "worker"), "stdout.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create service runtime: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write service log: %v", err)
	}
	return manager, path
}

func readFollowLogChunk(t *testing.T, reader io.Reader, want string) {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data := make([]byte, len(want))
		_, err := io.ReadFull(reader, data)
		done <- result{data: data, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil || string(got.data) != want {
			t.Fatalf("followed log chunk = (%q, %v), want (%q, nil)", got.data, got.err, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for followed log chunk %q", want)
	}
}

func assertFollowLogFileClosed(t *testing.T, file *os.File) {
	t.Helper()
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("rotated log descriptor remains open: %v", err)
	}
}
