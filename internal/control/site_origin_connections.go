package control

import (
	"net/http"
	"time"

	"simple_cdn/internal/domain"
)

type siteOriginConnectionsResponse struct {
	SiteID string                     `json:"site_id"`
	Nodes  []siteOriginConnectionNode `json:"nodes"`
}

type siteOriginConnectionNode struct {
	NodeID            string                     `json:"node_id"`
	NodeName          string                     `json:"node_name"`
	PublicIPv4        string                     `json:"public_ipv4"`
	Status            domain.NodeStatus          `json:"status"`
	Available         bool                       `json:"available"`
	UnavailableReason string                     `json:"unavailable_reason,omitempty"`
	Stale             bool                       `json:"stale"`
	CollectedAt       *time.Time                 `json:"collected_at,omitempty"`
	Probes            []domain.OriginProbeStatus `json:"probes"`
}

func (s *Server) siteOriginConnections(response http.ResponseWriter, request *http.Request) {
	site, _, err := s.Store.GetSite(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	nodes, err := s.Store.ListNodes()
	if err != nil {
		writeStoreError(response, err)
		return
	}
	assignedNodeIDs := site.AssignedNodeIDs()
	assigned := make(map[string]struct{}, len(assignedNodeIDs))
	for _, nodeID := range assignedNodeIDs {
		assigned[nodeID] = struct{}{}
	}

	result := siteOriginConnectionsResponse{
		SiteID: site.ID,
		Nodes:  make([]siteOriginConnectionNode, 0, len(assignedNodeIDs)),
	}
	now := time.Now().UTC()
	for _, node := range nodes {
		if _, ok := assigned[node.ID]; !ok {
			continue
		}
		machine := s.nodeMachineStatus(node, now)
		item := siteOriginConnectionNode{
			NodeID: node.ID, NodeName: node.Name, PublicIPv4: node.PublicIPv4, Status: node.Status,
			Available: machine.Available, UnavailableReason: machine.UnavailableReason, Stale: machine.Stale,
			Probes: make([]domain.OriginProbeStatus, 0),
		}
		if machine.Report != nil {
			collectedAt := machine.Report.CollectedAt.UTC()
			item.CollectedAt = &collectedAt
			item.Probes = originProbesForSite(machine.Report.OriginProbes, site.ID)
		}
		result.Nodes = append(result.Nodes, item)
	}
	writeJSON(response, http.StatusOK, result)
}

func originProbesForSite(probes []domain.OriginProbeStatus, siteID string) []domain.OriginProbeStatus {
	filtered := make([]domain.OriginProbeStatus, 0)
	for _, probe := range probes {
		references := make([]domain.OriginPoolReference, 0, len(probe.References))
		for _, reference := range probe.References {
			if reference.SiteID == siteID {
				references = append(references, reference)
			}
		}
		if len(references) == 0 {
			continue
		}
		probe.References = references
		filtered = append(filtered, probe)
	}
	return filtered
}
