package serve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if status.State != "backoff" && status.State != "attention_required" {
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
	if status.State != "backoff" && status.State != "attention_required" {
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
