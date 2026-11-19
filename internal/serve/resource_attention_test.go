package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func attentionTestServer(t *testing.T) (*server, string) {
	t.Helper()
	workspace := t.TempDir()
	puaWorkspace, err := app.Initialize(workspace, "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.RegisterUser(app.LegacyDefaultUserName); err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.CreateProject("Attention project", "attention"); err != nil {
		t.Fatal(err)
	}
	server := &server{config: filepath.Join(t.TempDir(), "serve.json")}
	if err := server.saveConfig(config{Version: agentHubConfigVersion, Workspaces: []serveWorkspace{{ID: "workspace-one", Path: workspace}}, AgentHubEndpoint: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	return server, workspace
}

func attentionRequest(t *testing.T, server *server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.Header.Set(workspaceUserHeader, app.LegacyDefaultUserName)
	server.handleWorkspace(recorder, request)
	return recorder
}

func TestResourceReadAPI(t *testing.T) {
	server, workspace := attentionTestServer(t)
	now := "2026-08-13T00:00:00Z"
	record := generationRecord{
		ID: "gen-attention", WorkspaceID: "workspace-one", ResourceID: "project1",
		Generation: 1, GenerationID: "gen-attention", AgentHubSessionID: "session-attention",
		Status: "idle", TurnNumber: 3, Title: "Attention", Cwd: workspace, CreatedAt: now, UpdatedAt: now,
	}
	if err := rewriteTestGenerationRecords(workspace, []generationRecord{record}); err != nil {
		t.Fatal(err)
	}

	recorder := attentionRequest(t, server, http.MethodPut, "/api/workspaces/workspace-one/resources/project1/read", `{"throughTurnNumber":3}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("read returned %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != "{}\n" {
		t.Fatalf("legacy read returned %s", recorder.Body.String())
	}

	tree, err := server.treeAt(context.Background(), workspace, app.LegacyDefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Projects[0].LatestTurnNumber != 3 {
		t.Fatalf("completed Turn projection missing: %#v", tree.Projects[0])
	}

	record.TurnNumber = 4
	if err := rewriteTestGenerationRecords(workspace, []generationRecord{record}); err != nil {
		t.Fatal(err)
	}
	tree, err = server.treeAt(context.Background(), workspace, app.LegacyDefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Projects[0].LatestTurnNumber != 4 || tree.Projects[0].Chat != nil {
		t.Fatalf("execution history changed Chat projection: %#v", tree.Projects[0])
	}
}

func TestResourceReadAPIRejectsFutureTurnsAndDoesNotPersist(t *testing.T) {
	server, workspace := attentionTestServer(t)
	now := "2026-08-13T00:00:00Z"
	if err := rewriteTestGenerationRecords(workspace, []generationRecord{{
		ID: "gen-read-boundary", WorkspaceID: "workspace-one", ResourceID: "project1",
		Generation: 1, GenerationID: "gen-read-boundary", AgentHubSessionID: "session-read-boundary",
		Status: "idle", TurnNumber: 3, Title: "Read boundary", Cwd: workspace, CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}

	for _, through := range []int{2, 1} {
		recorder := attentionRequest(t, server, http.MethodPut, "/api/workspaces/workspace-one/resources/project1/read", fmt.Sprintf(`{"throughTurnNumber":%d}`, through))
		if recorder.Code != http.StatusOK {
			t.Fatalf("read through Turn %d returned %d: %s", through, recorder.Code, recorder.Body.String())
		}
		if recorder.Body.String() != "{}\n" {
			t.Fatalf("legacy read returned %s", recorder.Body.String())
		}
	}

	recorder := attentionRequest(t, server, http.MethodPut, "/api/workspaces/workspace-one/resources/project1/read", `{"throughTurnNumber":4}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("future read returned %d: %s", recorder.Code, recorder.Body.String())
	}
	state, err := loadUIStateFile(userUIStatePath(workspace, app.LegacyDefaultUserName))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.ResourceStates) != 0 {
		t.Fatalf("legacy read persisted a cursor: %#v", state)
	}
}

func TestActiveTurnPreservesLatestCompletedTurnAndLegacyCursor(t *testing.T) {
	server, workspace := attentionTestServer(t)
	now := "2026-08-13T00:00:00Z"
	record := generationRecord{
		ID: "gen-active-attention", WorkspaceID: "workspace-one", ResourceID: "project1",
		Generation: 1, GenerationID: "gen-active-attention", AgentHubSessionID: "session-active-attention",
		Status: "running", CurrentTurnID: "turn-active", TurnNumber: 2, Title: "Active", Cwd: workspace,
		CreatedAt: now, UpdatedAt: now, TurnStartedAt: "2026-08-13T00:00:02Z", CompletionAt: "2026-08-13T00:00:01Z",
	}
	if err := rewriteTestGenerationRecords(workspace, []generationRecord{record}); err != nil {
		t.Fatal(err)
	}
	if err := saveUIStateFile(userUIStatePath(workspace, app.LegacyDefaultUserName), uiState{
		ResourceStates: map[string]resourceUserState{"project1": {ReadTurnNumber: intPointer(1)}},
	}); err != nil {
		t.Fatal(err)
	}
	tree, err := server.treeAt(context.Background(), workspace, app.LegacyDefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Activity.Running) != 1 || !tree.Activity.Running[0].Runtime.ActiveTurn {
		t.Fatalf("active Turn must remain in Running: %#v", tree.Activity.Running)
	}
	if tree.Projects[0].LatestTurnNumber != 1 || !tree.Projects[0].Runtime.ActiveTurn {
		t.Fatalf("active Turn projection is incorrect: %#v", tree.Projects[0])
	}
	recorder := attentionRequest(t, server, http.MethodPut, "/api/workspaces/workspace-one/resources/project1/read", `{"throughTurnNumber":2}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("read active Turn returned %d: %s", recorder.Code, recorder.Body.String())
	}

	record.Status = "idle"
	record.CurrentTurnID = ""
	record.CompletionAt = "2026-08-13T00:00:03Z"
	if err := rewriteTestGenerationRecords(workspace, []generationRecord{record}); err != nil {
		t.Fatal(err)
	}
	tree, err = server.treeAt(context.Background(), workspace, app.LegacyDefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Activity.Running) != 0 || tree.Projects[0].Runtime.ActiveTurn || tree.Projects[0].LatestTurnNumber != 2 {
		t.Fatalf("completed Turn projection is incorrect: project=%#v activity=%#v", tree.Projects[0], tree.Activity)
	}
	recorder = attentionRequest(t, server, http.MethodPut, "/api/workspaces/workspace-one/resources/project1/read", `{"throughTurnNumber":2}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("read completed Turn returned %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder = attentionRequest(t, server, http.MethodPut, "/api/workspaces/workspace-one/ui-state", `{"version":2,"expandedProjects":["project1"],"lastResourceId":"project1"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("ui-state update returned %d: %s", recorder.Code, recorder.Body.String())
	}
	state, err := server.loadUIState("workspace-one", app.LegacyDefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	if state.ResourceStates["project1"].ReadTurnNumber == nil || *state.ResourceStates["project1"].ReadTurnNumber != 1 {
		t.Fatalf("ui-state update overwrote resource state: %#v", state.ResourceStates)
	}
}

func TestSchedulerRetainsLatestCompletedTurn(t *testing.T) {
	server, workspace := attentionTestServer(t)
	now := "2026-08-13T00:00:00Z"
	if err := rewriteTestGenerationRecords(workspace, []generationRecord{
		{
			ID: "gen-scheduler-unread", WorkspaceID: "workspace-one", ResourceID: "scheduler",
			Generation: 1, GenerationID: "gen-scheduler-unread", AgentHubSessionID: "session-scheduler-unread",
			Status: "idle", TurnNumber: 3, Title: "Scheduler", Cwd: workspace, CreatedAt: now, UpdatedAt: now, CompletionAt: now,
		},
		{
			ID: "gen-project-unread", WorkspaceID: "workspace-one", ResourceID: "project1",
			Generation: 1, GenerationID: "gen-project-unread", AgentHubSessionID: "session-project-unread",
			Status: "idle", TurnNumber: 2, Title: "Project", Cwd: workspace, CreatedAt: now, UpdatedAt: now, CompletionAt: now,
		},
	}); err != nil {
		t.Fatal(err)
	}
	tree, err := server.treeAt(context.Background(), workspace, app.LegacyDefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Scheduler.LatestTurnNumber != 3 {
		t.Fatalf("scheduler must retain its latest completed Turn: %#v", tree.Scheduler)
	}
	if tree.Scheduler.Chat != nil {
		t.Fatalf("event unread must not create a chat: %#v", tree.Scheduler)
	}
}

func TestResourceActiveTurnIgnoresStaleTurnIDOnIdleGeneration(t *testing.T) {
	record := generationRecord{Status: "idle", CurrentTurnID: "turn-already-completed"}
	if generationHasActiveTurn(record) {
		t.Fatalf("idle generation with a stale Turn ID must not remain active: %#v", record)
	}
}

func TestActivityUsesLatestTurnAcrossRetiredGenerations(t *testing.T) {
	server, workspace := attentionTestServer(t)
	now := "2026-08-13T00:00:00Z"
	if err := rewriteTestGenerationRecords(workspace, []generationRecord{
		{ID: "gen-old-active", WorkspaceID: "workspace-one", ResourceID: "project1", Generation: 1, GenerationID: "gen-old-active", AgentHubSessionID: "session-old-active", Status: "running", CurrentTurnID: "turn-old", TurnNumber: 4, Title: "Old", Cwd: workspace, CreatedAt: now, UpdatedAt: now},
		{ID: "gen-new-idle", WorkspaceID: "workspace-one", ResourceID: "project1", Generation: 2, GenerationID: "gen-new-idle", AgentHubSessionID: "session-new-idle", Status: "idle", TurnNumber: 4, Title: "New", Cwd: workspace, CreatedAt: now, UpdatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	tree, err := server.treeAt(context.Background(), workspace, app.LegacyDefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Activity.Running) != 0 {
		t.Fatalf("retired older generation must be excluded from Running: %#v", tree.Activity.Running)
	}
	if tree.Projects[0].LatestTurnNumber != 4 {
		t.Fatalf("latest historical Turn must remain unread: %#v", tree.Projects[0])
	}
}

func TestLatestCompletedTurnPrefersAnExactCompletedGeneration(t *testing.T) {
	records := []generationRecord{
		{
			ID: "gen-completed", ResourceID: "project1", Generation: 1, GenerationID: "gen-completed",
			AgentHubSessionID: "session-completed", Status: "idle", TurnNumber: 3, CompletionAt: "2026-08-13T00:00:03Z",
		},
		{
			ID: "gen-active", ResourceID: "project1", Generation: 2, GenerationID: "gen-active",
			AgentHubSessionID: "session-active", Status: "running", CurrentTurnID: "turn-4", TurnNumber: 4,
		},
	}
	completed := latestCompletedTurnByResource(records)["project1"]
	if completed.TurnNumber != 3 || completed.Record.ID != "gen-completed" || !completed.Exact {
		t.Fatalf("latest completed Turn = %#v, want exact Turn 3 record", completed)
	}
	if got := completedTurnNumberForGeneration(generationRecord{Status: "running", TurnNumber: 1}); got != 0 {
		t.Fatalf("first active Turn completed boundary = %d, want 0", got)
	}
	inferred := latestCompletedTurnByResource(records[1:])["project1"]
	if inferred.TurnNumber != 3 || inferred.Record.ID != "gen-active" || inferred.Exact {
		t.Fatalf("inferred completed Turn = %#v, want Turn 3 from active generation", inferred)
	}
}

func TestDesktopRunningActivityIncludesApprovalAndPreservesWireShape(t *testing.T) {
	server, workspace := attentionTestServer(t)
	puaWorkspace, err := app.OpenWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	resourceIDs := []string{"project1"}
	for _, title := range []string{"Running older", "Idle older", "Idle newer"} {
		task, createErr := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: "project1", Title: title})
		if createErr != nil {
			t.Fatal(createErr)
		}
		resourceIDs = append(resourceIDs, task.ID)
	}
	records := []generationRecord{
		{ID: "gen-running-newer", ResourceID: resourceIDs[0], Generation: 1, GenerationID: "gen-running-newer", AgentHubSessionID: "session-running-newer", Status: "running", CurrentTurnID: "turn-running-newer", TurnNumber: 1, TurnStartedAt: "2026-08-13T00:00:20Z", UpdatedAt: "2026-08-13T00:00:21Z"},
		{ID: "gen-running-older", ResourceID: resourceIDs[1], Generation: 1, GenerationID: "gen-running-older", AgentHubSessionID: "session-running-older", Status: "waiting_approval", CurrentTurnID: "turn-running-older", TurnNumber: 1, TurnStartedAt: "2026-08-13T00:00:10Z", UpdatedAt: "2026-08-13T00:00:59Z"},
		{ID: "gen-idle-older", ResourceID: resourceIDs[2], Generation: 1, GenerationID: "gen-idle-older", AgentHubSessionID: "session-idle-older", Status: "idle", TurnNumber: 1, CompletionAt: "2026-08-13T00:00:30Z", UpdatedAt: "2026-08-13T00:01:00Z"},
		{ID: "gen-idle-newer", ResourceID: resourceIDs[3], Generation: 1, GenerationID: "gen-idle-newer", AgentHubSessionID: "session-idle-newer", Status: "idle", TurnNumber: 1, CompletionAt: "2026-08-13T00:00:40Z", UpdatedAt: "2026-08-13T00:00:41Z"},
	}
	if err := rewriteTestGenerationRecords(workspace, records); err != nil {
		t.Fatal(err)
	}

	tree, err := server.treeAt(context.Background(), workspace, app.LegacyDefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	running := []string{}
	for _, item := range tree.Activity.Running {
		running = append(running, item.ID)
	}
	if wantRunning := []string{resourceIDs[0], resourceIDs[1]}; !slices.Equal(running, wantRunning) {
		t.Fatalf("Running order = %v, want %v", running, wantRunning)
	}
	data, err := json.Marshal(tree.Activity)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil || len(keys) != 1 || keys["running"] == nil {
		t.Fatalf("activity wire fields = %s (%v)", data, err)
	}
}

func TestTreeRetainsTaskWorkflowStates(t *testing.T) {
	server, workspace := attentionTestServer(t)
	puaWorkspace, err := app.OpenWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: "project1", Title: "Blocked task"})
	if err != nil {
		t.Fatal(err)
	}
	errorTask, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: "project1", Title: "Error task"})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: "project1", Title: "Waiting task"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetTaskState(blocked.ID, app.TaskStateBlocked, "Needs input"); err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetTaskState(errorTask.ID, app.TaskStateError, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.SetTaskState(waiting.ID, app.TaskStateWaiting, "Waiting externally"); err != nil {
		t.Fatal(err)
	}

	tree, err := server.treeAt(context.Background(), workspace, app.LegacyDefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]app.TaskState{}
	for _, item := range tree.Projects[0].Children {
		got[item.ID] = item.State
	}
	if got[blocked.ID] != app.TaskStateBlocked || got[errorTask.ID] != app.TaskStateError || got[waiting.ID] != app.TaskStateWaiting {
		t.Fatalf("Tree task states = %v", got)
	}
}

func TestTurnOrdinalChangesOnlyForNewAgentHubTurn(t *testing.T) {
	workspace := t.TempDir()
	manager := newAgentManager(&server{})
	runtime := &agentRuntime{
		workspace: serveWorkspace{ID: "workspace-one", Path: workspace},
		manager:   manager,
		record:    generationRecord{ID: "gen-turn-ordinal", WorkspaceID: "workspace-one", ResourceID: "project1", GenerationID: "gen-turn-ordinal", Status: "idle"},
	}
	runtime.applyAgentHubSessionState(manager, agentHubSession{ID: "session-turns", State: "running", CurrentTurnID: "turn-one"})
	if got := runtime.snapshotGeneration().TurnNumber; got != 1 {
		t.Fatalf("first turn ordinal = %d, want 1", got)
	}
	runtime.applyAgentHubSessionState(manager, agentHubSession{ID: "session-turns", State: "running", CurrentTurnID: "turn-one"})
	if got := runtime.snapshotGeneration().TurnNumber; got != 1 {
		t.Fatalf("duplicate turn ordinal = %d, want 1", got)
	}
	runtime.applyAgentHubSessionState(manager, agentHubSession{ID: "session-turns", State: "ready", CurrentTurnID: "turn-one"})
	if record := runtime.snapshotGeneration(); record.CurrentTurnID != "" || generationHasActiveTurn(record) {
		t.Fatalf("ready session retained stale active Turn state: %#v", record)
	}
	runtime.applyAgentHubSessionState(manager, agentHubSession{ID: "session-turns", State: "running", CurrentTurnID: "turn-two"})
	if got := runtime.snapshotGeneration().TurnNumber; got != 2 {
		t.Fatalf("second turn ordinal = %d, want 2", got)
	}
	stored, err := loadGenerationRecord(workspace, runtime.snapshotGeneration().ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TurnNumber != 2 || stored.LastTurnID != "turn-two" {
		t.Fatalf("turn ordinal was not durable: %#v", stored)
	}
}

func intPointer(value int) *int {
	return &value
}

func TestTreeProjectsSharedChatUnreadForCurrentUser(t *testing.T) {
	s, workspace := attentionTestServer(t)
	user := app.LegacyDefaultUserName
	ids := []string{"workspace", "scheduler", "project1", "project1.task1"}
	for i, id := range ids {
		c := resourceChat{ID: chatID(id, user), ResourceID: id, User: user, Epoch: 1, Open: i%2 == 0, ReadThrough: 1,
			Messages: []chatMessage{
				{ID: "read", Role: "agent", CreatedAt: "2099-01-01T00:00:00Z"},
				{ID: "unread", Role: "agent", CreatedAt: fmt.Sprintf("2026-10-14T00:00:0%dZ", i)},
				{ID: "mine", Role: "user", CreatedAt: fmt.Sprintf("2099-01-02T00:00:0%dZ", i)},
			}}
		if err := saveChatLocked(workspace, c); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"project1.task2", "project1.task3"} {
		c := resourceChat{ID: chatID(id, user), ResourceID: id, User: user, Epoch: 1, Open: true,
			Messages: []chatMessage{{ID: "mine", Role: "user"}}}
		if id == "project1.task3" {
			c.Open = false
			c.Messages[0].Role = "agent"
			c.ReadThrough = 1
		}
		if err := saveChatLocked(workspace, c); err != nil {
			t.Fatal(err)
		}
	}
	records := []generationRecord{{ID: "event-only", GenerationID: "event-only", AgentHubSessionID: "session-events", Generation: 1, ResourceID: "project1.task4", Status: "idle", TurnNumber: 9}}
	privateChat, err := loadChatLocked(workspace, chatID("project1.task2", user))
	if err != nil {
		t.Fatal(err)
	}
	records = append(records, generationRecord{ID: "dialogue-only", GenerationID: "dialogue-only", AgentHubSessionID: "session-dialogue", Generation: 1, ResourceID: privateChat.address(), Status: "idle", TurnNumber: 5})
	if err := rewriteTestGenerationRecords(workspace, records); err != nil {
		t.Fatal(err)
	}
	makeTree := func() workspaceTree {
		return workspaceTree{
			Workspace: resourceSnapshot{ID: "workspace", Type: "workspace"},
			Scheduler: resourceSnapshot{ID: "scheduler", Type: "scheduler"},
			Projects: []resourceSnapshot{{ID: "project1", Type: "project", Children: []resourceSnapshot{
				{ID: "project1.task1", Type: "task"}, {ID: "project1.task2", Type: "task"}, {ID: "project1.task3", Type: "task"},
				{ID: "project1.task4", Type: "task", State: app.TaskStateBlocked, Runtime: &resourceRuntimeSnapshot{ActiveTurn: true}},
			}}},
		}
	}
	tree := makeTree()
	if err := s.enrichTreeUserState(workspace, &tree, user); err != nil {
		t.Fatal(err)
	}
	project := tree.Projects[0]
	if project.Children[0].Chat == nil || project.Children[0].Chat.Open || project.Children[0].Chat.UnreadCount != 1 {
		t.Fatalf("closed chat must keep its unread count: %#v", project.Children[0].Chat)
	}
	if project.Chat.UnreadCount != 1 {
		t.Fatal("tree chat must not aggregate or change its snapshot")
	}
	if len(tree.Activity.Running) != 1 || tree.Activity.Running[0].ID != "project1.task4" || project.Children[3].LatestTurnNumber != 9 || project.Children[3].Chat != nil {
		t.Fatalf("Running or event-only projection changed: %+v", tree)
	}
	other := makeTree()
	if err := s.enrichTreeUserState(workspace, &other, "other"); err != nil {
		t.Fatal(err)
	}
	if other.Projects[0].Children[3].LatestTurnNumber != 9 || other.Projects[0].Chat == nil {
		t.Fatal("shared chat visibility or completed Turn projection changed")
	}
	for _, id := range ids {
		c, err := loadChatLocked(workspace, sharedChatID(id))
		if err != nil {
			t.Fatal(err)
		}
		c.markRead(user, "mine")
		if err := saveChatLocked(workspace, c); err != nil {
			t.Fatal(err)
		}
	}
	tree = makeTree()
	if err := s.enrichTreeUserState(workspace, &tree, user); err != nil {
		t.Fatal(err)
	}
	chatResources := append([]resourceSnapshot{tree.Workspace, tree.Scheduler, tree.Projects[0]}, tree.Projects[0].Children[:3]...)
	for _, item := range chatResources {
		if item.Chat == nil || item.Chat.UnreadCount != 0 {
			t.Fatalf("read chat still has unread messages: %+v", item)
		}
	}
}
