package serve

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

func disabledBatchService(id string) ServiceConfig {
	return ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            id,
		Command:       []string{"/bin/true"},
	}
}

func TestWorkspaceServicesCollectionPutValidatesBeforeMutation(t *testing.T) {
	root := t.TempDir()
	workspace := serveWorkspace{ID: "workspace-one", Path: root}
	server := newServiceLifecycleTestServer(t, workspace)

	body := `{"services":[` +
		`{"schemaVersion":1,"id":"alpha","enabled":false,"command":["/bin/true"]},` +
		`{"schemaVersion":1,"id":"invalid!","enabled":false,"command":["/bin/true"]}` +
		`]}`
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/workspaces/workspace-one/services", strings.NewReader(body))
	server.handleWorkspaceServices(recorder, request, workspace.ID, nil)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("collection PUT returned %d: %s", recorder.Code, recorder.Body.String())
	}
	manager, _, err := serviceManagerForWorkspaceTest(server, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if statuses := manager.List(); len(statuses) != 0 {
		t.Fatalf("rejected collection created services: %#v", statuses)
	}
	if _, err := os.Stat(serviceConfigPath(root, "alpha")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("valid prefix definition survived rejected collection: %v", err)
	}
}

func TestServiceManagerApplyAllRejectsDuplicateIDs(t *testing.T) {
	manager, err := NewServiceManager(t.TempDir(), ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first := disabledBatchService("worker")
	second := first
	second.Args = []string{"replacement"}
	if err := manager.ApplyAll([]ServiceConfig{first, second}); err == nil || !strings.Contains(err.Error(), `duplicate service id "worker"`) {
		t.Fatalf("ApplyAll duplicate error = %v", err)
	}
	if len(manager.List()) != 0 {
		t.Fatal("duplicate collection mutated manager state")
	}
}

func TestServiceManagerApplyAllRollsBackDefinitionCommit(t *testing.T) {
	root := t.TempDir()
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	writeJSON := manager.definitionStore.writeJSON
	injected := errors.New("injected second definition failure")
	var writes atomic.Int32
	manager.definitionStore.writeJSON = func(path string, value any, mode os.FileMode, rename func(string, string) error) error {
		write := writes.Add(1)
		if err := writeJSON(path, value, mode, rename); err != nil {
			return err
		}
		if write == 2 {
			return injected
		}
		return nil
	}

	err = manager.ApplyAll([]ServiceConfig{disabledBatchService("alpha"), disabledBatchService("bravo")})
	if !errors.Is(err, injected) {
		t.Fatalf("ApplyAll error = %v, want injected commit failure", err)
	}
	if statuses := manager.List(); len(statuses) != 0 {
		t.Fatalf("failed commit left services: %#v", statuses)
	}
	for _, id := range []string{"alpha", "bravo"} {
		if _, err := os.Stat(serviceConfigPath(root, id)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("definition %s survived rollback: %v", id, err)
		}
		if _, err := os.Stat(filepath.Join(serviceRuntimePath(root, id), "state.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("runtime state %s survived rollback: %v", id, err)
		}
	}
	reconstructed, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(reconstructed.List()) != 0 {
		t.Fatal("rolled-back definitions were reconstructed")
	}
}

func TestServiceManagerApplyAllRollsBackLifecycleFailure(t *testing.T) {
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
	statusPath := filepath.Join(serviceRuntimePath(root, cfg.ID), "state.json")
	eventsPath := filepath.Join(serviceRuntimePath(root, cfg.ID), "events.jsonl")
	definitionBefore := readOptionalServiceTransactionFile(t, definitionPath)
	statusBefore := readOptionalServiceTransactionFile(t, statusPath)
	eventsBefore := readOptionalServiceTransactionFile(t, eventsPath)
	visibleBefore, err := manager.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	processBefore := manager.runtimes[cfg.ID].process
	processGroup := visibleBefore.ProcessGroup
	nativeSignal := manager.processPlatform.signalProcessGroup
	injected := errors.New("injected batch stop failure")
	manager.processPlatform.signalProcessGroup = func(group int, signal syscall.Signal) error {
		if group == processGroup {
			return injected
		}
		return nativeSignal(group, signal)
	}

	replacement := cfg
	replacement.Args = []string{"replacement"}
	applyErr := manager.ApplyAll([]ServiceConfig{replacement})
	manager.processPlatform.signalProcessGroup = nativeSignal
	if applyErr == nil || !strings.Contains(applyErr.Error(), injected.Error()) {
		t.Fatalf("ApplyAll error = %v, want lifecycle failure", applyErr)
	}
	visibleAfter, err := manager.Show(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stableServiceTransactionStatus(visibleAfter), stableServiceTransactionStatus(visibleBefore); !reflect.DeepEqual(got, want) {
		t.Fatalf("status after rollback = %#v, want %#v", got, want)
	}
	if manager.runtimes[cfg.ID].process != processBefore {
		t.Fatal("rollback abandoned the original live process")
	}
	if data := readOptionalServiceTransactionFile(t, definitionPath); !bytes.Equal(data, definitionBefore) {
		t.Fatalf("definition changed after lifecycle rollback:\n%s", data)
	}
	if data := readOptionalServiceTransactionFile(t, statusPath); !bytes.Equal(data, statusBefore) {
		t.Fatalf("runtime status changed after lifecycle rollback:\n%s", data)
	}
	if data := readOptionalServiceTransactionFile(t, eventsPath); !bytes.Equal(data, eventsBefore) {
		t.Fatalf("events changed after lifecycle rollback:\n%s", data)
	}
	if count := serviceTransactionMarkerCount(t, starts, "start"); count != 1 {
		t.Fatalf("starts after lifecycle rollback = %d, want 1", count)
	}
	if count := serviceTransactionMarkerCount(t, stops, "stop"); count != 0 {
		t.Fatalf("stops after lifecycle rollback = %d, want 0", count)
	}
}

func TestServiceManagerApplyAllStartsDependenciesInTopologicalOrder(t *testing.T) {
	root := t.TempDir()
	orderPath := filepath.Join(root, "start-order")
	service := func(id string, dependencies ...string) ServiceConfig {
		return ServiceConfig{
			SchemaVersion: serviceSchemaVersion,
			ID:            id,
			Enabled:       true,
			Command: []string{"/bin/sh", "-c",
				"printf '" + id + "\\n' >> " + shellQuote(orderPath) + "; trap 'exit 0' TERM; while :; do sleep 1; done"},
			DependsOn: dependencies,
		}
	}
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	consumer := service("consumer", "producer")
	producer := service("producer")
	if err := manager.ApplyAll([]ServiceConfig{consumer, producer}); err != nil {
		t.Fatal(err)
	}
	waitForTestPath(t, orderPath, "consumer")
	if got := strings.Fields(string(readOptionalServiceTransactionFile(t, orderPath))); !reflect.DeepEqual(got, []string{"producer", "consumer"}) {
		t.Fatalf("start order = %v, want producer then consumer", got)
	}
	for _, id := range []string{"producer", "consumer"} {
		status, err := manager.Show(id)
		if err != nil {
			t.Fatal(err)
		}
		if status.State != ServiceStateReady || !status.Readiness.Ready {
			t.Fatalf("service %s status = %#v", id, status)
		}
		persisted, err := LoadServiceConfig(serviceConfigPath(root, id))
		if err != nil {
			t.Fatal(err)
		}
		if persisted.ID != id {
			t.Fatalf("persisted service %s = %#v", id, persisted)
		}
	}
}
