package session

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func BenchmarkActiveTurnProjection(b *testing.B) {
	for _, rebuild := range []bool{true, false} {
		b.Run(map[bool]string{true: "forcedRebuild", false: "sameRevision"}[rebuild], func(b *testing.B) {
			store, err := Open(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			created, err := store.Create(CreateInput{})
			if err != nil {
				b.Fatal(err)
			}
			state := store.sessions[created.ID]
			// Populate an owned in-memory history fixture without thousands of
			// unrelated durable writes in the benchmark setup.
			for index := 0; index < 1000; index++ {
				turnID := fmt.Sprintf("turn_%d", index)
				for _, eventType := range []string{"turn.started", "message.assistant.delta", EventTurnCompleted} {
					data, _ := json.Marshal(map[string]string{"text": "a synthetic assistant reply"})
					state.events = append(state.events, Event{ID: int64(len(state.events) + 1), SessionID: created.ID, TurnID: turnID, Type: eventType, Time: time.Unix(int64(len(state.events)), 0), Data: data})
				}
			}
			state.session.CurrentTurnID = "turn_active"
			state.events = append(state.events, Event{ID: int64(len(state.events) + 1), SessionID: created.ID, TurnID: "turn_active", Type: "turn.started", Time: time.Now()})
			state.session.LastEventID = int64(len(state.events))
			state.revision++
			if _, err := store.Turn(created.ID, "turn_active"); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				if rebuild {
					store.turnCache.remove(created.ID)
					state.turnProjectionFile = nil
				}
				if _, err := store.Turn(created.ID, "turn_active"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
