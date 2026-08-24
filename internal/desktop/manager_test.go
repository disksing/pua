//go:build darwin

package desktop

import (
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		config  Config
		address string
		exposed bool
	}{
		{Config{Host: "127.0.0.1", Port: 4936, AutoCheck: true}, "127.0.0.1:4936", false},
		{Config{Host: "[::1]", Port: 4936}, "[::1]:4936", false},
		{Config{Host: "0.0.0.0", Port: 8080}, "0.0.0.0:8080", true},
	}
	for _, test := range tests {
		normalized, address, err := normalizeConfig(test.config)
		if err != nil {
			t.Fatal(err)
		}
		if address != test.address || normalized.SchemaVersion != desktopConfigVersion {
			t.Fatalf("normalizeConfig(%+v) = %+v, %q", test.config, normalized, address)
		}
		if exposed := !isLoopbackHost(normalized.Host); exposed != test.exposed {
			t.Fatalf("host %q exposed = %v", normalized.Host, exposed)
		}
	}
	for _, config := range []Config{{Host: "", Port: 4936}, {Host: "https://localhost", Port: 4936}, {Host: "bad host", Port: 4936}, {Host: "localhost", Port: 0}, {Host: "localhost", Port: 65536}} {
		if _, _, err := normalizeConfig(config); err == nil {
			t.Fatalf("normalizeConfig(%+v) succeeded", config)
		}
	}
}

func TestAutomaticUpdateDueUsesPersistedDailyInterval(t *testing.T) {
	temporary := t.TempDir()
	options := Options{Address: defaultAddress, AgentHubAddress: defaultAgentHubAddress,
		ConfigPath: filepath.Join(temporary, "serve.json"), AppSupportDir: filepath.Join(temporary, "support")}
	manager, err := NewManager(options)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	if !manager.AutomaticUpdateDue(now) {
		t.Fatal("missing update state was not due")
	}
	if err := writeJSONAtomic(componentUpdateStatePath(manager.Options()), componentUpdateState{SchemaVersion: componentUpdateStateVersion, LastCheckedAt: now}); err != nil {
		t.Fatal(err)
	}
	if manager.AutomaticUpdateDue(now.Add(23 * time.Hour)) {
		t.Fatal("update check became due before 24 hours")
	}
	if !manager.AutomaticUpdateDue(now.Add(24 * time.Hour)) {
		t.Fatal("update check was not due after 24 hours")
	}
}

func TestManagerSaveConfigPersistsAndReportsRestart(t *testing.T) {
	t.Parallel()
	temporary := t.TempDir()
	options := Options{Address: defaultAddress, AgentHubAddress: defaultAgentHubAddress,
		ConfigPath: filepath.Join(temporary, "serve.json"), AppSupportDir: filepath.Join(temporary, "support")}
	manager, err := NewManager(options)
	if err != nil {
		t.Fatal(err)
	}
	restart, err := manager.SaveConfig(Config{Host: "0.0.0.0", Port: 5940, AutoCheck: false})
	if err != nil {
		t.Fatal(err)
	}
	if !restart {
		t.Fatal("address change did not require restart")
	}
	stored, err := loadConfig(manager.Options())
	if err != nil {
		t.Fatal(err)
	}
	if stored.Host != "0.0.0.0" || stored.Port != 5940 || stored.AutoCheck {
		t.Fatalf("stored config = %+v", stored)
	}
	restart, err = manager.SaveConfig(stored)
	if err != nil || restart {
		t.Fatalf("unchanged config restart=%v err=%v", restart, err)
	}
}
