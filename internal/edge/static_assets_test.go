package edge

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"simple_cdn/internal/domain"
)

type staticAssetRoundTripFunc func(*http.Request) (*http.Response, error)

func (function staticAssetRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestSyncStaticAssetsDownloadsVerifiesAndRepairsObject(t *testing.T) {
	contents := []byte("console.log('edge static resource');\n")
	digest := fmt.Sprintf("%x", sha256.Sum256(contents))
	var requests atomic.Int32
	client := &http.Client{Transport: staticAssetRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		if request.URL.Path != "/api/edge/v1/static-assets/"+digest || request.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("unexpected request %s, encoding %q", request.URL.Path, request.Header.Get("Accept-Encoding"))
		}
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header),
			Body: io.NopCloser(bytes.NewReader(contents)), ContentLength: int64(len(contents)),
		}, nil
	})}
	directory := t.TempDir()
	agent := &Agent{Config: Config{
		ControlURL: "https://control.example.test", HTTPClient: client, StaticAssetDirectory: directory,
	}}
	reference := staticAssetTestReference(digest, int64(len(contents)))
	if err := agent.syncStaticAssets([]domain.StaticAssetReference{reference}); err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(filepath.Join(directory, digest))
	if err != nil || string(installed) != string(contents) {
		t.Fatalf("installed contents = %q, err = %v", installed, err)
	}
	if err := agent.syncStaticAssets([]domain.StaticAssetReference{reference}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("download requests = %d, want 1 for a valid existing object", requests.Load())
	}
	if err := os.WriteFile(filepath.Join(directory, digest), []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := agent.syncStaticAssets([]domain.StaticAssetReference{reference}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("download requests after repair = %d, want 2", requests.Load())
	}
	linkedObject := filepath.Join(directory, "linked-object")
	if err := os.WriteFile(linkedObject, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(directory, digest)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linkedObject, filepath.Join(directory, digest)); err != nil {
		t.Fatal(err)
	}
	if err := agent.syncStaticAssets([]domain.StaticAssetReference{reference}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 3 {
		t.Fatalf("download requests after symlink repair = %d, want 3", requests.Load())
	}
	info, err := os.Lstat(filepath.Join(directory, digest))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("repaired object mode = %v, want regular file", info.Mode())
	}
}

func TestSyncStaticAssetsRejectsDigestMismatchWithoutInstalling(t *testing.T) {
	digest := strings.Repeat("a", 64)
	client := &http.Client{Transport: staticAssetRoundTripFunc(func(*http.Request) (*http.Response, error) {
		contents := []byte("wrong")
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header),
			Body: io.NopCloser(bytes.NewReader(contents)), ContentLength: int64(len(contents)),
		}, nil
	})}
	directory := t.TempDir()
	agent := &Agent{Config: Config{
		ControlURL: "https://control.example.test", HTTPClient: client, StaticAssetDirectory: directory,
	}}
	if err := agent.syncStaticAssets([]domain.StaticAssetReference{staticAssetTestReference(digest, 5)}); err == nil {
		t.Fatal("accepted a static resource with the wrong digest")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed download left files: %#v", entries)
	}
}

func TestCleanupStaticAssetsOnlyRemovesUnreferencedContentObjects(t *testing.T) {
	directory := t.TempDir()
	desiredDigest := strings.Repeat("b", 64)
	staleDigest := strings.Repeat("c", 64)
	for name, contents := range map[string]string{
		desiredDigest: "keep", staleDigest: "remove", "README": "unmanaged",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	agent := &Agent{Config: Config{StaticAssetDirectory: directory}}
	if err := agent.cleanupStaticAssets([]domain.StaticAssetReference{staticAssetTestReference(desiredDigest, 4)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, desiredDigest)); err != nil {
		t.Fatalf("desired object was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, staleDigest)); !os.IsNotExist(err) {
		t.Fatalf("stale object remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "README")); err != nil {
		t.Fatalf("unmanaged file was removed: %v", err)
	}
}

func staticAssetTestReference(digest string, size int64) domain.StaticAssetReference {
	return domain.StaticAssetReference{
		AssetID: "asset", BindingID: "binding", SiteID: "site", URLPath: "/app.js",
		SHA256: digest, SizeBytes: size, ContentType: "application/javascript",
		CacheControl: domain.StaticAssetCacheHour,
	}
}
