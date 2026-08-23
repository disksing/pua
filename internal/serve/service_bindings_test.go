package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func writeTestBindingsData(t *testing.T, root, data string) {
	t.Helper()
	path := serviceBindingsPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestServiceManagerBindingReadsShareStrictValidation(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		sensitive string
	}{
		{
			name:      "unknown field",
			data:      `{"schemaVersion":1,"variables":{},"unknown":"unknown-field-secret"}`,
			sensitive: "unknown-field-secret",
		},
		{
			name:      "domain validation",
			data:      `{"schemaVersion":1,"secrets":{"TOKEN":"domain-validation-secret"}}`,
			sensitive: "domain-validation-secret",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			initialRoot := t.TempDir()
			writeTestBindingsData(t, initialRoot, test.data)
			_, initialErr := NewServiceManager(initialRoot, ServiceManagerOptions{})
			if initialErr == nil {
				t.Fatal("manager initialization accepted malformed bindings")
			}

			liveRoot := t.TempDir()
			manager, err := NewServiceManager(liveRoot, ServiceManagerOptions{})
			if err != nil {
				t.Fatal(err)
			}
			writeTestBindingsData(t, liveRoot, test.data)
			_, bindingsErr := manager.Bindings()
			if bindingsErr == nil {
				t.Fatal("Bindings accepted malformed bindings")
			}
			_, _, resolveErr := manager.ResolveBindings()
			if resolveErr == nil {
				t.Fatal("ResolveBindings accepted malformed bindings")
			}

			for entryPoint, got := range map[string]error{
				"Bindings":        bindingsErr,
				"ResolveBindings": resolveErr,
			} {
				if got.Error() != initialErr.Error() {
					t.Errorf("%s error = %q, initialization error = %q", entryPoint, got, initialErr)
				}
			}
			for entryPoint, got := range map[string]error{
				"initialization":  initialErr,
				"Bindings":        bindingsErr,
				"ResolveBindings": resolveErr,
			} {
				if strings.Contains(got.Error(), test.sensitive) {
					t.Errorf("%s error disclosed sensitive binding value: %v", entryPoint, got)
				}
			}
		})
	}
}

func TestServiceManagerBindingReadsShareMissingDefault(t *testing.T) {
	manager, err := NewServiceManager(t.TempDir(), ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := ServiceBindings{
		SchemaVersion: serviceSchemaVersion,
		Variables:     map[string]string{},
		Secrets:       map[string]string{},
	}
	bindings, err := manager.Bindings()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bindings, want) {
		t.Fatalf("missing bindings = %#v, want %#v", bindings, want)
	}
	variables, secrets, err := manager.ResolveBindings()
	if err != nil {
		t.Fatal(err)
	}
	if variables == nil || len(variables) != 0 || secrets == nil || len(secrets) != 0 {
		t.Fatalf("resolved missing bindings = %#v, %#v; want initialized empty maps", variables, secrets)
	}

	bindings.Variables["CALLER_MUTATION"] = "must-not-leak"
	again, err := manager.Bindings()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, want) {
		t.Fatalf("caller mutation leaked into later binding read: %#v", again)
	}
}

func TestServiceManagerBindingReadsShareValidData(t *testing.T) {
	root := t.TempDir()
	writeTestBindingsData(t, root, `{
		"schemaVersion": 1,
		"variables": {"PUBLIC_VALUE": "plain"},
		"secrets": {"API_TOKEN": "${secret.api-token}"}
	}`)
	manager, err := NewServiceManager(root, ServiceManagerOptions{
		Resolver: EnvironmentSecretResolver{Values: map[string]string{"api-token": "resolved-secret"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := ServiceBindings{
		SchemaVersion: serviceSchemaVersion,
		Variables:     map[string]string{"PUBLIC_VALUE": "plain"},
		Secrets:       map[string]string{"API_TOKEN": "${secret.api-token}"},
	}
	bindings, err := manager.Bindings()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bindings, want) {
		t.Fatalf("bindings = %#v, want %#v", bindings, want)
	}
	variables, secrets, err := manager.ResolveBindings()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(variables, map[string]string{"PUBLIC_VALUE": "plain"}) ||
		!reflect.DeepEqual(secrets, map[string]string{"API_TOKEN": "resolved-secret"}) {
		t.Fatalf("resolved bindings = %#v, %#v", variables, secrets)
	}
}

func TestServiceManagerReadsAndAppliesBindings(t *testing.T) {
	root := t.TempDir()
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "producer",
		Command:       []string{"/bin/echo", "ready"},
	})
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}

	empty, err := manager.Bindings()
	if err != nil {
		t.Fatal(err)
	}
	if empty.SchemaVersion != serviceSchemaVersion || empty.Variables == nil || empty.Secrets == nil {
		t.Fatalf("missing bindings = %#v, want initialized current-schema bindings", empty)
	}

	want := ServiceBindings{
		SchemaVersion: serviceSchemaVersion,
		Variables: map[string]string{
			"ENDPOINT": "http://${service.producer.HOST}",
		},
		Secrets: map[string]string{
			"API_TOKEN":    "${secret.api-token}",
			"EXPORT_TOKEN": "${service.producer.TOKEN}",
		},
	}
	applied, err := manager.ApplyBindings(ServiceBindings{
		Variables: cloneStringMap(want.Variables),
		Secrets:   cloneStringMap(want.Secrets),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(applied, want) {
		t.Fatalf("applied bindings = %#v, want %#v", applied, want)
	}
	loaded, err := manager.Bindings()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, want) {
		t.Fatalf("loaded bindings = %#v, want %#v", loaded, want)
	}

	info, err := os.Stat(serviceBindingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("bindings mode = %o, want 600", got)
	}
	if _, err := manager.ApplyBindings(ServiceBindings{
		Variables: map[string]string{"API_TOKEN": "${secret.api-token}"},
	}); err == nil || !strings.Contains(err.Error(), "must not appear") {
		t.Fatalf("secret variable validation error = %v", err)
	}
	unchanged, err := manager.Bindings()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(unchanged, want) {
		t.Fatalf("invalid update changed bindings to %#v", unchanged)
	}
}

func TestServiceManagerBindingsKeepExportSecretsOpaque(t *testing.T) {
	root := t.TempDir()
	writeTestService(t, root, ServiceConfig{
		SchemaVersion: serviceSchemaVersion,
		ID:            "producer",
		Enabled:       true,
		Exports:       true,
		Command: []string{"/bin/sh", "-c", `
			path="$PUA_SERVICE_EXPORT_PATH.tmp"
			printf '%s' '{"schemaVersion":1,"secrets":{"TOKEN":"manager-only-secret"}}' > "$path"
			mv "$path" "$PUA_SERVICE_EXPORT_PATH"
			sleep 10
		`},
	})
	manager, err := NewServiceManager(root, ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Stop(context.Background()); err != nil {
			t.Errorf("stop manager: %v", err)
		}
	})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.ApplyBindings(ServiceBindings{
		Variables: map[string]string{"TOKEN": "${service.producer.TOKEN}"},
	}); err == nil || !strings.Contains(err.Error(), "must be mapped under secrets") {
		t.Fatalf("export-secret variable validation error = %v", err)
	}
	bindings, err := manager.ApplyBindings(ServiceBindings{
		Secrets: map[string]string{"TOKEN": "${service.producer.TOKEN}"},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(serviceBindingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "manager-only-secret") {
		t.Fatalf("resolved export secret persisted in bindings: %s", data)
	}
	if got := bindings.Secrets["TOKEN"]; got != "${service.producer.TOKEN}" {
		t.Fatalf("returned secret binding = %q", got)
	}
}

func TestServiceManagerBindingsSerializeConcurrentAccess(t *testing.T) {
	manager, err := NewServiceManager(t.TempDir(), ServiceManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		worker := worker
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 10; iteration++ {
				_, err := manager.ApplyBindings(ServiceBindings{
					Variables: map[string]string{"WORKER": string(rune('A' + worker))},
				})
				if err != nil {
					t.Errorf("apply bindings: %v", err)
					return
				}
				if _, err := manager.Bindings(); err != nil {
					t.Errorf("read bindings: %v", err)
					return
				}
			}
		}()
	}
	wait.Wait()
	if _, err := manager.Bindings(); err != nil {
		t.Fatal(err)
	}
}

func TestHandleServiceBindingsPreservesWireBehavior(t *testing.T) {
	root := t.TempDir()
	server := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := server.saveConfig(config{
		Version:    agentHubConfigVersion,
		Workspaces: []serveWorkspace{{ID: "workspace-one", Path: root}},
	}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPut, "/api/workspaces/workspace-one/services/bindings", strings.NewReader(`{
		"variables":{"PUBLIC_VALUE":"plain"},
		"secrets":{"API_TOKEN":"${secret.api-token}"}
	}`))
	recorder := httptest.NewRecorder()
	server.handleServiceBindings(recorder, request, "workspace-one")
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT bindings returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var applied ServiceBindings
	if err := json.Unmarshal(recorder.Body.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	if applied.SchemaVersion != serviceSchemaVersion || applied.Variables["PUBLIC_VALUE"] != "plain" || applied.Secrets["API_TOKEN"] != "${secret.api-token}" {
		t.Fatalf("PUT bindings response = %#v", applied)
	}

	recorder = httptest.NewRecorder()
	server.handleServiceBindings(recorder, httptest.NewRequest(http.MethodGet, request.URL.String(), nil), "workspace-one")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET bindings returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var loaded ServiceBindings
	if err := json.Unmarshal(recorder.Body.Bytes(), &loaded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, applied) {
		t.Fatalf("GET bindings = %#v, want %#v", loaded, applied)
	}

	recorder = httptest.NewRecorder()
	server.handleServiceBindings(recorder, httptest.NewRequest(http.MethodPut, request.URL.String(), strings.NewReader(`{"variables":{},"unknown":true}`)), "workspace-one")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown PUT field returned %d: %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	server.handleServiceBindings(recorder, httptest.NewRequest(http.MethodPost, request.URL.String(), nil), "workspace-one")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST bindings returned %d: %s", recorder.Code, recorder.Body.String())
	}
}
