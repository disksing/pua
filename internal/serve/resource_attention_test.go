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
	var state resourceUserStateSnapshot
	if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.ReadTurnNumber == nil || *state.ReadTurnNumber != 3 {
		t.Fatalf("read did not record current Turn: %#v", state)
	}

	tree, err := server.treeAt(context.Background(), workspace, app.LegacyDefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Activity.Unread) != 0 || tree.Projects[0].UnreadCount != 0 {
		t.Fatalf("read resource remained unread: %#v", tree.Activity.Unread)
	}

	record.TurnNumber = 4
	if err := rewriteTestGenerationRecords(workspace, []generationRecord{record}); err != nil {
		t.Fatal(err)
	}
	tree, err = server.treeAt(context.Background(), workspace, app.LegacyDefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Activity.Unread) != 0 || tree.Projects[0].UnreadCount != 1 {
		t.Fatalf("event unread should increase without entering chat activity: %#v", tree.Activity.Unread)
	}
}

func TestResourceReadAPIRejectsFutureTurnsAndNeverRegresses(t *testing.T) {
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
		var state resourceUserStateSnapshot
		if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		if state.ReadTurnNumber == nil || *state.ReadTurnNumber != 2 {
			t.Fatalf("read cursor regressed after Turn %d request: %#v", through, state)
		}
	}

	recorder := attentionRequest(t, server, http.MethodPut, "/api/workspaces/workspace-one/resources/project1/read", `{"throughTurnNumber":4}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("future read returned %d: %s", recorder.Code, recorder.Body.String())
	}
	state, err := server.resourceUserStateForResource(workspace, "project1", app.LegacyDefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	if state.ReadTurnNumber == nil || *state.ReadTurnNumber != 2 {
		t.Fatalf("future read changed cursor: %#v", state)
	}
}

func TestActiveTurnDoesNotCountAsUnreadAndCannotBeMarkedRead(t *testing.T) {
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
	if _, err := server.mutateResourceUserStateAtPath(workspace, "project1", func(state *resourceUserState) {
		state.ReadTurnNumber = intPointer(1)
	}, app.LegacyDefaultUserName); err != nil {
		t.Fatal(err)
	}
	tree, err := server.treeAt(context.Background(), workspace, app.LegacyDefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Activity.Running) != 1 || !tree.Activity.Running[0].Runtime.ActiveTurn {
		t.Fatalf("active Turn must remain in Running: %#v", tree.Activity.Running)
	}
	if len(tree.Activity.Unread) != 0 || tree.Projects[0].UnreadCount != 0 || tree.Projects[0].LatestTurnNumber != 1 {
		t.Fatalf("active Turn contributed to unread state: project=%#v unread=%#v", tree.Projects[0], tree.Activity.Unread)
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
	if len(tree.Activity.Running) != 0 || len(tree.Activity.Unread) != 0 || tree.Projects[0].UnreadCount != 1 || tree.Projects[0].LatestTurnNumber != 2 {
		t.Fatalf("completed Turn did not become unread: project=%#v activity=%#v", tree.Projects[0], tree.Activity)
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
	if state.ResourceStates["project1"].ReadTurnNumber == nil || *state.ResourceStates["project1"].ReadTurnNumber != 2 {
		t.Fatalf("ui-state update overwrote resource state: %#v", state.ResourceStates)
	}
}

func TestSchedulerCountsUnreadInEventTabs(t *testing.T) {
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
	if tree.Scheduler.UnreadCount != 3 || tree.Scheduler.LatestTurnNumber != 3 {
		t.Fatalf("scheduler must count unread event Turns: %#v", tree.Scheduler)
	}
	if len(tree.Activity.Unread) != 0 {
		t.Fatalf("event unread must not enter chat activity: %#v", tree.Activity.Unread)
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
	if len(tree.Activity.Unread) != 0 || tree.Projects[0].LatestTurnNumber != 4 {
		t.Fatalf("latest historical Turn must remain unread: %#v", tree.Activity.Unread)
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
	baselineRecords := append(records, generationRecord{
		ID: "gen-first-active", ResourceID: "project3", Generation: 1, GenerationID: "gen-first-active",
		AgentHubSessionID: "session-first-active", Status: "running", CurrentTurnID: "turn-1", TurnNumber: 1,
	})
	baseline := completedTurnBaseline(map[string]int{"project1": 4, "project2": 8, "project3": 1}, baselineRecords)
	if baseline["project1"] != 3 || baseline["project2"] != 8 {
		t.Fatalf("completed baseline = %#v, want project1=3 and fallback project2=8", baseline)
	}
	if _, exists := baseline["project3"]; exists {
		t.Fatalf("first active Turn remained in completed baseline: %#v", baseline)
	}
}

func TestResourceActivityListsAndSortsIndependentCategories(t *testing.T) {
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
		{ID: "gen-running-older", ResourceID: resourceIDs[1], Generation: 1, GenerationID: "gen-running-older", AgentHubSessionID: "session-running-older", Status: "running", CurrentTurnID: "turn-running-older", TurnNumber: 1, TurnStartedAt: "2026-08-13T00:00:10Z", UpdatedAt: "2026-08-13T00:00:59Z"},
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
	unread := make([]string, 0, len(tree.Activity.Unread))
	for _, item := range tree.Activity.Unread {
		unread = append(unread, item.ID)
	}
	if wantUnread := []string{}; !slices.Equal(unread, wantUnread) {
		t.Fatalf("Unread order = %v, want %v", unread, wantUnread)
	}
	running := []string{}
	for _, item := range tree.Activity.Running {
		running = append(running, item.ID)
	}
	if wantRunning := []string{resourceIDs[0], resourceIDs[1]}; !slices.Equal(running, wantRunning) {
		t.Fatalf("Running order = %v, want %v", running, wantRunning)
	}
}

func TestResourceActivityProblemsIncludeOnlyBlockedAndErrorTasks(t *testing.T) {
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
	got := make([]string, 0, len(tree.Activity.Problems))
	for _, item := range tree.Activity.Problems {
		got = append(got, item.ID)
	}
	if len(got) != 2 || !slices.Contains(got, blocked.ID) || !slices.Contains(got, errorTask.ID) || slices.Contains(got, waiting.ID) {
		t.Fatalf("Problems = %v, want blocked and error Tasks only", got)
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

func TestActivityChatIncludesReadAndClosedChatsForCurrentUser(t *testing.T) {
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
	if err := s.enrichTreeResourceActivity(workspace, &tree, user); err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, item := range tree.Activity.Unread {
		got = append(got, item.ID)
		if item.Chat.UnreadCount > 0 && item.LatestUnreadChatAt == "" {
			t.Fatalf("invalid chat activity: %+v", item)
		}
	}
	if !slices.Equal(got, []string{"project1.task1", "project1", "scheduler", "workspace", "project1.task2", "project1.task3"}) {
		t.Fatalf("chat order/membership: %v", got)
	}
	if tree.Projects[0].Children[1].DialogueEvents.UnreadCount != 5 {
		t.Fatal("dialogue-only unread must stay in event tab")
	}
	if tree.Activity.Unread[0].Chat.Open {
		t.Fatal("closed chat must remain closed")
	}
	if tree.Projects[0].Chat.UnreadCount != 1 || tree.Projects[0].LatestUnreadChatAt != "" {
		t.Fatal("tree chat must not aggregate or change its snapshot")
	}
	if len(tree.Activity.Running) != 1 || len(tree.Activity.Problems) != 1 || tree.Activity.Running[0].ID != "project1.task4" || tree.Activity.Problems[0].LatestTurnNumber != 9 {
		t.Fatalf("Running/Issues changed: %+v", tree.Activity)
	}
	other := makeTree()
	if err := s.enrichTreeResourceActivity(workspace, &other, "other"); err != nil {
		t.Fatal(err)
	}
	if len(other.Activity.Unread) != 6 || other.Projects[0].Children[3].UnreadCount != 9 || other.Projects[0].Chat == nil {
		t.Fatal("shared chat visibility or event counts changed")
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
	if err := s.enrichTreeResourceActivity(workspace, &tree, user); err != nil {
		t.Fatal(err)
	}
	if len(tree.Activity.Unread) != 6 || len(tree.Activity.Running) != 1 || len(tree.Activity.Problems) != 1 {
		t.Fatal("reading chat must preserve all chat activity")
	}
	for _, item := range tree.Activity.Unread {
		if item.Chat.UnreadCount != 0 {
			t.Fatalf("read chat still has unread messages: %+v", item)
		}
	}
}

func TestActivityChatSortsByLatestMessageRegardlessOfReadState(t *testing.T) {
	s, workspace := attentionTestServer(t)
	user := app.LegacyDefaultUserName
	chats := []resourceChat{
		{ResourceID: "project1", Messages: []chatMessage{{Role: "agent", CreatedAt: "2026-10-19T01:00:00Z"}}},
		{ResourceID: "project1.task1", ReadThrough: 1, Messages: []chatMessage{{Role: "agent", CreatedAt: "2026-10-19T01:00:02Z"}}},
		{ResourceID: "project1.task2", Messages: []chatMessage{{Role: "user", CreatedAt: "2026-10-19T09:00:01+08:00"}}},
		{ResourceID: "project1.task3"},
		{ResourceID: "project1.task4", Messages: []chatMessage{{Role: "user", CreatedAt: "invalid"}}},
	}
	for i := range chats {
		chats[i].ID = sharedChatID(chats[i].ResourceID)
		chats[i].Version = 4
		chats[i].ReadCursors = map[string]int{user: chats[i].ReadThrough}
		chats[i].Epoch = 1
		chats[i].Open = true
		if err := saveChatLocked(workspace, chats[i]); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want []string) {
		t.Helper()
		tree := workspaceTree{Projects: []resourceSnapshot{{ID: "project1", Type: "project", Children: []resourceSnapshot{
			{ID: "project1.task1", Type: "task"}, {ID: "project1.task2", Type: "task"},
			{ID: "project1.task3", Type: "task"}, {ID: "project1.task4", Type: "task"},
		}}}}
		if err := s.enrichTreeResourceActivity(workspace, &tree, user); err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, item := range tree.Activity.Unread {
			got = append(got, item.ID)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("chat order = %v, want %v", got, want)
		}
	}
	original := []string{"project1.task1", "project1.task2", "project1", "project1.task3", "project1.task4"}
	check(original)
	// Reading and closing a chat must not demote its latest message.
	for i := range chats {
		chats[i].ReadCursors[user] = len(chats[i].Messages)
		chats[i].Open = false
		if err := saveChatLocked(workspace, chats[i]); err != nil {
			t.Fatal(err)
		}
	}
	check(original)
	// Both a user input and an agent reply move the updated chat to the top.
	for _, role := range []string{"user", "agent"} {
		chats[0].Messages = append(chats[0].Messages, chatMessage{Role: role, CreatedAt: "2026-10-19T01:00:03Z"})
		if err := saveChatLocked(workspace, chats[0]); err != nil {
			t.Fatal(err)
		}
		check([]string{"project1", "project1.task1", "project1.task2", "project1.task3", "project1.task4"})
		chats[0].Messages = chats[0].Messages[:1]
	}
}
