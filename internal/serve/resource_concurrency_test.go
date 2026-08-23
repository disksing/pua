package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func addRuntimeTestWorkspace(t *testing.T, manager *agentManager, id, name string) serveWorkspace {
	t.Helper()
	path := t.TempDir()
	puaWorkspace, err := app.Initialize(path, "en")
	if err != nil {
		t.Fatal(err)
	}
	project, err := puaWorkspace.CreateProject(name+" project", strings.ToLower(name)+"-project")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.CreateTask(app.CreateTaskInput{ProjectID: project.ID, Title: name + " task", Slug: strings.ToLower(name) + "-task"}); err != nil {
		t.Fatal(err)
	}
	workspace := serveWorkspace{ID: id, Name: name, Path: path}
	cfg, err := manager.server.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspaces = append(cfg.Workspaces, workspace)
	if err := manager.server.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func waitForResourceMailboxMessage(t *testing.T, workspacePath, messageID string, predicate func(resourceMailboxMessage) bool) resourceMailboxMessage {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		message, found, err := mailboxMessageByID(workspacePath, messageID)
		if err != nil {
			t.Fatal(err)
		}
		if found && predicate(message) {
			return message
		}
		select {
		case <-deadline.C:
			t.Fatalf("mailbox message %s did not reach the expected state", messageID)
		case <-ticker.C:
		}
	}
}

func TestResourceControllerPreservesFIFOAndIsolatesResources(t *testing.T) {
	manager, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var sameSecondRan = make(chan struct{})
	var sameThirdRan = make(chan struct{})
	otherRan := make(chan struct{})
	order := make(chan string, 4)
	firstDone := make(chan error, 1)
	otherDone := make(chan error, 1)

	go func() {
		firstDone <- manager.withResourceController(context.Background(), workspace, "project1", func() error {
			close(firstStarted)
			order <- "first"
			<-releaseFirst
			return nil
		})
	}()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first resource operation did not start")
	}

	controller, err := manager.controllerForResource(workspace, "project1")
	if err != nil {
		t.Fatal(err)
	}
	secondDone := controller.enqueue(context.Background(), func() error {
		close(sameSecondRan)
		order <- "second"
		return nil
	})
	thirdDone := controller.enqueue(context.Background(), func() error {
		close(sameThirdRan)
		order <- "third"
		return nil
	})
	go func() {
		otherDone <- manager.withResourceController(context.Background(), workspace, "project1.task1", func() error {
			close(otherRan)
			order <- "other"
			return nil
		})
	}()

	select {
	case <-otherRan:
	case <-time.After(time.Second):
		t.Fatal("a different resource remained blocked behind the first resource")
	}
	select {
	case <-sameSecondRan:
		t.Fatal("same-resource operation ran before the first operation completed")
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-sameThirdRan:
		t.Fatal("third same-resource operation ran before the first operation completed")
	default:
	}

	close(releaseFirst)
	observedOrder := make([]string, 0, 4)
	for range 4 {
		select {
		case label := <-order:
			observedOrder = append(observedOrder, label)
		case <-time.After(time.Second):
			t.Fatalf("resource controller operations did not produce a complete order: %#v", observedOrder)
		}
	}
	positions := make(map[string]int, len(observedOrder))
	for index, label := range observedOrder {
		positions[label] = index
	}
	if positions["first"] >= positions["second"] || positions["second"] >= positions["third"] {
		t.Fatalf("same-resource operations lost FIFO order: %#v", observedOrder)
	}
	for name, done := range map[string]<-chan error{
		"first": firstDone, "second": secondDone, "third": thirdDone, "other": otherDone,
	} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s resource operation failed: %v", name, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s resource operation did not complete", name)
		}
	}
}

func TestAcceptedMessageDoesNotBlockAnotherWorkspaceStatus(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspaceA, _ := newRuntimeTestManager(t, hub.URL)
	workspaceB := addRuntimeTestWorkspace(t, manager, "workspace-other", "Other")

	_, recordA := startRuntimeTestGeneration(t, manager, workspaceA, `{"resourceId":"project1.task1","prompt":"initial A"}`)
	_, recordB := startRuntimeTestGeneration(t, manager, workspaceB, `{"resourceId":"project1.task1","prompt":"initial B"}`)
	if recordA.AgentHubSessionID == "" || recordB.AgentHubSessionID == "" {
		t.Fatalf("test generations did not start: A=%#v B=%#v", recordA, recordB)
	}
	if err := manager.withResourceController(context.Background(), workspaceA, "project1.task1", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := manager.withResourceController(context.Background(), workspaceB, "project1.task1", func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	for _, sessionID := range []string{recordA.AgentHubSessionID, recordB.AgentHubSessionID} {
		session := fake.sessions[sessionID]
		session.State = "ready"
		session.CurrentTurnID = ""
		fake.sessions[sessionID] = session
	}
	messageStarted := make(chan struct{})
	releaseMessage := make(chan struct{})
	fake.messageHook = func(sessionID string, message agentHubInboundMessage) {
		payload, ok := decodePUAMessagePayload(message.Payload)
		if sessionID != recordA.AgentHubSessionID || !ok || payload.Text != "blocked A" {
			return
		}
		close(messageStarted)
		<-releaseMessage
	}
	fake.mu.Unlock()

	sendRequest := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspaceA.ID+"/resources/project1.task1/messages", strings.NewReader(`{"text":"blocked A","mode":"enqueue"}`))
	sendRecorder := httptest.NewRecorder()
	sendDone := make(chan struct{})
	go func() {
		manager.server.handleWorkspace(sendRecorder, sendRequest)
		close(sendDone)
	}()
	select {
	case <-sendDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("durable message acceptance waited for the blocked AgentHub request")
	}
	if sendRecorder.Code != http.StatusAccepted {
		t.Fatalf("message acceptance returned %d: %s", sendRecorder.Code, sendRecorder.Body.String())
	}
	var sent resourceMessageResponse
	if err := json.Unmarshal(sendRecorder.Body.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.MessageID == "" {
		t.Fatal("accepted message did not return a stable message id")
	}
	select {
	case <-messageStarted:
	case <-time.After(time.Second):
		t.Fatal("background delivery did not reach the controlled AgentHub barrier")
	}

	statusRecorder := httptest.NewRecorder()
	statusDone := make(chan struct{})
	go func() {
		manager.server.handleWorkspace(statusRecorder, httptest.NewRequest(http.MethodGet, "/api/workspaces/"+workspaceB.ID+"/resources/project1.task1/status", nil))
		close(statusDone)
	}()
	select {
	case <-statusDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("another Workspace status waited for the blocked resource")
	}
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("other Workspace status returned %d: %s", statusRecorder.Code, statusRecorder.Body.String())
	}
	bSendRecorder := httptest.NewRecorder()
	bSendDone := make(chan struct{})
	go func() {
		manager.server.handleWorkspace(bSendRecorder, httptest.NewRequest(http.MethodPost,
			"/api/workspaces/"+workspaceB.ID+"/resources/project1.task1/messages",
			strings.NewReader(`{"text":"independent B","mode":"enqueue"}`)))
		close(bSendDone)
	}()
	select {
	case <-bSendDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("another Workspace send waited for the blocked resource")
	}
	if bSendRecorder.Code != http.StatusAccepted {
		t.Fatalf("other Workspace send returned %d: %s", bSendRecorder.Code, bSendRecorder.Body.String())
	}
	var bSent resourceMessageResponse
	if err := json.Unmarshal(bSendRecorder.Body.Bytes(), &bSent); err != nil {
		t.Fatal(err)
	}

	close(releaseMessage)
	waitForResourceMailboxMessage(t, workspaceA.Path, sent.MessageID, func(message resourceMailboxMessage) bool {
		return message.Status == resourceMessageDelivered
	})
	waitForResourceMailboxMessage(t, workspaceB.Path, bSent.MessageID, func(message resourceMailboxMessage) bool {
		return message.Status == resourceMessageDelivered
	})
}

func TestAcceptedMessageRemainsAcceptedWhenBackgroundDeliveryFails(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	fake.mu.Lock()
	fake.failNextMessage = true
	fake.mu.Unlock()

	recorder := httptest.NewRecorder()
	manager.server.handleWorkspace(recorder, httptest.NewRequest(http.MethodPost,
		"/api/workspaces/"+workspace.ID+"/resources/project1.task1/messages",
		strings.NewReader(`{"text":"will fail after acceptance","mode":"enqueue"}`)))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("background delivery failure changed acceptance response to %d: %s", recorder.Code, recorder.Body.String())
	}
	var response resourceMessageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.MessageID == "" {
		t.Fatal("accepted failure test did not return a message id")
	}
	waitForResourceMailboxMessage(t, workspace.Path, response.MessageID, func(message resourceMailboxMessage) bool {
		return message.Status == resourceMessageDelivering && message.LastErrorCode != ""
	})
}
