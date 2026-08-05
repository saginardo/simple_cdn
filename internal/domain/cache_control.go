package domain

import (
	"errors"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

const (
	EdgeCapabilityCacheControl = "cache_control_v1"
	MaxCacheInvalidationRules  = 128
	MaxCacheWarmups            = 16
	MaxCacheWarmupPaths        = 100
	MaxDesiredCacheWarmups     = 4096
)

type CacheInvalidationScope string

const (
	CacheInvalidationFull   CacheInvalidationScope = "full"
	CacheInvalidationURL    CacheInvalidationScope = "url"
	CacheInvalidationPrefix CacheInvalidationScope = "prefix"
)

type CacheInvalidationRule struct {
	Scope      CacheInvalidationScope `json:"scope"`
	Value      string                 `json:"value"`
	Generation int64                  `json:"generation"`
}

type CacheWarmup struct {
	ID        string    `json:"id"`
	SiteID    string    `json:"site_id"`
	Host      string    `json:"host"`
	Paths     []string  `json:"paths"`
	CreatedAt time.Time `json:"created_at"`
}

func NormalizeCacheInvalidationTarget(scope CacheInvalidationScope, value string) (string, error) {
	value = strings.TrimSpace(value)
	if scope != CacheInvalidationURL && scope != CacheInvalidationPrefix {
		return "", errors.New("cache invalidation scope must be full, url, or prefix")
	}
	limit := 2048
	if scope == CacheInvalidationPrefix {
		limit = 1024
	}
	if value == "" || len(value) > limit || !strings.HasPrefix(value, "/") ||
		strings.ContainsAny(value, "\x00\r\n\t \"{};$\\") {
		return "", errors.New("cache invalidation target must be a safe absolute URL path")
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed == nil {
		return "", errors.New("cache invalidation target must be a clean public URL path")
	}
	cleanedPath := path.Clean(parsed.Path)
	cleanPath := parsed.Path == cleanedPath || (parsed.Path != "/" && parsed.Path == cleanedPath+"/")
	if parsed.Fragment != "" || parsed.Path == "" || !cleanPath ||
		parsed.Path == "/__cdn_health" || strings.HasPrefix(parsed.Path, "/_cdn/") {
		return "", errors.New("cache invalidation target must be a clean public URL path")
	}
	if scope == CacheInvalidationPrefix && (parsed.RawQuery != "" || parsed.Path != value || parsed.Path == "/") {
		return "", errors.New("cache prefix must be a clean path other than /")
	}
	return value, nil
}

func NormalizeCacheInvalidationRules(rules []CacheInvalidationRule) ([]CacheInvalidationRule, error) {
	if len(rules) > MaxCacheInvalidationRules {
		return nil, errors.New("too many cache invalidation rules")
	}
	result := make([]CacheInvalidationRule, 0, len(rules))
	seen := make(map[string]struct{}, len(rules))
	for _, rule := range rules {
		value, err := NormalizeCacheInvalidationTarget(rule.Scope, rule.Value)
		if err != nil || rule.Generation < 1 {
			return nil, errors.New("cache invalidation rule is invalid")
		}
		rule.Value = value
		key := string(rule.Scope) + "\x00" + value
		if _, found := seen[key]; found {
			return nil, errors.New("cache invalidation rules contain a duplicate target")
		}
		seen[key] = struct{}{}
		result = append(result, rule)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Generation < result[j].Generation })
	return result, nil
}

func NormalizeCacheWarmupPaths(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		normalized, err := NormalizeCacheInvalidationTarget(CacheInvalidationURL, value)
		if err != nil {
			return nil, errors.New("cache prewarm URL is invalid")
		}
		if _, found := seen[normalized]; found {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, normalized)
		if len(result) > MaxCacheWarmupPaths {
			return nil, errors.New("too many cache prewarm URLs")
		}
	}
	return result, nil
}

func NormalizeCacheWarmups(values []CacheWarmup) ([]CacheWarmup, error) {
	return normalizeCacheWarmups(values, MaxCacheWarmups)
}

func NormalizeDesiredCacheWarmups(values []CacheWarmup) ([]CacheWarmup, error) {
	return normalizeCacheWarmups(values, MaxDesiredCacheWarmups)
}

func normalizeCacheWarmups(values []CacheWarmup, limit int) ([]CacheWarmup, error) {
	if len(values) > limit {
		return nil, errors.New("too many cache prewarm jobs")
	}
	result := make([]CacheWarmup, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value.ID = strings.TrimSpace(value.ID)
		value.SiteID = strings.TrimSpace(value.SiteID)
		value.Host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value.Host), "."))
		paths, err := NormalizeCacheWarmupPaths(value.Paths)
		if value.ID == "" || value.SiteID == "" || !ValidHostname(value.Host) || value.CreatedAt.IsZero() || err != nil || len(paths) == 0 {
			return nil, errors.New("cache prewarm job is invalid")
		}
		if _, found := seen[value.ID]; found {
			return nil, errors.New("cache prewarm jobs contain a duplicate ID")
		}
		seen[value.ID] = struct{}{}
		value.Paths = paths
		result = append(result, value)
	}
	return result, nil
}
