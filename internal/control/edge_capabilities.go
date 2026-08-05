package control

import (
	"errors"
	"slices"
	"strings"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/nginx"
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
	staticAssetsNeedRebuild, err := s.staticAssetStateNeedsRebuild(nodeID, state, capabilities)
	if err != nil {
		return err
	}
	cacheWarmupsNeedRebuild, err := s.cacheWarmupStateNeedsRebuild(nodeID, state, capabilities)
	if err != nil {
		return err
	}
	if !http3StateNeedsRebuild(state, http3Wanted) && !originPoolStateNeedsRebuild(state, capabilities) && !originHTTP2StateNeedsRebuild(state, capabilities) && !requestTracingStateNeedsRebuild(state, capabilities) && !securityRuntimeStateNeedsRebuild(state, capabilities) && !compressionRuntimeStateNeedsRebuild(state, capabilities) && !staticAssetsNeedRebuild && !cacheWarmupsNeedRebuild && !runtimeOptimizationStateNeedsRebuild(state) {
		return nil
	}
	return s.Publisher.PublishNode(nodeID)
}

func (s *Server) cacheWarmupStateNeedsRebuild(nodeID string, state domain.DesiredState, capabilities []string) (bool, error) {
	publications, err := s.Store.ListSitePublications()
	if err != nil {
		return false, err
	}
	sites := make([]domain.Site, 0)
	for _, publication := range publications {
		if siteHasNode(publication.Site, nodeID) {
			sites = append(sites, publication.Site)
		}
	}
	expected, err := cacheWarmupsForNode(sites, capabilities)
	if err != nil {
		return false, err
	}
	return !cacheWarmupsEqual(state.CacheWarmups, expected), nil
}

func (s *Server) staticAssetStateNeedsRebuild(nodeID string, state domain.DesiredState, capabilities []string) (bool, error) {
	if !slices.Contains(capabilities, domain.EdgeCapabilityStaticAssets) {
		return len(state.StaticAssets) != 0, nil
	}
	publications, err := s.Store.ListSitePublications()
	if err != nil {
		return false, err
	}
	sites := make([]domain.Site, 0)
	for _, publication := range publications {
		if siteHasNode(publication.Site, nodeID) {
			sites = append(sites, publication.Site)
		}
	}
	references, err := s.Store.ListStaticAssetReferences()
	if err != nil {
		return false, err
	}
	expected, err := staticAssetsForNode(references, sites, capabilities)
	if err != nil {
		return false, err
	}
	return !slices.Equal(state.StaticAssets, expected), nil
}

func securityRuntimeStateNeedsRebuild(state domain.DesiredState, capabilities []string) bool {
	wafWanted := slices.Contains(capabilities, domain.EdgeCapabilityWAFChain)
	powWanted := wafWanted && slices.Contains(capabilities, domain.EdgeCapabilityPOWChallenge)
	wafConfigured := strings.Contains(state.NginxConfig, nginx.WAFRuntimeMarker)
	powConfigured := strings.Contains(state.NginxConfig, nginx.POWRuntimeMarker)
	return wafWanted != wafConfigured || powWanted != powConfigured
}

func compressionRuntimeStateNeedsRebuild(state domain.DesiredState, capabilities []string) bool {
	wanted := slices.Contains(capabilities, domain.EdgeCapabilityCompression) && strings.Contains(state.NginxConfig, "log_format cdn_json")
	configured := strings.Contains(state.NginxConfig, nginx.CompressionRuntimeMarker)
	return wanted != configured
}

func requestTracingStateNeedsRebuild(state domain.DesiredState, capabilities []string) bool {
	hasHTTPOrigin := strings.Contains(state.NginxConfig, "proxy_pass ") || strings.Contains(state.NginxConfig, "grpc_pass ")
	return hasHTTPOrigin && slices.Contains(capabilities, domain.EdgeCapabilityRequestTracing) && !nginx.HasRequestTracing(state.NginxConfig)
}

func runtimeOptimizationStateNeedsRebuild(state domain.DesiredState) bool {
	if !strings.Contains(state.NginxMainConfig, "pcre_jit on;") ||
		!strings.Contains(state.NginxMainConfig, "worker_shutdown_timeout 1h;") {
		return true
	}
	if strings.Contains(state.NginxConfig, "listen 443 ssl") &&
		!strings.Contains(state.NginxConfig, "ssl_session_timeout 30m;") {
		return true
	}
	return strings.Contains(state.NginxConfig, "listen 443 quic") &&
		!strings.Contains(state.NginxConfig, "quic_host_key ")
}

func originHTTP2StateNeedsRebuild(state domain.DesiredState, capabilities []string) bool {
	configured := strings.Contains(state.NginxConfig, "proxy_http_version 2;")
	capable := slices.Contains(capabilities, domain.EdgeCapabilityOriginHTTP2)
	return configured && !capable
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
