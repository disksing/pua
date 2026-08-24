package serve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestServiceManagerRetriesOrderedOrphanRecoveryFailures(t *testing.T) {
	tests := []struct {
		name                string
		cleanupFailureID    string
		stateWriteFailureID string
		wantInitialStarts   []string
		wantCleanupOrder    []string
	}{
		{
			name:              "first cleanup failure",
			cleanupFailureID:  "charlie",
			wantInitialStarts: []string{"alpha", "bravo", "delta"},
			wantCleanupOrder:  []string{"charlie", "delta", "bravo", "alpha", "charlie"},
		},
		{
			name:                "middle state write failure",
			stateWriteFailureID: "bravo",
			wantInitialStarts:   []string{"alpha", "delta"},
			wantCleanupOrder:    []string{"charlie", "delta", "bravo", "alpha"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			launchesPath := filepath.Join(root, "launches")
			cleanupPath := filepath.Join(root, "cleanups")
			cleanupFailurePath := filepath.Join(root, "cleanup-failed-once")
			configs := []ServiceConfig{
				orphanRecoveryTestConfig("alpha", nil, launchesPath, cleanupPath, cleanupFailurePath, test.cleanupFailureID),
				orphanRecoveryTestConfig("bravo", []string{"alpha"}, launchesPath, cleanupPath, cleanupFailurePath, test.cleanupFailureID),
				orphanRecoveryTestConfig("charlie", []string{"bravo"}, launchesPath, cleanupPath, cleanupFailurePath, test.cleanupFailureID),
				orphanRecoveryTestConfig("delta", nil, launchesPath, cleanupPath, cleanupFailurePath, test.cleanupFailureID),
			}
			orphanByPID := make(map[int]ServiceStatus, len(configs))
			present := make(map[int]bool, len(configs))
			for index, cfg := range configs {
				writeTestService(t, root, cfg)
				status := initialServiceStatus(cfg)
				status.State = ServiceStateReady
				status.PID = 4100 + index
				status.ProcessGroup = status.PID
				status.ProcessStartID = "persisted-start-" + cfg.ID
				status.InstanceToken = "persisted-token-" + cfg.ID
				status.CommandDigest = serviceCommandDigest(cfg)
				if err := writeServiceJSON(filepath.Join(serviceRuntimePath(root, cfg.ID), "state.json"), status, 0o600); err != nil {
					t.Fatal(err)
				}
				orphanByPID[status.PID] = status
				present[status.ProcessGroup] = true
			}

			manager, err := NewServiceManager(root, ServiceManagerOptions{})
			if err != nil {
				t.Fatal(err)
			}
			stopProcessTestManager(t, &manager)
			manager.processTerminationGrace = 20 * time.Millisecond
			processPlatform, signalOrder := orderedOrphanRecoveryTestPlatform(orphanByPID, present)
			manager.processPlatform = processPlatform

			injected := errors.New("injected orphan state write failure")
			if test.stateWriteFailureID != "" {
				nativeWrite := manager.runtimeStateStore.writeJSON
				failed := false
				manager.runtimeStateStore.writeJSON = func(path string, value any, mode os.FileMode) error {
					persisted, ok := value.(persistedServiceRuntimeState)
					if ok && !failed && persisted.ID == test.stateWriteFailureID && persisted.PID == 0 && !persisted.OrphanRecoveryPending {
						failed = true
						return injected
					}
					return nativeWrite(path, value, mode)
				}
			}

			startErr := manager.Start(context.Background())
			if startErr == nil {
				t.Fatal("Start succeeded despite injected orphan recovery failure")
			}
			if test.stateWriteFailureID != "" && !errors.Is(startErr, injected) {
				t.Fatalf("Start error = %v, want injected state write failure", startErr)
			}
			wantSignals := []string{"charlie", "delta", "bravo", "alpha"}
			if !reflect.DeepEqual(*signalOrder, wantSignals) {
				t.Fatalf("orphan signal order = %v, want %v", *signalOrder, wantSignals)
			}
			for group, groupPresent := range present {
				if groupPresent {
					t.Fatalf("orphan process group %d was not inspected and reaped", group)
				}
			}

			failedID := test.cleanupFailureID
			if failedID == "" {
				failedID = test.stateWriteFailureID
			}
			failedRuntime := manager.runtimes[failedID]
			if failedRuntime == nil || !failedRuntime.orphanRecoveryPending || failedRuntime.process != nil || failedRuntime.status.State != ServiceStateAttentionRequired || !failedRuntime.status.AttentionRequired {
				t.Fatalf("failed orphan runtime = %#v", failedRuntime)
			}
			if test.cleanupFailureID != "" {
				persisted, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, failedID), "state.json"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(persisted), `"orphanRecoveryPending": true`) || !strings.Contains(string(persisted), `"processConfig"`) {
					t.Fatalf("pending cleanup state omitted recovery provenance: %s", persisted)
				}
			}
			if err := manager.StartService(context.Background(), failedID); err == nil {
				t.Fatal("explicit start bypassed pending orphan recovery")
			}
			waitForLaunches(t, launchesPath, len(test.wantInitialStarts))
			assertOrphanRecoveryLaunchCounts(t, launchesPath, test.wantInitialStarts)

			if err := manager.Reconcile(context.Background()); err != nil {
				t.Fatalf("retry Reconcile: %v", err)
			}
			waitForLaunches(t, launchesPath, len(configs))
			assertOrphanRecoveryLaunchCounts(t, launchesPath, []string{"alpha", "bravo", "charlie", "delta"})
			if !reflect.DeepEqual(*signalOrder, wantSignals) {
				t.Fatalf("retry signaled an already reaped orphan: %v", *signalOrder)
			}
			for _, cfg := range configs {
				status, err := manager.Show(cfg.ID)
				if err != nil {
					t.Fatal(err)
				}
				if status.State != ServiceStateReady || status.PID <= 0 || manager.runtimes[cfg.ID].orphanRecoveryPending {
					t.Fatalf("service %s after recovery retry = %#v", cfg.ID, status)
				}
			}
			cleanups, err := os.ReadFile(cleanupPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Fields(string(cleanups)); !reflect.DeepEqual(got, test.wantCleanupOrder) {
				t.Fatalf("cleanup order = %v, want %v", got, test.wantCleanupOrder)
			}
		})
	}
}

func orphanRecoveryTestConfig(id string, dependencies []string, launchesPath, cleanupPath, failurePath, failureID string) ServiceConfig {
	cleanup := "printf '" + id + "\\n' >> " + shellQuote(cleanupPath)
	if id == failureID {
		cleanup += "; if ! test -f " + shellQuote(failurePath) + "; then : > " + shellQuote(failurePath) + "; exit 17; fi"
	}
	return ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            id,
		Enabled:       true,
		DependsOn:     dependencies,
		Command: []string{"/bin/sh", "-c",
			"printf '" + id + "\\n' >> " + shellQuote(launchesPath) + "; exec sleep 30"},
		Cleanup: &ServiceCleanupConfig{
			Command: []string{"/bin/sh", "-c", cleanup},
			Timeout: time.Second,
		},
	}
}

func orderedOrphanRecoveryTestPlatform(orphanByPID map[int]ServiceStatus, present map[int]bool) (*serviceProcessPlatform, *[]string) {
	native := nativeServiceProcessPlatform()
	order := []string{}
	platform := *native
	platform.identityInspectionAvailable = true
	platform.identityMarkerRequired = false
	platform.processGroupPresent = func(group int) (bool, error) {
		if value, ok := present[group]; ok {
			return value, nil
		}
		return native.processGroupPresent(group)
	}
	platform.processPresent = func(pid int) (bool, error) {
		if status, ok := orphanByPID[pid]; ok {
			return present[status.ProcessGroup], nil
		}
		return native.processPresent(pid)
	}
	platform.readProcessIdentity = func(pid int) (serviceProcessIdentity, error) {
		if status, ok := orphanByPID[pid]; ok {
			return serviceProcessIdentity{
				pid:     pid,
				command: "persisted-service-" + status.ID,
				environment: []string{
					serviceInstanceTokenEnvironment + "=" + status.InstanceToken,
					serviceCommandDigestEnvironment + "=" + status.CommandDigest,
				},
				processGroup: status.ProcessGroup,
				startID:      status.ProcessStartID,
			}, nil
		}
		return native.readProcessIdentity(pid)
	}
	platform.signalProcessGroup = func(group int, signal syscall.Signal) error {
		if status, ok := orphanByPID[group]; ok {
			order = append(order, status.ID)
			present[group] = false
			return nil
		}
		return native.signalProcessGroup(group, signal)
	}
	return &platform, &order
}

func assertOrphanRecoveryLaunchCounts(t *testing.T, path string, wantStarted []string) {
	t.Helper()
	want := make(map[string]int, 4)
	for _, id := range []string{"alpha", "bravo", "charlie", "delta"} {
		want[id] = 0
	}
	for _, id := range wantStarted {
		want[id]++
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]int, 4)
	for id := range want {
		got[id] = 0
	}
	for _, id := range strings.Fields(string(data)) {
		got[id]++
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("service launch counts = %v, want %v", got, want)
	}
}
