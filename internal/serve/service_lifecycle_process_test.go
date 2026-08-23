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
			t.Fatalf("service %s status = %#v, want state %q", id, status, want)
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

func startTermExitingServiceWithTermIgnoringChild(t *testing.T, shutdown bool) {
	t.Helper()
	root := t.TempDir()
	childPIDPath := filepath.Join(root, "child.pid")
	cleanupPath := filepath.Join(root, "cleanup")
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c",
			"trap 'exit 0' TERM; " +
				"(trap '' TERM; exec sleep 30) </dev/null >/dev/null 2>&1 & " +
				"printf '%s\\n' \"$!\" > " + shellQuote(childPIDPath) + "; " +
				"while :; do sleep 1; done"},
		Cleanup: &ServiceCleanupConfig{
			Command: []string{"/bin/sh", "-c", "printf 'cleaned\\n' > " + shellQuote(cleanupPath)},
			Timeout: time.Second,
		},
	})

	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manager.processTerminationGrace = 100 * time.Millisecond
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	running := waitForServiceState(t, manager, "worker", ServiceStateReady)
	childPIDs := waitForLaunches(t, childPIDPath, 1)
	childPID, err := strconv.Atoi(childPIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	childIdentity, err := readServiceProcessIdentity(childPID)
	if err != nil {
		t.Fatal(err)
	}
	if childIdentity.processGroup != running.ProcessGroup {
		t.Fatalf("child process group = %d, want service group %d", childIdentity.processGroup, running.ProcessGroup)
	}
	t.Cleanup(func() { _ = terminateProcessGroup(running.ProcessGroup, true) })

	started := time.Now()
	if shutdown {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err = manager.Stop(ctx)
	} else {
		err = manager.StopService(context.Background(), "worker")
	}
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("process-group shutdown duration = %s, want bounded graceful escalation", elapsed)
	}
	manager = nil
	waitForProcessGone(t, running.PID)
	waitForProcessGone(t, childPID)

	status := readPersistedServiceStatus(t, root, "worker")
	if status.State != ServiceStateStopped || status.PID != 0 || status.ProcessGroup != 0 || status.AttentionRequired {
		t.Fatalf("stopped process-group status = %#v", status)
	}
	if shutdown && status.ManualStop {
		t.Fatalf("manager shutdown persisted manual-stop intent: %#v", status)
	}
	if !shutdown && !status.ManualStop {
		t.Fatalf("manual stop lost manual-stop intent: %#v", status)
	}
	if !status.Cleanup.Succeeded || status.Cleanup.Attempts != 1 {
		t.Fatalf("cleanup status = %#v, want one successful cleanup", status.Cleanup)
	}
	cleanup, err := os.ReadFile(cleanupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(cleanup) != "cleaned\n" {
		t.Fatalf("cleanup output = %q", cleanup)
	}
	events, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "worker"), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(events), `"type":"exited"`) || strings.Contains(string(events), `"type":"start_failed"`) {
		t.Fatalf("graceful process-group shutdown entered restart policy: %s", events)
	}
}

func TestServiceManagerStopsEntireProcessGroupAfterLeaderExits(t *testing.T) {
	t.Run("manual stop", func(t *testing.T) {
		startTermExitingServiceWithTermIgnoringChild(t, false)
	})
	t.Run("manager shutdown", func(t *testing.T) {
		startTermExitingServiceWithTermIgnoringChild(t, true)
	})
}

func TestServiceManagerStopsAlreadyExitedLeader(t *testing.T) {
	root := t.TempDir()
	releasePath := filepath.Join(root, "release")
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command:       []string{"/bin/sh", "-c", "while test ! -f " + shellQuote(releasePath) + "; do sleep 0.01; done"},
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
	if err := os.WriteFile(releasePath, []byte("exit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForProcessGone(t, running.PID)
	if err := manager.StopService(context.Background(), "worker"); err != nil {
		t.Fatal(err)
	}
	manager = nil
	status := readPersistedServiceStatus(t, root, "worker")
	if status.State != ServiceStateStopped || status.PID != 0 || status.ProcessGroup != 0 || !status.ManualStop || status.AttentionRequired {
		t.Fatalf("already-exited stop status = %#v", status)
	}
}

func TestServiceManagerReapsDescendantsBeforeUnexpectedExitBackoff(t *testing.T) {
	if !serviceProcessIdentityRequired() {
		t.Skip("native service process identity is unavailable")
	}
	root := t.TempDir()
	childPIDPath := filepath.Join(root, "child.pid")
	launchPath := filepath.Join(root, "launches")
	now := time.Date(2026, time.August, 24, 6, 0, 0, 0, time.UTC)
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c",
			"printf '%s\\n' \"$PUA_SERVICE_INSTANCE_TOKEN\" >> " + shellQuote(launchPath) + "; " +
				"(trap '' TERM; exec sleep 30) </dev/null >/dev/null 2>&1 & " +
				"printf '%s\\n' \"$!\" > " + shellQuote(childPIDPath) + "; exit 17"},
		Restart: ServiceRestartConfig{
			InitialDelay: time.Minute,
			Multiplier:   2,
			MaxDelay:     time.Minute,
			ResetAfter:   time.Minute,
		},
	})

	manager, err := NewServiceManager(root, ServiceManagerOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	manager.processTerminationGrace = 50 * time.Millisecond
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	running, err := manager.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	childPIDs := waitForLaunches(t, childPIDPath, 1)
	childPID, err := strconv.Atoi(childPIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = terminateProcessGroup(running.ProcessGroup, true) })

	status := waitForServiceState(t, manager, "worker", ServiceStateBackoff)
	if status.ExitCode != 17 || status.FailureCount != 1 || status.PID != 0 || status.ProcessGroup != 0 {
		t.Fatalf("unexpected-exit status = %#v", status)
	}
	if processExists(childPID) {
		t.Fatalf("descendant %d remained alive after backoff was entered", childPID)
	}
	if launches := waitForLaunches(t, launchPath, 1); len(launches) != 1 {
		t.Fatalf("unexpected exit launched a replacement before backoff: %#v", launches)
	}
}

func TestServiceManagerRecoversDescendantsAfterLeaderExitAndReconstruction(t *testing.T) {
	if !serviceProcessIdentityRequired() {
		t.Skip("native service process identity is unavailable")
	}
	root := t.TempDir()
	firstLaunchPath := filepath.Join(root, "first-launch")
	childPIDPath := filepath.Join(root, "child.pid")
	launchPath := filepath.Join(root, "launches")
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c",
			"printf '%s\\n' \"$PUA_SERVICE_INSTANCE_TOKEN\" >> " + shellQuote(launchPath) + "; " +
				"if ! test -f " + shellQuote(firstLaunchPath) + "; then " +
				"printf 'first\\n' > " + shellQuote(firstLaunchPath) + "; " +
				"(trap '' TERM; exec sleep 30) </dev/null >/dev/null 2>&1 & " +
				"printf '%s\\n' \"$!\" > " + shellQuote(childPIDPath) + "; exit 17; fi; " +
				"exec sleep 30"},
	})

	original, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := original.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	running, err := original.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	childPIDs := waitForLaunches(t, childPIDPath, 1)
	childPID, err := strconv.Atoi(childPIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = terminateProcessGroup(running.ProcessGroup, true) })
	waitForProcessGone(t, running.PID)
	if !processExists(childPID) {
		t.Fatalf("residual descendant %d exited before reconstruction", childPID)
	}

	// Dropping the original manager after its leader has exited models a daemon
	// crash before the queued exit can be reconciled. Durable state still names
	// the original launch while only its descendant remains in the process group.
	original = nil
	reconstructed, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reconstructed.processTerminationGrace = 50 * time.Millisecond
	stopProcessTestManager(t, &reconstructed)
	if err := reconstructed.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	replacement := waitForServiceState(t, reconstructed, "worker", ServiceStateReady)
	if replacement.PID <= 0 || replacement.PID == running.PID || replacement.ProcessGroup == running.ProcessGroup {
		t.Fatalf("replacement status = %#v, original = %#v", replacement, running)
	}
	if processExists(childPID) {
		t.Fatalf("verified residual descendant %d survived reconstruction", childPID)
	}
	if launches := waitForLaunches(t, launchPath, 2); len(launches) != 2 || launches[0] == launches[1] {
		t.Fatalf("reconstruction launches = %#v, want distinct original and replacement tokens", launches)
	}
}

func TestServiceManagerRetainsResidualGroupWhenReconstructedIdentityMismatches(t *testing.T) {
	if !serviceProcessIdentityRequired() {
		t.Skip("native service process identity is unavailable")
	}
	root := t.TempDir()
	childPIDPath := filepath.Join(root, "child.pid")
	launchPath := filepath.Join(root, "launches")
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c",
			"printf '%s\\n' \"$PUA_SERVICE_INSTANCE_TOKEN\" >> " + shellQuote(launchPath) + "; " +
				"(trap '' TERM; exec sleep 30) </dev/null >/dev/null 2>&1 & " +
				"printf '%s\\n' \"$!\" > " + shellQuote(childPIDPath) + "; exit 17"},
	})

	original, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := original.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	running, err := original.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	childPIDs := waitForLaunches(t, childPIDPath, 1)
	childPID, err := strconv.Atoi(childPIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = terminateProcessGroup(running.ProcessGroup, true)
		waitForProcessGone(t, childPID)
	})
	waitForProcessGone(t, running.PID)

	persisted := readPersistedServiceStatus(t, root, "worker")
	persisted.InstanceToken += "-mismatch"
	if err := writeServiceJSON(filepath.Join(serviceRuntimePath(root, "worker"), "state.json"), persisted, 0o600); err != nil {
		t.Fatal(err)
	}
	original = nil
	reconstructed, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconstructed.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	retained, err := reconstructed.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if retained.State != ServiceStateAttentionRequired || !retained.AttentionRequired || retained.PID != running.PID || retained.ProcessGroup != running.ProcessGroup || retained.InstanceToken != persisted.InstanceToken {
		t.Fatalf("identity-mismatch status = %#v, want attention with retained ownership", retained)
	}
	if !processExists(childPID) {
		t.Fatal("identity mismatch signaled the residual process group")
	}
	if launches := waitForLaunches(t, launchPath, 1); len(launches) != 1 {
		t.Fatalf("identity mismatch launched a replacement: %#v", launches)
	}
}

func TestServiceManagerManualStopRecoversReconstructedResidualGroup(t *testing.T) {
	if !serviceProcessIdentityRequired() {
		t.Skip("native service process identity is unavailable")
	}
	root := t.TempDir()
	childPIDPath := filepath.Join(root, "child.pid")
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c",
			"(trap '' TERM; exec sleep 30) </dev/null >/dev/null 2>&1 & " +
				"printf '%s\\n' \"$!\" > " + shellQuote(childPIDPath) + "; exit 17"},
	})

	original, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := original.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	running, err := original.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	childPIDs := waitForLaunches(t, childPIDPath, 1)
	childPID, err := strconv.Atoi(childPIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = terminateProcessGroup(running.ProcessGroup, true) })
	waitForProcessGone(t, running.PID)
	original = nil

	reconstructed, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reconstructed.processTerminationGrace = 50 * time.Millisecond
	if err := reconstructed.StopService(context.Background(), "worker"); err != nil {
		t.Fatal(err)
	}
	if processExists(childPID) {
		t.Fatalf("manual stop left reconstructed descendant %d alive", childPID)
	}
	stopped, err := reconstructed.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != ServiceStateStopped || !stopped.ManualStop || stopped.AttentionRequired || stopped.PID != 0 || stopped.ProcessGroup != 0 {
		t.Fatalf("reconstructed manual-stop status = %#v", stopped)
	}
}

func TestServiceManagerRetainsOwnershipWhenLeaderIdentityChanges(t *testing.T) {
	if !serviceProcessIdentityRequired() {
		t.Skip("native service process identity is unavailable")
	}
	root := t.TempDir()
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command:       []string{"/bin/sh", "-c", "exec sleep 30"},
	})

	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	running := waitForServiceState(t, manager, "worker", ServiceStateReady)
	t.Cleanup(func() { _ = terminateProcessGroup(running.ProcessGroup, true) })
	manager.mu.Lock()
	manager.runtimes["worker"].status.ProcessStartID += "-reused"
	manager.mu.Unlock()

	err = manager.StopService(context.Background(), "worker")
	if err == nil || !strings.Contains(err.Error(), "leader identity changed") {
		t.Fatalf("identity-change stop error = %v", err)
	}
	retained, err := manager.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if retained.State != ServiceStateAttentionRequired || !retained.AttentionRequired || !retained.ManualStop || retained.PID != running.PID || retained.ProcessGroup != running.ProcessGroup {
		t.Fatalf("identity-change status = %#v, want attention with retained ownership", retained)
	}
	if !processExists(running.PID) {
		t.Fatal("identity mismatch signaled the potentially reused process group")
	}
	events, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "worker"), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), `"type":"stop_failed"`) {
		t.Fatalf("identity-change stop event missing: %s", events)
	}

	manager.mu.Lock()
	manager.runtimes["worker"].status.ProcessStartID = running.ProcessStartID
	manager.mu.Unlock()
	if err := manager.StopService(context.Background(), "worker"); err != nil {
		t.Fatal(err)
	}
	stopped, err := manager.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != ServiceStateStopped || stopped.AttentionRequired || stopped.PID != 0 || stopped.ProcessGroup != 0 {
		t.Fatalf("recovered stop status = %#v", stopped)
	}
	manager = nil
	waitForProcessGone(t, running.PID)
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

func TestServiceManagerChildEnvironmentIsolation(t *testing.T) {
	const resolvedSecret = "undeclared-daemon-secret"
	root := t.TempDir()
	readinessPath := filepath.Join(root, "readiness-environment")
	cleanupPath := filepath.Join(root, "cleanup-environment")
	home := filepath.Join(root, "service-home")
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("LANG", "C")
	t.Setenv("PUA_SECRET_OTHER", resolvedSecret)
	t.Setenv("PUA_RESOURCE_ID", "daemon-internal-resource")
	t.Setenv("DAEMON_DATABASE_PASSWORD", "generic-daemon-credential")

	probe := "printf 'pua=%s|internal=%s|generic=%s|mapped=%s|home=%s|lang=%s\\n' " +
		`"$PUA_SECRET_OTHER" "$PUA_RESOURCE_ID" "$DAEMON_DATABASE_PASSWORD" "$CHOSEN_TOKEN" "$HOME" "$LANG"`
	hookProbe := "printf 'pua=%s|internal=%s|generic=%s|mapped-length=%s|home=%s|lang=%s\\n' " +
		`"$PUA_SECRET_OTHER" "$PUA_RESOURCE_ID" "$DAEMON_DATABASE_PASSWORD" "${#CHOSEN_TOKEN}" "$HOME" "$LANG"`
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "environment",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c",
			probe + "; printf 'path=%s|export=%s\\n' \"$PATH\" \"$PUA_SERVICE_EXPORT_PATH\"; exec sleep 30"},
		Environment: map[string]ServiceEnvironment{
			"CHOSEN_TOKEN":            {Template: "${secret.OTHER}"},
			"LANG":                    {Literal: "POSIX"},
			"PUA_SERVICE_EXPORT_PATH": {Literal: "untrusted-export-path"},
		},
		Readiness: &ServiceReadinessConfig{
			Command:  []string{"/bin/sh", "-c", hookProbe + " > " + shellQuote(readinessPath)},
			Interval: 10 * time.Millisecond,
			Timeout:  time.Second,
		},
		Cleanup: &ServiceCleanupConfig{
			Command: []string{"/bin/sh", "-c", hookProbe + " > " + shellQuote(cleanupPath)},
			Timeout: time.Second,
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
	waitForServiceState(t, manager, "environment", ServiceStateReady)
	stdoutPath := filepath.Join(serviceRuntimePath(root, "environment"), "stdout.log")
	wantExportPath := filepath.Join(serviceRuntimePath(root, "environment"), "export.json")
	waitForTestPath(t, stdoutPath, "path=/usr/bin:/bin|export="+wantExportPath)

	stdout, err := os.ReadFile(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	wantProbe := "pua=|internal=|generic=|mapped=<redacted>|home=" + home + "|lang=POSIX"
	if !strings.Contains(string(stdout), wantProbe) {
		t.Fatalf("service environment output = %q, want %q", stdout, wantProbe)
	}
	forbiddenValues := []string{resolvedSecret, "daemon-internal-resource", "generic-daemon-credential"}
	for _, forbidden := range forbiddenValues {
		if strings.Contains(string(stdout), forbidden) {
			t.Fatalf("service stdout persisted undeclared daemon credential %q", forbidden)
		}
	}
	readiness, err := os.ReadFile(readinessPath)
	if err != nil {
		t.Fatal(err)
	}
	wantHookProbe := "pua=|internal=|generic=|mapped-length=" + strconv.Itoa(len(resolvedSecret)) + "|home=" + home + "|lang=POSIX"
	if got := strings.TrimSpace(string(readiness)); got != wantHookProbe {
		t.Fatalf("readiness environment = %q", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	manager = nil
	cleanup, err := os.ReadFile(cleanupPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(cleanup)); got != wantHookProbe {
		t.Fatalf("cleanup environment = %q", got)
	}
	for _, name := range []string{"state.json", "events.jsonl", "stdout.log", "stderr.log"} {
		data, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "environment"), name))
		if err != nil && os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range forbiddenValues {
			if strings.Contains(string(data), forbidden) {
				t.Fatalf("service runtime file %s persisted daemon credential %q", name, forbidden)
			}
		}
	}
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

func TestServiceManagerStartsTemplateDependenciesFirst(t *testing.T) {
	root := t.TempDir()
	orderPath := filepath.Join(root, "template-start-order")
	readyPath := filepath.Join(root, "template-dependency-ready")
	now := time.Date(2026, time.August, 24, 2, 0, 0, 0, time.UTC)
	restart := ServiceRestartConfig{
		InitialDelay: 100 * time.Millisecond,
		Multiplier:   2,
		MaxDelay:     time.Second,
		ResetAfter:   time.Minute,
	}
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "alpha-consumer",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c",
			"printf 'consumer:%s\\n' \"$SOURCE_URL\" >> " + shellQuote(orderPath) + "; exec sleep 30"},
		Environment: map[string]ServiceEnvironment{
			"SOURCE_URL": {Template: "${service.zulu-producer.URL}"},
		},
		Restart: restart,
	})
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "zulu-producer",
		Enabled:       true,
		Exports:       true,
		Command: []string{"/bin/sh", "-c",
			"printf 'producer\\n' >> " + shellQuote(orderPath) + "; " +
				"tmp=\"$PUA_SERVICE_EXPORT_PATH.tmp\"; " +
				"printf '%s' '{\"schemaVersion\":1,\"variables\":{\"URL\":\"ready\"}}' > \"$tmp\"; " +
				"mv \"$tmp\" \"$PUA_SERVICE_EXPORT_PATH\"; exec sleep 30"},
		Readiness: &ServiceReadinessConfig{
			Command:  []string{"/bin/sh", "-c", "test -f " + shellQuote(readyPath)},
			Interval: 10 * time.Millisecond,
			Timeout:  500 * time.Millisecond,
		},
		Restart: restart,
	})

	manager, err := NewServiceManager(root, ServiceManagerOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	consumer, err := manager.Show("alpha-consumer")
	if err != nil {
		t.Fatal(err)
	}
	if consumer.State != ServiceStateBlocked || consumer.PID != 0 {
		t.Fatalf("unready template consumer status = %#v, want blocked without a process", consumer)
	}
	if launches := waitForLaunches(t, orderPath, 1); len(launches) != 1 || launches[0] != "producer" {
		t.Fatalf("launch order before template dependency readiness = %#v, want producer only", launches)
	}

	if err := os.WriteFile(readyPath, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	launches := waitForLaunches(t, orderPath, 3)
	if got := strings.Join(launches, ","); got != "producer,producer,consumer:ready" {
		t.Fatalf("template dependency start order = %q, want producer,producer,consumer:ready", got)
	}
	consumer, err = manager.Show("alpha-consumer")
	if err != nil {
		t.Fatal(err)
	}
	if consumer.State != ServiceStateReady || consumer.PID <= 0 {
		t.Fatalf("template consumer status = %#v, want ready process", consumer)
	}
	if got, want := strings.Join(consumer.Dependencies, ","), "zulu-producer"; got != want {
		t.Fatalf("template consumer dependencies = %q, want %q", got, want)
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

func TestServiceManagerReadinessTimeoutReapsBackgroundOutputHolder(t *testing.T) {
	root := t.TempDir()
	servicePIDPath := filepath.Join(root, "service.pid")
	childPIDPath := filepath.Join(root, "readiness-child.pid")
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c",
			"printf '%s\\n' \"$$\" > " + shellQuote(servicePIDPath) + "; exec sleep 30"},
		Readiness: &ServiceReadinessConfig{
			Command: []string{"/bin/sh", "-c",
				"sleep 10 & printf '%s\\n' \"$!\" > " + shellQuote(childPIDPath)},
			Interval: time.Second,
			Timeout:  100 * time.Millisecond,
		},
	})

	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	started := time.Now()
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("readiness timeout duration = %s, want bounded completion", elapsed)
	}

	childPIDData, err := os.ReadFile(childPIDPath)
	if err != nil {
		t.Fatal(err)
	}
	servicePIDData, err := os.ReadFile(servicePIDPath)
	if err != nil {
		t.Fatal(err)
	}
	servicePID, err := strconv.Atoi(strings.TrimSpace(string(servicePIDData)))
	if err != nil {
		t.Fatal(err)
	}
	waitForProcessGone(t, servicePID)
	childPID, err := strconv.Atoi(strings.TrimSpace(string(childPIDData)))
	if err != nil {
		t.Fatal(err)
	}
	waitForProcessGone(t, childPID)
	status, err := manager.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceStateBackoff || status.Readiness.Ready || !strings.Contains(status.Readiness.LastError, "readiness failed") {
		t.Fatalf("readiness timeout status = %#v", status)
	}
	events, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "worker"), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), `"type":"readiness_failed"`) {
		t.Fatalf("readiness timeout event missing: %s", events)
	}
}

func TestServiceManagerCleanupTimeoutReapsBackgroundOutputHolder(t *testing.T) {
	root := t.TempDir()
	childPIDPath := filepath.Join(root, "cleanup-child.pid")
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command:       []string{"/bin/sh", "-c", "exec sleep 30"},
		Cleanup: &ServiceCleanupConfig{
			Command: []string{"/bin/sh", "-c",
				"sleep 10 & printf '%s\\n' \"$!\" > " + shellQuote(childPIDPath)},
			Timeout: 100 * time.Millisecond,
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
	started := time.Now()
	err = manager.StopService(context.Background(), "worker")
	if err == nil || !strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("cleanup timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("cleanup timeout duration = %s, want bounded completion", elapsed)
	}

	waitForProcessGone(t, running.PID)
	childPIDData, err := os.ReadFile(childPIDPath)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(childPIDData)))
	if err != nil {
		t.Fatal(err)
	}
	waitForProcessGone(t, childPID)
	status := readPersistedServiceStatus(t, root, "worker")
	if status.State != ServiceStateAttentionRequired || !status.AttentionRequired || status.Cleanup.Attempts != 1 || status.Cleanup.Succeeded || !strings.Contains(status.Cleanup.LastError, "cleanup failed") {
		t.Fatalf("cleanup timeout status = %#v", status)
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
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelShutdown()
	if err := manager.Stop(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	shutdownStatus := readPersistedServiceStatus(t, root, "worker")
	if shutdownStatus.State != ServiceStateStopped || !shutdownStatus.ManualStop || shutdownStatus.FailureCount != 0 || shutdownStatus.AttentionRequired || shutdownStatus.NextRetryAt != "" {
		t.Fatalf("graceful shutdown changed manual-stop intent: %#v", shutdownStatus)
	}
	shutdownEvents, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "worker"), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(shutdownEvents), `"type":"exited"`) || strings.Contains(string(shutdownEvents), `"type":"start_failed"`) {
		t.Fatalf("manual stop or graceful shutdown entered restart policy: %s", shutdownEvents)
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

func TestServiceManagerShutdownSuppressesRestartAndRestoresRunningService(t *testing.T) {
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

	restarted, err := NewServiceManager(root, ServiceManagerOptions{Now: func() time.Time { return now }})
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
	if restored.State != ServiceStateReady || restored.ManualStop || restored.PID <= 0 || restored.FailureCount != 0 || restored.AttentionRequired || restored.NextRetryAt != "" {
		t.Fatalf("reconstructed running service status = %#v", restored)
	}
	if launches := waitForLaunches(t, launchPath, 2); len(launches) != 2 || launches[0] == launches[1] {
		t.Fatalf("reconstruction did not restore the running service: %#v", launches)
	}
	restoredEvents, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "worker"), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(restoredEvents), `"type":"exited"`) || strings.Contains(string(restoredEvents), `"type":"start_failed"`) {
		t.Fatalf("shutdown or reconstruction entered restart policy: %s", restoredEvents)
	}
	if strings.Count(string(restoredEvents), `"type":"started"`) != 2 {
		t.Fatalf("reconstruction start events = %s, want two clean starts", restoredEvents)
	}
}
