package control

import (
	"errors"
	"net/http"
	"time"

	"simple_cdn/internal/domain"
)

type publishOverviewResponse struct {
	Sites   []publishSiteOverview    `json:"sites"`
	History []publishHistoryOverview `json:"history"`
}

type publishSiteOverview struct {
	SiteID        string                 `json:"site_id"`
	SiteName      string                 `json:"site_name"`
	Domains       []string               `json:"domains"`
	ConfigVersion int64                  `json:"config_version"`
	Published     bool                   `json:"published"`
	Enabled       bool                   `json:"enabled"`
	Deleting      bool                   `json:"deleting"`
	IPv6Enabled   bool                   `json:"ipv6_enabled"`
	HTTP3Enabled  bool                   `json:"http3_enabled"`
	TCPEnabled    bool                   `json:"tcp_enabled"`
	Task          *domain.DeploymentTask `json:"task"`
	Nodes         []publishNodeOverview  `json:"nodes"`
}

type publishNodeOverview struct {
	NodeID              string            `json:"node_id"`
	NodeName            string            `json:"node_name"`
	Role                string            `json:"role"`
	NodeStatus          domain.NodeStatus `json:"node_status"`
	PublicIPv4          string            `json:"public_ipv4,omitempty"`
	PublicIPv6          string            `json:"public_ipv6,omitempty"`
	AgentVersion        string            `json:"agent_version,omitempty"`
	AgentSHA256         string            `json:"agent_sha256,omitempty"`
	NginxVersion        string            `json:"nginx_version,omitempty"`
	NginxSHA256         string            `json:"nginx_sha256,omitempty"`
	Capabilities        []string          `json:"capabilities"`
	TargetVersion       int64             `json:"target_version"`
	DesiredVersion      int64             `json:"desired_version"`
	AppliedVersion      int64             `json:"applied_version"`
	ConfigurationStatus string            `json:"configuration_status"`
	DriftReason         string            `json:"drift_reason,omitempty"`
	NodeLastError       string            `json:"node_last_error,omitempty"`
	ErrorCode           string            `json:"error_code,omitempty"`
	Detail              string            `json:"detail,omitempty"`
	ReportedAt          *time.Time        `json:"reported_at,omitempty"`
	IPv4DNSEligible     bool              `json:"ipv4_dns_eligible"`
	IPv4LastCheckedAt   *time.Time        `json:"ipv4_last_checked_at,omitempty"`
	IPv4LastError       string            `json:"ipv4_last_error,omitempty"`
	IPv6DNSEligible     bool              `json:"ipv6_dns_eligible"`
	IPv6LastCheckedAt   *time.Time        `json:"ipv6_last_checked_at,omitempty"`
	IPv6LastError       string            `json:"ipv6_last_error,omitempty"`
}

type publishHistoryOverview struct {
	SiteID   string                     `json:"site_id"`
	SiteName string                     `json:"site_name"`
	Domains  []string                   `json:"domains"`
	Task     domain.DeploymentTask      `json:"task"`
	Nodes    []domain.PublishNodeResult `json:"nodes"`
}

func (s *Server) publishOverview(response http.ResponseWriter, _ *http.Request) {
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
	latest, history, err := s.Store.PublishStatuses(50)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}

	nodesByID := make(map[string]domain.Node, len(nodes))
	for _, node := range nodes {
		nodesByID[node.ID] = node
	}
	latestBySite := make(map[string]domain.PublishStatus, len(latest))
	for _, status := range latest {
		if status.Task != nil {
			latestBySite[status.Task.SiteID] = status
		}
	}
	sitesByID := make(map[string]domain.Site, len(sites))
	result := publishOverviewResponse{
		Sites:   make([]publishSiteOverview, 0, len(sites)),
		History: make([]publishHistoryOverview, 0, len(history)),
	}
	for _, site := range sites {
		sitesByID[site.ID] = site
		status := latestBySite[site.ID]
		if status.Task != nil && !publishTaskMatchesSite(*status.Task, site) {
			status = domain.PublishStatus{}
		}
		overview, err := s.buildPublishSiteOverview(site, status, nodesByID)
		if err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
		result.Sites = append(result.Sites, overview)
	}
	for _, status := range history {
		if status.Task == nil {
			continue
		}
		site := sitesByID[status.Task.SiteID]
		siteName := site.Name
		if siteName == "" {
			siteName = status.Task.SiteID
		}
		result.History = append(result.History, publishHistoryOverview{
			SiteID: status.Task.SiteID, SiteName: siteName,
			Domains: append([]string{}, site.Domains...), Task: *status.Task,
			Nodes: append([]domain.PublishNodeResult(nil), status.Nodes...),
		})
	}
	writeJSON(response, http.StatusOK, result)
}

func publishTaskMatchesSite(task domain.DeploymentTask, site domain.Site) bool {
	if site.Published || task.Status == domain.TaskQueued || task.Status == domain.TaskDispatching || task.Status == domain.TaskApplying {
		return true
	}
	return !task.CreatedAt.Before(site.UpdatedAt)
}

func publishNodeDriftReason(node domain.Node, desiredVersion int64, publishing bool) string {
	if node.Status != domain.NodeActive {
		return "node_inactive"
	}
	if desiredVersion < 1 {
		return "desired_state_missing"
	}
	if node.AppliedVersion < desiredVersion {
		return "version_behind"
	}
	if publishing {
		return "publication_active"
	}
	return ""
}

func (s *Server) buildPublishSiteOverview(site domain.Site, status domain.PublishStatus, nodesByID map[string]domain.Node) (publishSiteOverview, error) {
	overview := publishSiteOverview{
		SiteID: site.ID, SiteName: site.Name, Domains: append([]string{}, site.Domains...),
		ConfigVersion: site.ConfigVersion, Published: site.Published, Enabled: site.Enabled,
		Deleting: site.Deleting, IPv6Enabled: site.IPv6Enabled, HTTP3Enabled: site.HTTP3Enabled,
		TCPEnabled: site.TCPOnly || len(site.TCPForwards) > 0, Task: status.Task,
		Nodes: make([]publishNodeOverview, 0, len(site.AssignedNodeIDs())+len(status.Nodes)),
	}
	resultsByNode := make(map[string]domain.PublishNodeResult, len(status.Nodes))
	for _, result := range status.Nodes {
		resultsByNode[result.NodeID] = result
	}
	roles := make(map[string]string, len(site.Nodes)+len(site.BackupNodes))
	orderedNodeIDs := make([]string, 0, len(site.Nodes)+len(site.BackupNodes)+len(status.Nodes))
	seen := make(map[string]bool, cap(orderedNodeIDs))
	appendNode := func(nodeID, role string) {
		if nodeID == "" || seen[nodeID] {
			return
		}
		seen[nodeID] = true
		roles[nodeID] = role
		orderedNodeIDs = append(orderedNodeIDs, nodeID)
	}
	for _, nodeID := range site.Nodes {
		appendNode(nodeID, "primary")
	}
	for _, nodeID := range site.BackupNodes {
		appendNode(nodeID, "backup")
	}
	for _, result := range status.Nodes {
		appendNode(result.NodeID, "removed")
	}
	for _, nodeID := range orderedNodeIDs {
		node, found := nodesByID[nodeID]
		if !found {
			continue
		}
		result, targeted := resultsByNode[nodeID]
		desiredVersion, err := s.Store.DesiredVersion(nodeID)
		if err != nil {
			return publishSiteOverview{}, err
		}
		nodeHealth, err := s.Store.NodeHealth(nodeID)
		if err != nil {
			return publishSiteOverview{}, err
		}
		ipv4Health, err := s.Store.SiteNodeHealth(site.ID, nodeID)
		if err != nil {
			return publishSiteOverview{}, err
		}
		ipv6Health, err := s.Store.SiteNodeIPv6Health(site.ID, nodeID)
		if err != nil {
			return publishSiteOverview{}, err
		}
		publishing, err := s.Store.HasActiveNodePublication(nodeID)
		if err != nil {
			return publishSiteOverview{}, err
		}
		configurationStatus := "not_targeted"
		if targeted {
			configurationStatus = string(result.Status)
		} else if site.Published && desiredVersion > 0 && node.AppliedVersion >= desiredVersion {
			configurationStatus = string(domain.PublishNodeSucceeded)
		}
		configurationReady := desiredVersion > 0 && node.AppliedVersion >= desiredVersion && !publishing
		ipv4Eligible := site.Published && site.Enabled && !site.Deleting && node.Status == domain.NodeActive && configurationReady && nodeHealth.DNSEligible && ipv4Health.DNSEligible
		ipv4Error := ipv4Health.LastError
		if nodeHealth.LastError != "" {
			ipv4Error = nodeHealth.LastError
		}
		overview.Nodes = append(overview.Nodes, publishNodeOverview{
			NodeID: node.ID, NodeName: node.Name, Role: roles[node.ID], NodeStatus: node.Status,
			PublicIPv4: node.PublicIPv4, PublicIPv6: node.PublicIPv6, AgentVersion: node.AgentVersion,
			AgentSHA256: node.AgentSHA256, NginxVersion: node.NginxVersion, NginxSHA256: node.NginxSHA256,
			Capabilities: append([]string{}, node.Capabilities...), TargetVersion: result.TargetVersion,
			DesiredVersion: desiredVersion, AppliedVersion: node.AppliedVersion,
			ConfigurationStatus: configurationStatus, DriftReason: publishNodeDriftReason(node, desiredVersion, publishing),
			NodeLastError: node.LastError, ErrorCode: result.ErrorCode,
			Detail: result.Detail, ReportedAt: result.ReportedAt,
			IPv4DNSEligible: ipv4Eligible, IPv4LastCheckedAt: ipv4Health.LastCheckedAt, IPv4LastError: ipv4Error,
			IPv6DNSEligible:   ipv4Eligible && site.IPv6Enabled && node.PublicIPv6 != "" && ipv6Health.DNSEligible,
			IPv6LastCheckedAt: ipv6Health.LastCheckedAt, IPv6LastError: ipv6Health.LastError,
		})
	}
	return overview, nil
}

func (s *Server) retryPublish(response http.ResponseWriter, request *http.Request) {
	task, err := s.Publisher.RetryFailedPublish(request.PathValue("id"))
	if err != nil {
		if errors.Is(err, errPublishRetryNotReady) || errors.Is(err, errPublishRetryUnpublished) {
			writeError(response, http.StatusConflict, err)
			return
		}
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "retry_publish", "site", request.PathValue("id"), task.ID)
	writeJSON(response, http.StatusAccepted, task)
}
