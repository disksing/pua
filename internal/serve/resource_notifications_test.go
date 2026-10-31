package serve

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func newNotificationTestManager(t *testing.T, hubURL string, workspaces []serveWorkspace) *agentManager {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "serve.json")
	data, err := json.Marshal(agentHubServeConfig{
		Version: agentHubConfigVersion, Workspaces: workspaces,
		AgentHubEndpoint: hubURL, AgentHubInstanceID: "pua-notification-test",
		AgentProfiles: []agentHubProfileRoute{{Key: "default", AgentName: "fake-agent"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	server := &server{config: configPath, addr: "127.0.0.1:4936"}
	manager := newAgentManager(server)
	server.agents = manager
	return manager
}

func appendNotificationTestMessage(t *testing.T, workspacePath string, message resourceMailboxMessage) {
	t.Helper()
	_, err := mutateResourceMailbox(workspacePath, func(mailbox *resourceMailbox) error {
		mailbox.NextSequence++
		message.Sequence = mailbox.NextSequence
		mailbox.Messages = append(mailbox.Messages, message)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedNotificationModesUseOrdinaryMailboxReconciliation(t *testing.T) {
	tests := []struct {
		name             string
		generationStatus string
		sessionState     string
		steer            bool
		wantActual       string
		wantReason       string
		wantStatus       string
		wantSteerCall    bool
	}{
		{name: "active steer", generationStatus: "running", sessionState: "running", steer: true, wantActual: resourceMessageModeSteer, wantStatus: resourceMessageDelivered, wantSteerCall: true},
		{name: "no active turn", generationStatus: "idle", sessionState: "ready", steer: true, wantActual: resourceMessageModeEnqueue, wantReason: resourceMessageReasonNoActiveTurn, wantStatus: resourceMessageDelivered},
		{name: "active turn without steer capability", generationStatus: "running", sessionState: "running", wantActual: resourceMessageModeEnqueue, wantReason: resourceMessageReasonSteerUnsupported, wantStatus: resourceMessageQueued},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newRuntimeFakeAgentHub()
			hub := httptest.NewServer(fake)
			defer hub.Close()
			manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
			now := time.Now().Format(time.RFC3339Nano)
			record := generationRecord{
				ID: "gen-generated-" + strings.ReplaceAll(test.name, " ", "-"), WorkspaceID: workspace.ID, ResourceID: "project1.task1",
				Generation: 1, GenerationID: "gen-generated-" + strings.ReplaceAll(test.name, " ", "-"), AgentHubSessionID: "ses-generated-" + strings.ReplaceAll(test.name, " ", "-"),
				AgentHubAgentName: "fake-agent", SourceExternalID: workspace.ID + "/generated", Status: test.generationStatus,
				CreatedAt: now, UpdatedAt: now,
			}
			seedPollerGeneration(t, fake, workspace, record, agentHubSession{
				ID: record.AgentHubSessionID, State: test.sessionState, CurrentTurnID: "turn-generated",
				InputCapabilities: agentHubInputCapabilities{Steer: test.steer}, UpdatedAt: now,
			})

			generated := resourceMailboxMessage{
				ID: "generated-" + strings.ReplaceAll(test.name, " ", "-"), ResourceID: record.ResourceID, Text: "generated result",
				RequestedMode: resourceMessageModeSteer, ActualMode: resourceMessageModeSteer,
				Type:      resourceMessageTypeDeliveryTerminal,
				Causation: &resourceMessageCausation{Type: resourceMessageTypeDeliveryTerminal, SourceResourceID: "project1.task2", MessageID: "source-message"},
			}
			accepted, err := acceptGeneratedMailboxMessage(workspace.Path, generated)
			if err != nil {
				t.Fatal(err)
			}
			if accepted.Role != "system" || accepted.SubscribeResult != nil ||
				accepted.RequestedMode != resourceMessageModeSteer || accepted.ActualMode != resourceMessageModeSteer || accepted.ModeFrozen {
				t.Fatalf("generated durable accept did not preserve initial steer mapping: %#v", accepted)
			}
			if err := manager.withResourceController(context.Background(), workspace, record.ResourceID, func() error {
				return manager.reconcileResourceMailboxLocked(context.Background(), workspace, record.ResourceID)
			}); err != nil {
				t.Fatal(err)
			}
			result, found, err := mailboxMessageByID(workspace.Path, generated.ID)
			if err != nil || !found {
				t.Fatalf("generated message lookup: found=%v err=%v", found, err)
			}
			if result.ActualMode != test.wantActual || !result.ModeFrozen || result.DowngradeReason != test.wantReason || result.Status != test.wantStatus {
				t.Fatalf("generated mode reconciliation = %#v", result)
			}
			if test.name == "active steer" && result.TurnID != "turn-generated" {
				t.Fatalf("active steer was not bound to the exact active Turn: %#v", result)
			}
			fake.mu.Lock()
			steers := append([]bool(nil), fake.messageSteers...)
			fake.mu.Unlock()
			if test.wantSteerCall {
				if len(steers) != 1 || !steers[0] {
					t.Fatalf("active steer delivery = %#v", steers)
				}
			} else if test.wantStatus == resourceMessageDelivered {
				if len(steers) != 1 || steers[0] {
					t.Fatalf("enqueue downgrade delivery = %#v", steers)
				}
			} else if len(steers) != 0 {
				t.Fatalf("unsupported steer should wait without AgentHub delivery: %#v", steers)
			}
		})
	}
}

func TestGeneratedNotificationFrozenModeDoesNotDriftOnRetry(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	now := time.Now().Format(time.RFC3339Nano)
	record := generationRecord{
		ID: "gen-frozen-generated", WorkspaceID: workspace.ID, ResourceID: "project1.task1", Generation: 1,
		GenerationID: "gen-frozen-generated", AgentHubSessionID: "ses-frozen-generated", AgentHubAgentName: "fake-agent",
		SourceExternalID: workspace.ID + "/frozen-generated", Status: "running", CreatedAt: now, UpdatedAt: now,
	}
	seedPollerGeneration(t, fake, workspace, record, agentHubSession{
		ID: record.AgentHubSessionID, State: "running", CurrentTurnID: "turn-frozen", UpdatedAt: now,
	})
	generated := resourceMailboxMessage{
		ID: "generated-frozen", ResourceID: record.ResourceID, Text: "frozen result",
		RequestedMode: resourceMessageModeSteer, ActualMode: resourceMessageModeSteer,
		Type:      resourceMessageTypeDeliveryTerminal,
		Causation: &resourceMessageCausation{Type: resourceMessageTypeDeliveryTerminal, SourceResourceID: "project1.task2", MessageID: "source-frozen"},
	}
	if _, err := acceptGeneratedMailboxMessage(workspace.Path, generated); err != nil {
		t.Fatal(err)
	}
	if err := manager.withResourceController(context.Background(), workspace, record.ResourceID, func() error {
		return manager.reconcileResourceMailboxLocked(context.Background(), workspace, record.ResourceID)
	}); err != nil {
		t.Fatal(err)
	}
	first, found, err := mailboxMessageByID(workspace.Path, generated.ID)
	if err != nil || !found || first.Status != resourceMessageQueued || first.ActualMode != resourceMessageModeEnqueue ||
		!first.ModeFrozen || first.DowngradeReason != resourceMessageReasonSteerUnsupported {
		t.Fatalf("initial frozen downgrade = found=%v err=%v message=%#v", found, err, first)
	}

	fake.mu.Lock()
	session := fake.sessions[record.AgentHubSessionID]
	session.InputCapabilities.Steer = true
	fake.sessions[session.ID] = session
	fake.mu.Unlock()
	if err := manager.withResourceController(context.Background(), workspace, record.ResourceID, func() error {
		return manager.reconcileResourceMailboxLocked(context.Background(), workspace, record.ResourceID)
	}); err != nil {
		t.Fatal(err)
	}
	second, found, err := mailboxMessageByID(workspace.Path, generated.ID)
	if err != nil || !found || second.Status != resourceMessageQueued || second.RequestedMode != resourceMessageModeSteer ||
		second.ActualMode != resourceMessageModeEnqueue || !second.ModeFrozen || second.DowngradeReason != resourceMessageReasonSteerUnsupported {
		t.Fatalf("frozen retry drifted mode = found=%v err=%v message=%#v", found, err, second)
	}
	fake.mu.Lock()
	steers := append([]bool(nil), fake.messageSteers...)
	fake.mu.Unlock()
	if len(steers) != 0 {
		t.Fatalf("frozen enqueue retry unexpectedly called AgentHub: %#v", steers)
	}
}

func TestTerminalDeliveryNoticeRoutesToResourceSenderWithoutBounce(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()

	sourceRoot := t.TempDir()
	sourceApp, err := app.Initialize(sourceRoot, "en")
	if err != nil {
		t.Fatal(err)
	}
	sourceProject, _ := sourceApp.CreateProject("Source", "source")
	sourceTask, err := sourceApp.CreateTask(app.CreateTaskInput{ProjectID: sourceProject.ID, Title: "Source task"})
	if err != nil {
		t.Fatal(err)
	}
	sourceRuntime, _ := sourceApp.RuntimeConfig()
	targetRoot := t.TempDir()
	targetApp, err := app.Initialize(targetRoot, "en")
	if err != nil {
		t.Fatal(err)
	}
	targetProject, _ := targetApp.CreateProject("Target", "target")
	targetTask, err := targetApp.CreateTask(app.CreateTaskInput{ProjectID: targetProject.ID, Title: "Target task"})
	if err != nil {
		t.Fatal(err)
	}
	targetRuntime, _ := targetApp.RuntimeConfig()
	sourceWorkspace := serveWorkspace{ID: "source", Path: sourceRoot}
	targetWorkspace := serveWorkspace{ID: "target", Path: targetRoot}
	manager := newNotificationTestManager(t, hub.URL, []serveWorkspace{sourceWorkspace, targetWorkspace})
	client, _ := newAgentHubClient(hub.URL, nil)
	now := time.Now().Format(time.RFC3339Nano)
	original := resourceMailboxMessage{
		ID: "msg-failed", ResourceID: targetTask.ID, Text: "Never delivered", Role: "agent",
		Sender: &agentHubMessageSender{ID: sourceTask.ID}, SenderWorkspaceInstanceID: sourceRuntime.InstanceID,
		RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, Status: resourceMessageUndeliverable,
		AcceptedAt: now, UpdatedAt: now, TerminalAt: now, LastError: "target was archived", LastErrorCode: "resource_archived",
	}
	appendNotificationTestMessage(t, targetRoot, original)
	if err := manager.reconcileWorkspaceNotifications(context.Background(), targetWorkspace, client); err != nil {
		t.Fatal(err)
	}
	sourceMailbox, err := loadResourceMailbox(sourceRoot)
	if err != nil || len(sourceMailbox.Messages) != 1 {
		t.Fatalf("delivery notice mailbox = %#v, %v", sourceMailbox.Messages, err)
	}
	notice := sourceMailbox.Messages[0]
	if notice.Type != resourceMessageTypeDeliveryTerminal || notice.Role != "system" || notice.SubscribeResult != nil || notice.RequestedMode != resourceMessageModeSteer || notice.Causation == nil ||
		notice.Causation.TerminalCode != "resource_archived" || notice.SenderWorkspaceInstanceID != targetRuntime.InstanceID {
		t.Fatalf("delivery notice = %#v", notice)
	}
	unknown := original
	unknown.ID = "msg-unknown"
	unknown.Status = resourceMessageDeliveryUnknown
	unknown.LastError = "the upstream acceptance outcome could not be confirmed"
	unknown.LastErrorCode = "delivery_outcome_unknown"
	appendNotificationTestMessage(t, targetRoot, unknown)
	if err := manager.reconcileWorkspaceNotifications(context.Background(), targetWorkspace, client); err != nil {
		t.Fatal(err)
	}
	sourceMailbox, err = loadResourceMailbox(sourceRoot)
	if err != nil || len(sourceMailbox.Messages) != 2 {
		t.Fatalf("delivery_unknown notice mailbox = %#v, %v", sourceMailbox.Messages, err)
	}
	unknownNotice := sourceMailbox.Messages[1]
	if unknownNotice.Role != "system" || unknownNotice.SubscribeResult != nil || unknownNotice.RequestedMode != resourceMessageModeSteer || unknownNotice.Causation == nil || unknownNotice.Causation.MessageID != unknown.ID ||
		unknownNotice.Causation.TerminalCode != "delivery_outcome_unknown" || !strings.Contains(unknownNotice.Text, "delivery_unknown") ||
		strings.Contains(strings.ToLower(unknownNotice.Text), "not delivered") {
		t.Fatalf("delivery_unknown was misrepresented: %#v", unknownNotice)
	}
	if err := manager.reconcileWorkspaceNotifications(context.Background(), sourceWorkspace, client); err != nil {
		t.Fatal(err)
	}
	updatedMailbox, _ := loadResourceMailbox(sourceRoot)
	if len(updatedMailbox.Messages) != 2 {
		t.Fatalf("generated failure notice bounced: %#v", updatedMailbox.Messages)
	}
}

func TestNotificationReceiptTerminatesWhenTargetWorkspaceIsUnavailable(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	workspace := serveWorkspace{ID: "source", Path: root}
	manager := newNotificationTestManager(t, hub.URL, []serveWorkspace{workspace})
	now := time.Now().Format(time.RFC3339Nano)
	source := resourceMailboxMessage{
		ID: "msg-source", ResourceID: "workspace", Text: "source", Role: "agent",
		RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue,
		Status: resourceMessageDelivered, AcceptedAt: now, UpdatedAt: now,
		Notification: &resourceNotificationReceipt{
			ID: "msg-notify-missing", Type: resourceMessageTypeDeliveryTerminal, Status: resourceNotificationWaiting,
			TargetWorkspaceInstanceID: "ws-missing", TargetResourceID: "project1.task1", CreatedAt: now, UpdatedAt: now,
		},
	}
	appendNotificationTestMessage(t, root, source)
	generated := resourceMailboxMessage{
		ID: source.Notification.ID, ResourceID: "project1.task1", Text: "result",
		Type:      resourceMessageTypeDeliveryTerminal,
		Causation: &resourceMessageCausation{Type: resourceMessageTypeDeliveryTerminal, SourceWorkspaceInstanceID: "ws-source", SourceResourceID: "workspace"},
	}
	if err := manager.routeNotification(context.Background(), workspace, source, generated); err != nil {
		t.Fatal(err)
	}
	updated, found, err := mailboxMessageByID(root, source.ID)
	if err != nil || !found || updated.Notification == nil || updated.Notification.Status != resourceNotificationTerminal ||
		updated.Notification.LastErrorCode != "target_workspace_unavailable" {
		t.Fatalf("unavailable target receipt = %#v, found=%v err=%v", updated.Notification, found, err)
	}
}
