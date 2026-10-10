package session

import "testing"

func TestDurableDeliveryTerminalsSurviveProjectionRebuild(t *testing.T) {
	for _, terminal := range []string{MessageDeliveryFailed, MessageDeliveryCancelled} {
		t.Run(terminal, func(t *testing.T) {
			root := t.TempDir()
			store, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			value, err := store.Create(CreateInput{Cwd: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			input := appendProjectionEvent(t, store, value.ID, EventMessageInput, "turn-delivery", MessageInput{Text: "tick", MessageID: "tick"})
			appendProjectionEvent(t, store, value.ID, EventMessageDelivery, "turn-delivery", MessageDeliveryEventData{MessageEventID: input.ID, State: terminal, Attempt: 3, StartupAttempts: 3, Error: "startup failed"})
			// A late retry must not undo a confirmed failure/cancellation.
			appendProjectionEvent(t, store, value.ID, EventMessageDelivery, "turn-delivery", MessageDeliveryEventData{MessageEventID: input.ID, State: MessageDeliveryPending, Attempt: 4})
			reopened, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			message, found, err := reopened.DurableMessageByID(value.ID, "tick")
			if err != nil || !found || !message.Terminal || message.Delivered || message.State != terminal || message.Attempt != 3 || message.StartupAttempts != 3 || message.Error != "startup failed" {
				t.Fatalf("rebuilt delivery=%+v, found=%v, err=%v", message, found, err)
			}
		})
	}
}

func TestClosedLegacyTurnDoesNotReplayUnconfirmedInputOrInventAcceptance(t *testing.T) {
	for _, terminal := range []string{EventTurnCompleted, EventTurnFailed, EventTurnCancelled} {
		t.Run(terminal, func(t *testing.T) {
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			value, err := store.Create(CreateInput{Cwd: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			appendProjectionEvent(t, store, value.ID, EventMessageInput, "turn-legacy", MessageInput{Text: "steer", MessageID: "steer", Steer: true})
			appendProjectionEvent(t, store, value.ID, terminal, "turn-legacy", TurnTerminalEventData{Error: "terminal failure"})
			message, found, err := store.DurableMessageByID(value.ID, "steer")
			if err != nil || !found || !message.Terminal || message.Delivered {
				t.Fatalf("legacy terminal delivery=%+v, found=%v, err=%v", message, found, err)
			}
		})
	}
}
