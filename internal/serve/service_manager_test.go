package serve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/internal/security"
)

func writeTestService(t *testing.T, root string, cfg ServiceConfig) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".pua", "services"), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serviceConfigPath(root, cfg.ID), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestServiceManagerStateWireValues(t *testing.T) {
	states := []ServiceState{
		ServiceStateDisabled,
		ServiceStateStopped,
		ServiceStateBlocked,
		ServiceStateStarting,
		ServiceStateRunning,
		ServiceStateReady,
		ServiceStateBackoff,
		ServiceStateAttentionRequired,
	}
	data, err := json.Marshal(states)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), `["disabled","stopped","blocked","starting","running","ready","backoff","attention_required"]`; got != want {
		t.Fatalf("service state wire values = %s, want %s", got, want)
	}
}

func TestServiceConfigExportsDeclarationWire(t *testing.T) {
	var declared ServiceConfig
	decoder := json.NewDecoder(strings.NewReader(`{"schemaVersion":1,"id":"exporter","enabled":true,"command":["service"],"exports":true}`))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&declared); err != nil {
		t.Fatal(err)
	}
	if !declared.Exports {
		t.Fatal("exports declaration was not decoded")
	}
	data, err := json.Marshal(declared)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"exports":true`) {
		t.Fatalf("exports declaration was not encoded: %s", data)
	}
	var legacy ServiceConfig
	if err := json.Unmarshal([]byte(`{"schemaVersion":1,"id":"legacy","enabled":true,"command":["service"]}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Exports {
		t.Fatal("omitted exports declaration must remain disabled")
	}
}

func TestValidateServiceGraphRejectsCyclesAndSecretArguments(t *testing.T) {
	root := t.TempDir()
	configs := map[string]ServiceConfig{
		"alpha": {SchemaVersion: serviceSchemaVersion, ID: "alpha", Command: []string{"echo"}, DependsOn: []string{"beta"}},
		"beta":  {SchemaVersion: serviceSchemaVersion, ID: "beta", Command: []string{"echo"}, DependsOn: []string{"alpha"}},
	}
	if err := validateServiceGraph(root, configs); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle validation error = %v", err)
	}
	configs = map[string]ServiceConfig{"alpha": {SchemaVersion: serviceSchemaVersion, ID: "alpha", Command: []string{"echo", "${secret.token}"}}}
	if err := validateServiceGraph(root, configs); err == nil || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("secret argument validation error = %v", err)
	}
}

func TestServiceManagerStartsProcessAndPersistsRedactedLogs(t *testing.T) {
	root := t.TempDir()
	cfg := ServiceConfig{SchemaVersion: serviceSchemaVersion, ID: "worker", Enabled: true, Command: []string{"/bin/sh", "-c", "printf '%s\\n' \"$TOKEN\"; exit 7"}, Environment: map[string]ServiceEnvironment{"TOKEN": {SecretName: "token"}}, Restart: ServiceRestartConfig{InitialDelay: 10 * time.Millisecond, Multiplier: 2, MaxDelay: time.Second, ResetAfter: time.Minute}}
	writeTestService(t, root, cfg)
	m, err := NewServiceManager(root, ServiceManagerOptions{Resolver: EnvironmentSecretResolver{Values: map[string]string{"token": "secret-value"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err := m.Show("worker")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceStateBackoff && status.State != ServiceStateAttentionRequired {
		t.Fatalf("state = %q, want backoff", status.State)
	}
	data, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "worker"), "stdout.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-value") || !strings.Contains(string(data), "<redacted>") {
		t.Fatalf("log was not redacted: %q", data)
	}
	state, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "worker"), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(state), "secret-value") {
		t.Fatalf("secret persisted in state: %s", state)
	}
}

func TestServiceManagerRequiresInitialExportBeforeReadiness(t *testing.T) {
	root := t.TempDir()
	cfg := ServiceConfig{SchemaVersion: serviceSchemaVersion, ID: "ready", Enabled: true, Command: []string{"/bin/sh", "-c", "sleep 2"}, Readiness: &ServiceReadinessConfig{Command: []string{"/bin/sh", "-c", "exit 0"}, Interval: time.Second, Timeout: time.Second}, Restart: ServiceRestartConfig{InitialDelay: time.Millisecond, Multiplier: 2, MaxDelay: time.Second}}
	writeTestService(t, root, cfg)
	m, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err := m.Show("ready")
	if err != nil {
		t.Fatal(err)
	}
	if status.Readiness.Ready {
		t.Fatal("service became ready without an initial export")
	}
	if status.State != ServiceStateBackoff && status.State != ServiceStateAttentionRequired {
		t.Fatalf("state = %q, want backoff", status.State)
	}
}

func TestServiceManagerBuffersReadinessLogsUntilExportSecretsAreKnown(t *testing.T) {
	root := t.TempDir()
	cfg := ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "buffered",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c", `
			printf '%s\n' 'exported-secret'
			printf '%s' '{"schemaVersion":1,"secrets":{"TOKEN":"exported-secret"}}' > "$PUA_SERVICE_EXPORT_PATH"
			sleep 1
		`},
		Readiness: &ServiceReadinessConfig{Command: []string{"/bin/sh", "-c", "exit 0"}, Interval: time.Second, Timeout: time.Second},
		Restart:   ServiceRestartConfig{InitialDelay: time.Millisecond, Multiplier: 2, MaxDelay: time.Second},
	}
	writeTestService(t, root, cfg)
	m, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "buffered"), "stdout.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "exported-secret") || !strings.Contains(string(data), "<redacted>") {
		t.Fatalf("startup log was not redacted after export: %q", data)
	}
	if _, err := m.Exports("buffered"); err != nil {
		t.Fatal(err)
	}
	exportData, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "buffered"), "export.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(exportData), "exported-secret") {
		t.Fatalf("export secret remained on disk: %s", exportData)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestServiceManagerBuffersDeclaredExportLogsWithoutReadiness(t *testing.T) {
	root := t.TempDir()
	cfg := ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "exporter",
		Enabled:       true,
		Exports:       true,
		Command: []string{"/bin/sh", "-c", `
			sleep 0.1
			tmp="$PUA_SERVICE_EXPORT_PATH.tmp"
			printf '%s' '{"schemaVersion":1,"secrets":{"TOKEN":"no-ready-secret"}}' > "$tmp"
			mv "$tmp" "$PUA_SERVICE_EXPORT_PATH"
			printf '%s\n' 'no-ready-secret'
			sleep 1
		`},
		Restart: ServiceRestartConfig{InitialDelay: time.Millisecond, Multiplier: 2, MaxDelay: time.Second},
	}
	writeTestService(t, root, cfg)
	m, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "exporter"), "stdout.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "no-ready-secret") || !strings.Contains(string(data), "<redacted>") {
		t.Fatalf("startup log was not redacted after declared export: %q", data)
	}
	exportData, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "exporter"), "export.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(exportData), "no-ready-secret") {
		t.Fatalf("export secret remained on disk: %s", exportData)
	}
	status, err := m.Show("exporter")
	if err != nil {
		t.Fatal(err)
	}
	statusData, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(statusData), "no-ready-secret") {
		t.Fatalf("export secret appeared in public status: %s", statusData)
	}
	for _, name := range []string{"state.json", "events.jsonl"} {
		persisted, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "exporter"), name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(persisted), "no-ready-secret") {
			t.Fatalf("export secret appeared in %s: %s", name, persisted)
		}
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestServiceManagerRetainsExportSecretsAcrossReadinessPolls(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	producer := ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "producer",
		Enabled:       true,
		Command: []string{"/bin/sh", "-c", `
			tmp="$PUA_SERVICE_EXPORT_PATH.tmp"
			printf '%s' '{"schemaVersion":1,"secrets":{"TOKEN":"retained-secret"}}' > "$tmp"
			mv "$tmp" "$PUA_SERVICE_EXPORT_PATH"
			sleep 2
		`},
		Readiness: &ServiceReadinessConfig{Command: []string{"/bin/sh", "-c", "exit 0"}, Interval: time.Second, Timeout: time.Second},
		Restart:   ServiceRestartConfig{InitialDelay: time.Millisecond, Multiplier: 2, MaxDelay: time.Second},
	}
	writeTestService(t, root, producer)
	m, err := NewServiceManager(root, ServiceManagerOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	consumer := ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "consumer",
		Enabled:       true,
		Command:       []string{"/bin/sh", "-c", "sleep 1"},
		Environment:   map[string]ServiceEnvironment{"TOKEN": {Template: "${service.producer.TOKEN}"}},
		DependsOn:     []string{"producer"},
		Restart:       ServiceRestartConfig{InitialDelay: time.Millisecond, Multiplier: 2, MaxDelay: time.Second},
	}
	if err := m.Apply(consumer); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err := m.Show("consumer")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceStateReady {
		t.Fatalf("consumer state = %q, want ready (last error: %s)", status.State, status.LastError)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestServiceManagerRejectsNewExportSecretInVariable(t *testing.T) {
	root := t.TempDir()
	cfg := ServiceConfig{SchemaVersion: serviceSchemaVersion, ID: "exporter", Exports: true, Command: []string{"service"}}
	m := &ServiceManager{root: root, now: time.Now}
	rt := &serviceRuntime{config: cfg, redactor: security.NewRedactor()}
	path := filepath.Join(serviceRuntimePath(root, cfg.ID), "export.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	export := ServiceExportFile{
		SchemaVersion: serviceExportSchema,
		Variables:     map[string]string{"PUBLIC_TOKEN": "new-export-secret"},
		Secrets:       map[string]string{"TOKEN": "new-export-secret"},
	}
	if err := writeServiceJSON(path, export, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.readExportsLocked(rt); err == nil || !strings.Contains(err.Error(), "contains a secret") {
		t.Fatalf("read export error = %v, want secret variable rejection", err)
	}
}
