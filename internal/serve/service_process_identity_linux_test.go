//go:build linux

package serve

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestLinuxProcessIdentityRejectsEnvironmentSpoofing(t *testing.T) {
	const (
		processGroup = 41
		startID      = "12345"
		token        = "instance-token"
		digest       = "command-digest"
	)
	tests := []struct {
		name        string
		environment []string
	}{
		{name: "missing token", environment: []string{serviceCommandDigestEnvironment + "=" + digest}},
		{name: "prefix spoof", environment: []string{"X_" + serviceInstanceTokenEnvironment + "=" + token, serviceCommandDigestEnvironment + "=" + digest}},
		{name: "duplicate token", environment: []string{serviceInstanceTokenEnvironment + "=" + token, serviceInstanceTokenEnvironment + "=" + token, serviceCommandDigestEnvironment + "=" + digest}},
		{name: "wrong digest", environment: []string{serviceInstanceTokenEnvironment + "=" + token, serviceCommandDigestEnvironment + "=wrong"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity := serviceProcessIdentity{command: "/bin/service", environment: test.environment, processGroup: processGroup, startID: startID}
			if serviceProcessIdentityMatches(identity, processGroup, startID, token, digest) {
				t.Fatal("spoofed Linux identity matched")
			}
		})
	}
}

func TestLinuxProcessGroupMembersExposeInheritedToken(t *testing.T) {
	const token = "linux-member-token"
	cmd := exec.Command("/bin/sh", "-c", "exec sleep 30")
	cmd.Env = append(os.Environ(), serviceInstanceTokenEnvironment+"="+token)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = terminateProcessGroup(cmd.Process.Pid, true)
		_ = cmd.Wait()
	}()

	members, err := readServiceProcessGroupMembers(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if member.pid != cmd.Process.Pid {
			continue
		}
		matches, matchErr := platformServiceProcessGroupMemberMatches(member, token, "")
		if matchErr != nil {
			t.Fatal(matchErr)
		}
		if !matches {
			t.Fatal("Linux process-group member lost its inherited launch token")
		}
		return
	}
	t.Fatalf("PID %d missing from process group members: %#v", cmd.Process.Pid, members)
}
