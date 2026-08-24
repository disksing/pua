package serve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func dependencyRestartNode(tracePath, id, dependency, version string) ServiceConfig {
	value := `value="$VERSION"`
	environment := map[string]ServiceEnvironment{
		"VERSION": {Literal: version},
	}
	dependencies := []string(nil)
	if dependency != "" {
		value = `value="$UPSTREAM-` + id + `"`
		environment = map[string]ServiceEnvironment{
			"UPSTREAM": {Template: "${service." + dependency + ".URL}"},
		}
		dependencies = []string{dependency}
	}
	command := value + `
tmp="$PUA_SERVICE_EXPORT_PATH.tmp"
printf '{"schemaVersion":1,"variables":{"URL":"%s"}}' "$value" > "$tmp"
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
		DependsOn:     dependencies,
		Exports:       true,
	}
}

func dependencyRestartFileNode(tracePath, id, versionPath string) ServiceConfig {
	cfg := dependencyRestartNode(tracePath, id, "", "")
	cfg.Environment = map[string]ServiceEnvironment{
		"VERSION_FILE": {Literal: versionPath},
	}
	cfg.Command[2] = strings.Replace(cfg.Command[2], `value="$VERSION"`, `value="$(cat "$VERSION_FILE")"`, 1)
	return cfg
}

func startDependencyRestartManager(t *testing.T, root string, configs ...ServiceConfig) *ServiceManager {
	t.Helper()
	for _, cfg := range configs {
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
	for _, cfg := range configs {
		waitForServiceState(t, manager, cfg.ID, ServiceStateReady)
	}
	return manager
}

func servicePIDs(t *testing.T, manager *ServiceManager, ids ...string) map[string]int {
	t.Helper()
	result := make(map[string]int, len(ids))
	for _, id := range ids {
		status, err := manager.Show(id)
		if err != nil {
			t.Fatal(err)
		}
		result[id] = status.PID
	}
	return result
}

func resetDependencyRestartTrace(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func dependencyRestartTrace(t *testing.T, path string, count int) []string {
	t.Helper()
	return waitForLaunches(t, path, count)
}

func TestServiceManagerApplyRestartsTransitiveDependentsWithFreshExports(t *testing.T) {
	root := t.TempDir()
	tracePath := filepath.Join(root, "dependency-restart-trace")
	alpha := dependencyRestartNode(tracePath, "alpha", "", "one")
	bravo := dependencyRestartNode(tracePath, "bravo", "alpha", "")
	charlie := dependencyRestartNode(tracePath, "charlie", "bravo", "")
	delta := dependencyRestartNode(tracePath, "delta", "", "stable")
	manager := startDependencyRestartManager(t, root, alpha, bravo, charlie, delta)
	before := servicePIDs(t, manager, "alpha", "bravo", "charlie", "delta")
	resetDependencyRestartTrace(t, tracePath)

	replacement := dependencyRestartNode(tracePath, "alpha", "", "two")
	if err := manager.Apply(replacement); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alpha", "bravo", "charlie"} {
		waitForServiceState(t, manager, id, ServiceStateReady)
	}
	wantTrace := []string{
		"stop:charlie:one-bravo-charlie",
		"stop:bravo:one-bravo",
		"stop:alpha:one",
		"start:alpha:two",
		"start:bravo:two-bravo",
		"start:charlie:two-bravo-charlie",
	}
	if got := dependencyRestartTrace(t, tracePath, len(wantTrace)); !reflect.DeepEqual(got, wantTrace) {
		t.Fatalf("Apply dependency lifecycle trace = %v, want %v", got, wantTrace)
	}
	after := servicePIDs(t, manager, "alpha", "bravo", "charlie", "delta")
	for _, id := range []string{"alpha", "bravo", "charlie"} {
		if after[id] == before[id] {
			t.Fatalf("Apply retained affected service %s PID %d", id, after[id])
		}
	}
	if after["delta"] != before["delta"] {
		t.Fatalf("Apply restarted unrelated delta: PID %d became %d", before["delta"], after["delta"])
	}

	resetDependencyRestartTrace(t, tracePath)
	if err := manager.Apply(replacement); err != nil {
		t.Fatal(err)
	}
	if got := servicePIDs(t, manager, "alpha", "bravo", "charlie", "delta"); !reflect.DeepEqual(got, after) {
		t.Fatalf("no-op Apply changed PIDs: got %v, want %v", got, after)
	}
	if data, err := os.ReadFile(tracePath); err != nil || len(data) != 0 {
		t.Fatalf("no-op Apply lifecycle trace = %q, error %v", data, err)
	}
}

func TestServiceManagerApplyAllRestartsAcrossDependencyGraphChange(t *testing.T) {
	root := t.TempDir()
	tracePath := filepath.Join(root, "dependency-restart-trace")
	alpha := dependencyRestartNode(tracePath, "alpha", "", "one")
	bravo := dependencyRestartNode(tracePath, "bravo", "alpha", "")
	delta := dependencyRestartNode(tracePath, "delta", "", "stable")
	manager := startDependencyRestartManager(t, root, alpha, bravo, delta)
	before := servicePIDs(t, manager, "alpha", "bravo", "delta")
	resetDependencyRestartTrace(t, tracePath)

	if err := manager.ApplyAll([]ServiceConfig{
		dependencyRestartNode(tracePath, "alpha", "", "two"),
		dependencyRestartNode(tracePath, "bravo", "delta", ""),
	}); err != nil {
		t.Fatal(err)
	}
	wantTrace := []string{
		"stop:bravo:one-bravo",
		"stop:alpha:one",
		"start:alpha:two",
		"start:bravo:stable-bravo",
	}
	if got := dependencyRestartTrace(t, tracePath, len(wantTrace)); !reflect.DeepEqual(got, wantTrace) {
		t.Fatalf("ApplyAll graph-change lifecycle trace = %v, want %v", got, wantTrace)
	}
	after := servicePIDs(t, manager, "alpha", "bravo", "delta")
	if after["alpha"] == before["alpha"] || after["bravo"] == before["bravo"] {
		t.Fatalf("ApplyAll retained affected PIDs: before %v, after %v", before, after)
	}
	if after["delta"] != before["delta"] {
		t.Fatalf("ApplyAll changed new dependency delta PID %d to %d", before["delta"], after["delta"])
	}
	status, err := manager.Show("bravo")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(status.Dependencies, []string{"delta"}) {
		t.Fatalf("bravo dependencies after ApplyAll = %v, want delta", status.Dependencies)
	}
}

func TestServiceManagerRestartRefreshesDependentsAndPreservesManualStop(t *testing.T) {
	root := t.TempDir()
	tracePath := filepath.Join(root, "dependency-restart-trace")
	versionPath := filepath.Join(root, "alpha-version")
	if err := os.WriteFile(versionPath, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	alpha := dependencyRestartFileNode(tracePath, "alpha", versionPath)
	bravo := dependencyRestartNode(tracePath, "bravo", "alpha", "")
	delta := dependencyRestartNode(tracePath, "delta", "", "stable")
	manager := startDependencyRestartManager(t, root, alpha, bravo, delta)
	before := servicePIDs(t, manager, "alpha", "bravo", "delta")
	resetDependencyRestartTrace(t, tracePath)
	if err := os.WriteFile(versionPath, []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := manager.RestartService(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	wantTrace := []string{
		"stop:bravo:one-bravo",
		"stop:alpha:one",
		"start:alpha:two",
		"start:bravo:two-bravo",
	}
	if got := dependencyRestartTrace(t, tracePath, len(wantTrace)); !reflect.DeepEqual(got, wantTrace) {
		t.Fatalf("Restart dependency lifecycle trace = %v, want %v", got, wantTrace)
	}
	after := servicePIDs(t, manager, "alpha", "bravo", "delta")
	if after["alpha"] == before["alpha"] || after["bravo"] == before["bravo"] {
		t.Fatalf("Restart retained affected PIDs: before %v, after %v", before, after)
	}
	if after["delta"] != before["delta"] {
		t.Fatalf("Restart changed unrelated delta PID %d to %d", before["delta"], after["delta"])
	}

	if err := manager.StopService(context.Background(), "bravo"); err != nil {
		t.Fatal(err)
	}
	resetDependencyRestartTrace(t, tracePath)
	if err := os.WriteFile(versionPath, []byte("three\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.RestartService(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	wantTrace = []string{"stop:alpha:two", "start:alpha:three"}
	if got := dependencyRestartTrace(t, tracePath, len(wantTrace)); !reflect.DeepEqual(got, wantTrace) {
		t.Fatalf("Restart with manual dependent trace = %v, want %v", got, wantTrace)
	}
	status, err := manager.Show("bravo")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceStateStopped || !status.ManualStop || status.PID != 0 {
		t.Fatalf("manual dependent after dependency Restart = %#v", status)
	}
}

func TestServiceManagerStopOrdersDependentsAndStartRestoresThem(t *testing.T) {
	root := t.TempDir()
	tracePath := filepath.Join(root, "dependency-stop-trace")
	alpha := dependencyRestartNode(tracePath, "alpha", "", "one")
	bravo := dependencyRestartNode(tracePath, "bravo", "alpha", "")
	charlie := dependencyRestartNode(tracePath, "charlie", "bravo", "")
	delta := dependencyRestartNode(tracePath, "delta", "", "stable")
	manager := startDependencyRestartManager(t, root, alpha, bravo, charlie, delta)
	deltaPID := servicePIDs(t, manager, "delta")["delta"]
	resetDependencyRestartTrace(t, tracePath)

	if err := manager.StopService(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	wantTrace := []string{
		"stop:charlie:one-bravo-charlie",
		"stop:bravo:one-bravo",
		"stop:alpha:one",
	}
	if got := dependencyRestartTrace(t, tracePath, len(wantTrace)); !reflect.DeepEqual(got, wantTrace) {
		t.Fatalf("Stop dependency lifecycle trace = %v, want %v", got, wantTrace)
	}
	for _, id := range []string{"alpha", "bravo", "charlie"} {
		status, err := manager.Show(id)
		if err != nil {
			t.Fatal(err)
		}
		if status.State != ServiceStateStopped || status.PID != 0 || status.ProcessGroup != 0 {
			t.Fatalf("%s after dependency Stop = %#v", id, status)
		}
		if got, want := status.ManualStop, id == "alpha"; got != want {
			t.Fatalf("%s ManualStop after dependency Stop = %t, want %t", id, got, want)
		}
	}
	if got := servicePIDs(t, manager, "delta")["delta"]; got != deltaPID {
		t.Fatalf("Stop changed unrelated delta PID %d to %d", deltaPID, got)
	}

	resetDependencyRestartTrace(t, tracePath)
	if err := manager.StartService(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	wantTrace = []string{
		"start:alpha:one",
		"start:bravo:one-bravo",
		"start:charlie:one-bravo-charlie",
	}
	if got := dependencyRestartTrace(t, tracePath, len(wantTrace)); !reflect.DeepEqual(got, wantTrace) {
		t.Fatalf("Start dependency recovery trace = %v, want %v", got, wantTrace)
	}
	for _, id := range []string{"alpha", "bravo", "charlie"} {
		waitForServiceState(t, manager, id, ServiceStateReady)
	}
	if got := servicePIDs(t, manager, "delta")["delta"]; got != deltaPID {
		t.Fatalf("Start changed unrelated delta PID %d to %d", deltaPID, got)
	}
}

func TestServiceManagerStopPreservesManualDependentIntent(t *testing.T) {
	root := t.TempDir()
	tracePath := filepath.Join(root, "dependency-manual-stop-trace")
	alpha := dependencyRestartNode(tracePath, "alpha", "", "one")
	bravo := dependencyRestartNode(tracePath, "bravo", "alpha", "")
	charlie := dependencyRestartNode(tracePath, "charlie", "bravo", "")
	delta := dependencyRestartNode(tracePath, "delta", "", "stable")
	manager := startDependencyRestartManager(t, root, alpha, bravo, charlie, delta)
	deltaPID := servicePIDs(t, manager, "delta")["delta"]

	if err := manager.StopService(context.Background(), "bravo"); err != nil {
		t.Fatal(err)
	}
	resetDependencyRestartTrace(t, tracePath)
	if err := manager.StopService(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if got := dependencyRestartTrace(t, tracePath, 1); !reflect.DeepEqual(got, []string{"stop:alpha:one"}) {
		t.Fatalf("Stop with manual dependent trace = %v, want only alpha", got)
	}

	resetDependencyRestartTrace(t, tracePath)
	if err := manager.StartService(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if got := dependencyRestartTrace(t, tracePath, 1); !reflect.DeepEqual(got, []string{"start:alpha:one"}) {
		t.Fatalf("Start with manual dependent trace = %v, want only alpha", got)
	}
	bravoStatus, err := manager.Show("bravo")
	if err != nil {
		t.Fatal(err)
	}
	if bravoStatus.State != ServiceStateStopped || !bravoStatus.ManualStop || bravoStatus.PID != 0 {
		t.Fatalf("manual dependent after Start = %#v", bravoStatus)
	}
	charlieStatus, err := manager.Show("charlie")
	if err != nil {
		t.Fatal(err)
	}
	if charlieStatus.State != ServiceStateBlocked || charlieStatus.ManualStop || charlieStatus.PID != 0 {
		t.Fatalf("transitive dependent behind manual stop = %#v", charlieStatus)
	}

	resetDependencyRestartTrace(t, tracePath)
	if err := manager.StartService(context.Background(), "bravo"); err != nil {
		t.Fatal(err)
	}
	wantTrace := []string{
		"start:bravo:one-bravo",
		"start:charlie:one-bravo-charlie",
	}
	if got := dependencyRestartTrace(t, tracePath, len(wantTrace)); !reflect.DeepEqual(got, wantTrace) {
		t.Fatalf("manual dependent recovery trace = %v, want %v", got, wantTrace)
	}
	if got := servicePIDs(t, manager, "delta")["delta"]; got != deltaPID {
		t.Fatalf("manual Stop recovery changed unrelated delta PID %d to %d", deltaPID, got)
	}
}

func TestServiceManagerDisableOrdersDependentsAndEnableRestoresThem(t *testing.T) {
	root := t.TempDir()
	tracePath := filepath.Join(root, "dependency-disable-trace")
	alpha := dependencyRestartNode(tracePath, "alpha", "", "one")
	bravo := dependencyRestartNode(tracePath, "bravo", "alpha", "")
	charlie := dependencyRestartNode(tracePath, "charlie", "bravo", "")
	delta := dependencyRestartNode(tracePath, "delta", "", "stable")
	manager := startDependencyRestartManager(t, root, alpha, bravo, charlie, delta)
	deltaPID := servicePIDs(t, manager, "delta")["delta"]
	resetDependencyRestartTrace(t, tracePath)

	if err := manager.Disable(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	wantTrace := []string{
		"stop:charlie:one-bravo-charlie",
		"stop:bravo:one-bravo",
		"stop:alpha:one",
	}
	if got := dependencyRestartTrace(t, tracePath, len(wantTrace)); !reflect.DeepEqual(got, wantTrace) {
		t.Fatalf("Disable dependency lifecycle trace = %v, want %v", got, wantTrace)
	}
	alphaStatus, err := manager.Show("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if alphaStatus.Enabled || alphaStatus.State != ServiceStateDisabled || alphaStatus.ManualStop || alphaStatus.PID != 0 {
		t.Fatalf("disabled dependency status = %#v", alphaStatus)
	}
	for _, id := range []string{"bravo", "charlie"} {
		status, err := manager.Show(id)
		if err != nil {
			t.Fatal(err)
		}
		if !status.Enabled || status.State != ServiceStateStopped || status.ManualStop || status.PID != 0 {
			t.Fatalf("dependent %s after Disable = %#v", id, status)
		}
	}
	if got := servicePIDs(t, manager, "delta")["delta"]; got != deltaPID {
		t.Fatalf("Disable changed unrelated delta PID %d to %d", deltaPID, got)
	}

	resetDependencyRestartTrace(t, tracePath)
	if err := manager.Enable("alpha"); err != nil {
		t.Fatal(err)
	}
	wantTrace = []string{
		"start:alpha:one",
		"start:bravo:one-bravo",
		"start:charlie:one-bravo-charlie",
	}
	if got := dependencyRestartTrace(t, tracePath, len(wantTrace)); !reflect.DeepEqual(got, wantTrace) {
		t.Fatalf("Enable dependency recovery trace = %v, want %v", got, wantTrace)
	}
	for _, id := range []string{"alpha", "bravo", "charlie"} {
		waitForServiceState(t, manager, id, ServiceStateReady)
	}
	if got := servicePIDs(t, manager, "delta")["delta"]; got != deltaPID {
		t.Fatalf("Enable changed unrelated delta PID %d to %d", deltaPID, got)
	}
}

func TestServiceManagerStopAndDisableRetainTargetWhenDependentStopFails(t *testing.T) {
	for _, operation := range []string{"stop", "disable"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			tracePath := filepath.Join(root, "dependency-stop-failure-trace")
			alpha := dependencyRestartNode(tracePath, "alpha", "", "one")
			bravo := dependencyRestartNode(tracePath, "bravo", "alpha", "")
			charlie := dependencyRestartNode(tracePath, "charlie", "bravo", "")
			delta := dependencyRestartNode(tracePath, "delta", "", "stable")
			manager := startDependencyRestartManager(t, root, alpha, bravo, charlie, delta)
			before := servicePIDs(t, manager, "alpha", "bravo", "charlie", "delta")
			bravoStatus, err := manager.Show("bravo")
			if err != nil {
				t.Fatal(err)
			}
			resetDependencyRestartTrace(t, tracePath)
			injected := errors.New("injected dependent stop failure")
			nativeSignal := manager.processPlatform.signalProcessGroup
			manager.processPlatform.signalProcessGroup = func(group int, signal syscall.Signal) error {
				if group == bravoStatus.ProcessGroup {
					return injected
				}
				return nativeSignal(group, signal)
			}
			t.Cleanup(func() { manager.processPlatform.signalProcessGroup = nativeSignal })

			var operationErr error
			if operation == "stop" {
				operationErr = manager.StopService(context.Background(), "alpha")
			} else {
				operationErr = manager.Disable(context.Background(), "alpha")
			}
			if operationErr == nil || !strings.Contains(operationErr.Error(), `stop dependent service "bravo" before "alpha"`) || !strings.Contains(operationErr.Error(), injected.Error()) {
				t.Fatalf("%s error = %v, want dependent stop failure", operation, operationErr)
			}
			if got := dependencyRestartTrace(t, tracePath, 1); !reflect.DeepEqual(got, []string{"stop:charlie:one-bravo-charlie"}) {
				t.Fatalf("failed %s lifecycle trace = %v, want only charlie stopped", operation, got)
			}
			after := servicePIDs(t, manager, "alpha", "bravo", "charlie", "delta")
			if after["alpha"] != before["alpha"] || after["bravo"] != before["bravo"] {
				t.Fatalf("failed %s stopped target or failing dependent: before %v, after %v", operation, before, after)
			}
			if after["charlie"] != 0 {
				t.Fatalf("failed %s retained already stopped charlie PID %d", operation, after["charlie"])
			}
			if after["delta"] != before["delta"] {
				t.Fatalf("failed %s changed unrelated delta: before %v, after %v", operation, before, after)
			}
			failedDependent, err := manager.Show("bravo")
			if err != nil {
				t.Fatal(err)
			}
			if failedDependent.State != ServiceStateAttentionRequired || !failedDependent.AttentionRequired || failedDependent.PID != before["bravo"] || !strings.Contains(failedDependent.LastError, injected.Error()) {
				t.Fatalf("dependent after failed %s = %#v", operation, failedDependent)
			}
			target, err := manager.Show("alpha")
			if err != nil {
				t.Fatal(err)
			}
			if target.PID != before["alpha"] || target.ProcessGroup <= 0 {
				t.Fatalf("target after failed %s lost ownership: %#v", operation, target)
			}
			if operation == "stop" {
				if !target.Enabled || !target.ManualStop {
					t.Fatalf("target after failed Stop lost manual intent: %#v", target)
				}
			} else {
				if target.Enabled || target.ManualStop {
					t.Fatalf("target after failed Disable = %#v", target)
				}
				persisted, err := LoadServiceConfig(serviceConfigPath(root, "alpha"))
				if err != nil {
					t.Fatal(err)
				}
				if persisted.Enabled {
					t.Fatal("failed dependent stop did not retain disabled definition")
				}
			}

			manager.processPlatform.signalProcessGroup = nativeSignal
		})
	}
}

func TestServiceManagerDependencyStopFailureRollsBackApplyAll(t *testing.T) {
	root := t.TempDir()
	tracePath := filepath.Join(root, "dependency-restart-trace")
	alpha := dependencyRestartNode(tracePath, "alpha", "", "one")
	bravo := dependencyRestartNode(tracePath, "bravo", "alpha", "")
	manager := startDependencyRestartManager(t, root, alpha, bravo)
	before := servicePIDs(t, manager, "alpha", "bravo")
	bravoStatus, err := manager.Show("bravo")
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected dependent stop failure")
	nativeSignal := manager.processPlatform.signalProcessGroup
	manager.processPlatform.signalProcessGroup = func(group int, signal syscall.Signal) error {
		if group == bravoStatus.ProcessGroup {
			return injected
		}
		return nativeSignal(group, signal)
	}

	applyErr := manager.ApplyAll([]ServiceConfig{dependencyRestartNode(tracePath, "alpha", "", "two")})
	manager.processPlatform.signalProcessGroup = nativeSignal
	if applyErr == nil || !strings.Contains(applyErr.Error(), injected.Error()) {
		t.Fatalf("Apply error = %v, want injected dependent stop failure", applyErr)
	}
	after := servicePIDs(t, manager, "alpha", "bravo")
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("failed Apply changed live PIDs: got %v, want %v", after, before)
	}
	persisted, err := LoadServiceConfig(serviceConfigPath(root, "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.Environment["VERSION"].Literal; got != "one" {
		t.Fatalf("failed Apply persisted VERSION %q, want one", got)
	}
	status, err := manager.Show("bravo")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceStateReady || status.AttentionRequired {
		t.Fatalf("rolled-back dependent status = %#v", status)
	}
}

func TestServiceManagerApplyRetriesDependencyInvalidationAfterStopFailure(t *testing.T) {
	root := t.TempDir()
	tracePath := filepath.Join(root, "dependency-restart-trace")
	alpha := dependencyRestartNode(tracePath, "alpha", "", "one")
	bravo := dependencyRestartNode(tracePath, "bravo", "alpha", "")
	manager := startDependencyRestartManager(t, root, alpha, bravo)
	before := servicePIDs(t, manager, "alpha", "bravo")
	resetDependencyRestartTrace(t, tracePath)
	bravoStatus, err := manager.Show("bravo")
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected dependent stop failure")
	nativeSignal := manager.processPlatform.signalProcessGroup
	manager.processPlatform.signalProcessGroup = func(group int, signal syscall.Signal) error {
		if group == bravoStatus.ProcessGroup {
			return injected
		}
		return nativeSignal(group, signal)
	}

	applyErr := manager.Apply(dependencyRestartNode(tracePath, "alpha", "", "two"))
	manager.processPlatform.signalProcessGroup = nativeSignal
	if applyErr == nil || !strings.Contains(applyErr.Error(), injected.Error()) {
		t.Fatalf("Apply error = %v, want injected dependent stop failure", applyErr)
	}
	if got := servicePIDs(t, manager, "alpha", "bravo"); !reflect.DeepEqual(got, before) {
		t.Fatalf("failed Apply changed live PIDs: got %v, want %v", got, before)
	}
	persisted, err := LoadServiceConfig(serviceConfigPath(root, "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.Environment["VERSION"].Literal; got != "two" {
		t.Fatalf("failed Apply persisted VERSION %q, want two", got)
	}

	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantTrace := []string{
		"stop:bravo:one-bravo",
		"stop:alpha:one",
		"start:alpha:two",
		"start:bravo:two-bravo",
	}
	if got := dependencyRestartTrace(t, tracePath, len(wantTrace)); !reflect.DeepEqual(got, wantTrace) {
		t.Fatalf("retried Apply dependency lifecycle trace = %v, want %v", got, wantTrace)
	}
	after := servicePIDs(t, manager, "alpha", "bravo")
	if after["alpha"] == before["alpha"] || after["bravo"] == before["bravo"] {
		t.Fatalf("retried Apply retained affected PIDs: before %v, after %v", before, after)
	}
}

func TestServiceManagerDependencyStopFailureRequiresAttentionOnRestart(t *testing.T) {
	root := t.TempDir()
	tracePath := filepath.Join(root, "dependency-restart-trace")
	alpha := dependencyRestartNode(tracePath, "alpha", "", "one")
	bravo := dependencyRestartNode(tracePath, "bravo", "alpha", "")
	manager := startDependencyRestartManager(t, root, alpha, bravo)
	before := servicePIDs(t, manager, "alpha", "bravo")
	bravoStatus, err := manager.Show("bravo")
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected dependent stop failure")
	nativeSignal := manager.processPlatform.signalProcessGroup
	manager.processPlatform.signalProcessGroup = func(group int, signal syscall.Signal) error {
		if group == bravoStatus.ProcessGroup {
			return injected
		}
		return nativeSignal(group, signal)
	}

	restartErr := manager.RestartService(context.Background(), "alpha")
	manager.processPlatform.signalProcessGroup = nativeSignal
	if restartErr == nil || !strings.Contains(restartErr.Error(), injected.Error()) {
		t.Fatalf("Restart error = %v, want injected dependent stop failure", restartErr)
	}
	after := servicePIDs(t, manager, "alpha", "bravo")
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("failed Restart changed live PIDs: got %v, want %v", after, before)
	}
	status, err := manager.Show("bravo")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceStateAttentionRequired || !status.AttentionRequired || status.Readiness.Ready || !strings.Contains(status.LastError, injected.Error()) {
		t.Fatalf("dependent after failed Restart = %#v", status)
	}
}
