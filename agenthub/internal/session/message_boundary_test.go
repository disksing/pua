package session

import "testing"

func TestAssistantCompletionSealsBlocksWithoutEndingTurn(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.Create(CreateInput{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	appendProjectionEvent(t, store, value.ID, "turn.started", "turn", nil)
	appendProjectionEvent(t, store, value.ID, "message.assistant.delta", "turn", map[string]string{"text": "Checking "})
	appendProjectionEvent(t, store, value.ID, "message.assistant.delta", "turn", map[string]string{"text": "now"})
	appendProjectionEvent(t, store, value.ID, "message.assistant.completed", "turn", nil)
	turn, err := store.Turn(value.ID, "turn")
	if err != nil || turn.Closed || len(turn.Items) != 2 || !turn.Items[1].Complete || turn.Items[1].Text != "Checking now" {
		t.Fatalf("progress boundary = %+v, %v", turn, err)
	}
	appendProjectionEvent(t, store, value.ID, "message.assistant.delta", "turn", map[string]string{"text": "Done"})
	appendProjectionEvent(t, store, value.ID, EventTurnCompleted, "turn", nil)
	turn, err = store.Turn(value.ID, "turn")
	if err != nil || len(turn.Items) != 4 || turn.Items[2].Text != "Done" {
		t.Fatalf("messages merged across boundary: %+v, %v", turn, err)
	}
}
