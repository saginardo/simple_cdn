package control

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/logstore"
	"simple_cdn/internal/store"
)

const (
	nodeCacheWindow              = 24 * time.Hour
	nodeCacheStorageFreshness    = 15 * time.Minute
	nodeMachineLegacyFreshness   = 10 * time.Minute
	nodeMachineRealtimeFreshness = 15 * time.Second
	nodeMachineAdaptiveFreshness = 2*time.Duration(domain.DefaultMachineStatusIntervalSeconds)*time.Second + 15*time.Second
	nodeMachineNetworkFreshness  = 5 * time.Second
)

type nodeCacheStatusBucket struct {
	Status   string `json:"status"`
	Requests uint64 `json:"requests"`
	Bytes    int64  `json:"bytes"`
}

type nodeCacheStorageStatus struct {
	Available         bool       `json:"available"`
	UnavailableReason string     `json:"unavailable_reason,omitempty"`
	UsedBytes         int64      `json:"used_bytes"`
	TotalBytes        int64      `json:"total_bytes"`
	CollectedAt       *time.Time `json:"collected_at,omitempty"`
	Stale             bool       `json:"stale"`
}

type nodeCacheStatusResponse struct {
	Available         bool                    `json:"available"`
	UnavailableReason string                  `json:"unavailable_reason,omitempty"`
	From              time.Time               `json:"from"`
	To                time.Time               `json:"to"`
	LastSeenAt        *time.Time              `json:"last_seen_at,omitempty"`
	Requests          uint64                  `json:"requests"`
	Bytes             int64                   `json:"bytes"`
	CacheLookups      uint64                  `json:"cache_lookups"`
	CacheHits         uint64                  `json:"cache_hits"`
	CacheMisses       uint64                  `json:"cache_misses"`
	Bypasses          uint64                  `json:"bypasses"`
	Uncached          uint64                  `json:"uncached"`
	HitRate           float64                 `json:"hit_rate"`
	Statuses          []nodeCacheStatusBucket `json:"statuses"`
	Storage           nodeCacheStorageStatus  `json:"storage"`
}

type nodeCacheSettingsResponse struct {
	DefaultSizeGB   int  `json:"default_size_gb"`
	OverrideSizeGB  *int `json:"override_size_gb"`
	EffectiveSizeGB int  `json:"effective_size_gb"`
}

type nodeSiteSummary struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Domains      []string `json:"domains"`
	Enabled      bool     `json:"enabled"`
	Published    bool     `json:"published"`
	CacheEnabled bool     `json:"cache_enabled"`
}

type nodeDetailResponse struct {
	Node    nodeUpgradeStatusResponse `json:"node"`
	Machine nodeMachineStatusResponse `json:"machine"`
	Cache   nodeCacheSettingsResponse `json:"cache"`
	Sites   []nodeSiteSummary         `json:"sites"`
}

type nodeMachineStatusResponse struct {
	Available         bool                         `json:"available"`
	UnavailableReason string                       `json:"unavailable_reason,omitempty"`
	Stale             bool                         `json:"stale"`
	Report            *domain.MachineStatus        `json:"report,omitempty"`
	Network           *domain.MachineNetworkStatus `json:"network,omitempty"`
	NetworkStale      bool                         `json:"network_stale,omitempty"`
	OriginCollectedAt *time.Time                   `json:"origin_collected_at,omitempty"`
	OriginStale       bool                         `json:"origin_stale,omitempty"`
}

func (s *Server) nodeDetail(response http.ResponseWriter, request *http.Request) {
	if err := s.Store.ReconcileNodeUpgrades(); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	node, err := s.Store.GetNode(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	status, err := s.buildNodeUpgradeStatus(node)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	cacheSettings, err := s.nodeCacheSettings(node)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	configuredSites, err := s.Store.ListSitesForNode(node.ID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	sites := make([]nodeSiteSummary, 0)
	for _, site := range configuredSites {
		if !siteHasNode(site, node.ID) {
			continue
		}
		sites = append(sites, nodeSiteSummary{
			ID: site.ID, Name: site.Name, Domains: append([]string{}, site.Domains...),
			Enabled: site.Enabled, Published: site.Published, CacheEnabled: siteCacheEnabled(site),
		})
	}

	writeJSON(response, http.StatusOK, nodeDetailResponse{
		Node: status, Machine: s.nodeMachineStatus(node, time.Now().UTC()), Cache: cacheSettings, Sites: sites,
	})
}

func (s *Server) nodeCacheSettings(node domain.Node) (nodeCacheSettingsResponse, error) {
	settings, err := s.Store.ControlSettings()
	if err != nil {
		return nodeCacheSettingsResponse{}, err
	}
	effective, err := domain.EffectiveNodeCacheMaxSizeGB(node, settings.CacheDefaultSizeGB)
	if err != nil {
		return nodeCacheSettingsResponse{}, err
	}
	return nodeCacheSettingsResponse{
		DefaultSizeGB: settings.CacheDefaultSizeGB, OverrideSizeGB: node.CacheMaxSizeGB, EffectiveSizeGB: effective,
	}, nil
}

func (s *Server) updateNodeCacheSettings(response http.ResponseWriter, request *http.Request) {
	var input struct {
		CacheMaxSizeGB optionalNullableInt `json:"cache_max_size_gb"`
	}
	if !readJSON(response, request, &input) {
		return
	}
	if !input.CacheMaxSizeGB.Present {
		writeError(response, http.StatusBadRequest, errors.New("cache_max_size_gb is required"))
		return
	}
	node, err := s.Store.SetNodeCacheMaxSizeGB(request.PathValue("id"), input.CacheMaxSizeGB.Value)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeStoreError(response, err)
		} else {
			writeError(response, http.StatusBadRequest, err)
		}
		return
	}
	settings, err := s.nodeCacheSettings(node)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	detail := "inherit global cache limit"
	if node.CacheMaxSizeGB != nil {
		detail = fmt.Sprintf("cache_max_size_gb=%d", *node.CacheMaxSizeGB)
	}
	s.audit(request, adminID(request.Context()), "update_cache", "node", node.ID, detail)
	if err := s.publishNodeConfiguration(node.ID); err != nil {
		writeError(response, http.StatusConflict, fmt.Errorf("save cache quota but could not publish node configuration: %w", err))
		return
	}
	writeJSON(response, http.StatusOK, settings)
}

func (s *Server) updateNodePublicIPv6(response http.ResponseWriter, request *http.Request) {
	var input struct {
		PublicIPv6 string `json:"public_ipv6"`
	}
	if !readJSON(response, request, &input) {
		return
	}
	node, err := s.Store.SetNodePublicIPv6(request.PathValue("id"), input.PublicIPv6)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeStoreError(response, err)
		} else {
			writeError(response, http.StatusBadRequest, err)
		}
		return
	}
	detail := "public_ipv6=disabled"
	if node.PublicIPv6 != "" {
		detail = "public_ipv6=" + node.PublicIPv6
	}
	s.audit(request, adminID(request.Context()), "update_public_ipv6", "node", node.ID, detail)
	writeJSON(response, http.StatusOK, node)
}

func (s *Server) updateNodeNginxCapacity(response http.ResponseWriter, request *http.Request) {
	var input domain.NginxCapacity
	if !readJSON(response, request, &input) {
		return
	}
	node, err := s.Store.SetNodeNginxCapacity(request.PathValue("id"), input)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeStoreError(response, err)
		} else {
			writeError(response, http.StatusBadRequest, err)
		}
		return
	}
	if err := s.publishNodeConfiguration(node.ID); err != nil {
		writeError(response, http.StatusConflict, fmt.Errorf("save Nginx capacity but could not publish node configuration: %w", err))
		return
	}
	s.audit(request, adminID(request.Context()), "update_nginx_capacity", "node", node.ID, fmt.Sprintf("worker_processes=%d worker_connections=%d worker_rlimit_nofile=%d", node.NginxCapacity.WorkerProcesses, node.NginxCapacity.WorkerConnections, node.NginxCapacity.WorkerRlimitNoFile))
	writeJSON(response, http.StatusOK, node)
}

func (s *Server) publishNodeConfiguration(nodeID string) error {
	if s.Publisher.Store == nil {
		return nil
	}
	if s.Publisher.Cipher == nil {
		return errors.New("node publisher is not configured")
	}
	return s.Publisher.PublishNode(nodeID)
}

func (s *Server) nodeMachineStatus(node domain.Node, at time.Time) nodeMachineStatusResponse {
	return s.nodeMachineStatusWithCapabilityProfile(node.ID, machineStatusCapabilityProfileFor(node.Capabilities), at)
}

func (s *Server) nodeMachineStatusWithCapabilityProfile(nodeID string, capabilities machineStatusCapabilityProfile, at time.Time) nodeMachineStatusResponse {
	s.machineStatusMu.RLock()
	report, found := s.machineStatuses[nodeID]
	network, networkFound := s.machineNetworkStatuses[nodeID]
	origin, originFound := s.machineOriginStatuses[nodeID]
	networkDemandActive := s.machineStatusDemandActive[nodeID]
	s.machineStatusMu.RUnlock()
	if found {
		report.CollectedAt = report.CollectedAt.UTC()
		freshness := nodeMachineLegacyFreshness
		if capabilities.adaptive {
			freshness = nodeMachineAdaptiveFreshness
		} else if capabilities.stream {
			freshness = nodeMachineRealtimeFreshness
		}
		originReportCollectedAt := report.CollectedAt
		originFreshness := freshness
		if capabilities.adaptive {
			originFreshness = nodeMachineRealtimeFreshness
		}
		if originFound && origin.CollectedAt.After(originReportCollectedAt) {
			originReportCollectedAt = origin.CollectedAt.UTC()
			report.OriginProbes = origin.OriginProbes
		}
		// Envelope time orders reports; freshness follows the actual probe so
		// repeated uploads cannot make stopped probe data appear current.
		originCollectedAt := latestOriginProbeCheckedAt(report.OriginProbes, originReportCollectedAt)
		status := nodeMachineStatusResponse{
			Available: true, Stale: report.CollectedAt.Before(at.Add(-freshness)), Report: &report,
			OriginCollectedAt: &originCollectedAt,
			OriginStale:       originCollectedAt.Before(at.Add(-originFreshness)),
		}
		networkCollectedAt := report.CollectedAt
		if capabilities.adaptive && networkFound && network.CollectedAt.After(networkCollectedAt) {
			network.CollectedAt = network.CollectedAt.UTC()
			status.Network = &network
			networkCollectedAt = network.CollectedAt
		}
		if capabilities.adaptive {
			networkFreshness := nodeMachineAdaptiveFreshness
			if networkDemandActive {
				networkFreshness = nodeMachineNetworkFreshness
			}
			status.NetworkStale = networkCollectedAt.Before(at.Add(-networkFreshness))
		}
		return status
	}
	if capabilities.supported {
		return nodeMachineStatusResponse{UnavailableReason: "等待边缘节点首次上报机器状态"}
	}
	return nodeMachineStatusResponse{UnavailableReason: "升级边缘代理后可查看机器状态"}
}

func latestOriginProbeCheckedAt(probes []domain.OriginProbeStatus, fallback time.Time) time.Time {
	latest := time.Time{}
	for _, probe := range probes {
		if probe.CheckedAt.After(latest) {
			latest = probe.CheckedAt
		}
	}
	if latest.IsZero() {
		return fallback.UTC()
	}
	return latest.UTC()
}

func (s *Server) nodeCacheStatus(response http.ResponseWriter, request *http.Request) {
	nodeID := request.PathValue("id")
	node, err := s.Store.GetNode(nodeID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	to := time.Now().UTC().Truncate(time.Second)
	from := to.Add(-nodeCacheWindow)
	cache := buildNodeCacheStatus(from, to, nil, false, "访问日志存储未启用")
	if s.Logs != nil {
		buckets, cacheErr := s.Logs.NodeCache(request.Context(), nodeID, from, to)
		switch {
		case cacheErr == nil:
			cache = buildNodeCacheStatus(from, to, buckets, true, "")
		case errors.Is(cacheErr, logstore.ErrUnavailable):
			cache = buildNodeCacheStatus(from, to, nil, false, "访问日志存储未启用")
		default:
			cache = buildNodeCacheStatus(from, to, nil, false, "缓存统计暂不可用")
			if s.Logger != nil {
				s.Logger.Warn("node cache status unavailable", "node_id", nodeID, "error", cacheErr)
			}
		}
	}
	cache.Storage = s.nodeCacheStorageStatus(node, to)
	writeJSON(response, http.StatusOK, cache)
}

func (s *Server) nodeCacheStorageStatus(node domain.Node, at time.Time) nodeCacheStorageStatus {
	usage, err := s.Store.GetNodeCacheStorage(node.ID)
	if err == nil {
		collectedAt := usage.CollectedAt.UTC()
		return nodeCacheStorageStatus{
			Available: true, UsedBytes: usage.UsedBytes, TotalBytes: usage.TotalBytes,
			CollectedAt: &collectedAt, Stale: collectedAt.Before(at.Add(-nodeCacheStorageFreshness)),
		}
	}
	if !errors.Is(err, store.ErrNotFound) {
		if s.Logger != nil {
			s.Logger.Warn("node cache storage unavailable", "node_id", node.ID, "error", err)
		}
		return nodeCacheStorageStatus{UnavailableReason: "缓存空间上报暂不可用"}
	}
	for _, capability := range node.Capabilities {
		if capability == domain.EdgeCapabilityCacheUsage {
			return nodeCacheStorageStatus{UnavailableReason: "等待边缘节点首次采集缓存空间"}
		}
	}
	return nodeCacheStorageStatus{UnavailableReason: "升级边缘代理后可查看缓存空间"}
}

func buildNodeCacheStatus(from, to time.Time, buckets []logstore.NodeCacheBucket, available bool, unavailableReason string) nodeCacheStatusResponse {
	type aggregate struct {
		requests uint64
		bytes    int64
	}
	aggregates := make(map[string]aggregate)
	var lastSeenAt *time.Time
	for _, bucket := range buckets {
		status := strings.ToUpper(strings.TrimSpace(bucket.Status))
		if status == "" {
			status = "UNCACHED"
		}
		current := aggregates[status]
		current.requests += bucket.Requests
		current.bytes += bucket.Bytes
		aggregates[status] = current
		if lastSeenAt == nil || bucket.LastSeenAt.After(*lastSeenAt) {
			value := bucket.LastSeenAt.UTC()
			lastSeenAt = &value
		}
	}

	result := nodeCacheStatusResponse{
		Available: available, UnavailableReason: unavailableReason, From: from, To: to,
		LastSeenAt: lastSeenAt, Statuses: make([]nodeCacheStatusBucket, 0, len(aggregates)),
	}
	for status, values := range aggregates {
		result.Requests += values.requests
		result.Bytes += values.bytes
		switch status {
		case "HIT", "STALE", "UPDATING", "REVALIDATED":
			result.CacheHits += values.requests
		case "MISS", "EXPIRED":
			result.CacheMisses += values.requests
		case "BYPASS":
			result.Bypasses += values.requests
		case "UNCACHED":
			result.Uncached += values.requests
		}
		result.Statuses = append(result.Statuses, nodeCacheStatusBucket{Status: status, Requests: values.requests, Bytes: values.bytes})
	}
	result.CacheLookups = result.CacheHits + result.CacheMisses
	if result.CacheLookups > 0 {
		result.HitRate = float64(result.CacheHits) / float64(result.CacheLookups)
	}
	order := map[string]int{"HIT": 0, "MISS": 1, "BYPASS": 2, "EXPIRED": 3, "STALE": 4, "UPDATING": 5, "REVALIDATED": 6, "UNCACHED": 7}
	sort.Slice(result.Statuses, func(i, j int) bool {
		left, leftKnown := order[result.Statuses[i].Status]
		right, rightKnown := order[result.Statuses[j].Status]
		if leftKnown != rightKnown {
			return leftKnown
		}
		if leftKnown && left != right {
			return left < right
		}
		return result.Statuses[i].Status < result.Statuses[j].Status
	})
	return result
}

func containsNode(nodeIDs []string, nodeID string) bool {
	for _, candidate := range nodeIDs {
		if candidate == nodeID {
			return true
		}
	}
	return false
}

func siteCacheEnabled(site domain.Site) bool {
	if site.Passthrough || site.TCPOnly {
		return false
	}
	parsed, err := url.Parse(site.PrimaryOrigin.URL)
	if err != nil {
		return false
	}
	scheme := domain.ProxyScheme(strings.ToLower(parsed.Scheme))
	return scheme == "http" || scheme == "https"
}
