package serve

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func initializeWorkspaceWithoutInstanceID(t *testing.T, root string) *app.Workspace {
	t.Helper()
	workspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "workspace.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	delete(cfg, "instanceId")
	data, err = json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.RuntimeConfig(); err == nil {
		t.Fatal("legacy Workspace unexpectedly retained a runtime identity")
	}
	return workspace
}

func newLiveAddTestServer(t *testing.T, configPath string) *server {
	t.Helper()
	server := &server{
		config: configPath,
		locks:  newWorkspaceLockManager("127.0.0.1:4936", configPath),
	}
	server.agents = newAgentManager(server)
	t.Cleanup(func() {
		server.agents.waitBackground()
		server.locks.closeAll()
	})
	return server
}

func requirePersistedLiveAddIdentity(t *testing.T, server *server, added serveWorkspace, want string) {
	t.Helper()
	if strings.TrimSpace(added.InstanceID) == "" {
		t.Fatal("added Workspace has no instance id")
	}
	if want != "" && added.InstanceID != want {
		t.Fatalf("added Workspace instance id = %q, want %q", added.InstanceID, want)
	}
	cfg, err := server.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Workspaces) != 1 || cfg.Workspaces[0].ID != added.ID || cfg.Workspaces[0].InstanceID != added.InstanceID {
		t.Fatalf("persisted live add = %#v, want instance %q", cfg.Workspaces, added.InstanceID)
	}
	runtimeID, err := workspaceInstanceID(added.Path)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeID != added.InstanceID {
		t.Fatalf("runtime instance id = %q, persisted %q", runtimeID, added.InstanceID)
	}
}

func TestLiveAddPersistsAuthoritativeWorkspaceInstanceID(t *testing.T) {
	t.Run("legacy Workspace", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "workspace")
		initializeWorkspaceWithoutInstanceID(t, root)
		server := newLiveAddTestServer(t, filepath.Join(t.TempDir(), "serve.json"))

		added, err := server.addWorkspace(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		requirePersistedLiveAddIdentity(t, server, added, "")
	})

	t.Run("initialized Workspace", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "workspace")
		workspace, err := app.Initialize(root, "en")
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := workspace.RuntimeConfig()
		if err != nil {
			t.Fatal(err)
		}
		server := newLiveAddTestServer(t, filepath.Join(t.TempDir(), "serve.json"))

		added, err := server.addWorkspace(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		requirePersistedLiveAddIdentity(t, server, added, runtime.InstanceID)
	})
}

func TestLiveAddedWorkspaceStaleRemovalDrainsPersistedController(t *testing.T) {
	for _, test := range []struct {
		name       string
		breakState func(*testing.T, string)
	}{
		{
			name: "missing after restart",
			breakState: func(t *testing.T, root string) {
				if err := os.RemoveAll(root); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "corrupt after restart",
			breakState: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, "workspace.json"), []byte("{"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "workspace")
			initializeWorkspaceWithoutInstanceID(t, root)
			configPath := filepath.Join(t.TempDir(), "serve.json")
			first := newLiveAddTestServer(t, configPath)
			added, err := first.addWorkspace(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			requirePersistedLiveAddIdentity(t, first, added, "")
			first.locks.closeAll()

			restarted := newLiveAddTestServer(t, configPath)
			if err := restarted.acquireConfiguredWorkspaceLocks(); err != nil {
				t.Fatal(err)
			}
			if err := restarted.ensureConfiguredResourceRuntimes(); err != nil {
				t.Fatal(err)
			}
			persisted, err := restarted.loadConfig()
			if err != nil || len(persisted.Workspaces) != 1 || persisted.Workspaces[0].InstanceID != added.InstanceID {
				t.Fatalf("restarted live add = %#v, %v", persisted.Workspaces, err)
			}

			controller, release := holdSchedulerController(t, restarted.agents, added)
			test.breakState(t, root)
			removeDone := make(chan error, 1)
			go func() { removeDone <- restarted.removeWorkspace(added.ID) }()
			waitForSchedulerControllerQueue(t, controller, 1)

			queuedDone := make(chan error, 1)
			go func() {
				queuedDone <- restarted.agents.withResourceControllerInstanceID(context.Background(), added.InstanceID, app.SchedulerResourceID, func() error {
					return restarted.requireWorkspaceOwnership(root)
				})
			}()
			waitForSchedulerControllerQueue(t, controller, 2)
			release()
			if err := <-removeDone; err != nil {
				t.Fatalf("remove stale live-added Workspace: %v", err)
			}
			if err := <-queuedDone; err == nil || !strings.Contains(err.Error(), "not owned") {
				t.Fatalf("queued Scheduler work crossed ownership handoff: %v", err)
			}
			requireWorkspaceRemoved(t, restarted, added)

			newOwner := newWorkspaceLockManager("127.0.0.1:4999", filepath.Join(t.TempDir(), "new-owner.json"))
			defer newOwner.closeAll()
			if _, err := newOwner.acquire(root); err != nil {
				t.Fatalf("new owner could not acquire after drained removal: %v", err)
			}
		})
	}
}

func TestLiveAddSaveFailureLeavesIdentityRecoverableAndUnmanaged(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	workspace := initializeWorkspaceWithoutInstanceID(t, root)
	server := newLiveAddTestServer(t, filepath.Join(t.TempDir(), "serve.json"))
	wantErr := errors.New("injected live add save failure")
	var candidate config

	_, err := server.addWorkspaceWithOptionsAndSave(context.Background(), root, false, "", "", func(cfg config) error {
		candidate = cfg
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("live add save failure = %v, want %v", err, wantErr)
	}
	if len(candidate.Workspaces) != 1 || strings.TrimSpace(candidate.Workspaces[0].InstanceID) == "" {
		t.Fatalf("failed save candidate has no runtime identity: %#v", candidate.Workspaces)
	}
	cfg, err := server.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Workspaces) != 0 {
		t.Fatalf("failed save created managed entry: %#v", cfg.Workspaces)
	}
	if server.locks.owns(root) {
		t.Fatal("failed live add retained the Workspace ownership lock")
	}
	runtime, err := workspace.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.InstanceID != candidate.Workspaces[0].InstanceID {
		t.Fatalf("recoverable runtime id = %q, candidate %q", runtime.InstanceID, candidate.Workspaces[0].InstanceID)
	}

	probe := newWorkspaceLockManager("", "")
	if _, err := probe.acquire(root); err != nil {
		t.Fatalf("failed live add lock was not recoverable: %v", err)
	}
	probe.release(root)
	probe.closeAll()

	added, err := server.addWorkspace(context.Background(), root)
	if err != nil {
		t.Fatalf("retry live add: %v", err)
	}
	requirePersistedLiveAddIdentity(t, server, added, runtime.InstanceID)
}
