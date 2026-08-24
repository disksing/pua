package serve

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitForServiceRemovalFence(t *testing.T, s *server, workspaceID string) *serviceManagerRemoval {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.serviceMu.Lock()
		removal := s.serviceRemovals[workspaceID]
		s.serviceMu.Unlock()
		if removal != nil {
			return removal
		}
		if time.Now().After(deadline) {
			t.Fatal("Workspace removal did not establish its service operation fence")
		}
		time.Sleep(time.Millisecond)
	}
}

func assertRemovalStillWaiting(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		t.Fatalf("Workspace removal completed before its admitted service operation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWorkspaceRemovalWaitsForAdmittedServiceAction(t *testing.T) {
	for _, action := range []struct {
		name string
		run  func(context.Context, *ServiceManager, string) error
	}{
		{name: "start", run: func(ctx context.Context, manager *ServiceManager, id string) error {
			return manager.StartService(ctx, id)
		}},
		{name: "restart", run: func(ctx context.Context, manager *ServiceManager, id string) error {
			return manager.RestartService(ctx, id)
		}},
	} {
		t.Run(action.name, func(t *testing.T) {
			root := t.TempDir()
			launches := filepath.Join(root, "launches")
			writeTestService(t, root, ServiceConfig{
				SchemaVersion: serviceSchemaVersion,
				ID:            "worker",
				Enabled:       true,
				Command: []string{"/bin/sh", "-c",
					"printf 'launch\\n' >> " + shellQuote(launches) + "; exec sleep 30"},
			})
			workspace := serveWorkspace{ID: "workspace-one", Path: root}
			s := newServiceLifecycleTestServer(t, workspace)
			lease, err := s.acquireServiceManagerLease(workspace.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(lease.Release)

			removeResult := make(chan error, 1)
			go func() { removeResult <- s.removeWorkspace(workspace.ID) }()
			waitForServiceRemovalFence(t, s, workspace.ID)
			assertRemovalStillWaiting(t, removeResult)

			if err := action.run(context.Background(), lease.manager, "worker"); err != nil {
				t.Fatal(err)
			}
			waitForLaunches(t, launches, 1)
			lease.Release()
			select {
			case err := <-removeResult:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Workspace removal did not continue after the service action released its lease")
			}
			if err := action.run(context.Background(), lease.manager, "worker"); !errors.Is(err, errServiceManagerStopping) {
				t.Fatalf("detached manager action error = %v, want service manager stopping", err)
			}
			if got := len(waitForLaunches(t, launches, 1)); got != 1 {
				t.Fatalf("detached manager launched %d service generations, want 1", got)
			}
		})
	}
}

func TestWorkspaceRemovalFenceRejectsNewServiceOperation(t *testing.T) {
	workspace := serveWorkspace{ID: "workspace-one", Path: t.TempDir()}
	s := newServiceLifecycleTestServer(t, workspace)
	removal, owner, err := s.beginWorkspaceServiceManagerRemoval(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !owner {
		t.Fatal("first removal did not own the Workspace service fence")
	}
	t.Cleanup(func() { s.finishServiceManagerRemoval(removal, errors.New("test cleanup")) })

	if lease, err := s.acquireServiceManagerLease(workspace.ID); !errors.Is(err, errWorkspaceServiceRemovalInProgress) {
		if lease != nil {
			lease.Release()
		}
		t.Fatalf("operation admitted after removal fence: lease=%p err=%v", lease, err)
	}
}

func TestWorkspaceRemovalDrainsAllServiceOperationReferences(t *testing.T) {
	workspace := serveWorkspace{ID: "workspace-one", Path: t.TempDir()}
	s := newServiceLifecycleTestServer(t, workspace)
	first, err := s.acquireServiceManagerLease(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.acquireServiceManagerLease(workspace.ID)
	if err != nil {
		first.Release()
		t.Fatal(err)
	}
	t.Cleanup(first.Release)
	t.Cleanup(second.Release)

	removeResult := make(chan error, 1)
	go func() { removeResult <- s.removeWorkspace(workspace.ID) }()
	waitForServiceRemovalFence(t, s, workspace.ID)
	first.Release()
	assertRemovalStillWaiting(t, removeResult)
	second.Release()
	select {
	case err := <-removeResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Workspace removal did not drain after the final service operation released")
	}
	s.serviceMu.Lock()
	leaseCount := s.serviceLeases[workspace.ID]
	s.serviceMu.Unlock()
	if leaseCount != 0 {
		t.Fatalf("service operation reference count = %d, want 0", leaseCount)
	}
}

func TestNewServiceManagerLookupReturnsAnOperationLease(t *testing.T) {
	workspace := serveWorkspace{ID: "workspace-one", Path: t.TempDir()}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{workspace}}); err != nil {
		t.Fatal(err)
	}
	lease, err := s.acquireServiceManagerLease(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Release)

	removeResult := make(chan error, 1)
	go func() { removeResult <- s.removeWorkspace(workspace.ID) }()
	waitForServiceRemovalFence(t, s, workspace.ID)
	assertRemovalStillWaiting(t, removeResult)
	lease.Release()
	select {
	case err := <-removeResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("removal did not observe the newly constructed manager lease")
	}
}

func TestServiceActionFailureReleasesOperationLease(t *testing.T) {
	workspace := serveWorkspace{ID: "workspace-one", Path: t.TempDir()}
	s := newServiceLifecycleTestServer(t, workspace)
	request := httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-one/services/missing/start", nil)
	recorder := httptest.NewRecorder()
	s.handleWorkspaceServices(recorder, request, workspace.ID, []string{"missing", "start"})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("failed action status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	s.serviceMu.Lock()
	leaseCount := s.serviceLeases[workspace.ID]
	s.serviceMu.Unlock()
	if leaseCount != 0 {
		t.Fatalf("failed action retained %d service operation references", leaseCount)
	}
	if err := s.removeWorkspace(workspace.ID); err != nil {
		t.Fatalf("failed action blocked later Workspace removal: %v", err)
	}
}

func TestWorkspaceRemovalWaitsForSecretBindingResolution(t *testing.T) {
	workspace := serveWorkspace{ID: "workspace-one", Path: t.TempDir()}
	s := newServiceLifecycleTestServer(t, workspace)
	manager, _, err := serviceManagerForWorkspaceTest(s, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ApplyBindings(ServiceBindings{
		Secrets: map[string]string{"SERVICE_TOKEN": "${secret.lease-token}"},
	}); err != nil {
		t.Fatal(err)
	}
	resolverEntered := make(chan struct{})
	releaseResolver := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseResolver) }) }
	t.Cleanup(release)
	var resolverCalls atomic.Int32
	manager.mu.Lock()
	manager.resolver = ServiceSecretResolverFunc(func(name string) (string, string, error) {
		resolverCalls.Add(1)
		close(resolverEntered)
		<-releaseResolver
		return "ephemeral-value", "test", nil
	})
	manager.mu.Unlock()

	type environmentResult struct {
		secrets map[string]string
		err     error
	}
	environment := make(chan environmentResult, 1)
	go func() {
		_, secrets, err := s.serviceEnvironment(workspace)
		environment <- environmentResult{secrets: secrets, err: err}
	}()
	select {
	case <-resolverEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("secret binding resolution did not reach the resolver")
	}

	removeResult := make(chan error, 1)
	go func() { removeResult <- s.removeWorkspace(workspace.ID) }()
	waitForServiceRemovalFence(t, s, workspace.ID)
	assertRemovalStillWaiting(t, removeResult)
	if lease, err := s.acquireServiceManagerLease(workspace.ID); !errors.Is(err, errWorkspaceServiceRemovalInProgress) {
		if lease != nil {
			lease.Release()
		}
		t.Fatalf("secret operation admitted after removal fence: lease=%p err=%v", lease, err)
	}

	release()
	select {
	case result := <-environment:
		if result.err != nil || result.secrets["SERVICE_TOKEN"] != "ephemeral-value" {
			t.Fatalf("binding result = %#v, %v", result.secrets, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("secret binding resolution did not finish")
	}
	select {
	case err := <-removeResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Workspace removal did not continue after secret resolution")
	}
	if _, _, err := s.serviceEnvironment(workspace); err == nil {
		t.Fatal("removed Workspace resolved bindings after releasing service ownership")
	}
	if got := resolverCalls.Load(); got != 1 {
		t.Fatalf("secret resolver called %d times, want 1 before ownership release", got)
	}
}

func TestExplicitServiceActionsCannotReopenStoppingManager(t *testing.T) {
	manager, err := NewServiceManager(t.TempDir(), ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Apply(ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Command:       []string{"/bin/true"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, run := range map[string]func() error{
		"start":   func() error { return manager.StartService(context.Background(), "worker") },
		"restart": func() error { return manager.RestartService(context.Background(), "worker") },
	} {
		if err := run(); !errors.Is(err, errServiceManagerStopping) {
			t.Fatalf("%s action error = %v, want service manager stopping", name, err)
		}
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("lifecycle Start did not reopen the manager after rollback: %v", err)
	}
}
