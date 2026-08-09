package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

type nginxUpdateRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip nginxUpdateRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestNginxUpdateManagerDownloadsNotifiesAndServesImmutableArtifacts(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	bundlePath := filepath.Join(t.TempDir(), nginxReleaseBundleName)
	writeTestNginxArchive(t, bundlePath, validTestNginxEntries("1.30.5"))
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := ResolveNginxBundle(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	manifest := nginxReleaseManifest{
		SchemaVersion: 1, Channel: "stable", NginxVersion: "1.30.5", Architecture: "amd64",
		Filename: nginxReleaseBundleName, SHA256: metadata.SHA256, SizeBytes: int64(len(bundle)),
		OfficialSourceURL: "https://nginx.org/download/nginx-1.30.5.tar.gz",
		SourceSHA256:      strings.Repeat("c", 64), BuildCommit: strings.Repeat("d", 40),
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	releasesJSON, err := json.Marshal([]nginxGitHubRelease{
		{TagName: "nginx-v9.9.9", Prerelease: true},
		{
			TagName: "nginx-v1.30.5",
			Assets: []nginxGitHubReleaseAsset{
				{Name: nginxReleaseManifestName, BrowserDownloadURL: "https://downloads.example.test/manifest"},
				{Name: nginxReleaseBundleName, BrowserDownloadURL: "https://downloads.example.test/bundle", Size: int64(len(bundle))},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: nginxUpdateRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("request %s is missing GitHub authorization", request.URL)
		}
		switch request.URL.Host + request.URL.Path {
		case "api.example.test/repos/example/project/releases":
			return nginxUpdateResponse(http.StatusOK, releasesJSON), nil
		case "downloads.example.test/manifest":
			return nginxUpdateResponse(http.StatusOK, manifestJSON), nil
		case "downloads.example.test/bundle":
			return nginxUpdateResponse(http.StatusOK, bundle), nil
		default:
			t.Fatalf("unexpected update request: %s", request.URL)
			return nil, nil
		}
	})}
	artifactDirectory := t.TempDir()
	manager, err := NewNginxUpdateManager(NginxUpdateManagerConfig{
		Store: database, Directory: artifactDirectory, Repository: "example/project",
		GitHubAPIURL: "https://api.example.test", GitHubToken: "test-token",
		FallbackVersion: "1.30.4", Interval: 24 * time.Hour, Enabled: true, Client: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	candidate, err := database.CandidateNginxArtifact()
	if err != nil || candidate.Version != "1.30.5" || candidate.SHA256 != metadata.SHA256 || !manager.ArtifactReady(candidate) {
		t.Fatalf("candidate = %#v, err=%v", candidate, err)
	}
	stored, err := os.ReadFile(manager.ArtifactPath(candidate.SHA256))
	if err != nil || !bytes.Equal(stored, bundle) {
		t.Fatalf("stored bundle mismatch: err=%v", err)
	}
	messages, err := database.Messages(10, false)
	if err != nil || len(messages.Messages) != 1 || messages.Messages[0].Category != "nginx_update" {
		t.Fatalf("candidate messages = %#v, err=%v", messages, err)
	}
	if err := manager.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	messages, _ = database.Messages(10, false)
	if len(messages.Messages) != 1 {
		t.Fatalf("repeat check created duplicate messages: %#v", messages.Messages)
	}

	if _, changed, err := manager.Promote(candidate.SHA256); err != nil || !changed {
		t.Fatalf("promote candidate: changed=%v, err=%v", changed, err)
	}
	server := &Server{
		Store: database, ControlURL: "https://control.example.test", EdgeControlURL: "https://edge.example.test", NginxUpdates: manager,
		EdgeBinaryURL: "https://edge.example.test/downloads/agent", EdgeBinarySHA256: strings.Repeat("e", 64),
		NginxBundleURL: "https://edge.example.test/downloads/fallback", NginxBundleSHA256: strings.Repeat("f", 64),
		NginxVersion: "1.30.4",
	}
	target, err := server.currentNginxArtifactTarget()
	if err != nil || !target.Managed || target.SHA256 != candidate.SHA256 {
		t.Fatalf("current target = %#v, err=%v", target, err)
	}
	legacyInstallerResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(legacyInstallerResponse, httptest.NewRequest(http.MethodGet, "/install-edge.sh", nil))
	if legacyInstallerResponse.Code != http.StatusOK ||
		!strings.Contains(legacyInstallerResponse.Body.String(), server.NginxBundleURL) ||
		strings.Contains(legacyInstallerResponse.Body.String(), target.URL) {
		t.Fatal("legacy installer no longer serves the image-bundled fallback")
	}
	legacyInstaller := legacyInstallerResponse.Body.String()
	bundleRequest := httptest.NewRequest(http.MethodGet, target.URL, nil)
	bundleResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(bundleResponse, bundleRequest)
	if bundleResponse.Code != http.StatusOK || !bytes.Equal(bundleResponse.Body.Bytes(), bundle) ||
		bundleResponse.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("versioned bundle response = %d, cache=%q", bundleResponse.Code, bundleResponse.Header().Get("Cache-Control"))
	}

	node := domain.Node{Capabilities: []string{domain.EdgeCapabilityNginxBundle}}
	instruction := server.nodeUpgradeInstruction(node)
	parsedInstallerURL, err := url.Parse(instruction.Installer.URL)
	if err != nil {
		t.Fatal(err)
	}
	installerRequest := httptest.NewRequest(http.MethodGet, parsedInstallerURL.Path, nil)
	installerResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(installerResponse, installerRequest)
	if installerResponse.Code != http.StatusOK || resourceSHA256(installerResponse.Body.String()) != instruction.Installer.SHA256 ||
		!strings.Contains(installerResponse.Body.String(), target.URL) {
		t.Fatalf("immutable installer response = %d, url=%s", installerResponse.Code, instruction.Installer.URL)
	}
	oldInstaller := installerResponse.Body.String()
	enrollmentNode, err := database.CreateNode("managed-nginx-edge", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	enrollmentRequest := httptest.NewRequest(http.MethodPost, "/api/nodes/"+enrollmentNode.ID+"/enrollment", nil)
	enrollmentRequest.SetPathValue("id", enrollmentNode.ID)
	enrollmentResponse := httptest.NewRecorder()
	server.createEnrollmentToken(enrollmentResponse, enrollmentRequest)
	if enrollmentResponse.Code != http.StatusCreated {
		t.Fatalf("enrollment response = %d: %s", enrollmentResponse.Code, enrollmentResponse.Body.String())
	}
	var enrollment map[string]any
	if err := json.NewDecoder(enrollmentResponse.Body).Decode(&enrollment); err != nil {
		t.Fatal(err)
	}
	expectedBootstrapURL := "https://control.example.test/downloads/nginx/" + target.SHA256 + "/install-edge.sh"
	installCommand, _ := enrollment["install_command"].(string)
	if !strings.Contains(installCommand, expectedBootstrapURL) {
		t.Fatalf("enrollment command does not use immutable installer %s: %s", expectedBootstrapURL, installCommand)
	}

	nextBundlePath := filepath.Join(t.TempDir(), nginxReleaseBundleName)
	writeTestNginxArchive(t, nextBundlePath, validTestNginxEntries("1.30.6"))
	nextMetadata, err := ResolveNginxBundle(nextBundlePath)
	if err != nil {
		t.Fatal(err)
	}
	nextInfo, err := os.Stat(nextBundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(nextBundlePath, manager.ArtifactPath(nextMetadata.SHA256)); err != nil {
		t.Fatal(err)
	}
	nextArtifact := domain.NginxArtifact{
		SHA256: nextMetadata.SHA256, Version: nextMetadata.Version, ReleaseTag: "nginx-v1.30.6",
		SourceURL: "https://downloads.example.test/next", OfficialSourceURL: "https://nginx.org/download/nginx-1.30.6.tar.gz",
		SourceSHA256: strings.Repeat("1", 64), BuildCommit: strings.Repeat("2", 40),
		SizeBytes: nextInfo.Size(), DownloadedAt: time.Now().UTC(),
	}
	if _, _, err := database.SaveNginxArtifactCandidate(nextArtifact); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := manager.Promote(nextArtifact.SHA256); err != nil || !changed {
		t.Fatalf("promote next candidate: changed=%v, err=%v", changed, err)
	}
	retiredInstallerResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(retiredInstallerResponse, httptest.NewRequest(http.MethodGet, parsedInstallerURL.Path, nil))
	if retiredInstallerResponse.Code != http.StatusOK || retiredInstallerResponse.Body.String() != oldInstaller ||
		resourceSHA256(retiredInstallerResponse.Body.String()) != instruction.Installer.SHA256 {
		t.Fatal("promoting a new target changed the retired target's installer")
	}
	legacyAfterPromotion := httptest.NewRecorder()
	server.Handler().ServeHTTP(legacyAfterPromotion, httptest.NewRequest(http.MethodGet, "/install-edge.sh", nil))
	if legacyAfterPromotion.Code != http.StatusOK || legacyAfterPromotion.Body.String() != legacyInstaller {
		t.Fatal("promoting a new target changed the legacy installer used by older queued tasks")
	}
	if err := os.Remove(manager.ArtifactPath(nextArtifact.SHA256)); err != nil {
		t.Fatal(err)
	}
	status, err := server.buildNginxArtifactStatus()
	if err != nil || status.ArtifactError == "" || status.Current.SHA256 != nextArtifact.SHA256 {
		t.Fatalf("missing current artifact status = %#v, err=%v", status, err)
	}
}

func TestNginxReleaseManifestRequiresStableOfficialSource(t *testing.T) {
	asset := nginxGitHubReleaseAsset{Name: nginxReleaseBundleName, Size: 123}
	manifest := nginxReleaseManifest{
		SchemaVersion: 1, Channel: "stable", NginxVersion: "1.30.5", Architecture: "amd64",
		Filename: nginxReleaseBundleName, SHA256: strings.Repeat("a", 64), SizeBytes: 123,
		OfficialSourceURL: "https://nginx.org/download/nginx-1.30.5.tar.gz",
		SourceSHA256:      strings.Repeat("b", 64), BuildCommit: strings.Repeat("c", 40),
	}
	if err := validateNginxReleaseManifest(manifest, "nginx-v1.30.5", asset); err != nil {
		t.Fatalf("valid stable manifest: %v", err)
	}
	manifest.Channel = "mainline"
	if err := validateNginxReleaseManifest(manifest, "nginx-v1.30.5", asset); err == nil {
		t.Fatal("mainline manifest was accepted")
	}
	manifest.Channel = "stable"
	manifest.OfficialSourceURL = "https://example.test/nginx.tar.gz"
	if err := validateNginxReleaseManifest(manifest, "nginx-v1.30.5", asset); err == nil {
		t.Fatal("non-official source URL was accepted")
	}
}

func TestNginxUpdateManagerRunChecksImmediatelyAndOnInterval(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	checks := make(chan struct{}, 4)
	client := &http.Client{Transport: nginxUpdateRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/repos/example/project/releases" {
			t.Fatalf("unexpected update request: %s", request.URL)
		}
		checks <- struct{}{}
		return nginxUpdateResponse(http.StatusOK, []byte("[]")), nil
	})}
	manager, err := NewNginxUpdateManager(NginxUpdateManagerConfig{
		Store: database, Directory: t.TempDir(), Repository: "example/project",
		GitHubAPIURL: "https://api.example.test", FallbackVersion: "1.30.4",
		Interval: 10 * time.Millisecond, Enabled: true, Client: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		manager.Run(ctx)
		close(done)
	}()
	for check := 0; check < 2; check++ {
		select {
		case <-checks:
		case <-time.After(time.Second):
			cancel()
			t.Fatal("managed Nginx checker did not run on schedule")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("managed Nginx checker did not stop after cancellation")
	}
}

func nginxUpdateResponse(status int, contents []byte) *http.Response {
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status), Header: make(http.Header),
		Body: io.NopCloser(bytes.NewReader(contents)),
	}
}
