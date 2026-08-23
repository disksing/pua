package serve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewServiceManagerRejectsInvalidRuntimeStateBeforeStartingDuplicate(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(string) error
		open    func(string, error) (*ServiceManager, error)
	}{
		{
			name: "malformed",
			corrupt: func(path string) error {
				return os.WriteFile(path, []byte("{not-json\n"), 0o600)
			},
			open: func(root string, _ error) (*ServiceManager, error) {
				return NewServiceManager(root, ServiceManagerOptions{})
			},
		},
		{
			name:    "unreadable",
			corrupt: func(string) error { return nil },
			open: func(root string, injected error) (*ServiceManager, error) {
				return newServiceManager(root, ServiceManagerOptions{}, serviceRuntimeStateStore{
					readFile: func(path string) ([]byte, error) {
						if filepath.Base(path) == "state.json" {
							return nil, injected
						}
						return os.ReadFile(path)
					},
				})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			starts := filepath.Join(root, "starts")
			writeTestService(t, root, ServiceConfig{
				SchemaVersion: serviceSchemaVersion,
				ID:            "worker",
				Enabled:       true,
				Command: []string{"/bin/sh", "-c",
					"printf 'start\\n' >> " + shellQuote(starts) + "; while :; do sleep 1; done"},
			})

			owner, err := NewServiceManager(root, ServiceManagerOptions{})
			if err != nil {
				t.Fatal(err)
			}
			stopProcessTestManager(t, &owner)
			if err := owner.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			waitForLaunches(t, starts, 1)

			statePath := filepath.Join(serviceRuntimePath(root, "worker"), "state.json")
			if err := test.corrupt(statePath); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected runtime state read failure")
			reconstructed, err := test.open(root, injected)
			if err == nil {
				if reconstructed != nil {
					_ = reconstructed.Stop(context.Background())
				}
				t.Fatal("NewServiceManager succeeded with invalid runtime state")
			}
			if !strings.Contains(err.Error(), "load service worker runtime state") {
				t.Fatalf("NewServiceManager error = %v, want runtime state context", err)
			}
			if test.name == "unreadable" && !errors.Is(err, injected) {
				t.Fatalf("NewServiceManager error = %v, want injected read failure", err)
			}
			time.Sleep(50 * time.Millisecond)
			if launches := serviceTransactionMarkerCount(t, starts, "start"); launches != 1 {
				t.Fatalf("service launches = %d, want surviving group only", launches)
			}
		})
	}
}

func TestServiceManagerReapsNewProcessWhenRuntimeStateWriteFails(t *testing.T) {
	root := t.TempDir()
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command:       []string{"/bin/sh", "-c", "while :; do sleep 1; done"},
	})
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manager.processTerminationGrace = 100 * time.Millisecond
	t.Cleanup(func() {
		manager.runtimeStateStore = defaultServiceRuntimeStateStore()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = manager.Stop(ctx)
	})

	injected := errors.New("injected runtime state write failure")
	startedPID := 0
	manager.runtimeStateStore.writeJSON = func(_ string, value any, _ os.FileMode) error {
		persisted, ok := value.(persistedServiceRuntimeState)
		if !ok {
			t.Fatalf("runtime state value type = %T, want persistedServiceRuntimeState", value)
		}
		startedPID = persisted.PID
		return injected
	}

	err = manager.Start(context.Background())
	if !errors.Is(err, injected) {
		t.Fatalf("Start error = %v, want injected write failure", err)
	}
	if startedPID <= 0 {
		t.Fatalf("persisted PID candidate = %d, want a started process", startedPID)
	}
	waitForProcessGone(t, startedPID)
	rt := manager.runtimes["worker"]
	if rt.process != nil || rt.status.PID != 0 || rt.status.ProcessGroup != 0 {
		t.Fatalf("runtime retained unrecorded process ownership: process=%v status=%#v", rt.process, rt.status)
	}
	if rt.status.State != ServiceStateAttentionRequired || !rt.status.AttentionRequired {
		t.Fatalf("runtime status after failed ownership record = %#v", rt.status)
	}
	if !strings.Contains(rt.status.LastError, injected.Error()) {
		t.Fatalf("runtime error = %q, want write failure", rt.status.LastError)
	}
	statePath := filepath.Join(serviceRuntimePath(root, "worker"), "state.json")
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("runtime state exists after injected write failure: %v", err)
	}
	eventsPath := filepath.Join(serviceRuntimePath(root, "worker"), "events.jsonl")
	if events, err := os.ReadFile(eventsPath); err == nil && strings.Contains(string(events), `"type":"started"`) {
		t.Fatalf("started event published without durable ownership: %s", events)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
