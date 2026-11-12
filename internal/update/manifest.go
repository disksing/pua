package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"time"

	productversion "github.com/disksing/pua/internal/version"
)

const ManifestSchemaVersion = 1

type Manifest struct {
	SchemaVersion int              `json:"schemaVersion"`
	Channel       string           `json:"channel"`
	GeneratedAt   time.Time        `json:"generatedAt"`
	PUA           ComponentRelease `json:"pua"`
	AgentHub      ComponentRelease `json:"agentHub"`
}

type ComponentRelease struct {
	Component                 string  `json:"component"`
	Version                   string  `json:"version"`
	Commit                    string  `json:"commit"`
	ReleaseNotesURL           string  `json:"releaseNotesUrl,omitempty"`
	MinDesktopManagerProtocol int     `json:"minDesktopManagerProtocol"`
	APIMajor                  string  `json:"apiMajor,omitempty"`
	MinAgentHubVersion        string  `json:"minAgentHubVersion,omitempty"`
	Assets                    []Asset `json:"assets"`
}

type Asset struct {
	OS                string `json:"os"`
	Arch              string `json:"arch"`
	URL               string `json:"url"`
	SHA256            string `json:"sha256"`
	Size              int64  `json:"size"`
	SigningTeamID     string `json:"signingTeamId,omitempty"`
	SigningIdentifier string `json:"signingIdentifier,omitempty"`
	// CodeIdentity is accepted for manifests published before signingTeamId and
	// signingIdentifier were introduced. New release descriptors omit it.
	CodeIdentity string `json:"codeIdentity,omitempty"`
}

func Verify(data []byte, signatureBase64 string, publicKey ed25519.PublicKey) (Manifest, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return Manifest{}, errors.New("component update public key is invalid")
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signatureBase64))
	if err != nil || len(signature) != ed25519.SignatureSize {
		return Manifest{}, errors.New("component update signature is invalid")
	}
	if !ed25519.Verify(publicKey, data, signature) {
		return Manifest{}, errors.New("component update manifest signature verification failed")
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode component update manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func ParsePublicKey(value string) (ed25519.PublicKey, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, errors.New("component update public key must be a base64 Ed25519 public key")
	}
	return ed25519.PublicKey(decoded), nil
}

func (manifest Manifest) Validate() error {
	if manifest.SchemaVersion != ManifestSchemaVersion {
		return fmt.Errorf("unsupported component update manifest schema %d", manifest.SchemaVersion)
	}
	if manifest.Channel != "stable" && manifest.Channel != "beta" {
		return fmt.Errorf("unsupported component update channel %q", manifest.Channel)
	}
	if manifest.GeneratedAt.IsZero() {
		return errors.New("component update manifest has no generation time")
	}
	if err := validateRelease(manifest.PUA, "pua", manifest.Channel); err != nil {
		return err
	}
	if err := validateRelease(manifest.AgentHub, "agenthub", manifest.Channel); err != nil {
		return err
	}
	if _, err := productversion.Parse(manifest.PUA.MinAgentHubVersion); err != nil {
		return fmt.Errorf("PUA minimum AgentHub version is invalid: %w", err)
	}
	if manifest.PUA.APIMajor == "" || manifest.AgentHub.APIMajor == "" || manifest.PUA.APIMajor != manifest.AgentHub.APIMajor {
		return errors.New("PUA and AgentHub update entries must declare the same API major")
	}
	return nil
}

func validateRelease(release ComponentRelease, component, channel string) error {
	if release.Component != component {
		return fmt.Errorf("component update entry %q is labeled %q", component, release.Component)
	}
	parsed, err := productversion.Parse(release.Version)
	if err != nil {
		return fmt.Errorf("%s version is invalid: %w", component, err)
	}
	if channel == "stable" && (len(parsed.Prerelease) > 0 || len(parsed.Metadata) > 0) {
		return fmt.Errorf("stable %s version must not contain prerelease or build metadata", component)
	}
	if strings.TrimSpace(release.Commit) == "" || release.MinDesktopManagerProtocol < 1 {
		return fmt.Errorf("%s update entry has incomplete build or manager compatibility information", component)
	}
	if len(release.Assets) == 0 {
		return fmt.Errorf("%s update entry has no assets", component)
	}
	seen := map[string]bool{}
	for _, asset := range release.Assets {
		key := asset.OS + "/" + asset.Arch
		if seen[key] {
			return fmt.Errorf("%s update entry has duplicate asset %s", component, key)
		}
		seen[key] = true
		if asset.OS == "" || asset.Arch == "" || asset.Size < 1 {
			return fmt.Errorf("%s asset %s is incomplete", component, key)
		}
		if asset.OS == "darwin" {
			hasSigningPair := strings.TrimSpace(asset.SigningTeamID) != "" && strings.TrimSpace(asset.SigningIdentifier) != ""
			if !hasSigningPair && strings.TrimSpace(asset.CodeIdentity) == "" {
				return fmt.Errorf("%s asset %s has no Developer ID signing metadata", component, key)
			}
			if (strings.TrimSpace(asset.SigningTeamID) == "") != (strings.TrimSpace(asset.SigningIdentifier) == "") {
				return fmt.Errorf("%s asset %s has incomplete Developer ID signing metadata", component, key)
			}
		}
		if digest, err := hex.DecodeString(asset.SHA256); err != nil || len(digest) != sha256.Size {
			return fmt.Errorf("%s asset %s has invalid SHA-256", component, key)
		}
		parsedURL, err := url.Parse(asset.URL)
		if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" || parsedURL.User != nil {
			return fmt.Errorf("%s asset %s must use an absolute HTTPS URL without credentials", component, key)
		}
	}
	return nil
}

func (release ComponentRelease) AssetForCurrentPlatform() (Asset, error) {
	return release.AssetFor(runtime.GOOS, runtime.GOARCH)
}

func (release ComponentRelease) AssetFor(osName, arch string) (Asset, error) {
	for _, asset := range release.Assets {
		if asset.OS == osName && asset.Arch == arch {
			return asset, nil
		}
	}
	return Asset{}, fmt.Errorf("%s %s has no asset for %s/%s", release.Component, release.Version, osName, arch)
}
