package serve

import (
	"encoding/json"
	"testing"
)

func TestWorkspaceCardLifecycle(t *testing.T) {
	server, _ := attentionTestServer(t)
	const path = "/api/workspaces/workspace-one/cards"
	post := func(body any, want int) {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rec := attentionRequest(t, server, "POST", path, string(data))
		if rec.Code != want {
			t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
		}
	}
	key := "example-publisher-key-for-test-only"
	id := cardKeyID(key)
	post(cardChange{Action: "create", Key: key, Kind: "html", Source: "agent", Title: "Review", Content: &cardContent{HTML: "<p>Ready</p>"}}, 200)
	post(cardChange{Action: "update", Key: "wrong", ID: id, Version: 1, Content: &cardContent{}}, 403)
	post(cardChange{Action: "update", Key: key, ID: id, Version: 1, Content: &cardContent{HTML: "<p onclick='example'>bad</p>"}}, 400)
	post(cardChange{Action: "update", Key: key, ID: id, Version: 1, Content: &cardContent{HTML: "<table><tr><td>Done</td></tr></table>"}}, 200)
	post(cardChange{Action: "remove", ID: id, Version: 1}, 409)
	rec := attentionRequest(t, server, "GET", path, "")
	var cards []workspaceCard
	if err := json.Unmarshal(rec.Body.Bytes(), &cards); err != nil || len(cards) != 1 || cards[0].Version != 2 {
		t.Fatal(rec.Body.String())
	}
	post(cardChange{Action: "remove", ID: id, Version: 2}, 200)
	post(cardChange{Action: "update", Key: key, ID: id, Version: 2, Content: &cardContent{}}, 404)
}
