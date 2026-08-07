package control

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

type cacheOperationCreateRequest struct {
	SiteID       string                        `json:"site_id"`
	Scope        domain.CacheInvalidationScope `json:"scope"`
	Value        string                        `json:"value"`
	Prewarm      bool                          `json:"prewarm"`
	PrewarmPaths []string                      `json:"prewarm_paths"`
}

type cacheSiteOverview struct {
	SiteID               string                 `json:"site_id"`
	SiteName             string                 `json:"site_name"`
	Domains              []string               `json:"domains"`
	Cacheable            bool                   `json:"cacheable"`
	DisabledReason       string                 `json:"disabled_reason,omitempty"`
	CacheGeneration      int64                  `json:"cache_generation"`
	RuleCount            int                    `json:"rule_count"`
	NodeCount            int                    `json:"node_count"`
	ActiveNodeCount      int                    `json:"active_node_count"`
	ReportingNodeCount   int                    `json:"reporting_node_count"`
	LastOperation        *domain.CacheOperation `json:"last_operation,omitempty"`
	PendingConfiguration bool                   `json:"pending_configuration"`
}

type cacheRuleOverview struct {
	SiteID     string                        `json:"site_id"`
	SiteName   string                        `json:"site_name"`
	Scope      domain.CacheInvalidationScope `json:"scope"`
	Value      string                        `json:"value"`
	Generation int64                         `json:"generation"`
}

type cacheOperationsOverviewResponse struct {
	Sites      []cacheSiteOverview     `json:"sites"`
	Operations []domain.CacheOperation `json:"operations"`
	Rules      []cacheRuleOverview     `json:"rules"`
}

func (s *Server) cacheOperationsOverview(response http.ResponseWriter, _ *http.Request) {
	operations, err := s.Store.ListCacheOperations("", 200)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	sites, err := s.Store.ListSites()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	nodes, err := s.Store.ListNodes()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	nodesByID := make(map[string]domain.Node, len(nodes))
	for _, node := range nodes {
		nodesByID[node.ID] = node
	}
	lastBySite := make(map[string]*domain.CacheOperation)
	for index := range operations {
		if lastBySite[operations[index].SiteID] == nil {
			lastBySite[operations[index].SiteID] = &operations[index]
		}
	}
	overview := cacheOperationsOverviewResponse{
		Sites: make([]cacheSiteOverview, 0, len(sites)), Operations: operations, Rules: make([]cacheRuleOverview, 0),
	}
	for _, site := range sites {
		cacheable, reason := cacheSiteEligibility(site)
		item := cacheSiteOverview{
			SiteID: site.ID, SiteName: site.Name, Domains: append([]string(nil), site.Domains...),
			Cacheable: cacheable, DisabledReason: reason, CacheGeneration: site.CacheGeneration,
			RuleCount: len(site.CacheInvalidations), NodeCount: len(site.Nodes), LastOperation: lastBySite[site.ID],
			PendingConfiguration: !site.Published,
		}
		for _, nodeID := range site.Nodes {
			node, found := nodesByID[nodeID]
			if !found || node.Status != domain.NodeActive {
				continue
			}
			item.ActiveNodeCount++
			if slices.Contains(node.Capabilities, domain.EdgeCapabilityCacheControl) &&
				slices.Contains(node.Capabilities, domain.EdgeCapabilityCacheWarmupResults) {
				item.ReportingNodeCount++
			}
		}
		overview.Sites = append(overview.Sites, item)
		for _, rule := range site.CacheInvalidations {
			overview.Rules = append(overview.Rules, cacheRuleOverview{
				SiteID: site.ID, SiteName: site.Name, Scope: rule.Scope, Value: rule.Value, Generation: rule.Generation,
			})
		}
	}
	writeJSON(response, http.StatusOK, overview)
}

func cacheSiteEligibility(site domain.Site) (bool, string) {
	if site.Deleting {
		return false, "deleting"
	}
	if site.TCPOnly {
		return false, "tcp_only"
	}
	if site.Passthrough {
		return false, "passthrough"
	}
	if !site.OriginResponseBuffering {
		return false, "origin_response_buffering_disabled"
	}
	origin := strings.ToLower(strings.TrimSpace(site.PrimaryOrigin.URL))
	if !strings.HasPrefix(origin, "http://") && !strings.HasPrefix(origin, "https://") {
		return false, "unsupported_origin"
	}
	return true, ""
}

func (s *Server) listCacheOperations(response http.ResponseWriter, request *http.Request) {
	limit := 200
	if value := strings.TrimSpace(request.URL.Query().Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 500 {
			writeError(response, http.StatusBadRequest, errors.New("limit must be between 1 and 500"))
			return
		}
		limit = parsed
	}
	operations, err := s.Store.ListCacheOperations(request.URL.Query().Get("site_id"), limit)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, operations)
}

func (s *Server) getCacheOperation(response http.ResponseWriter, request *http.Request) {
	operation, err := s.Store.GetCacheOperation(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, operation)
}

func (s *Server) createCacheOperation(response http.ResponseWriter, request *http.Request) {
	var input cacheOperationCreateRequest
	if !readJSON(response, request, &input) {
		return
	}
	input.SiteID = strings.TrimSpace(input.SiteID)
	if input.SiteID == "" {
		writeError(response, http.StatusBadRequest, errors.New("site_id is required"))
		return
	}
	legacy := cacheInvalidationRequest{Scope: input.Scope, Value: input.Value, Prewarm: input.Prewarm, PrewarmPaths: input.PrewarmPaths}
	if legacy.Scope == "" {
		legacy.Scope = domain.CacheInvalidationFull
	}
	target, err := normalizeCacheInvalidationRequest(legacy)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	paths, err := s.cachePrewarmPaths(request.Context(), input.SiteID, legacy, target)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	operation, task, err := s.Publisher.RunCacheInvalidation(store.CacheOperationInput{
		SiteID: input.SiteID, Scope: legacy.Scope, Target: target, PrewarmPaths: paths,
		Actor: adminID(request.Context()), RemoteAddr: s.requestIP(request),
	})
	if err != nil {
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "invalidate_cache", "cache_operation", operation.ID,
		"site="+operation.SiteID+" task="+task.ID)
	writeJSON(response, http.StatusAccepted, operation)
}

func normalizeCacheInvalidationRequest(input cacheInvalidationRequest) (string, error) {
	if input.Scope == "" {
		input.Scope = domain.CacheInvalidationFull
	}
	if input.Scope == domain.CacheInvalidationFull {
		if strings.TrimSpace(input.Value) != "" {
			return "", errors.New("full cache invalidation does not accept a target")
		}
		return "", nil
	}
	return domain.NormalizeCacheInvalidationTarget(input.Scope, input.Value)
}

func (s *Server) retryCacheOperation(response http.ResponseWriter, request *http.Request) {
	operation, task, err := s.Publisher.RetryCachePrewarm(request.PathValue("id"), adminID(request.Context()), s.requestIP(request))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "retry_cache_prewarm", "cache_operation", operation.ID,
		"retry_of="+operation.RetryOfID+" task="+task.ID)
	writeJSON(response, http.StatusAccepted, operation)
}
