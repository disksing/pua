package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/disksing/pua/internal/workspacepath"
)

const workspaceMutationLockFile = "application.lock"
const workspaceInitializationMarkerFile = "initializing.json"

func workspaceInitializationMarker(root string) string {
	return filepath.Join(workspacepath.ControlDir(root), workspaceInitializationMarkerFile)
}

// withWorkspaceMutationLock serializes mutations which allocate resource ids
// or update files that do not have a narrower file-mutation lock. The lock lives
// under the explicit Workspace root and is therefore shared by independent
// Workspace handles and processes without any package-global state.
func withWorkspaceMutationLock(root string, update func() error) error {
	return withWorkspaceFileLock(context.Background(), root, workspaceMutationLockFile, update)
}

func withWorkspaceFileLock(ctx context.Context, root, name string, update func() error) error {
	lockDir, err := workspacepath.ResolveControlDir(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(lockDir, name), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	flags := syscall.LOCK_EX
	if ctx.Done() != nil {
		flags |= syscall.LOCK_NB
	}
	for {
		if ctx.Err() != nil {
			return fmt.Errorf("waiting for %s: %w", name, ctx.Err())
		}
		err := syscall.Flock(int(lock.Fd()), flags)
		if err == nil {
			break
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := ctx.Err(); err != nil {
		return err
	}
	return update()
}
