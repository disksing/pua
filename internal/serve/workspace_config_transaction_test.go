package serve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func newWorkspaceConfigTransactionFixture(t *testing.T, initialCount int) (*server, []serveWorkspace) {
	t.Helper()
	workspaces := make([]serveWorkspace, 0, initialCount)
	for range initialCount {
		root := t.TempDir()
		initialized, err := app.Initialize(root, "en")
		if err != nil {
			t.Fatal(err)
		}
		workspaces = append(workspaces, serveWorkspace{
			ID:   workspaceID(initialized.Root()),
			Name: workspaceName(initialized.Root()),
			Path: initialized.Root(),
		})
	}
	configPath := filepath.Join(t.TempDir(), "serve.json")
	serviceContext, cancelServices := context.WithCancel(context.Background())
	s := &server{
		config:         configPath,
		locks:          newWorkspaceLockManager("127.0.0.1:4936", configPath),
		serviceContext: serviceContext,
	}
	if err := s.saveConfig(config{
		Version:    agentHubConfigVersion,
		Workspaces: workspaces,
	}); err != nil {
		t.Fatal(err)
	}
	for _, workspace := range workspaces {
		if _, err := s.locks.acquire(workspace.Path); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.initializeServiceManagers(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopContext, cancelStop := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelStop()
		if err := s.stopServices(stopContext); err != nil {
			t.Errorf("stop transaction fixture services: %v", err)
		}
		cancelServices()
		s.locks.closeAll()
	})
	return s, workspaces
}

func newWorkspaceConfigTransactionAddition(t *testing.T, withService bool) serveWorkspace {
	t.Helper()
	root := t.TempDir()
	initialized, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	if withService {
		writeTestService(t, initialized.Root(), ServiceConfig{
			SchemaVersion: serviceSchemaVersion,
			ID:            "worker",
			Enabled:       true,
			Command:       []string{"/bin/sh", "-c", "exec sleep 30"},
		})
	}
	return serveWorkspace{
		ID:   workspaceID(initialized.Root()),
		Name: workspaceName(initialized.Root()),
		Path: initialized.Root(),
	}
}

func waitForWorkspaceConfigTransactionSignal(t *testing.T, signal <-chan string) string {
	t.Helper()
	select {
	case value := <-signal:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Workspace configuration transaction signal")
		return ""
	}
}

func assertWorkspaceConfigIDs(t *testing.T, s *server, want ...string) {
	t.Helper()
	cfg, err := s.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(cfg.Workspaces))
	for _, workspace := range cfg.Workspaces {
		got = append(got, workspace.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("configured Workspace IDs = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("configured Workspace IDs = %v, want %v", got, want)
		}
	}
}

func registeredWorkspaceManager(t *testing.T, s *server, workspace serveWorkspace) *ServiceManager {
	t.Helper()
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()
	_, manager, err := s.registeredServiceManagerLocked(workspace)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestRemoveWorkspacePreservesConcurrentAddition(t *testing.T) {
	s, initial := newWorkspaceConfigTransactionFixture(t, 2)
	removed, retained := initial[0], initial[1]
	added := newWorkspaceConfigTransactionAddition(t, true)

	stopEntered := make(chan string, 1)
	releaseStop := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseStop) }) })
	s.serviceStopper = func(manager *ServiceManager, _ context.Context) error {
		stopEntered <- manager.Root()
		<-releaseStop
		return nil
	}

	removeResult := make(chan error, 1)
	go func() { removeResult <- s.removeWorkspace(removed.ID) }()
	if root := waitForWorkspaceConfigTransactionSignal(t, stopEntered); root != removed.Path {
		t.Fatalf("blocked removal manager root = %q, want %q", root, removed.Path)
	}

	committed, err := s.addWorkspace(context.Background(), added.Path)
	if err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(releaseStop) })
	if err := <-removeResult; err != nil {
		t.Fatal(err)
	}

	assertWorkspaceConfigIDs(t, s, retained.ID, added.ID)
	if s.locks.owns(removed.Path) {
		t.Fatal("removed Workspace retained its ownership lock")
	}
	if !s.locks.owns(retained.Path) || !s.locks.owns(added.Path) {
		t.Fatal("surviving Workspace lost its ownership lock")
	}
	if manager := registeredWorkspaceManager(t, s, removed); manager != nil {
		t.Fatal("removed Workspace retained its service manager")
	}
	addedManager := registeredWorkspaceManager(t, s, committed)
	if addedManager == nil {
		t.Fatal("concurrent addition lost its service manager")
	}
	status, err := addedManager.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceStateReady || status.PID <= 0 || status.ProcessGroup <= 0 {
		t.Fatalf("concurrently added service status = %#v, want a ready owned process", status)
	}
}

func TestConcurrentWorkspaceRemovalsDoNotReintroduceEachOther(t *testing.T) {
	s, workspaces := newWorkspaceConfigTransactionFixture(t, 2)
	stopEntered := make(chan string, 2)
	releaseStop := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseStop) }) })
	s.serviceStopper = func(manager *ServiceManager, _ context.Context) error {
		stopEntered <- manager.Root()
		<-releaseStop
		return nil
	}

	results := make(chan error, len(workspaces))
	for _, workspace := range workspaces {
		workspace := workspace
		go func() { results <- s.removeWorkspace(workspace.ID) }()
	}
	entered := map[string]bool{}
	for range workspaces {
		entered[waitForWorkspaceConfigTransactionSignal(t, stopEntered)] = true
	}
	for _, workspace := range workspaces {
		if !entered[workspace.Path] {
			t.Fatalf("removal never claimed Workspace %q", workspace.Path)
		}
	}
	releaseOnce.Do(func() { close(releaseStop) })
	for range workspaces {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}

	assertWorkspaceConfigIDs(t, s)
	s.serviceMu.Lock()
	managerCount := len(s.services)
	s.serviceMu.Unlock()
	if managerCount != 0 {
		t.Fatalf("concurrent removals retained %d service managers", managerCount)
	}
	for _, workspace := range workspaces {
		if s.locks.owns(workspace.Path) {
			t.Fatalf("concurrent removal retained the lock for %q", workspace.Path)
		}
	}
}

func TestRemoveWorkspaceSaveFailurePreservesConcurrentAddition(t *testing.T) {
	s, initial := newWorkspaceConfigTransactionFixture(t, 2)
	removed, retained := initial[0], initial[1]
	added := newWorkspaceConfigTransactionAddition(t, false)
	removedManager := registeredWorkspaceManager(t, s, removed)

	stopEntered := make(chan string, 1)
	releaseStop := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseStop) }) })
	s.serviceStopper = func(manager *ServiceManager, _ context.Context) error {
		stopEntered <- manager.Root()
		<-releaseStop
		return nil
	}

	removeResult := make(chan error, 1)
	go func() { removeResult <- s.removeWorkspace(removed.ID) }()
	waitForWorkspaceConfigTransactionSignal(t, stopEntered)
	committed, err := s.addWorkspace(context.Background(), added.Path)
	if err != nil {
		t.Fatal(err)
	}

	configDir := filepath.Dir(s.config)
	if err := os.Chmod(configDir, 0o500); err != nil {
		t.Fatal(err)
	}
	restorePermissions := func() {
		if err := os.Chmod(configDir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(restorePermissions)
	releaseOnce.Do(func() { close(releaseStop) })
	removeErr := <-removeResult
	if removeErr == nil {
		t.Fatal("removeWorkspace succeeded despite the injected config save failure")
	}
	var pathErr *os.PathError
	if !errors.As(removeErr, &pathErr) {
		t.Fatalf("removeWorkspace error = %v, want config path failure", removeErr)
	}
	restorePermissions()

	assertWorkspaceConfigIDs(t, s, removed.ID, retained.ID, added.ID)
	manager, _, lookupErr := s.serviceManagerForWorkspace(removed.ID)
	if lookupErr != nil {
		t.Fatalf("failed removal left Workspace supervision fenced: %v", lookupErr)
	}
	if manager != removedManager {
		t.Fatal("failed removal replaced or lost its authoritative manager")
	}
	if manager := registeredWorkspaceManager(t, s, committed); manager == nil {
		t.Fatal("failed removal lost the concurrently added manager")
	}
	for _, workspace := range []serveWorkspace{removed, retained, committed} {
		if !s.locks.owns(workspace.Path) {
			t.Fatalf("failed removal lost the lock for %q", workspace.Path)
		}
	}
}
