package serve

import (
	"context"
	"errors"
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

func waitForServiceLifecycleClosing(t *testing.T, s *server) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.serviceMu.Lock()
		closing := s.serviceClosing
		s.serviceMu.Unlock()
		if closing {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("service lifecycle did not begin closing")
		}
		time.Sleep(time.Millisecond)
	}
}

func assertServiceShutdownWaiting(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		t.Fatalf("service shutdown completed before admitted lifecycle work: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestServiceShutdownWaitsForAdmittedOperationLease(t *testing.T) {
	workspace := serveWorkspace{ID: "workspace-one", Path: t.TempDir()}
	s := newServiceLifecycleTestServer(t, workspace)
	lease, err := s.acquireServiceManagerLease(workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Release)

	result := make(chan error, 1)
	go func() { result <- s.stopServices(context.Background()) }()
	assertServiceShutdownWaiting(t, result)

	lease.Release()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("service shutdown did not continue after the admitted operation lease finished")
	}
}

func TestServiceShutdownDrainsAdmittedStartAndRestartBeforeFinalStop(t *testing.T) {
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

			// Hold the manager boundary after lifecycle admission so the action is
			// guaranteed not to have started when shutdown closes admission.
			lease.manager.mu.Lock()
			managerUnlocked := false
			t.Cleanup(func() {
				lease.Release()
				if !managerUnlocked {
					lease.manager.mu.Unlock()
				}
			})
			actionEntered := make(chan struct{})
			type actionOutcome struct {
				err  error
				pgid int
			}
			actionResult := make(chan actionOutcome, 1)
			go func() {
				close(actionEntered)
				outcome := actionOutcome{err: action.run(context.Background(), lease.manager, "worker")}
				if status, statusErr := lease.manager.Show("worker"); outcome.err == nil && statusErr != nil {
					outcome.err = statusErr
				} else {
					outcome.pgid = status.ProcessGroup
				}
				actionResult <- outcome
				lease.Release()
			}()
			<-actionEntered

			shutdownResult := make(chan error, 1)
			go func() { shutdownResult <- s.stopServices(context.Background()) }()
			waitForServiceLifecycleClosing(t, s)
			assertServiceShutdownWaiting(t, shutdownResult)

			lease.manager.mu.Unlock()
			managerUnlocked = true
			launchedProcessGroup := 0
			select {
			case outcome := <-actionResult:
				if outcome.err != nil {
					t.Fatalf("admitted %s failed during shutdown drain: %v", action.name, outcome.err)
				}
				if outcome.pgid <= 0 {
					t.Fatalf("admitted %s did not launch a process group", action.name)
				}
				launchedProcessGroup = outcome.pgid
			case <-time.After(3 * time.Second):
				t.Fatalf("admitted %s did not finish", action.name)
			}
			select {
			case err := <-shutdownResult:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("service shutdown did not finish after the admitted action")
			}

			status, err := lease.manager.Show("worker")
			if err != nil {
				t.Fatal(err)
			}
			if status.PID != 0 || status.ProcessGroup != 0 || status.State != ServiceStateStopped {
				t.Fatalf("final service status = %#v, want stopped without a live process", status)
			}
			if present, err := processGroupPresent(launchedProcessGroup); err != nil || present {
				t.Fatalf("final process group %d present = %v, %v, want reaped", launchedProcessGroup, present, err)
			}
			if launches, err := os.ReadFile(launches); err != nil || string(launches) != "launch\n" {
				t.Fatalf("admitted %s launches = %q, %v, want one completed launch before final stop", action.name, launches, err)
			}
		})
	}
}

func TestServiceShutdownIncludesAdditionCommittedDuringDrain(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command:       []string{"/bin/sh", "-c", "exec sleep 30"},
	})
	configPath := filepath.Join(t.TempDir(), "serve.json")
	s := &server{
		config:         configPath,
		locks:          newWorkspaceLockManager("127.0.0.1:4936", configPath),
		serviceContext: context.Background(),
	}
	if err := s.saveConfig(config{Version: agentHubConfigVersion}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.locks.closeAll)

	factoryEntered := make(chan struct{})
	releaseFactory := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFactory) }) }
	t.Cleanup(release)
	s.serviceFactory = func(root string, options ServiceManagerOptions) (*ServiceManager, error) {
		close(factoryEntered)
		<-releaseFactory
		return NewServiceManager(root, options)
	}
	stopped := make(chan *ServiceManager, 1)
	s.serviceShutdownStopper = func(manager *ServiceManager, ctx context.Context) error {
		stopped <- manager
		return manager.Stop(ctx)
	}

	type additionResult struct {
		workspace serveWorkspace
		err       error
	}
	addition := make(chan additionResult, 1)
	go func() {
		workspace, err := s.addWorkspace(context.Background(), root)
		addition <- additionResult{workspace: workspace, err: err}
	}()
	select {
	case <-factoryEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("Workspace addition did not reach its post-commit manager construction")
	}

	shutdown := make(chan error, 1)
	go func() { shutdown <- s.stopServices(context.Background()) }()
	waitForServiceLifecycleClosing(t, s)
	assertServiceShutdownWaiting(t, shutdown)
	release()

	var added additionResult
	select {
	case added = <-addition:
		if added.err != nil {
			t.Fatal(added.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("admitted Workspace addition did not finish")
	}
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("service shutdown did not finish after the addition")
	}
	select {
	case manager := <-stopped:
		if manager.Root() != added.workspace.Path {
			t.Fatalf("stopped manager root = %q, want %q", manager.Root(), added.workspace.Path)
		}
	default:
		t.Fatal("final service snapshot omitted the manager added during drain")
	}
}

func TestServiceLifecycleClosingRejectsNewRequestsWithoutEffects(t *testing.T) {
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
	s.beginServiceLifecycleShutdown()

	request := httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-one/services/worker/start", nil)
	recorder := httptest.NewRecorder()
	s.handleWorkspaceServices(recorder, request, workspace.ID, []string{"worker", "start"})
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), errServiceLifecycleClosing.Error()) {
		t.Fatalf("request after closing = %d %q, want stable shutdown error", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(launches); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("request after closing launch effect = %v, want none", err)
	}
	if err := s.removeWorkspace(workspace.ID); !errors.Is(err, errServiceLifecycleClosing) {
		t.Fatalf("removal after closing error = %v, want %v", err, errServiceLifecycleClosing)
	}
	if _, err := s.addWorkspace(context.Background(), t.TempDir()); !errors.Is(err, errServiceLifecycleClosing) {
		t.Fatalf("addition after closing error = %v, want %v", err, errServiceLifecycleClosing)
	}
	s.serviceMu.Lock()
	mutationCount := s.serviceMutations
	managerCount := len(s.services)
	s.serviceMu.Unlock()
	if mutationCount != 0 || managerCount != 1 {
		t.Fatalf("rejected closing effects = (%d mutations, %d managers), want (0, 1)", mutationCount, managerCount)
	}
}

func TestServiceShutdownWaitsForSecretBindingLease(t *testing.T) {
	workspace := serveWorkspace{ID: "workspace-one", Path: t.TempDir()}
	s := newServiceLifecycleTestServer(t, workspace)
	manager, _, err := serviceManagerForWorkspaceTest(s, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ApplyBindings(ServiceBindings{Secrets: map[string]string{"TOKEN": "${secret.token}"}}); err != nil {
		t.Fatal(err)
	}
	resolverEntered := make(chan struct{})
	releaseResolver := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseResolver) }) }
	t.Cleanup(release)
	manager.mu.Lock()
	manager.resolver = ServiceSecretResolverFunc(func(string) (string, string, error) {
		close(resolverEntered)
		<-releaseResolver
		return "ephemeral", "test", nil
	})
	manager.mu.Unlock()

	environment := make(chan error, 1)
	go func() {
		_, _, err := s.serviceEnvironment(workspace)
		environment <- err
	}()
	select {
	case <-resolverEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("binding resolution did not begin")
	}
	shutdown := make(chan error, 1)
	go func() { shutdown <- s.stopServices(context.Background()) }()
	waitForServiceLifecycleClosing(t, s)
	assertServiceShutdownWaiting(t, shutdown)
	release()
	if err := <-environment; err != nil {
		t.Fatal(err)
	}
	if err := <-shutdown; err != nil {
		t.Fatal(err)
	}
}

func TestServiceShutdownDrainsAllManagersAfterStopFailure(t *testing.T) {
	first := serveWorkspace{ID: "workspace-one", Path: t.TempDir()}
	second := serveWorkspace{ID: "workspace-two", Path: t.TempDir()}
	s := newServiceLifecycleTestServer(t, first, second)
	injected := errors.New("stop failed")
	stopped := make(map[string]int)
	s.serviceShutdownStopper = func(manager *ServiceManager, _ context.Context) error {
		stopped[manager.Root()]++
		if manager.Root() == first.Path {
			return injected
		}
		return nil
	}

	if err := s.stopServices(context.Background()); !errors.Is(err, injected) {
		t.Fatalf("shutdown error = %v, want %v", err, injected)
	}
	if stopped[first.Path] != 1 || stopped[second.Path] != 1 {
		t.Fatalf("stop attempts = %#v, want every manager once", stopped)
	}
}
