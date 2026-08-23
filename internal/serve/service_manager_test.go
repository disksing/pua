package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func waitForTestFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", filepath.Base(path))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForTestPath(t *testing.T, path, content string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), content) {
			return
		}
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in %s", content, filepath.Base(path))
		}
		time.Sleep(10 * time.Millisecond)
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
	var readinessOnly ServiceConfig
	if err := json.Unmarshal([]byte(`{"schemaVersion":1,"id":"ready","enabled":true,"command":["service"],"readiness":{"command":["check"]}}`), &readinessOnly); err != nil {
		t.Fatal(err)
	}
	if readinessOnly.Exports {
		t.Fatal("readiness must not enable an omitted exports declaration")
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

func TestValidateServiceGraphRejectsMixedDependencyCycles(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name    string
		configs map[string]ServiceConfig
	}{
		{
			name: "two services",
			configs: map[string]ServiceConfig{
				"alpha": {
					SchemaVersion: serviceSchemaVersion,
					ID:            "alpha",
					Command:       []string{"echo"},
					DependsOn:     []string{"beta"},
				},
				"beta": {
					SchemaVersion: serviceSchemaVersion,
					ID:            "beta",
					Command:       []string{"echo"},
					Environment: map[string]ServiceEnvironment{
						"URL": {Template: "${service.alpha.URL}"},
					},
				},
			},
		},
		{
			name: "three services",
			configs: map[string]ServiceConfig{
				"alpha": {
					SchemaVersion: serviceSchemaVersion,
					ID:            "alpha",
					Command:       []string{"echo"},
					DependsOn:     []string{"beta"},
				},
				"beta": {
					SchemaVersion: serviceSchemaVersion,
					ID:            "beta",
					Command:       []string{"echo"},
					Environment: map[string]ServiceEnvironment{
						"URL": {Template: "${service.charlie.URL}"},
					},
				},
				"charlie": {
					SchemaVersion: serviceSchemaVersion,
					ID:            "charlie",
					Command:       []string{"echo"},
					DependsOn:     []string{"alpha"},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateServiceGraph(root, test.configs)
			if err == nil || !strings.Contains(err.Error(), "dependency cycle") {
				t.Fatalf("mixed dependency validation error = %v, want dependency cycle", err)
			}
		})
	}
}

func TestServiceDependencyGraphUnionsAndDeduplicatesEdges(t *testing.T) {
	configs := map[string]ServiceConfig{
		"alpha": {
			DependsOn: []string{"beta"},
			Environment: map[string]ServiceEnvironment{
				"A_FIRST":  {Template: "${service.beta.URL}"},
				"B_AGAIN":  {Template: "${service.beta.PORT}:${service.charlie.PORT}"},
				"C_SECRET": {Template: "${secret.token}"},
				"D_DIRECT": {SecretName: "another-token"},
			},
		},
		"beta":    {},
		"charlie": {},
	}

	graph, err := buildServiceDependencyGraph(configs)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(graph), len(configs); got != want {
		t.Fatalf("dependency graph service count = %d, want %d", got, want)
	}
	if got, want := strings.Join(graph["alpha"], ","), "beta,charlie"; got != want {
		t.Fatalf("alpha dependency edges = %q, want %q", got, want)
	}
	if len(graph["beta"]) != 0 || len(graph["charlie"]) != 0 {
		t.Fatalf("unrelated dependency edges = %#v", graph)
	}
	order, err := graph.topologicalOrder()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(order, ","), "beta,charlie,alpha"; got != want {
		t.Fatalf("topological order = %q, want %q", got, want)
	}
}

func TestServiceDependencyGraphReportsReferenceField(t *testing.T) {
	tests := []struct {
		name      string
		config    ServiceConfig
		wantError string
	}{
		{
			name:      "unknown explicit dependency",
			config:    ServiceConfig{DependsOn: []string{"missing"}},
			wantError: `dependsOn: unknown service "missing"`,
		},
		{
			name: "unknown template dependency",
			config: ServiceConfig{Environment: map[string]ServiceEnvironment{
				"URL": {Template: "${service.missing.URL}"},
			}},
			wantError: `environment.URL: unknown service "missing"`,
		},
		{
			name:      "explicit self dependency",
			config:    ServiceConfig{DependsOn: []string{"alpha"}},
			wantError: "dependsOn: service cannot depend on itself",
		},
		{
			name: "template self dependency",
			config: ServiceConfig{Environment: map[string]ServiceEnvironment{
				"URL": {Template: "${service.alpha.URL}"},
			}},
			wantError: "environment.URL: service cannot reference itself",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := buildServiceDependencyGraph(map[string]ServiceConfig{"alpha": test.config})
			if err == nil || err.Error() != test.wantError {
				t.Fatalf("dependency graph error = %v, want %q", err, test.wantError)
			}
		})
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

func TestServiceManagerNamedSecretResolutionParity(t *testing.T) {
	type scenario struct {
		name            string
		secretName      string
		resolver        ServiceSecretResolver
		configure       func(*testing.T)
		wantValue       string
		wantSource      string
		wantError       string
		sensitiveValues []string
	}

	const fallbackSecretName = "pua-review-secret-resolution-fallback"
	fallbackValue := "fallback-secret-value"
	scenarios := []scenario{
		{
			name:       "default resolver fallback",
			secretName: fallbackSecretName,
			configure: func(t *testing.T) {
				t.Setenv(fallbackSecretName, fallbackValue)
			},
			wantValue:  fallbackValue,
			wantSource: "environment:" + fallbackSecretName,
		},
		{
			name:       "missing secret",
			secretName: "pua-review-secret-resolution-missing",
			configure: func(t *testing.T) {
				for _, key := range []string{"pua-review-secret-resolution-missing", "PUA_SECRET_PUA_REVIEW_SECRET_RESOLUTION_MISSING"} {
					value, present := os.LookupEnv(key)
					if err := os.Unsetenv(key); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if present {
							if err := os.Setenv(key, value); err != nil {
								t.Errorf("restore %s: %v", key, err)
							}
						}
					})
				}
			},
			wantError: `secret "pua-review-secret-resolution-missing" is unavailable`,
		},
		{
			name:       "resolver error",
			secretName: "resolver-error",
			resolver: ServiceSecretResolverFunc(func(string) (string, string, error) {
				return "resolver-error-secret-value", "backend", errors.New("backend exposed resolver-error-secret-value")
			}),
			wantError:       `secret "resolver-error" is unavailable`,
			sensitiveValues: []string{"resolver-error-secret-value", "backend exposed"},
		},
		{
			name:       "NUL value",
			secretName: "nul-value",
			resolver: ServiceSecretResolverFunc(func(string) (string, string, error) {
				return "nul-secret-value\x00tail", "backend", nil
			}),
			wantError:       `secret "nul-value" contains NUL`,
			sensitiveValues: []string{"nul-secret-value", "tail"},
		},
	}
	references := []struct {
		name  string
		entry func(string) ServiceEnvironment
	}{
		{name: "SecretName", entry: func(name string) ServiceEnvironment { return ServiceEnvironment{SecretName: name} }},
		{name: "complete template", entry: func(name string) ServiceEnvironment { return ServiceEnvironment{Template: "${secret." + name + "}"} }},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			if scenario.configure != nil {
				scenario.configure(t)
			}
			for _, reference := range references {
				t.Run(reference.name, func(t *testing.T) {
					manager := &ServiceManager{resolver: scenario.resolver}
					value, source, err := manager.resolveEnvironmentValueLocked(reference.entry(scenario.secretName))
					if scenario.wantError == "" {
						if err != nil {
							t.Fatal("named secret resolution unexpectedly failed")
						}
						if value != scenario.wantValue || source != scenario.wantSource {
							t.Fatal("named secret resolution returned unexpected value or source")
						}
						return
					}
					if err == nil || err.Error() != scenario.wantError {
						t.Fatal("named secret resolution did not return the safe error contract")
					}
					if value != "" || source != "" {
						t.Fatal("failed named secret resolution returned a value or source")
					}
					for _, sensitive := range scenario.sensitiveValues {
						if strings.Contains(err.Error(), sensitive) {
							t.Fatal("named secret resolution error leaked sensitive resolver data")
						}
					}
				})
			}
		})
	}
}

func TestServiceManagerReadinessDoesNotRequireInitialExport(t *testing.T) {
	root := t.TempDir()
	cfg := ServiceConfig{SchemaVersion: serviceSchemaVersion, ID: "ready", Enabled: true, Command: []string{"/bin/sh", "-c", "printf 'readiness-only-output\\n'; sleep 2"}, Readiness: &ServiceReadinessConfig{Command: []string{"/bin/sh", "-c", "exit 0"}, Interval: time.Second, Timeout: time.Second}, Restart: ServiceRestartConfig{InitialDelay: time.Millisecond, Multiplier: 2, MaxDelay: time.Second}}
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
	if !status.Readiness.Ready {
		t.Fatalf("readiness-only service did not become ready: %s", status.LastError)
	}
	if status.State != ServiceStateReady {
		t.Fatalf("state = %q, want ready", status.State)
	}
	waitForTestPath(t, filepath.Join(serviceRuntimePath(root, "ready"), "stdout.log"), "readiness-only-output")
	if _, err := os.Stat(filepath.Join(serviceRuntimePath(root, "ready"), "export.json")); !os.IsNotExist(err) {
		t.Fatalf("readiness-only service unexpectedly wrote an export hand-off: %v", err)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestServiceManagerDeclaredExporterRequiresValidInitialHandoff(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		readiness  bool
		exportData string
	}{
		{name: "missing without readiness"},
		{name: "missing with readiness", readiness: true},
		{name: "malformed without readiness", exportData: `{not-json`},
		{name: "malformed with readiness", readiness: true, exportData: `{not-json`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			command := "sleep 2"
			if scenario.exportData != "" {
				command = "printf '%s' '" + scenario.exportData + "' > \"$PUA_SERVICE_EXPORT_PATH\"; sleep 2"
			}
			cfg := ServiceConfig{
				SchemaVersion: serviceSchemaVersion,
				ID:            "exporter",
				Enabled:       true,
				Exports:       true,
				Command:       []string{"/bin/sh", "-c", command},
				Restart:       ServiceRestartConfig{InitialDelay: time.Millisecond, Multiplier: 2, MaxDelay: time.Second},
			}
			if scenario.readiness {
				cfg.Readiness = &ServiceReadinessConfig{Command: []string{"/bin/sh", "-c", "exit 0"}, Interval: time.Second, Timeout: 100 * time.Millisecond}
			}
			writeTestService(t, root, cfg)
			m, err := NewServiceManager(root, ServiceManagerOptions{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			if err := m.Start(ctx); err != nil {
				t.Fatal(err)
			}
			status, err := m.Show("exporter")
			if err != nil {
				t.Fatal(err)
			}
			if status.Readiness.Ready {
				t.Fatal("declared exporter became ready without a valid initial hand-off")
			}
			if status.State != ServiceStateBackoff && status.State != ServiceStateAttentionRequired {
				t.Fatalf("state = %q, want backoff", status.State)
			}
			if status.LastError == "" {
				t.Fatal("missing or malformed initial hand-off was not reported")
			}
		})
	}
}

func TestServiceManagerBuffersReadinessLogsUntilExportSecretsAreKnown(t *testing.T) {
	root := t.TempDir()
	startedPath := filepath.Join(root, "exporter-started")
	releasePath := filepath.Join(root, "release-export")
	cfg := ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "buffered",
		Enabled:       true,
		Exports:       true,
		Command: []string{"/bin/sh", "-c", fmt.Sprintf(`
			printf '%%s\n' 'exported-secret'
			: > %s
			while [ ! -f %s ]; do sleep 0.01; done
			tmp="$PUA_SERVICE_EXPORT_PATH.tmp"
			printf '%%s' '{"schemaVersion":1,"secrets":{"TOKEN":"exported-secret"}}' > "$tmp"
			mv "$tmp" "$PUA_SERVICE_EXPORT_PATH"
			printf 'after-handoff\n'
			sleep 1
		`, shellQuote(startedPath), shellQuote(releasePath))},
		Readiness: &ServiceReadinessConfig{Command: []string{"/bin/sh", "-c", "exit 0"}, Interval: time.Second, Timeout: 3 * time.Second},
		Restart:   ServiceRestartConfig{InitialDelay: time.Millisecond, Multiplier: 2, MaxDelay: time.Second},
	}
	writeTestService(t, root, cfg)
	m, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &m)
	t.Cleanup(func() { _ = os.WriteFile(releasePath, nil, 0o600) })
	started := make(chan error, 1)
	go func() { started <- m.Start(context.Background()) }()
	waitForTestFile(t, startedPath)
	select {
	case err := <-started:
		t.Fatalf("declared exporter completed startup before its hand-off: %v", err)
	default:
	}
	logPath := filepath.Join(serviceRuntimePath(root, "buffered"), "stdout.log")
	gateDeadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(gateDeadline) {
		data, err := os.ReadFile(logPath)
		if err == nil && len(data) > 0 {
			t.Fatal("declared exporter persisted startup output before its hand-off")
		}
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("declared exporter did not accept its initial hand-off")
	}
	waitForTestPath(t, logPath, "after-handoff")
	data, err := os.ReadFile(logPath)
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
	status, err := m.Show("buffered")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceStateReady || !status.Readiness.Ready {
		t.Fatalf("declared readiness exporter did not become ready: %#v", status)
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

func TestServiceManagerRejectsSecretRotationBeforePersistingLaterLogs(t *testing.T) {
	root := t.TempDir()
	runtimeDir := serviceRuntimePath(root, "exporter")
	triggerPath := filepath.Join(root, "rotate")
	donePath := filepath.Join(root, "rotated")
	cfg := ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "exporter",
		Enabled:       true,
		Exports:       true,
		Command: []string{"/bin/sh", "-c", fmt.Sprintf(`
			tmp="$PUA_SERVICE_EXPORT_PATH.tmp"
			printf '%%s' '{"schemaVersion":1,"variables":{"PUBLIC":"initial"},"secrets":{"TOKEN":"initial-secret"}}' > "$tmp"
			mv "$tmp" "$PUA_SERVICE_EXPORT_PATH"
			while [ ! -f %s ]; do sleep 0.01; done
			printf '%%s' '{"schemaVersion":1,"variables":{"PUBLIC":"updated"}}' > "$tmp"
			mv "$tmp" "$PUA_SERVICE_EXPORT_PATH"
			printf 'ordinary-output\n'
			while [ ! -f %s ]; do sleep 0.01; done
			printf '%%s' '{"schemaVersion":1,"variables":{"PUBLIC":"rejected"},"secrets":{"TOKEN":"rotated-secret"}}' > "$tmp"
			mv "$tmp" "$PUA_SERVICE_EXPORT_PATH"
			printf 'rotated-secret\n'
			printf 'rotated-secret\n' >&2
			: > %s
			sleep 2
		`, shellQuote(filepath.Join(root, "update-variable")), shellQuote(triggerPath), shellQuote(donePath))},
		Restart: ServiceRestartConfig{InitialDelay: time.Second, Multiplier: 2, MaxDelay: time.Second},
	}
	writeTestService(t, root, cfg)
	m, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "update-variable"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForTestPath(t, filepath.Join(runtimeDir, "stdout.log"), "ordinary-output")
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	exports, err := m.Exports("exporter")
	if err != nil {
		t.Fatal(err)
	}
	if got := exports.Variables["PUBLIC"]; got != "updated" {
		t.Fatalf("public variable update = %q, want updated", got)
	}
	if err := os.WriteFile(triggerPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForTestFile(t, donePath)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err := m.Show("exporter")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceStateBackoff && status.State != ServiceStateAttentionRequired {
		t.Fatalf("state = %q, want rejected export to stop service", status.State)
	}
	if !strings.Contains(status.LastError, "immutable") {
		t.Fatalf("last error = %q, want immutable export contract", status.LastError)
	}
	projection, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(projection), "rotated-secret") {
		t.Fatalf("rotated secret appeared in API projection: %s", projection)
	}
	exports, err = m.Exports("exporter")
	if err != nil {
		t.Fatal(err)
	}
	exportProjection, err := json.Marshal(exports)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(exportProjection), "rotated-secret") {
		t.Fatalf("rotated secret appeared in export projection: %s", exportProjection)
	}
	for _, name := range []string{"stdout.log", "stderr.log", "state.json", "events.jsonl", "export.json"} {
		data, err := os.ReadFile(filepath.Join(runtimeDir, name))
		if err != nil && os.IsNotExist(err) && name == "stderr.log" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "rotated-secret") {
			t.Fatalf("rotated secret appeared in %s: %s", name, data)
		}
	}
}

func TestServiceManagerPreservesExportReplacementForLogGuard(t *testing.T) {
	const (
		initialSecret = "initial-handoff-secret"
		rotatedSecret = "rotated-handoff-secret"
	)
	root := t.TempDir()
	runtimeDir := serviceRuntimePath(root, "exporter")
	path := filepath.Join(runtimeDir, "export.json")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := &ServiceManager{root: root, now: time.Now}
	rt := &serviceRuntime{
		config:        ServiceConfig{SchemaVersion: serviceSchemaVersion, ID: "exporter", Exports: true},
		secretNames:   map[string]ServiceSecretMetadata{},
		exportSecrets: map[string]string{},
		redactor:      security.NewRedactor(),
	}
	initial := ServiceExportFile{
		SchemaVersion: serviceExportSchema,
		Variables:     map[string]string{"PUBLIC": "initial"},
		Secrets:       map[string]string{"TOKEN": initialSecret},
	}
	if err := writeServiceJSON(path, initial, 0o644); err != nil {
		t.Fatal(err)
	}
	handoff, err := openServiceExportHandoff(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(handoff.data, []byte(initialSecret)) {
		t.Fatal("opened hand-off did not retain the initial export")
	}
	rotated := ServiceExportFile{
		SchemaVersion: serviceExportSchema,
		Variables:     map[string]string{"PUBLIC": "rotated"},
		Secrets:       map[string]string{"TOKEN": rotatedSecret},
	}
	if err := writeServiceJSON(path, rotated, 0o644); err != nil {
		_ = handoff.file.Close()
		t.Fatal(err)
	}
	accepted, err := m.readExportHandoffWithGateLocked(rt, handoff, false)
	if err != nil {
		_ = handoff.file.Close()
		t.Fatal(err)
	}
	rt.exports = accepted
	if _, err := handoff.file.Seek(0, io.SeekStart); err != nil {
		_ = handoff.file.Close()
		t.Fatal(err)
	}
	scrubbedInitial, err := io.ReadAll(handoff.file)
	initialInfo, statErr := handoff.file.Stat()
	if closeErr := handoff.file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if statErr != nil {
		t.Fatal(statErr)
	}
	if bytes.Contains(scrubbedInitial, []byte(initialSecret)) {
		t.Fatalf("accepted inode retained its secret: %s", scrubbedInitial)
	}
	if initialInfo.Mode().Perm() != 0o600 {
		t.Fatalf("accepted inode mode = %o, want 600", initialInfo.Mode().Perm())
	}

	pathnameData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(pathnameData, []byte(rotatedSecret)) || bytes.Contains(pathnameData, []byte(initialSecret)) {
		t.Fatalf("concurrent pathname replacement was overwritten: %s", pathnameData)
	}

	logPath := filepath.Join(runtimeDir, "stdout.log")
	writer := newServiceLogWriter(
		newServiceLogSink(logPath, ServiceLogRotationConfig{}),
		rt.redactor,
		false,
		func() error { return m.guardServiceLogExport(rt) },
	)
	if written, err := writer.Write([]byte("ordinary-output " + rotatedSecret + "\n")); err == nil || written != 0 {
		t.Fatalf("guarded write = %d, %v; want rejection before persistence", written, err)
	}
	_ = writer.Close()
	if !rt.redactor.ContainsSecret([]byte(rotatedSecret)) {
		t.Fatal("log guard rejected the replacement without registering its secret")
	}
	if err := m.exportProtocolErrorLocked(rt); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("protocol error = %v, want immutable export rejection", err)
	}
	if !containsString(rt.secretValues, rotatedSecret) {
		t.Fatal("rejected replacement secret was not promoted for durable redaction")
	}

	for _, durablePath := range []string{path, logPath} {
		data, err := os.ReadFile(durablePath)
		if err != nil && os.IsNotExist(err) && durablePath == logPath {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(rotatedSecret)) {
			t.Fatalf("replacement secret reached %s: %s", filepath.Base(durablePath), data)
		}
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("replacement hand-off mode = %o, want 600", info.Mode().Perm())
	}
	projection, err := json.Marshal(publicExports(rt.exports, rt.secretNames))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(projection, []byte(rotatedSecret)) || bytes.Contains(projection, []byte("rotated")) {
		t.Fatalf("rejected replacement reached visible exports: %s", projection)
	}
}

func TestServiceManagerDiscardsTimedOutStartupLogsWithoutExport(t *testing.T) {
	root := t.TempDir()
	cfg := ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "exporter",
		Enabled:       true,
		Exports:       true,
		Command:       []string{"/bin/sh", "-c", "printf 'unknown-startup-secret'; sleep 2"},
		Readiness:     &ServiceReadinessConfig{Command: []string{"/bin/sh", "-c", "exit 0"}, Interval: time.Second, Timeout: 100 * time.Millisecond},
		Restart:       ServiceRestartConfig{InitialDelay: time.Second, Multiplier: 2, MaxDelay: time.Second},
	}
	writeTestService(t, root, cfg)
	m, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"stdout.log", "stderr.log"} {
		data, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "exporter"), name))
		if err != nil && os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(data) != 0 {
			t.Fatalf("timed-out startup buffer reached %s: %q", name, data)
		}
	}
}

func TestServiceManagerRetainsExportSecretsAcrossReadinessPolls(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	producer := ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "producer",
		Enabled:       true,
		Exports:       true,
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
