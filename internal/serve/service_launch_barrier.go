package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

const (
	serviceLaunchBarrierMode       = "__pua_internal_service_launch_barrier_v1"
	serviceLaunchBarrierRelease    = byte(0xa7)
	serviceLaunchExecErrorMaxBytes = 4 << 10
	serviceLaunchEnvironmentMax    = 8 << 20
)

// init intercepts only a supervisor-created re-exec. The helper blocks before
// the configured executable and argv are passed directly to syscall.Exec, so
// no shell or string interpolation is introduced by the launch protocol.
func init() {
	if len(os.Args) < 7 || os.Args[1] != serviceLaunchBarrierMode {
		return
	}
	os.Exit(runServiceLaunchBarrier(os.Args[2:]))
}

func runServiceLaunchBarrier(arguments []string) int {
	if len(arguments) < 5 {
		return 125
	}
	releaseFD, err := strconv.Atoi(arguments[0])
	if err != nil || releaseFD < 3 {
		return 125
	}
	statusFD, err := strconv.Atoi(arguments[1])
	if err != nil || statusFD < 3 || statusFD == releaseFD {
		return 125
	}
	environmentFD, err := strconv.Atoi(arguments[2])
	if err != nil || environmentFD < 3 || environmentFD == releaseFD || environmentFD == statusFD {
		return 125
	}
	execPath := arguments[3]
	argv := arguments[4:]
	if execPath == "" || len(argv) == 0 || argv[0] == "" {
		return 125
	}

	release := os.NewFile(uintptr(releaseFD), "service-launch-release")
	status := os.NewFile(uintptr(statusFD), "service-launch-status")
	environmentFile := os.NewFile(uintptr(environmentFD), "service-launch-environment")
	if release == nil || status == nil || environmentFile == nil {
		return 125
	}
	// Successful exec must close this pipe so the parent can distinguish it
	// from a helper-side exec failure without parsing service output.
	syscall.CloseOnExec(statusFD)
	environmentData, environmentErr := io.ReadAll(io.LimitReader(environmentFile, serviceLaunchEnvironmentMax+1))
	_ = environmentFile.Close()
	if environmentErr != nil || len(environmentData) > serviceLaunchEnvironmentMax {
		_ = release.Close()
		_ = status.Close()
		return 125
	}
	var environment []string
	if err := json.Unmarshal(environmentData, &environment); err != nil {
		_ = release.Close()
		_ = status.Close()
		return 125
	}
	for _, entry := range environment {
		if entry == "" || !environmentEntryValid(entry) {
			_ = release.Close()
			_ = status.Close()
			return 125
		}
	}
	var authorization [1]byte
	_, readErr := io.ReadFull(release, authorization[:])
	_ = release.Close()
	if readErr != nil || authorization[0] != serviceLaunchBarrierRelease {
		_ = status.Close()
		return 125
	}
	if err := syscall.Exec(execPath, argv, environment); err != nil {
		message := []byte(fmt.Sprintf("%s: %v", execPath, err))
		if len(message) > serviceLaunchExecErrorMaxBytes {
			message = message[:serviceLaunchExecErrorMaxBytes]
		}
		_, _ = status.Write(message)
		_ = status.Close()
		return 126
	}
	return 0
}

// serviceLaunchBarrier owns the parent endpoints of a three-pipe protocol. The
// child receives only releaseRead, statusWrite, and environmentRead. Thus a
// parent crash closes the only writer that can authorize exec, and the helper
// exits on EOF. Service-controlled environment variables reach only the final
// exec, never the privileged re-exec helper itself.
type serviceLaunchBarrier struct {
	command          *exec.Cmd
	releaseRead      *os.File
	releaseWrite     *os.File
	statusRead       *os.File
	statusWrite      *os.File
	environmentRead  *os.File
	environmentWrite *os.File
	environment      []string
}

func newServiceLaunchBarrier(command []string, environment []string, extraFiles []*os.File) (*serviceLaunchBarrier, error) {
	if len(command) == 0 || command[0] == "" {
		return nil, errors.New("service command is required")
	}
	serviceCommand := exec.Command(command[0], command[1:]...)
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve service launch helper: %w", err)
	}
	releaseRead, releaseWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create service launch release pipe: %w", err)
	}
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		_ = releaseRead.Close()
		_ = releaseWrite.Close()
		return nil, fmt.Errorf("create service launch status pipe: %w", err)
	}
	environmentRead, environmentWrite, err := os.Pipe()
	if err != nil {
		_ = releaseRead.Close()
		_ = releaseWrite.Close()
		_ = statusRead.Close()
		_ = statusWrite.Close()
		return nil, fmt.Errorf("create service launch environment pipe: %w", err)
	}
	releaseFD := 3 + len(extraFiles)
	statusFD := releaseFD + 1
	environmentFD := statusFD + 1
	arguments := []string{
		serviceLaunchBarrierMode,
		strconv.Itoa(releaseFD),
		strconv.Itoa(statusFD),
		strconv.Itoa(environmentFD),
		serviceCommand.Path,
	}
	arguments = append(arguments, command...)
	launcher := exec.Command(executable, arguments...)
	launcher.ExtraFiles = append(append([]*os.File(nil), extraFiles...), releaseRead, statusWrite, environmentRead)
	launcher.Env = serviceLaunchHelperEnvironment(environment)
	return &serviceLaunchBarrier{
		command:          launcher,
		releaseRead:      releaseRead,
		releaseWrite:     releaseWrite,
		statusRead:       statusRead,
		statusWrite:      statusWrite,
		environmentRead:  environmentRead,
		environmentWrite: environmentWrite,
		environment:      append([]string(nil), environment...),
	}, nil
}

func (b *serviceLaunchBarrier) start() error {
	if b == nil || b.command == nil {
		return errors.New("service launch barrier is unavailable")
	}
	err := b.command.Start()
	_ = b.releaseRead.Close()
	_ = b.statusWrite.Close()
	_ = b.environmentRead.Close()
	b.releaseRead = nil
	b.statusWrite = nil
	b.environmentRead = nil
	if err != nil {
		_ = b.releaseWrite.Close()
		_ = b.statusRead.Close()
		_ = b.environmentWrite.Close()
		b.releaseWrite = nil
		b.statusRead = nil
		b.environmentWrite = nil
		return err
	}
	environmentData, marshalErr := json.Marshal(b.environment)
	if marshalErr == nil && len(environmentData) > serviceLaunchEnvironmentMax {
		marshalErr = fmt.Errorf("service environment exceeds %d-byte launch limit", serviceLaunchEnvironmentMax)
	}
	if marshalErr == nil {
		var written int
		written, marshalErr = b.environmentWrite.Write(environmentData)
		if marshalErr == nil && written != len(environmentData) {
			marshalErr = io.ErrShortWrite
		}
	}
	closeErr := b.environmentWrite.Close()
	b.environmentWrite = nil
	if marshalErr == nil {
		marshalErr = closeErr
	}
	if marshalErr != nil {
		_ = b.releaseWrite.Close()
		_ = b.statusRead.Close()
		b.releaseWrite = nil
		b.statusRead = nil
		_ = terminateProcessGroup(b.command.Process.Pid, true)
		_ = b.command.Wait()
		return fmt.Errorf("transfer service launch environment: %w", marshalErr)
	}
	return nil
}

func (b *serviceLaunchBarrier) release() error {
	if b == nil || b.releaseWrite == nil || b.statusRead == nil {
		return errors.New("service launch barrier is unavailable")
	}
	written, writeErr := b.releaseWrite.Write([]byte{serviceLaunchBarrierRelease})
	_ = b.releaseWrite.Close()
	b.releaseWrite = nil
	if writeErr != nil || written != 1 {
		_ = b.statusRead.Close()
		b.statusRead = nil
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return fmt.Errorf("release service launch barrier: %w", writeErr)
	}
	status, readErr := io.ReadAll(io.LimitReader(b.statusRead, serviceLaunchExecErrorMaxBytes+1))
	_ = b.statusRead.Close()
	b.statusRead = nil
	if readErr != nil {
		return fmt.Errorf("confirm service command exec: %w", readErr)
	}
	if len(status) > 0 {
		if len(status) > serviceLaunchExecErrorMaxBytes {
			status = status[:serviceLaunchExecErrorMaxBytes]
		}
		return fmt.Errorf("exec service command: %s", status)
	}
	return nil
}

func (b *serviceLaunchBarrier) close() {
	if b == nil {
		return
	}
	for _, file := range []*os.File{b.releaseRead, b.releaseWrite, b.statusRead, b.statusWrite, b.environmentRead, b.environmentWrite} {
		if file != nil {
			_ = file.Close()
		}
	}
	b.releaseRead, b.releaseWrite, b.statusRead, b.statusWrite = nil, nil, nil, nil
	b.environmentRead, b.environmentWrite = nil, nil
	b.environment = nil
}

func serviceLaunchHelperEnvironment(environment []string) []string {
	result := make([]string, 0, 2)
	for _, name := range []string{serviceInstanceTokenEnvironment, serviceCommandDigestEnvironment} {
		if value := valueFromEnvironment(environment, name); value != "" {
			result = append(result, name+"="+value)
		}
	}
	return result
}

func environmentEntryValid(entry string) bool {
	for index, value := range entry {
		if value == '\x00' {
			return false
		}
		if value == '=' {
			return index > 0
		}
	}
	return false
}
