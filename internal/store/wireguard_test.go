package store

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestWireGuardTunnelEnrollmentSiteReferencesAndPerformance(t *testing.T) {
	database, err := Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("wireguard-edge", "203.0.113.40")
	if err != nil {
		t.Fatal(err)
	}
	otherNode, err := database.CreateNode("other-edge", "203.0.113.41")
	if err != nil {
		t.Fatal(err)
	}
	tunnel, err := database.CreateWireGuardTunnel(domain.WireGuardTunnel{
		Name: "origin-a", EndpointHost: "origin.example.test", AddressCIDR: "10.253.10.0/24",
	}, []string{node.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tunnel.Revision != 1 || tunnel.OriginAddress != "10.253.10.1" || len(tunnel.Peers) != 1 || tunnel.Peers[0].Address != "10.253.10.2" {
		t.Fatalf("created tunnel = %#v", tunnel)
	}
	if _, err := database.CreateWireGuardTunnel(domain.WireGuardTunnel{
		Name: "overlap", EndpointHost: "other.example.test", AddressCIDR: "10.253.10.128/25",
	}, []string{otherNode.ID}, nil); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("overlapping tunnel error = %v", err)
	}

	originKey := wireGuardTestKey(1)
	edgeKey := wireGuardTestKey(2)
	const tokenHash = "one-time-token-hash"
	if err := database.CreateWireGuardInstallToken(tunnel.ID, tokenHash, time.Now().UTC().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	pending, ready, err := database.ConfigureWireGuardOrigin(tokenHash, originKey)
	if err != nil {
		t.Fatal(err)
	}
	if ready || pending.Revision != 2 || pending.OriginConfiguredRevision != 0 {
		t.Fatalf("pending origin enrollment = ready:%t %#v", ready, pending)
	}
	configs, err := database.WireGuardEdgeConfigs(node.ID)
	if err != nil || len(configs) != 1 || configs[0].Revision != 2 || configs[0].OriginPublicKey != originKey {
		t.Fatalf("edge configs = %#v, %v", configs, err)
	}
	if err := database.UpdateWireGuardPeerReports(node.ID, []domain.WireGuardPeerReport{{
		TunnelID: tunnel.ID, Revision: 2, InterfaceName: domain.WireGuardInterfaceName(tunnel.ID), PublicKey: edgeKey,
	}}); err != nil {
		t.Fatal(err)
	}
	configured, ready, err := database.ConfigureWireGuardOrigin(tokenHash, originKey)
	if err != nil {
		t.Fatal(err)
	}
	if !ready || configured.Revision != 3 || configured.OriginConfiguredRevision != 3 || configured.Peers[0].AppliedRevision != 2 {
		t.Fatalf("configured origin enrollment = ready:%t %#v", ready, configured)
	}
	if err := database.UpdateWireGuardPeerReports(node.ID, []domain.WireGuardPeerReport{{
		TunnelID: tunnel.ID, Revision: configured.Revision, InterfaceName: domain.WireGuardInterfaceName(tunnel.ID), PublicKey: edgeKey,
	}}); err != nil {
		t.Fatal(err)
	}
	configured, err = database.GetWireGuardTunnel(tunnel.ID)
	if err != nil || configured.Peers[0].AppliedRevision != configured.Revision || configured.Peers[0].LastError != "" {
		t.Fatalf("converged tunnel = %#v, %v", configured, err)
	}
	if _, _, err := database.ConfigureWireGuardOrigin(tokenHash, originKey); !errors.Is(err, ErrWireGuardInstallToken) {
		t.Fatalf("used install token error = %v", err)
	}

	if _, err := database.CreateSite(domain.Site{
		Name: "invalid-wireguard-site", Domains: []string{"invalid.example.test"}, Nodes: []string{otherNode.ID},
		PrimaryOrigin: domain.Origin{URL: "http://app.example.test", WireGuardTunnelID: tunnel.ID, Enabled: true}, Enabled: true,
	}, "zone-invalid"); err == nil || !strings.Contains(err.Error(), "not assigned to every edge node") {
		t.Fatalf("site with unassigned tunnel error = %v", err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "wireguard-site", Domains: []string{"cdn.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://app.example.test:8443", WireGuardTunnelID: tunnel.ID, Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	if site.PrimaryOrigin.WireGuardTunnelID != tunnel.ID {
		t.Fatalf("site tunnel selection = %q", site.PrimaryOrigin.WireGuardTunnelID)
	}
	configured.Name = "origin-a-updated"
	if _, err := database.UpdateWireGuardTunnel(configured, []string{otherNode.ID}, nil); !errors.Is(err, ErrWireGuardTunnelInUse) {
		t.Fatalf("remove referenced peer error = %v", err)
	}
	if err := database.DeleteWireGuardTunnel(tunnel.ID); !errors.Is(err, ErrWireGuardTunnelInUse) {
		t.Fatalf("delete referenced tunnel error = %v", err)
	}

	test, err := database.CreateWireGuardPerformanceTest(tunnel.ID, node.ID, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := database.ClaimWireGuardPerformanceTest(node.ID)
	if err != nil || claimed == nil || claimed.ID != test.ID || claimed.Status != domain.WireGuardPerformanceRunning {
		t.Fatalf("claimed test = %#v, %v", claimed, err)
	}
	partial := &domain.WireGuardPerformanceResult{
		DirectTCP: &domain.WireGuardTCPMeasurement{Mbps: 90, Retransmits: 1},
	}
	if err := database.FinishWireGuardPerformanceTest(node.ID, test.ID, partial, "WireGuard UDP: connection refused"); err != nil {
		t.Fatal(err)
	}
	finished, err := database.GetWireGuardPerformanceTest(test.ID)
	if err != nil || finished.Status != domain.WireGuardPerformanceFailed || finished.Result == nil || finished.Result.DirectTCP == nil || finished.Error == "" {
		t.Fatalf("partial performance result = %#v, %v", finished, err)
	}
}

func TestWireGuardTunnelEgressLimitsAndSameHostProtection(t *testing.T) {
	database, err := Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-a", "203.0.113.60")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateWireGuardTunnel(domain.WireGuardTunnel{
		Name: "same-host", EndpointHost: node.PublicIPv4, AddressCIDR: "10.253.60.0/24",
	}, []string{node.ID}, nil); !errors.Is(err, ErrWireGuardSameHost) {
		t.Fatalf("same-host tunnel error = %v", err)
	}
	tunnel, err := database.CreateWireGuardTunnel(domain.WireGuardTunnel{
		Name: "limited", EndpointHost: "198.51.100.60", AddressCIDR: "10.253.61.0/24",
		OriginEgressLimitMbps: 80,
	}, []string{node.ID}, map[string]int{node.ID: 40})
	if err != nil {
		t.Fatal(err)
	}
	if tunnel.OriginEgressLimitMbps != 80 || tunnel.Peers[0].EdgeEgressLimitMbps != 40 {
		t.Fatalf("created limits = %#v", tunnel)
	}
	configs, err := database.WireGuardEdgeConfigs(node.ID)
	if err != nil || len(configs) != 1 || configs[0].EdgeEgressLimitMbps != 40 {
		t.Fatalf("edge egress config = %#v, %v", configs, err)
	}
	tunnel.OriginEgressLimitMbps = 70
	updated, err := database.UpdateWireGuardTunnel(tunnel, []string{node.ID}, map[string]int{node.ID: 30})
	if err != nil {
		t.Fatal(err)
	}
	if updated.OriginEgressLimitMbps != 70 || updated.Peers[0].EdgeEgressLimitMbps != 30 {
		t.Fatalf("updated limits = %#v", updated)
	}
	if _, err := database.UpdateWireGuardTunnel(updated, []string{node.ID}, map[string]int{"missing": 10}); err == nil || !strings.Contains(err.Error(), "unselected") {
		t.Fatalf("unselected edge limit error = %v", err)
	}
}

func wireGuardTestKey(fill byte) string {
	raw := make([]byte, 32)
	for index := range raw {
		raw[index] = fill
	}
	return base64.StdEncoding.EncodeToString(raw)
}
