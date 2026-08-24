package serve

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var errSimulatedServiceDefinitionCrash = errors.New("simulated service definition transaction crash")

func TestValidateServicesSerializesDefinitionCommitAndRollback(t *testing.T) {
	root := t.TempDir()
	oldConfigs := serviceDefinitionGenerationConfigs(root, "old")
	for index := range oldConfigs {
		oldConfigs[index].Enabled = false
		writeTestService(t, root, oldConfigs[index])
	}
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	next := serviceDefinitionGenerationConfigs(root, "new")
	for index := range next {
		next[index].Enabled = false
	}

	targetPaused := make(chan struct{})
	releaseTarget := make(chan struct{})
	rollbackPaused := make(chan struct{})
	releaseRollback := make(chan struct{})
	var journalSyncs atomic.Int32
	var definitionSyncs atomic.Int32
	injected := errors.New("injected definition commit failure")
	manager.definitionTransactionStore.checkpoint = func(boundary string) error {
		switch boundary {
		case "journal-sync":
			switch journalSyncs.Add(1) {
			case 1:
				close(targetPaused)
				<-releaseTarget
			case 2:
				close(rollbackPaused)
				<-releaseRollback
			}
		case "definitions-sync":
			if definitionSyncs.Add(1) == 1 {
				return injected
			}
		}
		return nil
	}

	applyResult := make(chan error, 1)
	go func() { applyResult <- manager.ApplyAll(next) }()
	waitForServiceDefinitionTestSignal(t, targetPaused, "target journal")
	targetJournal := readOptionalServiceTransactionFile(t, serviceDefinitionTransactionPath(root))
	assertServiceDefinitionFilesGeneration(t, root, "old")

	validationResult := make(chan error, 1)
	go func() { validationResult <- ValidateServices(root) }()
	assertServiceDefinitionValidationBlocked(t, validationResult, "target journal")
	if current := readOptionalServiceTransactionFile(t, serviceDefinitionTransactionPath(root)); !bytes.Equal(current, targetJournal) {
		t.Fatal("validation replaced or removed the target journal")
	}
	assertServiceDefinitionFilesGeneration(t, root, "old")
	close(releaseTarget)

	waitForServiceDefinitionTestSignal(t, rollbackPaused, "rollback journal")
	rollbackJournal := readOptionalServiceTransactionFile(t, serviceDefinitionTransactionPath(root))
	if bytes.Equal(rollbackJournal, targetJournal) {
		t.Fatal("definition failure did not replace the target journal with rollback intent")
	}
	assertServiceDefinitionFilesGeneration(t, root, "new")
	assertServiceDefinitionValidationBlocked(t, validationResult, "rollback journal")
	if current := readOptionalServiceTransactionFile(t, serviceDefinitionTransactionPath(root)); !bytes.Equal(current, rollbackJournal) {
		t.Fatal("validation replaced or removed the rollback journal")
	}
	assertServiceDefinitionFilesGeneration(t, root, "new")
	close(releaseRollback)

	if err := waitForServiceDefinitionTestResult(t, applyResult, "ApplyAll rollback"); !errors.Is(err, injected) {
		t.Fatalf("ApplyAll error = %v, want injected failure", err)
	}
	if err := waitForServiceDefinitionTestResult(t, validationResult, "validation after rollback"); err != nil {
		t.Fatalf("ValidateServices after rollback = %v", err)
	}
	assertServiceDefinitionFilesGeneration(t, root, "old")
	assertNoServiceDefinitionJournal(t, root)
}

func TestValidateServicesFailsClosedWithoutRecoveringExternalJournal(t *testing.T) {
	root := t.TempDir()
	oldConfigs := serviceDefinitionGenerationConfigs(root, "old")
	for index := range oldConfigs {
		oldConfigs[index].Enabled = false
		writeTestService(t, root, oldConfigs[index])
	}
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	snapshots := make(map[string]serviceFileSnapshot, len(oldConfigs))
	for _, cfg := range oldConfigs {
		snapshots[cfg.ID], err = snapshotServiceFile(serviceConfigPath(root, cfg.ID))
		if err != nil {
			t.Fatal(err)
		}
	}
	next := serviceDefinitionGenerationConfigs(root, "new")
	for index := range next {
		next[index].Enabled = false
	}
	if err := manager.beginServiceDefinitionTransactionLocked(serviceConfigMap(next), []string{"alpha", "bravo"}, nil); err != nil {
		t.Fatal(err)
	}
	manager.definitionTransactionStore.checkpoint = crashAtServiceDefinitionBoundary("journal-sync")
	if err := manager.beginServiceDefinitionTransactionLocked(serviceConfigMap(oldConfigs), []string{"alpha", "bravo"}, snapshots); !errors.Is(err, errSimulatedServiceDefinitionCrash) {
		t.Fatalf("rollback journal error = %v, want simulated crash", err)
	}
	manager.definitionTransactionStore.checkpoint = nil

	journalBefore := readOptionalServiceTransactionFile(t, serviceDefinitionTransactionPath(root))
	definitionsBefore := map[string][]byte{}
	for _, id := range []string{"alpha", "bravo"} {
		definitionsBefore[id] = readOptionalServiceTransactionFile(t, serviceConfigPath(root, id))
	}
	if err := ValidateServices(root); !errors.Is(err, errServiceDefinitionTransactionInProgress) {
		t.Fatalf("ValidateServices error = %v, want transaction in progress", err)
	}
	if journalAfter := readOptionalServiceTransactionFile(t, serviceDefinitionTransactionPath(root)); !bytes.Equal(journalAfter, journalBefore) {
		t.Fatal("validation mutated the external rollback journal")
	}
	for id, before := range definitionsBefore {
		if after := readOptionalServiceTransactionFile(t, serviceConfigPath(root, id)); !bytes.Equal(after, before) {
			t.Fatalf("validation mutated service definition %s", id)
		}
	}
	assertServiceDefinitionFilesGeneration(t, root, "new")

	// Manager construction is the recovery boundary. It may apply the durable
	// rollback journal after read-only validation has refused to do so.
	if _, err := NewServiceManager(root, ServiceManagerOptions{}); err != nil {
		t.Fatal(err)
	}
	assertServiceDefinitionFilesGeneration(t, root, "old")
	assertNoServiceDefinitionJournal(t, root)
}

func TestValidateServicesReadsNormalDefinitionsWithoutTransaction(t *testing.T) {
	root := t.TempDir()
	for _, cfg := range serviceDefinitionGenerationConfigs(root, "normal") {
		cfg.Enabled = false
		writeTestService(t, root, cfg)
	}
	if err := ValidateServices(root); err != nil {
		t.Fatalf("ValidateServices = %v", err)
	}
	assertNoServiceDefinitionJournal(t, root)
}

func TestServiceDefinitionTransactionRecoversEveryCommitBoundary(t *testing.T) {
	tests := []struct {
		boundary string
		want     string
	}{
		{boundary: "journal-temp", want: "old"},
		{boundary: "journal-write", want: "old"},
		{boundary: "journal-rename", want: "new"},
		{boundary: "journal-sync", want: "new"},
		{boundary: "definition-rename:alpha", want: "new"},
		{boundary: "definition-rename:bravo", want: "new"},
		{boundary: "definitions-sync", want: "new"},
	}
	for _, test := range tests {
		t.Run(test.boundary, func(t *testing.T) {
			root := t.TempDir()
			oldConfigs := serviceDefinitionGenerationConfigs(root, "old")
			for _, cfg := range oldConfigs {
				writeTestService(t, root, cfg)
			}
			manager, err := NewServiceManager(root, ServiceManagerOptions{})
			if err != nil {
				t.Fatal(err)
			}
			manager.definitionTransactionStore.checkpoint = crashAtServiceDefinitionBoundary(test.boundary)
			next := serviceConfigMap(serviceDefinitionGenerationConfigs(root, "new"))
			if err := manager.beginServiceDefinitionTransactionLocked(next, []string{"alpha", "bravo"}, nil); !errors.Is(err, errSimulatedServiceDefinitionCrash) {
				t.Fatalf("transaction error = %v, want simulated crash", err)
			}

			reconstructed, err := NewServiceManager(root, ServiceManagerOptions{})
			if err != nil {
				t.Fatal(err)
			}
			assertServiceDefinitionGeneration(t, reconstructed, test.want, 2)
			assertServiceDefinitionGenerationStarts(t, reconstructed, root, test.want)
			assertNoServiceDefinitionJournal(t, root)
		})
	}
}

func TestServiceDefinitionTransactionRecoversEveryRollbackBoundary(t *testing.T) {
	tests := []struct {
		boundary string
		want     string
	}{
		{boundary: "journal-temp", want: "new"},
		{boundary: "journal-write", want: "new"},
		{boundary: "journal-rename", want: "old"},
		{boundary: "journal-sync", want: "old"},
		{boundary: "definition-rename:alpha", want: "old"},
		{boundary: "definition-remove:bravo", want: "old"},
		{boundary: "definitions-sync", want: "old"},
	}
	for _, test := range tests {
		t.Run(test.boundary, func(t *testing.T) {
			root := t.TempDir()
			oldConfig := serviceDefinitionGenerationConfigs(root, "old")[0]
			writeTestService(t, root, oldConfig)
			manager, err := NewServiceManager(root, ServiceManagerOptions{})
			if err != nil {
				t.Fatal(err)
			}
			oldConfigs := cloneServiceConfigs(manager.configs)
			snapshots := map[string]serviceFileSnapshot{}
			for _, id := range []string{"alpha", "bravo"} {
				snapshots[id], err = snapshotServiceFile(serviceConfigPath(root, id))
				if err != nil {
					t.Fatal(err)
				}
			}
			next := serviceConfigMap(serviceDefinitionGenerationConfigs(root, "new"))
			if err := manager.beginServiceDefinitionTransactionLocked(next, []string{"alpha", "bravo"}, nil); err != nil {
				t.Fatal(err)
			}
			manager.definitionTransactionStore.checkpoint = crashAtServiceDefinitionBoundary(test.boundary)
			if err := manager.beginServiceDefinitionTransactionLocked(oldConfigs, []string{"alpha", "bravo"}, snapshots); !errors.Is(err, errSimulatedServiceDefinitionCrash) {
				t.Fatalf("rollback transaction error = %v, want simulated crash", err)
			}

			reconstructed, err := NewServiceManager(root, ServiceManagerOptions{})
			if err != nil {
				t.Fatal(err)
			}
			wantCount := 2
			if test.want == "old" {
				wantCount = 1
			}
			assertServiceDefinitionGeneration(t, reconstructed, test.want, wantCount)
			assertServiceDefinitionGenerationStarts(t, reconstructed, root, test.want)
			assertNoServiceDefinitionJournal(t, root)
		})
	}
}

func TestServiceDefinitionTransactionRecoversJournalRemovalBoundaries(t *testing.T) {
	for _, boundary := range []string{"journal-remove", "journal-remove-sync"} {
		t.Run(boundary, func(t *testing.T) {
			root := t.TempDir()
			for _, cfg := range serviceDefinitionGenerationConfigs(root, "old") {
				writeTestService(t, root, cfg)
			}
			manager, err := NewServiceManager(root, ServiceManagerOptions{})
			if err != nil {
				t.Fatal(err)
			}
			next := serviceConfigMap(serviceDefinitionGenerationConfigs(root, "new"))
			if err := manager.beginServiceDefinitionTransactionLocked(next, []string{"alpha", "bravo"}, nil); err != nil {
				t.Fatal(err)
			}
			manager.definitionTransactionStore.checkpoint = crashAtServiceDefinitionBoundary(boundary)
			if err := manager.finishServiceDefinitionTransactionLocked(); !errors.Is(err, errSimulatedServiceDefinitionCrash) {
				t.Fatalf("finish error = %v, want simulated crash", err)
			}

			reconstructed, err := NewServiceManager(root, ServiceManagerOptions{})
			if err != nil {
				t.Fatal(err)
			}
			assertServiceDefinitionGeneration(t, reconstructed, "new", 2)
			assertServiceDefinitionGenerationStarts(t, reconstructed, root, "new")
			assertNoServiceDefinitionJournal(t, root)
		})
	}
}

func TestServiceDefinitionTransactionJournalFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		mode    os.FileMode
		wantErr string
	}{
		{name: "corrupt", data: []byte(`{"schemaVersion":1`), mode: 0o600, wantErr: "unexpected EOF"},
		{name: "unknown schema", data: []byte(`{"schemaVersion":2,"collection":[],"operations":[{"id":"worker","action":"remove"}]}`), mode: 0o600, wantErr: "unsupported schema version"},
		{name: "unsafe mode", data: []byte(`{}`), mode: 0o644, wantErr: "journal mode"},
		{name: "oversize", data: make([]byte, serviceDefinitionTransactionLimit+1), mode: 0o600, wantErr: "journal exceeds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Dir(serviceDefinitionTransactionPath(root))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(serviceDefinitionTransactionPath(root), test.data, test.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := NewServiceManager(root, ServiceManagerOptions{}); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("NewServiceManager error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestServiceDefinitionTransactionRejectsSymlinkJournal(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Dir(serviceDefinitionTransactionPath(root))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "outside-journal")
		if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, serviceDefinitionTransactionPath(root)); err != nil {
			t.Fatal(err)
		}
		if _, err := NewServiceManager(root, ServiceManagerOptions{}); err == nil || !strings.Contains(err.Error(), "not a symlink") {
			t.Fatalf("NewServiceManager error = %v, want symlink rejection", err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		root := t.TempDir()
		control := filepath.Join(root, ".pua")
		outside := t.TempDir()
		if err := os.MkdirAll(control, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outside, serviceDefinitionTransactionFile), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(control, serviceConfigDir)); err != nil {
			t.Fatal(err)
		}
		if _, err := NewServiceManager(root, ServiceManagerOptions{}); err == nil || !strings.Contains(err.Error(), "service definition transaction directory") {
			t.Fatalf("NewServiceManager error = %v, want directory symlink rejection", err)
		}
	})
}

func TestServiceDefinitionTransactionJournalNeverContainsResolvedSecrets(t *testing.T) {
	const resolvedSecret = "resolved-database-password"
	root := t.TempDir()
	cfg := disabledBatchService("worker")
	cfg.Environment = map[string]ServiceEnvironment{"DATABASE_PASSWORD": {SecretName: "database-password"}}
	writeTestService(t, root, cfg)
	manager, err := NewServiceManager(root, ServiceManagerOptions{
		Resolver: ServiceSecretResolverFunc(func(string) (string, string, error) {
			return resolvedSecret, "test", nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.runtimes[cfg.ID].secretValues = []string{resolvedSecret}
	replacement := cloneServiceConfig(manager.configs[cfg.ID])
	replacement.Args = []string{"new"}
	manager.definitionTransactionStore.checkpoint = crashAtServiceDefinitionBoundary("journal-rename")
	if err := manager.beginServiceDefinitionTransactionLocked(map[string]ServiceConfig{cfg.ID: replacement}, []string{cfg.ID}, nil); !errors.Is(err, errSimulatedServiceDefinitionCrash) {
		t.Fatalf("transaction error = %v, want simulated crash", err)
	}
	journal, err := os.ReadFile(serviceDefinitionTransactionPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(journal), resolvedSecret) {
		t.Fatal("service definition journal persisted a resolved secret")
	}
	if !strings.Contains(string(journal), "database-password") {
		t.Fatal("service definition journal lost the declarative secret reference")
	}
}

func TestServiceDefinitionTransactionRollbackPreservesManualDefinition(t *testing.T) {
	root := t.TempDir()
	path := serviceConfigPath(root, "worker")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// The id is intentionally omitted: per-file loading historically derives it
	// from the filename, and rollback must preserve both that form and its mode.
	manual := []byte("{\n  \"schemaVersion\": 1,\n  \"enabled\": false,\n  \"command\": [\"/bin/true\"]\n}\n")
	if err := os.WriteFile(path, manual, 0o640); err != nil {
		t.Fatal(err)
	}
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	oldConfigs := cloneServiceConfigs(manager.configs)
	snapshot, err := snapshotServiceFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := cloneServiceConfig(manager.configs["worker"])
	replacement.Args = []string{"new"}
	if err := manager.beginServiceDefinitionTransactionLocked(map[string]ServiceConfig{"worker": replacement}, []string{"worker"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.beginServiceDefinitionTransactionLocked(oldConfigs, []string{"worker"}, map[string]serviceFileSnapshot{"worker": snapshot}); err != nil {
		t.Fatal(err)
	}
	if err := manager.finishServiceDefinitionTransactionLocked(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != string(manual) {
		t.Fatalf("restored manual definition = %q, %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("restored manual definition mode = %04o, want 0640", info.Mode().Perm())
	}
	if _, err := NewServiceManager(root, ServiceManagerOptions{}); err != nil {
		t.Fatal(err)
	}
}

func crashAtServiceDefinitionBoundary(want string) func(string) error {
	return func(boundary string) error {
		if boundary == want {
			return errSimulatedServiceDefinitionCrash
		}
		return nil
	}
}

func serviceDefinitionGenerationConfigs(root, generation string) []ServiceConfig {
	marker := filepath.Join(root, "starts")
	config := func(id string, dependencies ...string) ServiceConfig {
		return ServiceConfig{
			SchemaVersion: serviceSchemaVersion,
			ID:            id,
			Enabled:       true,
			Command: []string{"/bin/sh", "-c",
				"printf '" + id + ":" + generation + "\\n' >> " + shellQuote(marker) + "; trap 'exit 0' TERM; while :; do sleep 1; done"},
			DependsOn: dependencies,
		}
	}
	return []ServiceConfig{config("alpha"), config("bravo", "alpha")}
}

func serviceConfigMap(configs []ServiceConfig) map[string]ServiceConfig {
	result := make(map[string]ServiceConfig, len(configs))
	for _, cfg := range configs {
		result[cfg.ID] = defaultServiceConfig(cfg)
	}
	return result
}

func assertServiceDefinitionGeneration(t *testing.T, manager *ServiceManager, generation string, wantCount int) {
	t.Helper()
	if len(manager.configs) != wantCount {
		t.Fatalf("recovered services = %v, want %d %s services", serviceConfigIDs(manager.configs), wantCount, generation)
	}
	for id, cfg := range manager.configs {
		if !strings.Contains(strings.Join(cfg.Command, " "), ":"+generation+`\n`) {
			t.Fatalf("service %s recovered a mixed generation: %#v", id, cfg.Command)
		}
	}
	if len(manager.configs) == 2 {
		if got := manager.graph["bravo"]; len(got) != 1 || got[0] != "alpha" {
			t.Fatalf("recovered dependency graph = %#v", manager.graph)
		}
	}
}

func assertServiceDefinitionGenerationStarts(t *testing.T, manager *ServiceManager, root, generation string) {
	t.Helper()
	stopProcessTestManager(t, &manager)
	if err := manager.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "starts")
	wantLast := "alpha:" + generation
	if len(manager.configs) == 2 {
		wantLast = "bravo:" + generation
	}
	waitForTestPath(t, marker, wantLast)
	lines := strings.Fields(string(readOptionalServiceTransactionFile(t, marker)))
	if len(lines) != len(manager.configs) {
		t.Fatalf("service starts = %v, want one start per recovered service", lines)
	}
	for _, line := range lines {
		if !strings.HasSuffix(line, ":"+generation) {
			t.Fatalf("mixed service generation started: %v", lines)
		}
	}
}

func assertNoServiceDefinitionJournal(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Lstat(serviceDefinitionTransactionPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("service definition journal survived recovery: %v", err)
	}
}

func waitForServiceDefinitionTestSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitForServiceDefinitionTestResult(t *testing.T, result <-chan error, description string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
		return nil
	}
}

func assertServiceDefinitionValidationBlocked(t *testing.T, result <-chan error, description string) {
	t.Helper()
	select {
	case err := <-result:
		t.Fatalf("ValidateServices returned during %s: %v", description, err)
	case <-time.After(100 * time.Millisecond):
	}
}

func assertServiceDefinitionFilesGeneration(t *testing.T, root, generation string) {
	t.Helper()
	reader := &ServiceManager{root: root}
	configs, err := reader.readServiceDefinitionsLocked()
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 2 {
		t.Fatalf("service definition count = %d, want 2", len(configs))
	}
	for id, cfg := range configs {
		if !strings.Contains(strings.Join(cfg.Command, " "), ":"+generation+`\n`) {
			t.Fatalf("service %s has mixed definition generation: %#v", id, cfg.Command)
		}
	}
}

func serviceConfigIDs(configs map[string]ServiceConfig) []string {
	ids := make([]string, 0, len(configs))
	for id := range configs {
		ids = append(ids, id)
	}
	return ids
}
