package serve

import (
	"reflect"
	"testing"
)

func TestProviderMessageTextMatchesLegacyAgentHubFormat(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		role   string
		sender *agentHubMessageSender
		steer  bool
		want   string
	}{
		{name: "plain user", text: "hello", role: "user", want: "hello"},
		{name: "named agent", text: "review", role: "agent", sender: &agentHubMessageSender{Name: "Review Agent"}, want: "Message from agent \"Review Agent\":\nreview"},
		{name: "steer", text: "urgent", role: "agent", sender: &agentHubMessageSender{ID: "project1.task2"}, steer: true, want: "Message from agent \"project1.task2\" (steer):\nurgent"},
		{name: "escaped sender", text: "body", role: "system", sender: &agentHubMessageSender{Name: "line\n\"quoted\""}, want: "Message from system \"line\\n\\\"quoted\\\"\":\nbody"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := providerMessageText(test.text, test.role, test.sender, test.steer); got != test.want {
				t.Fatalf("provider text = %q, want %q", got, test.want)
			}
		})
	}
}

func TestProviderMessageTextDescribesLocalizedResponseAndInsertedConversation(t *testing.T) {
	tests := []struct {
		name    string
		message resourceMailboxMessage
		steer   bool
		want    string
	}{
		{
			name: "English user opener",
			message: resourceMailboxMessage{Text: "start", Role: "user", Sender: &agentHubMessageSender{Name: "disksing"},
				ProviderContext: &providerMessageContext{Language: "en", OpenerRole: "user", OpenerSender: &agentHubMessageSender{Name: "disksing"}, OpenerResponse: providerResponseProgressFinal}},
			want: "Message from user \"disksing\" [sender receives: progress + final reply]:\nstart",
		},
		{
			name: "English subscribed Agent opener",
			message: resourceMailboxMessage{Text: "coordinate", Role: "agent", Sender: &agentHubMessageSender{ID: "project1.task347"},
				ProviderContext: &providerMessageContext{Language: "en", OpenerRole: "agent", OpenerSender: &agentHubMessageSender{ID: "project1.task347"}, OpenerResponse: providerResponseFinalOnly}},
			want: "Message from agent \"project1.task347\" [sender receives: final reply only]:\ncoordinate",
		},
		{
			name: "English unsubscribed Agent opener",
			message: resourceMailboxMessage{Text: "notify", Role: "agent", Sender: &agentHubMessageSender{ID: "project1.task347"},
				ProviderContext: &providerMessageContext{Language: "en", OpenerRole: "agent", OpenerSender: &agentHubMessageSender{ID: "project1.task347"}, OpenerResponse: providerResponseNone}},
			want: "Message from agent \"project1.task347\" [sender receives: no progress or final reply]:\nnotify",
		},
		{
			name: "Chinese system opener",
			message: resourceMailboxMessage{Text: "执行调度", Role: "system", Sender: &agentHubMessageSender{Name: "PUA Scheduler"},
				ProviderContext: &providerMessageContext{Language: "zh-CN", OpenerRole: "system", OpenerSender: &agentHubMessageSender{Name: "PUA Scheduler"}, OpenerResponse: providerResponseNone}},
			want: "来自系统 \"PUA Scheduler\" 的消息 [发送方接收：无]：\n执行调度",
		},
		{
			name: "same Agent insertion",
			message: resourceMailboxMessage{Text: "more detail", Role: "agent", Sender: &agentHubMessageSender{ID: "project1.task347"},
				ProviderContext: &providerMessageContext{Language: "en", OpenerRole: "agent", OpenerSender: &agentHubMessageSender{ID: "project1.task347"}, OpenerResponse: providerResponseFinalOnly}},
			steer: true,
			want:  "Inserted message from agent \"project1.task347\":\nmore detail",
		},
		{
			name: "different Agent insertion into user conversation",
			message: resourceMailboxMessage{Text: "check this", Role: "agent", Sender: &agentHubMessageSender{ID: "project1.task347"},
				ProviderContext: &providerMessageContext{Language: "en", OpenerRole: "user", OpenerSender: &agentHubMessageSender{Name: "disksing"}, OpenerResponse: providerResponseProgressFinal}},
			steer: true,
			want:  "Inserted message from agent \"project1.task347\" (Reply via `pua message send --to=project1.task347 '<reply>'`. Current conversation: user \"disksing\" [sender receives: progress + final reply]):\ncheck this",
		},
		{
			name: "Chinese different Agent insertion",
			message: resourceMailboxMessage{Text: "请核验", Role: "agent", Sender: &agentHubMessageSender{ID: "project1.task347"},
				ProviderContext: &providerMessageContext{Language: "zh-CN", OpenerRole: "user", OpenerSender: &agentHubMessageSender{Name: "disksing"}, OpenerResponse: providerResponseProgressFinal}},
			steer: true,
			want:  "来自 Agent \"project1.task347\" 的插入消息（回复请使用 `pua message send --to=project1.task347 '<reply>'`。当前会话：用户 \"disksing\" [发送方接收：进度 + 最终回复]）：\n请核验",
		},
		{
			name: "unsafe inserted sender omits reply command",
			message: resourceMailboxMessage{Text: "note", Role: "agent", Sender: &agentHubMessageSender{Name: "Display only"},
				ProviderContext: &providerMessageContext{Language: "en", OpenerRole: "user", OpenerSender: &agentHubMessageSender{Name: "disksing"}, OpenerResponse: providerResponseProgressFinal}},
			steer: true,
			want:  "Inserted message from agent \"Display only\" (Current conversation: user \"disksing\" [sender receives: progress + final reply]):\nnote",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := providerMessageTextWithContext(test.message, test.steer); got != test.want {
				t.Fatalf("provider text = %q, want %q", got, test.want)
			}
		})
	}
}

func TestProviderOpenerResponseUsesRoleAndEffectiveSubscription(t *testing.T) {
	sender := &agentHubMessageSender{ID: "project1.task2"}
	tests := []struct {
		name    string
		role    string
		message *resourceMailboxMessage
		want    string
	}{
		{name: "user", role: "user", want: providerResponseProgressFinal},
		{name: "system", role: "system", want: providerResponseNone},
		{name: "subscribed Agent", role: "agent", message: &resourceMailboxMessage{Role: "agent", Sender: sender, SenderWorkspaceInstanceID: "instance-1", SubscribeResult: true}, want: providerResponseFinalOnly},
		{name: "disabled Agent", role: "agent", message: &resourceMailboxMessage{Role: "agent", Sender: sender, SenderWorkspaceInstanceID: "instance-1", SubscribeResult: true, ResultSubscriptionStatus: resourceResultSubscriptionDisabled}, want: providerResponseNone},
		{name: "unsubscribed Agent", role: "agent", message: &resourceMailboxMessage{Role: "agent", Sender: sender, SenderWorkspaceInstanceID: "instance-1"}, want: providerResponseNone},
		{name: "invalid Agent sender", role: "agent", message: &resourceMailboxMessage{Role: "agent", Sender: &agentHubMessageSender{Name: "display"}, SenderWorkspaceInstanceID: "instance-1", SubscribeResult: true}, want: providerResponseNone},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := providerOpenerResponse(test.role, test.message); got != test.want {
				t.Fatalf("response = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAgentHubMailboxMessageOwnsPayloadAndProviderPrompt(t *testing.T) {
	message := resourceMailboxMessage{
		ID: "msg-1", Text: "inspect", Role: "agent", Type: "resource.message",
		Sender:                    &agentHubMessageSender{ID: "project1.task2", Name: "Worker"},
		SenderWorkspaceInstanceID: "workspace-1", ActualMode: resourceMessageModeSteer,
		Causation: &resourceMessageCausation{Type: "task", SourceResourceID: "project1.task2", MessageID: "cause-1"},
	}
	wire, err := agentHubMailboxMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := decodePUAMessagePayload(wire.Payload)
	if !ok {
		t.Fatalf("payload did not decode: %s", wire.Payload)
	}
	if wire.SchemaVersion != agentHubOpaqueMessageSchema || wire.MessageID != message.ID || !wire.Steer ||
		wire.Text != "Message from agent \"Worker\" (steer):\ninspect" {
		t.Fatalf("wire message = %+v", wire)
	}
	if payload.Text != message.Text || payload.Role != message.Role || payload.SenderWorkspaceInstanceID != "workspace-1" ||
		!reflect.DeepEqual(payload.Sender, message.Sender) || !reflect.DeepEqual(payload.Causation, message.Causation) {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestCanonicalAgentHubMessageMatchesV2AndLegacyData(t *testing.T) {
	expected := resourceMailboxMessage{
		ID: "msg-1", Text: "inspect", Role: "agent", Sender: &agentHubMessageSender{Name: "Worker"},
		ActualMode: resourceMessageModeEnqueue,
	}
	v2, err := agentHubMailboxMessage(expected)
	if err != nil {
		t.Fatal(err)
	}
	legacy := agentHubInboundMessage{Text: expected.Text, Role: expected.Role, Sender: expected.Sender, MessageID: expected.ID}
	if !canonicalAgentHubMessageMatches(v2, expected) || !canonicalAgentHubMessageMatches(legacy, expected) {
		t.Fatal("equivalent v2 and legacy canonical inputs must both match")
	}
	v2.Text = "changed"
	if canonicalAgentHubMessageMatches(v2, expected) {
		t.Fatal("changed provider text matched canonical input")
	}
}

func TestCanonicalAgentHubMessageAcceptsPreUpgradeProviderEnvelope(t *testing.T) {
	expected := resourceMailboxMessage{
		ID: "msg-1", Text: "inspect", Role: "agent", Sender: &agentHubMessageSender{ID: "project1.task2"},
		SenderWorkspaceInstanceID: "instance-1", SubscribeResult: true, ActualMode: resourceMessageModeSteer,
		ProviderContext: &providerMessageContext{Language: "en", TurnID: "turn-1", OpenerRole: "user",
			OpenerSender: &agentHubMessageSender{Name: "disksing"}, OpenerResponse: providerResponseProgressFinal},
	}
	canonical, err := agentHubMailboxMessage(expected)
	if err != nil {
		t.Fatal(err)
	}
	canonical.Text = providerMessageText(expected.Text, expected.Role, expected.Sender, true)
	if !canonicalAgentHubMessageMatches(canonical, expected) {
		t.Fatalf("pre-upgrade provider envelope did not match: %#v", canonical)
	}
	canonical.Text = "different"
	if canonicalAgentHubMessageMatches(canonical, expected) {
		t.Fatal("different provider envelope matched")
	}
}

func TestPUAMessagePresentationDecodesV2AndKeepsLegacy(t *testing.T) {
	payload, err := marshalPUAMessagePayload(puaMessagePayload{
		Schema: puaMessagePayloadSchema, Text: "original", Role: "agent", Sender: &agentHubMessageSender{Name: "Worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text, role, sender := puaMessagePresentation("provider prompt", "", nil, payload)
	if text != "original" || role != "agent" || sender == nil || sender.Name != "Worker" {
		t.Fatalf("v2 presentation = %q %q %+v", text, role, sender)
	}
	legacySender := &agentHubMessageSender{Name: "Old Client"}
	text, role, sender = puaMessagePresentation("legacy", "system", legacySender, nil)
	if text != "legacy" || role != "system" || sender != legacySender {
		t.Fatalf("legacy presentation = %q %q %+v", text, role, sender)
	}
}
