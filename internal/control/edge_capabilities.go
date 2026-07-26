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
	if !http3StateNeedsRebuild(state, capabilities) {
		return nil
	}
	return s.Publisher.PublishNode(nodeID)
}

func http3StateNeedsRebuild(state domain.DesiredState, capabilities []string) bool {
	configured := strings.Contains(state.NginxConfig, "listen 443 quic")
	declared := slices.Contains(state.PublicUDPPorts, 443)
	hasHTTPSSite := configured || strings.Contains(state.NginxConfig, "listen 443 ssl;")
	if !hasHTTPSSite && len(state.PublicUDPPorts) == 0 {
		return false
	}
	wanted := false
	for _, capability := range capabilities {
		if strings.TrimSpace(capability) == domain.EdgeCapabilityHTTP3 {
			wanted = true
			break
		}
	}
	if wanted {
		return !configured || !declared
	}
	return configured || len(state.PublicUDPPorts) != 0
}
