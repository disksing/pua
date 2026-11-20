package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/disksing/pua/internal/workspacepath"
)

// workspaceServeLockName is the canonical Workspace-scoped lock path. Only
// the OS advisory lock on this file decides which pua serve process owns
// the Workspace; the JSON metadata is diagnostic only.
const workspaceServeLockName = "serve.lock"

type workspaceLockMetadata struct {
	PID           int    `json:"pid"`
	Address       string `json:"address,omitempty"`
	ConfigPath    string `json:"configPath,omitempty"`
	WorkspacePath string `json:"workspacePath"`
	StartedAt     string `json:"startedAt"`
}

type workspaceLock struct {
	workspace string
	file      *os.File
}

// workspaceLockConflictError reports that another pua serve instance owns
// the Workspace. It carries only non-sensitive owner diagnostics.
type workspaceLockConflictError struct {
	workspace string
	owner     workspaceLockMetadata
}

func (e *workspaceLockConflictError) Error() string {
	owner := ""
	if e.owner.PID > 0 {
		owner = fmt.Sprintf(" (owned by PID %d", e.owner.PID)
		if e.owner.Address != "" {
			owner += fmt.Sprintf(", address %s", e.owner.Address)
		}
		if e.owner.ConfigPath != "" {
			owner += fmt.Sprintf(", config %s", e.owner.ConfigPath)
		}
		owner += ")"
	}
	return fmt.Sprintf("workspace %s is already managed by another pua serve instance%s; stop that instance or remove the workspace from this configuration", e.workspace, owner)
}

// workspaceLockManager tracks the Workspace ownership locks held by one
// pua serve process. Locks are keyed by canonical workspace path so
// relative paths, ".." segments, and symlinks cannot bypass ownership.
type workspaceLockManager struct {
	mu         sync.Mutex
	address    string
	configPath string
	locks      map[string]*workspaceLock
}

func newWorkspaceLockManager(address, configPath string) *workspaceLockManager {
	return &workspaceLockManager{
		address:    strings.TrimSpace(address),
		configPath: strings.TrimSpace(configPath),
		locks:      make(map[string]*workspaceLock),
	}
}

// canonicalWorkspacePath returns the absolute canonical form of a workspace
// path, resolving symlinks when the path exists. The fallback absolute path
// still deduplicates relative and ".." spellings of the same directory.
func canonicalWorkspacePath(path string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", fmt.Errorf("resolve workspace path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// acquire takes exclusive ownership of the Workspace containing the given
// path and returns its canonical path. Acquiring an already-owned Workspace
// is a no-op, so duplicate adds never stack or drop locks.
func (m *workspaceLockManager) acquire(workspacePath string) (string, error) {
	canonical, err := canonicalWorkspacePath(workspacePath)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.locks[canonical]; ok {
		return canonical, nil
	}
	lockDir, err := workspacepath.ResolveControlDir(canonical)
	if err != nil {
		return "", fmt.Errorf("resolve Workspace control directory: %w", err)
	}
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return "", fmt.Errorf("create workspace lock directory %s: %w", lockDir, err)
	}
	lockPath := filepath.Join(lockDir, workspaceServeLockName)
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", fmt.Errorf("open workspace serve lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		metadata := readWorkspaceLockMetadata(file)
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return "", &workspaceLockConflictError{workspace: canonical, owner: metadata}
		}
		return "", fmt.Errorf("lock workspace serve lock %s: %w", lockPath, err)
	}
	metadata := workspaceLockMetadata{
		PID:           os.Getpid(),
		Address:       m.address,
		ConfigPath:    m.configPath,
		WorkspacePath: canonical,
		StartedAt:     time.Now().Format(time.RFC3339),
	}
	if err := writeWorkspaceLockMetadata(file, metadata); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return "", fmt.Errorf("write workspace serve lock %s: %w", lockPath, err)
	}
	m.locks[canonical] = &workspaceLock{workspace: canonical, file: file}
	return canonical, nil
}

// owns reports whether this process currently holds the serve lock for the
// Workspace containing the given path.
func (m *workspaceLockManager) owns(workspacePath string) bool {
	canonical, err := canonicalWorkspacePath(workspacePath)
	if err != nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.locks[canonical]
	return ok
}

// release drops ownership of one Workspace, if held.
func (m *workspaceLockManager) release(workspacePath string) {
	canonical, err := canonicalWorkspacePath(workspacePath)
	if err != nil {
		return
	}
	m.mu.Lock()
	lock, ok := m.locks[canonical]
	if ok {
		delete(m.locks, canonical)
	}
	m.mu.Unlock()
	if ok {
		_ = lock.close()
	}
}

// closeAll releases every held Workspace lock. The OS also releases them
// automatically if the process exits without a clean shutdown.
func (m *workspaceLockManager) closeAll() {
	m.mu.Lock()
	locks := m.locks
	m.locks = make(map[string]*workspaceLock)
	m.mu.Unlock()
	for _, lock := range locks {
		_ = lock.close()
	}
}

func (lock *workspaceLock) close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	file := lock.file
	lock.file = nil
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func readWorkspaceLockMetadata(file *os.File) workspaceLockMetadata {
	if file == nil {
		return workspaceLockMetadata{}
	}
	if _, err := file.Seek(0, 0); err != nil {
		return workspaceLockMetadata{}
	}
	var metadata workspaceLockMetadata
	_ = json.NewDecoder(file).Decode(&metadata)
	return metadata
}

func writeWorkspaceLockMetadata(file *os.File, metadata workspaceLockMetadata) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(metadata); err != nil {
		return err
	}
	return file.Sync()
}

// ownsWorkspace reports whether this server may manage or write the given
// Workspace. A nil lock manager only occurs in isolated unit tests that
// never start pua serve; production servers always own their workspaces.
func (s *server) ownsWorkspace(workspacePath string) bool {
	if s.locks == nil {
		return true
	}
	return s.locks.owns(workspacePath)
}

// requireWorkspaceOwnership guards every management and write entry point so
// stale requests that outlive a dynamic workspace removal cannot write a
// Workspace this serve instance no longer owns.
func (s *server) requireWorkspaceOwnership(workspacePath string) error {
	if s.ownsWorkspace(workspacePath) {
		return nil
	}
	canonical, err := canonicalWorkspacePath(workspacePath)
	if err != nil {
		canonical = workspacePath
	}
	return fmt.Errorf("workspace %s is not owned by this pua serve instance; management and write operations are disabled", canonical)
}

// acquireConfiguredWorkspaceLocks takes ownership of every configured
// Workspace in stable canonical-path order. The semantics are all or
// nothing: any conflict aborts startup and releases every lock acquired in
// this round, so a serve instance never manages only part of its configured workspaces.
func (s *server) acquireConfiguredWorkspaceLocks() error {
	if s.locks == nil {
		return nil
	}
	cfg, err := s.loadConfig()
	if err != nil {
		return err
	}
	canonicalPaths := make([]string, 0, len(cfg.Workspaces))
	for _, workspace := range cfg.Workspaces {
		canonical, err := canonicalWorkspacePath(workspace.Path)
		if err != nil {
			return err
		}
		canonicalPaths = append(canonicalPaths, canonical)
	}
	sort.Strings(canonicalPaths)
	for _, canonical := range canonicalPaths {
		if _, err := s.locks.acquire(canonical); err != nil {
			s.locks.closeAll()
			return fmt.Errorf("acquire workspace ownership: %w", err)
		}
	}
	return nil
}
