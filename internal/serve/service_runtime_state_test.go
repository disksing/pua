package serve

import (
	"context"
	"encoding/json"
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
	launches := filepath.Join(root, "launches")
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c",
			"printf 'launch\\n' >> " + shellQuote(launches) + "; while :; do sleep 1; done"},
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
	nativeWrite := manager.runtimeStateStore.writeJSON
	manager.runtimeStateStore.writeJSON = func(path string, value any, mode os.FileMode) error {
		persisted, ok := value.(persistedServiceRuntimeState)
		if !ok {
			t.Fatalf("runtime state value type = %T, want persistedServiceRuntimeState", value)
		}
		if persisted.PID > 0 {
			startedPID = persisted.PID
			return injected
		}
		return nativeWrite(path, value, mode)
	}

	err = manager.Start(context.Background())
	if !errors.Is(err, injected) {
		t.Fatalf("Start error = %v, want injected write failure", err)
	}
	if startedPID <= 0 {
		t.Fatalf("persisted PID candidate = %d, want a started process", startedPID)
	}
	waitForProcessGone(t, startedPID)
	if data, err := os.ReadFile(launches); err == nil {
		t.Fatalf("service code executed without durable process ownership: %q", data)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
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
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var pending persistedServiceRuntimeState
	if err := json.Unmarshal(stateData, &pending); err != nil {
		t.Fatal(err)
	}
	if !pending.LaunchPending || pending.PID != 0 || pending.ProcessGroup != 0 || pending.ProcessConfig == nil {
		t.Fatalf("durable launch intent after ownership write failure = %#v", pending)
	}
	eventsPath := filepath.Join(serviceRuntimePath(root, "worker"), "events.jsonl")
	if events, err := os.ReadFile(eventsPath); err == nil && strings.Contains(string(events), `"type":"started"`) {
		t.Fatalf("started event published without durable ownership: %s", events)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestServiceManagerPersistsLaunchIntentBeforeFork(t *testing.T) {
	root := t.TempDir()
	launches := filepath.Join(root, "launches")
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c",
			"printf 'launch\\n' >> " + shellQuote(launches) + "; exec sleep 30"},
	})
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	t.Cleanup(func() { manager.runtimeStateStore = defaultServiceRuntimeStateStore() })
	injected := errors.New("injected launch intent write failure")
	forked := false
	manager.beforeServiceLaunchRelease = func() { forked = true }
	manager.runtimeStateStore.writeJSON = func(_ string, value any, _ os.FileMode) error {
		persisted, ok := value.(persistedServiceRuntimeState)
		if !ok {
			t.Fatalf("runtime state value type = %T, want persistedServiceRuntimeState", value)
		}
		if persisted.PID != 0 || persisted.ProcessGroup != 0 {
			t.Fatalf("first launch intent unexpectedly owns a process: %#v", persisted)
		}
		return injected
	}

	err = manager.Start(context.Background())
	if !errors.Is(err, injected) {
		t.Fatalf("Start error = %v, want intent write failure", err)
	}
	if forked {
		t.Fatal("service helper forked before launch intent persisted")
	}
	if data, err := os.ReadFile(launches); err == nil {
		t.Fatalf("service code executed before launch intent persisted: %q", data)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestServiceManagerRecoversFailedLaunchAuthorization(t *testing.T) {
	root := t.TempDir()
	launches := filepath.Join(root, "launches")
	cleanups := filepath.Join(root, "cleanups")
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c",
			"printf 'launch\\n' >> " + shellQuote(launches) + "; exec sleep 30"},
		Cleanup: &ServiceCleanupConfig{
			Command: []string{"/bin/sh", "-c", "printf 'cleanup\\n' >> " + shellQuote(cleanups)},
			Timeout: time.Second,
		},
	})
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manager.processTerminationGrace = 100 * time.Millisecond
	injected := errors.New("injected launch authorization write failure")
	nativeWrite := manager.runtimeStateStore.writeJSON
	startedPID := 0
	manager.runtimeStateStore.writeJSON = func(path string, value any, mode os.FileMode) error {
		persisted, ok := value.(persistedServiceRuntimeState)
		if !ok {
			t.Fatalf("runtime state value type = %T, want persistedServiceRuntimeState", value)
		}
		if persisted.PID > 0 && !persisted.LaunchPending {
			startedPID = persisted.PID
			return injected
		}
		return nativeWrite(path, value, mode)
	}

	err = manager.Start(context.Background())
	if !errors.Is(err, injected) {
		t.Fatalf("Start error = %v, want authorization write failure", err)
	}
	if startedPID <= 0 {
		t.Fatalf("barrier PID = %d, want real helper", startedPID)
	}
	waitForProcessGone(t, startedPID)
	if data, err := os.ReadFile(launches); err == nil {
		t.Fatalf("service code executed without durable launch authorization: %q", data)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if cleanups := waitForLaunches(t, cleanups, 1); len(cleanups) != 1 {
		t.Fatalf("failed launch cleanup count = %d, want one", len(cleanups))
	}
	stateData, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "worker"), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pending persistedServiceRuntimeState
	if err := json.Unmarshal(stateData, &pending); err != nil {
		t.Fatal(err)
	}
	if !pending.LaunchPending || pending.PID != startedPID || pending.ProcessGroup != startedPID || pending.ProcessStartID == "" {
		t.Fatalf("recoverable authorization failure state = %#v", pending)
	}

	manager.runtimeStateStore = defaultServiceRuntimeStateStore()
	reconstructed, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reconstructed.processTerminationGrace = 100 * time.Millisecond
	stopProcessTestManager(t, &reconstructed)
	if err := reconstructed.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if launches := waitForLaunches(t, launches, 1); len(launches) != 1 {
		t.Fatalf("recovered launch count = %d, want one", len(launches))
	}
	if cleanups := waitForLaunches(t, cleanups, 1); len(cleanups) != 1 {
		t.Fatalf("recovery duplicated failed launch cleanup: %d", len(cleanups))
	}
}
