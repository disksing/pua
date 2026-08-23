package serve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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

func TestServiceManagerFailureSourcesShareBackoffPolicy(t *testing.T) {
	type failureSource struct {
		name      string
		eventType string
		lastError string
		writesPID bool
		configure func(root, pidPath string) ServiceConfig
	}
	sources := []failureSource{
		{
			name:      "start",
			eventType: "start_failed",
			lastError: "missing-service-command",
			configure: func(root, _ string) ServiceConfig {
				return ServiceConfig{Command: []string{filepath.Join(root, "missing-service-command")}}
			},
		},
		{
			name:      "readiness",
			eventType: "readiness_failed",
			lastError: "readiness failed",
			writesPID: true,
			configure: func(_, pidPath string) ServiceConfig {
				return ServiceConfig{
					Command: []string{"/bin/sh", "-c", "printf '%s\\n' \"$$\" > " + shellQuote(pidPath) + "; exec sleep 30"},
					Readiness: &ServiceReadinessConfig{
						Command:  []string{"/bin/sh", "-c", "while ! test -s " + shellQuote(pidPath) + "; do sleep 0.01; done; exit 23"},
						Interval: 10 * time.Millisecond,
						Timeout:  time.Second,
					},
				}
			},
		},
		{
			name:      "unexpected exit",
			eventType: "exited",
			lastError: "exit status 17",
			writesPID: true,
			configure: func(_, pidPath string) ServiceConfig {
				return ServiceConfig{Command: []string{"/bin/sh", "-c", "printf '%s\\n' \"$$\" > " + shellQuote(pidPath) + "; exit 17"}}
			},
		},
	}
	thresholds := []struct {
		name      string
		seed      int
		wantCount int
		wantDelay time.Duration
		wantState ServiceState
		attention bool
	}{
		{name: "below attention threshold", seed: 0, wantCount: 1, wantDelay: 100 * time.Millisecond, wantState: ServiceStateBackoff},
		{name: "at attention threshold", seed: 4, wantCount: 5, wantDelay: 1600 * time.Millisecond, wantState: ServiceStateAttentionRequired, attention: true},
	}

	for _, source := range sources {
		source := source
		for _, threshold := range thresholds {
			threshold := threshold
			t.Run(source.name+"/"+threshold.name, func(t *testing.T) {
				root := t.TempDir()
				pidPath := filepath.Join(root, "service.pid")
				cleanupPath := filepath.Join(root, "cleanup.log")
				now := time.Date(2026, time.August, 24, 4, 0, 0, 0, time.UTC)
				cfg := source.configure(root, pidPath)
				cfg.SchemaVersion = serviceSchemaVersion
				cfg.ID = "worker"
				cfg.Enabled = true
				cfg.Restart = ServiceRestartConfig{
					InitialDelay: 100 * time.Millisecond,
					Multiplier:   2,
					MaxDelay:     10 * time.Second,
					ResetAfter:   time.Minute,
				}
				cfg.Cleanup = &ServiceCleanupConfig{
					Command: []string{"/bin/sh", "-c", "printf 'cleanup\\n' >> " + shellQuote(cleanupPath)},
					Timeout: time.Second,
				}
				writeTestService(t, root, cfg)

				manager, err := NewServiceManager(root, ServiceManagerOptions{Now: func() time.Time { return now }})
				if err != nil {
					t.Fatal(err)
				}
				stopProcessTestManager(t, &manager)
				manager.mu.Lock()
				manager.runtimes["worker"].status.FailureCount = threshold.seed
				manager.mu.Unlock()
				if err := manager.Start(context.Background()); err != nil {
					t.Fatal(err)
				}

				status := waitForServiceState(t, manager, "worker", threshold.wantState)
				wantRetry := now.Add(threshold.wantDelay).Format(time.RFC3339Nano)
				if status.FailureCount != threshold.wantCount || status.AttentionRequired != threshold.attention || status.NextRetryAt != wantRetry {
					t.Fatalf("failure policy status = %#v, want count=%d attention=%v retry=%q", status, threshold.wantCount, threshold.attention, wantRetry)
				}
				if !strings.Contains(status.LastError, source.lastError) {
					t.Fatalf("last error = %q, want source detail %q", status.LastError, source.lastError)
				}
				persisted := readPersistedServiceStatus(t, root, "worker")
				if persisted.State != status.State || persisted.FailureCount != status.FailureCount || persisted.AttentionRequired != status.AttentionRequired || persisted.NextRetryAt != status.NextRetryAt {
					t.Fatalf("persisted failure policy = %#v, want runtime %#v", persisted, status)
				}
				cleanup, err := os.ReadFile(cleanupPath)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := string(cleanup), "cleanup\n"; got != want {
					t.Fatalf("cleanup output = %q, want %q", got, want)
				}
				events, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "worker"), "events.jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(events), `"type":"`+source.eventType+`"`) {
					t.Fatalf("%s event missing: %s", source.eventType, events)
				}
				for _, other := range []string{"start_failed", "readiness_failed", "exited"} {
					if other != source.eventType && strings.Contains(string(events), `"type":"`+other+`"`) {
						t.Fatalf("failure source emitted %s instead of only %s: %s", other, source.eventType, events)
					}
				}
				if source.writesPID {
					data, err := os.ReadFile(pidPath)
					if err != nil {
						t.Fatal(err)
					}
					pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
					if err != nil {
						t.Fatal(err)
					}
					waitForProcessGone(t, pid)
				}
			})
		}
	}
}

func TestServiceManagerUnexpectedExitResetsFailuresAfterStableRun(t *testing.T) {
	root := t.TempDir()
	releasePath := filepath.Join(root, "release")
	now := time.Date(2026, time.August, 24, 5, 0, 0, 0, time.UTC)
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c",
			"while ! test -f " + shellQuote(releasePath) + "; do sleep 0.01; done; exit 19"},
		Restart: ServiceRestartConfig{
			InitialDelay: 100 * time.Millisecond,
			Multiplier:   2,
			MaxDelay:     10 * time.Second,
			ResetAfter:   time.Minute,
		},
	})

	manager, err := NewServiceManager(root, ServiceManagerOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	manager.mu.Lock()
	manager.runtimes["worker"].status.FailureCount = 4
	manager.mu.Unlock()
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status, err := manager.Show("worker"); err != nil || status.State != ServiceStateReady {
		t.Fatalf("service did not enter its stable run: status=%#v err=%v", status, err)
	}

	now = now.Add(2 * time.Minute)
	if err := os.WriteFile(releasePath, []byte("exit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status := waitForServiceState(t, manager, "worker", ServiceStateBackoff)
	wantRetry := now.Add(100 * time.Millisecond).Format(time.RFC3339Nano)
	if status.FailureCount != 1 || status.AttentionRequired || status.NextRetryAt != wantRetry {
		t.Fatalf("stable-run reset status = %#v, want count=1 attention=false retry=%q", status, wantRetry)
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
