package control

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"simple_cdn/internal/domain"
)

type wireGuardOriginServiceStatus string

const (
	wireGuardOriginServiceUnknown     wireGuardOriginServiceStatus = "unknown"
	wireGuardOriginServiceHealthy     wireGuardOriginServiceStatus = "healthy"
	wireGuardOriginServicePartial     wireGuardOriginServiceStatus = "partial"
	wireGuardOriginServiceDegraded    wireGuardOriginServiceStatus = "degraded"
	wireGuardOriginServiceUnreachable wireGuardOriginServiceStatus = "unreachable"
)

type wireGuardTunnelDetailResponse struct {
	Tunnel         domain.WireGuardTunnel   `json:"tunnel"`
	OriginServices []wireGuardOriginService `json:"origin_services"`
}

type wireGuardOriginService struct {
	Port           int                               `json:"port"`
	Scheme         string                            `json:"scheme"`
	HTTPVersion    domain.OriginHTTPVersion          `json:"http_version,omitempty"`
	Status         wireGuardOriginServiceStatus      `json:"status"`
	ReachableNodes int                               `json:"reachable_nodes"`
	ObservedNodes  int                               `json:"observed_nodes"`
	TotalNodes     int                               `json:"total_nodes"`
	LastReportedAt *time.Time                        `json:"last_reported_at,omitempty"`
	Sites          []wireGuardOriginServiceReference `json:"sites"`
}

type wireGuardOriginServiceReference struct {
	SiteID    string   `json:"site_id"`
	SiteName  string   `json:"site_name"`
	Domains   []string `json:"domains"`
	Role      string   `json:"role"`
	Published bool     `json:"published"`
}

type wireGuardOriginServiceKey struct {
	Port        int
	Scheme      string
	HTTPVersion domain.OriginHTTPVersion
}

type wireGuardOriginServiceBuilder struct {
	service            wireGuardOriginService
	references         map[string]struct{}
	expectedReferences map[string]map[string]struct{}
}

func (s *Server) getWireGuardTunnel(response http.ResponseWriter, request *http.Request) {
	tunnel, err := s.Store.GetWireGuardTunnel(request.PathValue("id"))
	if err != nil {
		writeStoreError(response, err)
		return
	}
	services, err := s.wireGuardOriginServices(tunnel, time.Now().UTC())
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, wireGuardTunnelDetailResponse{
		Tunnel: tunnel, OriginServices: services,
	})
}

func (s *Server) wireGuardOriginServices(tunnel domain.WireGuardTunnel, at time.Time) ([]wireGuardOriginService, error) {
	sites, err := s.Store.WireGuardTunnelReferences(tunnel.ID)
	if err != nil {
		return nil, err
	}
	nodes, err := s.Store.ListNodes()
	if err != nil {
		return nil, err
	}
	nodesByID := make(map[string]domain.Node, len(nodes))
	for _, node := range nodes {
		nodesByID[node.ID] = node
	}

	builders := make(map[wireGuardOriginServiceKey]*wireGuardOriginServiceBuilder)
	for _, site := range sites {
		if !site.Enabled || site.TCPOnly {
			continue
		}
		origins := []struct {
			role   string
			origin *domain.Origin
		}{
			{role: "primary", origin: &site.PrimaryOrigin},
			{role: "backup", origin: site.BackupOrigin},
		}
		for _, candidate := range origins {
			if candidate.origin == nil || !candidate.origin.Enabled || candidate.origin.WireGuardTunnelID != tunnel.ID {
				continue
			}
			key, err := wireGuardServiceKey(*candidate.origin)
			if err != nil {
				return nil, fmt.Errorf("site %s %s origin: %w", site.Name, candidate.role, err)
			}
			builder := builders[key]
			if builder == nil {
				builder = &wireGuardOriginServiceBuilder{
					service: wireGuardOriginService{
						Port: key.Port, Scheme: key.Scheme, HTTPVersion: key.HTTPVersion,
						Sites: make([]wireGuardOriginServiceReference, 0),
					},
					references:         make(map[string]struct{}),
					expectedReferences: make(map[string]map[string]struct{}),
				}
				builders[key] = builder
			}
			referenceID := wireGuardServiceReferenceID(site.ID, candidate.role)
			if _, exists := builder.references[referenceID]; !exists {
				builder.references[referenceID] = struct{}{}
				builder.service.Sites = append(builder.service.Sites, wireGuardOriginServiceReference{
					SiteID: site.ID, SiteName: site.Name, Domains: append([]string(nil), site.Domains...),
					Role: candidate.role, Published: site.Published,
				})
			}
			for _, nodeID := range site.Nodes {
				if builder.expectedReferences[nodeID] == nil {
					builder.expectedReferences[nodeID] = make(map[string]struct{})
				}
				builder.expectedReferences[nodeID][referenceID] = struct{}{}
			}
		}
	}

	services := make([]wireGuardOriginService, 0, len(builders))
	for key, builder := range builders {
		for nodeID, expected := range builder.expectedReferences {
			node, found := nodesByID[nodeID]
			if !found {
				continue
			}
			machine := s.nodeMachineStatus(node, at)
			if machine.Report == nil {
				continue
			}
			observedReferences := make(map[string]bool, len(expected))
			referenceHealth := make(map[string]bool, len(expected))
			matched := false
			for _, probe := range machine.Report.OriginProbes {
				references := matchingWireGuardProbeReferences(probe, tunnel.OriginAddress, key, expected)
				if len(references) == 0 {
					continue
				}
				matched = true
				healthy := probe.Healthy && probe.CircuitState == domain.OriginCircuitClosed
				for _, referenceID := range references {
					if observedReferences[referenceID] {
						referenceHealth[referenceID] = referenceHealth[referenceID] && healthy
						continue
					}
					observedReferences[referenceID] = true
					referenceHealth[referenceID] = healthy
				}
			}
			if matched {
				collectedAt := machine.Report.CollectedAt.UTC()
				if builder.service.LastReportedAt == nil || collectedAt.After(*builder.service.LastReportedAt) {
					builder.service.LastReportedAt = &collectedAt
				}
			}
			if machine.Stale || !matched {
				continue
			}
			builder.service.ObservedNodes++
			reachable := len(observedReferences) == len(expected)
			if reachable {
				for referenceID := range expected {
					if !referenceHealth[referenceID] {
						reachable = false
						break
					}
				}
			}
			if reachable {
				builder.service.ReachableNodes++
			}
		}
		builder.service.TotalNodes = len(builder.expectedReferences)
		builder.service.Status = wireGuardServiceStatus(builder.service)
		sort.Slice(builder.service.Sites, func(i, j int) bool {
			left, right := builder.service.Sites[i], builder.service.Sites[j]
			if left.SiteName != right.SiteName {
				return left.SiteName < right.SiteName
			}
			return left.Role < right.Role
		})
		services = append(services, builder.service)
	}
	sort.Slice(services, func(i, j int) bool {
		left, right := services[i], services[j]
		if left.Port != right.Port {
			return left.Port < right.Port
		}
		if left.Scheme != right.Scheme {
			return left.Scheme < right.Scheme
		}
		return left.HTTPVersion < right.HTTPVersion
	})
	return services, nil
}

func wireGuardServiceKey(origin domain.Origin) (wireGuardOriginServiceKey, error) {
	parsed, err := url.Parse(origin.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return wireGuardOriginServiceKey{}, fmt.Errorf("invalid origin URL")
	}
	scheme := domain.ProxyScheme(strings.ToLower(parsed.Scheme))
	port := 80
	if domain.OriginUsesTLS(strings.ToLower(parsed.Scheme)) {
		port = 443
	}
	if explicit := parsed.Port(); explicit != "" {
		port, err = strconv.Atoi(explicit)
		if err != nil || port < 1 || port > 65535 {
			return wireGuardOriginServiceKey{}, fmt.Errorf("invalid origin port")
		}
	}
	version := domain.EffectiveOriginHTTPVersion(origin)
	if domain.IsGRPCScheme(scheme) {
		version = ""
	}
	return wireGuardOriginServiceKey{Port: port, Scheme: scheme, HTTPVersion: version}, nil
}

func matchingWireGuardProbeReferences(
	probe domain.OriginProbeStatus,
	originAddress string,
	key wireGuardOriginServiceKey,
	expected map[string]struct{},
) []string {
	host, portText, err := net.SplitHostPort(probe.Address)
	if err != nil || !wireGuardServiceHostsEqual(host, originAddress) {
		return nil
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port != key.Port || probe.Scheme != key.Scheme || probe.HTTPVersion != key.HTTPVersion {
		return nil
	}
	references := make([]string, 0, len(probe.References))
	for _, reference := range probe.References {
		referenceID := wireGuardServiceReferenceID(reference.SiteID, reference.Role)
		if _, exists := expected[referenceID]; exists {
			references = append(references, referenceID)
		}
	}
	return references
}

func wireGuardServiceHostsEqual(left, right string) bool {
	leftIP, rightIP := net.ParseIP(left), net.ParseIP(right)
	if leftIP != nil && rightIP != nil {
		return leftIP.Equal(rightIP)
	}
	return strings.EqualFold(left, right)
}

func wireGuardServiceReferenceID(siteID, role string) string {
	return siteID + "\x00" + role
}

func wireGuardServiceStatus(service wireGuardOriginService) wireGuardOriginServiceStatus {
	if service.TotalNodes == 0 || service.ObservedNodes == 0 {
		return wireGuardOriginServiceUnknown
	}
	if service.ObservedNodes < service.TotalNodes {
		return wireGuardOriginServicePartial
	}
	if service.ReachableNodes == service.TotalNodes {
		return wireGuardOriginServiceHealthy
	}
	if service.ReachableNodes == 0 {
		return wireGuardOriginServiceUnreachable
	}
	return wireGuardOriginServiceDegraded
}
