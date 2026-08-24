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

type repeatingServiceExportReader struct {
	value byte
	read  int
}

func (reader *repeatingServiceExportReader) Read(data []byte) (int, error) {
	for index := range data {
		data[index] = reader.value
	}
	reader.read += len(data)
	return len(data), nil
}

type failingServiceExportReader struct {
	data []byte
	err  error
}

func (reader *failingServiceExportReader) Read(data []byte) (int, error) {
	if len(reader.data) == 0 {
		return 0, reader.err
	}
	n := copy(data, reader.data)
	reader.data = reader.data[n:]
	return n, reader.err
}

func TestServiceManagerStateWireValues(t *testing.T) {
	states := []ServiceState{
		ServiceStateDisabled,
		ServiceStateStopped,
		ServiceStateBlocked,
		ServiceStateStarting,
		ServiceStateReady,
		ServiceStateBackoff,
		ServiceStateAttentionRequired,
	}
	data, err := json.Marshal(states)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), `["disabled","stopped","blocked","starting","ready","backoff","attention_required"]`; got != want {
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

func TestServiceDefinitionJSONRequiresOneValue(t *testing.T) {
	valid := `{"schemaVersion":1,"id":"worker","enabled":false,"command":["service"]}`
	for _, test := range []struct {
		name string
		data string
		want string
	}{
		{name: "concatenated objects", data: valid + `{"schemaVersion":1}`, want: "multiple JSON values"},
		{name: "trailing garbage", data: valid + ` trailing-definition-data`, want: "invalid trailing JSON data"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := serviceConfigPath(root, "worker")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadServiceConfig(path); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadServiceConfig error = %v, want %q", err, test.want)
			}
			if _, err := NewServiceManager(root, ServiceManagerOptions{}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewServiceManager error = %v, want %q", err, test.want)
			}
		})
	}

	root := t.TempDir()
	path := serviceConfigPath(root, "worker")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(valid+" \n\t"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadServiceConfig(path); err != nil {
		t.Fatalf("LoadServiceConfig rejected trailing whitespace: %v", err)
	}
	if _, err := NewServiceManager(root, ServiceManagerOptions{}); err != nil {
		t.Fatalf("NewServiceManager rejected trailing whitespace: %v", err)
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

func TestServiceManagerScopesExportsToDeclaredGeneration(t *testing.T) {
	root := t.TempDir()
	consumerOutput := filepath.Join(root, "consumer-output")
	disabledReady := filepath.Join(root, "disabled-ready")
	staleHandoffLink := filepath.Join(root, "stale-handoff-link")
	disabledHandoffLink := filepath.Join(root, "disabled-handoff-link")
	exportPath := filepath.Join(serviceRuntimePath(root, "producer"), "export.json")
	producer := ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "producer",
		Enabled:       true,
		Exports:       true,
		Command: []string{"/bin/sh", "-c", `
			tmp="$PUA_SERVICE_EXPORT_PATH.tmp"
			printf '%s' '{"schemaVersion":1,"variables":{"PUBLIC":"old-public"},"secrets":{"TOKEN":"old-secret"}}' > "$tmp"
			mv "$tmp" "$PUA_SERVICE_EXPORT_PATH"
			sleep 30
		`},
		Cleanup: &ServiceCleanupConfig{
			Command: []string{"/bin/sh", "-c", `
				printf '%s' '{"schemaVersion":1,"variables":{"PUBLIC":"stale-public"},"secrets":{"TOKEN":"stale-secret"}}' > "$PUA_SERVICE_EXPORT_PATH"
				ln "$PUA_SERVICE_EXPORT_PATH" ` + shellQuote(staleHandoffLink) + `
			`},
			Timeout: time.Second,
		},
		Restart: ServiceRestartConfig{InitialDelay: time.Millisecond, Multiplier: 2, MaxDelay: time.Second},
	}
	consumer := ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "consumer",
		Enabled:       true,
		Command:       []string{"/bin/sh", "-c", "printf '%s|%s' \"$PUBLIC\" \"$TOKEN\" > " + shellQuote(consumerOutput) + "; sleep 30"},
		Environment: map[string]ServiceEnvironment{
			"PUBLIC": {Template: "${service.producer.PUBLIC}"},
			"TOKEN":  {Template: "${service.producer.TOKEN}"},
		},
		DependsOn: []string{"producer"},
		Restart:   ServiceRestartConfig{InitialDelay: time.Millisecond, Multiplier: 2, MaxDelay: time.Second},
	}
	writeTestService(t, root, producer)
	writeTestService(t, root, consumer)
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Stop(context.Background()); err != nil {
			t.Errorf("stop manager: %v", err)
		}
	})
	if _, err := manager.ApplyBindings(ServiceBindings{
		Variables: map[string]string{"BOUND_PUBLIC": "${service.producer.PUBLIC}"},
		Secrets:   map[string]string{"BOUND_TOKEN": "${service.producer.TOKEN}"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForTestPath(t, consumerOutput, "old-public|old-secret")
	variables, secrets, err := manager.ResolveBindings()
	if err != nil {
		t.Fatal(err)
	}
	if variables["BOUND_PUBLIC"] != "old-public" || secrets["BOUND_TOKEN"] != "old-secret" {
		t.Fatalf("initial bindings = %#v, %#v", variables, secrets)
	}
	if err := os.Remove(consumerOutput); err != nil {
		t.Fatal(err)
	}

	disabled := producer
	disabled.Exports = false
	disabled.Cleanup = nil
	disabled.Command = []string{"/bin/sh", "-c", `
		tmp="$PUA_SERVICE_EXPORT_PATH.tmp"
		printf '%s' '{"schemaVersion":1,"variables":{"PUBLIC":"disabled-public"},"secrets":{"TOKEN":"disabled-secret"}}' > "$tmp"
		mv "$tmp" "$PUA_SERVICE_EXPORT_PATH"
		ln "$PUA_SERVICE_EXPORT_PATH" ` + shellQuote(disabledHandoffLink) + `
		: > ` + shellQuote(disabledReady) + `
		sleep 30
	`}
	disabled.Readiness = &ServiceReadinessConfig{
		Command:  []string{"/bin/sh", "-c", "while [ ! -f " + shellQuote(disabledReady) + " ]; do sleep 0.01; done"},
		Interval: time.Second,
		Timeout:  2 * time.Second,
	}
	if err := manager.Apply(disabled); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Show("producer")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceStateReady || len(status.Exports.Variables) != 0 || len(status.Exports.Secrets) != 0 {
		t.Fatalf("disabled-export generation published exports: %#v", status)
	}
	if _, err := os.Stat(exportPath); !os.IsNotExist(err) {
		data, _ := os.ReadFile(exportPath)
		t.Fatalf("disabled-export hand-off survived scrubbing: %v: %s", err, data)
	}
	for _, link := range []string{staleHandoffLink, disabledHandoffLink} {
		data, err := os.ReadFile(link)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) != 0 {
			t.Fatalf("disabled-export hand-off inode retained bytes in %s: %s", filepath.Base(link), data)
		}
	}
	if dependent, err := manager.Show("consumer"); err != nil {
		t.Fatal(err)
	} else if dependent.State == ServiceStateReady {
		t.Fatalf("dependent remained ready with disabled exports: %#v", dependent)
	}
	if _, err := os.Stat(consumerOutput); !os.IsNotExist(err) {
		data, _ := os.ReadFile(consumerOutput)
		t.Fatalf("dependent received stale exports: %v: %s", err, data)
	}
	if variables, secrets, err := manager.ResolveBindings(); err == nil {
		t.Fatalf("bindings resolved disabled exports: %#v, %#v", variables, secrets)
	}
	state, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "producer"), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, stale := range []string{"old-public", "old-secret", "stale-public", "stale-secret", "disabled-public", "disabled-secret"} {
		if bytes.Contains(state, []byte(stale)) {
			t.Fatalf("disabled-export state retained %q: %s", stale, state)
		}
	}

	reenabled := producer
	reenabled.Cleanup = nil
	reenabled.Command = []string{"/bin/sh", "-c", `
		tmp="$PUA_SERVICE_EXPORT_PATH.tmp"
		printf '%s' '{"schemaVersion":1,"variables":{"PUBLIC":"new-public"},"secrets":{"TOKEN":"new-secret"}}' > "$tmp"
		mv "$tmp" "$PUA_SERVICE_EXPORT_PATH"
		sleep 30
	`}
	if err := manager.Apply(reenabled); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForTestPath(t, consumerOutput, "new-public|new-secret")
	variables, secrets, err = manager.ResolveBindings()
	if err != nil {
		t.Fatal(err)
	}
	if variables["BOUND_PUBLIC"] != "new-public" || secrets["BOUND_TOKEN"] != "new-secret" {
		t.Fatalf("re-enabled bindings = %#v, %#v", variables, secrets)
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

func TestServiceManagerRejectedExportJSONIsScrubbedAndOpaque(t *testing.T) {
	for _, test := range []struct {
		name   string
		data   string
		secret string
	}{
		{
			name:   "concatenated objects",
			data:   `{"schemaVersion":1}{"schemaVersion":1,"secrets":{"TOKEN":"concatenated-export-secret"}}`,
			secret: "concatenated-export-secret",
		},
		{
			name:   "trailing garbage",
			data:   `{"schemaVersion":1} trailing-export-secret`,
			secret: "trailing-export-secret",
		},
		{
			name:   "unknown field",
			data:   `{"schemaVersion":1,"unknown":"unknown-export-secret"}`,
			secret: "unknown-export-secret",
		},
		{
			name:   "malformed object",
			data:   `{"schemaVersion":1,"secrets":{"TOKEN":"malformed-export-secret"}`,
			secret: "malformed-export-secret",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(serviceRuntimePath(root, "exporter"), "export.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(test.data), 0o644); err != nil {
				t.Fatal(err)
			}
			manager := &ServiceManager{root: root, now: time.Now}
			runtime := &serviceRuntime{
				config:   ServiceConfig{SchemaVersion: serviceSchemaVersion, ID: "exporter", Exports: true},
				redactor: security.NewRedactor(),
			}
			if _, err := manager.readExportsLocked(runtime); err == nil {
				t.Fatal("rejected export was accepted")
			} else if strings.Contains(err.Error(), test.secret) || !strings.Contains(err.Error(), "invalid JSON hand-off") {
				t.Fatalf("rejected export error disclosed input or lost protocol context: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte(test.secret)) || !json.Valid(data) {
				t.Fatalf("rejected export was not safely scrubbed: %s", data)
			}
			if info, err := os.Stat(path); err != nil {
				t.Fatal(err)
			} else if info.Mode().Perm() != 0o600 {
				t.Fatalf("scrubbed export mode = %o, want 600", info.Mode().Perm())
			}
		})
	}
}

func TestServiceManagerBoundsExportHandoffWhileReading(t *testing.T) {
	stream := &repeatingServiceExportReader{value: 'x'}
	data, err := readBoundedServiceExport(stream)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != serviceExportMaxBytes+1 {
		t.Fatalf("bounded read length = %d, want %d", len(data), serviceExportMaxBytes+1)
	}
	if stream.read != serviceExportMaxBytes+1 {
		t.Fatalf("underlying streaming read = %d bytes, want %d", stream.read, serviceExportMaxBytes+1)
	}

	injected := errors.New("injected service export read failure")
	partial := []byte("partial-export")
	data, err = readBoundedServiceExport(&failingServiceExportReader{data: append([]byte(nil), partial...), err: injected})
	if !errors.Is(err, injected) {
		t.Fatalf("bounded read error = %v, want injected failure", err)
	}
	if !bytes.Equal(data, partial) {
		t.Fatalf("bounded partial read = %q, want %q", data, partial)
	}
}

func TestServiceManagerExportHandoffSizeBoundary(t *testing.T) {
	valid := []byte(`{"schemaVersion":1,"variables":{"PUBLIC":"visible"}}`)
	if len(valid) >= serviceExportMaxBytes {
		t.Fatalf("test export length = %d, want below limit", len(valid))
	}

	for _, test := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "exact limit", size: serviceExportMaxBytes},
		{name: "sentinel byte", size: serviceExportMaxBytes + 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(serviceRuntimePath(root, "exporter"), "export.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			data := append([]byte(nil), valid...)
			data = append(data, bytes.Repeat([]byte{' '}, test.size-len(data))...)
			if err := os.WriteFile(path, data, 0o400); err != nil {
				t.Fatal(err)
			}
			manager := &ServiceManager{root: root, now: time.Now}
			runtime := &serviceRuntime{
				config:        ServiceConfig{SchemaVersion: serviceSchemaVersion, ID: "exporter", Exports: true},
				secretNames:   map[string]ServiceSecretMetadata{},
				exportSecrets: map[string]string{},
				redactor:      security.NewRedactor(),
			}
			export, err := manager.readExportsLocked(runtime)
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "exceeds 1 MiB") {
					t.Fatalf("oversized export error = %v, want size rejection", err)
				}
				scrubbed, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if len(scrubbed) >= serviceExportMaxBytes || !json.Valid(scrubbed) {
					t.Fatalf("oversized export was not replaced with bounded sanitized JSON: len=%d data=%q", len(scrubbed), scrubbed)
				}
				return
			}
			if err != nil {
				t.Fatalf("exact-limit export was rejected: %v", err)
			}
			if export.Variables["PUBLIC"] != "visible" {
				t.Fatalf("exact-limit export = %#v", export)
			}
		})
	}
}

func TestServiceManagerOversizedSparseExportScrubsHardlinks(t *testing.T) {
	const secret = "oversized-sparse-export-secret"
	root := t.TempDir()
	path := filepath.Join(serviceRuntimePath(root, "exporter"), "export.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o400)
	if err != nil {
		t.Fatal(err)
	}
	prefix := []byte(`{"schemaVersion":1,"secrets":{"TOKEN":"` + secret + `"}}`)
	if _, err := file.Write(prefix); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Truncate(int64(serviceExportMaxBytes * 64)); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	hardlink := filepath.Join(filepath.Dir(path), "export-hardlink.json")
	if err := os.Link(path, hardlink); err != nil {
		t.Fatal(err)
	}

	manager := &ServiceManager{root: root, now: time.Now}
	runtime := &serviceRuntime{
		config:   ServiceConfig{SchemaVersion: serviceSchemaVersion, ID: "exporter", Exports: true},
		redactor: security.NewRedactor(),
	}
	if _, err := manager.readExportsLocked(runtime); err == nil || !strings.Contains(err.Error(), "exceeds 1 MiB") {
		t.Fatalf("oversized sparse export error = %v, want size rejection", err)
	}
	for _, candidate := range []string{path, hardlink} {
		scrubbed, err := os.ReadFile(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(scrubbed, []byte(secret)) || len(scrubbed) >= serviceExportMaxBytes || !json.Valid(scrubbed) {
			t.Fatalf("oversized inode retained bytes through %s: len=%d data=%q", filepath.Base(candidate), len(scrubbed), scrubbed)
		}
	}
}

func TestServiceManagerWriteOpenFailureRemovesSecretHandoff(t *testing.T) {
	const secret = "write-open-failure-secret"
	root := t.TempDir()
	path := filepath.Join(serviceRuntimePath(root, "exporter"), "export.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"secrets":{"TOKEN":"`+secret+`"}}`), 0o400); err != nil {
		t.Fatal(err)
	}
	hardlink := filepath.Join(filepath.Dir(path), "export-hardlink.json")
	if err := os.Link(path, hardlink); err != nil {
		t.Fatal(err)
	}
	manager := &ServiceManager{
		root: root,
		now:  time.Now,
		exportOpenFile: func(path string, flag int, perm os.FileMode) (*os.File, error) {
			if flag&os.O_RDWR != 0 {
				return nil, errors.New("injected write-open failure")
			}
			return os.OpenFile(path, flag, perm)
		},
	}
	runtime := &serviceRuntime{
		config:   ServiceConfig{SchemaVersion: serviceSchemaVersion, ID: "exporter", Exports: true},
		redactor: security.NewRedactor(),
	}
	if _, err := manager.readExportsLocked(runtime); err == nil {
		t.Fatal("export with an unwritable hand-off was accepted")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("failed hand-off pathname remains: %v", err)
	}
	data, err := os.ReadFile(hardlink)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(secret)) || len(data) != 0 {
		t.Fatalf("failed hand-off hardlink retained secret bytes: %q", data)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".export-handoff-*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range matches {
		data, err := os.ReadFile(match)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("failed hand-off quarantine retained its secret: %s", data)
		}
	}
}

func TestServiceManagerReadOnlySecretHandoffIsAcceptedAndScrubbed(t *testing.T) {
	const secret = "read-only-export-secret"
	root := t.TempDir()
	path := filepath.Join(serviceRuntimePath(root, "exporter"), "export.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	export := ServiceExportFile{
		SchemaVersion: serviceExportSchema,
		Variables:     map[string]string{"PUBLIC": "visible"},
		Secrets:       map[string]string{"TOKEN": secret},
	}
	data, err := json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o400); err != nil {
		t.Fatal(err)
	}
	manager := &ServiceManager{root: root, now: time.Now}
	runtime := &serviceRuntime{
		config:        ServiceConfig{SchemaVersion: serviceSchemaVersion, ID: "exporter", Exports: true},
		secretNames:   map[string]ServiceSecretMetadata{},
		exportSecrets: map[string]string{},
		redactor:      security.NewRedactor(),
	}
	accepted, err := manager.readExportsLocked(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Variables["PUBLIC"] != "visible" || accepted.Secrets["TOKEN"] != secret {
		t.Fatalf("accepted export = %#v", accepted)
	}
	scrubbed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(scrubbed, []byte(secret)) || !bytes.Contains(scrubbed, []byte("visible")) {
		t.Fatalf("read-only export was not scrubbed: %s", scrubbed)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("scrubbed export mode = %o, want 600", info.Mode().Perm())
	}
}

func TestServiceManagerSecretHandoffDoesNotFollowSymlink(t *testing.T) {
	const secret = "symlink-target-secret"
	root := t.TempDir()
	runtimeDir := serviceRuntimePath(root, "exporter")
	path := filepath.Join(runtimeDir, "export.json")
	target := filepath.Join(runtimeDir, "target.json")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"schemaVersion":1,"secrets":{"TOKEN":"` + secret + `"}}`)
	if err := os.WriteFile(target, original, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	manager := &ServiceManager{root: root, now: time.Now}
	runtime := &serviceRuntime{
		config:   ServiceConfig{SchemaVersion: serviceSchemaVersion, ID: "exporter", Exports: true},
		redactor: security.NewRedactor(),
	}
	if _, err := manager.readExportsLocked(runtime); err == nil {
		t.Fatal("symlink export hand-off was accepted")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, original) {
		t.Fatalf("symlink target was changed: %s", data)
	}
	if info, err := os.Lstat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("export replacement mode = %v, want symlink", info.Mode())
	}
}

func TestServiceManagerWriteOpenFailurePreservesReplacementHandoff(t *testing.T) {
	const (
		originalSecret    = "failed-original-secret"
		replacementSecret = "preserved-replacement-secret"
	)
	root := t.TempDir()
	path := filepath.Join(serviceRuntimePath(root, "exporter"), "export.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"secrets":{"TOKEN":"`+originalSecret+`"}}`), 0o400); err != nil {
		t.Fatal(err)
	}
	replacement := ServiceExportFile{
		SchemaVersion: serviceExportSchema,
		Secrets:       map[string]string{"TOKEN": replacementSecret},
	}
	manager := &ServiceManager{root: root, now: time.Now}
	manager.exportOpenFile = func(openPath string, flag int, perm os.FileMode) (*os.File, error) {
		if flag&os.O_RDWR != 0 {
			if err := writeServiceJSON(path, replacement, 0o600); err != nil {
				return nil, err
			}
			return nil, errors.New("injected write-open failure after replacement")
		}
		return os.OpenFile(openPath, flag, perm)
	}
	runtime := &serviceRuntime{
		config:   ServiceConfig{SchemaVersion: serviceSchemaVersion, ID: "exporter", Exports: true},
		redactor: security.NewRedactor(),
	}
	if _, err := manager.readExportsLocked(runtime); err == nil {
		t.Fatal("replaced hand-off was accepted during the failed read")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(originalSecret)) || !bytes.Contains(data, []byte(replacementSecret)) {
		t.Fatalf("replacement hand-off was removed or changed: %s", data)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".export-handoff-*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range matches {
		data, err := os.ReadFile(match)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(originalSecret)) {
			t.Fatalf("replaced original remained in quarantine: %s", data)
		}
	}
}

func TestServiceManagerRetriesExportReplacedDuringIdentityCheck(t *testing.T) {
	const secret = "identity-replacement-secret"
	root := t.TempDir()
	path := filepath.Join(serviceRuntimePath(root, "exporter"), "export.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	writeExport := func(public string) {
		t.Helper()
		if err := writeServiceJSON(path, ServiceExportFile{
			SchemaVersion: serviceExportSchema,
			Variables:     map[string]string{"PUBLIC": public},
			Secrets:       map[string]string{"TOKEN": secret},
		}, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	writeExport("accepted")
	manager := &ServiceManager{root: root, now: time.Now}
	runtime := &serviceRuntime{
		config:        ServiceConfig{SchemaVersion: serviceSchemaVersion, ID: "exporter", Exports: true},
		secretNames:   map[string]ServiceSecretMetadata{},
		exportSecrets: map[string]string{},
		redactor:      security.NewRedactor(),
	}
	if _, err := manager.readExportsLocked(runtime); err != nil {
		t.Fatal(err)
	}

	writeExport("raced")
	hardlink := filepath.Join(filepath.Dir(path), "export-hardlink.json")
	readOpens := 0
	manager.exportOpenFile = func(openPath string, flag int, perm os.FileMode) (*os.File, error) {
		file, err := os.OpenFile(openPath, flag, perm)
		if err != nil {
			return nil, err
		}
		if flag&os.O_RDWR == 0 && readOpens == 0 {
			readOpens++
			temporary := path + ".replacement"
			if err := writeServiceJSON(temporary, ServiceExportFile{
				SchemaVersion: serviceExportSchema,
				Variables:     map[string]string{"PUBLIC": "latest"},
				Secrets:       map[string]string{"TOKEN": secret},
			}, 0o400); err != nil {
				_ = file.Close()
				return nil, err
			}
			if err := os.Rename(temporary, path); err != nil {
				_ = file.Close()
				return nil, err
			}
			if err := os.Link(path, hardlink); err != nil {
				_ = file.Close()
				return nil, err
			}
		}
		return file, nil
	}

	export, err := manager.readExportsLocked(runtime)
	if err != nil {
		t.Fatalf("verified replacement was not retried: %v", err)
	}
	if export.Variables["PUBLIC"] != "latest" {
		t.Fatalf("accepted export = %#v, want latest replacement", export)
	}
	if runtime.exportViolation != "" {
		t.Fatalf("normal atomic replacement latched violation %q", runtime.exportViolation)
	}
	if readOpens != 1 {
		t.Fatalf("injected read replacements = %d, want 1", readOpens)
	}
	for _, candidate := range []string{path, hardlink} {
		data, err := os.ReadFile(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("verified replacement retained secret in %s: %s", candidate, data)
		}
	}
}

func TestServiceManagerBoundsRepeatedExportIdentityChanges(t *testing.T) {
	const secret = "repeated-identity-change-secret"
	root := t.TempDir()
	path := filepath.Join(serviceRuntimePath(root, "exporter"), "export.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	writeExport := func(public string) error {
		return writeServiceJSON(path, ServiceExportFile{
			SchemaVersion: serviceExportSchema,
			Variables:     map[string]string{"PUBLIC": public},
			Secrets:       map[string]string{"TOKEN": secret},
		}, 0o400)
	}
	if err := writeExport("accepted"); err != nil {
		t.Fatal(err)
	}
	manager := &ServiceManager{root: root, now: time.Now}
	runtime := &serviceRuntime{
		config:        ServiceConfig{SchemaVersion: serviceSchemaVersion, ID: "exporter", Exports: true},
		secretNames:   map[string]ServiceSecretMetadata{},
		exportSecrets: map[string]string{},
		redactor:      security.NewRedactor(),
	}
	if _, err := manager.readExportsLocked(runtime); err != nil {
		t.Fatal(err)
	}
	if err := writeExport("raced"); err != nil {
		t.Fatal(err)
	}

	hardlink := filepath.Join(filepath.Dir(path), "latest-hardlink.json")
	readOpens := 0
	manager.exportOpenFile = func(openPath string, flag int, perm os.FileMode) (*os.File, error) {
		file, err := os.OpenFile(openPath, flag, perm)
		if err != nil {
			return nil, err
		}
		if flag&os.O_RDWR != 0 {
			return file, nil
		}
		readOpens++
		temporary := path + ".replacement"
		if err := writeServiceJSON(temporary, ServiceExportFile{
			SchemaVersion: serviceExportSchema,
			Variables:     map[string]string{"PUBLIC": fmt.Sprintf("replacement-%d", readOpens)},
			Secrets:       map[string]string{"TOKEN": secret},
		}, 0o400); err != nil {
			_ = file.Close()
			return nil, err
		}
		if err := os.Rename(temporary, path); err != nil {
			_ = file.Close()
			return nil, err
		}
		if readOpens == serviceExportIdentityCheckAttempts {
			if err := os.Link(path, hardlink); err != nil {
				_ = file.Close()
				return nil, err
			}
		}
		return file, nil
	}

	if _, err := manager.readExportsLocked(runtime); err == nil || !strings.Contains(err.Error(), "changed repeatedly") {
		t.Fatalf("repeated identity changes error = %v", err)
	}
	if readOpens != serviceExportIdentityCheckAttempts {
		t.Fatalf("identity attempts = %d, want %d", readOpens, serviceExportIdentityCheckAttempts)
	}
	manager.exportOpenFile = nil
	if _, err := manager.readExportsLocked(runtime); err == nil || !strings.Contains(err.Error(), "changed repeatedly") {
		t.Fatalf("latched identity violation error = %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("violating pathname remains after fail-closed read: %v", err)
	}
	data, err := os.ReadFile(hardlink)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(secret)) || len(data) != 0 {
		t.Fatalf("violating hand-off hardlink retained secret: %q", data)
	}
}

func TestServiceManagerStopScrubsNewHandoffAfterViolation(t *testing.T) {
	const (
		acceptedSecret = "accepted-before-violation-secret"
		newSecret      = "new-after-violation-secret"
	)
	root := t.TempDir()
	cfg := ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "exporter",
		Enabled:       true,
		Exports:       true,
		Command:       []string{"/usr/bin/true"},
	}
	writeTestService(t, root, cfg)
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	runtime := manager.runtimes[cfg.ID]
	runtime.secretNames = map[string]ServiceSecretMetadata{}
	runtime.exportSecrets = map[string]string{}
	runtime.redactor = security.NewRedactor()
	path := filepath.Join(serviceRuntimePath(root, cfg.ID), "export.json")
	if err := writeServiceJSON(path, ServiceExportFile{
		SchemaVersion: serviceExportSchema,
		Secrets:       map[string]string{"TOKEN": acceptedSecret},
	}, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.readExportsLocked(runtime); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schemaVersion":99,"secrets":{"TOKEN":"rejected"}}`), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.readExportsLocked(runtime); err == nil {
		t.Fatal("invalid replacement did not latch a protocol violation")
	}
	if err := writeServiceJSON(path, ServiceExportFile{
		SchemaVersion: serviceExportSchema,
		Secrets:       map[string]string{"TOKEN": newSecret},
	}, 0o400); err != nil {
		t.Fatal(err)
	}
	hardlink := filepath.Join(filepath.Dir(path), "stopped-hardlink.json")
	if err := os.Link(path, hardlink); err != nil {
		t.Fatal(err)
	}

	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("violating pathname remains after stop: %v", err)
	}
	data, err := os.ReadFile(hardlink)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(newSecret)) || len(data) != 0 {
		t.Fatalf("stopped hand-off hardlink retained secret: %q", data)
	}
	if _, err := NewServiceManager(root, ServiceManagerOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(hardlink)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("manager restart restored scrubbed secret bytes: %q", data)
	}
}

func TestServiceManagerStopReportsExportScrubFailureAfterViolation(t *testing.T) {
	const secret = "unscrubbed-stop-secret"
	root := t.TempDir()
	cfg := ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "exporter",
		Enabled:       true,
		Exports:       true,
		Command:       []string{"/usr/bin/true"},
	}
	writeTestService(t, root, cfg)
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	runtime := manager.runtimes[cfg.ID]
	runtime.exportViolation = "existing export protocol violation"
	path := filepath.Join(serviceRuntimePath(root, cfg.ID), "export.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(secret), 0o400); err != nil {
		t.Fatal(err)
	}
	scrubFailure := errors.New("injected export scrub failure")
	manager.exportScrubPath = func(string) error { return scrubFailure }

	err = manager.Stop(context.Background())
	if err == nil || !errors.Is(err, scrubFailure) || !strings.Contains(err.Error(), "existing export protocol violation") {
		t.Fatalf("stop error = %v, want protocol and scrub failures", err)
	}
	if runtime.status.State != ServiceStateAttentionRequired || !runtime.status.AttentionRequired {
		t.Fatalf("status after scrub failure = %#v", runtime.status)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Contains(data, []byte(secret)) {
		t.Fatalf("injected failure did not preserve the regression fixture: %q", data)
	}
}

func TestRemoveVerifiedServiceExportHandoffScrubsUnrestoredReplacement(t *testing.T) {
	const quarantinedSecret = "quarantined-replacement-secret"
	root := t.TempDir()
	path := filepath.Join(root, "export.json")
	original := []byte(`{"schemaVersion":1,"variables":{"PUBLIC":"original"}}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	replacement := []byte(`{"schemaVersion":1,"secrets":{"TOKEN":"` + quarantinedSecret + `"}}`)
	newer := []byte(`{"schemaVersion":1,"variables":{"PUBLIC":"newer"}}`)
	var quarantinePath string
	operations := defaultServiceExportHandoffOperations()
	operations.rename = func(oldPath, newPath string) error {
		replacementPath := oldPath + ".replacement"
		if err := os.WriteFile(replacementPath, replacement, 0o400); err != nil {
			return err
		}
		if err := os.Link(replacementPath, filepath.Join(root, "replacement-hardlink")); err != nil {
			return err
		}
		if err := os.Rename(replacementPath, oldPath); err != nil {
			return err
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			return err
		}
		quarantinePath = newPath
		return os.WriteFile(oldPath, newer, 0o600)
	}

	err = removeVerifiedServiceExportHandoffWithOperations(path, opened, operations)
	if err == nil || !errors.Is(err, os.ErrExist) || !strings.Contains(err.Error(), "restore concurrently replaced export hand-off") {
		t.Fatalf("remove error = %v, want restore failure", err)
	}
	active, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(active, newer) {
		t.Fatalf("newer export hand-off changed: %s", active)
	}
	if _, err := os.Lstat(quarantinePath); !os.IsNotExist(err) {
		t.Fatalf("quarantine remains after failed restore: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(quarantinedSecret)) {
			t.Fatalf("managed export directory retained secret in %s: %s", entry.Name(), data)
		}
	}
}

func TestRemoveVerifiedServiceExportHandoffJoinsQuarantineCleanupFailure(t *testing.T) {
	const quarantinedSecret = "cleanup-failure-secret"
	root := t.TempDir()
	path := filepath.Join(root, "export.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	newer := []byte(`{"schemaVersion":1,"variables":{"PUBLIC":"newer"}}`)
	var quarantinePath string
	cleanupFailure := errors.New("injected quarantine unlink failure")
	operations := defaultServiceExportHandoffOperations()
	operations.rename = func(oldPath, newPath string) error {
		replacementPath := oldPath + ".replacement"
		if err := os.WriteFile(replacementPath, []byte(quarantinedSecret), 0o600); err != nil {
			return err
		}
		if err := os.Rename(replacementPath, oldPath); err != nil {
			return err
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			return err
		}
		quarantinePath = newPath
		return os.WriteFile(oldPath, newer, 0o600)
	}
	operations.remove = func(removePath string) error {
		if removePath == quarantinePath {
			return cleanupFailure
		}
		return os.Remove(removePath)
	}

	err = removeVerifiedServiceExportHandoffWithOperations(path, opened, operations)
	if err == nil || !errors.Is(err, os.ErrExist) || !errors.Is(err, cleanupFailure) ||
		!strings.Contains(err.Error(), "restore concurrently replaced export hand-off") ||
		!strings.Contains(err.Error(), "discard unrestored export hand-off") ||
		!strings.Contains(err.Error(), "injected quarantine unlink failure") {
		t.Fatalf("remove error = %v, want joined restore and cleanup failures", err)
	}
	active, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(active, newer) {
		t.Fatalf("newer export hand-off changed: %s", active)
	}
	quarantine, err := os.ReadFile(quarantinePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(quarantine, []byte(quarantinedSecret)) || len(quarantine) != 0 {
		t.Fatalf("failed quarantine unlink retained secret bytes: %q", quarantine)
	}
}

func TestServiceManagerRejectedExportScrubsOpenedDescriptorBeforeReplacement(t *testing.T) {
	const (
		openedSecret      = "opened-rejected-secret"
		replacementSecret = "replacement-accepted-secret"
	)
	root := t.TempDir()
	path := filepath.Join(serviceRuntimePath(root, "exporter"), "export.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1}{"secrets":{"TOKEN":"`+openedSecret+`"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	handoff, err := openServiceExportHandoff(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := ServiceExportFile{
		SchemaVersion: serviceExportSchema,
		Variables:     map[string]string{"PUBLIC": "visible"},
		Secrets:       map[string]string{"TOKEN": replacementSecret},
	}
	if err := writeServiceJSON(path, replacement, 0o644); err != nil {
		_ = handoff.file.Close()
		t.Fatal(err)
	}
	manager := &ServiceManager{root: root, now: time.Now}
	runtime := &serviceRuntime{
		config:        ServiceConfig{SchemaVersion: serviceSchemaVersion, ID: "exporter", Exports: true},
		secretNames:   map[string]ServiceSecretMetadata{},
		exportSecrets: map[string]string{},
		redactor:      security.NewRedactor(),
	}
	if _, err := manager.readExportHandoffWithGateLocked(runtime, handoff, false); err == nil {
		_ = handoff.file.Close()
		t.Fatal("opened rejected hand-off was accepted")
	}
	if _, err := handoff.file.Seek(0, io.SeekStart); err != nil {
		_ = handoff.file.Close()
		t.Fatal(err)
	}
	openedData, err := io.ReadAll(handoff.file)
	if closeErr := handoff.file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(openedData, []byte(openedSecret)) || !json.Valid(openedData) {
		t.Fatalf("opened rejected inode was not scrubbed: %s", openedData)
	}
	pathnameData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(pathnameData, []byte(replacementSecret)) || bytes.Contains(pathnameData, []byte(openedSecret)) {
		t.Fatalf("pathname replacement was overwritten: %s", pathnameData)
	}
	accepted, err := manager.readExportsLocked(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Variables["PUBLIC"] != "visible" || accepted.Secrets["TOKEN"] != replacementSecret {
		t.Fatalf("replacement export = %#v", accepted)
	}
	pathnameData, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(pathnameData, []byte(replacementSecret)) {
		t.Fatalf("accepted replacement retained its secret: %s", pathnameData)
	}
}

func TestServiceManagerTrailingWhitespaceExportRemainsVisible(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(serviceRuntimePath(root, "exporter"), "export.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"variables":{"PUBLIC":"visible"}}`+" \n\t"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &ServiceManager{root: root, now: time.Now}
	runtime := &serviceRuntime{config: ServiceConfig{ID: "exporter", Exports: true}, redactor: security.NewRedactor()}
	export, err := manager.readExportsLocked(runtime)
	if err != nil {
		t.Fatalf("trailing whitespace export was rejected: %v", err)
	}
	if export.Variables["PUBLIC"] != "visible" {
		t.Fatalf("visible export variables = %#v", export.Variables)
	}
}

func TestServiceManagerRejectedExportDoesNotReachDurableOutputs(t *testing.T) {
	const secret = "strict-tail-secret-marker"
	root := t.TempDir()
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "exporter",
		Enabled:       true,
		Exports:       true,
		Command: []string{"/bin/sh", "-c", `
			printf '%s' '{"schemaVersion":1}{"schemaVersion":1,"secrets":{"TOKEN":"strict-tail-secret-marker"}}' > "$PUA_SERVICE_EXPORT_PATH"
			printf '%s\n' 'strict-tail-secret-marker'
			exec sleep 30
		`},
		Restart: ServiceRestartConfig{InitialDelay: time.Second, Multiplier: 2, MaxDelay: time.Second},
	})
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stopProcessTestManager(t, &manager)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Show("exporter")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ServiceStateBackoff && status.State != ServiceStateAttentionRequired {
		t.Fatalf("state = %q, want rejected export failure", status.State)
	}
	projection, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(projection, []byte(secret)) {
		t.Fatalf("public status disclosed rejected export bytes: %s", projection)
	}
	for _, name := range []string{"export.json", "stdout.log", "stderr.log", "state.json", "events.jsonl"} {
		data, err := os.ReadFile(filepath.Join(serviceRuntimePath(root, "exporter"), name))
		if err != nil && os.IsNotExist(err) && (name == "export.json" || name == "stdout.log" || name == "stderr.log") {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("rejected export bytes reached %s: %s", name, data)
		}
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
		if err != nil && os.IsNotExist(err) && (name == "stderr.log" || name == "export.json") {
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

func TestServiceManagerRedactsFailedReadinessExportReplacement(t *testing.T) {
	for _, test := range []struct {
		name          string
		replacement   string
		candidateName string
		candidate     string
	}{
		{
			name:          "rejected secret rotation",
			replacement:   `{"schemaVersion":1,"variables":{"PUBLIC":"rejected-public"},"secrets":{"REJECTED_TOKEN_NAME":"rejected-readiness-secret"}}`,
			candidateName: "REJECTED_TOKEN_NAME",
			candidate:     "rejected-readiness-secret",
		},
		{
			name:          "malformed secret candidate",
			replacement:   `{"schemaVersion":1,"secrets":{"MALFORMED_TOKEN_NAME":"malformed-readiness-secret"`,
			candidateName: "MALFORMED_TOKEN_NAME",
			candidate:     "malformed-readiness-secret",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			runtimeDir := serviceRuntimePath(root, "exporter")
			cfg := ServiceConfig{
				SchemaVersion: serviceSchemaVersion,
				ID:            "exporter",
				Enabled:       true,
				Exports:       true,
				Command: []string{"/bin/sh", "-c", `
					tmp="$PUA_SERVICE_EXPORT_PATH.tmp"
					printf '%s' '{"schemaVersion":1,"variables":{"PUBLIC":"initial"},"secrets":{"INITIAL_TOKEN":"initial-readiness-secret"}}' > "$tmp"
					mv "$tmp" "$PUA_SERVICE_EXPORT_PATH"
					exec sleep 30
				`},
				Readiness: &ServiceReadinessConfig{
					Command: []string{"/bin/sh", "-c", fmt.Sprintf(`
						tmp="$PUA_SERVICE_EXPORT_PATH.tmp"
						printf '%%s' %s > "$tmp"
						mv "$tmp" "$PUA_SERVICE_EXPORT_PATH"
						printf 'candidate-name=%%s\n' %s
						printf 'candidate-value=%%s\n' %s >&2
						exit 23
					`, shellQuote(test.replacement), shellQuote(test.candidateName), shellQuote(test.candidate))},
					Interval: time.Second,
					Timeout:  time.Second,
				},
				Restart: ServiceRestartConfig{InitialDelay: time.Second, Multiplier: 2, MaxDelay: time.Second},
			}
			writeTestService(t, root, cfg)
			manager, err := NewServiceManager(root, ServiceManagerOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Stop(context.Background()) })

			status, err := manager.Show("exporter")
			if err != nil {
				t.Fatal(err)
			}
			if status.State != ServiceStateBackoff && status.State != ServiceStateAttentionRequired {
				t.Fatalf("state = %q, want failed readiness to stop service", status.State)
			}
			if status.Readiness.Ready || status.Exports.Variables["PUBLIC"] == "rejected-public" {
				t.Fatalf("failed readiness candidate was published: %#v", status)
			}
			projection, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			assertNoReadinessCandidate := func(location string, data []byte) {
				t.Helper()
				for _, forbidden := range []string{test.candidateName, test.candidate} {
					if bytes.Contains(data, []byte(forbidden)) {
						t.Fatalf("failed readiness candidate %q reached %s: %s", forbidden, location, data)
					}
				}
			}
			assertNoReadinessCandidate("API status", projection)
			for _, name := range []string{"events.jsonl", "state.json"} {
				data, err := os.ReadFile(filepath.Join(runtimeDir, name))
				if err != nil {
					t.Fatal(err)
				}
				assertNoReadinessCandidate(name, data)
			}
			for _, name := range []string{"stdout.log", "stderr.log"} {
				data, err := os.ReadFile(filepath.Join(runtimeDir, name))
				if err != nil && os.IsNotExist(err) {
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				assertNoReadinessCandidate(name, data)
			}
			handoff, err := os.ReadFile(filepath.Join(runtimeDir, "export.json"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			assertNoReadinessCandidate("export.json", handoff)
			if err == nil && !json.Valid(handoff) {
				t.Fatalf("failed readiness hand-off was not replaced with valid sanitized JSON: %s", handoff)
			}
		})
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
		func() error { return m.guardServiceLogExportForConfig(rt, rt.config) },
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
	const secret = "new-export-secret"
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
		Variables:     map[string]string{"PUBLIC_TOKEN": secret},
		Secrets:       map[string]string{"TOKEN": secret},
	}
	if err := writeServiceJSON(path, export, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.readExportsLocked(rt); err == nil || !strings.Contains(err.Error(), "contains a secret") {
		t.Fatalf("read export error = %v, want secret variable rejection", err)
	}
	if !containsString(rt.secretValues, secret) || !rt.redactor.ContainsSecret([]byte(secret)) {
		t.Fatal("rejected export secret was not retained for later redaction")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(secret)) {
		t.Fatalf("rejected validation retained its secret: %s", data)
	}
}
