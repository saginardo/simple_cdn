package control

import (
	"errors"
	"slices"
	"strings"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

func (s *Server) reconcileEdgeRuntimeCapabilities(nodeID string, capabilities []string) error {
	state, _, err := s.Store.NodeState(nodeID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	http3Wanted, err := s.nodeWantsHTTP3(nodeID, capabilities)
	if err != nil {
		return err
	}
	if !http3StateNeedsRebuild(state, http3Wanted) && !originPoolStateNeedsRebuild(state, capabilities) {
		return nil
	}
	return s.Publisher.PublishNode(nodeID)
}

func (s *Server) nodeWantsHTTP3(nodeID string, capabilities []string) (bool, error) {
	capable := false
	for _, capability := range capabilities {
		if strings.TrimSpace(capability) == domain.EdgeCapabilityHTTP3 {
			capable = true
			break
		}
	}
	if !capable {
		return false, nil
	}
	publications, err := s.Store.ListSitePublications()
	if err != nil {
		return false, err
	}
	for _, publication := range publications {
		site := publication.Site
		if site.Enabled && !site.TCPOnly && site.HTTP3Enabled && siteHasNode(site, nodeID) {
			return true, nil
		}
	}
	return false, nil
}

func originPoolStateNeedsRebuild(state domain.DesiredState, capabilities []string) bool {
	hasHTTPOrigin := strings.Contains(state.NginxConfig, "proxy_pass ") || strings.Contains(state.NginxConfig, "grpc_pass ")
	if !hasHTTPOrigin {
		return false
	}
	wanted := slices.Contains(capabilities, domain.EdgeCapabilityOriginConnection)
	return wanted != (len(state.OriginPools) > 0)
}

func http3StateNeedsRebuild(state domain.DesiredState, wanted bool) bool {
	configured := strings.Contains(state.NginxConfig, "listen 443 quic")
	declared := slices.Contains(state.PublicUDPPorts, 443)
	return configured != wanted || declared != wanted
}
