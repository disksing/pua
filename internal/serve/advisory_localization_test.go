package serve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func setAdvisoryTestLanguage(t *testing.T, root, language string) {
	t.Helper()
	path := filepath.Join(root, "workspace.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	config["language"] = language
	data, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestOpenerShowsOnlyActionableQueueContext(t *testing.T) {
	for _, language := range []string{"en", "zh-CN"} {
		t.Run(language, func(t *testing.T) {
			context := &providerMessageContext{Protocol: 1, Language: language, Concise: true}
			message := resourceMailboxMessage{ProviderContext: context, Text: "body"}
			plain := providerMessageTextWithContext(message, false)
			context.Queue = &mailboxQueueSnapshot{}
			if providerMessageTextWithContext(message, false) != plain {
				t.Fatal("empty queue added instructions")
			}
			context.Concise = false
			if !strings.Contains(providerMessageTextWithContext(message, false), "Queue snapshot at") {
				t.Fatal("legacy retry lost its frozen presentation")
			}
			context.Concise = true
			context.Queue.WaitingTotal, context.Queue.BatchEligibleFromSource = 3, 1
			for _, visible := range []bool{true, false} {
				context.ChatVisible = visible
				text := providerMessageTextWithContext(message, false)
				if !strings.Contains(text, "3") || strings.Contains(text, "pua message list") == visible || strings.Contains(text, "waiting=") {
					t.Fatalf("wrong opener for visible=%v: %s", visible, text)
				}
			}
		})
	}
}

func TestWorkspaceAdvisoryGuidanceGateAndIdentity(t *testing.T) {
	for _, language := range []string{"en", "zh-CN"} {
		for _, guidance := range []string{"current", "stale", "missing", "broken", "unreadable"} {
			t.Run(language+"/"+guidance, func(t *testing.T) {
				root, err := app.Initialize(t.TempDir(), language)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root.Root(), "AGENTS.md")
				switch guidance {
				case "stale":
					err = os.WriteFile(path, []byte("Custom legacy guidance.\n"), 0644)
				case "missing":
					err = os.Remove(path)
				case "broken":
					err = os.WriteFile(path, []byte("<!-- managed by pua cli -->\n"), 0644)
				case "unreadable":
					if err = os.Remove(path); err == nil {
						err = os.Mkdir(path, 0755)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				before, _ := os.ReadFile(path)
				message := resourceMailboxMessage{
					ID: "chat-71800545c910e2efe3f4fd2faba371af", Type: "user_chat_input", Role: "agent",
					Sender: &agentHubMessageSender{Name: "disksing"}, Text: "Keep the original business body.\n\n",
				}
				message.Causation = &resourceMessageCausation{AutomaticChat: true}
				text, err := renderWorkspaceAdvisory(context.Background(), serveWorkspace{Path: root.Root()},
					queuePreviewRenderer(&turnWorkFile{}, &turnWork{ID: "active", State: "running"},
						agentHubSession{}, message, resourceMailbox{}))
				if err != nil {
					t.Fatal(err)
				}
				party := `user "disksing"`
				if language == "zh-CN" {
					party = `用户 "disksing"`
				}
				for _, fact := range []string{party, "pua turn switch --message=" + message.ID, "pua turn status"} {
					if !strings.Contains(text, fact) {
						t.Fatalf("missing fact %q in %q", fact, text)
					}
				}
				if !strings.HasSuffix(text, message.Text) {
					t.Fatalf("original body changed: %q", text)
				}
				if strings.Contains(text, "pua message read") {
					t.Fatalf("unexpected read hint for user input: %q", text)
				}
				after, _ := os.ReadFile(path)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("advisory mutated existing Workspace guidance")
				}
			})
		}
	}
}

func TestWorkspaceAdvisoryLanguageValidation(t *testing.T) {
	for _, language := range []string{"", "en_US", "en-us", "zh", "zh-Hans", "xx"} {
		t.Run(language, func(t *testing.T) {
			root, err := app.Initialize(t.TempDir(), "en")
			if err != nil {
				t.Fatal(err)
			}
			setAdvisoryTestLanguage(t, root.Root(), language)
			called := false
			text, err := renderWorkspaceAdvisory(context.Background(), serveWorkspace{Path: root.Root()}, func(language string, compact bool) (string, error) {
				called = true
				return language, nil
			})
			if language == "xx" {
				if err == nil || called {
					t.Fatal("invalid language silently fell back or rendered")
				}
				return
			}
			want := "en"
			if strings.HasPrefix(language, "zh") {
				want = "zh-CN"
			}
			if err != nil || text != want || !called {
				t.Fatalf("language=%q, err=%v", text, err)
			}
			if err := os.WriteFile(filepath.Join(root.Root(), "workspace.json"), []byte("{"), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := renderWorkspaceAdvisory(context.Background(), serveWorkspace{Path: root.Root()}, func(string, bool) (string, error) {
				t.Fatal("unreadable config rendered")
				return "", nil
			}); err == nil {
				t.Fatal("unreadable config silently fell back")
			}
		})
	}
}

func TestAdvisoryReservationKeepsFrozenInputAfterConfigChange(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	message := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	work := *workFile(t, workspace).find(message.ID)
	setAdvisoryProvider(fake, work.SessionID, "pi")
	formal := queueWorkflowUser(t, manager, workspace, "frozen-advisory", "Question")
	reconcileWorkflow(t, manager, workspace, client)
	file := workFile(t, workspace)
	if len(file.Advisories) != 1 || file.Advisories[0].Input == nil {
		t.Fatal("missing durable advisory input")
	}
	frozen := *file.Advisories[0].Input
	setAdvisoryTestLanguage(t, workspace.Path, "xx")
	fake.mu.Lock()
	session := fake.sessions[work.SessionID]
	fake.mu.Unlock()
	if err := manager.reserveAdvisory(context.Background(), workspace, "project1.task1", &file, session,
		client, work, file.Advisories[0].ID, "queue_preview", formal.ID, func(string, bool) (string, error) {
			t.Fatal("existing input was rendered again")
			return "", nil
		}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(frozen, *file.Advisories[0].Input) {
		t.Fatal("frozen input changed")
	}
}

func TestLocalizedChatAdvisoryAcrossProvidersAndGuidance(t *testing.T) {
	for _, provider := range []string{"codex", "pi"} {
		for _, language := range []string{"en", "zh-CN"} {
			for _, compact := range []bool{false, true} {
				name := provider + "/" + language + "/fallback"
				if compact {
					name = provider + "/" + language + "/compact"
				}
				t.Run(name, func(t *testing.T) {
					manager, workspace, fake, client := workflowFixture(t)
					message := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "Work", resourceMessageModeEnqueue, nil)
					work := *workFile(t, workspace).find(message.ID)
					setAdvisoryProvider(fake, work.SessionID, provider)
					setAdvisoryTestLanguage(t, workspace.Path, language)
					root, err := app.OpenWorkspace(workspace.Path)
					if err != nil {
						t.Fatal(err)
					}
					if compact {
						err = root.RefreshTurnGuidance("workspace")
					} else {
						err = os.WriteFile(filepath.Join(workspace.Path, "AGENTS.md"), []byte("Legacy guidance.\n"), 0644)
					}
					if err != nil {
						t.Fatal(err)
					}
					mustChatOK(t, chatCall(t, manager, workspace, "project1.task1", "reader", "close", chatRequest{}, false))
					reconcileWorkflow(t, manager, workspace, client)
					file := workFile(t, workspace)
					if len(file.Advisories) != 1 {
						t.Fatalf("missing Chat notice: %+v", file.Advisories)
					}
					advisory := file.Advisories[0]
					state, audience := "Chat closed", "original audience"
					if language == "zh-CN" {
						state, audience = "Chat 已关闭", "原受众"
					}
					for _, fact := range []string{state, audience, "project1", "pua turn status"} {
						if !strings.Contains(advisory.Input.Text, fact) {
							t.Fatalf("missing %q in %q", fact, advisory.Input.Text)
						}
					}
					if strings.Contains(advisory.Input.Text, "currentControl") == compact ||
						(advisory.Input.ExpectedTurnID != "") != (provider == "codex") {
						t.Fatalf("incorrect guidance/provider path: %+v", advisory)
					}
					payload, ok := decodePUAMessagePayload(advisory.Input.Payload)
					if !ok || payload.Text != advisory.Input.Text || payload.Type != "chat_state" {
						t.Fatal("localized wire and payload diverged")
					}
					if file.find(work.ID).Context.ChatVisible != work.Context.ChatVisible ||
						file.find(work.ID).Context.ChatEpoch != work.Context.ChatEpoch {
						t.Fatal("notice changed frozen audience")
					}
				})
			}
		}
	}
}

func TestLegacyQueuePreviewRetriesFrozenWire(t *testing.T) {
	manager, workspace, fake, client := workflowFixture(t)
	original := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "background", resourceMessageModeEnqueue, nil)
	message := queueWorkflowUser(t, manager, workspace, "legacy-frozen", "Original user body")
	file := workFile(t, workspace)
	work := file.find(original.ID)
	payload, err := marshalPUAMessagePayload(puaMessagePayload{
		Schema: puaMessagePayloadSchema, Role: "system", Type: "queue_preview",
		Text: "Legacy self-contained English notification.\nOriginal user body",
	})
	if err != nil {
		t.Fatal(err)
	}
	frozen := agentHubInboundMessage{
		SchemaVersion: 2, Steer: true, ExpectedTurnID: work.TurnID,
		MessageID: "preview-" + chatID(work.ID, message.ID),
		Text:      "Legacy self-contained English notification.\nOriginal user body", Payload: payload,
	}
	work.PreviewAttempts = map[string]agentHubInboundMessage{message.ID: frozen}
	if err := saveTurnWork(workspace.Path, "project1.task1", file); err != nil {
		t.Fatal(err)
	}
	setAdvisoryTestLanguage(t, workspace.Path, "xx")
	fake.mu.Lock()
	session := fake.sessions[work.SessionID]
	fake.mu.Unlock()
	message.Text = "Changed user body"
	if err := manager.previewQueuedInput(context.Background(), workspace, session, client, message); err != nil {
		t.Fatal(err)
	}
	inputs := previewInputs(fake)
	if !reflect.DeepEqual(inputs[frozen.MessageID], frozen) {
		t.Fatal("legacy frozen wire changed or language was re-read")
	}
}
