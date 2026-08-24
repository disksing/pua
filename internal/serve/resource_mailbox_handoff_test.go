package serve

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func configureMailboxHandoffAgentHub(t *testing.T, server *server, endpoint string) {
	t.Helper()
	cfg, err := server.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AgentHubEndpoint = endpoint
	cfg.AgentHubInstanceID = "pua-mailbox-handoff-test"
	cfg.AgentProfiles = []agentProfileRoute{{Key: "default", AgentName: "fake-agent"}}
	if err := server.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func requireWorkspaceOwnershipError(t *testing.T, err error) {
	t.Helper()
	var apiErr *resourceAPIError
	if !errors.As(err, &apiErr) || apiErr.Code != "workspace_not_owned" {
		t.Fatalf("mailbox reconcile ownership error = %v", err)
	}
}

func TestResourceMailboxReconcileAllowsIsolatedManagerWithoutServer(t *testing.T) {
	workspacePath := t.TempDir()
	if _, err := app.Initialize(workspacePath, "en"); err != nil {
		t.Fatal(err)
	}
	manager := newAgentManager(nil)
	workspace := serveWorkspace{ID: "isolated", Path: workspacePath}
	if err := manager.reconcileResourceMailboxLocked(context.Background(), workspace, "workspace"); err != nil {
		t.Fatalf("isolated storage-only mailbox reconcile: %v", err)
	}
}

func TestResourceMailboxReconcileCannotCrossWorkspaceOwnershipHandoff(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	var hubRequests atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hubRequests.Add(1)
		fake.ServeHTTP(w, r)
	}))
	defer hub.Close()
	first, second, workspace, _ := newSchedulerOwnershipHandoffFixture(t)
	configureMailboxHandoffAgentHub(t, first, hub.URL)
	configureMailboxHandoffAgentHub(t, second, hub.URL)

	schedulerController, releaseScheduler := holdSchedulerController(t, first.agents, workspace)
	targetController, releaseTarget := holdResourceController(t, first.agents, workspace, "workspace")

	schedulerMessage, err := first.agents.acceptResourceMessageDurable(context.Background(), workspace, app.SchedulerResourceID, resourceMessageRequest{
		Text: "compile a natural-language schedule", Mode: resourceMessageModeEnqueue, Role: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	targetMessage, err := first.agents.acceptResourceMessageDurable(context.Background(), workspace, "workspace", resourceMessageRequest{
		Text: "ordinary target follow-up", Mode: resourceMessageModeEnqueue, Role: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	schedulerBefore, err := loadResourceMailboxStoreForRead(workspace.Path, app.SchedulerResourceID)
	if err != nil {
		t.Fatal(err)
	}
	targetBefore, err := loadResourceMailboxStoreForRead(workspace.Path, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	generationsBefore, err := loadGenerationRecords(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}

	removeDone := make(chan error, 1)
	go func() { removeDone <- first.removeWorkspace(workspace.ID) }()
	waitForSchedulerControllerQueue(t, schedulerController, 1)
	first.enqueueSchedulerMailboxReconcile(workspace, schedulerMessage)
	waitForSchedulerControllerQueue(t, schedulerController, 2)

	cancelledContext, cancelQueued := context.WithCancel(context.Background())
	cancelledDone := make(chan error, 1)
	go func() {
		cancelledDone <- first.agents.withResourceController(cancelledContext, workspace, "workspace", func() error {
			return errors.New("cancelled mailbox reconcile unexpectedly ran")
		})
	}()
	waitForResourceControllerQueue(t, targetController, 1)
	cancelQueued()
	if err := <-cancelledDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled queued mailbox reconcile = %v", err)
	}
	targetReconcileDone := make(chan error, 1)
	if err := first.agents.enqueueResourceController(workspace, "workspace", func() error {
		reconcileErr := first.agents.reconcileResourceMailboxLocked(context.Background(), workspace, "workspace")
		if reconcileErr != nil {
			recordMailboxFailure(workspace.Path, targetMessage.ID, reconcileErr)
		}
		targetReconcileDone <- reconcileErr
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitForResourceControllerQueue(t, targetController, 2)

	releaseScheduler()
	if err := <-removeDone; err != nil {
		t.Fatalf("remove Workspace: %v", err)
	}
	if err := first.agents.withResourceController(context.Background(), workspace, app.SchedulerResourceID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if first.ownsWorkspace(workspace.Path) {
		t.Fatal("removed Server still owns the Workspace")
	}

	workspace = addWorkspaceAfterSchedulerHandoff(t, second, workspace)
	releaseTarget()
	select {
	case err := <-targetReconcileDone:
		requireWorkspaceOwnershipError(t, err)
	case <-time.After(time.Second):
		t.Fatal("stale ordinary mailbox follow-up did not finish")
	}
	if err := first.agents.withResourceController(context.Background(), workspace, "workspace", func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	schedulerAfter, err := loadResourceMailboxStoreForRead(workspace.Path, app.SchedulerResourceID)
	if err != nil {
		t.Fatal(err)
	}
	targetAfter, err := loadResourceMailboxStoreForRead(workspace.Path, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	generationsAfter, err := loadGenerationRecords(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(schedulerAfter, schedulerBefore) {
		t.Fatalf("stale Scheduler follow-up changed mailbox: before=%#v after=%#v", schedulerBefore, schedulerAfter)
	}
	if !reflect.DeepEqual(targetAfter, targetBefore) {
		t.Fatalf("stale target follow-up changed mailbox: before=%#v after=%#v", targetBefore, targetAfter)
	}
	if !reflect.DeepEqual(generationsAfter, generationsBefore) {
		t.Fatalf("stale mailbox follow-up changed generations: before=%#v after=%#v", generationsBefore, generationsAfter)
	}
	fake.mu.Lock()
	staleSessions, staleInputs := len(fake.sessions), len(fake.messageIDs)
	fake.mu.Unlock()
	if staleSessions != 0 || staleInputs != 0 {
		t.Fatalf("stale mailbox follow-up contacted AgentHub: sessions=%d inputs=%d", staleSessions, staleInputs)
	}
	if staleRequests := hubRequests.Load(); staleRequests != 0 {
		t.Fatalf("stale mailbox follow-up made %d AgentHub requests", staleRequests)
	}

	closed := make(chan struct{})
	go func() {
		first.agents.waitBackground()
		first.locks.closeAll()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("stale Server close did not drain cancelled and guarded mailbox work")
	}

	for _, resourceID := range []string{app.SchedulerResourceID, "workspace"} {
		if err := second.agents.withResourceController(context.Background(), workspace, resourceID, func() error {
			return second.agents.reconcileResourceMailboxLocked(context.Background(), workspace, resourceID)
		}); err != nil {
			t.Fatalf("new owner reconcile %s: %v", resourceID, err)
		}
	}
	for _, messageID := range []string{schedulerMessage.ID, targetMessage.ID} {
		message, found, err := mailboxMessageByID(workspace.Path, messageID)
		if err != nil || !found || message.Status != resourceMessageDelivered {
			t.Fatalf("new owner delivery %s = found %v, message %#v, error %v", messageID, found, message, err)
		}
	}
	fake.mu.Lock()
	newOwnerSessions := len(fake.sessions)
	fake.mu.Unlock()
	if newOwnerSessions != 2 {
		t.Fatalf("new owner AgentHub sessions = %d, want 2", newOwnerSessions)
	}
	if newOwnerRequests := hubRequests.Load(); newOwnerRequests == 0 {
		t.Fatal("new owner delivery did not contact AgentHub")
	}
}
