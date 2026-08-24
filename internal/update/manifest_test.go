package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testManifest() Manifest {
	asset := func(name string) Asset {
		return Asset{OS: "darwin", Arch: "arm64", URL: "https://github.com/disksing/pua/releases/download/" + name + "/asset.zip",
			SHA256: strings.Repeat("a", 64), Size: 123, CodeIdentity: "Developer ID Application: Test"}
	}
	return Manifest{SchemaVersion: ManifestSchemaVersion, Channel: "stable", GeneratedAt: time.Now().UTC(),
		PUA: ComponentRelease{Component: "pua", Version: "0.4.2", Commit: "pua-sha", MinDesktopManagerProtocol: 1,
			APIMajor: "1", MinAgentHubVersion: "0.7.0", Assets: []Asset{asset("pua-v0.4.2")}},
		AgentHub: ComponentRelease{Component: "agenthub", Version: "0.7.1", Commit: "hub-sha", MinDesktopManagerProtocol: 1,
			APIMajor: "1", Assets: []Asset{asset("agenthub-v0.7.1")}},
	}
}

func TestVerifySignedManifest(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(testManifest())
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, data))
	manifest, err := Verify(data, signature, publicKey)
	if err != nil || manifest.PUA.Version != "0.4.2" {
		t.Fatalf("Verify() = %+v, %v", manifest, err)
	}
	data[0] ^= 1
	if _, err := Verify(data, signature, publicKey); err == nil {
		t.Fatal("tampered manifest passed signature verification")
	}
}

func TestManifestRejectsDevelopmentVersionInStableChannel(t *testing.T) {
	manifest := testManifest()
	manifest.PUA.Version = "0.5.0-dev.1+gabc"
	if err := manifest.Validate(); err == nil {
		t.Fatal("stable manifest accepted development version")
	}
}

func TestResolveIndependentAndCombinedUpdates(t *testing.T) {
	manifest := testManifest()
	installed := Installed{PUAVersion: "0.4.1", AgentHubVersion: "0.7.1", ManagerProtocol: 1, OS: "darwin", Arch: "arm64"}
	plan, err := Resolve(manifest, installed)
	if err != nil {
		t.Fatal(err)
	}
	if plan.PUA == nil || plan.AgentHub != nil || plan.CompatibilityError != "" {
		t.Fatalf("unexpected PUA-only plan: %+v", plan)
	}

	installed = Installed{PUAVersion: "0.4.1", AgentHubVersion: "0.6.9", ManagerProtocol: 1, OS: "darwin", Arch: "arm64"}
	plan, err = Resolve(manifest, installed)
	if err != nil {
		t.Fatal(err)
	}
	if plan.PUA == nil || plan.AgentHub == nil || plan.CompatibilityError != "" {
		t.Fatalf("unexpected combined plan: %+v", plan)
	}

	manifest.PUA.MinDesktopManagerProtocol = 2
	plan, err = Resolve(manifest, installed)
	if err != nil || !plan.AppUpdateRequired || plan.RequiredManager != 2 {
		t.Fatalf("manager protocol plan = %+v, %v", plan, err)
	}

	installed = Installed{PUAVersion: manifest.PUA.Version, AgentHubVersion: manifest.AgentHub.Version, ManagerProtocol: 1, OS: "darwin", Arch: "arm64"}
	plan, err = Resolve(manifest, installed)
	if err != nil || plan.AppUpdateRequired {
		t.Fatalf("current components incorrectly require an app update: %+v, %v", plan, err)
	}
}
