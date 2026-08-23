package serve

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestServiceProcessIdentityMatchingRejectsReuseAndSpoofing(t *testing.T) {
	const (
		processGroup = 41
		startID      = "123:456"
		token        = "instance-token"
		digest       = "command-digest"
	)
	valid := serviceProcessIdentity{
		command: "/bin/service --serve",
		environment: []string{
			"PATH=/usr/bin",
			serviceInstanceTokenEnvironment + "=" + token,
			serviceCommandDigestEnvironment + "=" + digest,
		},
		processGroup: processGroup,
		startID:      startID,
	}
	tests := []struct {
		name     string
		identity serviceProcessIdentity
		group    int
		startID  string
		token    string
		digest   string
		want     bool
	}{
		{name: "matches", identity: valid, group: processGroup, startID: startID, token: token, digest: digest, want: true},
		{name: "pid reuse changes group", identity: valid, group: processGroup + 1, startID: startID, token: token, digest: digest},
		{name: "pid reuse changes start ID", identity: valid, group: processGroup, startID: "789:012", token: token, digest: digest},
		{name: "empty command", identity: serviceProcessIdentity{environment: valid.environment, processGroup: processGroup, startID: startID}, group: processGroup, startID: startID, token: token, digest: digest},
		{name: "empty token", identity: valid, group: processGroup, startID: startID, digest: digest},
		{name: "empty digest", identity: valid, group: processGroup, startID: startID, token: token},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := serviceProcessIdentityMatches(test.identity, test.group, test.startID, test.token, test.digest); got != test.want {
				t.Fatalf("serviceProcessIdentityMatches() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestProcessIdentityMatchesNativeChild(t *testing.T) {
	const token = "native-instance-token"
	const digest = "native-command-digest"
	cmd := exec.Command("/bin/sh", "-c", "exec sleep 30")
	cmd.Env = append(os.Environ(),
		serviceInstanceTokenEnvironment+"="+token,
		serviceCommandDigestEnvironment+"="+digest,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = terminateProcessGroup(pid, true)
		_ = cmd.Wait()
	}()

	identity, err := readServiceProcessIdentity(pid)
	if err != nil {
		t.Fatal(err)
	}
	if !processIdentityMatches(pid, pid, identity.startID, token, digest) {
		identity, identityErr := readServiceProcessIdentity(pid)
		t.Fatalf("native identity did not match for PID %d (identity %#v, error %v)", pid, identity, identityErr)
	}
	if processIdentityMatches(pid, pid+1, identity.startID, token, digest) {
		t.Fatal("identity matched a replacement process group")
	}
	if processIdentityMatches(pid, pid, identity.startID+"-reused", token, digest) {
		t.Fatal("identity matched a replacement PID incarnation")
	}
}

func TestTerminateOwnedServiceProcessGroupForcesAfterCancellation(t *testing.T) {
	if !serviceProcessIdentityRequired() {
		t.Skip("native service process identity is unavailable")
	}
	const token = "cancelled-instance-token"
	const digest = "cancelled-command-digest"
	readyPath := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; printf 'ready\\n' > "+shellQuote(readyPath)+"; exec sleep 30")
	cmd.Env = append(os.Environ(),
		serviceInstanceTokenEnvironment+"="+token,
		serviceCommandDigestEnvironment+"="+digest,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	exit := make(chan serviceProcessExit, 1)
	go func() { exit <- serviceProcessExit{err: cmd.Wait()} }()
	waited := false
	t.Cleanup(func() {
		if waited {
			return
		}
		_ = terminateProcessGroup(pid, true)
		<-exit
	})
	waitForTestPath(t, readyPath, "ready")

	identity, err := readServiceProcessIdentity(pid)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	err = terminateOwnedServiceProcessGroup(ctx, serviceProcessGroupIdentity{
		leaderPID:            pid,
		processGroup:         pid,
		startID:              identity.startID,
		instanceToken:        token,
		commandDigest:        digest,
		verifyLeaderIdentity: true,
	}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancelled process-group termination took %s", elapsed)
	}
	if err := waitForServiceProcessLeader(exit); err != nil {
		t.Fatal(err)
	}
	waited = true
	if present, err := processGroupPresent(pid); err != nil || present {
		t.Fatalf("process group after cancelled termination = present %t, error %v", present, err)
	}
}

func TestServiceManagerReplacesVerifiedOrphanProcess(t *testing.T) {
	for _, state := range []ServiceState{ServiceStateReady, ServiceState("unknown")} {
		t.Run(string(state), func(t *testing.T) {
			testServiceManagerReplacesVerifiedOrphanProcess(t, state)
		})
	}
}

func testServiceManagerReplacesVerifiedOrphanProcess(t *testing.T, state ServiceState) {
	root := t.TempDir()
	cfg := ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "orphan",
		Enabled:       true,
		Command:       []string{"/bin/sh", "-c", "exec sleep 30"},
	}
	writeTestService(t, root, cfg)
	const orphanToken = "persisted-orphan-token"
	orphanDigest := serviceCommandDigest(cfg)
	orphan := exec.Command("/bin/sh", "-c", "exec sleep 30")
	orphan.Env = append(os.Environ(),
		serviceInstanceTokenEnvironment+"="+orphanToken,
		serviceCommandDigestEnvironment+"="+orphanDigest,
	)
	orphan.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	orphanPID := orphan.Process.Pid
	orphanExit := make(chan error, 1)
	go func() { orphanExit <- orphan.Wait() }()
	orphanWaited := false
	defer func() {
		if !orphanWaited {
			_ = terminateProcessGroup(orphanPID, true)
			<-orphanExit
		}
	}()
	orphanIdentity, err := readServiceProcessIdentity(orphanPID)
	if err != nil {
		t.Fatal(err)
	}
	orphanStatus := initialServiceStatus(cfg)
	orphanStatus.State = state
	orphanStatus.PID = orphanPID
	orphanStatus.ProcessGroup = orphanPID
	orphanStatus.ProcessStartID = orphanIdentity.startID
	orphanStatus.InstanceToken = orphanToken
	orphanStatus.CommandDigest = orphanDigest
	if err := writeServiceJSON(filepath.Join(serviceRuntimePath(root, cfg.ID), "state.json"), orphanStatus, 0o600); err != nil {
		t.Fatal(err)
	}

	// Loading a manager from persisted state models a daemon restart after
	// ownership was lost without a graceful service shutdown.
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Stop(context.Background()) }()
	select {
	case <-orphanExit:
		orphanWaited = true
	case <-time.After(2 * time.Second):
		t.Fatalf("verified orphan PID %d remained after restart", orphanPID)
	}
	newStatus, err := manager.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newStatus.PID <= 0 || newStatus.PID == orphanPID {
		t.Fatalf("replacement PID = %d, orphan PID = %d", newStatus.PID, orphanPID)
	}

	if processExists(orphanPID) {
		t.Fatalf("verified orphan PID %d remained after restart", orphanPID)
	}
}

func processExists(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}
