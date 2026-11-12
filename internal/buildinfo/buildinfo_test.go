package buildinfo

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCurrentSeparatesVersionAndSourceIdentity(t *testing.T) {
	previous := []string{Version, Channel, Branch, SHA, BuildTime, EmbeddedAgentHubVersion, MinAgentHubVersion, MinDesktopManagerProtocol}
	t.Cleanup(func() {
		Version, Channel, Branch, SHA, BuildTime, EmbeddedAgentHubVersion, MinAgentHubVersion, MinDesktopManagerProtocol =
			previous[0], previous[1], previous[2], previous[3], previous[4], previous[5], previous[6], previous[7]
	})
	Version, Channel, Branch, SHA = "0.4.2", "stable", "master", "abc123"
	BuildTime, EmbeddedAgentHubVersion, MinAgentHubVersion, MinDesktopManagerProtocol = "2026-08-24T00:00:00Z", "0.7.1", "0.7.0", "2"

	info := Current("pua")
	if info.Component != "pua" || info.Version != "0.4.2" || info.SHA != "abc123" {
		t.Fatalf("unexpected info: %+v", info)
	}
	if info.EmbeddedAgentHubVersion != "0.7.1" || info.MinAgentHubVersion != "0.7.0" || info.MinDesktopManagerProtocol != 2 {
		t.Fatalf("unexpected compatibility info: %+v", info)
	}
	data, err := JSON("pua")
	if err != nil {
		t.Fatal(err)
	}
	var decoded Info
	if err := json.Unmarshal(data, &decoded); err != nil || decoded != info {
		t.Fatalf("decoded JSON = %+v, %v; want %+v", decoded, err, info)
	}
	if text := Text("pua"); !strings.Contains(text, "version=0.4.2") || !strings.Contains(text, "sha=abc123") {
		t.Fatalf("Text() = %q", text)
	}
}

func TestIsDevelopment(t *testing.T) {
	for _, info := range []Info{{Channel: "dev", Version: "0.1.0"}, {Channel: "beta", Version: "0.2.0-dev.4+gabc"}, {Channel: "stable", Version: "0.2.0+gabc.dirty"}} {
		if !IsDevelopment(info) {
			t.Fatalf("IsDevelopment(%+v) = false", info)
		}
	}
	if IsDevelopment(Info{Channel: "stable", Version: "0.2.0"}) {
		t.Fatal("stable release was marked development")
	}
}
