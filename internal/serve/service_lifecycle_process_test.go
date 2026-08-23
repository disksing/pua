package serve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stopProcessTestManager(t *testing.T, manager **ServiceManager) {
	t.Helper()
	t.Cleanup(func() {
		if *manager == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := (*manager).Stop(ctx); err != nil {
			t.Errorf("stop process test manager: %v", err)
		}
	})
}

func waitForServiceState(t *testing.T, manager *ServiceManager, id string, want ServiceState) ServiceStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := manager.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		status, err := manager.Show(id)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == want {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("service %s state = %q, want %q", id, status.State, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForLaunches(t *testing.T, path string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Fields(string(data))
			if len(lines) >= want {
				return lines
			}
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("launch count in %s did not reach %d", filepath.Base(path), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for processExists(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d remained alive", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readPersistedServiceStatus(t *testing.T, root, id string) ServiceStatus {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, id), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var status ServiceStatus
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestServiceManagerBlocksDependentsUntilReadiness(t *testing.T) {
	root := t.TempDir()
	orderPath := filepath.Join(root, "start-order")
	readyPath := filepath.Join(root, "dependency-ready")
	now := time.Date(2026, time.August, 24, 1, 0, 0, 0, time.UTC)
	restart := ServiceRestartConfig{
		InitialDelay: 100 * time.Millisecond,
		Multiplier:   2,
		MaxDelay:     time.Second,
		ResetAfter:   time.Minute,
	}
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "dependency",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c",
			"printf 'dependency\\n' >> " + shellQuote(orderPath) + "; exec sleep 30"},
		Readiness: &ServiceReadinessConfig{
			Command:  []string{"/bin/sh", "-c", "sleep 0.05; test -f " + shellQuote(readyPath)},
			Interval: 10 * time.Millisecond,
			Timeout:  500 * time.Millisecond,
		},
		Restart: restart,
	})
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "consumer",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c",
			"printf 'consumer\\n' >> " + shellQuote(orderPath) + "; exec sleep 30"},
		DependsOn: []string{"dependency"},
		Restart:   restart,
	})

	manager, err := NewServiceManager(root, ServiceManagerOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	dependency, err := manager.Show("dependency")
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := manager.Show("consumer")
	if err != nil {
		t.Fatal(err)
	}
	if dependency.State != ServiceStateBackoff || dependency.Readiness.Ready {
		t.Fatalf("unready dependency status = %#v, want backoff and not ready", dependency)
	}
	if consumer.State != ServiceStateBlocked || consumer.PID != 0 {
		t.Fatalf("consumer status = %#v, want blocked without a process", consumer)
	}
	if launches := waitForLaunches(t, orderPath, 1); len(launches) != 1 || launches[0] != "dependency" {
		t.Fatalf("launch order before readiness = %#v, want dependency only", launches)
	}

	if err := os.WriteFile(readyPath, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	dependency = waitForServiceState(t, manager, "dependency", ServiceStateReady)
	consumer = waitForServiceState(t, manager, "consumer", ServiceStateReady)
	if dependency.PID <= 0 || consumer.PID <= 0 {
		t.Fatalf("ready process IDs = dependency %d, consumer %d", dependency.PID, consumer.PID)
	}
	launches := waitForLaunches(t, orderPath, 3)
	if got := strings.Join(launches, ","); got != "dependency,dependency,consumer" {
		t.Fatalf("launch order = %q, want dependency,dependency,consumer", got)
	}
	events, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "dependency"), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(events), `"type":"started"`) != 2 || !strings.Contains(string(events), `"type":"readiness_failed"`) {
		t.Fatalf("dependency events did not record blocked startup and retry: %s", events)
	}
}

func TestServiceManagerRetriesTimedOutCleanup(t *testing.T) {
	root := t.TempDir()
	attemptPath := filepath.Join(root, "cleanup-attempt")
	tracePath := filepath.Join(root, "cleanup-trace")
	cleanupScript := "attempt=0; " +
		"if test -f " + shellQuote(attemptPath) + "; then read attempt < " + shellQuote(attemptPath) + "; fi; " +
		"attempt=$((attempt + 1)); printf '%s\\n' \"$attempt\" > " + shellQuote(attemptPath) + "; " +
		"printf 'attempt-%s-start\\n' \"$attempt\" >> " + shellQuote(tracePath) + "; " +
		"if test \"$attempt\" -eq 1; then exec sleep 30; fi; " +
		"printf 'attempt-%s-finished\\n' \"$attempt\" >> " + shellQuote(tracePath)
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command:       []string{"/bin/sh", "-c", "exec sleep 30"},
		Cleanup: &ServiceCleanupConfig{
			Command: []string{"/bin/sh", "-c", cleanupScript},
			Timeout: 100 * time.Millisecond,
			Retries: 1,
		},
	})

	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	running, err := manager.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if running.State != ServiceStateReady || running.PID <= 0 {
		t.Fatalf("worker did not start before cleanup: %#v", running)
	}

	started := time.Now()
	if err := manager.StopService(context.Background(), "worker"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("cleanup retry duration = %s, want one bounded timeout", elapsed)
	}
	waitForProcessGone(t, running.PID)
	trace, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(trace), "attempt-1-start\nattempt-2-start\nattempt-2-finished\n"; got != want {
		t.Fatalf("cleanup trace = %q, want %q", got, want)
	}
	status := readPersistedServiceStatus(t, root, "worker")
	if status.State != ServiceStateStopped || !status.ManualStop || status.Cleanup.Attempts != 2 || !status.Cleanup.Succeeded || status.Cleanup.LastError != "" {
		t.Fatalf("persisted cleanup status = %#v", status)
	}

	restarted, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restarted.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Cleanup.Attempts != 2 || !restored.Cleanup.Succeeded || !restored.ManualStop {
		t.Fatalf("reloaded cleanup status = %#v", restored)
	}
}

func TestServiceManagerPersistsBackoffAcrossReconstruction(t *testing.T) {
	root := t.TempDir()
	launchPath := filepath.Join(root, "launches")
	now := time.Date(2026, time.August, 24, 2, 0, 0, 0, time.UTC)
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "crasher",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c",
			"printf '%s\\n' \"$PUA_SERVICE_INSTANCE_TOKEN\" >> " + shellQuote(launchPath) + "; exit 17"},
		Restart: ServiceRestartConfig{
			InitialDelay: 500 * time.Millisecond,
			Multiplier:   2,
			MaxDelay:     5 * time.Second,
			ResetAfter:   time.Minute,
		},
	})

	manager, err := NewServiceManager(root, ServiceManagerOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := waitForServiceState(t, manager, "crasher", ServiceStateBackoff)
	if status.FailureCount != 1 || status.ExitCode != 17 {
		t.Fatalf("unexpected-exit status = %#v", status)
	}
	wantRetry := now.Add(500 * time.Millisecond).Format(time.RFC3339Nano)
	if status.NextRetryAt != wantRetry {
		t.Fatalf("next retry = %q, want %q", status.NextRetryAt, wantRetry)
	}
	if launches := waitForLaunches(t, launchPath, 1); len(launches) != 1 {
		t.Fatalf("launches before reconstruction = %#v", launches)
	}
	events, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "crasher"), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), `"type":"exited"`) || !strings.Contains(string(events), `"code":17`) {
		t.Fatalf("unexpected exit event missing: %s", events)
	}

	restarted, err := NewServiceManager(root, ServiceManagerOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	manager = restarted
	if err := restarted.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if launches := waitForLaunches(t, launchPath, 1); len(launches) != 1 {
		t.Fatalf("manager reconstruction bypassed persisted backoff: %#v", launches)
	}
	restored, err := restarted.Show("crasher")
	if err != nil {
		t.Fatal(err)
	}
	if restored.State != ServiceStateBackoff || restored.FailureCount != 1 || restored.NextRetryAt != wantRetry {
		t.Fatalf("restored backoff = %#v", restored)
	}

	now = now.Add(time.Second)
	if err := restarted.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	launches := waitForLaunches(t, launchPath, 2)
	if launches[0] == launches[1] {
		t.Fatalf("retry reused service instance token %q", launches[0])
	}
	status = waitForServiceState(t, restarted, "crasher", ServiceStateBackoff)
	if status.FailureCount != 2 {
		t.Fatalf("failure count after due retry = %d, want 2", status.FailureCount)
	}
}

func TestServiceManagerManualStartStopAndRestart(t *testing.T) {
	root := t.TempDir()
	launchPath := filepath.Join(root, "manual-launches")
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c",
			"printf '%s\\n' \"$PUA_SERVICE_INSTANCE_TOKEN\" >> " + shellQuote(launchPath) + "; exec sleep 30"},
	})

	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if err := manager.StartService(context.Background(), "worker"); err != nil {
		t.Fatal(err)
	}
	first, err := manager.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if first.State != ServiceStateReady || first.ManualStop || first.PID <= 0 {
		t.Fatalf("manual start status = %#v", first)
	}
	waitForLaunches(t, launchPath, 1)

	if err := manager.StopService(context.Background(), "worker"); err != nil {
		t.Fatal(err)
	}
	waitForProcessGone(t, first.PID)
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	stopped, err := manager.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != ServiceStateStopped || !stopped.ManualStop || stopped.FailureCount != 0 || stopped.NextRetryAt != "" {
		t.Fatalf("manual stop status = %#v", stopped)
	}
	if launches := waitForLaunches(t, launchPath, 1); len(launches) != 1 {
		t.Fatalf("manual stop allowed automatic restart: %#v", launches)
	}

	restarted, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manager = restarted
	if err := restarted.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, err := restarted.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if restored.State != ServiceStateStopped || !restored.ManualStop {
		t.Fatalf("reconstructed manual stop = %#v", restored)
	}
	if launches := waitForLaunches(t, launchPath, 1); len(launches) != 1 {
		t.Fatalf("reconstruction ignored manual stop: %#v", launches)
	}

	if err := restarted.StartService(context.Background(), "worker"); err != nil {
		t.Fatal(err)
	}
	secondLaunches := waitForLaunches(t, launchPath, 2)
	second, err := restarted.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if second.State != ServiceStateReady || second.ManualStop || second.PID <= 0 || secondLaunches[0] == secondLaunches[1] {
		t.Fatalf("second manual start status = %#v, launches = %#v", second, secondLaunches)
	}

	if err := restarted.RestartService(context.Background(), "worker"); err != nil {
		t.Fatal(err)
	}
	launches := waitForLaunches(t, launchPath, 3)
	afterRestart, err := restarted.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if afterRestart.State != ServiceStateReady || afterRestart.ManualStop || afterRestart.PID <= 0 {
		t.Fatalf("manual restart status = %#v", afterRestart)
	}
	if launches[2] == launches[0] || launches[2] == launches[1] {
		t.Fatalf("manual restart reused instance token: %#v", launches)
	}
}

func TestServiceManagerShutdownSuppressesRestart(t *testing.T) {
	root := t.TempDir()
	launchPath := filepath.Join(root, "shutdown-launches")
	now := time.Date(2026, time.August, 24, 3, 0, 0, 0, time.UTC)
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c",
			"printf '%s\\n' \"$PUA_SERVICE_INSTANCE_TOKEN\" >> " + shellQuote(launchPath) + "; exec sleep 30"},
		Restart: ServiceRestartConfig{
			InitialDelay: 10 * time.Millisecond,
			Multiplier:   2,
			MaxDelay:     time.Second,
			ResetAfter:   time.Minute,
		},
	})

	manager, err := NewServiceManager(root, ServiceManagerOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	running, err := manager.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if running.State != ServiceStateReady || running.PID <= 0 {
		t.Fatalf("worker did not start before shutdown: %#v", running)
	}
	waitForLaunches(t, launchPath, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	waitForProcessGone(t, running.PID)
	status := readPersistedServiceStatus(t, root, "worker")
	if status.State != ServiceStateStopped || status.ManualStop || status.FailureCount != 0 || status.NextRetryAt != "" || status.PID != 0 {
		t.Fatalf("graceful shutdown status entered restart policy: %#v", status)
	}
	now = now.Add(time.Hour)
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	afterReconcile, err := manager.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if afterReconcile.State != ServiceStateStopped || afterReconcile.PID != 0 || afterReconcile.FailureCount != 0 || afterReconcile.NextRetryAt != "" {
		t.Fatalf("stopped manager changed lifecycle state after reconcile: %#v", afterReconcile)
	}
	if launches := waitForLaunches(t, launchPath, 1); len(launches) != 1 {
		t.Fatalf("shutdown manager restarted its service: %#v", launches)
	}
	events, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "worker"), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(events), `"type":"exited"`) || strings.Contains(string(events), `"type":"start_failed"`) {
		t.Fatalf("graceful shutdown was recorded as an unexpected failure: %s", events)
	}
	if !strings.Contains(string(events), `"type":"started"`) {
		t.Fatalf("shutdown test did not observe a persisted start event: %s", events)
	}
}
