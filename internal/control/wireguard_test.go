package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

func TestCreateWireGuardTunnelRejectsEndpointResolvingToSelectedEdge(t *testing.T) {
	database, err := store.Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("same-host-edge", "203.0.113.90")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCapabilities(node.ID, []string{domain.EdgeCapabilityWireGuard}); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		Store: database,
		WireGuardEndpointResolver: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP(node.PublicIPv4)}, nil
		},
	}
	body := `{"name":"same-host","endpoint_host":"origin.example.test","address_cidr":"10.253.90.0/24","node_ids":["` + node.ID + `"]}`
	response := httptest.NewRecorder()
	server.createWireGuardTunnel(response, httptest.NewRequest(http.MethodPost, "/api/wireguard/tunnels", strings.NewReader(body)))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "same public IPv4") {
		t.Fatalf("same-host response = %d %s", response.Code, response.Body.String())
	}
}

func TestCreateWireGuardPerformanceTestRequiresRecentHandshake(t *testing.T) {
	database, err := store.Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("performance-edge", "203.0.113.91")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCapabilities(node.ID, []string{
		domain.EdgeCapabilityWireGuard, domain.EdgeCapabilityWireGuardPerformance,
		domain.EdgeCapabilityWireGuardPerformanceV2,
	}); err != nil {
		t.Fatal(err)
	}
	tunnel, err := database.CreateWireGuardTunnel(domain.WireGuardTunnel{
		Name: "performance", EndpointHost: "198.51.100.91", AddressCIDR: "10.253.91.0/24",
	}, []string{node.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const tokenHash = "performance-token"
	if err := database.CreateWireGuardInstallToken(tunnel.ID, tokenHash, time.Now().UTC().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	originKey := controlWireGuardTestKey(1)
	edgeKey := controlWireGuardTestKey(2)
	tunnel, ready, err := database.ConfigureWireGuardOrigin(tokenHash, originKey)
	if err != nil || ready {
		t.Fatalf("initial origin configure = %#v, %t, %v", tunnel, ready, err)
	}
	stale := time.Now().UTC().Add(-4 * time.Minute)
	if err := database.UpdateWireGuardPeerReports(node.ID, []domain.WireGuardPeerReport{{
		TunnelID: tunnel.ID, Revision: tunnel.Revision, InterfaceName: domain.WireGuardInterfaceName(tunnel.ID),
		PublicKey: edgeKey, LatestHandshake: &stale,
	}}); err != nil {
		t.Fatal(err)
	}
	tunnel, ready, err = database.ConfigureWireGuardOrigin(tokenHash, originKey)
	if err != nil || !ready {
		t.Fatalf("final origin configure = %#v, %t, %v", tunnel, ready, err)
	}
	if err := database.UpdateWireGuardPeerReports(node.ID, []domain.WireGuardPeerReport{{
		TunnelID: tunnel.ID, Revision: tunnel.Revision, InterfaceName: domain.WireGuardInterfaceName(tunnel.ID),
		PublicKey: edgeKey, LatestHandshake: &stale,
	}}); err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}
	body := []byte(`{"tunnel_id":"` + tunnel.ID + `","node_id":"` + node.ID + `","target_mbps":10,"duration_seconds":3}`)
	response := httptest.NewRecorder()
	server.createWireGuardPerformanceTest(response, httptest.NewRequest(http.MethodPost, "/api/wireguard/performance-tests", bytes.NewReader(body)))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "last 3 minutes") {
		t.Fatalf("stale handshake response = %d %s", response.Code, response.Body.String())
	}
	fresh := time.Now().UTC()
	if err := database.UpdateWireGuardPeerReports(node.ID, []domain.WireGuardPeerReport{{
		TunnelID: tunnel.ID, Revision: tunnel.Revision, InterfaceName: domain.WireGuardInterfaceName(tunnel.ID),
		PublicKey: edgeKey, LatestHandshake: &fresh,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCapabilities(node.ID, []string{
		domain.EdgeCapabilityWireGuard, domain.EdgeCapabilityWireGuardPerformance,
	}); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	server.createWireGuardPerformanceTest(response, httptest.NewRequest(http.MethodPost, "/api/wireguard/performance-tests", bytes.NewReader(body)))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "bidirectional") {
		t.Fatalf("legacy capability response = %d %s", response.Code, response.Body.String())
	}
	if err := database.SetNodeCapabilities(node.ID, []string{
		domain.EdgeCapabilityWireGuard, domain.EdgeCapabilityWireGuardPerformance,
		domain.EdgeCapabilityWireGuardPerformanceV2,
	}); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	server.createWireGuardPerformanceTest(response, httptest.NewRequest(http.MethodPost, "/api/wireguard/performance-tests", bytes.NewReader(body)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("fresh handshake response = %d %s", response.Code, response.Body.String())
	}
}

func controlWireGuardTestKey(fill byte) string {
	raw := make([]byte, 32)
	for index := range raw {
		raw[index] = fill
	}
	return base64.StdEncoding.EncodeToString(raw)
}
