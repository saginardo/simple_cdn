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
	EdgeCapabilityCacheControl       = "cache_control_v1"
	EdgeCapabilityCacheWarmupResults = "cache_warmup_results_v1"
	MaxCacheInvalidationRules        = 128
	MaxCacheWarmups                  = 16
	MaxCacheWarmupPaths              = 100
	MaxDesiredCacheWarmups           = 4096
	MaxCacheWarmupResults            = 128
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

type CacheOperationKind string

const (
	CacheOperationInvalidate   CacheOperationKind = "invalidate"
	CacheOperationPrewarmRetry CacheOperationKind = "prewarm_retry"
)

type CacheOperationStatus string

const (
	CacheOperationQueued    CacheOperationStatus = "queued"
	CacheOperationApplying  CacheOperationStatus = "applying"
	CacheOperationSucceeded CacheOperationStatus = "succeeded"
	CacheOperationPartial   CacheOperationStatus = "partial"
	CacheOperationFailed    CacheOperationStatus = "failed"
)

type CacheConfigurationStatus string

const (
	CacheConfigurationNotTargeted CacheConfigurationStatus = "not_targeted"
	CacheConfigurationPending     CacheConfigurationStatus = "pending"
	CacheConfigurationSucceeded   CacheConfigurationStatus = "succeeded"
	CacheConfigurationFailed      CacheConfigurationStatus = "failed"
	CacheConfigurationTimedOut    CacheConfigurationStatus = "timed_out"
)

type CacheWarmupStatus string

const (
	CacheWarmupNotRequested CacheWarmupStatus = "not_requested"
	CacheWarmupNotTargeted  CacheWarmupStatus = "not_targeted"
	CacheWarmupPending      CacheWarmupStatus = "pending"
	CacheWarmupSucceeded    CacheWarmupStatus = "succeeded"
	CacheWarmupPartial      CacheWarmupStatus = "partial"
	CacheWarmupFailed       CacheWarmupStatus = "failed"
	CacheWarmupUnreported   CacheWarmupStatus = "unreported"
	CacheWarmupUnsupported  CacheWarmupStatus = "unsupported"
	CacheWarmupSkipped      CacheWarmupStatus = "skipped"
)

type CacheWarmupFailure struct {
	Path   string `json:"path,omitempty"`
	Detail string `json:"detail"`
}

// CacheWarmupResult is durably queued by an edge until the controller accepts
// it. WarmupID is also the cache operation ID for controller-created jobs.
type CacheWarmupResult struct {
	WarmupID      string               `json:"warmup_id"`
	SiteID        string               `json:"site_id"`
	Status        CacheWarmupStatus    `json:"status"`
	AttemptedURLs int                  `json:"attempted_urls"`
	SucceededURLs int                  `json:"succeeded_urls"`
	Failures      []CacheWarmupFailure `json:"failures,omitempty"`
	CompletedAt   time.Time            `json:"completed_at"`
}

type CacheOperationNode struct {
	NodeID              string                   `json:"node_id"`
	NodeName            string                   `json:"node_name"`
	TargetVersion       int64                    `json:"target_version,omitempty"`
	ConfigurationStatus CacheConfigurationStatus `json:"configuration_status"`
	WarmupStatus        CacheWarmupStatus        `json:"warmup_status"`
	AttemptedURLs       int                      `json:"attempted_urls"`
	SucceededURLs       int                      `json:"succeeded_urls"`
	Failures            []CacheWarmupFailure     `json:"failures"`
	ReportedAt          *time.Time               `json:"reported_at,omitempty"`
}

type CacheOperation struct {
	ID              string                 `json:"id"`
	SiteID          string                 `json:"site_id"`
	SiteName        string                 `json:"site_name"`
	Kind            CacheOperationKind     `json:"kind"`
	RetryOfID       string                 `json:"retry_of_id,omitempty"`
	PublishTaskID   string                 `json:"publish_task_id,omitempty"`
	Scope           CacheInvalidationScope `json:"scope"`
	Target          string                 `json:"target,omitempty"`
	PrewarmPaths    []string               `json:"prewarm_paths"`
	CacheGeneration int64                  `json:"cache_generation"`
	ConfigVersion   int64                  `json:"config_version"`
	Status          CacheOperationStatus   `json:"status"`
	Detail          string                 `json:"detail,omitempty"`
	Actor           string                 `json:"actor,omitempty"`
	RemoteAddr      string                 `json:"remote_addr,omitempty"`
	Nodes           []CacheOperationNode   `json:"nodes"`
	CreatedAt       time.Time              `json:"created_at"`
	UpdatedAt       time.Time              `json:"updated_at"`
	CompletedAt     *time.Time             `json:"completed_at,omitempty"`
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

func NormalizeCacheWarmupResults(values []CacheWarmupResult) ([]CacheWarmupResult, error) {
	if len(values) > MaxCacheWarmupResults {
		return nil, errors.New("too many cache prewarm results")
	}
	result := make([]CacheWarmupResult, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value.WarmupID = strings.TrimSpace(value.WarmupID)
		value.SiteID = strings.TrimSpace(value.SiteID)
		if value.WarmupID == "" || len(value.WarmupID) > 128 || value.SiteID == "" || len(value.SiteID) > 128 ||
			value.CompletedAt.IsZero() || value.AttemptedURLs < 1 || value.AttemptedURLs > MaxCacheWarmupPaths ||
			value.SucceededURLs < 0 || value.SucceededURLs > value.AttemptedURLs || len(value.Failures) > MaxCacheWarmupPaths {
			return nil, errors.New("cache prewarm result is invalid")
		}
		switch value.Status {
		case CacheWarmupSucceeded:
			if value.SucceededURLs != value.AttemptedURLs || len(value.Failures) != 0 {
				return nil, errors.New("cache prewarm result is inconsistent")
			}
		case CacheWarmupPartial:
			if value.SucceededURLs == 0 || value.SucceededURLs == value.AttemptedURLs || len(value.Failures) == 0 {
				return nil, errors.New("cache prewarm result is inconsistent")
			}
		case CacheWarmupFailed:
			if value.SucceededURLs != 0 || len(value.Failures) == 0 {
				return nil, errors.New("cache prewarm result is inconsistent")
			}
		default:
			return nil, errors.New("cache prewarm result status is invalid")
		}
		for index := range value.Failures {
			failure := &value.Failures[index]
			failure.Path = strings.TrimSpace(failure.Path)
			failure.Detail = strings.TrimSpace(failure.Detail)
			if len(failure.Path) > 2048 || failure.Detail == "" || len(failure.Detail) > 1024 || strings.ContainsAny(failure.Detail, "\x00\r\n") {
				return nil, errors.New("cache prewarm failure is invalid")
			}
			if failure.Path != "" {
				normalized, err := NormalizeCacheInvalidationTarget(CacheInvalidationURL, failure.Path)
				if err != nil {
					return nil, errors.New("cache prewarm failure path is invalid")
				}
				failure.Path = normalized
			}
		}
		if _, found := seen[value.WarmupID]; found {
			return nil, errors.New("cache prewarm results contain a duplicate job")
		}
		seen[value.WarmupID] = struct{}{}
		value.CompletedAt = value.CompletedAt.UTC()
		result = append(result, value)
	}
	return result, nil
}
