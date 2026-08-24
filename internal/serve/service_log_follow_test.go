package serve

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func TestWorkspaceServiceFollowLogsStreamsBeforeRequestEnds(t *testing.T) {
	root := t.TempDir()
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Command:       []string{"true"},
	})
	logPath := filepath.Join(serviceRuntimePath(root, "worker"), "stdout.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	workspace := serveWorkspace{ID: "workspace-one", Path: root}
	server := newServiceLifecycleTestServer(t, workspace)
	requestStarted := make(chan struct{})
	handlerDone := make(chan struct{})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		defer close(handlerDone)
		server.handleWorkspace(w, r)
	}))
	t.Cleanup(httpServer.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		httpServer.URL+"/api/workspaces/workspace-one/services/worker/logs?follow=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	type responseResult struct {
		response *http.Response
		err      error
	}
	responseDone := make(chan responseResult, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(request)
		responseDone <- responseResult{response: response, err: requestErr}
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("follow request did not reach the server")
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logFile.WriteString("small line\n"); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}

	var response *http.Response
	select {
	case result := <-responseDone:
		if result.err != nil {
			t.Fatalf("start followed log request: %v", result.err)
		}
		response = result.response
	case <-time.After(time.Second):
		cancel()
		<-handlerDone
		t.Fatal("small followed log chunk was not flushed before the request ended")
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusOK {
		t.Fatalf("followed log status = %d", response.StatusCode)
	}

	type lineResult struct {
		line string
		err  error
	}
	lineDone := make(chan lineResult, 1)
	go func() {
		line, readErr := bufio.NewReader(response.Body).ReadString('\n')
		lineDone <- lineResult{line: line, err: readErr}
	}()
	select {
	case result := <-lineDone:
		if result.err != nil || result.line != "small line\n" {
			t.Fatalf("followed log chunk = (%q, %v), want (%q, nil)", result.line, result.err, "small line\n")
		}
	case <-time.After(time.Second):
		cancel()
		t.Fatal("timed out reading flushed followed log chunk")
	}

	cancel()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("follow handler did not stop after request cancellation")
	}
	server.serviceMu.Lock()
	leaseCount := server.serviceLeases[workspace.ID]
	server.serviceMu.Unlock()
	if leaseCount != 0 {
		t.Fatalf("canceled follow request retained %d service operation references", leaseCount)
	}
}

func TestWorkspaceRemovalRevokesFollowedLogsBeforeNewGeneration(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Command:       []string{"true"},
	})
	logPath := filepath.Join(serviceRuntimePath(root, "worker"), "stdout.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("old generation\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	workspace := serveWorkspace{ID: workspaceID(root), Path: root}
	server := newServiceLifecycleTestServer(t, workspace)
	httpServer := httptest.NewServer(http.HandlerFunc(server.handleWorkspace))
	t.Cleanup(httpServer.Close)
	response := openFollowedServiceLog(t, httpServer.URL, workspace.ID, "worker")
	t.Cleanup(func() { _ = response.Body.Close() })
	readFollowedHTTPChunk(t, response.Body, "old generation\n")

	stopEntered := make(chan struct{})
	releaseStop := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseStop) }) }
	t.Cleanup(release)
	server.serviceStopper = func(manager *ServiceManager, ctx context.Context) error {
		close(stopEntered)
		<-releaseStop
		return manager.Stop(ctx)
	}
	removeResult := make(chan error, 1)
	go func() { removeResult <- server.removeWorkspace(workspace.ID) }()
	select {
	case <-stopEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Workspace removal did not reach the manager stop boundary")
	}
	assertFollowedHTTPLogEOF(t, response.Body,
		"old followed log remained active at the ownership release boundary")
	server.serviceMu.Lock()
	leaseCount := server.serviceLeases[workspace.ID]
	server.serviceMu.Unlock()
	if leaseCount != 0 {
		t.Fatalf("Workspace removal reached manager stop with %d followed-log leases", leaseCount)
	}

	release()
	select {
	case err := <-removeResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Workspace removal did not finish")
	}
	if _, err := server.addWorkspace(context.Background(), root); err != nil {
		t.Fatalf("re-add Workspace: %v", err)
	}
	rotatedPath := logPath + ".previous"
	if err := os.Rename(logPath, rotatedPath); err != nil {
		t.Fatalf("rotate old-generation log: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("new generation\n"), 0o600); err != nil {
		t.Fatalf("write new-generation log: %v", err)
	}
	assertFollowedHTTPLogEOF(t, response.Body,
		"old followed log read bytes from the replacement manager generation")
}

func TestWorkspaceRemovalRevokesOnlyItsFollowedLogs(t *testing.T) {
	workspaces := make([]serveWorkspace, 0, 2)
	for _, id := range []string{"workspace-one", "workspace-two"} {
		root := t.TempDir()
		writeTestService(t, root, ServiceConfig{
			SchemaVersion: serviceSchemaVersion,
			ID:            "worker",
			Command:       []string{"true"},
		})
		logPath := filepath.Join(serviceRuntimePath(root, "worker"), "stdout.log")
		if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(logPath, []byte(id+" initial\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		workspaces = append(workspaces, serveWorkspace{ID: id, Path: root})
	}
	server := newServiceLifecycleTestServer(t, workspaces...)
	httpServer := httptest.NewServer(http.HandlerFunc(server.handleWorkspace))
	t.Cleanup(httpServer.Close)
	first := openFollowedServiceLog(t, httpServer.URL, workspaces[0].ID, "worker")
	t.Cleanup(func() { _ = first.Body.Close() })
	second := openFollowedServiceLog(t, httpServer.URL, workspaces[1].ID, "worker")
	t.Cleanup(func() { _ = second.Body.Close() })
	readFollowedHTTPChunk(t, first.Body, "workspace-one initial\n")
	readFollowedHTTPChunk(t, second.Body, "workspace-two initial\n")

	if err := server.removeWorkspace(workspaces[0].ID); err != nil {
		t.Fatal(err)
	}
	assertFollowedHTTPLogEOF(t, first.Body, "removed Workspace follower remained active")
	secondLog := filepath.Join(serviceRuntimePath(workspaces[1].Path, "worker"), "stdout.log")
	file, err := os.OpenFile(secondLog, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("still owned\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	readFollowedHTTPChunk(t, second.Body, "still owned\n")
	server.serviceMu.Lock()
	firstLeases := server.serviceLeases[workspaces[0].ID]
	secondLeases := server.serviceLeases[workspaces[1].ID]
	server.serviceMu.Unlock()
	if firstLeases != 0 || secondLeases != 1 {
		t.Fatalf("followed-log leases after isolated removal = (%d, %d), want (0, 1)", firstLeases, secondLeases)
	}
}

func TestServiceShutdownRevokesFollowedLogs(t *testing.T) {
	root := t.TempDir()
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Command:       []string{"true"},
	})
	logPath := filepath.Join(serviceRuntimePath(root, "worker"), "stdout.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("before shutdown\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := serveWorkspace{ID: "workspace-one", Path: root}
	server := newServiceLifecycleTestServer(t, workspace)
	httpServer := httptest.NewServer(http.HandlerFunc(server.handleWorkspace))
	t.Cleanup(httpServer.Close)
	response := openFollowedServiceLog(t, httpServer.URL, workspace.ID, "worker")
	t.Cleanup(func() { _ = response.Body.Close() })
	readFollowedHTTPChunk(t, response.Body, "before shutdown\n")

	shutdown := make(chan error, 1)
	go func() { shutdown <- server.stopServices(context.Background()) }()
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("service shutdown did not revoke the followed log")
	}
	assertFollowedHTTPLogEOF(t, response.Body, "followed log remained active after service shutdown")
	server.serviceMu.Lock()
	leaseCount := server.serviceLeases[workspace.ID]
	server.serviceMu.Unlock()
	if leaseCount != 0 {
		t.Fatalf("service shutdown retained %d followed-log leases", leaseCount)
	}
}

func TestWorkspaceServiceFollowLogsRejectsWriterWithoutFlusher(t *testing.T) {
	root := t.TempDir()
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Command:       []string{"true"},
	})
	logPath := filepath.Join(serviceRuntimePath(root, "worker"), "stdout.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("small line\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	workspace := serveWorkspace{ID: "workspace-one", Path: root}
	server := newServiceLifecycleTestServer(t, workspace)
	recorder := httptest.NewRecorder()
	writer := struct{ http.ResponseWriter }{ResponseWriter: recorder}
	request := httptest.NewRequest(http.MethodGet,
		"/api/workspaces/workspace-one/services/worker/logs?follow=true", nil)
	server.handleWorkspace(writer, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("followed log status without flusher = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if !strings.Contains(recorder.Body.String(), "streaming is not supported") {
		t.Fatalf("followed log error without flusher = %q", recorder.Body.String())
	}
}

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
	if err := os.Rename(path, path+".canceled"); err != nil {
		t.Fatalf("rotate log after cancellation: %v", err)
	}
	if err := os.WriteFile(path, []byte("must not reopen\n"), 0o600); err != nil {
		t.Fatalf("replace log after cancellation: %v", err)
	}
	if n, err := reader.Read(make([]byte, len("must not reopen\n"))); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read replacement after cancellation = (%d, %v), want (0, EOF)", n, err)
	}
	activeFile := follower.file
	if err := reader.Close(); err != nil {
		t.Fatalf("close followed log: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close followed log twice: %v", err)
	}
	assertFollowLogFileClosed(t, activeFile)
}

func TestServiceManagerFollowLogsCloseInterruptsConcurrentRead(t *testing.T) {
	manager, _ := newFollowLogTestManager(t, "")
	reader, err := manager.LogsContext(context.Background(), "worker", "stdout", true)
	if err != nil {
		t.Fatal(err)
	}
	reader.(*followLogReader).pollInterval = time.Second
	readDone := make(chan error, 1)
	go func() {
		_, readErr := reader.Read(make([]byte, 1))
		readDone <- readErr
	}()
	if err := reader.Close(); err != nil {
		t.Fatalf("close followed log: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close followed log twice: %v", err)
	}
	select {
	case err := <-readDone:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("concurrent read after close error = %v, want EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not interrupt concurrent followed-log read")
	}
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

func openFollowedServiceLog(t *testing.T, baseURL, workspaceID, serviceID string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet,
		baseURL+"/api/workspaces/"+workspaceID+"/services/"+serviceID+"/logs?follow=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open followed log: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		t.Fatalf("followed log status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	return response
}

func readFollowedHTTPChunk(t *testing.T, reader io.Reader, want string) {
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
			t.Fatalf("followed HTTP log chunk = (%q, %v), want (%q, nil)", got.data, got.err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for followed HTTP log chunk %q", want)
	}
}

func assertFollowedHTTPLogEOF(t *testing.T, reader io.Reader, message string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := reader.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("%s: read error = %v, want EOF", message, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal(message)
	}
}
