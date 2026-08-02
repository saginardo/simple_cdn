package control

import (
	"crypto/rand"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/nginx"
	"simple_cdn/internal/store"
)

type securityCoverageNode struct {
	ID                  string            `json:"id"`
	Name                string            `json:"name"`
	Status              domain.NodeStatus `json:"status"`
	Capable             bool              `json:"capable"`
	Configured          bool              `json:"configured"`
	RateLimitCapable    bool              `json:"rate_limit_capable"`
	RateLimitConfigured bool              `json:"rate_limit_configured"`
	WAFChainCapable     bool              `json:"waf_chain_capable"`
	POWCapable          bool              `json:"pow_capable"`
	POWConfigured       bool              `json:"pow_configured"`
	DesiredVersion      int64             `json:"desired_version"`
	AppliedVersion      int64             `json:"applied_version"`
	LastError           string            `json:"last_error,omitempty"`
}

type securityOverviewResponse struct {
	Policies          []domain.SecurityPolicy  `json:"policies"`
	POWPolicies       []domain.POWPolicy       `json:"pow_policies"`
	RateLimitPolicies []domain.RateLimitPolicy `json:"rate_limit_policies"`
	Sites             []securitySiteOption     `json:"sites"`
	Bans              []domain.SecurityBan     `json:"bans"`
	ActiveBanCount    int                      `json:"active_ban_count"`
	Events            []domain.SecurityEvent   `json:"events"`
	Nodes             []securityCoverageNode   `json:"nodes"`
	DeploymentError   string                   `json:"deployment_error,omitempty"`
}

type securitySiteOption struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Domains  []string `json:"domains"`
	Enabled  bool     `json:"enabled"`
	Deleting bool     `json:"deleting"`
}

type securityPolicyRequest struct {
	Name               string                      `json:"name"`
	Enabled            bool                        `json:"enabled"`
	SiteIDs            []string                    `json:"site_ids"`
	Conditions         []domain.SecurityCondition  `json:"conditions"`
	Pattern            string                      `json:"pattern"`
	Action             domain.SecurityPolicyAction `json:"action"`
	BanDurationSeconds int                         `json:"ban_duration_seconds"`
	ResponseStatus     int                         `json:"response_status"`
	Priority           int                         `json:"priority"`
}

type securityPolicyMoveRequest struct {
	Direction string `json:"direction"`
}

type powPolicyRequest struct {
	Name                string   `json:"name"`
	Enabled             bool     `json:"enabled"`
	SiteIDs             []string `json:"site_ids"`
	PathPattern         string   `json:"path_pattern"`
	DifficultyBits      int      `json:"difficulty_bits"`
	ChallengeTTLSeconds int      `json:"challenge_ttl_seconds"`
	PassTTLSeconds      int      `json:"pass_ttl_seconds"`
	Priority            int      `json:"priority"`
}

type rateLimitPolicyRequest struct {
	Name                     string `json:"name"`
	Enabled                  bool   `json:"enabled"`
	RequestsPerSecond        int    `json:"requests_per_second"`
	ResponseConditionEnabled bool   `json:"response_condition_enabled"`
	ResponseStatusClasses    []int  `json:"response_status_classes"`
	BanEnabled               bool   `json:"ban_enabled"`
	BanAfterConsecutive429   int    `json:"ban_after_consecutive_429"`
	BanDurationSeconds       int    `json:"ban_duration_seconds"`
}

func (s *Server) securityOverview(deploymentErr error) (securityOverviewResponse, error) {
	policies, err := s.Store.ListSecurityPolicies()
	if err != nil {
		return securityOverviewResponse{}, err
	}
	powPolicies, err := s.Store.ListPOWPolicies()
	if err != nil {
		return securityOverviewResponse{}, err
	}
	rateLimitPolicies, err := s.Store.ListRateLimitPolicies()
	if err != nil {
		return securityOverviewResponse{}, err
	}
	bans, err := s.Store.ListActiveSecurityBansLimit(500)
	if err != nil {
		return securityOverviewResponse{}, err
	}
	activeBanCount, err := s.Store.CountActiveSecurityBans()
	if err != nil {
		return securityOverviewResponse{}, err
	}
	events, err := s.Store.ListRecentSecurityEvents(100)
	if err != nil {
		return securityOverviewResponse{}, err
	}
	sites, err := s.Store.ListSites()
	if err != nil {
		return securityOverviewResponse{}, err
	}
	siteOptions := make([]securitySiteOption, 0, len(sites))
	for _, site := range sites {
		if site.TCPOnly || site.Deleting {
			continue
		}
		siteOptions = append(siteOptions, securitySiteOption{
			ID: site.ID, Name: site.Name, Domains: site.Domains, Enabled: site.Enabled, Deleting: site.Deleting,
		})
	}
	nodes, err := s.Store.ListNodes()
	if err != nil {
		return securityOverviewResponse{}, err
	}
	coverage := make([]securityCoverageNode, 0, len(nodes))
	for _, node := range nodes {
		desiredVersion, err := s.Store.DesiredVersion(node.ID)
		if err != nil {
			return securityOverviewResponse{}, err
		}
		configured, powConfigured, rateLimitConfigured := false, false, false
		if nodeState, _, stateErr := s.Store.NodeState(node.ID); stateErr == nil {
			nodeSecurityPolicies := securityPoliciesForCapabilities(policies, node.Capabilities)
			configured = nginx.HasSecurityRevision(nodeState.NginxConfig, nodeSecurityPolicies)
			if slices.Contains(node.Capabilities, domain.EdgeCapabilityWAFChain) {
				configured = configured && strings.Contains(nodeState.NginxConfig, nginx.WAFRuntimeMarker)
			}
			nodePOWPolicies := powPoliciesForCoverage(powPolicies, sites, node.ID, node.Capabilities)
			powConfigured = slices.Contains(node.Capabilities, domain.EdgeCapabilityPOWChallenge) &&
				strings.Contains(nodeState.NginxConfig, nginx.POWRuntimeMarker) &&
				nginx.HasPOWRevision(nodeState.NginxConfig, nodePOWPolicies)
			nodeRateLimitPolicies := rateLimitPoliciesForCapabilities(rateLimitPolicies, node.Capabilities)
			rateLimitConfigured = nginx.HasRateLimitRevision(nodeState.NginxConfig, nodeRateLimitPolicies)
		} else if !errors.Is(stateErr, store.ErrNotFound) {
			return securityOverviewResponse{}, stateErr
		}
		coverage = append(coverage, securityCoverageNode{
			ID: node.ID, Name: node.Name, Status: node.Status,
			Capable:             slices.Contains(node.Capabilities, domain.EdgeCapabilitySecurity),
			Configured:          configured,
			RateLimitCapable:    slices.Contains(node.Capabilities, domain.EdgeCapabilityRateLimit),
			RateLimitConfigured: rateLimitConfigured,
			WAFChainCapable:     slices.Contains(node.Capabilities, domain.EdgeCapabilityWAFChain),
			POWCapable:          slices.Contains(node.Capabilities, domain.EdgeCapabilityPOWChallenge),
			POWConfigured:       powConfigured,
			DesiredVersion:      desiredVersion, AppliedVersion: node.AppliedVersion, LastError: node.LastError,
		})
	}
	result := securityOverviewResponse{
		Policies: policies, POWPolicies: powPolicies, RateLimitPolicies: rateLimitPolicies, Bans: bans,
		Sites: siteOptions, ActiveBanCount: activeBanCount, Events: events, Nodes: coverage,
	}
	if deploymentErr != nil {
		result.DeploymentError = deploymentErr.Error()
	}
	return result, nil
}

func powPoliciesForCoverage(policies []domain.POWPolicy, sites []domain.Site, nodeID string, capabilities []string) []domain.POWPolicy {
	if !slices.Contains(capabilities, domain.EdgeCapabilityWAFChain) ||
		!slices.Contains(capabilities, domain.EdgeCapabilityPOWChallenge) {
		return nil
	}
	siteIDs := make(map[string]struct{})
	for _, site := range sites {
		if site.Enabled && !site.TCPOnly && siteHasNode(site, nodeID) {
			siteIDs[site.ID] = struct{}{}
		}
	}
	result := make([]domain.POWPolicy, 0, len(policies))
	for _, policy := range policies {
		if !policy.Enabled {
			continue
		}
		for _, siteID := range policy.SiteIDs {
			if _, found := siteIDs[siteID]; found {
				result = append(result, policy)
				break
			}
		}
	}
	return result
}

func (s *Server) getSecurityOverview(response http.ResponseWriter, request *http.Request) {
	result, err := s.securityOverview(nil)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func securityPolicyFromRequest(input securityPolicyRequest) domain.SecurityPolicy {
	return domain.SecurityPolicy{
		Name: input.Name, Enabled: input.Enabled, SiteIDs: input.SiteIDs, Conditions: input.Conditions,
		Pattern: input.Pattern, Action: input.Action, BanDurationSeconds: input.BanDurationSeconds,
		ResponseStatus: input.ResponseStatus, Priority: input.Priority,
	}
}

func powPolicyFromRequest(input powPolicyRequest) domain.POWPolicy {
	return domain.POWPolicy{
		Name: input.Name, Enabled: input.Enabled, SiteIDs: input.SiteIDs, PathPattern: input.PathPattern,
		DifficultyBits: input.DifficultyBits, ChallengeTTLSeconds: input.ChallengeTTLSeconds,
		PassTTLSeconds: input.PassTTLSeconds, Priority: input.Priority,
	}
}

func rateLimitPolicyFromRequest(input rateLimitPolicyRequest) domain.RateLimitPolicy {
	return domain.RateLimitPolicy{
		Name: input.Name, Enabled: input.Enabled, RequestsPerSecond: input.RequestsPerSecond,
		ResponseConditionEnabled: input.ResponseConditionEnabled,
		ResponseStatusClasses:    input.ResponseStatusClasses,
		BanEnabled:               input.BanEnabled,
		BanAfterConsecutive429:   input.BanAfterConsecutive429,
		BanDurationSeconds:       input.BanDurationSeconds,
	}
}

func (s *Server) createSecurityPolicy(response http.ResponseWriter, request *http.Request) {
	var input securityPolicyRequest
	if !readJSON(response, request, &input) {
		return
	}
	policy, err := s.Store.CreateSecurityPolicy(securityPolicyFromRequest(input))
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	deploymentErr := s.Publisher.PublishAll()
	s.audit(request, adminID(request.Context()), "create", "security_policy", policy.ID, policy.Name)
	result, err := s.securityOverview(deploymentErr)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusCreated, result)
}

func (s *Server) updateSecurityPolicy(response http.ResponseWriter, request *http.Request) {
	var input securityPolicyRequest
	if !readJSON(response, request, &input) {
		return
	}
	policy, err := s.Store.UpdateSecurityPolicy(request.PathValue("id"), securityPolicyFromRequest(input))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeStoreError(response, err)
		} else {
			writeError(response, http.StatusBadRequest, err)
		}
		return
	}
	deploymentErr := s.Publisher.PublishAll()
	s.audit(request, adminID(request.Context()), "update", "security_policy", policy.ID, policy.Name)
	result, err := s.securityOverview(deploymentErr)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) deleteSecurityPolicy(response http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	if err := s.Store.DeleteSecurityPolicy(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeStoreError(response, err)
		} else {
			writeError(response, http.StatusConflict, err)
		}
		return
	}
	deploymentErr := s.Publisher.PublishAll()
	s.audit(request, adminID(request.Context()), "delete", "security_policy", id, "")
	result, err := s.securityOverview(deploymentErr)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) moveSecurityPolicy(response http.ResponseWriter, request *http.Request) {
	var input securityPolicyMoveRequest
	if !readJSON(response, request, &input) {
		return
	}
	id := request.PathValue("id")
	if err := s.Store.MoveSecurityPolicy(id, input.Direction); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeStoreError(response, err)
		} else {
			writeError(response, http.StatusBadRequest, err)
		}
		return
	}
	deploymentErr := s.Publisher.PublishAll()
	s.audit(request, adminID(request.Context()), "move", "security_policy", id, strings.ToLower(strings.TrimSpace(input.Direction)))
	result, err := s.securityOverview(deploymentErr)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) createPOWPolicy(response http.ResponseWriter, request *http.Request) {
	var input powPolicyRequest
	if !readJSON(response, request, &input) {
		return
	}
	if s.Cipher == nil {
		writeError(response, http.StatusServiceUnavailable, errors.New("proof-of-work encryption is not configured"))
		return
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	ciphertext, err := s.Cipher.Encrypt(secret)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	policy, err := s.Store.CreatePOWPolicy(powPolicyFromRequest(input), ciphertext)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	deploymentErr := s.Publisher.PublishAll()
	s.audit(request, adminID(request.Context()), "create", "pow_policy", policy.ID, policy.Name)
	result, err := s.securityOverview(deploymentErr)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusCreated, result)
}

func (s *Server) updatePOWPolicy(response http.ResponseWriter, request *http.Request) {
	var input powPolicyRequest
	if !readJSON(response, request, &input) {
		return
	}
	policy, err := s.Store.UpdatePOWPolicy(request.PathValue("id"), powPolicyFromRequest(input))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeStoreError(response, err)
		} else {
			writeError(response, http.StatusBadRequest, err)
		}
		return
	}
	deploymentErr := s.Publisher.PublishAll()
	s.audit(request, adminID(request.Context()), "update", "pow_policy", policy.ID, policy.Name)
	result, err := s.securityOverview(deploymentErr)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) deletePOWPolicy(response http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	if err := s.Store.DeletePOWPolicy(id); err != nil {
		writeStoreError(response, err)
		return
	}
	deploymentErr := s.Publisher.PublishAll()
	s.audit(request, adminID(request.Context()), "delete", "pow_policy", id, "")
	result, err := s.securityOverview(deploymentErr)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) createRateLimitPolicy(response http.ResponseWriter, request *http.Request) {
	var input rateLimitPolicyRequest
	if !readJSON(response, request, &input) {
		return
	}
	policy, err := s.Store.CreateRateLimitPolicy(rateLimitPolicyFromRequest(input))
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	deploymentErr := s.Publisher.PublishAll()
	s.audit(request, adminID(request.Context()), "create", "rate_limit_policy", policy.ID, policy.Name)
	result, err := s.securityOverview(deploymentErr)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusCreated, result)
}

func (s *Server) updateRateLimitPolicy(response http.ResponseWriter, request *http.Request) {
	var input rateLimitPolicyRequest
	if !readJSON(response, request, &input) {
		return
	}
	policy, err := s.Store.UpdateRateLimitPolicy(request.PathValue("id"), rateLimitPolicyFromRequest(input))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeStoreError(response, err)
		} else {
			writeError(response, http.StatusBadRequest, err)
		}
		return
	}
	deploymentErr := s.Publisher.PublishAll()
	s.audit(request, adminID(request.Context()), "update", "rate_limit_policy", policy.ID, policy.Name)
	result, err := s.securityOverview(deploymentErr)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) deleteRateLimitPolicy(response http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	if err := s.Store.DeleteRateLimitPolicy(id); err != nil {
		writeStoreError(response, err)
		return
	}
	deploymentErr := s.Publisher.PublishAll()
	s.audit(request, adminID(request.Context()), "delete", "rate_limit_policy", id, "")
	result, err := s.securityOverview(deploymentErr)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) deploySecurityPolicies(response http.ResponseWriter, request *http.Request) {
	if err := s.Publisher.PublishAll(); err != nil {
		writeError(response, http.StatusConflict, err)
		return
	}
	s.audit(request, adminID(request.Context()), "deploy", "security_policy", "all", "rebuilt capable edge states")
	result, err := s.securityOverview(nil)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusAccepted, result)
}

func (s *Server) deleteSecurityBan(response http.ResponseWriter, request *http.Request) {
	ip := strings.TrimSpace(request.PathValue("ip"))
	if err := s.Store.DeleteSecurityBan(ip); err != nil {
		writeStoreError(response, err)
		return
	}
	s.invalidateEdgeSecurityRevision()
	s.audit(request, adminID(request.Context()), "unban", "security_ban", ip, "")
	result, err := s.securityOverview(nil)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) edgeSecurityEvents(response http.ResponseWriter, request *http.Request) {
	var batch domain.EdgeSecurityEventBatch
	if !readJSON(response, request, &batch) {
		return
	}
	if len(batch.Events) == 0 || len(batch.Events) > 200 {
		writeError(response, http.StatusBadRequest, errors.New("security event batch must contain 1-200 events"))
		return
	}
	nodeID := edgeNodeID(request.Context())
	accepted, err := s.Store.RecordSecurityEvents(nodeID, batch.Events)
	if err != nil {
		var inputError *store.SecurityEventInputError
		if errors.As(err, &inputError) {
			writeJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error(), "invalid_event_index": inputError.Index})
		} else {
			writeError(response, http.StatusInternalServerError, err)
		}
		return
	}
	for _, event := range batch.Events {
		if event.Action == domain.SecurityActionBan {
			s.invalidateEdgeSecurityRevision()
			break
		}
	}
	result := map[string]any{"accepted": accepted}
	if revision, revisionErr := s.cachedEdgeSecurityRevision(); revisionErr == nil {
		result["security_revision"] = revision
	}
	writeJSON(response, http.StatusAccepted, result)
}

func (s *Server) edgeSecurityBans(response http.ResponseWriter, request *http.Request) {
	bans, err := s.Store.ListActiveSecurityBans()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	edgeBans := make([]domain.EdgeSecurityBan, 0, len(bans))
	for _, ban := range bans {
		edgeBans = append(edgeBans, domain.EdgeSecurityBan{IP: ban.IP, ExpiresAt: ban.ExpiresAt})
	}
	revision := securityBansRevision(bans)
	if requestHasRevision(request, revision) {
		writeRevisionNotModified(response, revision)
		return
	}
	response.Header().Set("ETag", revisionETag(revision))
	writeJSON(response, http.StatusOK, domain.EdgeSecurityBanState{Bans: edgeBans, GeneratedAt: time.Now().UTC()})
}
