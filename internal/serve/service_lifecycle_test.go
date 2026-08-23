package serve

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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
