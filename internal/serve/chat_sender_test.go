package serve

import (
	"testing"

	"github.com/disksing/pua/internal/app"
)

func TestChatSenderUsesMatchingGenerationAndPreservesSnapshots(t *testing.T) {
	tests := []struct {
		name    string
		record  generationRecord
		work    turnWork
		retired bool
		want    string
	}{
		{name: "actual agent before profile", record: generationRecord{AgentHubAgentName: "  Original Agent  ", ResolvedProfile: "fast", BindingKind: "profile", BindingName: "fast"}, want: "Original Agent"},
		{name: "direct agent", record: generationRecord{BindingKind: "agent", BindingName: "Direct Agent"}, want: "Direct Agent"},
		{name: "resolved profile before requested", record: generationRecord{ResolvedProfile: "default", BindingKind: "profile", BindingName: "deleted"}, want: "default"},
		{name: "profile binding fallback", record: generationRecord{BindingKind: "profile", BindingName: "reasoning"}, want: "reasoning"},
		{name: "retired generation", record: generationRecord{AgentHubAgentName: "Retired Agent"}, retired: true, want: "Retired Agent"},
		{name: "missing names"},
		{name: "blank names", record: generationRecord{AgentHubAgentName: " ", ResolvedProfile: "\t", BindingKind: "agent", BindingName: " "}},
		{name: "unknown binding kind", record: generationRecord{BindingKind: "unknown", BindingName: "Unverified"}},
		{name: "wrong session", record: generationRecord{AgentHubAgentName: "Unrelated Agent"}, work: turnWork{GenerationID: "sender-gen", SessionID: "wrong-session"}},
		{name: "wrong generation", record: generationRecord{AgentHubAgentName: "Unrelated Agent"}, work: turnWork{GenerationID: "wrong-generation", SessionID: "sender-session"}},
		{name: "missing generation", record: generationRecord{AgentHubAgentName: "Current Agent"}, work: turnWork{SessionID: "sender-session"}},
		{name: "missing session", record: generationRecord{AgentHubAgentName: "Current Agent"}, work: turnWork{GenerationID: "sender-gen"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, workspace, _, _ := workflowFixture(t)
			resource := "project1.task1"
			record := test.record
			record.ID, record.GenerationID, record.Generation = "sender-gen", "sender-gen", 1
			record.ResourceID, record.WorkspaceID, record.AgentHubSessionID = resource, workspace.ID, "sender-session"
			if test.retired {
				saveRetiredGenerationForTest(t, workspace.Path, record, "sender_test")
				current := generationRecord{ID: "new-gen", GenerationID: "new-gen", Generation: 2, ResourceID: resource, WorkspaceID: workspace.ID, AgentHubSessionID: "new-session", AgentHubAgentName: "New Agent"}
				if err := saveGenerationRecord(workspace.Path, current); err != nil {
					t.Fatal(err)
				}
			} else if err := saveGenerationRecord(workspace.Path, record); err != nil {
				t.Fatal(err)
			}
			chat, err := loadDialogue(workspace.Path, dialoguePrefix+sharedChatID(resource))
			if err != nil {
				t.Fatal(err)
			}
			work := test.work
			if work.GenerationID == "" && work.SessionID == "" {
				work.GenerationID, work.SessionID = record.GenerationID, record.AgentHubSessionID
			}
			work.TurnID = "sender-turn"
			work.Context = providerMessageContext{ChatVisible: true, ChatEpoch: chat.Epoch}
			turn := agentHubTurn{Closed: true, Items: []agentHubTurnItem{{Type: "message", Role: "assistant", Text: "Original reply", StartEventID: 1}}}
			if err := syncTurnChat(workspace.Path, resource, &work, turn); err != nil {
				t.Fatal(err)
			}
			chat, err = loadDialogue(workspace.Path, chat.address())
			if err != nil || len(chat.Messages) != 1 || chat.Messages[0].Sender != test.want || chat.public().Messages[0].Sender != test.want {
				t.Fatalf("sender snapshot = %+v, want %q, err %v", chat.Messages, test.want, err)
			}
			if !test.retired {
				record.AgentHubAgentName = "Changed Agent"
				if err := saveGenerationRecord(workspace.Path, record); err != nil {
					t.Fatal(err)
				}
			}
			if err := syncTurnChat(workspace.Path, resource, &work, turn); err != nil {
				t.Fatal(err)
			}
			chat, err = loadDialogue(workspace.Path, chat.address())
			if err != nil || len(chat.Messages) != 1 || chat.Messages[0].Sender != test.want {
				t.Fatalf("retry changed sender: %+v, %v", chat.Messages, err)
			}
		})
	}
}

func TestChatSenderDoesNotBackfillOldMessagesOrUseUnrelatedResources(t *testing.T) {
	_, workspace, _, _ := workflowFixture(t)
	resource := "project1.task1"
	chat, err := loadDialogue(workspace.Path, dialoguePrefix+sharedChatID(resource))
	if err != nil {
		t.Fatal(err)
	}
	work := turnWork{GenerationID: "same-gen", SessionID: "same-session", TurnID: "turn", Context: providerMessageContext{ChatVisible: true, ChatEpoch: chat.Epoch}}
	oldID := "auto-" + chatID(work.SessionID, work.TurnID+":1")
	if err := chat.appendMessage(oldID, "agent", "Legacy reply"); err != nil {
		t.Fatal(err)
	}
	if err := saveChatLocked(workspace.Path, chat); err != nil {
		t.Fatal(err)
	}
	record := generationRecord{ID: "same-gen", GenerationID: "same-gen", Generation: 1, ResourceID: "project1", WorkspaceID: workspace.ID, AgentHubSessionID: "same-session", AgentHubAgentName: "Other Resource Agent"}
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	turn := agentHubTurn{Closed: true, Items: []agentHubTurnItem{
		{Type: "message", Role: "assistant", Text: "Legacy reply", StartEventID: 1},
		{Type: "message", Role: "assistant", Text: "Unverified reply", StartEventID: 2},
	}}
	if err := syncTurnChat(workspace.Path, resource, &work, turn); err != nil {
		t.Fatal(err)
	}
	record.ResourceID, record.AgentHubAgentName = resource, "Current Agent"
	if err := saveGenerationRecord(workspace.Path, record); err != nil {
		t.Fatal(err)
	}
	if err := syncTurnChat(workspace.Path, resource, &work, turn); err != nil {
		t.Fatal(err)
	}
	chat, err = loadDialogue(workspace.Path, chat.address())
	if err != nil || len(chat.Messages) != 2 {
		t.Fatalf("messages = %+v, %v", chat.Messages, err)
	}
	for _, message := range chat.Messages {
		if message.Sender != "" {
			t.Fatalf("retroactively attributed message: %+v", message)
		}
	}
}

func TestChatSenderFollowsExplicitAndInheritedBindings(t *testing.T) {
	for _, source := range []string{"explicit", "workspace", "project"} {
		for _, kind := range []string{"profile", "agent"} {
			t.Run(source+"/"+kind, func(t *testing.T) {
				manager, workspace, _, _ := workflowFixture(t)
				ws, err := app.OpenWorkspace(workspace.Path)
				if err != nil {
					t.Fatal(err)
				}
				binding := app.AgentBinding{Kind: kind, Name: "chosen"}
				if source == "workspace" {
					if _, err := ws.SetResourceAgentDefaults(app.ResourceAgentDefaults{Project: app.AgentBinding{Kind: "profile", Name: "default"}, Task: binding}); err != nil {
						t.Fatal(err)
					}
				}
				if source == "project" {
					if _, err := ws.SetProjectTaskDefault("project1", binding); err != nil {
						t.Fatal(err)
					}
				}
				input := app.CreateTaskInput{ProjectID: "project1", Title: "Sender inheritance"}
				if source == "explicit" {
					input.AgentBinding = binding
				}
				task, err := ws.CreateTask(input)
				if err != nil {
					t.Fatal(err)
				}
				resolved, err := manager.resolveResourceAgent(workspace, task.ID, config{AgentProfiles: []agentProfileRoute{{Key: "chosen", AgentName: "Routed Agent"}}})
				if err != nil {
					t.Fatal(err)
				}
				record := generationRecord{ID: "inherited-gen", GenerationID: "inherited-gen", Generation: 1, ResourceID: task.ID, WorkspaceID: workspace.ID, AgentHubSessionID: "inherited-session",
					AgentHubAgentName: resolved.AgentName, ResolvedProfile: resolved.ResolvedProfile, BindingKind: resolved.Binding.Kind, BindingName: resolved.Binding.Name}
				if err := saveGenerationRecord(workspace.Path, record); err != nil {
					t.Fatal(err)
				}
				if _, err := ws.SetResourceAgentBinding(task.ID, app.AgentBinding{Kind: "agent", Name: "Next Agent"}); err != nil {
					t.Fatal(err)
				}
				chat, err := ensureChatLocked(workspace.Path, task.ID, "reader")
				if err != nil {
					t.Fatal(err)
				}
				chat.open()
				if err := saveChatLocked(workspace.Path, chat); err != nil {
					t.Fatal(err)
				}
				work := turnWork{GenerationID: record.GenerationID, SessionID: record.AgentHubSessionID, TurnID: "inherited-turn", Context: providerMessageContext{ChatVisible: true, ChatEpoch: chat.Epoch}}
				turn := agentHubTurn{Closed: true, Items: []agentHubTurnItem{{Type: "message", Role: "assistant", Text: "Bound reply", StartEventID: 1}}}
				if err := syncTurnChat(workspace.Path, task.ID, &work, turn); err != nil {
					t.Fatal(err)
				}
				want := "chosen"
				if kind == "profile" {
					want = "Routed Agent"
				}
				chat, err = loadDialogue(workspace.Path, chat.address())
				if err != nil || len(chat.Messages) != 1 || chat.Messages[0].Sender != want {
					t.Fatalf("inherited sender = %+v, want %q, err %v", chat.Messages, want, err)
				}
			})
		}
	}
}
