package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	componentupdate "github.com/disksing/pua/internal/update"
)

func TestCommandCreatesVerifiableManifest(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	puaPath, agentHubPath := filepath.Join(directory, "pua.json"), filepath.Join(directory, "agenthub.json")
	manifest := testReleaseManifest()
	write := func(path string, value any) {
		data, _ := json.Marshal(value)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(puaPath, manifest.PUA)
	write(agentHubPath, manifest.AgentHub)
	output := filepath.Join(directory, "channel-v1.json")
	command := exec.Command("go", "run", ".", "-pua", puaPath, "-agenthub", agentHubPath, "-output", output,
		"-private-key", base64.StdEncoding.EncodeToString(privateKey))
	if result, err := command.CombinedOutput(); err != nil {
		t.Fatalf("command failed: %v: %s", err, result)
	}
	data, _ := os.ReadFile(output)
	signature, _ := os.ReadFile(output + ".sig")
	if _, err := componentupdate.Verify(data, string(signature), publicKey); err != nil {
		t.Fatal(err)
	}
}

func testReleaseManifest() componentupdate.Manifest {
	asset := componentupdate.Asset{OS: "darwin", Arch: "arm64", URL: "https://example.test/component", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 10, SigningTeamID: "TESTTEAM01", SigningIdentifier: "com.example.component"}
	return componentupdate.Manifest{PUA: componentupdate.ComponentRelease{Component: "pua", Version: "1.0.0", Commit: "abc", MinDesktopManagerProtocol: 1, APIMajor: "1", MinAgentHubVersion: "1.0.0", Assets: []componentupdate.Asset{asset}},
		AgentHub: componentupdate.ComponentRelease{Component: "agenthub", Version: "1.0.0", Commit: "def", MinDesktopManagerProtocol: 1, APIMajor: "1", Assets: []componentupdate.Asset{asset}}}
}
