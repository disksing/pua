package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	componentupdate "github.com/disksing/pua/internal/update"
)

func main() {
	puaPath := flag.String("pua", "", "PUA release descriptor JSON")
	agentHubPath := flag.String("agenthub", "", "AgentHub release descriptor JSON")
	outputPath := flag.String("output", "channel-v1.json", "manifest output path")
	signaturePath := flag.String("signature", "", "base64 signature output path")
	channel := flag.String("channel", "stable", "stable or beta")
	privateKeyText := flag.String("private-key", "", "base64 raw Ed25519 private key (prefer PUA_COMPONENT_MANIFEST_PRIVATE_KEY)")
	generateKey := flag.Bool("generate-key", false, "generate a new component manifest signing keypair")
	flag.Parse()
	if *generateKey {
		publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			fatal("generate key: %v", err)
		}
		result, _ := json.MarshalIndent(map[string]string{"publicKey": base64.StdEncoding.EncodeToString(publicKey), "privateKey": base64.StdEncoding.EncodeToString(privateKey)}, "", "  ")
		fmt.Println(string(result))
		return
	}
	if *puaPath == "" || *agentHubPath == "" {
		fatal("both -pua and -agenthub are required")
	}
	privateValue := strings.TrimSpace(*privateKeyText)
	if privateValue == "" {
		privateValue = strings.TrimSpace(os.Getenv("PUA_COMPONENT_MANIFEST_PRIVATE_KEY"))
	}
	privateKey, err := base64.StdEncoding.DecodeString(privateValue)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		fatal("component manifest private key must be a base64 raw Ed25519 private key")
	}
	readRelease := func(path string) componentupdate.ComponentRelease {
		data, err := os.ReadFile(path)
		if err != nil {
			fatal("read %s: %v", path, err)
		}
		var release componentupdate.ComponentRelease
		if err := json.Unmarshal(data, &release); err != nil {
			fatal("decode %s: %v", path, err)
		}
		return release
	}
	manifest := componentupdate.Manifest{SchemaVersion: componentupdate.ManifestSchemaVersion, Channel: *channel,
		GeneratedAt: time.Now().UTC(), PUA: readRelease(*puaPath), AgentHub: readRelease(*agentHubPath)}
	if err := manifest.Validate(); err != nil {
		fatal("validate manifest: %v", err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		fatal("encode manifest: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(*outputPath, data, 0o644); err != nil {
		fatal("write manifest: %v", err)
	}
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.PrivateKey(privateKey), data)) + "\n"
	if *signaturePath == "" {
		*signaturePath = *outputPath + ".sig"
	}
	if err := os.WriteFile(*signaturePath, []byte(signature), 0o644); err != nil {
		fatal("write signature: %v", err)
	}
}

func fatal(format string, arguments ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "component-manifest: "+format+"\n", arguments...)
	os.Exit(1)
}
