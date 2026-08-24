package serve

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
)

const (
	serviceDefinitionTransactionSchema = 1
	serviceDefinitionTransactionFile   = ".definitions-transaction"
	serviceDefinitionTransactionLimit  = 4 << 20
)

var errServiceDefinitionTransactionInProgress = errors.New("service definition transaction is in progress; retry after it completes or restart the service manager to recover it")

type serviceDefinitionTransactionLock struct {
	mu   sync.Mutex
	refs int
}

var serviceDefinitionTransactionLocks = struct {
	sync.Mutex
	byRoot map[string]*serviceDefinitionTransactionLock
}{byRoot: map[string]*serviceDefinitionTransactionLock{}}

// acquireServiceDefinitionTransactionLock serializes an owning manager's
// complete definition transaction with read-only validation in this process.
// References are counted so validating many temporary Workspace roots does not
// leave a process-lifetime registry behind.
func acquireServiceDefinitionTransactionLock(root string) func() {
	key := filepath.Clean(root)
	if resolved, err := filepath.EvalSymlinks(key); err == nil {
		key = filepath.Clean(resolved)
	}
	serviceDefinitionTransactionLocks.Lock()
	entry := serviceDefinitionTransactionLocks.byRoot[key]
	if entry == nil {
		entry = &serviceDefinitionTransactionLock{}
		serviceDefinitionTransactionLocks.byRoot[key] = entry
	}
	entry.refs++
	serviceDefinitionTransactionLocks.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		serviceDefinitionTransactionLocks.Lock()
		entry.refs--
		if entry.refs == 0 && serviceDefinitionTransactionLocks.byRoot[key] == entry {
			delete(serviceDefinitionTransactionLocks.byRoot, key)
		}
		serviceDefinitionTransactionLocks.Unlock()
	}
}

func ensureNoServiceDefinitionTransaction(root string) error {
	_, err := os.Lstat(serviceDefinitionTransactionPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check service definition transaction: %w", err)
	}
	return errServiceDefinitionTransactionInProgress
}

type serviceDefinitionTransactionOperation struct {
	ID     string         `json:"id"`
	Action string         `json:"action"`
	Config *ServiceConfig `json:"config,omitempty"`
	Data   []byte         `json:"data,omitempty"`
	Mode   *uint32        `json:"mode,omitempty"`
}

type serviceDefinitionTransaction struct {
	SchemaVersion int                                     `json:"schemaVersion"`
	Collection    []ServiceConfig                         `json:"collection"`
	Operations    []serviceDefinitionTransactionOperation `json:"operations"`
}

// serviceDefinitionTransactionStore is deliberately small. The checkpoint is
// nil in production and lets tests model a daemon disappearing after a durable
// filesystem boundary without teaching the production protocol about tests.
type serviceDefinitionTransactionStore struct {
	rename     func(string, string) error
	remove     func(string) error
	syncDir    func(string) error
	checkpoint func(string) error
}

func defaultServiceDefinitionTransactionStore() serviceDefinitionTransactionStore {
	return serviceDefinitionTransactionStore{
		rename:  os.Rename,
		remove:  os.Remove,
		syncDir: syncServiceDefinitionDirectory,
	}
}

func serviceDefinitionTransactionPath(root string) string {
	return filepath.Join(root, ".pua", serviceConfigDir, serviceDefinitionTransactionFile)
}

func serviceDefinitionTransactionFromConfigs(configs map[string]ServiceConfig, operationIDs []string, snapshots map[string]serviceFileSnapshot) serviceDefinitionTransaction {
	ids := make([]string, 0, len(configs))
	for id := range configs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	collection := make([]ServiceConfig, 0, len(ids))
	for _, id := range ids {
		collection = append(collection, cloneServiceConfig(configs[id]))
	}

	operationIDs = append([]string(nil), operationIDs...)
	sort.Strings(operationIDs)
	operations := make([]serviceDefinitionTransactionOperation, 0, len(operationIDs))
	for _, id := range operationIDs {
		if cfg, ok := configs[id]; ok {
			cfg = cloneServiceConfig(cfg)
			operation := serviceDefinitionTransactionOperation{ID: id, Action: "write", Config: &cfg}
			if snapshot, ok := snapshots[id]; ok && snapshot.exists {
				operation.Data = append([]byte(nil), snapshot.data...)
				mode := uint32(snapshot.mode.Perm())
				operation.Mode = &mode
			}
			operations = append(operations, operation)
		} else {
			operations = append(operations, serviceDefinitionTransactionOperation{ID: id, Action: "remove"})
		}
	}
	return serviceDefinitionTransaction{
		SchemaVersion: serviceDefinitionTransactionSchema,
		Collection:    collection,
		Operations:    operations,
	}
}

func validateServiceDefinitionTransaction(root string, transaction serviceDefinitionTransaction) (map[string]ServiceConfig, error) {
	if transaction.SchemaVersion != serviceDefinitionTransactionSchema {
		return nil, fmt.Errorf("unsupported schema version %d", transaction.SchemaVersion)
	}
	if len(transaction.Operations) == 0 {
		return nil, errors.New("operations must not be empty")
	}
	configs := make(map[string]ServiceConfig, len(transaction.Collection))
	for _, raw := range transaction.Collection {
		cfg := defaultServiceConfig(raw)
		if cfg.ID == "" {
			return nil, errors.New("collection contains an empty service id")
		}
		if _, duplicate := configs[cfg.ID]; duplicate {
			return nil, fmt.Errorf("collection contains duplicate service id %q", cfg.ID)
		}
		configs[cfg.ID] = cfg
	}
	if _, err := validatedServiceDependencyGraph(root, configs); err != nil {
		return nil, fmt.Errorf("invalid collection: %w", err)
	}

	seen := make(map[string]struct{}, len(transaction.Operations))
	for _, operation := range transaction.Operations {
		if !serviceIDPattern.MatchString(operation.ID) {
			return nil, fmt.Errorf("operation has invalid service id %q", operation.ID)
		}
		if _, duplicate := seen[operation.ID]; duplicate {
			return nil, fmt.Errorf("duplicate operation for service %q", operation.ID)
		}
		seen[operation.ID] = struct{}{}
		switch operation.Action {
		case "write":
			if operation.Config == nil {
				return nil, fmt.Errorf("write operation for service %q has no config", operation.ID)
			}
			cfg := defaultServiceConfig(*operation.Config)
			if cfg.ID != operation.ID {
				return nil, fmt.Errorf("write operation for service %q contains config %q", operation.ID, cfg.ID)
			}
			collectionConfig, ok := configs[operation.ID]
			if !ok || serviceConfigDigest(collectionConfig) != serviceConfigDigest(cfg) {
				return nil, fmt.Errorf("write operation for service %q does not match the target collection", operation.ID)
			}
			if len(operation.Data) > 0 {
				var persisted ServiceConfig
				if err := decodeStrictServiceJSON(bytes.NewReader(operation.Data), &persisted); err != nil {
					return nil, fmt.Errorf("write operation for service %q has invalid declarative data: %w", operation.ID, err)
				}
				if persisted.ID == "" {
					persisted.ID = operation.ID
				}
				persisted = defaultServiceConfig(persisted)
				if persisted.ID != operation.ID || serviceConfigDigest(persisted) != serviceConfigDigest(cfg) {
					return nil, fmt.Errorf("write operation for service %q has declarative data that does not match its config", operation.ID)
				}
				if operation.Mode == nil || *operation.Mode&^uint32(0o777) != 0 {
					return nil, fmt.Errorf("write operation for service %q has invalid mode", operation.ID)
				}
			} else if operation.Mode != nil {
				return nil, fmt.Errorf("write operation for service %q has a mode without declarative data", operation.ID)
			}
		case "remove":
			if operation.Config != nil || len(operation.Data) != 0 || operation.Mode != nil {
				return nil, fmt.Errorf("remove operation for service %q contains write data", operation.ID)
			}
			if _, ok := configs[operation.ID]; ok {
				return nil, fmt.Errorf("remove operation for service %q remains in the target collection", operation.ID)
			}
		default:
			return nil, fmt.Errorf("operation for service %q has invalid action %q", operation.ID, operation.Action)
		}
	}
	return configs, nil
}

func (m *ServiceManager) recoverServiceDefinitionTransactionLocked() error {
	path := serviceDefinitionTransactionPath(m.root)
	if _, err := os.Lstat(path); err == nil {
		if err := validateServiceDefinitionTransactionDirectory(m.root, filepath.Dir(path)); err != nil {
			return fmt.Errorf("recover service definition transaction: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("recover service definition transaction: %w", err)
	}
	transaction, exists, err := readServiceDefinitionTransaction(path)
	if err != nil {
		return fmt.Errorf("recover service definition transaction: %w", err)
	}
	if !exists {
		return nil
	}
	want, err := validateServiceDefinitionTransaction(m.root, transaction)
	if err != nil {
		return fmt.Errorf("recover service definition transaction: invalid journal: %w", err)
	}
	if err := m.applyServiceDefinitionOperationsLocked(transaction.Operations); err != nil {
		return fmt.Errorf("recover service definition transaction: %w", err)
	}
	got, err := m.readServiceDefinitionsLocked()
	if err != nil {
		return fmt.Errorf("verify recovered service definition transaction: %w", err)
	}
	if !sameServiceConfigCollection(got, want) {
		return errors.New("verify recovered service definition transaction: definitions do not match the journal target collection")
	}
	if err := m.finishServiceDefinitionTransactionLocked(); err != nil {
		return fmt.Errorf("finish recovered service definition transaction: %w", err)
	}
	return nil
}

func (m *ServiceManager) beginServiceDefinitionTransactionLocked(configs map[string]ServiceConfig, operationIDs []string, snapshots map[string]serviceFileSnapshot) error {
	transaction := serviceDefinitionTransactionFromConfigs(configs, operationIDs, snapshots)
	if _, err := validateServiceDefinitionTransaction(m.root, transaction); err != nil {
		return fmt.Errorf("create service definition transaction: %w", err)
	}
	if err := m.writeServiceDefinitionTransactionLocked(transaction); err != nil {
		return err
	}
	return m.applyServiceDefinitionOperationsLocked(transaction.Operations)
}

func (m *ServiceManager) writeServiceDefinitionTransactionLocked(transaction serviceDefinitionTransaction) error {
	data, err := jsonMarshalServiceDefinitionTransaction(transaction)
	if err != nil {
		return err
	}
	if len(data) > serviceDefinitionTransactionLimit {
		return fmt.Errorf("service definition transaction exceeds %d bytes", serviceDefinitionTransactionLimit)
	}
	dir := filepath.Join(m.root, ".pua", serviceConfigDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := validateServiceDefinitionTransactionDirectory(m.root, dir); err != nil {
		return err
	}
	store := m.definitionTransactionStore
	defaults := defaultServiceDefinitionTransactionStore()
	if store.rename == nil {
		store.rename = defaults.rename
	}
	if store.syncDir == nil {
		store.syncDir = defaults.syncDir
	}
	temp, err := os.CreateTemp(dir, ".definitions-transaction-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if err := serviceDefinitionTransactionCheckpoint(store, "journal-temp"); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := serviceDefinitionTransactionCheckpoint(store, "journal-write"); err != nil {
		return err
	}
	path := serviceDefinitionTransactionPath(m.root)
	if err := store.rename(tempPath, path); err != nil {
		return err
	}
	if err := serviceDefinitionTransactionCheckpoint(store, "journal-rename"); err != nil {
		return err
	}
	if err := store.syncDir(dir); err != nil {
		return err
	}
	return serviceDefinitionTransactionCheckpoint(store, "journal-sync")
}

func jsonMarshalServiceDefinitionTransaction(transaction serviceDefinitionTransaction) ([]byte, error) {
	data, err := json.MarshalIndent(transaction, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func (m *ServiceManager) applyServiceDefinitionOperationsLocked(operations []serviceDefinitionTransactionOperation) error {
	store := m.definitionTransactionStore
	defaults := defaultServiceDefinitionTransactionStore()
	if store.syncDir == nil {
		store.syncDir = defaults.syncDir
	}
	for _, operation := range operations {
		var err error
		if operation.Action == "write" {
			if len(operation.Data) > 0 {
				path := serviceConfigPath(m.root, operation.ID)
				rename := m.definitionStore.rename
				if rename == nil {
					rename = os.Rename
				}
				err = writeServiceDataAtomic(path, operation.Data, os.FileMode(*operation.Mode), rename)
			} else {
				err = m.persistDefinitionLocked(*operation.Config)
			}
		} else {
			err = m.removeDefinitionLocked(operation.ID)
		}
		if err != nil {
			return fmt.Errorf("%s service definition %q: %w", operation.Action, operation.ID, serviceDefinitionOperationError(m.runtimes[operation.ID], err))
		}
		boundaryAction := operation.Action
		if boundaryAction == "write" {
			boundaryAction = "rename"
		}
		if err := serviceDefinitionTransactionCheckpoint(store, "definition-"+boundaryAction+":"+operation.ID); err != nil {
			return err
		}
	}
	dir := filepath.Join(m.root, ".pua", serviceConfigDir)
	if err := store.syncDir(dir); err != nil {
		return err
	}
	return serviceDefinitionTransactionCheckpoint(store, "definitions-sync")
}

func (m *ServiceManager) finishServiceDefinitionTransactionLocked() error {
	store := m.definitionTransactionStore
	defaults := defaultServiceDefinitionTransactionStore()
	if store.remove == nil {
		store.remove = defaults.remove
	}
	if store.syncDir == nil {
		store.syncDir = defaults.syncDir
	}
	path := serviceDefinitionTransactionPath(m.root)
	if err := store.remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := serviceDefinitionTransactionCheckpoint(store, "journal-remove"); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := store.syncDir(dir); err != nil {
		return err
	}
	return serviceDefinitionTransactionCheckpoint(store, "journal-remove-sync")
}

func readServiceDefinitionTransaction(path string) (serviceDefinitionTransaction, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return serviceDefinitionTransaction{}, false, nil
	}
	if err != nil {
		return serviceDefinitionTransaction{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return serviceDefinitionTransaction{}, false, errors.New("journal must be a regular file, not a symlink")
	}
	if info.Mode().Perm() != 0o600 {
		return serviceDefinitionTransaction{}, false, fmt.Errorf("journal mode is %04o, want 0600", info.Mode().Perm())
	}
	if info.Size() > serviceDefinitionTransactionLimit {
		return serviceDefinitionTransaction{}, false, fmt.Errorf("journal exceeds %d bytes", serviceDefinitionTransactionLimit)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return serviceDefinitionTransaction{}, false, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return serviceDefinitionTransaction{}, false, err
	}
	if !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() {
		return serviceDefinitionTransaction{}, false, errors.New("journal changed while it was opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, serviceDefinitionTransactionLimit+1))
	if err != nil {
		return serviceDefinitionTransaction{}, false, err
	}
	if len(data) > serviceDefinitionTransactionLimit {
		return serviceDefinitionTransaction{}, false, fmt.Errorf("journal exceeds %d bytes", serviceDefinitionTransactionLimit)
	}
	var transaction serviceDefinitionTransaction
	if err := decodeStrictServiceJSON(bytes.NewReader(data), &transaction); err != nil {
		return serviceDefinitionTransaction{}, false, err
	}
	return transaction, true, nil
}

func validateServiceDefinitionTransactionDirectory(root, dir string) error {
	control := filepath.Join(root, ".pua")
	if !pathWithinResolved(control, dir) {
		return errors.New("service definition transaction directory escapes the workspace control directory")
	}
	for _, path := range []string{control, dir} {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("service definition transaction directory %s must not be a symlink", path)
		}
	}
	return nil
}

func syncServiceDefinitionDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

func serviceDefinitionTransactionCheckpoint(store serviceDefinitionTransactionStore, boundary string) error {
	if store.checkpoint == nil {
		return nil
	}
	return store.checkpoint(boundary)
}

func sameServiceConfigCollection(left, right map[string]ServiceConfig) bool {
	if len(left) != len(right) {
		return false
	}
	for id, leftConfig := range left {
		rightConfig, ok := right[id]
		if !ok || serviceConfigDigest(leftConfig) != serviceConfigDigest(rightConfig) {
			return false
		}
	}
	return true
}
