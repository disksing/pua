package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxManifestBytes  = 2 << 20
	maxSignatureBytes = 4096
	maxAssetBytes     = 512 << 20
)

func Fetch(ctx context.Context, client *http.Client, manifestURL string, publicKey ed25519.PublicKey) (Manifest, error) {
	if client == nil {
		return Manifest{}, errors.New("component update HTTP client is nil")
	}
	parsed, err := url.Parse(strings.TrimSpace(manifestURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return Manifest{}, errors.New("component update manifest must use an absolute HTTPS URL without credentials")
	}
	manifestData, err := fetchLimited(ctx, client, parsed.String(), maxManifestBytes)
	if err != nil {
		return Manifest{}, fmt.Errorf("download component update manifest: %w", err)
	}
	signatureData, err := fetchLimited(ctx, client, parsed.String()+".sig", maxSignatureBytes)
	if err != nil {
		return Manifest{}, fmt.Errorf("download component update signature: %w", err)
	}
	return Verify(manifestData, string(signatureData), publicKey)
}

// DownloadAsset downloads exactly the bytes covered by the signed manifest.
// destination must be a new staging path; callers only publish it after any
// platform code-signature checks have also succeeded.
func DownloadAsset(ctx context.Context, client *http.Client, asset Asset, destination string) error {
	if client == nil {
		return errors.New("component update HTTP client is nil")
	}
	if asset.Size < 1 || asset.Size > maxAssetBytes {
		return fmt.Errorf("component update asset size %d is outside the supported range", asset.Size)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create component update staging directory: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download component update asset: %w", err)
	}
	defer response.Body.Close()
	if response.Request != nil && response.Request.URL != nil && response.Request.URL.Scheme != "https" {
		return errors.New("component update asset redirected to a non-HTTPS URL")
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download component update asset: HTTP %d", response.StatusCode)
	}
	if response.ContentLength >= 0 && response.ContentLength != asset.Size {
		return fmt.Errorf("component update asset length is %d, want %d", response.ContentLength, asset.Size)
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".component-download-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(response.Body, asset.Size+1))
	if copyErr != nil {
		_ = temporary.Close()
		return fmt.Errorf("write component update asset: %w", copyErr)
	}
	if written != asset.Size {
		_ = temporary.Close()
		return fmt.Errorf("component update asset length is %d, want %d", written, asset.Size)
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); !strings.EqualFold(actual, asset.SHA256) {
		_ = temporary.Close()
		return errors.New("component update asset SHA-256 verification failed")
	}
	if err := temporary.Chmod(0o755); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return err
	}
	return nil
}

func fetchLimited(ctx context.Context, client *http.Client, target string, maximum int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.Request != nil && response.Request.URL != nil && response.Request.URL.Scheme != "https" {
		return nil, errors.New("component update response redirected to a non-HTTPS URL")
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maximum {
		return nil, fmt.Errorf("response exceeds %d bytes", maximum)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("response exceeds %d bytes", maximum)
	}
	return data, nil
}
