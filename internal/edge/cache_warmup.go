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

	"simple_cdn/internal/domain"
)

const (
	cacheWarmupTimeout        = 45 * time.Second
	cacheWarmupResponseLimit  = int64(512 << 20)
	completedWarmupStateLimit = 256 << 10
)

func (a *Agent) runCacheWarmups(warmups []domain.CacheWarmup) (string, error) {
	completed, storedOrder, err := a.loadCompletedCacheWarmups()
	if err != nil {
		return "", err
	}
	if len(warmups) == 0 {
		if len(storedOrder) == 0 {
			return "", nil
		}
		return "", a.saveCompletedCacheWarmups([]string{})
	}
	attempted := 0
	succeeded := 0
	var failures []error
	for _, warmup := range warmups {
		if _, found := completed[warmup.ID]; found {
			continue
		}
		attempted++
		ctx, cancel := context.WithTimeout(context.Background(), cacheWarmupTimeout)
		warmer := a.Config.CacheWarmer
		if warmer == nil {
			warmer = a.warmCache
		}
		err := warmer(ctx, warmup)
		cancel()
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", warmup.Host, err))
		} else {
			succeeded++
		}
		completed[warmup.ID] = struct{}{}
	}
	order := make([]string, 0, len(warmups))
	for _, warmup := range warmups {
		if _, found := completed[warmup.ID]; found {
			order = append(order, warmup.ID)
		}
	}
	if attempted == 0 {
		if slices.Equal(storedOrder, order) {
			return "", nil
		}
		return "", a.saveCompletedCacheWarmups(order)
	}
	if err := a.saveCompletedCacheWarmups(order); err != nil {
		failures = append(failures, err)
	}
	detail := fmt.Sprintf("cache prewarm completed %d of %d job(s)", succeeded, attempted)
	return detail, errors.Join(failures...)
}

func (a *Agent) warmCache(ctx context.Context, warmup domain.CacheWarmup) error {
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
		endpoint := (&url.URL{Scheme: "https", Host: warmup.Host, Path: path}).String()
		if parsed, err := url.ParseRequestURI(path); err == nil {
			endpoint = (&url.URL{Scheme: "https", Host: warmup.Host, Path: parsed.Path, RawQuery: parsed.RawQuery}).String()
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Accept-Encoding", "identity")
		request.Header.Set("X-CDN-Prewarm", "1")
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("GET %s: %w", path, err)
		}
		read, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, cacheWarmupResponseLimit+1))
		closeErr := response.Body.Close()
		if readErr != nil {
			return fmt.Errorf("GET %s: %w", path, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("GET %s: %w", path, closeErr)
		}
		if read > cacheWarmupResponseLimit {
			return fmt.Errorf("GET %s exceeded the prewarm response limit", path)
		}
		if response.StatusCode < 200 || response.StatusCode >= 400 {
			return fmt.Errorf("GET %s returned %s", path, response.Status)
		}
	}
	return nil
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
