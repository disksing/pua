package serve

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func userTestServer(t *testing.T) (*server, string) {
	t.Helper()
	workspace := t.TempDir()
	initialized, err := app.Initialize(workspace, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := initialized.RegisterUser(app.LegacyDefaultUserName); err != nil {
		t.Fatal(err)
	}
	server := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := server.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Path: workspace}}}); err != nil {
		t.Fatal(err)
	}
	return server, workspace
}

func userRequest(t *testing.T, server *server, method, path, body, userName string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if userName != "" {
		request.Header.Set(workspaceUserHeader, userName)
	}
	recorder := httptest.NewRecorder()
	server.handleWorkspace(recorder, request)
	return recorder
}

func TestWorkspaceUsersAPIRegistersUpdatesListsAndDeletes(t *testing.T) {
	server, _ := userTestServer(t)
	recorder := userRequest(t, server, http.MethodPost, "/api/workspaces/workspace-one/users", `{"name":"alice_2-test"}`, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("register returned %d: %s", recorder.Code, recorder.Body.String())
	}
	recorder = userRequest(t, server, http.MethodPost, "/api/workspaces/workspace-one/users", `{"name":"bad/name"}`, "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid register returned %d", recorder.Code)
	}
	recorder = userRequest(t, server, http.MethodPut, "/api/workspaces/workspace-one/users/alice_2-test", `{"preference":"Call me Alice"}`, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("update returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var profile app.UserProfile
	if err := json.Unmarshal(recorder.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	if profile.Preference != "Call me Alice" {
		t.Fatalf("updated profile = %#v", profile)
	}
	recorder = userRequest(t, server, http.MethodGet, "/api/workspaces/workspace-one/users", "", "alice_2-test")
	var listed struct {
		Users []app.UserProfile `json:"users"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || len(listed.Users) != 2 {
		t.Fatalf("list returned %d: %s", recorder.Code, recorder.Body.String())
	}
	recorder = userRequest(t, server, http.MethodDelete, "/api/workspaces/workspace-one/users/alice_2-test", "", "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete returned %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestPersonalWorkspaceAPIsRequireAnExistingSelectedUser(t *testing.T) {
	server, _ := userTestServer(t)
	missing := userRequest(t, server, http.MethodGet, "/api/workspaces/workspace-one/tree", "", "")
	if missing.Code != http.StatusBadRequest || !strings.Contains(missing.Body.String(), `"code":"user_required"`) {
		t.Fatalf("missing user = %d %s", missing.Code, missing.Body.String())
	}
	unknown := userRequest(t, server, http.MethodGet, "/api/workspaces/workspace-one/tree", "", "Missing")
	if unknown.Code != http.StatusBadRequest || !strings.Contains(unknown.Body.String(), `"code":"user_not_found"`) {
		t.Fatalf("unknown user = %d %s", unknown.Code, unknown.Body.String())
	}
}

func TestWorkspaceUsersAPIPreventsDeletingLastUser(t *testing.T) {
	server, _ := userTestServer(t)
	last := userRequest(t, server, http.MethodDelete, "/api/workspaces/workspace-one/users/User", "", "")
	if last.Code != http.StatusConflict || !strings.Contains(last.Body.String(), `"code":"last_user"`) {
		t.Fatalf("delete last user = %d %s", last.Code, last.Body.String())
	}
}

func TestLegacyUIStateWaitsForFirstUserWhenWorkspaceHasNoUsers(t *testing.T) {
	workspace := t.TempDir()
	if _, err := app.Initialize(workspace, "en"); err != nil {
		t.Fatal(err)
	}
	server := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := server.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Path: workspace}}}); err != nil {
		t.Fatal(err)
	}
	legacy := uiState{Version: 1, ExpandedProjects: []string{"project1"}}
	if err := saveJSONStateFile(uiStatePath(workspace), ".legacy-ui-*.tmp", legacy); err != nil {
		t.Fatal(err)
	}
	if err := server.ensureWorkspaceUsersAndMigrateUIState(workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(uiStatePath(workspace)); err != nil {
		t.Fatalf("legacy state removed before an identity existed: %v", err)
	}
	recorder := userRequest(t, server, http.MethodPost, "/api/workspaces/workspace-one/users", `{"name":"Alice"}`, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("create first user = %d %s", recorder.Code, recorder.Body.String())
	}
	migrated, err := loadUIStateFile(userUIStatePath(workspace, "Alice"))
	if err != nil || len(migrated.ExpandedProjects) != 1 || migrated.ExpandedProjects[0] != "project1" {
		t.Fatalf("first user legacy state = %#v, %v", migrated, err)
	}
	if _, err := os.Stat(uiStatePath(workspace)); !os.IsNotExist(err) {
		t.Fatalf("legacy state remains after first-user migration: %v", err)
	}
}

func TestUIAndResourceStateAreIsolatedByUser(t *testing.T) {
	server, workspace := userTestServer(t)
	puaWorkspace, err := app.OpenWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.RegisterUser("Alice"); err != nil {
		t.Fatal(err)
	}
	project, err := puaWorkspace.CreateProject("Shared project", "shared")
	if err != nil {
		t.Fatal(err)
	}

	recorder := userRequest(t, server, http.MethodPut, "/api/workspaces/workspace-one/ui-state", `{"version":1,"expandedProjects":["project1"]}`, "Alice")
	if recorder.Code != http.StatusOK {
		t.Fatalf("Alice UI update returned %d: %s", recorder.Code, recorder.Body.String())
	}
	recorder = userRequest(t, server, http.MethodPut, "/api/workspaces/workspace-one/resources/"+project.ID+"/read", `{"throughTurnNumber":0}`, "Alice")
	if recorder.Code != http.StatusOK {
		t.Fatalf("Alice read update returned %d: %s", recorder.Code, recorder.Body.String())
	}

	alice, err := server.loadUIState("workspace-one", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	defaultUser, err := server.loadUIState("workspace-one", app.DefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	if len(alice.ExpandedProjects) != 1 || alice.ResourceStates[project.ID].ReadTurnNumber == nil {
		t.Fatalf("Alice state = %#v", alice)
	}
	if len(defaultUser.ExpandedProjects) != 0 || defaultUser.ResourceStates[project.ID].ReadTurnNumber != nil {
		t.Fatalf("default User inherited Alice state: %#v", defaultUser)
	}
}

func TestLegacyUIStateMigratesToDefaultUserAndResourceState(t *testing.T) {
	server, workspace := userTestServer(t)
	read := 2
	shared := resourceState{Version: 1, TurnNumbers: map[string]int{"project1": 4, "workspace": 3}}
	if err := saveResourceStateFile(resourceStatePath(workspace), shared); err != nil {
		t.Fatal(err)
	}
	legacy := uiState{
		Version: 1, ExpandedProjects: []string{"project1"},
		Attention: map[string]resourceAttentionState{
			"project1":  {DismissedTurn: &read, TurnNumber: 4},
			"workspace": {TurnNumber: 3},
		},
	}
	if err := saveJSONStateFile(uiStatePath(workspace), ".legacy-ui-*.tmp", legacy); err != nil {
		t.Fatal(err)
	}
	if err := server.ensureWorkspaceUsersAndMigrateUIState(workspace); err != nil {
		t.Fatal(err)
	}
	migrated, err := loadUIStateFile(userUIStatePath(workspace, app.DefaultUserName))
	if err != nil {
		t.Fatal(err)
	}
	resourceState := migrated.ResourceStates["project1"]
	if resourceState.ReadTurnNumber == nil || *resourceState.ReadTurnNumber != 2 {
		t.Fatalf("migrated resource state = %#v", resourceState)
	}
	workspaceState := migrated.ResourceStates["workspace"]
	if workspaceState.ReadTurnNumber == nil || *workspaceState.ReadTurnNumber != 3 {
		t.Fatalf("untracked resource did not receive migration baseline: %#v", workspaceState)
	}
	loadedShared, err := loadResourceStateFile(resourceStatePath(workspace))
	if err != nil {
		t.Fatal(err)
	}
	if loadedShared.TurnNumbers["project1"] != 4 {
		t.Fatalf("resource state = %#v", loadedShared)
	}
	if _, err := os.Stat(uiStatePath(workspace)); !os.IsNotExist(err) {
		t.Fatalf("legacy UI state was not removed after migration: %v", err)
	}
	if err := server.ensureWorkspaceUsersAndMigrateUIState(workspace); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".pua", "users", "User", "profile.json")); err != nil {
		t.Fatal(err)
	}
}

func TestNewWorkspaceUserStartsAtCurrentTurnBaseline(t *testing.T) {
	server, workspace := userTestServer(t)
	puaWorkspace, err := app.OpenWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	project, err := puaWorkspace.CreateProject("Existing project", "existing")
	if err != nil {
		t.Fatal(err)
	}
	if err := rewriteTestGenerationRecords(workspace, []generationRecord{{
		ID: "gen-user-baseline", WorkspaceID: "workspace-one", ResourceID: project.ID,
		Generation: 1, GenerationID: "gen-user-baseline", AgentHubSessionID: "session-user-baseline",
		Status: "idle", TurnNumber: 5, Title: project.Title, Cwd: workspace,
	}}); err != nil {
		t.Fatal(err)
	}

	recorder := userRequest(t, server, http.MethodPost, "/api/workspaces/workspace-one/users", `{"name":"Alice"}`, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("register returned %d: %s", recorder.Code, recorder.Body.String())
	}
	state, err := loadUIStateFile(userUIStatePath(workspace, "Alice"))
	if err != nil {
		t.Fatal(err)
	}
	baseline := state.ResourceStates[project.ID].ReadTurnNumber
	if baseline == nil || *baseline != 5 {
		t.Fatalf("new user baseline = %#v, want Turn 5", state.ResourceStates[project.ID])
	}
}
