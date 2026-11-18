package provider

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Model Pi's important distinction: bare steer only queues, while prompt
// starts when idle and uses streamingBehavior when a run is still active.
func runPiSteerBoundaryHelper(scanner *bufio.Scanner) {
	active := false
	for scanner.Scan() {
		var request struct {
			ID                json.RawMessage `json:"id"`
			Type              string          `json:"type"`
			Message           string          `json:"message"`
			StreamingBehavior string          `json:"streamingBehavior"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		disposition := ""
		switch request.Type {
		case "get_state":
			fmt.Printf(`{"id":%s,"type":"response","command":"get_state","success":true,"data":{"sessionId":"boundary","isStreaming":%t}}`+"\n", request.ID, active)
			continue
		case "steer":
			disposition = "queued-only"
		case "prompt":
			if active {
				if request.StreamingBehavior != "steer" {
					fmt.Printf(`{"id":%s,"type":"response","command":"prompt","success":false,"error":"already streaming"}`+"\n", request.ID)
					continue
				}
				disposition = "steered"
			} else {
				active = true
				disposition = "started"
			}
		}
		fmt.Printf(`{"id":%s,"type":"response","command":%q,"success":true,"data":{}}`+"\n", request.ID, request.Type)
		if disposition != "" {
			fmt.Printf(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":%q}}`+"\n", disposition+":"+request.Message)
		}
		if request.Message == "finish" {
			active = false
		}
		if request.Message == "finish" || request.Message == "boundary-preview" {
			fmt.Println(`{"type":"agent_settled"}`)
		}
	}
}

func TestPiSteerStartsIdleRunAndSteersActiveRun(t *testing.T) {
	events := make(chan Event, 16)
	value := newPi(helperCLI(t, "pi-steer-boundary"), Options{
		Cwd:   t.TempDir(),
		Hooks: Hooks{Event: func(event Event) { events <- event }},
	})
	if err := value.Start(""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = value.Close() })
	for _, step := range []struct {
		text, want string
		steer      bool
	}{
		{"first", "started:first", true},
		{"boundary-preview", "steered:boundary-preview", true},
		{"finish", "steered:finish", true},
		{"after-settled", "started:after-settled", true},
		{"finish", "steered:finish", true},
		{"ordinary", "started:ordinary", false},
	} {
		if !step.steer {
			if err := value.CheckNewTurnReady(); err != nil {
				t.Fatalf("Pi did not become ready for the formal input: %v", err)
			}
		}
		if err := value.Prompt(step.text, step.steer); err != nil {
			t.Fatal(err)
		}
		select {
		case event := <-events:
			data, _ := json.Marshal(event.Data)
			var output struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(data, &output)
			if event.Type != "message.assistant.delta" || output.Text != step.want {
				t.Fatalf("%s: event=%+v data=%s, want %q", step.text, event, data, step.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s was not consumed", step.text)
		}
		if step.text == "finish" || step.text == "boundary-preview" {
			select {
			case event := <-events:
				if !event.TurnDone || event.TurnFailed {
					t.Fatalf("settled event = %+v", event)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("run did not settle")
			}
			value.prompts.pending.Wait()
			if step.text == "boundary-preview" {
				if err := value.CheckNewTurnReady(); !errors.Is(err, ErrProviderBusy) {
					t.Fatalf("old Turn settled while Pi was busy: %v", err)
				}
			}
		}
	}
}
