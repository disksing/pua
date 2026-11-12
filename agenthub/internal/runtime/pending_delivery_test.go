package runtime

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disksing/pua/agenthub/internal/provider"
	"github.com/disksing/pua/agenthub/internal/session"
)

func pendingTestSession(t *testing.T) (*session.Store, session.Session, *Manager) {
	t.Helper()
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.Create(session.CreateInput{Cwd: t.TempDir(), AgentName: "Fast Agent"})
	if err != nil {
		t.Fatal(err)
	}
	manager := New(store, testConfig())
	t.Cleanup(manager.Close)
	return store, value, manager
}

func pendingTestMessage(t *testing.T, store *session.Store, id, messageID string) session.DurableMessage {
	t.Helper()
	message, found, err := store.DurableMessageByID(id, messageID)
	if err != nil || !found {
		t.Fatalf("message = %+v, found=%v, err=%v", message, found, err)
	}
	return message
}

func waitPendingTerminal(t *testing.T, store *session.Store, id, messageID string) session.DurableMessage {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		message := pendingTestMessage(t, store, id, messageID)
		value, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if message.Terminal && message.State != session.MessageDeliveryClosed && value.CurrentTurnID == "" && (value.State == session.StateStopped || value.State == session.StateReady) {
			return message
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("input never settled: %+v", pendingTestMessage(t, store, id, messageID))
	return session.DurableMessage{}
}

func TestPendingNativeSessionMissingFailsTurnWithoutReplay(t *testing.T) {
	store, value, manager := pendingTestSession(t)
	var starts atomic.Int32
	manager.factory = func(options provider.Options) (provider.Session, error) {
		starts.Add(1)
		return &fakeSession{hooks: options.Hooks, startErr: fmt.Errorf("restore: %w", provider.ErrNativeSessionMissing)}, nil
	}
	input := session.MessageInput{Text: "tick", MessageID: "native-missing"}
	if _, err := manager.SendMessage(value.ID, input); err != nil {
		t.Fatal(err)
	}
	message := pendingTestMessage(t, store, value.ID, input.MessageID)
	if !message.Terminal || message.Delivered || message.State != session.MessageDeliveryFailed || message.StartupAttempts != 1 || message.Error == "" {
		t.Fatalf("missing native session was not terminal: %+v", message)
	}
	turn, err := store.Turn(value.ID, message.TurnID)
	if err != nil || !turn.Closed || turn.Status != "failed" {
		t.Fatalf("turn = %+v, err=%v", turn, err)
	}
	if _, err := manager.SendMessage(value.ID, input); err != nil {
		t.Fatal(err)
	}
	manager.deliverPendingLocked(value.ID)
	if starts.Load() != 1 {
		t.Fatalf("terminal input restarted provider %d times", starts.Load())
	}
	if _, err := store.Archive(value.ID); err != nil {
		t.Fatalf("failed generation cannot be archived: %v", err)
	}
}

func TestPendingStartupBudgetAndTransientRecovery(t *testing.T) {
	for _, succeeds := range []bool{false, true} {
		t.Run(fmt.Sprint(succeeds), func(t *testing.T) {
			store, value, manager := pendingTestSession(t)
			var starts atomic.Int32
			manager.factory = func(options provider.Options) (provider.Session, error) {
				attempt := starts.Add(1)
				adapter := &fakeSession{hooks: options.Hooks}
				if !succeeds || attempt < 3 {
					adapter.startErr = errors.New("temporary handshake failure")
				}
				return adapter, nil
			}
			input := session.MessageInput{Text: "tick", MessageID: "startup-budget"}
			if _, err := manager.SendMessage(value.ID, input); err != nil {
				t.Fatal(err)
			}
			first := pendingTestMessage(t, store, value.ID, input.MessageID)
			if first.Terminal || first.Attempt != 1 || first.StartupAttempts != 1 || first.RetryAt == "" {
				t.Fatalf("first failed startup = %+v", first)
			}
			// An explicit duplicate respects the same durable retry deadline.
			if _, err := manager.SendMessage(value.ID, input); err != nil {
				t.Fatal(err)
			}
			if starts.Load() != 1 {
				t.Fatal("duplicate input bypassed startup backoff")
			}
			message := waitPendingTerminal(t, store, value.ID, input.MessageID)
			if starts.Load() != 3 || message.Attempt != 3 || message.Delivered != succeeds {
				t.Fatalf("starts=%d, message=%+v", starts.Load(), message)
			}
			if !succeeds {
				if message.State != session.MessageDeliveryFailed || message.StartupAttempts != 3 {
					t.Fatalf("budget did not settle failure: %+v", message)
				}
				turn, err := store.Turn(value.ID, message.TurnID)
				if err != nil || !turn.Closed || turn.Status != "failed" {
					t.Fatalf("budget turn = %+v, err=%v", turn, err)
				}
			}
			if _, err := manager.SendMessage(value.ID, input); err != nil {
				t.Fatal(err)
			}
			if starts.Load() != 3 {
				t.Fatal("terminal input was replayed")
			}
		})
	}
}

func TestStopPendingInputSurvivesRestartAndStableReplay(t *testing.T) {
	store, value, manager := pendingTestSession(t)
	manager.factory = func(options provider.Options) (provider.Session, error) {
		return &fakeSession{hooks: options.Hooks, startErr: errors.New("temporary startup failure")}, nil
	}
	input := session.MessageInput{Text: "tick", MessageID: "stop-pending"}
	if _, err := manager.SendMessage(value.ID, input); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(value.ID); err != nil {
		t.Fatal(err)
	}
	message := pendingTestMessage(t, store, value.ID, input.MessageID)
	if !message.Terminal || message.Delivered || message.State != session.MessageDeliveryCancelled {
		t.Fatalf("stopped input = %+v", message)
	}
	manager.Close()
	store, err := session.Open(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	manager = New(store, testConfig())
	t.Cleanup(manager.Close)
	manager.factory = func(provider.Options) (provider.Session, error) {
		t.Error("cancelled input restarted provider")
		return nil, errors.New("unexpected startup")
	}
	if _, err := manager.SendMessage(value.ID, input); err != nil {
		t.Fatal(err)
	}
	manager.deliverPendingLocked(value.ID)
	turn, err := store.Turn(value.ID, message.TurnID)
	if err != nil || !turn.Closed || turn.Status != "cancelled" {
		t.Fatalf("stopped turn = %+v, err=%v", turn, err)
	}
	if _, err := store.Archive(value.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverLegacyPendingStopAndPersistedStartupBudget(t *testing.T) {
	for _, state := range []string{session.StateStopping, session.StateStopped} {
		for _, reason := range []string{session.StopReasonRequested, session.StopReasonStartupError} {
			t.Run(state+"/"+reason, func(t *testing.T) {
				store, value, initial := pendingTestSession(t)
				initial.Close()
				input, err := session.NormalizeMessageInput(session.MessageInput{Text: "tick", MessageID: "legacy-pending"})
				if err != nil {
					t.Fatal(err)
				}
				event, err := store.Append(value.ID, session.EventMessageInput, "turn-legacy", mustJSON(input))
				if err != nil {
					t.Fatal(err)
				}
				_, _ = store.Append(value.ID, "turn.started", "turn-legacy", nil)
				_, _ = store.Append(value.ID, session.EventMessageDelivery, "turn-legacy", mustJSON(session.MessageDeliveryEventData{MessageEventID: event.ID, State: session.MessageDeliveryPending, Attempt: 2, StartupAttempts: 2}))
				_, _ = store.Append(value.ID, "session.state", "", mustJSON(session.StateEventData{State: state, Reason: reason}))
				if state == session.StateStopping {
					_, _ = store.Append(value.ID, "session.state", "", mustJSON(session.StateEventData{State: state}))
				}
				store, err = session.Open(store.Root())
				if err != nil {
					t.Fatal(err)
				}
				manager := New(store, testConfig())
				t.Cleanup(manager.Close)
				var starts atomic.Int32
				manager.factory = func(options provider.Options) (provider.Session, error) {
					starts.Add(1)
					return &fakeSession{hooks: options.Hooks, startErr: errors.New("still unavailable")}, nil
				}
				if _, err := manager.SendMessage(value.ID, input); err != nil {
					t.Fatal(err)
				}
				message := pendingTestMessage(t, store, value.ID, input.MessageID)
				if !message.Terminal || message.Delivered {
					t.Fatalf("recovered input = %+v", message)
				}
				if reason == session.StopReasonRequested {
					if starts.Load() != 0 || message.State != session.MessageDeliveryCancelled {
						t.Fatalf("requested stop restarted: starts=%d, message=%+v", starts.Load(), message)
					}
				} else if starts.Load() != 1 || message.StartupAttempts != 3 || message.State != session.MessageDeliveryFailed {
					t.Fatalf("restart reset startup budget: starts=%d, message=%+v", starts.Load(), message)
				}
				if _, err := store.Archive(value.ID); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

type pendingBlockingStart struct {
	*fakeSession
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	startErr error
}

func (f *pendingBlockingStart) Start(string) error {
	close(f.entered)
	<-f.release
	return f.startErr
}
func (f *pendingBlockingStart) Close() error { f.once.Do(func() { close(f.release) }); return nil }

func TestStopDuringPendingStartupCancelsExactlyOnce(t *testing.T) {
	for _, startErr := range []error{nil, errors.New("startup interrupted")} {
		t.Run(fmt.Sprint(startErr), func(t *testing.T) {
			store, value, manager := pendingTestSession(t)
			adapter := &pendingBlockingStart{startErr: startErr, fakeSession: &fakeSession{}, entered: make(chan struct{}), release: make(chan struct{})}
			manager.factory = func(options provider.Options) (provider.Session, error) {
				adapter.hooks = options.Hooks
				return adapter, nil
			}
			input := session.MessageInput{Text: "tick", MessageID: "stop-starting"}
			sent := make(chan error, 1)
			go func() { _, err := manager.SendMessage(value.ID, input); sent <- err }()
			select {
			case <-adapter.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("startup never began")
			}
			if err := manager.Stop(value.ID); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-sent:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("send remained blocked")
			}
			message := pendingTestMessage(t, store, value.ID, input.MessageID)
			if !message.Terminal || message.Delivered || message.State != session.MessageDeliveryCancelled {
				t.Fatalf("startup-stop input = %+v", message)
			}
			events, err := store.EventsAfter(value.ID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			terminals := 0
			for _, event := range events {
				if event.Type == session.EventTurnCancelled {
					terminals++
				}
			}
			if terminals != 1 {
				t.Fatalf("stop produced %d terminals", terminals)
			}
			if _, err := manager.ensureInput(value.ID, message.EventID); err == nil {
				t.Fatal("a cancelled input passed the provider startup guard")
			}
			if _, err := store.Archive(value.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}
