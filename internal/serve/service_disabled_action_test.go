package serve

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestServiceManagerRejectsStartAndRestartWhileDisabled(t *testing.T) {
	for _, action := range []struct {
		name string
		run  func(*ServiceManager) error
	}{
		{name: "start", run: func(manager *ServiceManager) error {
			return manager.StartService(context.Background(), "worker")
		}},
		{name: "restart", run: func(manager *ServiceManager) error {
			return manager.RestartService(context.Background(), "worker")
		}},
	} {
		t.Run(action.name, func(t *testing.T) {
			root := t.TempDir()
			manager, err := NewServiceManager(root, ServiceManagerOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Apply(disabledBatchService("worker")); err != nil {
				t.Fatal(err)
			}
			before, err := manager.Show("worker")
			if err != nil {
				t.Fatal(err)
			}
			// Show stamps its synthesized empty export view with the read time.
			before.Exports.UpdatedAt = ""
			persistedBefore, err := os.ReadFile(serviceConfigPath(root, "worker"))
			if err != nil {
				t.Fatal(err)
			}

			actionErr := action.run(manager)
			if !errors.Is(actionErr, errServiceDisabled) || !strings.Contains(actionErr.Error(), action.name+` service "worker"`) {
				t.Fatalf("%s error = %v, want disabled-service error", action.name, actionErr)
			}
			after, err := manager.Show("worker")
			if err != nil {
				t.Fatal(err)
			}
			after.Exports.UpdatedAt = ""
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("status after rejected %s = %#v, want %#v", action.name, after, before)
			}
			persistedAfter, err := os.ReadFile(serviceConfigPath(root, "worker"))
			if err != nil {
				t.Fatal(err)
			}
			if string(persistedAfter) != string(persistedBefore) {
				t.Fatalf("definition changed after rejected %s", action.name)
			}
			if manager.started {
				t.Fatalf("rejected %s started the supervisor", action.name)
			}
		})
	}
}

func TestWorkspaceServiceActionsReturnStableDisabledConflict(t *testing.T) {
	root := t.TempDir()
	writeTestService(t, root, disabledBatchService("worker"))
	workspace := serveWorkspace{ID: "workspace-one", Path: root}
	server := newServiceLifecycleTestServer(t, workspace)

	for _, action := range []string{"start", "restart"} {
		t.Run(action, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/workspaces/workspace-one/services/worker/"+action, nil)
			server.handleWorkspaceServices(recorder, request, workspace.ID, []string{"worker", action})

			if recorder.Code != http.StatusConflict {
				t.Fatalf("%s returned %d: %s", action, recorder.Code, recorder.Body.String())
			}
			body := recorder.Body.String()
			if !strings.Contains(body, `"code":"service_disabled"`) ||
				!strings.Contains(body, action+` service \"worker\": service is disabled; enable it first`) {
				t.Fatalf("%s response = %s", action, body)
			}
		})
	}
}
