package serve

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func readOptionalServiceTransactionFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func stableServiceTransactionStatus(status ServiceStatus) ServiceStatus {
	status.UpdatedAt = ""
	status.Exports.UpdatedAt = ""
	if status.Exports.Variables == nil {
		status.Exports.Variables = map[string]string{}
	}
	for index := range status.Exports.Secrets {
		status.Exports.Secrets[index].UpdatedAt = ""
	}
	return status
}

func stableServiceTransactionList(statuses []ServiceStatus) []ServiceStatus {
	result := append([]ServiceStatus(nil), statuses...)
	for index := range result {
		result[index] = stableServiceTransactionStatus(result[index])
	}
	return result
}

func serviceTransactionMarkerCount(t *testing.T, path, marker string) int {
	t.Helper()
	data := readOptionalServiceTransactionFile(t, path)
	return strings.Count(string(data), marker+"\n")
}

func serviceTransactionProcessConfig(root, id string) (ServiceConfig, string, string) {
	starts := filepath.Join(root, id+"-starts")
	stops := filepath.Join(root, id+"-stops")
	command := "trap 'printf \"stop\\n\" >> " + shellQuote(stops) + "; exit 0' TERM; " +
		"printf 'start\\n' >> " + shellQuote(starts) + "; while :; do sleep 1; done"
	return ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            id,
		Enabled:       true,
		Command:       []string{"/bin/sh", "-c", command},
		Restart: ServiceRestartConfig{
			InitialDelay: time.Second,
			Multiplier:   2,
			MaxDelay:     time.Second,
		},
	}, starts, stops
}

func TestServiceManagerEnableWriteFailureIsAtomic(t *testing.T) {
	root := t.TempDir()
	cfg, starts, _ := serviceTransactionProcessConfig(root, "worker")
	cfg.Enabled = false
	writeTestService(t, root, cfg)
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	definitionPath := serviceConfigPath(root, cfg.ID)
	eventsPath := filepath.Join(serviceRuntimePath(root, cfg.ID), "events.jsonl")
	definitionBefore := readOptionalServiceTransactionFile(t, definitionPath)
	eventsBefore := readOptionalServiceTransactionFile(t, eventsPath)
	statusBefore, err := manager.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	listBefore := manager.List()
	injected := errors.New("injected definition write failure")
	manager.definitionStore.writeJSON = func(string, any, os.FileMode, func(string, string) error, func(string) error) error {
		return injected
	}

	start := make(chan struct{})
	enableResult := make(chan error, 1)
	reconcileResult := make(chan error, 1)
	go func() {
		<-start
		enableResult <- manager.Enable(cfg.ID)
	}()
	go func() {
		<-start
		reconcileResult <- manager.Reconcile(context.Background())
	}()
	close(start)
	if err := <-enableResult; !errors.Is(err, injected) {
		t.Fatalf("Enable error = %v, want injected write failure", err)
	}
	if err := <-reconcileResult; err != nil {
		t.Fatal(err)
	}

	statusAfter, err := manager.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stableServiceTransactionStatus(statusAfter), stableServiceTransactionStatus(statusBefore); !reflect.DeepEqual(got, want) {
		t.Fatalf("status after failed Enable = %#v, want %#v", got, want)
	}
	if got, want := stableServiceTransactionList(manager.List()), stableServiceTransactionList(listBefore); !reflect.DeepEqual(got, want) {
		t.Fatalf("list after failed Enable = %#v, want %#v", got, want)
	}
	if data := readOptionalServiceTransactionFile(t, definitionPath); !bytes.Equal(data, definitionBefore) {
		t.Fatalf("definition changed after failed Enable:\n%s", data)
	}
	if data := readOptionalServiceTransactionFile(t, eventsPath); !bytes.Equal(data, eventsBefore) {
		t.Fatalf("events changed after failed Enable:\n%s", data)
	}
	if count := serviceTransactionMarkerCount(t, starts, "start"); count != 0 {
		t.Fatalf("service starts after failed Enable = %d, want 0", count)
	}

	reconstructed, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reconstructedStatus, err := reconstructed.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stableServiceTransactionStatus(reconstructedStatus), stableServiceTransactionStatus(statusAfter); !reflect.DeepEqual(got, want) {
		t.Fatalf("reconstructed status = %#v, want %#v", got, want)
	}
}

func TestServiceManagerDisableRenameFailureIsAtomic(t *testing.T) {
	root := t.TempDir()
	cfg, starts, stops := serviceTransactionProcessConfig(root, "worker")
	writeTestService(t, root, cfg)
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForTestPath(t, starts, "start")

	definitionPath := serviceConfigPath(root, cfg.ID)
	eventsPath := filepath.Join(serviceRuntimePath(root, cfg.ID), "events.jsonl")
	definitionBefore := readOptionalServiceTransactionFile(t, definitionPath)
	eventsBefore := readOptionalServiceTransactionFile(t, eventsPath)
	statusBefore, err := manager.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	listBefore := manager.List()
	injected := errors.New("injected definition rename failure")
	manager.definitionStore.rename = func(string, string) error { return injected }

	if err := manager.Disable(context.Background(), cfg.ID); !errors.Is(err, injected) {
		t.Fatalf("Disable error = %v, want injected rename failure", err)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	statusAfter, err := manager.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stableServiceTransactionStatus(statusAfter), stableServiceTransactionStatus(statusBefore); !reflect.DeepEqual(got, want) {
		t.Fatalf("status after failed Disable = %#v, want %#v", got, want)
	}
	if got, want := stableServiceTransactionList(manager.List()), stableServiceTransactionList(listBefore); !reflect.DeepEqual(got, want) {
		t.Fatalf("list after failed Disable = %#v, want %#v", got, want)
	}
	if data := readOptionalServiceTransactionFile(t, definitionPath); !bytes.Equal(data, definitionBefore) {
		t.Fatalf("definition changed after failed Disable:\n%s", data)
	}
	if data := readOptionalServiceTransactionFile(t, eventsPath); !bytes.Equal(data, eventsBefore) {
		t.Fatalf("events changed after failed Disable:\n%s", data)
	}
	if count := serviceTransactionMarkerCount(t, starts, "start"); count != 1 {
		t.Fatalf("service starts after failed Disable = %d, want 1", count)
	}
	if count := serviceTransactionMarkerCount(t, stops, "stop"); count != 0 {
		t.Fatalf("service stops after failed Disable = %d, want 0", count)
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(definitionPath), ".service-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("failed definition rename left temporary files: %v", temps)
	}

	reconstructed, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reconstructedStatus, err := reconstructed.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stableServiceTransactionStatus(reconstructedStatus), stableServiceTransactionStatus(statusAfter); !reflect.DeepEqual(got, want) {
		t.Fatalf("reconstructed status = %#v, want %#v", got, want)
	}
}

func TestServiceManagerRemoveWriteFailureHasNoLifecycleEffects(t *testing.T) {
	const secret = "private-definition-secret"
	root := t.TempDir()
	cfg, starts, stops := serviceTransactionProcessConfig(root, "worker")
	cfg.Environment = map[string]ServiceEnvironment{"TOKEN": {SecretName: "token"}}
	writeTestService(t, root, cfg)
	manager, err := NewServiceManager(root, ServiceManagerOptions{
		Resolver: EnvironmentSecretResolver{Values: map[string]string{"token": secret}},
	})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForTestPath(t, starts, "start")

	definitionPath := serviceConfigPath(root, cfg.ID)
	eventsPath := filepath.Join(serviceRuntimePath(root, cfg.ID), "events.jsonl")
	definitionBefore := readOptionalServiceTransactionFile(t, definitionPath)
	eventsBefore := readOptionalServiceTransactionFile(t, eventsPath)
	statusBefore, err := manager.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	listBefore := manager.List()
	injected := errors.New("injected definition write failure containing " + secret)
	manager.definitionStore.writeJSON = func(string, any, os.FileMode, func(string, string) error, func(string) error) error {
		return injected
	}
	var removes atomic.Int32
	manager.definitionStore.remove = func(string) error {
		removes.Add(1)
		return nil
	}

	removeErr := manager.Remove(context.Background(), cfg.ID)
	if removeErr == nil || !strings.Contains(removeErr.Error(), "injected definition write failure") {
		t.Fatalf("Remove error = %v, want injected write failure", removeErr)
	}
	if strings.Contains(removeErr.Error(), secret) || !strings.Contains(removeErr.Error(), "<redacted>") {
		t.Fatalf("Remove error did not safely redact the resolved secret: %v", removeErr)
	}
	if got := removes.Load(); got != 0 {
		t.Fatalf("definition remove calls after write failure = %d, want 0", got)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	statusAfter, err := manager.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stableServiceTransactionStatus(statusAfter), stableServiceTransactionStatus(statusBefore); !reflect.DeepEqual(got, want) {
		t.Fatalf("status after failed Remove preflight = %#v, want %#v", got, want)
	}
	if got, want := stableServiceTransactionList(manager.List()), stableServiceTransactionList(listBefore); !reflect.DeepEqual(got, want) {
		t.Fatalf("list after failed Remove preflight = %#v, want %#v", got, want)
	}
	if data := readOptionalServiceTransactionFile(t, definitionPath); !bytes.Equal(data, definitionBefore) {
		t.Fatalf("definition changed after failed Remove preflight:\n%s", data)
	}
	if data := readOptionalServiceTransactionFile(t, eventsPath); !bytes.Equal(data, eventsBefore) {
		t.Fatalf("events changed after failed Remove preflight:\n%s", data)
	}
	if count := serviceTransactionMarkerCount(t, starts, "start"); count != 1 {
		t.Fatalf("service starts after failed Remove preflight = %d, want 1", count)
	}
	if count := serviceTransactionMarkerCount(t, stops, "stop"); count != 0 {
		t.Fatalf("service stops after failed Remove preflight = %d, want 0", count)
	}
}

func TestServiceManagerRemoveFailureRetainsDurableDisabledState(t *testing.T) {
	root := t.TempDir()
	cfg, starts, stops := serviceTransactionProcessConfig(root, "worker")
	writeTestService(t, root, cfg)
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForTestPath(t, starts, "start")

	definitionPath := serviceConfigPath(root, cfg.ID)
	eventsPath := filepath.Join(serviceRuntimePath(root, cfg.ID), "events.jsonl")
	eventsBefore := readOptionalServiceTransactionFile(t, eventsPath)
	injected := errors.New("injected definition remove failure")
	removeEntered := make(chan struct{})
	releaseRemove := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRemove) }) }
	defer release()
	manager.definitionStore.remove = func(string) error {
		close(removeEntered)
		<-releaseRemove
		return injected
	}

	removeResult := make(chan error, 1)
	go func() { removeResult <- manager.Remove(context.Background(), cfg.ID) }()
	select {
	case <-removeEntered:
	case <-time.After(time.Second):
		t.Fatal("Remove did not reach the injected definition store")
	}
	if count := serviceTransactionMarkerCount(t, stops, "stop"); count != 1 {
		t.Fatalf("service stops before definition removal = %d, want 1", count)
	}
	reconcileAttempted := make(chan struct{})
	reconcileResult := make(chan error, 1)
	go func() {
		close(reconcileAttempted)
		reconcileResult <- manager.Reconcile(context.Background())
	}()
	<-reconcileAttempted
	select {
	case err := <-reconcileResult:
		t.Fatalf("concurrent Reconcile escaped the removal transaction: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	if err := <-removeResult; !errors.Is(err, injected) {
		t.Fatalf("Remove error = %v, want injected remove failure", err)
	}
	if err := <-reconcileResult; err != nil {
		t.Fatal(err)
	}

	status, err := manager.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Enabled || status.State != ServiceStateDisabled || status.ManualStop || status.PID != 0 || status.ProcessGroup != 0 || status.AttentionRequired {
		t.Fatalf("status after failed Remove = %#v, want durable disabled state", status)
	}
	list := manager.List()
	if len(list) != 1 || list[0].ID != cfg.ID || list[0].Enabled || list[0].State != ServiceStateDisabled {
		t.Fatalf("list after failed Remove = %#v", list)
	}
	persistedConfig, err := LoadServiceConfig(definitionPath)
	if err != nil {
		t.Fatal(err)
	}
	if persistedConfig.Enabled {
		t.Fatalf("definition after failed Remove remained enabled: %#v", persistedConfig)
	}
	if data := readOptionalServiceTransactionFile(t, eventsPath); !bytes.Equal(data, eventsBefore) {
		t.Fatalf("events changed after failed Remove:\n%s", data)
	}
	if count := serviceTransactionMarkerCount(t, starts, "start"); count != 1 {
		t.Fatalf("service starts after failed Remove and Reconcile = %d, want 1", count)
	}
	if count := serviceTransactionMarkerCount(t, stops, "stop"); count != 1 {
		t.Fatalf("service stops after failed Remove = %d, want 1", count)
	}
	persisted := readPersistedServiceStatus(t, root, cfg.ID)
	if persisted.Enabled || persisted.ManualStop || persisted.State != ServiceStateDisabled || persisted.PID != 0 || persisted.ProcessGroup != 0 {
		t.Fatalf("persisted status after failed Remove = %#v", persisted)
	}

	reconstructed, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconstructed.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if count := serviceTransactionMarkerCount(t, starts, "start"); count != 1 {
		t.Fatalf("service starts after reconstruction = %d, want 1", count)
	}
	reconstructedStatus, err := reconstructed.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reconstructedStatus.Enabled || reconstructedStatus.State != ServiceStateDisabled || reconstructedStatus.ManualStop {
		t.Fatalf("reconstructed status after failed Remove = %#v", reconstructedStatus)
	}
	if data := readOptionalServiceTransactionFile(t, eventsPath); !bytes.Equal(data, eventsBefore) {
		t.Fatalf("reconstruction changed events after failed Remove:\n%s", data)
	}
}

func TestServiceManagerDisableStopFailureKeepsDiskAndAttentionAligned(t *testing.T) {
	const (
		pid       = 424242
		startID   = "transaction-start"
		token     = "transaction-token"
		secret    = "private-stop-secret"
		serviceID = "worker"
	)
	root := t.TempDir()
	cfg := ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            serviceID,
		Enabled:       true,
		Command:       []string{"transaction-worker"},
	}
	writeTestService(t, root, cfg)
	status := initialServiceStatus(cfg)
	status.State = ServiceStateReady
	status.PID = pid
	status.ProcessGroup = pid
	status.ProcessStartID = startID
	status.InstanceToken = token
	status.CommandDigest = serviceCommandDigest(cfg)
	if err := writeServiceJSON(filepath.Join(serviceRuntimePath(root, serviceID), "state.json"), status, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manager.runtimes[serviceID].secretValues = []string{secret}
	manager.processPlatform = &serviceProcessPlatform{
		identityInspectionAvailable: true,
		processGroupPresent:         func(int) (bool, error) { return true, nil },
		processPresent:              func(int) (bool, error) { return true, nil },
		readProcessIdentity: func(int) (serviceProcessIdentity, error) {
			return serviceProcessIdentity{
				pid:          pid,
				command:      cfg.Command[0],
				environment:  []string{serviceInstanceTokenEnvironment + "=" + token, serviceCommandDigestEnvironment + "=" + status.CommandDigest},
				processGroup: pid,
				startID:      startID,
			}, nil
		},
		signalProcessGroup: func(int, syscall.Signal) error {
			return errors.New("injected permission failure containing " + secret)
		},
	}

	disableErr := manager.Disable(context.Background(), serviceID)
	if disableErr == nil {
		t.Fatal("Disable succeeded while the owned process group could not stop")
	}
	if strings.Contains(disableErr.Error(), secret) {
		t.Fatalf("Disable error disclosed a resolved secret: %v", disableErr)
	}
	persistedConfig, err := LoadServiceConfig(serviceConfigPath(root, serviceID))
	if err != nil {
		t.Fatal(err)
	}
	if persistedConfig.Enabled {
		t.Fatal("failed process stop rolled the durable disabled definition back to enabled")
	}
	visible, err := manager.Show(serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if visible.Enabled || visible.State != ServiceStateAttentionRequired || !visible.AttentionRequired || visible.PID != pid || visible.ProcessGroup != pid || visible.ManualStop {
		t.Fatalf("status after post-persist stop failure = %#v", visible)
	}
	if strings.Contains(visible.LastError, secret) {
		t.Fatalf("status disclosed a resolved secret: %#v", visible)
	}
	persistedStatus := readPersistedServiceStatus(t, root, serviceID)
	if persistedStatus.Enabled || persistedStatus.State != ServiceStateAttentionRequired || !persistedStatus.AttentionRequired || persistedStatus.PID != pid || persistedStatus.ProcessGroup != pid {
		t.Fatalf("persisted status after post-persist stop failure = %#v", persistedStatus)
	}
	events := readOptionalServiceTransactionFile(t, filepath.Join(serviceRuntimePath(root, serviceID), "events.jsonl"))
	if !bytes.Contains(events, []byte(`"type":"stop_failed"`)) || bytes.Contains(events, []byte(secret)) {
		t.Fatalf("unsafe stop-failure events: %s", events)
	}

	reconstructed, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reconstructedStatus, err := reconstructed.Show(serviceID)
	if err != nil {
		t.Fatal(err)
	}
	if reconstructedStatus.Enabled || reconstructedStatus.State != ServiceStateAttentionRequired || !reconstructedStatus.AttentionRequired || reconstructedStatus.PID != pid || reconstructedStatus.ProcessGroup != pid {
		t.Fatalf("reconstructed status after post-persist stop failure = %#v", reconstructedStatus)
	}
}

func TestServiceManagerRemoveRejectsBindingsBeforeStopping(t *testing.T) {
	root := t.TempDir()
	cfg, starts, stops := serviceTransactionProcessConfig(root, "producer")
	writeTestService(t, root, cfg)
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if _, err := manager.ApplyBindings(ServiceBindings{
		Variables: map[string]string{"ENDPOINT": "${service.producer.URL}"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForTestPath(t, starts, "start")
	definitionPath := serviceConfigPath(root, cfg.ID)
	eventsPath := filepath.Join(serviceRuntimePath(root, cfg.ID), "events.jsonl")
	definitionBefore := readOptionalServiceTransactionFile(t, definitionPath)
	eventsBefore := readOptionalServiceTransactionFile(t, eventsPath)
	statusBefore, err := manager.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	var removes atomic.Int32
	manager.definitionStore.remove = func(string) error {
		removes.Add(1)
		return nil
	}

	removeErr := manager.Remove(context.Background(), cfg.ID)
	if removeErr == nil || !strings.Contains(removeErr.Error(), `unknown service "producer"`) {
		t.Fatalf("Remove error = %v, want binding dependency rejection", removeErr)
	}
	if got := removes.Load(); got != 0 {
		t.Fatalf("definition remove calls after binding rejection = %d, want 0", got)
	}
	statusAfter, err := manager.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stableServiceTransactionStatus(statusAfter), stableServiceTransactionStatus(statusBefore); !reflect.DeepEqual(got, want) {
		t.Fatalf("status after binding rejection = %#v, want %#v", got, want)
	}
	if data := readOptionalServiceTransactionFile(t, definitionPath); !bytes.Equal(data, definitionBefore) {
		t.Fatalf("definition changed after binding rejection:\n%s", data)
	}
	if data := readOptionalServiceTransactionFile(t, eventsPath); !bytes.Equal(data, eventsBefore) {
		t.Fatalf("events changed after binding rejection:\n%s", data)
	}
	if count := serviceTransactionMarkerCount(t, starts, "start"); count != 1 {
		t.Fatalf("service starts after binding rejection = %d, want 1", count)
	}
	if count := serviceTransactionMarkerCount(t, stops, "stop"); count != 0 {
		t.Fatalf("service stops after binding rejection = %d, want 0", count)
	}
	if _, err := manager.Bindings(); err != nil {
		t.Fatalf("binding became invalid after rejected Remove: %v", err)
	}
}

func TestServiceManagerApplyPersistenceFailureIsAtomic(t *testing.T) {
	for _, running := range []bool{false, true} {
		for _, failurePoint := range []string{"write", "rename"} {
			name := failurePoint + "_"
			if running {
				name += "running"
			} else {
				name += "stopped"
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				cfg, starts, stops := serviceTransactionProcessConfig(root, "worker")
				cfg.Enabled = running
				writeTestService(t, root, cfg)
				manager, err := NewServiceManager(root, ServiceManagerOptions{})
				if err != nil {
					t.Fatal(err)
				}
				stopProcessTestManager(t, &manager)
				if err := manager.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
				if running {
					waitForTestPath(t, starts, "start")
				}

				definitionPath := serviceConfigPath(root, cfg.ID)
				statusPath := filepath.Join(serviceRuntimePath(root, cfg.ID), "state.json")
				eventsPath := filepath.Join(serviceRuntimePath(root, cfg.ID), "events.jsonl")
				definitionBefore := readOptionalServiceTransactionFile(t, definitionPath)
				persistedStatusBefore := readOptionalServiceTransactionFile(t, statusPath)
				eventsBefore := readOptionalServiceTransactionFile(t, eventsPath)
				visibleBefore, err := manager.Show(cfg.ID)
				if err != nil {
					t.Fatal(err)
				}
				configBefore := manager.configs[cfg.ID]
				graphBefore := manager.graph
				processBefore := manager.runtimes[cfg.ID].process
				runtimeStatusBefore := manager.runtimes[cfg.ID].status

				injected := errors.New("injected definition " + failurePoint + " failure")
				switch failurePoint {
				case "write":
					manager.definitionStore.writeJSON = func(string, any, os.FileMode, func(string, string) error, func(string) error) error {
						return injected
					}
				case "rename":
					manager.definitionStore.rename = func(string, string) error { return injected }
				}
				replacement := cfg
				replacement.Args = []string{"replacement"}
				if err := manager.Apply(replacement); !errors.Is(err, injected) {
					t.Fatalf("Apply error = %v, want injected %s failure", err, failurePoint)
				}

				visibleAfter, err := manager.Show(cfg.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := stableServiceTransactionStatus(visibleAfter), stableServiceTransactionStatus(visibleBefore); !reflect.DeepEqual(got, want) {
					t.Fatalf("status after failed Apply = %#v, want %#v", visibleAfter, visibleBefore)
				}
				if got := manager.runtimes[cfg.ID].status; !reflect.DeepEqual(got, runtimeStatusBefore) {
					t.Fatalf("runtime status after failed Apply = %#v, want %#v", got, runtimeStatusBefore)
				}
				if got := manager.configs[cfg.ID]; !reflect.DeepEqual(got, configBefore) {
					t.Fatalf("config after failed Apply = %#v, want %#v", got, configBefore)
				}
				if !reflect.DeepEqual(manager.graph, graphBefore) {
					t.Fatalf("graph after failed Apply = %#v, want %#v", manager.graph, graphBefore)
				}
				if manager.runtimes[cfg.ID].process != processBefore {
					t.Fatal("failed Apply replaced the live process")
				}
				if data := readOptionalServiceTransactionFile(t, definitionPath); !bytes.Equal(data, definitionBefore) {
					t.Fatalf("definition changed after failed Apply:\n%s", data)
				}
				if data := readOptionalServiceTransactionFile(t, statusPath); !bytes.Equal(data, persistedStatusBefore) {
					t.Fatalf("persisted status changed after failed Apply:\n%s", data)
				}
				if data := readOptionalServiceTransactionFile(t, eventsPath); !bytes.Equal(data, eventsBefore) {
					t.Fatalf("events changed after failed Apply:\n%s", data)
				}
				wantStarts := 0
				if running {
					wantStarts = 1
				}
				if count := serviceTransactionMarkerCount(t, starts, "start"); count != wantStarts {
					t.Fatalf("service starts after failed Apply = %d, want %d", count, wantStarts)
				}
				if count := serviceTransactionMarkerCount(t, stops, "stop"); count != 0 {
					t.Fatalf("service stops after failed Apply = %d, want 0", count)
				}
				temps, err := filepath.Glob(filepath.Join(filepath.Dir(definitionPath), ".service-*.tmp"))
				if err != nil {
					t.Fatal(err)
				}
				if len(temps) != 0 {
					t.Fatalf("failed Apply left temporary files: %v", temps)
				}

				reconstructed, err := NewServiceManager(root, ServiceManagerOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if got := reconstructed.configs[cfg.ID]; !reflect.DeepEqual(got, configBefore) {
					t.Fatalf("reconstructed config = %#v, want %#v", got, configBefore)
				}
				reconstructedStatus, err := reconstructed.Show(cfg.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := stableServiceTransactionStatus(reconstructedStatus), stableServiceTransactionStatus(visibleAfter); !reflect.DeepEqual(got, want) {
					t.Fatalf("reconstructed status = %#v, want %#v", reconstructedStatus, visibleAfter)
				}
			})
		}
	}
}

func TestServiceManagerApplyValidatesBindingsBeforePersistence(t *testing.T) {
	root := t.TempDir()
	cfg, starts, stops := serviceTransactionProcessConfig(root, "worker")
	writeTestService(t, root, cfg)
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForTestPath(t, starts, "start")
	if err := writeServiceJSON(serviceBindingsPath(root), ServiceBindings{
		SchemaVersion: serviceSchemaVersion,
		Variables:     map[string]string{"ENDPOINT": "${service.missing.URL}"},
	}, 0o600); err != nil {
		t.Fatal(err)
	}

	definitionPath := serviceConfigPath(root, cfg.ID)
	eventsPath := filepath.Join(serviceRuntimePath(root, cfg.ID), "events.jsonl")
	definitionBefore := readOptionalServiceTransactionFile(t, definitionPath)
	eventsBefore := readOptionalServiceTransactionFile(t, eventsPath)
	replacement := cfg
	replacement.Args = []string{"replacement"}
	applyErr := manager.Apply(replacement)
	if applyErr == nil || !strings.Contains(applyErr.Error(), `unknown service "missing"`) {
		t.Fatalf("Apply error = %v, want binding validation failure", applyErr)
	}
	if data := readOptionalServiceTransactionFile(t, definitionPath); !bytes.Equal(data, definitionBefore) {
		t.Fatalf("definition changed after rejected Apply:\n%s", data)
	}
	if data := readOptionalServiceTransactionFile(t, eventsPath); !bytes.Equal(data, eventsBefore) {
		t.Fatalf("events changed after rejected Apply:\n%s", data)
	}
	if count := serviceTransactionMarkerCount(t, starts, "start"); count != 1 {
		t.Fatalf("service starts after rejected Apply = %d, want 1", count)
	}
	if count := serviceTransactionMarkerCount(t, stops, "stop"); count != 0 {
		t.Fatalf("service stops after rejected Apply = %d, want 0", count)
	}
}

func TestServiceManagerApplyPersistsRestartFailure(t *testing.T) {
	const secret = "private-apply-secret"
	root := t.TempDir()
	cfg, starts, stops := serviceTransactionProcessConfig(root, "worker")
	cfg.Environment = map[string]ServiceEnvironment{"TOKEN": {SecretName: "token"}}
	writeTestService(t, root, cfg)
	manager, err := NewServiceManager(root, ServiceManagerOptions{
		Resolver: EnvironmentSecretResolver{Values: map[string]string{"token": secret}},
	})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForTestPath(t, starts, "start")

	replacement := cfg
	replacement.Command = []string{filepath.Join(root, secret, "missing-service")}
	if err := manager.Apply(replacement); err != nil {
		t.Fatalf("Apply returned a reconciled start failure: %v", err)
	}
	if count := serviceTransactionMarkerCount(t, starts, "start"); count != 1 {
		t.Fatalf("service starts after replacement failure = %d, want 1", count)
	}
	if count := serviceTransactionMarkerCount(t, stops, "stop"); count != 1 {
		t.Fatalf("old service stops after replacement failure = %d, want 1", count)
	}
	persistedConfig, err := LoadServiceConfig(serviceConfigPath(root, cfg.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persistedConfig, manager.configs[cfg.ID]) || !reflect.DeepEqual(manager.runtimes[cfg.ID].config, manager.configs[cfg.ID]) {
		t.Fatalf("disk and memory disagree after restart failure: disk=%#v configs=%#v runtime=%#v", persistedConfig, manager.configs[cfg.ID], manager.runtimes[cfg.ID].config)
	}
	status, err := manager.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceStateBackoff || status.FailureCount != 1 || status.AttentionRequired || status.PID != 0 || status.ProcessGroup != 0 || status.ManualStop {
		t.Fatalf("status after replacement start failure = %#v", status)
	}
	if strings.Contains(status.LastError, secret) || !strings.Contains(status.LastError, "<redacted>") {
		t.Fatalf("restart failure was not safely redacted: %#v", status)
	}
	events := readOptionalServiceTransactionFile(t, filepath.Join(serviceRuntimePath(root, cfg.ID), "events.jsonl"))
	if !bytes.Contains(events, []byte(`"type":"start_failed"`)) || bytes.Contains(events, []byte(secret)) {
		t.Fatalf("unsafe restart failure events: %s", events)
	}
	persistedStatus := readPersistedServiceStatus(t, root, cfg.ID)
	if got, want := stableServiceTransactionStatus(persistedStatus), stableServiceTransactionStatus(status); !reflect.DeepEqual(got, want) {
		t.Fatalf("persisted restart failure status = %#v, want %#v", got, want)
	}

	reconstructed, err := NewServiceManager(root, ServiceManagerOptions{
		Resolver: EnvironmentSecretResolver{Values: map[string]string{"token": secret}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reconstructed.configs[cfg.ID], persistedConfig) {
		t.Fatalf("reconstructed config = %#v, want %#v", reconstructed.configs[cfg.ID], persistedConfig)
	}
	reconstructedStatus, err := reconstructed.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stableServiceTransactionStatus(reconstructedStatus), stableServiceTransactionStatus(status); !reflect.DeepEqual(got, want) {
		t.Fatalf("reconstructed restart failure status = %#v, want %#v", got, want)
	}
}

func TestServiceManagerApplyKeepsReconcileOutsideTransaction(t *testing.T) {
	root := t.TempDir()
	cfg, starts, stops := serviceTransactionProcessConfig(root, "worker")
	writeTestService(t, root, cfg)
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForTestPath(t, starts, "start")

	persisted := make(chan struct{})
	releaseWrite := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseWrite) }) }
	defer release()
	writeJSON := manager.definitionStore.writeJSON
	manager.definitionStore.writeJSON = func(path string, value any, mode os.FileMode, rename func(string, string) error, syncDir func(string) error) error {
		if err := writeJSON(path, value, mode, rename, syncDir); err != nil {
			return err
		}
		close(persisted)
		<-releaseWrite
		return nil
	}
	replacement := cfg
	replacement.Args = []string{"replacement"}
	applyResult := make(chan error, 1)
	go func() { applyResult <- manager.Apply(replacement) }()
	select {
	case <-persisted:
	case <-time.After(time.Second):
		t.Fatal("Apply did not persist the desired definition")
	}
	persistedConfig, err := LoadServiceConfig(serviceConfigPath(root, cfg.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persistedConfig.Args, replacement.Args) {
		t.Fatalf("persisted Apply args = %#v, want %#v", persistedConfig.Args, replacement.Args)
	}
	reconcileAttempted := make(chan struct{})
	reconcileResult := make(chan error, 1)
	go func() {
		close(reconcileAttempted)
		reconcileResult <- manager.Reconcile(context.Background())
	}()
	<-reconcileAttempted
	select {
	case err := <-reconcileResult:
		t.Fatalf("Reconcile observed an intermediate Apply state: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	if err := <-applyResult; err != nil {
		t.Fatal(err)
	}
	if err := <-reconcileResult; err != nil {
		t.Fatal(err)
	}
	if count := serviceTransactionMarkerCount(t, starts, "start"); count != 2 {
		t.Fatalf("service starts after committed Apply = %d, want 2", count)
	}
	if count := serviceTransactionMarkerCount(t, stops, "stop"); count != 1 {
		t.Fatalf("service stops after committed Apply = %d, want 1", count)
	}
}

func TestServiceManagerApplyPreservesManualStop(t *testing.T) {
	root := t.TempDir()
	cfg, starts, stops := serviceTransactionProcessConfig(root, "worker")
	writeTestService(t, root, cfg)
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForTestPath(t, starts, "start")
	if err := manager.StopService(context.Background(), cfg.ID); err != nil {
		t.Fatal(err)
	}

	replacement := cfg
	replacement.Args = []string{"replacement"}
	if err := manager.Apply(replacement); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceStateStopped || !status.ManualStop || !status.Enabled || status.PID != 0 || status.ProcessGroup != 0 {
		t.Fatalf("status after Apply to manually stopped service = %#v", status)
	}
	if count := serviceTransactionMarkerCount(t, starts, "start"); count != 1 {
		t.Fatalf("manual stop allowed Apply restart count = %d, want 1", count)
	}
	if count := serviceTransactionMarkerCount(t, stops, "stop"); count != 1 {
		t.Fatalf("manual stop count after Apply = %d, want 1", count)
	}
	persistedConfig, err := LoadServiceConfig(serviceConfigPath(root, cfg.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persistedConfig, manager.configs[cfg.ID]) {
		t.Fatalf("manual-stop Apply disk config = %#v, want %#v", persistedConfig, manager.configs[cfg.ID])
	}
}
