package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/disksing/pua/agenthub/internal/session"
)

func TestStatusExposesAggregateStoreDiagnosticsWithoutListingSessions(t *testing.T) {
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(session.CreateInput{}); err != nil {
		t.Fatal(err)
	}
	handler := New(store, "test", time.Now()).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	var body struct {
		SessionStore struct {
			Count       int                      `json:"sessionCount"`
			Diagnostics session.StoreDiagnostics `json:"diagnostics"`
		} `json:"sessionStore"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || body.SessionStore.Count != 1 || body.SessionStore.Diagnostics.OpenNanos <= 0 || body.SessionStore.Diagnostics.CachedEventPayloadBytes <= 0 || body.SessionStore.Diagnostics.MetadataQueries != 0 {
		t.Fatalf("status = %+v, code=%d", body, response.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/sessions?limit=1", nil).WithContext(ctx)
	handler.ServeHTTP(response, request)
	if response.Body.Len() != 0 {
		t.Fatalf("cancelled list encoded a response: %s", response.Body.String())
	}
}
