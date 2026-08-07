package edge

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"simple_cdn/internal/domain"
)

const (
	cacheWarmupTimeout           = 45 * time.Second
	cacheWarmupResponseLimit     = int64(512 << 20)
	completedWarmupStateLimit    = 256 << 10
	pendingWarmupResultFileLimit = 256 << 10
)

func (a *Agent) runCacheWarmups(warmups []domain.CacheWarmup) (string, []domain.CacheWarmupResult, error) {
	completed, storedOrder, err := a.loadCompletedCacheWarmups()
	if err != nil {
		return "", nil, err
	}
	if len(warmups) == 0 {
		if len(storedOrder) == 0 {
			return "", nil, nil
		}
		return "", nil, a.saveCompletedCacheWarmups([]string{})
	}

	results := make([]domain.CacheWarmupResult, 0)
	var failures []error
	for _, warmup := range warmups {
		if _, found := completed[warmup.ID]; found {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), cacheWarmupTimeout)
		result := a.executeCacheWarmup(ctx, warmup)
		cancel()
		results = append(results, result)
		if result.Status != domain.CacheWarmupSucceeded {
			failures = append(failures, fmt.Errorf("%s: %d of %d URL(s) failed", warmup.Host,
				result.AttemptedURLs-result.SucceededURLs, result.AttemptedURLs))
		}
	}
	if len(results) == 0 {
		order := completedWarmupOrder(warmups, completed)
		if slices.Equal(storedOrder, order) {
			return "", nil, nil
		}
		return "", nil, a.saveCompletedCacheWarmups(order)
	}

	if err := a.queueCacheWarmupResults(results); err != nil {
		failures = append(failures, err)
	} else {
		for _, result := range results {
			completed[result.WarmupID] = struct{}{}
		}
	}
	order := completedWarmupOrder(warmups, completed)
	if err := a.saveCompletedCacheWarmups(order); err != nil {
		failures = append(failures, err)
	}
	succeeded := 0
	for _, result := range results {
		if result.Status == domain.CacheWarmupSucceeded {
			succeeded++
		}
	}
	detail := fmt.Sprintf("cache prewarm completed %d of %d job(s)", succeeded, len(results))
	return detail, results, errors.Join(failures...)
}

func completedWarmupOrder(warmups []domain.CacheWarmup, completed map[string]struct{}) []string {
	order := make([]string, 0, len(warmups))
	for _, warmup := range warmups {
		if _, found := completed[warmup.ID]; found {
			order = append(order, warmup.ID)
		}
	}
	return order
}

func (a *Agent) executeCacheWarmup(ctx context.Context, warmup domain.CacheWarmup) domain.CacheWarmupResult {
	if a.Config.CacheWarmer == nil {
		return a.warmCacheResult(ctx, warmup)
	}
	result := domain.CacheWarmupResult{
		WarmupID: warmup.ID, SiteID: warmup.SiteID, AttemptedURLs: len(warmup.Paths), CompletedAt: time.Now().UTC(),
	}
	if err := a.Config.CacheWarmer(ctx, warmup); err != nil {
		result.Status = domain.CacheWarmupFailed
		result.Failures = []domain.CacheWarmupFailure{{Detail: cacheWarmupFailureDetail(err)}}
		return result
	}
	result.Status = domain.CacheWarmupSucceeded
	result.SucceededURLs = len(warmup.Paths)
	return result
}

func (a *Agent) warmCache(ctx context.Context, warmup domain.CacheWarmup) error {
	result := a.warmCacheResult(ctx, warmup)
	if result.Status == domain.CacheWarmupSucceeded {
		return nil
	}
	failures := make([]error, 0, len(result.Failures))
	for _, failure := range result.Failures {
		if failure.Path == "" {
			failures = append(failures, errors.New(failure.Detail))
		} else {
			failures = append(failures, fmt.Errorf("GET %s: %s", failure.Path, failure.Detail))
		}
	}
	return errors.Join(failures...)
}

func (a *Agent) warmCacheResult(ctx context.Context, warmup domain.CacheWarmup) domain.CacheWarmupResult {
	result := domain.CacheWarmupResult{
		WarmupID: warmup.ID, SiteID: warmup.SiteID, AttemptedURLs: len(warmup.Paths), CompletedAt: time.Now().UTC(),
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 15 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", "127.0.0.1:443")
		},
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		DisableCompression:    true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for _, path := range warmup.Paths {
		if err := warmCachePath(ctx, client, warmup.Host, path); err != nil {
			result.Failures = append(result.Failures, domain.CacheWarmupFailure{Path: path, Detail: cacheWarmupFailureDetail(err)})
			continue
		}
		result.SucceededURLs++
	}
	result.CompletedAt = time.Now().UTC()
	switch {
	case result.SucceededURLs == result.AttemptedURLs:
		result.Status = domain.CacheWarmupSucceeded
	case result.SucceededURLs == 0:
		result.Status = domain.CacheWarmupFailed
	default:
		result.Status = domain.CacheWarmupPartial
	}
	return result
}

func warmCachePath(ctx context.Context, client *http.Client, host, path string) error {
	endpoint := (&url.URL{Scheme: "https", Host: host, Path: path}).String()
	if parsed, err := url.ParseRequestURI(path); err == nil {
		endpoint = (&url.URL{Scheme: "https", Host: host, Path: parsed.Path, RawQuery: parsed.RawQuery}).String()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("X-CDN-Prewarm", "1")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	read, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, cacheWarmupResponseLimit+1))
	closeErr := response.Body.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if read > cacheWarmupResponseLimit {
		return errors.New("response exceeded the prewarm response limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return fmt.Errorf("origin returned %s", response.Status)
	}
	return nil
}

func cacheWarmupFailureDetail(err error) string {
	detail := strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ", "\x00", " ").Replace(err.Error()))
	detail = strings.ToValidUTF8(detail, "?")
	if detail == "" {
		detail = "cache prewarm failed"
	}
	if len(detail) > 1024 {
		detail = detail[:1024]
		for !utf8.ValidString(detail) {
			detail = detail[:len(detail)-1]
		}
	}
	return detail
}

func (a *Agent) loadCompletedCacheWarmups() (map[string]struct{}, []string, error) {
	contents, err := os.ReadFile(a.completedCacheWarmupsPath())
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]struct{}), nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read completed cache prewarm jobs: %w", err)
	}
	var order []string
	if len(contents) > completedWarmupStateLimit || json.Unmarshal(contents, &order) != nil {
		return nil, nil, errors.New("completed cache prewarm state is invalid")
	}
	completed := make(map[string]struct{}, len(order))
	for _, id := range order {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, nil, errors.New("completed cache prewarm state is invalid")
		}
		completed[id] = struct{}{}
	}
	return completed, order, nil
}

func (a *Agent) saveCompletedCacheWarmups(order []string) error {
	contents, err := json.Marshal(order)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(a.Config.StateDir, 0o700); err != nil {
		return err
	}
	if err := atomicWriteFile(a.completedCacheWarmupsPath(), append(contents, '\n'), 0o600); err != nil {
		return fmt.Errorf("write completed cache prewarm jobs: %w", err)
	}
	return nil
}

func (a *Agent) completedCacheWarmupsPath() string {
	return filepath.Join(a.Config.StateDir, "cache-warmups.json")
}

func (a *Agent) pendingCacheWarmupResults() ([]domain.CacheWarmupResult, error) {
	a.cacheWarmupMu.Lock()
	defer a.cacheWarmupMu.Unlock()
	return a.loadPendingCacheWarmupResultsLocked()
}

func (a *Agent) queueCacheWarmupResults(results []domain.CacheWarmupResult) error {
	if len(results) == 0 {
		return nil
	}
	a.cacheWarmupMu.Lock()
	defer a.cacheWarmupMu.Unlock()
	current, err := a.loadPendingCacheWarmupResultsLocked()
	if err != nil {
		return err
	}
	byID := make(map[string]int, len(current))
	for index := range current {
		byID[current[index].WarmupID] = index
	}
	for _, result := range results {
		if index, found := byID[result.WarmupID]; found {
			current[index] = result
			continue
		}
		byID[result.WarmupID] = len(current)
		current = append(current, result)
	}
	return a.savePendingCacheWarmupResultsLocked(current)
}

func (a *Agent) acknowledgeCacheWarmupResults(sent []domain.CacheWarmupResult) error {
	if len(sent) == 0 {
		return nil
	}
	a.cacheWarmupMu.Lock()
	defer a.cacheWarmupMu.Unlock()
	current, err := a.loadPendingCacheWarmupResultsLocked()
	if err != nil {
		return err
	}
	acknowledged := make(map[string]time.Time, len(sent))
	for _, result := range sent {
		acknowledged[result.WarmupID] = result.CompletedAt
	}
	remaining := current[:0]
	for _, result := range current {
		completedAt, found := acknowledged[result.WarmupID]
		if found && result.CompletedAt.Equal(completedAt) {
			continue
		}
		remaining = append(remaining, result)
	}
	return a.savePendingCacheWarmupResultsLocked(remaining)
}

func (a *Agent) loadPendingCacheWarmupResultsLocked() ([]domain.CacheWarmupResult, error) {
	contents, err := os.ReadFile(a.pendingCacheWarmupResultsPath())
	if errors.Is(err, os.ErrNotExist) {
		return []domain.CacheWarmupResult{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pending cache prewarm results: %w", err)
	}
	var results []domain.CacheWarmupResult
	if len(contents) > pendingWarmupResultFileLimit || json.Unmarshal(contents, &results) != nil {
		return nil, errors.New("pending cache prewarm result state is invalid")
	}
	results, err = domain.NormalizeCacheWarmupResults(results)
	if err != nil {
		return nil, fmt.Errorf("pending cache prewarm result state is invalid: %w", err)
	}
	return results, nil
}

func (a *Agent) savePendingCacheWarmupResultsLocked(results []domain.CacheWarmupResult) error {
	results, err := domain.NormalizeCacheWarmupResults(results)
	if err != nil {
		return err
	}
	contents, err := json.Marshal(results)
	if err != nil {
		return err
	}
	if len(contents) > pendingWarmupResultFileLimit {
		return errors.New("pending cache prewarm result state exceeds its size limit")
	}
	if err := os.MkdirAll(a.Config.StateDir, 0o700); err != nil {
		return err
	}
	if err := atomicWriteFile(a.pendingCacheWarmupResultsPath(), append(contents, '\n'), 0o600); err != nil {
		return fmt.Errorf("write pending cache prewarm results: %w", err)
	}
	return nil
}

func (a *Agent) pendingCacheWarmupResultsPath() string {
	return filepath.Join(a.Config.StateDir, "cache-warmup-results.json")
}
