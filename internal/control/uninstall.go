package control

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"simple_cdn/internal/auth"
	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

const (
	uninstallDNSWait       = 75 * time.Second
	uninstallTokenLifetime = 30 * time.Minute
)

type nodeUninstallBlocker struct {
	Code     string `json:"code"`
	SiteID   string `json:"site_id,omitempty"`
	SiteName string `json:"site_name,omitempty"`
	Detail   string `json:"detail"`
}

type nodeUninstallStatusResponse struct {
	Node               domain.Node             `json:"node"`
	Job                *store.NodeUninstallJob `json:"job,omitempty"`
	Blockers           []nodeUninstallBlocker  `json:"blockers"`
	CanGenerateCommand bool                    `json:"can_generate_command"`
	ReadyInSeconds     int64                   `json:"ready_in_seconds"`
	UninstallCommand   string                  `json:"uninstall_command,omitempty"`
}

func (s *Server) uninstallEdgeScript(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	_, _ = response.Write([]byte(uninstallEdgeScript))
}

func (s *Server) prepareNodeUninstall(response http.ResponseWriter, request *http.Request) {
	nodeID := request.PathValue("id")
	node, err := s.Store.GetNode(nodeID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	if node.Status != domain.NodeDraining && node.Status != domain.NodeRevoked {
		writeError(response, http.StatusConflict, errors.New("pause scheduling or revoke authorization before preparing uninstall"))
		return
	}
	if job, err := s.Store.NodeUninstallJob(nodeID); err == nil && job.Status != store.NodeUninstallCanceled {
		s.writeNodeUninstallStatus(response, http.StatusOK, nodeID, "")
		return
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeStoreError(response, err)
		return
	}
	if s.DNS == nil {
		writeError(response, http.StatusServiceUnavailable, errors.New("DNS provider is required to prepare node uninstall"))
		return
	}

	sites, err := s.Store.ListSites()
	if err != nil {
		writeStoreError(response, err)
		return
	}
	publications, err := s.Store.ListSitePublications()
	if err != nil {
		writeStoreError(response, err)
		return
	}
	affected := make([]string, 0)
	affectedByID := make(map[string]bool)
	zones := make(map[string]bool)
	addAffected := func(site domain.Site) {
		zones[site.ZoneID] = true
		if siteHasNode(site, nodeID) && !affectedByID[site.ID] {
			affectedByID[site.ID] = true
			affected = append(affected, site.ID)
		}
	}
	for _, site := range sites {
		addAffected(site)
	}
	for _, publication := range publications {
		addAffected(publication.Site)
	}
	for zoneID := range zones {
		if err := s.DNS.RemoveNode(request.Context(), zoneID, nodeID); err != nil {
			writeError(response, http.StatusBadGateway, fmt.Errorf("remove node DNS records: %w", err))
			return
		}
	}
	readyAt := time.Now().UTC().Add(uninstallDNSWait)
	if _, err := s.Store.PrepareNodeUninstall(nodeID, affected, readyAt); err != nil {
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "prepare_uninstall", "node", nodeID, "DNS records removed; ready after "+readyAt.Format(time.RFC3339))
	s.writeNodeUninstallStatus(response, http.StatusCreated, nodeID, "")
}

func (s *Server) nodeUninstallStatus(response http.ResponseWriter, request *http.Request) {
	s.writeNodeUninstallStatus(response, http.StatusOK, request.PathValue("id"), "")
}

func (s *Server) createNodeUninstallCommand(response http.ResponseWriter, request *http.Request) {
	nodeID := request.PathValue("id")
	status, err := s.buildNodeUninstallStatus(nodeID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	if status.Job == nil {
		writeError(response, http.StatusConflict, errors.New("prepare node uninstall first"))
		return
	}
	if !status.CanGenerateCommand {
		writeJSON(response, http.StatusConflict, map[string]any{"error": "node uninstall prerequisites are not satisfied", "uninstall": status})
		return
	}
	if !validHTTPSURL(s.ControlURL) {
		writeError(response, http.StatusConflict, errors.New("CONTROL_PUBLIC_URL must be an HTTPS URL before generating an uninstall command"))
		return
	}
	token, err := auth.NewOpaqueToken(32)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	expiresAt := time.Now().UTC().Add(uninstallTokenLifetime)
	if _, err := s.Store.IssueNodeUninstallToken(nodeID, token, expiresAt); err != nil {
		writeStoreError(response, err)
		return
	}
	scriptURL := strings.TrimRight(s.ControlURL, "/") + "/uninstall-edge.sh"
	command := fmt.Sprintf(
		"curl -fsSL %q | sudo bash -s -- --control-url %q --token %q --node-id %q --node-name %s --node-ipv4 %q",
		scriptURL, s.ControlURL, token, status.Node.ID, shellSingleQuote(status.Node.Name), status.Node.PublicIPv4,
	)
	s.audit(request, adminID(request.Context()), "create_uninstall_token", "node", nodeID, "expires "+expiresAt.Format(time.RFC3339))
	s.writeNodeUninstallStatus(response, http.StatusCreated, nodeID, command)
}

func (s *Server) cancelNodeUninstall(response http.ResponseWriter, request *http.Request) {
	nodeID := request.PathValue("id")
	if _, err := s.Store.CancelNodeUninstall(nodeID); err != nil {
		if errors.Is(err, store.ErrUninstallActive) {
			writeError(response, http.StatusConflict, err)
			return
		}
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "cancel_uninstall", "node", nodeID, "")
	s.writeNodeUninstallStatus(response, http.StatusOK, nodeID, "")
}

func (s *Server) forceCompleteNodeUninstall(response http.ResponseWriter, request *http.Request) {
	nodeID := request.PathValue("id")
	node, err := s.Store.GetNode(nodeID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	confirmation, ok := readNodeConfirmation(response, request)
	if !ok {
		return
	}
	if confirmation != node.Name {
		writeError(response, http.StatusBadRequest, errors.New("confirmation must exactly match the node name"))
		return
	}
	status, err := s.buildNodeUninstallStatus(nodeID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	if status.Job == nil || !nodeUninstallJobActive(status.Job.Status) || len(status.Blockers) != 0 || status.ReadyInSeconds > 0 {
		writeJSON(response, http.StatusConflict, map[string]any{"error": "node uninstall prerequisites are not satisfied", "uninstall": status})
		return
	}
	detail := "forced completion; remote cleanup was not verified"
	if _, err := s.Store.ForceCompleteNodeUninstall(nodeID, detail); err != nil {
		writeStoreError(response, err)
		return
	}
	s.audit(request, adminID(request.Context()), "force_complete_uninstall", "node", nodeID, detail)
	s.writeNodeUninstallStatus(response, http.StatusOK, nodeID, "")
}

func (s *Server) deleteNode(response http.ResponseWriter, request *http.Request) {
	nodeID := request.PathValue("id")
	node, err := s.Store.GetNode(nodeID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	confirmation, ok := readNodeConfirmation(response, request)
	if !ok {
		return
	}
	if confirmation != node.Name {
		writeError(response, http.StatusBadRequest, errors.New("confirmation must exactly match the node name"))
		return
	}
	assigned, err := s.assignedSites(nodeID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	if len(assigned) != 0 {
		writeError(response, http.StatusConflict, errors.New("remove the node from all sites before deleting its record"))
		return
	}
	allowed := node.Status == domain.NodeUninstalled
	if node.Status == domain.NodePending && node.LastHeartbeatAt == nil && node.AppliedVersion == 0 {
		allowed, err = s.Store.PendingNodeCanBeDeleted(nodeID)
		if err != nil {
			writeStoreError(response, err)
			return
		}
	}
	if !allowed {
		writeError(response, http.StatusConflict, errors.New("only uninstalled or never-enrolled pending nodes can be deleted"))
		return
	}
	if err := s.Store.DeleteNode(nodeID); err != nil {
		writeStoreError(response, err)
		return
	}
	s.machineStatusMu.Lock()
	delete(s.machineStatuses, nodeID)
	s.machineStatusMu.Unlock()
	s.audit(request, adminID(request.Context()), "delete", "node", nodeID, node.Name)
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) startNodeUninstall(response http.ResponseWriter, request *http.Request) {
	token, ok := uninstallBearerToken(response, request)
	if !ok {
		return
	}
	job, err := s.Store.StartNodeUninstall(token)
	if err != nil {
		writeUninstallTokenError(response, err)
		return
	}
	if job.Status == store.NodeUninstallRunning {
		s.audit(request, "uninstall:"+job.NodeID, "start_uninstall", "node", job.NodeID, "")
	}
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) failNodeUninstall(response http.ResponseWriter, request *http.Request) {
	token, ok := uninstallBearerToken(response, request)
	if !ok {
		return
	}
	detail, err := io.ReadAll(io.LimitReader(request.Body, 2001))
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	if len(detail) > 2000 {
		writeError(response, http.StatusRequestEntityTooLarge, errors.New("uninstall failure detail is too long"))
		return
	}
	job, err := s.Store.FailNodeUninstall(token, strings.TrimSpace(string(detail)))
	if err != nil {
		writeUninstallTokenError(response, err)
		return
	}
	if job.Status == store.NodeUninstallFailed {
		s.audit(request, "uninstall:"+job.NodeID, "fail_uninstall", "node", job.NodeID, job.Detail)
	}
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) completeNodeUninstall(response http.ResponseWriter, request *http.Request) {
	token, ok := uninstallBearerToken(response, request)
	if !ok {
		return
	}
	job, err := s.Store.CompleteNodeUninstall(token)
	if err != nil {
		writeUninstallTokenError(response, err)
		return
	}
	if job.Status == store.NodeUninstallSucceeded {
		s.audit(request, "uninstall:"+job.NodeID, "complete_uninstall", "node", job.NodeID, "remote cleanup verified")
	}
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) writeNodeUninstallStatus(response http.ResponseWriter, statusCode int, nodeID, command string) {
	status, err := s.buildNodeUninstallStatus(nodeID)
	if err != nil {
		writeStoreError(response, err)
		return
	}
	status.UninstallCommand = command
	writeJSON(response, statusCode, status)
}

func (s *Server) buildNodeUninstallStatus(nodeID string) (nodeUninstallStatusResponse, error) {
	node, err := s.Store.GetNode(nodeID)
	if err != nil {
		return nodeUninstallStatusResponse{}, err
	}
	result := nodeUninstallStatusResponse{Node: node, Blockers: []nodeUninstallBlocker{}}
	job, err := s.Store.NodeUninstallJob(nodeID)
	if errors.Is(err, store.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return nodeUninstallStatusResponse{}, err
	}
	result.Job = &job
	if job.Status == store.NodeUninstallCanceled {
		return result, nil
	}

	sites, err := s.Store.ListSites()
	if err != nil {
		return nodeUninstallStatusResponse{}, err
	}
	sitesByID := make(map[string]domain.Site, len(sites))
	for _, site := range sites {
		sitesByID[site.ID] = site
		if siteHasNode(site, nodeID) {
			result.Blockers = append(result.Blockers, nodeUninstallBlocker{Code: "still_assigned", SiteID: site.ID, SiteName: site.Name, Detail: "remove this node from the site"})
		}
	}
	publications, err := s.Store.ListSitePublications()
	if err != nil {
		return nodeUninstallStatusResponse{}, err
	}
	publicationsByID := make(map[string]domain.Site, len(publications))
	for _, publication := range publications {
		publicationsByID[publication.Site.ID] = publication.Site
	}
	for _, siteID := range job.AffectedSiteIDs {
		site, found := sitesByID[siteID]
		if !found || siteHasNode(site, nodeID) {
			continue
		}
		published, hasPublication := publicationsByID[siteID]
		if hasPublication && siteHasNode(published, nodeID) {
			result.Blockers = append(result.Blockers, nodeUninstallBlocker{Code: "site_not_published", SiteID: site.ID, SiteName: site.Name, Detail: "publish the site after removing this node"})
			continue
		}
		if !hasPublication && !site.Published {
			result.Blockers = append(result.Blockers, nodeUninstallBlocker{Code: "site_not_published", SiteID: site.ID, SiteName: site.Name, Detail: "publish the site after removing this node"})
			continue
		}
		liveSite := site
		if hasPublication {
			liveSite = published
		}
		if !liveSite.Enabled {
			continue
		}
		active := 0
		for _, assignedNodeID := range liveSite.AssignedNodeIDs() {
			assignedNode, nodeErr := s.Store.GetNode(assignedNodeID)
			if nodeErr == nil && assignedNode.Status == domain.NodeActive {
				active++
			}
		}
		if active == 0 {
			result.Blockers = append(result.Blockers, nodeUninstallBlocker{Code: "no_active_node", SiteID: site.ID, SiteName: site.Name, Detail: "assign and publish another active node, or disable the site"})
		}
	}
	if remaining := time.Until(job.ReadyAt); remaining > 0 {
		result.ReadyInSeconds = int64((remaining + time.Second - 1) / time.Second)
	}
	result.CanGenerateCommand = len(result.Blockers) == 0 && result.ReadyInSeconds == 0 &&
		(job.Status == store.NodeUninstallPreparing || job.Status == store.NodeUninstallReady || job.Status == store.NodeUninstallFailed)
	return result, nil
}

func (s *Server) assignedSites(nodeID string) ([]domain.Site, error) {
	sites, err := s.Store.ListSites()
	if err != nil {
		return nil, err
	}
	publications, err := s.Store.ListSitePublications()
	if err != nil {
		return nil, err
	}
	assigned := make([]domain.Site, 0)
	assignedByID := make(map[string]bool)
	sitesByID := make(map[string]domain.Site, len(sites))
	for _, site := range sites {
		sitesByID[site.ID] = site
		if siteHasNode(site, nodeID) {
			assigned = append(assigned, site)
			assignedByID[site.ID] = true
		}
	}
	for _, publication := range publications {
		if !siteHasNode(publication.Site, nodeID) || assignedByID[publication.Site.ID] {
			continue
		}
		if draft, found := sitesByID[publication.Site.ID]; found {
			assigned = append(assigned, draft)
		} else {
			assigned = append(assigned, publication.Site)
		}
		assignedByID[publication.Site.ID] = true
	}
	return assigned, nil
}

func siteHasNode(site domain.Site, nodeID string) bool {
	for _, assignedNodeID := range site.AssignedNodeIDs() {
		if assignedNodeID == nodeID {
			return true
		}
	}
	return false
}

func nodeUninstallJobActive(status store.NodeUninstallJobStatus) bool {
	return status == store.NodeUninstallPreparing || status == store.NodeUninstallReady || status == store.NodeUninstallRunning || status == store.NodeUninstallFailed
}

func readNodeConfirmation(response http.ResponseWriter, request *http.Request) (string, bool) {
	var input struct {
		Confirmation string `json:"confirmation"`
	}
	if !readJSON(response, request, &input) {
		return "", false
	}
	return input.Confirmation, true
}

func uninstallBearerToken(response http.ResponseWriter, request *http.Request) (string, bool) {
	scheme, token, found := strings.Cut(strings.TrimSpace(request.Header.Get("Authorization")), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		writeError(response, http.StatusUnauthorized, errors.New("valid uninstall token required"))
		return "", false
	}
	return strings.TrimSpace(token), true
}

func writeUninstallTokenError(response http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrTokenInvalid) || errors.Is(err, store.ErrNotFound) {
		writeError(response, http.StatusUnauthorized, errors.New("uninstall token is invalid or expired"))
		return
	}
	writeError(response, http.StatusConflict, err)
}
