package serve

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestHistoryAndQueueResumeSource(t *testing.T) {
	const resource = "project8.task2"
	agent := &agentHubMessageSender{ID: "project4.task9", Name: "Reviewer"}
	user := &agentHubMessageSender{ID: "user-3", Name: "Tess"}
	tests := []struct {
		name      string
		opener    resourceMailboxMessage
		context   *providerMessageContext
		compacted bool
		want      turnResumeSource
	}{
		{
			name:    "agent with legacy context and parent receipt",
			opener:  resourceMailboxMessage{ID: "original", Role: "agent", Sender: agent, Type: "review_request"},
			context: &providerMessageContext{OpenerRole: "agent", OpenerSender: agent},
			want:    turnResumeSource{Role: "agent", Type: "review_request", Sender: agent},
		},
		{
			name:      "direct user with compacted receipts after repeated resume",
			context:   &providerMessageContext{OpenerRole: "user", OpenerType: "user_chat_input", OpenerSender: user},
			compacted: true,
			want:      turnResumeSource{Role: "user", Type: "user_chat_input", Sender: user},
		},
		{
			name:      "notification type survives compacted receipts",
			context:   &providerMessageContext{OpenerRole: "system", OpenerType: "chat_notification"},
			compacted: true,
			want:      turnResumeSource{Role: "system", Type: "chat_notification"},
		},
		{
			name: "legacy scheduler type from parent receipt",
			opener: resourceMailboxMessage{ID: "original", Role: "system", Type: "schedule_exact_due",
				Causation: &resourceMessageCausation{SourceResourceID: "scheduler"}},
			context: &providerMessageContext{OpenerRole: "system"},
			want:    turnResumeSource{Role: "system", Type: "schedule_exact_due", SourceResourceID: "scheduler"},
		},
		{
			name:      "known role and sender from compacted legacy work",
			context:   &providerMessageContext{OpenerRole: "agent", OpenerSender: agent},
			compacted: true,
			want:      turnResumeSource{Role: "agent", Sender: agent},
		},
		{
			name: "unknown legacy source",
			want: turnResumeSource{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resume := resourceMailboxMessage{ID: "second-resume", ResourceID: resource, Status: resourceMessageQueued,
				Role: "system", Type: "turn_resume", Text: "Resume formal work", ProviderContext: tt.context,
				Causation: &resourceMessageCausation{Type: "turn_resume", SourceResourceID: resource, MessageID: "first-resume", TurnReference: "first-history"}}
			first := resourceMailboxMessage{ID: "first-resume", Role: "system", Type: "turn_resume", ProviderContext: tt.context,
				Causation: &resourceMessageCausation{Type: "turn_resume", SourceResourceID: resource, MessageID: "original"}}
			mailbox := resourceMailbox{Messages: []resourceMailboxMessage{resume, first, tt.opener}}
			if tt.compacted {
				mailbox.Messages = mailbox.Messages[:1]
			}
			waiting := waitingMailboxMessages(mailbox, t.TempDir(), resource)
			if len(waiting) != 1 || waiting[0].Role != "system" || waiting[0].Type != "turn_resume" ||
				!reflect.DeepEqual(waiting[0].ResumeSource, &tt.want) {
				t.Fatalf("queue = %+v, want resume source %+v", waiting, tt.want)
			}

			wire, err := agentHubMailboxMessage(resume)
			if err != nil {
				t.Fatal(err)
			}
			if tt.context != nil && tt.context.OpenerType != "" {
				legacy := resume
				legacy.ProviderContext = cloneProviderMessageContext(resume.ProviderContext)
				legacy.ProviderContext.OpenerType = ""
				if !canonicalAgentHubMessageMatches(wire, legacy) {
					t.Fatal("display source changed canonical delivery matching")
				}
			}
			works := turnWorkFile{}
			if tt.compacted {
				works.Works = []turnWork{{ID: resume.ID, Context: *tt.context}}
				mailbox.Messages = nil
			} else {
				mailbox.Messages[0].GenerationID, mailbox.Messages[0].TurnID = "gen", "turn"
			}
			summary, err := historyTurnSummary("ws", resource, generationRecord{GenerationID: "gen"}, agentHubTurn{
				ID: "turn", TriggerPayload: wire.Payload, TriggerMessageID: wire.MessageID,
			}, mailbox, works)
			if err != nil {
				t.Fatal(err)
			}
			if summary.TriggerRole != "system" || summary.TriggerType != "turn_resume" || summary.TriggerMessageID != resume.ID ||
				summary.TriggerPreview != resume.Text || !reflect.DeepEqual(summary.ResumeSource, &tt.want) {
				t.Fatalf("history = %+v, want resume source %+v", summary, tt.want)
			}
			if !tt.compacted && (summary.TriggerDelivery == nil || summary.TriggerDelivery.Role != "system" ||
				summary.TriggerDelivery.Causation.MessageID != "first-resume") {
				t.Fatalf("formal trigger delivery changed: %+v", summary.TriggerDelivery)
			}
		})
	}
}

func TestResumeSourceUsesScopedParentEvidence(t *testing.T) {
	const resource = "project8.task2"
	first := turnWork{ID: "first-resume", ResumeID: "second-resume", History: "first-history"}
	works := turnWorkFile{Works: []turnWork{{ID: "original", ResumeID: first.ID, Context: providerMessageContext{
		OpenerRole: "system", OpenerType: "chat_notification",
	}}, first}}
	for _, tt := range []struct {
		resource string
		history  string
		wantType string
	}{
		{resource, first.History, "chat_notification"},
		{resource, "unrelated-history", ""},
		{"project8.task3", first.History, ""},
	} {
		raw, err := json.Marshal(puaMessagePayload{Schema: puaMessagePayloadSchema, Role: "system", Type: "turn_resume",
			Causation: &resourceMessageCausation{Type: "turn_resume", SourceResourceID: tt.resource, MessageID: first.ID, TurnReference: tt.history}})
		if err != nil {
			t.Fatal(err)
		}
		summary, err := historyTurnSummary("ws", resource, generationRecord{GenerationID: "gen"},
			agentHubTurn{ID: "turn", TriggerMessageID: first.ResumeID, TriggerPayload: raw}, resourceMailbox{}, works)
		if err != nil || summary.ResumeSource == nil || summary.ResumeSource.Type != tt.wantType {
			t.Fatalf("scope %+v: source = %+v, err = %v", tt, summary.ResumeSource, err)
		}
	}
}
