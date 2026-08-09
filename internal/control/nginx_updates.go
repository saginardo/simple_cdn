package control

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

const (
	nginxReleaseManifestName = "cdn-nginx-linux-amd64.json"
	nginxReleaseBundleName   = "cdn-nginx-linux-amd64.tar.gz"
	nginxReleaseListLimit    = 30
	nginxManifestLimit       = 64 << 10
)

var (
	nginxReleaseTagPattern  = regexp.MustCompile(`^nginx-v([0-9]+\.[0-9]+\.[0-9]+)$`)
	githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

type NginxUpdateManagerConfig struct {
	Store           *store.Store
	Directory       string
	Repository      string
	GitHubAPIURL    string
	GitHubToken     string
	FallbackVersion string
	Interval        time.Duration
	Enabled         bool
	Client          *http.Client
	Logger          *slog.Logger
}

type NginxUpdateRuntimeStatus struct {
	Enabled              bool       `json:"enabled"`
	Repository           string     `json:"repository"`
	CheckIntervalSeconds int64      `json:"check_interval_seconds"`
	Checking             bool       `json:"checking"`
	LastCheckedAt        *time.Time `json:"last_checked_at,omitempty"`
	LastError            string     `json:"last_error,omitempty"`
}

type NginxUpdateManager struct {
	store           *store.Store
	directory       string
	repository      string
	githubAPIURL    string
	githubToken     string
	fallbackVersion string
	interval        time.Duration
	enabled         bool
	client          *http.Client
	logger          *slog.Logger

	checkMu sync.Mutex
	stateMu sync.RWMutex
	state   NginxUpdateRuntimeStatus
}

type nginxGitHubRelease struct {
	TagName    string                    `json:"tag_name"`
	Draft      bool                      `json:"draft"`
	Prerelease bool                      `json:"prerelease"`
	Assets     []nginxGitHubReleaseAsset `json:"assets"`
}

type nginxGitHubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type nginxReleaseManifest struct {
	SchemaVersion     int    `json:"schema_version"`
	Channel           string `json:"channel"`
	NginxVersion      string `json:"nginx_version"`
	Architecture      string `json:"architecture"`
	Filename          string `json:"filename"`
	SHA256            string `json:"sha256"`
	SizeBytes         int64  `json:"size_bytes"`
	OfficialSourceURL string `json:"official_source_url"`
	SourceSHA256      string `json:"source_sha256"`
	BuildCommit       string `json:"build_commit"`
}

func NewNginxUpdateManager(config NginxUpdateManagerConfig) (*NginxUpdateManager, error) {
	config.Directory = strings.TrimSpace(config.Directory)
	config.Repository = strings.TrimSpace(config.Repository)
	config.GitHubAPIURL = strings.TrimRight(strings.TrimSpace(config.GitHubAPIURL), "/")
	config.GitHubToken = strings.TrimSpace(config.GitHubToken)
	config.FallbackVersion = strings.TrimSpace(config.FallbackVersion)
	if config.Store == nil {
		return nil, errors.New("Nginx update store is required")
	}
	if config.Directory == "" {
		return nil, errors.New("Nginx artifact directory is required")
	}
	if !githubRepositoryPattern.MatchString(config.Repository) {
		return nil, errors.New("NGINX_UPDATE_GITHUB_REPOSITORY must be owner/repository")
	}
	if config.GitHubAPIURL == "" {
		config.GitHubAPIURL = "https://api.github.com"
	}
	parsedAPI, err := url.Parse(config.GitHubAPIURL)
	if err != nil || parsedAPI.Scheme != "https" || parsedAPI.Host == "" || parsedAPI.User != nil || parsedAPI.RawQuery != "" || parsedAPI.Fragment != "" {
		return nil, errors.New("NGINX_UPDATE_GITHUB_API_URL must be an HTTPS origin")
	}
	if config.Interval <= 0 {
		return nil, errors.New("NGINX_UPDATE_CHECK_INTERVAL must be positive")
	}
	if config.FallbackVersion != "" && !nginxVersionPattern.MatchString(config.FallbackVersion) {
		return nil, errors.New("fallback Nginx version is invalid")
	}
	if err := os.MkdirAll(config.Directory, 0o750); err != nil {
		return nil, fmt.Errorf("create Nginx artifact directory: %w", err)
	}
	client := config.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Minute}
	}
	manager := &NginxUpdateManager{
		store: config.Store, directory: config.Directory, repository: config.Repository,
		githubAPIURL: config.GitHubAPIURL, githubToken: config.GitHubToken,
		fallbackVersion: config.FallbackVersion, interval: config.Interval,
		enabled: config.Enabled, client: client, logger: config.Logger,
	}
	manager.state = NginxUpdateRuntimeStatus{
		Enabled: config.Enabled, Repository: config.Repository,
		CheckIntervalSeconds: int64(config.Interval / time.Second),
	}
	return manager, nil
}

func (m *NginxUpdateManager) Run(ctx context.Context) {
	if !m.enabled {
		<-ctx.Done()
		return
	}
	for {
		if err := m.Check(ctx); err != nil && ctx.Err() == nil && m.logger != nil {
			m.logger.Warn("managed Nginx update check failed", "error", err)
		}
		timer := time.NewTimer(m.interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

func (m *NginxUpdateManager) Status() NginxUpdateRuntimeStatus {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.state
}

func (m *NginxUpdateManager) Check(ctx context.Context) (err error) {
	if !m.enabled {
		return errors.New("managed Nginx update checks are disabled")
	}
	m.checkMu.Lock()
	defer m.checkMu.Unlock()
	m.stateMu.Lock()
	m.state.Checking = true
	m.stateMu.Unlock()
	defer func() {
		checkedAt := time.Now().UTC()
		lastError := ""
		if err != nil && !errors.Is(err, context.Canceled) {
			lastError = err.Error()
			if len(lastError) > 1000 {
				lastError = lastError[:1000]
			}
		}
		m.stateMu.Lock()
		m.state.Checking = false
		m.state.LastCheckedAt = &checkedAt
		m.state.LastError = lastError
		m.stateMu.Unlock()
	}()

	releases, err := m.githubReleases(ctx)
	if err != nil {
		return err
	}
	release, version, found := newestStableNginxRelease(releases)
	if !found {
		return nil
	}
	currentVersion := m.fallbackVersion
	if current, currentErr := m.store.CurrentNginxArtifact(); currentErr == nil {
		currentVersion = current.Version
	} else if !errors.Is(currentErr, store.ErrNotFound) {
		return fmt.Errorf("read current Nginx artifact: %w", currentErr)
	}
	if currentVersion != "" && compareNginxVersions(version, currentVersion) <= 0 {
		return nil
	}
	if existing, existingErr := m.store.NginxArtifactByVersion(version); existingErr == nil {
		if existing.State == domain.NginxArtifactCandidate && m.ArtifactReady(existing) {
			return m.ensureCandidateMessage(existing)
		}
	} else if !errors.Is(existingErr, store.ErrNotFound) {
		return fmt.Errorf("read Nginx artifact catalog: %w", existingErr)
	}
	manifestAsset, bundleAsset, err := nginxReleaseAssets(release)
	if err != nil {
		return fmt.Errorf("release %s: %w", release.TagName, err)
	}
	manifest, err := m.downloadManifest(ctx, manifestAsset.BrowserDownloadURL)
	if err != nil {
		return fmt.Errorf("download release %s manifest: %w", release.TagName, err)
	}
	if err := validateNginxReleaseManifest(manifest, release.TagName, bundleAsset); err != nil {
		return fmt.Errorf("validate release %s manifest: %w", release.TagName, err)
	}
	pathname, metadata, size, err := m.downloadBundle(ctx, bundleAsset.BrowserDownloadURL, manifest)
	if err != nil {
		return fmt.Errorf("download release %s bundle: %w", release.TagName, err)
	}
	artifact := domain.NginxArtifact{
		SHA256: metadata.SHA256, Version: metadata.Version, ReleaseTag: release.TagName,
		SourceURL: bundleAsset.BrowserDownloadURL, OfficialSourceURL: manifest.OfficialSourceURL,
		SourceSHA256: manifest.SourceSHA256, BuildCommit: manifest.BuildCommit,
		SizeBytes: size, DownloadedAt: time.Now().UTC(),
	}
	saved, _, err := m.store.SaveNginxArtifactCandidate(artifact)
	if err != nil {
		return err
	}
	if saved.SHA256 != metadata.SHA256 {
		return errors.New("cataloged Nginx artifact SHA-256 does not match downloaded bundle")
	}
	if err := m.ensureCandidateMessage(saved); err != nil {
		return err
	}
	if m.logger != nil {
		m.logger.Info("managed Nginx candidate downloaded", "version", saved.Version, "sha256", saved.SHA256, "path", pathname)
	}
	return nil
}

func (m *NginxUpdateManager) ensureCandidateMessage(artifact domain.NginxArtifact) error {
	_, _, err := m.store.CreateMessageOnce(domain.Message{
		Severity: domain.MessageWarning, Category: "nginx_update",
		Title:      "发现可用的 Nginx 稳定版",
		Body:       "Nginx " + artifact.Version + " 已完成构建校验并下载到主控，等待管理员设为节点升级目标。",
		SourceType: "nginx_artifact", SourceID: artifact.SHA256, SourceStatus: "candidate",
		CreatedAt: artifact.DownloadedAt,
	})
	return err
}

func (m *NginxUpdateManager) ArtifactPath(sha256 string) string {
	sha256 = strings.ToLower(strings.TrimSpace(sha256))
	if !validSHA256Digest(sha256) {
		return ""
	}
	return filepath.Join(m.directory, sha256+".tar.gz")
}

func (m *NginxUpdateManager) ArtifactReady(artifact domain.NginxArtifact) bool {
	pathname := m.ArtifactPath(artifact.SHA256)
	info, err := os.Stat(pathname)
	return err == nil && info.Mode().IsRegular() && info.Size() == artifact.SizeBytes && info.Size() > 0
}

func (m *NginxUpdateManager) Promote(sha256 string) (domain.NginxArtifact, bool, error) {
	artifact, err := m.store.NginxArtifactBySHA(sha256)
	if err != nil {
		return domain.NginxArtifact{}, false, err
	}
	if !m.ArtifactReady(artifact) {
		return domain.NginxArtifact{}, false, errors.New("downloaded Nginx artifact is missing or has the wrong size")
	}
	return m.store.PromoteNginxArtifact(artifact.SHA256)
}

func (m *NginxUpdateManager) githubReleases(ctx context.Context) ([]nginxGitHubRelease, error) {
	parts := strings.Split(m.repository, "/")
	endpoint := m.githubAPIURL + "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) +
		"/releases?per_page=" + strconv.Itoa(nginxReleaseListLimit)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "simple-cdn-control")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if m.githubToken != "" {
		request.Header.Set("Authorization", "Bearer "+m.githubToken)
	}
	response, err := m.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("GitHub releases API returned %s: %s", response.Status, strings.TrimSpace(string(detail)))
	}
	var releases []nginxGitHubRelease
	decoder := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
	if err := decoder.Decode(&releases); err != nil {
		return nil, err
	}
	return releases, nil
}

func (m *NginxUpdateManager) downloadManifest(ctx context.Context, sourceURL string) (nginxReleaseManifest, error) {
	contents, err := m.downloadBytes(ctx, sourceURL, nginxManifestLimit)
	if err != nil {
		return nginxReleaseManifest{}, err
	}
	var manifest nginxReleaseManifest
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nginxReleaseManifest{}, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nginxReleaseManifest{}, errors.New("manifest has trailing data")
	}
	return manifest, nil
}

func (m *NginxUpdateManager) downloadBytes(ctx context.Context, sourceURL string, limit int64) ([]byte, error) {
	if !validHTTPSURL(sourceURL) {
		return nil, errors.New("release asset URL must use HTTPS")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", "simple-cdn-control")
	if m.githubToken != "" {
		request.Header.Set("Authorization", "Bearer "+m.githubToken)
	}
	response, err := m.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release asset returned %s", response.Status)
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, fmt.Errorf("release asset exceeds %d bytes", limit)
	}
	return contents, nil
}

func (m *NginxUpdateManager) downloadBundle(ctx context.Context, sourceURL string, manifest nginxReleaseManifest) (string, NginxBundleMetadata, int64, error) {
	if !validHTTPSURL(sourceURL) {
		return "", NginxBundleMetadata{}, 0, errors.New("release asset URL must use HTTPS")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return "", NginxBundleMetadata{}, 0, err
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", "simple-cdn-control")
	if m.githubToken != "" {
		request.Header.Set("Authorization", "Bearer "+m.githubToken)
	}
	response, err := m.client.Do(request)
	if err != nil {
		return "", NginxBundleMetadata{}, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", NginxBundleMetadata{}, 0, fmt.Errorf("release asset returned %s", response.Status)
	}
	temporary, err := os.CreateTemp(m.directory, ".nginx-download-*.tmp")
	if err != nil {
		return "", NginxBundleMetadata{}, 0, err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	written, copyErr := io.Copy(temporary, io.LimitReader(response.Body, nginxBundleLimit+1))
	closeErr := temporary.Close()
	if copyErr != nil {
		return "", NginxBundleMetadata{}, 0, copyErr
	}
	if closeErr != nil {
		return "", NginxBundleMetadata{}, 0, closeErr
	}
	if written <= 0 || written > nginxBundleLimit {
		return "", NginxBundleMetadata{}, 0, errors.New("Nginx bundle must be between 1 byte and 128 MiB")
	}
	if manifest.SizeBytes != written {
		return "", NginxBundleMetadata{}, 0, errors.New("Nginx bundle size does not match release manifest")
	}
	metadata, err := ResolveNginxBundle(temporaryName)
	if err != nil {
		return "", NginxBundleMetadata{}, 0, err
	}
	if metadata.Version != manifest.NginxVersion || !strings.EqualFold(metadata.SHA256, manifest.SHA256) {
		return "", NginxBundleMetadata{}, 0, errors.New("Nginx bundle contents do not match release manifest")
	}
	destination := m.ArtifactPath(metadata.SHA256)
	if destination == "" {
		return "", NginxBundleMetadata{}, 0, errors.New("Nginx bundle has an invalid SHA-256")
	}
	if info, statErr := os.Stat(destination); statErr == nil {
		if !info.Mode().IsRegular() || info.Size() != written {
			return "", NginxBundleMetadata{}, 0, errors.New("existing Nginx artifact path is invalid")
		}
		existingMetadata, resolveErr := ResolveNginxBundle(destination)
		if resolveErr != nil || existingMetadata != metadata {
			return "", NginxBundleMetadata{}, 0, errors.New("existing Nginx artifact content does not match its content address")
		}
		return destination, metadata, written, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", NginxBundleMetadata{}, 0, statErr
	}
	if err := os.Chmod(temporaryName, 0o640); err != nil {
		return "", NginxBundleMetadata{}, 0, err
	}
	if err := os.Rename(temporaryName, destination); err != nil {
		return "", NginxBundleMetadata{}, 0, err
	}
	return destination, metadata, written, nil
}

func nginxReleaseAssets(release nginxGitHubRelease) (nginxGitHubReleaseAsset, nginxGitHubReleaseAsset, error) {
	var manifest, bundle nginxGitHubReleaseAsset
	for _, asset := range release.Assets {
		switch asset.Name {
		case nginxReleaseManifestName:
			manifest = asset
		case nginxReleaseBundleName:
			bundle = asset
		}
	}
	if manifest.BrowserDownloadURL == "" || bundle.BrowserDownloadURL == "" {
		return manifest, bundle, errors.New("required amd64 manifest or bundle is missing")
	}
	return manifest, bundle, nil
}

func newestStableNginxRelease(releases []nginxGitHubRelease) (nginxGitHubRelease, string, bool) {
	var selected nginxGitHubRelease
	version := ""
	for _, release := range releases {
		if release.Draft || release.Prerelease {
			continue
		}
		match := nginxReleaseTagPattern.FindStringSubmatch(strings.TrimSpace(release.TagName))
		if len(match) != 2 || (version != "" && compareNginxVersions(match[1], version) <= 0) {
			continue
		}
		selected, version = release, match[1]
	}
	return selected, version, version != ""
}

func validateNginxReleaseManifest(manifest nginxReleaseManifest, releaseTag string, asset nginxGitHubReleaseAsset) error {
	manifest.SHA256 = strings.ToLower(strings.TrimSpace(manifest.SHA256))
	manifest.SourceSHA256 = strings.ToLower(strings.TrimSpace(manifest.SourceSHA256))
	manifest.BuildCommit = strings.ToLower(strings.TrimSpace(manifest.BuildCommit))
	match := nginxReleaseTagPattern.FindStringSubmatch(strings.TrimSpace(releaseTag))
	if len(match) != 2 || manifest.SchemaVersion != 1 || manifest.Channel != "stable" ||
		manifest.NginxVersion != match[1] || manifest.Architecture != "amd64" || manifest.Filename != nginxReleaseBundleName ||
		!validSHA256Digest(manifest.SHA256) || !validSHA256Digest(manifest.SourceSHA256) || manifest.SizeBytes <= 0 || manifest.SizeBytes > nginxBundleLimit ||
		asset.Size != manifest.SizeBytes || len(manifest.BuildCommit) != 40 {
		return errors.New("manifest fields are invalid or inconsistent")
	}
	if _, err := hex.DecodeString(manifest.BuildCommit); err != nil {
		return errors.New("manifest build commit is invalid")
	}
	expectedSourceURL := "https://nginx.org/download/nginx-" + manifest.NginxVersion + ".tar.gz"
	if manifest.OfficialSourceURL != expectedSourceURL {
		return errors.New("manifest does not reference the official nginx.org source archive")
	}
	return nil
}

func compareNginxVersions(left, right string) int {
	leftParts := strings.Split(left, ".")
	rightParts := strings.Split(right, ".")
	if len(leftParts) != 3 || len(rightParts) != 3 {
		return strings.Compare(left, right)
	}
	for index := 0; index < 3; index++ {
		leftValue, leftErr := strconv.Atoi(leftParts[index])
		rightValue, rightErr := strconv.Atoi(rightParts[index])
		if leftErr != nil || rightErr != nil {
			return strings.Compare(left, right)
		}
		if leftValue < rightValue {
			return -1
		}
		if leftValue > rightValue {
			return 1
		}
	}
	return 0
}
