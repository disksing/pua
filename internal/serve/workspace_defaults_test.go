package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func TestWorkspaceDefaultsAndProjectTaskDefaultHTTPAPI(t *testing.T) {
	root := t.TempDir()
	puaWorkspace, err := app.Initialize(root, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.RegisterUser(app.LegacyDefaultUserName); err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.EnsureResourceRuntime(); err != nil {
		t.Fatal(err)
	}
	project, err := puaWorkspace.CreateProject("Project", "project")
	if err != nil {
		t.Fatal(err)
	}
	workspace := serveWorkspace{ID: "workspace-defaults", Name: "Defaults", Path: root}
	s := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := s.saveConfig(config{
		Version: agentHubConfigVersion, Workspaces: []serveWorkspace{workspace},
		AgentProfiles: []agentProfileRoute{{Key: "default", AgentName: "fake-agent"}, {Key: "review", AgentName: "fake-agent"}},
	}); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(workspaceUserHeader, app.LegacyDefaultUserName)
		s.handleWorkspace(recorder, req)
		return recorder
	}

	defaultsResponse := request(http.MethodPut, "/api/workspaces/workspace-defaults/defaults", `{"project":{"kind":"profile","name":"review"},"task":{"kind":"agent","name":"task-agent"}}`)
	var defaultsBody struct {
		ResourceDefaults app.ResourceAgentDefaults `json:"resourceDefaults"`
	}
	defaultsErr := json.Unmarshal(defaultsResponse.Body.Bytes(), &defaultsBody)
	if defaultsResponse.Code != http.StatusOK || defaultsErr != nil {
		t.Fatalf("update Workspace defaults = %d %s", defaultsResponse.Code, defaultsResponse.Body.String())
	}
	if defaultsBody.ResourceDefaults.Project.Name != "review" || defaultsBody.ResourceDefaults.Task != (app.AgentBinding{Kind: "agent", Name: "task-agent"}) {
		t.Fatalf("updated Workspace defaults = %#v", defaultsBody.ResourceDefaults)
	}

	unknownProfile := request(http.MethodPut, "/api/workspaces/workspace-defaults/defaults", `{"project":{"kind":"profile","name":"missing"},"task":{"kind":"profile","name":"default"}}`)
	if unknownProfile.Code != http.StatusBadRequest {
		t.Fatalf("unknown Profile default = %d %s", unknownProfile.Code, unknownProfile.Body.String())
	}

	policyResponse := request(http.MethodPut, "/api/workspaces/workspace-defaults/generation-policy", `{"budgetEnabled":false,"maxTurns":25,"maxAccumulatedTurnMinutes":150,"inactivityEnabled":true,"maxInactivityMinutes":1440}`)
	var policyBody struct {
		GenerationPolicy app.GenerationPolicy `json:"generationPolicy"`
	}
	policyErr := json.Unmarshal(policyResponse.Body.Bytes(), &policyBody)
	if policyResponse.Code != http.StatusOK || policyErr != nil || policyBody.GenerationPolicy != (app.GenerationPolicy{BudgetEnabled: false, MaxTurns: 25, MaxAccumulatedTurnMinutes: 150, InactivityEnabled: true, MaxInactivityMinutes: 1440}) {
		t.Fatalf("update Generation policy = %d %s", policyResponse.Code, policyResponse.Body.String())
	}
	legacyPolicy := request(http.MethodPut, "/api/workspaces/workspace-defaults/generation-policy", `{"enabled":false,"maxTurns":25,"maxAccumulatedTurnMinutes":150}`)
	legacyPolicyErr := json.Unmarshal(legacyPolicy.Body.Bytes(), &policyBody)
	if legacyPolicy.Code != http.StatusOK || legacyPolicyErr != nil || policyBody.GenerationPolicy != (app.GenerationPolicy{BudgetEnabled: false, MaxTurns: 25, MaxAccumulatedTurnMinutes: 150, InactivityEnabled: false, MaxInactivityMinutes: 1440}) {
		t.Fatalf("legacy Generation policy = %d %s", legacyPolicy.Code, legacyPolicy.Body.String())
	}
	invalidPolicy := request(http.MethodPut, "/api/workspaces/workspace-defaults/generation-policy", `{"budgetEnabled":true,"maxTurns":0,"maxAccumulatedTurnMinutes":0,"inactivityEnabled":true,"maxInactivityMinutes":1440}`)
	if invalidPolicy.Code != http.StatusBadRequest {
		t.Fatalf("invalid Generation policy = %d %s", invalidPolicy.Code, invalidPolicy.Body.String())
	}

	watchdogResponse := request(http.MethodPut, "/api/workspaces/workspace-defaults/stall-watchdog-policy", `{"enabled":true,"timeoutMinutes":45}`)
	var watchdogBody struct {
		Policy app.StallWatchdogPolicy `json:"stallWatchdogPolicy"`
	}
	watchdogErr := json.Unmarshal(watchdogResponse.Body.Bytes(), &watchdogBody)
	if watchdogResponse.Code != http.StatusOK || watchdogErr != nil || watchdogBody.Policy != (app.StallWatchdogPolicy{Enabled: true, TimeoutMinutes: 45}) {
		t.Fatalf("update stall watchdog policy = %d %s", watchdogResponse.Code, watchdogResponse.Body.String())
	}
	invalidWatchdog := request(http.MethodPut, "/api/workspaces/workspace-defaults/stall-watchdog-policy", `{"enabled":true,"timeoutMinutes":0}`)
	if invalidWatchdog.Code != http.StatusBadRequest {
		t.Fatalf("invalid stall watchdog policy = %d %s", invalidWatchdog.Code, invalidWatchdog.Body.String())
	}

	taskDefaultResponse := request(http.MethodPut, "/api/workspaces/workspace-defaults/resources/project1/task-default", `{"kind":"profile","name":"review"}`)
	var taskDefaultBody struct {
		TaskDefault app.AgentBinding `json:"taskDefault"`
	}
	taskDefaultErr := json.Unmarshal(taskDefaultResponse.Body.Bytes(), &taskDefaultBody)
	if taskDefaultResponse.Code != http.StatusOK || taskDefaultErr != nil || taskDefaultBody.TaskDefault != (app.AgentBinding{Kind: "profile", Name: "review"}) {
		t.Fatalf("update Project Task default = %d %s", taskDefaultResponse.Code, taskDefaultResponse.Body.String())
	}

	task, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: "Task"})
	if err != nil {
		t.Fatal(err)
	}
	if task.AgentBinding != (app.AgentBinding{Kind: "profile", Name: "review"}) {
		t.Fatalf("Task did not use the Project default: %#v", task.AgentBinding)
	}

	clearedResponse := request(http.MethodPut, "/api/workspaces/workspace-defaults/resources/project1/task-default", `{}`)
	var clearedBody struct {
		TaskDefault app.AgentBinding `json:"taskDefault"`
	}
	clearedErr := json.Unmarshal(clearedResponse.Body.Bytes(), &clearedBody)
	if clearedResponse.Code != http.StatusOK || clearedErr != nil || clearedBody.TaskDefault != (app.AgentBinding{}) {
		t.Fatalf("clear Project Task default = %d %s", clearedResponse.Code, clearedResponse.Body.String())
	}

	nonProject := request(http.MethodPut, "/api/workspaces/workspace-defaults/resources/"+task.ID+"/task-default", `{"kind":"profile","name":"review"}`)
	if nonProject.Code != http.StatusBadRequest {
		t.Fatalf("Task default on a Task = %d %s", nonProject.Code, nonProject.Body.String())
	}

	treeResponse := request(http.MethodGet, "/api/workspaces/workspace-defaults/tree", "")
	if treeResponse.Code != http.StatusOK || !strings.Contains(treeResponse.Body.String(), `"resourceDefaults"`) || !strings.Contains(treeResponse.Body.String(), `"generationPolicy"`) || !strings.Contains(treeResponse.Body.String(), `"stallWatchdogPolicy"`) || !strings.Contains(treeResponse.Body.String(), `"review"`) {
		t.Fatalf("tree did not project Workspace defaults: %d %s", treeResponse.Code, treeResponse.Body.String())
	}

	detailResponse := request(http.MethodGet, "/api/workspaces/workspace-defaults/resources/project1", "")
	if detailResponse.Code != http.StatusOK {
		t.Fatalf("read Project detail = %d %s", detailResponse.Code, detailResponse.Body.String())
	}
}
