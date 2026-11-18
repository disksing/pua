package companion

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disksing/pua/agenthub/internal/config"
)

func magpieSettings(serverURL string) config.Magpie {
	settings := config.Defaults().Magpie
	settings.ServerURL = serverURL
	settings.Enabled = true
	return settings
}

func TestMagpieWindowCountdown(t *testing.T) {
	const payload = `{"object":"list","data":[{"provider":"claude","name":"Claude","kind":"subscription","readAt":"2026-01-01T12:00:00Z","windows":[
		{"name":"5 hours","used":20,"remaining":80,"resetsAt":"2026-01-01T14:30:00Z"},
		{"name":"7 days","used":40,"remaining":60,"resetsAt":"2026-01-05T12:00:00Z"},
		{"name":"7 days · Sonnet","used":0,"remaining":100,"resetsAt":"2026-01-09T12:00:00Z"},
		{"name":"25 hours","used":30,"remaining":70,"resetsAt":"2026-01-01T14:00:00Z"},
		{"name":"7 days · Opus","used":0,"remaining":100},
		{"name":"5 hours · expired","used":90,"remaining":10,"resetsAt":"2026-01-01T12:00:00Z"},
		{"name":"5 hours · invalid reset","used":20,"remaining":80,"resetsAt":"unknown"},
		{"name":"5 hours · unlimited","unlimited":true,"used":0,"remaining":100,"resetsAt":"2026-01-01T14:00:00Z"},
		{"name":"7 days · wallet","balance":0,"unit":"USD","used":0,"remaining":0,"resetsAt":"2026-01-03T12:00:00Z"},
		{"name":"7 days · text","text":true,"display":"Pending","used":0,"remaining":0,"resetsAt":"2026-01-03T12:00:00Z"},
		{"name":"7 days · no reading","resetsAt":"2026-01-03T12:00:00Z"}
	]}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, payload)
	}))
	defer upstream.Close()
	service := NewService(nil)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	service.Configure(magpieSettings(upstream.URL))

	windows := service.Snapshot(t.Context()).Providers[0].Quotas
	for index, want := range []float64{50, 100.0 * 4 / 7, 100} {
		window := windows[index]
		if window.WindowPositionPercent == nil || *window.WindowPositionPercent != want {
			t.Errorf("%s: cursor = %v, want %g%%", window.Label, window.WindowPositionPercent, want)
		}
	}
	for _, window := range windows[3:] {
		if window.WindowPositionPercent != nil {
			t.Errorf("%s: unexpected cursor %g%%", window.Label, *window.WindowPositionPercent)
		}
	}
	if windows[3].ResetInSeconds == nil || !windows[5].Expired || windows[5].ResetInSeconds != nil {
		t.Errorf("unexpected reset metadata: unknown=%+v expired=%+v", windows[3], windows[5])
	}

	now = now.Add(30 * time.Second)
	first := service.Snapshot(t.Context()).Providers[0].Quotas[0]
	if first.WindowPositionPercent == nil || *first.WindowPositionPercent >= 50 || first.ResetInSeconds == nil || *first.ResetInSeconds != 8970 {
		t.Errorf("cursor should move left as reset approaches: %+v", first)
	}
	now = now.Add(8970 * time.Second)
	first = service.Snapshot(t.Context()).Providers[0].Quotas[0]
	if !first.Expired || first.ResetInSeconds != nil || first.WindowPositionPercent != nil {
		t.Errorf("cursor should disappear at reset: %+v", first)
	}
}

func TestMagpieNormalizesAccountsAndWindows(t *testing.T) {
	const payload = `{"data":[
		{"provider":"codex","kind":"subscription","user":"alice@example.test","name":"Codex","plan":"Plus","readAt":"2026-01-01T12:00:00.123456+00:00","windows":[
			{"name":"Primary 5h","used":0,"remaining":100,"resetsAt":"2026-01-01T15:00:00Z"},
			{"name":"Secondary 5h","used":100,"resetsAt":"2026-01-01T12:01:00Z"},
			{"name":"Included","unlimited":true,"used":0,"remaining":100},
			{"name":"Wallet","balance":0,"unit":"USD","used":0,"remaining":0},
			{"name":"Credits","amount":0,"limit":50,"unit":"credits","remaining":100},
			{"name":"Info","text":true,"display":"Pending","used":0,"remaining":0},
			{"name":"Missing"}]},
		{"provider":"codex","kind":"subscription","user":"bob@example.test","name":"Codex","asOf":"2026-01-01T11:40:00Z","windows":[{"name":"5h","used":30}]},
		{"provider":"deepseek","kind":"balance","name":"DeepSeek","balance":"¥0.00","windows":[]},
		{"provider":"claude","kind":"subscription","name":"claude@example.test","user":"claude@example.test","error":"denied: secret-token","windows":[]}
	]}`
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/v1/magpie/quotas?kept=1" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		fmt.Fprint(w, payload)
	}))
	defer upstream.Close()
	service := NewService(nil)
	now := time.Date(2026, 1, 1, 12, 0, 30, 0, time.UTC)
	service.now = func() time.Time { return now }
	service.Configure(magpieSettings(upstream.URL))
	snapshot := service.Snapshot(t.Context())
	if !snapshot.Connected || len(snapshot.Providers) != 4 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	first, second, balance, failed := snapshot.Providers[0], snapshot.Providers[1], snapshot.Providers[2], snapshot.Providers[3]
	if first.ID == second.ID || first.Provider != second.Provider || first.AccountLabel == second.AccountLabel || first.PlanLabel != "Plus" || !second.Stale {
		t.Fatalf("account projections = %+v / %+v", first, second)
	}
	primary, secondary, unlimited, wallet, credits, text, unknown := first.Quotas[0], first.Quotas[1], first.Quotas[2], first.Quotas[3], first.Quotas[4], first.Quotas[5], first.Quotas[6]
	if primary.Kind != secondary.Kind || primary.Key == secondary.Key || *primary.UsedPercent != 0 || *secondary.RemainingPercent != 0 || *secondary.ResetInSeconds != 30 {
		t.Fatalf("normalized windows = %+v / %+v", primary, secondary)
	}
	if !unlimited.Unlimited || unlimited.RemainingPercent != nil || wallet.Display != "0 USD" || wallet.RemainingPercent != nil || text.Display != "Pending" || text.RemainingPercent != nil || unknown.RemainingPercent != nil || unknown.Status != "unavailable" {
		t.Fatalf("special windows = %+v", first.Quotas)
	}
	if credits.Used == nil || *credits.Used != 0 || *credits.Limit != 50 || credits.Unit != "credits" || credits.ResetInSeconds != nil || balance.Quotas[0].Display != "¥0.00" || balance.Quotas[0].RemainingPercent != nil {
		t.Fatalf("counts/balance = %+v / %+v", credits, balance)
	}
	if failed.Status != "unavailable" || failed.Error == "" {
		t.Fatalf("failed card = %+v", failed)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil || strings.Contains(string(encoded), "@example.test") || strings.Contains(string(encoded), "secret-token") {
		t.Fatal("upstream identity or error text leaked into the snapshot")
	}
	now = now.Add(40 * time.Second)
	cached := service.Snapshot(t.Context())
	if requests.Load() != 1 || !cached.Providers[0].Quotas[1].Expired || !cached.Providers[0].Quotas[1].Stale || cached.Providers[0].Quotas[1].ResetInSeconds != nil || cached.Providers[0].Quotas[0].Stale {
		t.Fatalf("expired reading = %+v, requests = %d", cached.Providers[0], requests.Load())
	}
}

func TestMagpieCacheCoalescesAndRecovers(t *testing.T) {
	var requests, mode atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		switch mode.Load() {
		case 0:
			fmt.Fprint(w, `{"data":[{"provider":"codex","kind":"subscription","user":"alice","windows":[{"name":"5h","used":20}]},{"provider":"kimi","kind":"plan","windows":[{"name":"Weekly","used":10}]}]}`)
		case 1:
			http.Error(w, "private body", http.StatusServiceUnavailable)
		case 2:
			fmt.Fprint(w, `{"data":[{"provider":"codex","kind":"subscription","user":"alice","error":"private detail","readDeferred":true,"windows":[]}]}`)
		default:
			fmt.Fprint(w, `{"data":[]}`)
		}
	}))
	defer upstream.Close()
	now := time.Now()
	service := NewService(nil)
	service.now = func() time.Time { return now }
	service.Configure(magpieSettings(upstream.URL))
	var group sync.WaitGroup
	for range 10 {
		group.Go(func() {
			if got := service.Snapshot(t.Context()); !got.Connected || len(got.Providers) != 2 {
				t.Errorf("initial snapshot = %+v", got)
			}
		})
	}
	group.Wait()
	if requests.Load() != 1 {
		t.Fatalf("concurrent snapshot requests = %d", requests.Load())
	}
	mode.Store(1)
	captured := now
	now = now.Add(time.Minute)
	for range 3 {
		got := service.Snapshot(t.Context())
		if got.Connected || !got.Stale || len(got.Providers) != 2 || !got.UpdatedAt.Equal(captured) || !strings.Contains(got.Error, "503") || strings.Contains(got.Error, "private") {
			t.Fatalf("fallback snapshot = %+v", got)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("failure should be cached during backoff, got %d requests", requests.Load())
	}
	mode.Store(2)
	now = now.Add(5 * time.Second)
	if got := service.Snapshot(t.Context()); !got.Connected || len(got.Providers) != 1 || !got.Providers[0].Stale || !got.Providers[0].ReadDeferred || got.Providers[0].Error == "" || len(got.Providers[0].Quotas) != 1 {
		t.Fatalf("partial snapshot = %+v", got)
	}
	mode.Store(3)
	now = now.Add(time.Minute)
	if got := service.Snapshot(t.Context()); !got.Connected || got.Stale || got.Error != "" || len(got.Providers) != 0 {
		t.Fatalf("empty recovered snapshot = %+v", got)
	}
}

func TestMagpieOldRequestCannotPopulateReconfiguredCache(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	oldServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		fmt.Fprint(w, `{"data":[{"provider":"old","kind":"balance","balance":"$1"}]}`)
	}))
	defer oldServer.Close()
	newServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"provider":"new","kind":"balance","balance":"$2"}]}`)
	}))
	defer newServer.Close()
	service := NewService(nil)
	service.Configure(magpieSettings(oldServer.URL))
	done := make(chan QuotaSnapshot, 1)
	go func() { done <- service.Snapshot(t.Context()) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("old request did not start")
	}
	service.Configure(magpieSettings(newServer.URL))
	got := service.Snapshot(t.Context())
	close(release)
	if len(got.Providers) != 1 || got.Providers[0].Provider != "new" {
		t.Fatalf("new snapshot = %+v", got)
	}
	if got := <-done; len(got.Providers) != 1 || got.Providers[0].Provider != "new" {
		t.Fatalf("old request returned data across configuration boundary: %+v", got)
	}
}

func TestMagpieConnectionRejectsInvalidResponsesAndRedirects(t *testing.T) {
	for _, scenario := range []struct {
		status int
		body   string
	}{
		{http.StatusUnauthorized, "private"},
		{http.StatusForbidden, "private"},
		{http.StatusOK, `{"data":null}`},
		{http.StatusOK, `{"object":"list"}`},
		{http.StatusOK, `{"data":[null]}`},
		{http.StatusOK, `{"data":[`},
		{http.StatusOK, strings.Repeat(" ", maxResponseSize+1)},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(scenario.status)
			fmt.Fprint(w, scenario.body)
		}))
		if _, err := NewService(nil).TestConnection(t.Context(), magpieSettings(server.URL)); err == nil || strings.Contains(err.Error(), "private") {
			t.Errorf("status %d returned unexpected error %v", scenario.status, err)
		}
		server.Close()
	}
	var followed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/other" {
			followed.Store(true)
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		http.Redirect(w, r, "/other", http.StatusFound)
	}))
	defer server.Close()
	if _, err := NewService(server.Client()).TestConnection(t.Context(), magpieSettings(server.URL)); err == nil || followed.Load() {
		t.Fatalf("redirect followed or accepted: %v", err)
	}
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := NewService(nil).TestConnection(ctx, magpieSettings(server.URL)); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("deadline error = %v", err)
	}
}

func TestMagpieAccountIdentityIsStableAndKeepsAmbiguousRowsUnavailable(t *testing.T) {
	rows := []magpieRow{
		{Provider: "codex", Kind: "subscription", User: "alice", ReadAt: "2026-01-01T00:00:00Z"},
		{Provider: "codex", Kind: "subscription", User: "ALICE", ReadAt: "2026-01-01T01:00:00Z"},
		{Provider: "codex", Kind: "subscription", User: "alice", From: "office"},
		{Provider: "codex", Kind: "plan", User: "alice"},
		{Provider: "codex", Kind: "balance", Balance: "$5"},
		{Provider: "codex", Kind: "balance", Balance: "$6"},
	}
	providers, err := mapRows(rows)
	if err != nil || len(providers) != 4 {
		t.Fatalf("providers = %+v, %v", providers, err)
	}
	if providers[0].CapturedAt != rows[1].ReadAt || providers[0].ID == providers[1].ID || providers[0].ID == providers[2].ID || providers[3].Error == "" || len(providers[3].Quotas) != 0 {
		t.Fatalf("providers = %+v", providers)
	}
	reordered, err := mapRows([]magpieRow{rows[1], rows[3], rows[2], rows[0], rows[5], rows[4]})
	if err != nil || reordered[0].ID != providers[0].ID || reordered[0].CapturedAt != rows[1].ReadAt || reordered[3].ID != providers[3].ID {
		t.Fatalf("reordered = %+v, %v", reordered, err)
	}
}
