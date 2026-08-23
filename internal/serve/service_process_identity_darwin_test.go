//go:build darwin

package serve

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

func TestParseDarwinProcessArguments(t *testing.T) {
	data := make([]byte, 4)
	binary.NativeEndian.PutUint32(data, 3)
	data = append(data, "/bin/sh\x00\x00"...)
	data = append(data, "/bin/sh\x00-c\x00exec sleep 30\x00"...)
	data = append(data,
		serviceInstanceTokenEnvironment+"=token\x00"+
			serviceCommandDigestEnvironment+"=digest\x00PATH=/usr/bin\x00\x00"...,
	)
	arguments, environment, err := parseDarwinProcessArguments(data)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/bin/sh", "-c", "exec sleep 30"}; !slices.Equal(arguments, want) {
		t.Fatalf("arguments = %#v, want %#v", arguments, want)
	}
	if want := []string{
		serviceInstanceTokenEnvironment + "=token",
		serviceCommandDigestEnvironment + "=digest",
		"PATH=/usr/bin",
	}; !slices.Equal(environment, want) {
		t.Fatalf("environment = %#v, want %#v", environment, want)
	}
}

func TestParseDarwinProcessArgumentsFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "short", data: []byte{1, 0, 0}},
		{name: "zero arguments", data: []byte{0, 0, 0, 0}},
		{name: "missing executable terminator", data: append([]byte{1, 0, 0, 0}, "/bin/sh"...)},
		{name: "truncated arguments", data: append([]byte{2, 0, 0, 0}, "/bin/sh\x00\x00/bin/sh\x00"...)},
		{name: "malformed environment", data: append([]byte{1, 0, 0, 0}, "/bin/sh\x00\x00/bin/sh\x00NOT_ENV\x00"...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := parseDarwinProcessArguments(test.data); err == nil {
				t.Fatal("parseDarwinProcessArguments() succeeded")
			}
		})
	}
}

func TestDarwinProcessIdentityMarkerSurvivesExec(t *testing.T) {
	markerPath := filepath.Join(t.TempDir(), ".identity-token")
	marker, err := os.OpenFile(markerPath, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sleep", "30")
	cmd.ExtraFiles = []*os.File{marker}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = marker.Close()
		t.Fatal(err)
	}
	_ = marker.Close()
	defer func() {
		_ = terminateProcessGroup(cmd.Process.Pid, true)
		_ = cmd.Wait()
	}()

	matches, err := darwinProcessHasIdentityMarker(cmd.Process.Pid, markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !matches {
		t.Fatal("exec'd child lost its service identity marker")
	}
	matches, err = darwinProcessHasIdentityMarker(cmd.Process.Pid, markerPath+"-other")
	if err != nil {
		t.Fatal(err)
	}
	if matches {
		t.Fatal("service identity marker matched a different launch token path")
	}
}
