package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/workspacepath"
)

func TestCreatedTaskStartsWithoutOpeningChat(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	fake.enforceMessageIDs = true
	hub := httptest.NewServer(fake)
	t.Cleanup(hub.Close)
	m, workspace, _ := newRuntimeTestManager(t, hub.URL)
	created := httptest.NewRecorder()
	m.server.handleWorkspace(created, httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspace.ID+"/tasks", bytes.NewBufferString(`{"project":"project1","title":"Auto started task","detail":"Begin work"}`)))
	mustChatOK(t, created)
	var task struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &task); err != nil || task.ID == "" {
		t.Fatalf("created task: %s, error: %v", created.Body.String(), err)
	}
	if _, found, err := currentResourceGeneration(workspace.Path, task.ID); err != nil || found {
		t.Fatalf("task creation started a generation: found=%v, error=%v", found, err)
	}

	input := "Please read task.md and begin work."
	data, err := json.Marshal(map[string]any{
		"text": input, "role": "system", "sender": map[string]string{"name": "PUA Task Start"}, "subscribeResult": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := httptest.NewRecorder()
	m.server.handleWorkspace(started, httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspace.ID+"/resources/"+task.ID+"/messages", bytes.NewReader(data)))
	if started.Code != http.StatusAccepted {
		t.Fatalf("auto-start: %d %s", started.Code, started.Body.String())
	}
	if err := m.withResourceController(context.Background(), workspace, task.ID, func() error {
		return m.reconcileResourceMailboxLocked(context.Background(), workspace, task.ID)
	}); err != nil {
		t.Fatal(err)
	}
	generation, found, err := currentResourceGeneration(workspace.Path, task.ID)
	if err != nil || !found || generation.AgentHubSessionID == "" {
		t.Fatalf("auto-start did not create an execution session: %#v, error=%v", generation, err)
	}
	mailbox, err := loadResourceMailboxForResource(workspace.Path, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mailbox.Messages) != 1 || mailbox.Messages[0].Status != resourceMessageDelivered {
		t.Fatalf("auto-start input not delivered exactly once: %#v", mailbox.Messages)
	}
	message := mailbox.Messages[0]
	if message.Role != "system" || message.SubscribeResult || message.Sender == nil || message.Sender.Name != "PUA Task Start" {
		t.Fatalf("unexpected start message: %#v", message)
	}
	fake.mu.Lock()
	providerInput := fake.messageInputs[message.ID]
	fake.mu.Unlock()
	if !strings.Contains(providerInput.Text, input) {
		t.Fatalf("start prompt not delivered to provider: %#v", providerInput)
	}
	chatDir := filepath.Join(workspacepath.ControlDir(workspace.Path), "chats")
	entries, err := os.ReadDir(chatDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("auto-start created chat state: %v", entries)
	}
}
