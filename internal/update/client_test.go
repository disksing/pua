package update

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func TestDownloadAssetVerifiesSizeAndDigest(t *testing.T) {
	body := "signed component"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	destination := filepath.Join(t.TempDir(), "pua")
	asset := Asset{URL: "https://example.test/pua", Size: int64(len(body)), SHA256: digest}
	if err := DownloadAsset(context.Background(), client, asset, destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != body {
		t.Fatalf("downloaded data = %q, %v", data, err)
	}
	asset.SHA256 = strings.Repeat("0", 64)
	if err := DownloadAsset(context.Background(), client, asset, destination+"-bad"); err == nil {
		t.Fatal("digest mismatch was accepted")
	}
}

func TestFetchRequiresHTTPS(t *testing.T) {
	if _, err := Fetch(context.Background(), http.DefaultClient, "http://example.test/channel-v1.json", make([]byte, 32)); err == nil {
		t.Fatal("insecure manifest URL was accepted")
	}
}
