package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

func TestWireGuardTunnelDetailAggregatesOriginServices(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	nodes := make([]domain.Node, 3)
	for index, name := range []string{"edge-a", "edge-b", "edge-stale"} {
		nodes[index], err = database.CreateNode(name, "203.0.113."+string(rune('1'+index)))
		if err != nil {
			t.Fatal(err)
		}
		if err := database.SetNodeCapabilities(nodes[index].ID, []string{
			domain.EdgeCapabilityWireGuard,
			domain.EdgeCapabilityMachineStatus,
			domain.EdgeCapabilityMachineStatusStream,
			domain.EdgeCapabilityOriginConnection,
		}); err != nil {
			t.Fatal(err)
		}
	}
	tunnel, err := database.CreateWireGuardTunnel(domain.WireGuardTunnel{
		Name: "origin", EndpointHost: "198.51.100.44", AddressCIDR: "10.253.44.0/24",
	}, []string{nodes[0].ID, nodes[1].ID, nodes[2].ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h2cSite, err := database.CreateSite(domain.Site{
		Name: "API", Domains: []string{"api.example.test"}, Nodes: []string{nodes[0].ID, nodes[1].ID},
		PrimaryOrigin: domain.Origin{
			URL: "http://api-origin.example.test:8443", HTTPVersion: domain.OriginHTTPVersionH2C,
			WireGuardTunnelID: tunnel.ID, Enabled: true,
		}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	backup := &domain.Origin{
		URL: "http://fallback-origin.example.test:8443", HTTPVersion: domain.OriginHTTPVersionH2C,
		WireGuardTunnelID: tunnel.ID, Enabled: true,
	}
	backupSite, err := database.CreateSite(domain.Site{
		Name: "Fallback", Domains: []string{"fallback.example.test"}, Nodes: []string{nodes[0].ID, nodes[1].ID},
		PrimaryOrigin: domain.Origin{URL: "http://direct.example.test:8080", HTTPVersion: domain.OriginHTTPVersionH2C, Enabled: true},
		BackupOrigin:  backup, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	tlsSite, err := database.CreateSite(domain.Site{
		Name: "Secure", Domains: []string{"secure.example.test"}, Nodes: []string{nodes[0].ID},
		PrimaryOrigin: domain.Origin{
			URL: "https://secure-origin.example.test:8443", HTTPVersion: domain.OriginHTTPVersionHTTP2,
			WireGuardTunnelID: tunnel.ID, Enabled: true,
		}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	grpcSite, err := database.CreateSite(domain.Site{
		Name: "RPC", Domains: []string{"rpc.example.test"}, Nodes: []string{nodes[2].ID},
		PrimaryOrigin: domain.Origin{
			URL: "grpc://rpc-origin.example.test:50051", WireGuardTunnelID: tunnel.ID, Enabled: true,
		}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{Store: database}
	now := time.Now().UTC().Truncate(time.Millisecond)
	address8443 := tunnel.OriginAddress + ":8443"
	freshA := controlTestMachineStatus(now.Add(-time.Second))
	freshA.OriginProbes = []domain.OriginProbeStatus{
		{
			PoolID: "h2c-a", Address: address8443, Scheme: "http", HTTPVersion: domain.OriginHTTPVersionH2C,
			References: []domain.OriginPoolReference{{SiteID: h2cSite.ID, Role: "primary"}, {SiteID: backupSite.ID, Role: "backup"}},
			Healthy:    true, CircuitState: domain.OriginCircuitClosed, CheckedAt: freshA.CollectedAt,
		},
		{
			PoolID: "https-a", Address: address8443, Scheme: "https", HTTPVersion: domain.OriginHTTPVersionHTTP2,
			References: []domain.OriginPoolReference{{SiteID: tlsSite.ID, Role: "primary"}},
			Healthy:    true, CircuitState: domain.OriginCircuitClosed, CheckedAt: freshA.CollectedAt,
		},
	}
	server.recordNodeMachineStatus(nodes[0].ID, freshA)

	freshB := controlTestMachineStatus(now.Add(-2 * time.Second))
	freshB.OriginProbes = []domain.OriginProbeStatus{{
		PoolID: "h2c-b", Address: address8443, Scheme: "http", HTTPVersion: domain.OriginHTTPVersionH2C,
		References: []domain.OriginPoolReference{{SiteID: h2cSite.ID, Role: "primary"}, {SiteID: backupSite.ID, Role: "backup"}},
		Healthy:    false, CircuitState: domain.OriginCircuitOpen, CheckedAt: freshB.CollectedAt,
	}}
	server.recordNodeMachineStatus(nodes[1].ID, freshB)

	stale := controlTestMachineStatus(now.Add(-time.Minute))
	stale.OriginProbes = []domain.OriginProbeStatus{{
		PoolID: "grpc-stale", Address: tunnel.OriginAddress + ":50051", Scheme: "grpc",
		References: []domain.OriginPoolReference{{SiteID: grpcSite.ID, Role: "primary"}},
		Healthy:    true, CircuitState: domain.OriginCircuitClosed, CheckedAt: stale.CollectedAt,
	}}
	server.recordNodeMachineStatus(nodes[2].ID, stale)

	request := httptest.NewRequest(http.MethodGet, "/api/wireguard/tunnels/"+tunnel.ID, nil)
	request.SetPathValue("id", tunnel.ID)
	response := httptest.NewRecorder()
	server.getWireGuardTunnel(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var result wireGuardTunnelDetailResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Tunnel.ID != tunnel.ID || len(result.OriginServices) != 3 {
		t.Fatalf("detail = %#v", result)
	}

	h2c := findWireGuardOriginService(t, result.OriginServices, 8443, "http", domain.OriginHTTPVersionH2C)
	if h2c.Status != wireGuardOriginServiceDegraded || h2c.ReachableNodes != 1 || h2c.ObservedNodes != 2 || h2c.TotalNodes != 2 {
		t.Fatalf("H2C service = %#v", h2c)
	}
	if len(h2c.Sites) != 2 || h2c.Sites[0].SiteName != "API" || h2c.Sites[0].Role != "primary" ||
		h2c.Sites[1].SiteName != "Fallback" || h2c.Sites[1].Role != "backup" {
		t.Fatalf("H2C references = %#v", h2c.Sites)
	}
	if h2c.LastReportedAt == nil || !h2c.LastReportedAt.Equal(freshA.CollectedAt) {
		t.Fatalf("H2C last report = %v", h2c.LastReportedAt)
	}

	https := findWireGuardOriginService(t, result.OriginServices, 8443, "https", domain.OriginHTTPVersionHTTP2)
	if https.Status != wireGuardOriginServiceHealthy || https.ReachableNodes != 1 || https.ObservedNodes != 1 || https.TotalNodes != 1 {
		t.Fatalf("HTTPS service = %#v", https)
	}

	grpc := findWireGuardOriginService(t, result.OriginServices, 50051, "grpc", "")
	if grpc.Status != wireGuardOriginServiceUnknown || grpc.ReachableNodes != 0 || grpc.ObservedNodes != 0 || grpc.TotalNodes != 1 {
		t.Fatalf("gRPC service = %#v", grpc)
	}
	if grpc.LastReportedAt == nil || !grpc.LastReportedAt.Equal(stale.CollectedAt) {
		t.Fatalf("gRPC last report = %v", grpc.LastReportedAt)
	}
}

func TestWireGuardTunnelDetailReturnsNotFound(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	request := httptest.NewRequest(http.MethodGet, "/api/wireguard/tunnels/missing", nil)
	request.SetPathValue("id", "missing")
	response := httptest.NewRecorder()
	(&Server{Store: database}).getWireGuardTunnel(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func findWireGuardOriginService(
	t *testing.T,
	services []wireGuardOriginService,
	port int,
	scheme string,
	httpVersion domain.OriginHTTPVersion,
) wireGuardOriginService {
	t.Helper()
	for _, service := range services {
		if service.Port == port && service.Scheme == scheme && service.HTTPVersion == httpVersion {
			return service
		}
	}
	t.Fatalf("service %d/%s/%s not found in %#v", port, scheme, httpVersion, services)
	return wireGuardOriginService{}
}
