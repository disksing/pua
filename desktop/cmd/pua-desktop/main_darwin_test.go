//go:build darwin

package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/disksing/pua/internal/desktop"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// The Wails bundled asset server rebases the embedded FS root to the directory
// containing index.html (assets/), so every absolute asset reference in the
// Service Manager page must resolve relative to that rebased root.
func TestBundledAssetsServeServiceManager(t *testing.T) {
	server := application.BundledAssetFileServer(assets)
	page, err := assets.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("read embedded index.html: %v", err)
	}
	references := append(assetReferences(t, string(page)), "/", "/wails/runtime.js")
	for _, path := range references {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Errorf("GET %s (referenced by index.html): status %d, want %d", path, recorder.Code, http.StatusOK)
		}
	}
}

func assetReferences(t *testing.T, html string) []string {
	t.Helper()
	pattern := regexp.MustCompile(`(?:href|src)="(/[^"]*)"`)
	var references []string
	for _, match := range pattern.FindAllStringSubmatch(html, -1) {
		references = append(references, match[1])
	}
	if len(references) == 0 {
		t.Fatal("index.html references no absolute assets")
	}
	return references
}

func TestWindowURL(t *testing.T) {
	status := desktop.Status{
		PUA:      desktop.ComponentStatus{Endpoint: "http://127.0.0.1:4936/"},
		AgentHub: desktop.ComponentStatus{Endpoint: "http://127.0.0.1:14646/agenthub"},
	}
	cases := []struct {
		name string
		want string
	}{
		{"main", "http://127.0.0.1:4936/"},
		{"agenthub", "http://127.0.0.1:14646/agenthub/"},
		{"beeper", "http://127.0.0.1:14646/agenthub/beeper"},
	}
	for _, tc := range cases {
		if got := windowURL(status, tc.name); got != tc.want {
			t.Errorf("windowURL(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestWindowURLWithoutEndpoints(t *testing.T) {
	var status desktop.Status
	for _, name := range []string{"main", "agenthub", "beeper"} {
		if got := windowURL(status, name); got != "" {
			t.Errorf("windowURL(%q) = %q, want empty", name, got)
		}
	}
	// AgentHub windows must not fall back to the PUA endpoint: a managed PUA
	// server runs with --agenthub-mode=external and does not serve /agenthub/.
	status.PUA.Endpoint = "http://127.0.0.1:4936"
	for _, name := range []string{"agenthub", "beeper"} {
		if got := windowURL(status, name); got != "" {
			t.Errorf("windowURL(%q) = %q, want empty", name, got)
		}
	}
}
