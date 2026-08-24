package serve

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

const liveExportTestSecret = "live-export-test-secret"

func liveExportProducerNode(tracePath, id, version, versionPath string) ServiceConfig {
	value := `value="$VERSION"`
	environment := map[string]ServiceEnvironment{"VERSION": {Literal: version}}
	if versionPath != "" {
		value = `value="$(cat "$VERSION_FILE")"`
		environment = map[string]ServiceEnvironment{"VERSION_FILE": {Literal: versionPath}}
	}
	command := value + `
tmp="$PUA_SERVICE_EXPORT_PATH.tmp"
printf '{"schemaVersion":1,"variables":{"URL":"%s","STABLE":"stable"},"secrets":{"TOKEN":"` + liveExportTestSecret + `"}}' "$value" > "$tmp"
mv "$tmp" "$PUA_SERVICE_EXPORT_PATH"
printf 'start:` + id + `:%s\n' "$value" >> ` + shellQuote(tracePath) + `
trap 'printf "stop:` + id + `:%s\n" "$value" >> ` + shellQuote(tracePath) + `; exit 0' TERM
while :; do sleep 1; done`
	return ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            id,
		Enabled:       true,
		Command:       []string{"/bin/sh", "-c", command},
		Environment:   environment,
		Exports:       true,
	}
}

func replaceLiveServiceExport(t *testing.T, root, id string, variables, secrets map[string]string) {
	t.Helper()
	path := filepath.Join(serviceRuntimePath(root, id), "export.json")
	temporary := path + ".next"
	if err := writeServiceJSON(temporary, ServiceExportFile{
		SchemaVersion: serviceExportSchema,
		Variables:     cloneStringMap(variables),
		Secrets:       cloneStringMap(secrets),
	}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, path); err != nil {
		t.Fatal(err)
	}
}

func requireEmptyServiceTrace(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "" {
		t.Fatalf("unexpected service lifecycle trace: %s", data)
	}
}

func TestServiceManagerLiveExportChangeRestartsOnlyAffectedConsumers(t *testing.T) {
	root := t.TempDir()
	tracePath := filepath.Join(root, "live-export-trace")
	alpha := liveExportProducerNode(tracePath, "alpha", "one", "")
	bravo := dependencyRestartNode(tracePath, "bravo", "alpha", "")
	charlie := dependencyRestartNode(tracePath, "charlie", "bravo", "")
	delta := dependencyRestartNode(tracePath, "delta", "", "unrelated")
	echo := dependencyRestartNode(tracePath, "echo", "alpha", "")
	echo.Environment["UPSTREAM"] = ServiceEnvironment{Template: "${service.alpha.STABLE}"}
	foxtrot := dependencyRestartNode(tracePath, "foxtrot", "", "explicit-only")
	foxtrot.DependsOn = []string{"alpha"}
	manager := startDependencyRestartManager(t, root, alpha, bravo, charlie, delta, echo, foxtrot)
	before := servicePIDs(t, manager, "alpha", "bravo", "charlie", "delta", "echo", "foxtrot")
	resetDependencyRestartTrace(t, tracePath)

	replaceLiveServiceExport(t, root, "alpha", map[string]string{"URL": "two", "STABLE": "stable"}, nil)
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantTrace := []string{
		"stop:charlie:one-bravo-charlie",
		"stop:bravo:one-bravo",
		"start:bravo:two-bravo",
		"start:charlie:two-bravo-charlie",
	}
	if got := dependencyRestartTrace(t, tracePath, len(wantTrace)); !reflect.DeepEqual(got, wantTrace) {
		t.Fatalf("live export lifecycle trace = %v, want %v", got, wantTrace)
	}
	after := servicePIDs(t, manager, "alpha", "bravo", "charlie", "delta", "echo", "foxtrot")
	if after["alpha"] != before["alpha"] {
		t.Fatalf("live export restarted producer: before %d, after %d", before["alpha"], after["alpha"])
	}
	for _, id := range []string{"bravo", "charlie"} {
		if after[id] == before[id] {
			t.Fatalf("live export retained stale consumer %s PID %d", id, after[id])
		}
	}
	for _, id := range []string{"delta", "echo", "foxtrot"} {
		if after[id] != before[id] {
			t.Fatalf("live export restarted unaffected service %s: before %d, after %d", id, before[id], after[id])
		}
	}

	resetDependencyRestartTrace(t, tracePath)
	stablePIDs := servicePIDs(t, manager, "alpha", "bravo", "charlie", "delta", "echo", "foxtrot")
	for _, secrets := range []map[string]string{nil, {"TOKEN": liveExportTestSecret}} {
		replaceLiveServiceExport(t, root, "alpha", map[string]string{"URL": "two", "STABLE": "stable"}, secrets)
		if err := manager.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := servicePIDs(t, manager, "alpha", "bravo", "charlie", "delta", "echo", "foxtrot"); !reflect.DeepEqual(got, stablePIDs) {
		t.Fatalf("identical or secret-only handoff restarted services: before %v, after %v", stablePIDs, got)
	}
	requireEmptyServiceTrace(t, tracePath)
}

func TestServiceManagerLiveExportChangeLeavesManualConsumerStopped(t *testing.T) {
	root := t.TempDir()
	tracePath := filepath.Join(root, "live-export-manual-trace")
	alpha := liveExportProducerNode(tracePath, "alpha", "one", "")
	bravo := dependencyRestartNode(tracePath, "bravo", "alpha", "")
	charlie := dependencyRestartNode(tracePath, "charlie", "bravo", "")
	manager := startDependencyRestartManager(t, root, alpha, bravo, charlie)
	producerPID := servicePIDs(t, manager, "alpha")["alpha"]
	if err := manager.StopService(context.Background(), "bravo"); err != nil {
		t.Fatal(err)
	}
	resetDependencyRestartTrace(t, tracePath)

	replaceLiveServiceExport(t, root, "alpha", map[string]string{"URL": "two", "STABLE": "stable"}, nil)
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := servicePIDs(t, manager, "alpha")["alpha"]; got != producerPID {
		t.Fatalf("live export restarted producer: before %d, after %d", producerPID, got)
	}
	bravoStatus, err := manager.Show("bravo")
	if err != nil {
		t.Fatal(err)
	}
	charlieStatus, err := manager.Show("charlie")
	if err != nil {
		t.Fatal(err)
	}
	if !bravoStatus.ManualStop || bravoStatus.PID != 0 || bravoStatus.State != ServiceStateStopped {
		t.Fatalf("manual consumer restarted after live export: %#v", bravoStatus)
	}
	if charlieStatus.PID != 0 || charlieStatus.State != ServiceStateBlocked {
		t.Fatalf("transitive consumer behind manual stop restarted: %#v", charlieStatus)
	}
	requireEmptyServiceTrace(t, tracePath)
}

func TestServiceManagerLiveExportRetriesDependentStopFailure(t *testing.T) {
	root := t.TempDir()
	tracePath := filepath.Join(root, "live-export-retry-trace")
	alpha := liveExportProducerNode(tracePath, "alpha", "one", "")
	bravo := dependencyRestartNode(tracePath, "bravo", "alpha", "")
	charlie := dependencyRestartNode(tracePath, "charlie", "bravo", "")
	manager := startDependencyRestartManager(t, root, alpha, bravo, charlie)
	before := servicePIDs(t, manager, "alpha", "bravo", "charlie")
	bravoStatus, err := manager.Show("bravo")
	if err != nil {
		t.Fatal(err)
	}
	resetDependencyRestartTrace(t, tracePath)
	injected := errors.New("injected live export stop failure")
	nativeSignal := manager.processPlatform.signalProcessGroup
	owner := manager
	t.Cleanup(func() { owner.processPlatform.signalProcessGroup = nativeSignal })
	manager.processPlatform.signalProcessGroup = func(group int, signal syscall.Signal) error {
		if group == bravoStatus.ProcessGroup {
			return injected
		}
		return nativeSignal(group, signal)
	}

	replaceLiveServiceExport(t, root, "alpha", map[string]string{"URL": "two", "STABLE": "stable"}, nil)
	if err := manager.Reconcile(context.Background()); err == nil || !strings.Contains(err.Error(), injected.Error()) {
		t.Fatalf("first live export reconcile error = %v, want injected failure", err)
	}
	state, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "alpha"), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted persistedServiceRuntimeState
	if err := json.Unmarshal(state, &persisted); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted.PendingExportChanges, []string{"URL"}) || persisted.Exports.Variables["URL"] != "two" {
		t.Fatalf("committed changed-key invalidation was not durable: %#v", persisted)
	}
	if strings.Contains(string(state), liveExportTestSecret) {
		t.Fatalf("pending invalidation persisted an exported secret: %s", state)
	}
	manager.processPlatform.signalProcessGroup = nativeSignal
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantTrace := []string{
		"stop:charlie:one-bravo-charlie",
		"stop:bravo:one-bravo",
		"start:bravo:two-bravo",
		"start:charlie:two-bravo-charlie",
	}
	if got := dependencyRestartTrace(t, tracePath, len(wantTrace)); !reflect.DeepEqual(got, wantTrace) {
		t.Fatalf("retried live export lifecycle trace = %v, want %v", got, wantTrace)
	}
	after := servicePIDs(t, manager, "alpha", "bravo", "charlie")
	if after["alpha"] != before["alpha"] || after["bravo"] == before["bravo"] || after["charlie"] == before["charlie"] {
		t.Fatalf("retried live export PIDs = %v, before %v", after, before)
	}
	for _, id := range []string{"bravo", "charlie"} {
		status, err := manager.Show(id)
		if err != nil {
			t.Fatal(err)
		}
		if status.State != ServiceStateReady || status.AttentionRequired {
			t.Fatalf("consumer %s did not recover after stop retry: %#v", id, status)
		}
	}
}

func TestServiceManagerReconstructsCommittedLiveExportInvalidation(t *testing.T) {
	root := t.TempDir()
	tracePath := filepath.Join(root, "live-export-reconstruct-trace")
	versionPath := filepath.Join(root, "producer-version")
	if err := os.WriteFile(versionPath, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	alpha := liveExportProducerNode(tracePath, "alpha", "", versionPath)
	bravo := dependencyRestartNode(tracePath, "bravo", "alpha", "")
	charlie := dependencyRestartNode(tracePath, "charlie", "bravo", "")
	for _, cfg := range []ServiceConfig{alpha, bravo, charlie} {
		writeTestService(t, root, cfg)
	}
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alpha", "bravo", "charlie"} {
		waitForServiceState(t, manager, id, ServiceStateReady)
	}
	bravoStatus, err := manager.Show("bravo")
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected reconstruction stop failure")
	nativeSignal := manager.processPlatform.signalProcessGroup
	owner := manager
	t.Cleanup(func() { owner.processPlatform.signalProcessGroup = nativeSignal })
	manager.processPlatform.signalProcessGroup = func(group int, signal syscall.Signal) error {
		if group == bravoStatus.ProcessGroup {
			return injected
		}
		return nativeSignal(group, signal)
	}
	if err := os.WriteFile(versionPath, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	replaceLiveServiceExport(t, root, "alpha", map[string]string{"URL": "two", "STABLE": "stable"}, nil)
	if err := manager.Reconcile(context.Background()); err == nil || !strings.Contains(err.Error(), injected.Error()) {
		t.Fatalf("live export reconcile error = %v, want injected failure", err)
	}
	manager.processPlatform.signalProcessGroup = nativeSignal

	reconstructed, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manager = reconstructed
	if got := sortedStringSet(manager.runtimes["alpha"].pendingExportKeys); !reflect.DeepEqual(got, []string{"URL"}) || !manager.runtimes["alpha"].exportKeysCommitted {
		t.Fatalf("reconstructed pending export invalidation = %v, committed=%t", got, manager.runtimes["alpha"].exportKeysCommitted)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alpha", "bravo", "charlie"} {
		waitForServiceState(t, manager, id, ServiceStateReady)
	}
	for id, want := range map[string]string{"alpha": "two", "bravo": "two-bravo", "charlie": "two-bravo-charlie"} {
		exports, err := manager.Exports(id)
		if err != nil {
			t.Fatal(err)
		}
		if got := exports.Variables["URL"]; got != want {
			t.Fatalf("reconstructed service %s export URL = %q, want %q", id, got, want)
		}
	}
}
