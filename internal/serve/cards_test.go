package serve

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/disksing/pua/internal/app"
)

func TestMagpieQuotaFilterScope(t *testing.T) {
	server, root := attentionTestServer(t)
	workspace, err := app.OpenWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.RegisterUser("second"); err != nil {
		t.Fatal(err)
	}
	_, otherRoot := attentionTestServer(t)
	cfg, err := server.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspaces = append(cfg.Workspaces, serveWorkspace{ID: "workspace-two", Path: otherRoot})
	if err := server.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	request := func(workspace, user, method, body string) []byte {
		t.Helper()
		req := httptest.NewRequest(method, "/api/workspaces/"+workspace+"/cards", strings.NewReader(body))
		req.Header.Set(workspaceUserHeader, user)
		rec := httptest.NewRecorder()
		server.handleWorkspace(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s %s %s: %d %s", workspace, user, method, rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}
	const user = app.LegacyDefaultUserName
	const create = `{"action":"create","kind":"quota","source":"magpie-quota","title":"Usage"}`
	var created struct{ ID string }
	if err := json.Unmarshal(request("workspace-one", user, "POST", create), &created); err != nil {
		t.Fatal(err)
	}
	request("workspace-one", user, "POST", create)
	request("workspace-one", "second", "POST", create)
	request("workspace-two", user, "POST", create)
	request("workspace-one", user, "POST", fmt.Sprintf(`{"action":"layout","id":%q,"version":1,"hiddenQuotaKeys":["weekly"]}`, created.ID))
	for _, scope := range []struct{ workspace, user string }{{"workspace-one", user}, {"workspace-one", "second"}, {"workspace-two", user}} {
		var cards []workspaceCard
		if err := json.Unmarshal(request(scope.workspace, scope.user, "GET", ""), &cards); err != nil || len(cards) == 0 {
			t.Fatalf("%v: %v %v", scope, cards, err)
		}
		ownsCard := scope.workspace == "workspace-one" && scope.user == user
		if slices.ContainsFunc(cards, func(card workspaceCard) bool { return card.ID == created.ID }) != ownsCard {
			t.Fatalf("wrong card layout for %v: %v", scope, cards)
		}
		for _, card := range cards {
			var want []string
			if card.ID == created.ID {
				want = []string{"weekly"}
			}
			if !slices.Equal(card.HiddenQuotaKeys, want) {
				t.Fatalf("%v card %q: got %v, want %v", scope, card.ID, card.HiddenQuotaKeys, want)
			}
		}
	}
	result := request("workspace-one", user, "POST", fmt.Sprintf(`{"action":"layout","id":%q,"version":2,"hiddenQuotaKeys":[]}`, created.ID))
	if strings.Contains(string(result), "weekly") {
		t.Fatal(string(result))
	}
}

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

func TestMagpieQuotaCard(t *testing.T) {
	server, root := attentionTestServer(t)
	server.agents = newAgentManager(server)
	const cards = "/api/workspaces/workspace-one/cards"
	const quota = cards + "/magpie-quota"
	read := func(want int) string {
		t.Helper()
		rec := attentionRequest(t, server, "GET", quota, "")
		if rec.Code != want {
			t.Fatalf("quota returned %d: %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	read(404)
	for _, tc := range []struct {
		source string
		status int
	}{{"agent", 400}, {"magpie-quota", 200}} {
		rec := attentionRequest(t, server, "POST", cards, `{"action":"create","title":"Usage","kind":"quota","source":"`+tc.source+`"}`)
		if rec.Code != tc.status {
			t.Fatalf("create returned %d: %s", rec.Code, rec.Body.String())
		}
	}
	workspace, err := app.OpenWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.RegisterUser("second"); err != nil {
		t.Fatal(err)
	}
	for user, code := range map[string]int{"second": 404, "": 403} {
		request := httptest.NewRequest("GET", quota, nil)
		request.Header.Set(workspaceUserHeader, user)
		rec := httptest.NewRecorder()
		server.handleWorkspace(rec, request)
		if rec.Code != code {
			t.Fatalf("user %q returned %d", user, rec.Code)
		}
	}

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/quota" || r.Header.Get(workspaceUserHeader) != "" {
			t.Errorf("unexpected AgentHub request: %s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"quota":{"configured":true,"connected":false,"stale":true,"error":"private","providers":[{"id":"account","label":"Provider","accountLabel":"test","secret":"private","quotas":[{"key":"weekly","label":"Weekly","remainingPercent":0,"windowPositionPercent":0,"resetInSeconds":12},{"key":"daily","expired":true}]}]}}`)
	}))
	defer hub.Close()
	cfg, err := server.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AgentHubInstanceID, cfg.AgentHubEndpoint = "hub", hub.URL
	if err := server.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	result := read(200)
	var snapshot cardQuotaSnapshot
	if err := json.Unmarshal([]byte(result), &snapshot); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result, "private") || !snapshot.Stale || len(snapshot.Providers) != 1 || len(snapshot.Providers[0].Quotas) != 2 {
		t.Fatal(result)
	}
	provider := snapshot.Providers[0]
	weekly, daily := provider.Quotas[0], provider.Quotas[1]
	if provider.ID != "account" || weekly.Key != "weekly" || weekly.RemainingPercent == nil || *weekly.RemainingPercent != 0 ||
		weekly.WindowPositionPercent == nil || *weekly.WindowPositionPercent != 0 || weekly.ResetInSeconds == nil || *weekly.ResetInSeconds != 12 ||
		!daily.Expired || daily.WindowPositionPercent != nil || daily.ResetInSeconds != nil {
		t.Fatal(result)
	}
	hub.Close()
	read(502)
}
