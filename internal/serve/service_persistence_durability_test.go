package serve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
)

type injectedServiceDataDirectory struct {
	syncErr    error
	closeErr   error
	syncCalls  atomic.Int32
	closeCalls atomic.Int32
}

func (d *injectedServiceDataDirectory) Sync() error {
	d.syncCalls.Add(1)
	return d.syncErr
}

func (d *injectedServiceDataDirectory) Close() error {
	d.closeCalls.Add(1)
	return d.closeErr
}

func TestSyncServiceDataDirectoryPropagatesFilesystemFailures(t *testing.T) {
	openErr := errors.New("injected directory open failure")
	if err := syncServiceDataDirectoryWithOpen("unused", func(string) (serviceDataDirectory, error) {
		return nil, openErr
	}); !errors.Is(err, openErr) {
		t.Fatalf("open error = %v, want injected failure", err)
	}

	syncErr := errors.New("injected directory sync failure")
	closeErr := errors.New("injected directory close failure")
	directory := &injectedServiceDataDirectory{syncErr: syncErr, closeErr: closeErr}
	err := syncServiceDataDirectoryWithOpen("unused", func(string) (serviceDataDirectory, error) {
		return directory, nil
	})
	if !errors.Is(err, syncErr) || !errors.Is(err, closeErr) {
		t.Fatalf("sync error = %v, want joined sync and close failures", err)
	}
	if got := directory.syncCalls.Load(); got != 1 {
		t.Fatalf("directory sync calls = %d, want 1", got)
	}
	if got := directory.closeCalls.Load(); got != 1 {
		t.Fatalf("directory close calls = %d, want 1", got)
	}
}

func TestServiceManagerApplyRollsBackDefinitionDirectorySyncFailure(t *testing.T) {
	root := t.TempDir()
	original := servicePersistenceTestConfig("worker", "old")
	writeTestService(t, root, original)
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected definition directory sync failure")
	nativeSync := manager.definitionStore.syncDir
	var syncs atomic.Int32
	manager.definitionStore.syncDir = func(path string) error {
		if syncs.Add(1) == 1 {
			return injected
		}
		return nativeSync(path)
	}
	replacement := servicePersistenceTestConfig("worker", "new")
	if err := manager.Apply(replacement); !errors.Is(err, injected) {
		t.Fatalf("Apply error = %v, want injected sync failure", err)
	}

	if got := manager.configs[original.ID]; !reflect.DeepEqual(got, original) {
		t.Fatalf("manager config after failed Apply = %#v, want %#v", got, original)
	}
	persisted, err := LoadServiceConfig(serviceConfigPath(root, original.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted, original) {
		t.Fatalf("persisted config after failed Apply = %#v, want %#v", persisted, original)
	}
	assertNoServiceDefinitionJournal(t, root)
}

func TestServiceManagerRemoveRetainsSafeStateOnDirectorySyncFailure(t *testing.T) {
	root := t.TempDir()
	cfg := servicePersistenceTestConfig("worker", "old")
	writeTestService(t, root, cfg)
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected removal directory sync failure")
	nativeSync := manager.definitionStore.syncDir
	var syncs atomic.Int32
	manager.definitionStore.syncDir = func(path string) error {
		if syncs.Add(1) == 2 {
			return injected
		}
		return nativeSync(path)
	}
	if err := manager.Remove(context.Background(), cfg.ID); !errors.Is(err, injected) {
		t.Fatalf("Remove error = %v, want injected sync failure", err)
	}
	status, err := manager.Show(cfg.ID)
	if err != nil {
		t.Fatalf("removed service was dropped from memory after uncertain unlink: %v", err)
	}
	if status.Enabled || status.State != ServiceStateDisabled {
		t.Fatalf("retained service status = %#v, want safely disabled", status)
	}
	if _, err := os.Stat(serviceConfigPath(root, cfg.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("definition after remove sync failure = %v, want current unlink visible", err)
	}
	syncsBeforeRetry := syncs.Load()
	manager.definitionStore.syncDir = func(path string) error {
		syncs.Add(1)
		return nativeSync(path)
	}
	if err := manager.Remove(context.Background(), cfg.ID); err != nil {
		t.Fatalf("Remove retry = %v", err)
	}
	if got := syncs.Load(); got != syncsBeforeRetry+1 {
		t.Fatalf("directory syncs after Remove retry = %d, want one confirmation after %d", got, syncsBeforeRetry)
	}
	if _, err := manager.Show(cfg.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Show after confirmed Remove retry = %v, want not exist", err)
	}
	reconstructed, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := reconstructed.List(); len(got) != 0 {
		t.Fatalf("reconstructed services = %#v, want removed definition authoritative", got)
	}
}

func TestRemoveDefinitionMissingNoOpDoesNotRequireDirectorySync(t *testing.T) {
	manager, err := NewServiceManager(t.TempDir(), ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("unexpected directory sync")
	manager.definitionStore.syncDir = func(string) error { return injected }
	if err := manager.removeDefinitionLocked("missing"); err != nil {
		t.Fatalf("remove missing definition = %v, want no-op success", err)
	}
}

func TestServiceManagerApplyAllRollsBackJournalFinishSyncFailure(t *testing.T) {
	root := t.TempDir()
	original := servicePersistenceTestConfig("worker", "old")
	writeTestService(t, root, original)
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected journal finish sync failure")
	nativeSync := manager.definitionTransactionStore.syncDir
	var syncs atomic.Int32
	manager.definitionTransactionStore.syncDir = func(path string) error {
		if syncs.Add(1) == 3 {
			return injected
		}
		return nativeSync(path)
	}
	if err := manager.ApplyAll([]ServiceConfig{servicePersistenceTestConfig("worker", "new")}); !errors.Is(err, injected) {
		t.Fatalf("ApplyAll error = %v, want injected sync failure", err)
	}
	persisted, err := LoadServiceConfig(serviceConfigPath(root, original.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted, original) {
		t.Fatalf("persisted config after failed ApplyAll = %#v, want %#v", persisted, original)
	}
	if got := manager.configs[original.ID]; !reflect.DeepEqual(got, original) {
		t.Fatalf("manager config after failed ApplyAll = %#v, want %#v", got, original)
	}
	assertNoServiceDefinitionJournal(t, root)
}

func TestServiceDefinitionJournalFinishRetryConfirmsMissingEntry(t *testing.T) {
	root := t.TempDir()
	original := servicePersistenceTestConfig("worker", "old")
	writeTestService(t, root, original)
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	next := serviceConfigMap([]ServiceConfig{servicePersistenceTestConfig("worker", "new")})
	if err := manager.beginServiceDefinitionTransactionLocked(next, []string{"worker"}, nil); err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected journal removal sync failure")
	nativeSync := manager.definitionTransactionStore.syncDir
	manager.definitionTransactionStore.syncDir = func(string) error { return injected }
	if err := manager.finishServiceDefinitionTransactionLocked(); !errors.Is(err, injected) {
		t.Fatalf("first finish error = %v, want injected sync failure", err)
	}
	if _, err := os.Stat(serviceDefinitionTransactionPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal after failed finish sync = %v, want current unlink visible", err)
	}
	var confirmations atomic.Int32
	manager.definitionTransactionStore.syncDir = func(path string) error {
		confirmations.Add(1)
		return nativeSync(path)
	}
	if err := manager.finishServiceDefinitionTransactionLocked(); err != nil {
		t.Fatalf("finish retry = %v", err)
	}
	if got := confirmations.Load(); got != 1 {
		t.Fatalf("finish retry directory confirmations = %d, want 1", got)
	}
}

func TestServiceRuntimeStatePropagatesDirectorySyncFailure(t *testing.T) {
	root := t.TempDir()
	cfg := servicePersistenceTestConfig("worker", "old")
	writeTestService(t, root, cfg)
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected runtime state directory sync failure")
	manager.runtimeStateStore.syncDir = func(string) error { return injected }

	manager.mu.Lock()
	err = manager.persistStatusLocked(manager.runtimes[cfg.ID])
	manager.mu.Unlock()
	if !errors.Is(err, injected) {
		t.Fatalf("persist runtime state error = %v, want injected sync failure", err)
	}
	if _, err := os.Stat(filepath.Join(serviceRuntimePath(root, cfg.ID), "state.json")); err != nil {
		t.Fatalf("renamed runtime state should remain visible after sync failure: %v", err)
	}
}

func servicePersistenceTestConfig(id, generation string) ServiceConfig {
	return defaultServiceConfig(ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            id,
		Enabled:       false,
		Command:       []string{"/bin/echo"},
		Args:          []string{generation},
	})
}
