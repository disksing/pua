package serve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const (
	workspaceRemovalTestPID    = 83
	workspaceRemovalTestStart  = "workspace-removal-start"
	workspaceRemovalTestToken  = "workspace-removal-token"
	workspaceRemovalTestDigest = "workspace-removal-digest"
)

func newServiceLifecycleTestServer(t *testing.T, workspaces ...serveWorkspace) *server {
	t.Helper()
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{
		Version:    agentHubConfigVersion,
		Workspaces: workspaces,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.initializeServiceManagers(); err != nil {
		t.Fatal(err)
	}
	return s
}

func serviceManagerIsStopping(manager *ServiceManager) bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.stopping
}

func newWorkspaceRemovalTestServer(t *testing.T) (*server, serveWorkspace, *ServiceManager) {
	t.Helper()
	root := t.TempDir()
	service := ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command:       []string{"workspace-removal-worker"},
	}
	writeTestService(t, root, service)
	status := initialServiceStatus(service)
	status.State = ServiceStateReady
	status.PID = workspaceRemovalTestPID
	status.ProcessGroup = workspaceRemovalTestPID
	status.ProcessStartID = workspaceRemovalTestStart
	status.InstanceToken = workspaceRemovalTestToken
	status.CommandDigest = workspaceRemovalTestDigest
	if err := writeServiceJSON(filepath.Join(serviceRuntimePath(root, service.ID), "state.json"), status, 0o600); err != nil {
		t.Fatal(err)
	}

	workspace := serveWorkspace{ID: "workspace-one", Path: root}
	configPath := filepath.Join(t.TempDir(), "serve.json")
	s := &server{config: configPath, locks: newWorkspaceLockManager("127.0.0.1:4936", configPath)}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{workspace}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.locks.acquire(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.locks.closeAll)
	if err := s.initializeServiceManagers(); err != nil {
		t.Fatal(err)
	}
	manager, _, err := s.serviceManagerForWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, workspace, manager
}

func workspaceRemovalTestPlatform(present *atomic.Bool, signal func(int, syscall.Signal) error) *serviceProcessPlatform {
	return &serviceProcessPlatform{
		identityInspectionAvailable: true,
		processGroupPresent:         func(int) (bool, error) { return present.Load(), nil },
		processPresent:              func(int) (bool, error) { return present.Load(), nil },
		readProcessIdentity: func(int) (serviceProcessIdentity, error) {
			return serviceProcessIdentity{
				pid:     workspaceRemovalTestPID,
				command: "workspace-removal-worker",
				environment: []string{
					serviceInstanceTokenEnvironment + "=" + workspaceRemovalTestToken,
					serviceCommandDigestEnvironment + "=" + workspaceRemovalTestDigest,
				},
				processGroup: workspaceRemovalTestPID,
				startID:      workspaceRemovalTestStart,
			}, nil
		},
		readProcessGroupMembers: func(int) ([]serviceProcessIdentity, error) {
			return nil, errProcessIdentityUnavailable
		},
		processGroupMemberMatches: func(serviceProcessIdentity, string, string) (bool, error) {
			return false, errProcessIdentityUnavailable
		},
		signalProcessGroup: signal,
	}
}

func TestServiceRegistryCanonicalizesWorkspacePathAliases(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := serveWorkspace{ID: "workspace-one", Path: root}
	s := newServiceLifecycleTestServer(t, workspace)
	manager, _, err := s.serviceManagerForWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := s.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspaces[0].Path = " \t" + filepath.Join(root, "nested", "..") + string(os.PathSeparator) + " \n"
	if err := s.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	aliasManager, _, err := s.serviceManagerForWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if aliasManager != manager {
		t.Fatal("dot segments, a trailing separator, and surrounding whitespace created a second manager")
	}
	if got := len(s.services); got != 1 {
		t.Fatalf("service manager count after syntactic alias = %d, want 1", got)
	}

	link := filepath.Join(base, "workspace-link")
	if err := os.Symlink(root, link); err != nil {
		t.Logf("symlink alias unavailable: %v", err)
		return
	}
	cfg.Workspaces[0].Path = link
	if err := s.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	linkManager, _, err := s.serviceManagerForWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if linkManager != manager {
		t.Fatal("symlink alias created a second manager")
	}
	if got := len(s.services); got != 1 {
		t.Fatalf("service manager count after symlink alias = %d, want 1", got)
	}
}

func TestRemoveWorkspaceFindsRegisteredManagerAfterCanonicalizationError(t *testing.T) {
	root := t.TempDir()
	workspace := serveWorkspace{ID: "workspace-one", Path: root}
	s := newServiceLifecycleTestServer(t, workspace)
	manager, _, err := s.serviceManagerForWorkspace(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}

	resolutionErr := errors.New("canonicalization failed")
	s.serviceMu.Lock()
	removed, err := s.removeServiceManagerForResolutionLocked(
		workspace,
		serviceWorkspaceKey{},
		resolutionErr,
	)
	s.serviceMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if removed != manager {
		t.Fatal("canonicalization failure did not resolve the registered manager by Workspace identity")
	}
	if err := removed.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !serviceManagerIsStopping(manager) {
		t.Fatal("canonicalization failure orphaned the registered manager")
	}
	if got := len(s.services); got != 0 {
		t.Fatalf("service manager count after removal = %d, want 0", got)
	}
	s.serviceMu.Lock()
	removed, err = s.removeServiceManagerForResolutionLocked(workspace, serviceWorkspaceKey{}, resolutionErr)
	s.serviceMu.Unlock()
	if removed != nil || !errors.Is(err, resolutionErr) {
		t.Fatalf("missing manager resolution = (%v, %v), want (nil, canonicalization error)", removed, err)
	}
}

func TestRemoveWorkspaceStopsOnlyItsRegisteredManager(t *testing.T) {
	first := serveWorkspace{ID: "workspace-one", Path: t.TempDir()}
	second := serveWorkspace{ID: "workspace-two", Path: t.TempDir()}
	s := newServiceLifecycleTestServer(t, first, second)
	firstManager, _, err := s.serviceManagerForWorkspace(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondManager, _, err := s.serviceManagerForWorkspace(second.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.removeWorkspace(first.ID); err != nil {
		t.Fatal(err)
	}
	if !serviceManagerIsStopping(firstManager) {
		t.Fatal("removed Workspace manager was not stopped")
	}
	if serviceManagerIsStopping(secondManager) {
		t.Fatal("removing one Workspace stopped a different manager")
	}
	if got := len(s.services); got != 1 {
		t.Fatalf("service manager count after removal = %d, want 1", got)
	}
	remaining, _, err := s.serviceManagerForWorkspace(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != secondManager {
		t.Fatal("remaining Workspace lost its registered manager")
	}
}

func TestRemoveWorkspaceRetainsManagerAndLockWhenServiceStopFails(t *testing.T) {
	s, workspace, manager := newWorkspaceRemovalTestServer(t)
	var present atomic.Bool
	present.Store(true)
	manager.processPlatform = workspaceRemovalTestPlatform(&present, func(int, syscall.Signal) error {
		return syscall.EPERM
	})

	err := s.removeWorkspace(workspace.ID)
	if err == nil {
		t.Fatal("removeWorkspace succeeded while a verified process group remained")
	}
	if message := err.Error(); !strings.Contains(message, "attention-required") || !strings.Contains(message, "retry") || strings.Contains(message, fmt.Sprint(workspaceRemovalTestPID)) || strings.Contains(message, syscall.EPERM.Error()) {
		t.Fatalf("removeWorkspace returned an unsafe or unactionable error: %q", message)
	}
	cfg, loadErr := s.loadConfig()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(cfg.Workspaces) != 1 || cfg.Workspaces[0].ID != workspace.ID {
		t.Fatalf("failed removal discarded Workspace config: %#v", cfg.Workspaces)
	}
	retained, _, lookupErr := s.serviceManagerForWorkspace(workspace.ID)
	if lookupErr != nil {
		t.Fatalf("failed removal lost manager lookup: %v", lookupErr)
	}
	if retained != manager {
		t.Fatal("failed removal replaced the authoritative service manager")
	}
	status, statusErr := retained.Show("worker")
	if statusErr != nil {
		t.Fatal(statusErr)
	}
	if status.State != ServiceStateAttentionRequired || !status.AttentionRequired || status.ProcessGroup != workspaceRemovalTestPID {
		t.Fatalf("failed removal did not retain durable attention state: %#v", status)
	}
	if !s.locks.owns(workspace.Path) {
		t.Fatal("failed removal released Workspace ownership")
	}
	probe := newWorkspaceLockManager("127.0.0.1:4999", filepath.Join(t.TempDir(), "serve.json"))
	t.Cleanup(probe.closeAll)
	if _, acquireErr := probe.acquire(workspace.Path); acquireErr == nil {
		t.Fatal("a second server acquired the Workspace after failed service shutdown")
	}

	present.Store(false)
	if retryErr := s.removeWorkspace(workspace.ID); retryErr != nil {
		t.Fatalf("removeWorkspace retry after process cleanup failed: %v", retryErr)
	}
	if s.locks.owns(workspace.Path) {
		t.Fatal("successful retry retained Workspace ownership")
	}
	if _, acquireErr := probe.acquire(workspace.Path); acquireErr != nil {
		t.Fatalf("second server could not acquire Workspace after successful retry: %v", acquireErr)
	}
	s.serviceMu.Lock()
	managerCount := len(s.services)
	s.serviceMu.Unlock()
	if managerCount != 0 {
		t.Fatalf("successful retry retained %d service managers", managerCount)
	}
}

func TestConcurrentWorkspaceRemovalStopsAndReleasesOnce(t *testing.T) {
	s, workspace, manager := newWorkspaceRemovalTestServer(t)
	var present atomic.Bool
	present.Store(true)
	enteredStop := make(chan struct{})
	releaseStop := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseStop) }) }
	t.Cleanup(release)
	var signals atomic.Int32
	manager.processPlatform = workspaceRemovalTestPlatform(&present, func(int, syscall.Signal) error {
		signals.Add(1)
		close(enteredStop)
		<-releaseStop
		present.Store(false)
		return nil
	})

	firstResult := make(chan error, 1)
	go func() { firstResult <- s.removeWorkspace(workspace.ID) }()
	select {
	case <-enteredStop:
	case <-time.After(time.Second):
		t.Fatal("first removal did not reach service shutdown")
	}
	s.serviceMu.Lock()
	_, registered, registeredErr := s.registeredServiceManagerLocked(workspace)
	managerCount := len(s.services)
	s.serviceMu.Unlock()
	if registeredErr != nil || registered != manager || managerCount != 1 {
		t.Fatalf("in-flight removal registration = (%p, %d, %v), want (%p, 1, nil)", registered, managerCount, registeredErr, manager)
	}
	if err := s.reconcileServices(context.Background()); err != nil {
		t.Fatal(err)
	}
	secondResult := make(chan error, 1)
	go func() { secondResult <- s.removeWorkspace(workspace.ID) }()
	select {
	case err := <-secondResult:
		t.Fatalf("concurrent removal did not join the in-flight shutdown: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	release()
	for name, result := range map[string]<-chan error{"first": firstResult, "second": secondResult} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("%s concurrent removal failed: %v", name, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s concurrent removal did not complete", name)
		}
	}
	if got := signals.Load(); got != 1 {
		t.Fatalf("concurrent removal signaled the service group %d times, want 1", got)
	}
	s.serviceMu.Lock()
	managerCount = len(s.services)
	s.serviceMu.Unlock()
	if managerCount != 0 {
		t.Fatalf("concurrent removal retained %d service managers", managerCount)
	}
	probe := newWorkspaceLockManager("", "")
	t.Cleanup(probe.closeAll)
	if _, err := probe.acquire(workspace.Path); err != nil {
		t.Fatalf("concurrent removal did not release Workspace ownership: %v", err)
	}
}
