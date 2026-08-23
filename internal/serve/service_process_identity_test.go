package serve

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func unsupportedTestServiceProcessPlatform(groupPresent bool, signals *[]syscall.Signal) *serviceProcessPlatform {
	return &serviceProcessPlatform{
		identityInspectionAvailable: false,
		identityMarkerRequired:      false,
		processGroupPresent:         func(int) (bool, error) { return groupPresent, nil },
		processPresent:              func(int) (bool, error) { return true, nil },
		readProcessIdentity: func(int) (serviceProcessIdentity, error) {
			return serviceProcessIdentity{}, errProcessIdentityUnavailable
		},
		readProcessGroupMembers: func(int) ([]serviceProcessIdentity, error) { return nil, errProcessIdentityUnavailable },
		processGroupMemberMatches: func(serviceProcessIdentity, string, string) (bool, error) {
			return false, errProcessIdentityUnavailable
		},
		signalProcessGroup: func(_ int, signal syscall.Signal) error {
			*signals = append(*signals, signal)
			return nil
		},
	}
}

func TestReconstructedServiceProcessGroupRequiresIdentityInspection(t *testing.T) {
	var signals []syscall.Signal
	platform := unsupportedTestServiceProcessPlatform(true, &signals)
	err := terminateOwnedServiceProcessGroup(context.Background(), serviceProcessGroupIdentity{
		leaderPID:       41,
		processGroup:    41,
		startID:         "persisted-start",
		instanceToken:   "persisted-token",
		commandDigest:   "persisted-digest",
		ownership:       serviceProcessOwnershipReconstructed,
		processPlatform: platform,
	}, 0)
	if err == nil || !strings.Contains(err.Error(), "identity inspection is unavailable") {
		t.Fatalf("reconstructed ownership error = %v", err)
	}
	if len(signals) != 0 {
		t.Fatalf("reconstructed ownership sent signals: %v", signals)
	}
}

func TestCurrentManagerProcessGroupUsesContinuityWithoutIdentityInspection(t *testing.T) {
	groupPresent := true
	var signals []syscall.Signal
	platform := unsupportedTestServiceProcessPlatform(groupPresent, &signals)
	platform.processGroupPresent = func(int) (bool, error) { return groupPresent, nil }
	platform.signalProcessGroup = func(_ int, signal syscall.Signal) error {
		signals = append(signals, signal)
		groupPresent = false
		return nil
	}
	err := terminateOwnedServiceProcessGroup(context.Background(), serviceProcessGroupIdentity{
		leaderPID:       42,
		processGroup:    42,
		ownership:       serviceProcessOwnershipCurrentManager,
		processPlatform: platform,
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 1 || signals[0] != syscall.SIGTERM {
		t.Fatalf("current-manager continuity signals = %v, want [SIGTERM]", signals)
	}
}

func TestReconstructedDeadServiceProcessGroupNeedsNoIdentityProof(t *testing.T) {
	var signals []syscall.Signal
	platform := unsupportedTestServiceProcessPlatform(false, &signals)
	platform.processPresent = func(int) (bool, error) {
		t.Fatal("dead process group must not probe its leader")
		return false, nil
	}
	err := terminateOwnedServiceProcessGroup(context.Background(), serviceProcessGroupIdentity{
		leaderPID:       43,
		processGroup:    43,
		ownership:       serviceProcessOwnershipReconstructed,
		processPlatform: platform,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 0 {
		t.Fatalf("dead reconstructed group sent signals: %v", signals)
	}
}

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

func TestServiceProcessIdentityMatchesNativeChild(t *testing.T) {
	if !serviceProcessIdentityInspectionAvailable() {
		t.Skip("native service process identity is unavailable")
	}
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
	if !serviceProcessIdentityMatches(identity, pid, identity.startID, token, digest) {
		identity, identityErr := readServiceProcessIdentity(pid)
		t.Fatalf("native identity did not match for PID %d (identity %#v, error %v)", pid, identity, identityErr)
	}
	if serviceProcessIdentityMatches(identity, pid+1, identity.startID, token, digest) {
		t.Fatal("identity matched a replacement process group")
	}
	if serviceProcessIdentityMatches(identity, pid, identity.startID+"-reused", token, digest) {
		t.Fatal("identity matched a replacement PID incarnation")
	}
}

func TestServiceManagerStartsAndStopsWithCurrentManagerContinuity(t *testing.T) {
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
	platform := nativeServiceProcessPlatform()
	platform.identityInspectionAvailable = false
	platform.identityMarkerRequired = false
	identityReads := 0
	platform.readProcessIdentity = func(int) (serviceProcessIdentity, error) {
		identityReads++
		return serviceProcessIdentity{}, errProcessIdentityUnavailable
	}
	manager.processPlatform = platform
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	running, err := manager.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if running.PID <= 0 || running.ProcessGroup != running.PID || running.ProcessStartID != "" {
		t.Fatalf("unsupported-platform start status = %#v", running)
	}
	if identityReads != 0 {
		t.Fatalf("unsupported-platform launch identity reads = %d", identityReads)
	}
	if err := manager.StopService(context.Background(), "worker"); err != nil {
		t.Fatal(err)
	}
	stopped, err := manager.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if stopped.PID != 0 || stopped.ProcessGroup != 0 || stopped.State != ServiceStateStopped {
		t.Fatalf("unsupported-platform stop status = %#v", stopped)
	}
	if identityReads != 0 {
		t.Fatalf("current-manager stop identity reads = %d", identityReads)
	}
}

func TestServiceManagerRetainsUnsupportedReconstructedOwnership(t *testing.T) {
	root := t.TempDir()
	cfg := ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       true,
		Command:       []string{"original-service"},
	}
	writeTestService(t, root, cfg)
	status := initialServiceStatus(cfg)
	status.State = ServiceStateReady
	status.PID = 44
	status.ProcessGroup = 44
	status.ProcessStartID = "persisted-start"
	status.InstanceToken = "persisted-token"
	status.CommandDigest = serviceCommandDigest(cfg)
	if err := writeServiceJSON(filepath.Join(serviceRuntimePath(root, cfg.ID), "state.json"), status, 0o600); err != nil {
		t.Fatal(err)
	}

	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var signals []syscall.Signal
	manager.processPlatform = unsupportedTestServiceProcessPlatform(true, &signals)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	retained, err := manager.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.State != ServiceStateAttentionRequired || !retained.AttentionRequired || retained.PID != status.PID || retained.ProcessGroup != status.ProcessGroup {
		t.Fatalf("unsupported reconstructed status = %#v", retained)
	}
	if !strings.Contains(retained.LastError, "identity inspection is unavailable") {
		t.Fatalf("unsupported reconstructed error = %q", retained.LastError)
	}
	if err := manager.StartService(context.Background(), cfg.ID); err == nil {
		t.Fatal("start accepted unresolved reconstructed ownership")
	}
	replacement := cfg
	replacement.Command = []string{"replacement-service"}
	if err := manager.Apply(replacement); err == nil {
		t.Fatal("replacement accepted unresolved reconstructed ownership")
	}
	if got := manager.configs[cfg.ID].Command[0]; got != replacement.Command[0] {
		t.Fatalf("post-persist replacement command = %q, want %q", got, replacement.Command[0])
	}
	persistedReplacement, err := LoadServiceConfig(serviceConfigPath(root, cfg.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got := persistedReplacement.Command[0]; got != replacement.Command[0] {
		t.Fatalf("persisted replacement command = %q, want %q", got, replacement.Command[0])
	}
	retained, err = manager.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.State != ServiceStateAttentionRequired || !retained.AttentionRequired || retained.PID != status.PID || retained.ProcessGroup != status.ProcessGroup {
		t.Fatalf("post-persist replacement ownership = %#v", retained)
	}
	if err := manager.Remove(context.Background(), cfg.ID); err == nil {
		t.Fatal("remove accepted unresolved reconstructed ownership")
	}
	if _, err := manager.Show(cfg.ID); err != nil {
		t.Fatalf("blocked remove discarded service: %v", err)
	}
	if len(signals) != 0 {
		t.Fatalf("unsupported reconstructed ownership sent signals: %v", signals)
	}
}

func TestServiceManagerClearsDeadUnsupportedReconstructedGroup(t *testing.T) {
	root := t.TempDir()
	cfg := ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "worker",
		Enabled:       false,
		Command:       []string{"unused-service"},
	}
	writeTestService(t, root, cfg)
	status := initialServiceStatus(cfg)
	status.State = ServiceStateAttentionRequired
	status.AttentionRequired = true
	status.PID = 45
	status.ProcessGroup = 45
	status.LastError = "old ownership error"
	if err := writeServiceJSON(filepath.Join(serviceRuntimePath(root, cfg.ID), "state.json"), status, 0o600); err != nil {
		t.Fatal(err)
	}

	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var signals []syscall.Signal
	manager.processPlatform = unsupportedTestServiceProcessPlatform(false, &signals)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	cleared, err := manager.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.PID != 0 || cleared.ProcessGroup != 0 || cleared.AttentionRequired || cleared.LastError != "" || cleared.State != ServiceStateDisabled {
		t.Fatalf("dead unsupported reconstructed status = %#v", cleared)
	}
	if len(signals) != 0 {
		t.Fatalf("dead unsupported reconstructed group sent signals: %v", signals)
	}
}

func TestTerminateOwnedServiceProcessGroupForcesAfterCancellation(t *testing.T) {
	if !serviceProcessIdentityInspectionAvailable() {
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
		leaderPID:     pid,
		processGroup:  pid,
		startID:       identity.startID,
		instanceToken: token,
		commandDigest: digest,
		ownership:     serviceProcessOwnershipReconstructed,
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
