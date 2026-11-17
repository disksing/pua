package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/disksing/pua/agenthub/internal/semantic"
	"github.com/disksing/pua/agenthub/internal/session"
)

func TestAdvisoryExpectedTurnSurvivesPublicEventProjection(t *testing.T) {
	store, created := seedEventStore(t, 1)
	server := httptest.NewServer(New(store, "test", time.Now()).Handler())
	defer server.Close()
	for _, expectedTurn := range []string{"turn_origin", ""} {
		input := session.MessageInput{
			SchemaVersion: 2, MessageID: "advisory-" + expectedTurn, Text: "Chat changed",
			Steer: true, ExpectedTurnID: expectedTurn,
			Payload: json.RawMessage(`{"role":"system","schema":"pua.message.v1","text":"Chat changed"}`),
		}
		source, err := store.Append(created.ID, "message.input", "turn_origin", mustMarshal(t, input))
		if err != nil {
			t.Fatal(err)
		}
		page := getBackwardPage(t, server.URL, created.ID, fmt.Sprintf("after=%d", source.ID-1))
		if len(page.Frames) != 1 || len(page.Frames[0].Events) != 1 {
			t.Fatalf("missing canonical input: %+v", page)
		}
		response, err := http.Get(fmt.Sprintf("%s/v1/sessions/%s/event/%d", server.URL, created.ID, source.ID))
		if err != nil {
			t.Fatal(err)
		}
		var detail semantic.Detail
		decodeErr := json.NewDecoder(response.Body).Decode(&detail)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || decodeErr != nil {
			t.Fatalf("event detail: status=%d err=%v", response.StatusCode, decodeErr)
		}
		for _, data := range []any{page.Frames[0].Events[0].Data, detail.Frame.Events[0].Data, detail.SourceEvent.Data} {
			encoded := mustMarshal(t, data)
			var projected session.MessageInput
			if err := json.Unmarshal(encoded, &projected); err != nil {
				t.Fatal(err)
			}
			projected.Role = ""
			if !reflect.DeepEqual(projected, input) {
				t.Fatalf("public projection changed durable input: got=%+v want=%+v", projected, input)
			}
		}
	}
}
