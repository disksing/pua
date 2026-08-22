package pua

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func TestBindingSetCommandsUseResourceAgentBindingEndpoint(t *testing.T) {
	withTempCwd(t, func(_ string) {
		run(t, "init")
		run(t, "project", "create", "Binding project")
		run(t, "task", "create", "--project=project1", "Binding task")

		bindings := map[string]app.AgentBinding{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			if r.Method != http.MethodPut || len(parts) != 6 || parts[0] != "api" || parts[1] != "workspaces" || parts[3] != "resources" || parts[5] != "agent-binding" {
				http.NotFound(w, r)
				return
			}
			var binding app.AgentBinding
			if err := json.NewDecoder(r.Body).Decode(&binding); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			normalized, err := app.NormalizeAgentBinding(binding)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			bindings[parts[4]] = normalized
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"agentBinding": normalized})
		}))
		defer server.Close()

		cases := []struct {
			args       []string
			resourceID string
			expected   app.AgentBinding
		}{
			{
				args:       []string{"workspace", "binding", "set", "--profile=FAST", "--server=" + server.URL},
				resourceID: "workspace",
				expected:   app.AgentBinding{Kind: "profile", Name: "fast"},
			},
			{
				args:       []string{"project", "binding", "set", "--project=1", "--agent", "Kimi-K3", "--server=" + server.URL},
				resourceID: "project1",
				expected:   app.AgentBinding{Kind: "agent", Name: "Kimi-K3"},
			},
			{
				args:       []string{"task", "binding", "set", "--project=1", "--task=1", "--profile", "review", "--server=" + server.URL},
				resourceID: "project1.task1",
				expected:   app.AgentBinding{Kind: "profile", Name: "review"},
			},
		}
		for _, testCase := range cases {
			var response map[string]app.AgentBinding
			if err := json.Unmarshal([]byte(run(t, testCase.args...)), &response); err != nil {
				t.Fatalf("%v: invalid response: %v", testCase.args, err)
			}
			if response["agentBinding"] != testCase.expected || bindings[testCase.resourceID] != testCase.expected {
				t.Fatalf("%v: binding=%#v requests=%#v", testCase.args, response["agentBinding"], bindings)
			}
		}

		if len(bindings) != len(cases) {
			t.Fatalf("expected one request per resource, got %#v", bindings)
		}
	})
}

func TestBindingHelp(t *testing.T) {
	for _, testCase := range []struct {
		args   []string
		marker string
	}{
		{args: []string{"help", "workspace"}, marker: workspaceBindingUsage[7:]},
		{args: []string{"help", "project"}, marker: projectBindingUsage[7:]},
		{args: []string{"help", "task"}, marker: taskBindingUsage[7:]},
	} {
		if output := run(t, testCase.args...); !strings.Contains(output, testCase.marker) {
			t.Fatalf("%v help is missing %q:\n%s", testCase.args, testCase.marker, output)
		}
	}
}

func TestTaskCreateBindingFlagsAndDryRun(t *testing.T) {
	withTempCwd(t, func(root string) {
		run(t, "init")
		run(t, "project", "create", "Binding project")

		created := run(t, "task", "create", "--project=project1", "--profile=FAST", "Profile task")
		var profileTask app.Task
		if err := json.Unmarshal([]byte(created), &profileTask); err != nil {
			t.Fatal(err)
		}
		if profileTask.AgentBinding != (app.AgentBinding{Kind: "profile", Name: "fast"}) {
			t.Fatalf("profile binding = %#v", profileTask.AgentBinding)
		}

		created = run(t, "task", "create", "--project=project1", "--agent", "Kimi-K3", "Agent task")
		var agentTask app.Task
		if err := json.Unmarshal([]byte(created), &agentTask); err != nil {
			t.Fatal(err)
		}
		if agentTask.AgentBinding != (app.AgentBinding{Kind: "agent", Name: "Kimi-K3"}) {
			t.Fatalf("agent binding = %#v", agentTask.AgentBinding)
		}

		if _, err := runErr(t, "task", "create", "--project=project1", "--profile=fast", "--agent=Kimi-K3", "Conflicting task"); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("expected profile/agent conflict, got %v", err)
		}

		template := "---\n" +
			"schema-version: 2\n" +
			"title: Brief\n" +
			"task-title: \"{{ summary }}\"\n" +
			"fields:\n" +
			"  - name: summary\n" +
			"    type: text\n" +
			"    label: Summary\n" +
			"    required: true\n" +
			"---\n" +
			"# {{ summary }}\n"
		if err := os.WriteFile(filepath.Join(root, "project1", "templates", "brief.md"), []byte(template), 0o644); err != nil {
			t.Fatal(err)
		}
		preview := run(t, "task", "create", "--project=project1", "--template=brief", "--field", "summary=Preview", "--profile=FAST", "--dry-run")
		var taskPreview app.TaskPreview
		if err := json.Unmarshal([]byte(preview), &taskPreview); err != nil {
			t.Fatal(err)
		}
		if taskPreview.AgentBinding != (app.AgentBinding{Kind: "profile", Name: "fast"}) {
			t.Fatalf("dry-run binding = %#v", taskPreview.AgentBinding)
		}
	})
}
