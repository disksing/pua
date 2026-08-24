package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	serviceLaunchCrashHelperEnvironment = "PUA_TEST_SERVICE_LAUNCH_CRASH_HELPER"
	serviceLaunchCrashRootEnvironment   = "PUA_TEST_SERVICE_LAUNCH_CRASH_ROOT"
	serviceLaunchCrashReadyEnvironment  = "PUA_TEST_SERVICE_LAUNCH_CRASH_READY"
)

func TestServiceManagerCrashBeforeLaunchRelease(t *testing.T) {
	root := t.TempDir()
	launches := filepath.Join(root, "launches")
	cleanups := filepath.Join(root, "cleanups")
	ready := filepath.Join(root, "launch-checkpoint")
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

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	helper := exec.Command(executable, "-test.run=^TestServiceManagerLaunchCrashHelper$")
	helper.Env = append(os.Environ(),
		serviceLaunchCrashHelperEnvironment+"=1",
		serviceLaunchCrashRootEnvironment+"="+root,
		serviceLaunchCrashReadyEnvironment+"="+ready,
	)
	helper.Stdout, helper.Stderr = &output, &output
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	helperDone := false
	t.Cleanup(func() {
		if !helperDone && helper.Process != nil {
			_ = helper.Process.Kill()
			_, _ = helper.Process.Wait()
		}
	})

	waitForServiceLaunchCheckpoint(t, ready, helper, &output)
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(launches); err == nil {
			t.Fatalf("service code executed before durable launch release: %q", data)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	statePath := filepath.Join(serviceRuntimePath(root, "worker"), "state.json")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read pending launch state: %v", err)
	}
	var pending persistedServiceRuntimeState
	if err := json.Unmarshal(stateData, &pending); err != nil {
		t.Fatal(err)
	}
	if !pending.LaunchPending || pending.PID <= 0 || pending.ProcessGroup != pending.PID || pending.ProcessStartID == "" {
		t.Fatalf("pending launch state = %#v, want durable exact process ownership", pending)
	}

	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := helper.Wait(); err == nil {
		t.Fatal("crash helper exited successfully after kill")
	}
	helperDone = true
	waitForProcessGone(t, pending.PID)
	if data, err := os.ReadFile(launches); err == nil {
		t.Fatalf("barrier child executed service code after supervisor crash: %q", data)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}

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
		t.Fatalf("successful launch count = %d, want exactly one", len(launches))
	}
	time.Sleep(100 * time.Millisecond)
	if launches := waitForLaunches(t, launches, 1); len(launches) != 1 {
		t.Fatalf("successful launch count = %d, want exactly one", len(launches))
	}
	if data, err := os.ReadFile(cleanups); err == nil {
		t.Fatalf("unreleased pending launch ran cleanup: %q", data)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	status, err := reconstructed.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if status.PID <= 0 || status.ProcessGroup != status.PID {
		t.Fatalf("successful service status = %#v", status)
	}
}

func TestServiceManagerLaunchCrashHelper(t *testing.T) {
	if os.Getenv(serviceLaunchCrashHelperEnvironment) != "1" {
		t.Skip("subprocess helper")
	}
	root := os.Getenv(serviceLaunchCrashRootEnvironment)
	ready := os.Getenv(serviceLaunchCrashReadyEnvironment)
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manager.beforeServiceLaunchRelease = func() {
		if err := os.WriteFile(ready, []byte("ready\n"), 0o600); err != nil {
			panic(err)
		}
		select {}
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Fatal("service launch checkpoint did not block")
}

func TestServiceManagerReapsBarrierWhenLaunchReleaseFails(t *testing.T) {
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
	stopProcessTestManager(t, &manager)
	injected := errors.New("injected launch release failure")
	startedPID := 0
	manager.beforeServiceLaunchRelease = func() {
		startedPID = manager.runtimes["worker"].status.PID
	}
	manager.releaseServiceLaunch = func(*serviceLaunchBarrier) error { return injected }

	err = manager.Start(context.Background())
	if err != nil {
		t.Fatalf("Start returned persisted release failure: %v", err)
	}
	if startedPID <= 0 {
		t.Fatalf("barrier PID = %d, want real helper", startedPID)
	}
	waitForProcessGone(t, startedPID)
	if data, err := os.ReadFile(launches); err == nil {
		t.Fatalf("service code executed after release failure: %q", data)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if cleanups := waitForLaunches(t, cleanups, 1); len(cleanups) != 1 {
		t.Fatalf("cleanup count after release failure = %d, want one", len(cleanups))
	}
	status, err := manager.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceStateBackoff || !strings.Contains(status.LastError, injected.Error()) {
		t.Fatalf("release failure status = %#v", status)
	}
	stateData, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "worker"), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateData), `"launchPending": true`) || strings.Contains(string(stateData), `"pid": `+fmt.Sprint(startedPID)) {
		t.Fatalf("release failure retained launch ownership: %s", stateData)
	}
}

func TestServiceLaunchHelperEnvironmentExcludesServiceInjection(t *testing.T) {
	serviceEnvironment := []string{
		"PATH=/service/bin",
		"LD_PRELOAD=/tmp/untrusted.so",
		"DYLD_INSERT_LIBRARIES=/tmp/untrusted.dylib",
		"TOKEN=private-value",
		serviceInstanceTokenEnvironment + "=instance-token",
		serviceCommandDigestEnvironment + "=command-digest",
	}
	got := serviceLaunchHelperEnvironment(serviceEnvironment)
	want := []string{
		serviceInstanceTokenEnvironment + "=instance-token",
		serviceCommandDigestEnvironment + "=command-digest",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("launch helper environment = %#v, want identity-only %#v", got, want)
	}
}

func waitForServiceLaunchCheckpoint(t *testing.T, ready string, helper *exec.Cmd, output *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if data, err := os.ReadFile(ready); err == nil && strings.TrimSpace(string(data)) == "ready" {
			return
		} else if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if helper.ProcessState != nil {
			t.Fatalf("launch crash helper exited before checkpoint: %s", output.String())
		}
		if time.Now().After(deadline) {
			t.Fatal(fmt.Sprintf("launch crash helper did not reach checkpoint: %s", output.String()))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
