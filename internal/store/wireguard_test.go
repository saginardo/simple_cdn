package store

import (
	"encoding/base64"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestWireGuardPeerTransferRatesResetAndResample(t *testing.T) {
	database, err := Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("rate-edge", "203.0.113.39")
	if err != nil {
		t.Fatal(err)
	}
	tunnel, err := database.CreateWireGuardTunnel(domain.WireGuardTunnel{
		Name: "rate-origin", EndpointHost: "198.51.100.39", AddressCIDR: "10.253.39.0/24",
	}, []string{node.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := wireGuardTestKey(7)
	report := domain.WireGuardPeerReport{
		TunnelID: tunnel.ID, Revision: tunnel.Revision, InterfaceName: domain.WireGuardInterfaceName(tunnel.ID),
		PublicKey: key, RXBytes: 1_000, TXBytes: 2_000,
	}
	if err := database.UpdateWireGuardPeerReports(node.ID, []domain.WireGuardPeerReport{report}); err != nil {
		t.Fatal(err)
	}
	current, err := database.GetWireGuardTunnel(tunnel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if peer := current.Peers[0]; peer.RXBytesPerSecond != nil || peer.TXBytesPerSecond != nil || peer.TransferSampleSecs != nil {
		t.Fatalf("first transfer sample = %#v", peer)
	}
	report.Revision = current.Revision

	setWireGuardPeerReportedAt(t, database, tunnel.ID, node.ID, time.Now().UTC().Add(-10*time.Second))
	report.RXBytes = 21_000
	report.TXBytes = 42_000
	if err := database.UpdateWireGuardPeerReports(node.ID, []domain.WireGuardPeerReport{report}); err != nil {
		t.Fatal(err)
	}
	current, err = database.GetWireGuardTunnel(tunnel.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertWireGuardTransferSample(t, current.Peers[0], 20_000, 40_000)

	setWireGuardPeerReportedAt(t, database, tunnel.ID, node.ID, time.Now().UTC().Add(-10*time.Second))
	report.RXBytes = 500
	report.TXBytes = 1_000
	if err := database.UpdateWireGuardPeerReports(node.ID, []domain.WireGuardPeerReport{report}); err != nil {
		t.Fatal(err)
	}
	current, err = database.GetWireGuardTunnel(tunnel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if peer := current.Peers[0]; peer.RXBytes != 500 || peer.TXBytes != 1_000 || peer.RXBytesPerSecond != nil ||
		peer.TXBytesPerSecond != nil || peer.TransferSampleSecs != nil {
		t.Fatalf("reset transfer sample = %#v", peer)
	}

	setWireGuardPeerReportedAt(t, database, tunnel.ID, node.ID, time.Now().UTC().Add(-10*time.Second))
	report.RXBytes = 5_500
	report.TXBytes = 11_000
	if err := database.UpdateWireGuardPeerReports(node.ID, []domain.WireGuardPeerReport{report}); err != nil {
		t.Fatal(err)
	}
	current, err = database.GetWireGuardTunnel(tunnel.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertWireGuardTransferSample(t, current.Peers[0], 5_000, 10_000)
}

func setWireGuardPeerReportedAt(t *testing.T, database *Store, tunnelID, nodeID string, value time.Time) {
	t.Helper()
	if _, err := database.db.Exec(`UPDATE wireguard_tunnel_nodes SET last_reported_at=? WHERE tunnel_id=? AND node_id=?`,
		stamp(value), tunnelID, nodeID); err != nil {
		t.Fatal(err)
	}
}

func assertWireGuardTransferSample(t *testing.T, peer domain.WireGuardPeer, rxDelta, txDelta float64) {
	t.Helper()
	if peer.RXBytesPerSecond == nil || peer.TXBytesPerSecond == nil || peer.TransferSampleSecs == nil {
		t.Fatalf("missing transfer sample = %#v", peer)
	}
	if *peer.TransferSampleSecs < 9 || *peer.TransferSampleSecs > 11 {
		t.Fatalf("sample seconds = %f", *peer.TransferSampleSecs)
	}
	if delta := math.Abs((*peer.RXBytesPerSecond)*(*peer.TransferSampleSecs) - rxDelta); delta > 0.01 {
		t.Fatalf("RX sampled bytes differ by %f", delta)
	}
	if delta := math.Abs((*peer.TXBytesPerSecond)*(*peer.TransferSampleSecs) - txDelta); delta > 0.01 {
		t.Fatalf("TX sampled bytes differ by %f", delta)
	}
}

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
		DirectTCP:        &domain.WireGuardTCPMeasurement{Mbps: 90, Retransmits: 1},
		DirectTCPReverse: &domain.WireGuardTCPMeasurement{Mbps: 80, Retransmits: 2},
	}
	if err := database.FinishWireGuardPerformanceTest(node.ID, test.ID, partial, "WireGuard UDP: connection refused"); err != nil {
		t.Fatal(err)
	}
	finished, err := database.GetWireGuardPerformanceTest(test.ID)
	if err != nil || finished.Status != domain.WireGuardPerformanceFailed || finished.Result == nil || finished.Result.DirectTCP == nil ||
		finished.Result.DirectTCPReverse == nil || finished.Result.DirectTCPReverse.Mbps != 80 || finished.Error == "" {
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

func TestWireGuardListTunnelsIncludesEveryPeer(t *testing.T) {
	database, err := Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	first, err := database.CreateNode("list-edge-a", "203.0.113.70")
	if err != nil {
		t.Fatal(err)
	}
	second, err := database.CreateNode("list-edge-b", "203.0.113.71")
	if err != nil {
		t.Fatal(err)
	}
	firstTunnel, err := database.CreateWireGuardTunnel(domain.WireGuardTunnel{
		Name: "a-origin", EndpointHost: "198.51.100.70", AddressCIDR: "10.253.70.0/24",
	}, []string{first.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	secondTunnel, err := database.CreateWireGuardTunnel(domain.WireGuardTunnel{
		Name: "b-origin", EndpointHost: "198.51.100.71", AddressCIDR: "10.253.71.0/24",
	}, []string{first.ID, second.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tunnels, err := database.ListWireGuardTunnels()
	if err != nil {
		t.Fatal(err)
	}
	if len(tunnels) != 2 || len(tunnels[0].Peers) != 1 || len(tunnels[1].Peers) != 2 {
		t.Fatalf("listed tunnels = %#v", tunnels)
	}
	byID := make(map[string]domain.WireGuardTunnel, len(tunnels))
	for _, tunnel := range tunnels {
		byID[tunnel.ID] = tunnel
	}
	if byID[firstTunnel.ID].Peers[0].NodeID != first.ID ||
		len(byID[secondTunnel.ID].Peers) != 2 {
		t.Fatalf("listed peer assignment = %#v", byID)
	}
}

func TestWireGuardTunnelReferencesQueriesSitesDirectly(t *testing.T) {
	database, err := Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("reference-edge", "203.0.113.72")
	if err != nil {
		t.Fatal(err)
	}
	tunnel, err := database.CreateWireGuardTunnel(domain.WireGuardTunnel{
		Name: "reference-origin", EndpointHost: "198.51.100.72", AddressCIDR: "10.253.72.0/24",
	}, []string{node.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	primarySite, err := database.CreateSite(domain.Site{
		Name: "Primary", Domains: []string{"primary.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "http://primary.example.test:8080", WireGuardTunnelID: tunnel.ID, Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	backupOrigin := &domain.Origin{URL: "http://backup.example.test:8080", WireGuardTunnelID: tunnel.ID, Enabled: true}
	backupSite, err := database.CreateSite(domain.Site{
		Name: "Backup", Domains: []string{"backup.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "http://direct.example.test:8080", Enabled: true},
		BackupOrigin:  backupOrigin, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateSite(domain.Site{
		Name: "Unrelated", Domains: []string{"unrelated.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "http://unrelated.example.test:8080", Enabled: true}, Enabled: true,
	}, "zone"); err != nil {
		t.Fatal(err)
	}
	references, err := database.WireGuardTunnelReferences(tunnel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 2 || references[0].ID != backupSite.ID || references[1].ID != primarySite.ID {
		t.Fatalf("direct tunnel references = %#v", references)
	}
}

func TestWireGuardEdgeConfigRevisionTracksPublishedChanges(t *testing.T) {
	database, err := Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("revision-edge", "203.0.113.73")
	if err != nil {
		t.Fatal(err)
	}
	tunnel, err := database.CreateWireGuardTunnel(domain.WireGuardTunnel{
		Name: "revision-origin", EndpointHost: "198.51.100.73", AddressCIDR: "10.253.73.0/24",
	}, []string{node.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := database.WireGuardEdgeConfigRevision(node.ID)
	if err != nil || revision == "" {
		t.Fatalf("initial edge config revision = %q, %v", revision, err)
	}
	tunnel.MTU = 1380
	if _, err := database.UpdateWireGuardTunnel(tunnel, []string{node.ID}, nil); err != nil {
		t.Fatal(err)
	}
	updated, err := database.WireGuardEdgeConfigRevision(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated == revision {
		t.Fatalf("edge config revision did not change after tunnel update: %q", revision)
	}
	otherNode, err := database.CreateNode("revision-other-edge", "203.0.113.74")
	if err != nil {
		t.Fatal(err)
	}
	if otherRevision, err := database.WireGuardEdgeConfigRevision(otherNode.ID); err != nil || otherRevision != "" {
		t.Fatalf("unassigned edge revision = %q, %v", otherRevision, err)
	}
	if _, err := database.CreateWireGuardTunnel(domain.WireGuardTunnel{
		Name: "revision-newest", EndpointHost: "198.51.100.74", AddressCIDR: "10.253.74.0/24",
	}, []string{node.ID}, nil); err != nil {
		t.Fatal(err)
	}
	withSecondTunnel, err := database.WireGuardEdgeConfigRevision(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpdateWireGuardTunnel(tunnel, []string{otherNode.ID}, nil); err != nil {
		t.Fatal(err)
	}
	afterRemoval, err := database.WireGuardEdgeConfigRevision(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterRemoval == withSecondTunnel {
		t.Fatalf("edge config revision did not change after removing a non-latest tunnel: %q", afterRemoval)
	}
}

func TestWireGuardClaimSweepsOnlyStaleRunningTests(t *testing.T) {
	database, err := Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("claim-edge", "203.0.113.75")
	if err != nil {
		t.Fatal(err)
	}
	tunnel, err := database.CreateWireGuardTunnel(domain.WireGuardTunnel{
		Name: "claim-origin", EndpointHost: "198.51.100.75", AddressCIDR: "10.253.75.0/24",
	}, []string{node.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	test, err := database.CreateWireGuardPerformanceTest(tunnel.ID, node.ID, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`UPDATE wireguard_performance_tests SET status=?, started_at=? WHERE id=?`,
		domain.WireGuardPerformanceRunning, stamp(time.Now().UTC().Add(-11*time.Minute)), test.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := database.ClaimWireGuardPerformanceTest(node.ID)
	if err != nil || claimed != nil {
		t.Fatalf("claim after stale sweep = %#v, %v", claimed, err)
	}
	finished, err := database.GetWireGuardPerformanceTest(test.ID)
	if err != nil || finished.Status != domain.WireGuardPerformanceFailed || !strings.Contains(finished.Error, "deadline") {
		t.Fatalf("stale test after sweep = %#v, %v", finished, err)
	}
}

func TestWireGuardClaimWaitsForCurrentAppliedRevision(t *testing.T) {
	database, err := Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("claim-revision-edge", "203.0.113.76")
	if err != nil {
		t.Fatal(err)
	}
	tunnel, err := database.CreateWireGuardTunnel(domain.WireGuardTunnel{
		Name: "claim-revision-origin", EndpointHost: "198.51.100.76", AddressCIDR: "10.253.76.0/24",
	}, []string{node.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := wireGuardTestKey(7)
	if _, err := database.db.Exec(`UPDATE wireguard_tunnels
		SET origin_public_key=?, origin_configured_revision=revision WHERE id=?`, publicKey, tunnel.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`UPDATE wireguard_tunnel_nodes
		SET public_key=?, applied_revision=? WHERE tunnel_id=? AND node_id=?`, publicKey, tunnel.Revision, tunnel.ID, node.ID); err != nil {
		t.Fatal(err)
	}
	test, err := database.CreateWireGuardPerformanceTest(tunnel.ID, node.ID, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	tunnel.MTU = 1380
	updated, err := database.UpdateWireGuardTunnel(tunnel, []string{node.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := database.ClaimWireGuardPerformanceTest(node.ID)
	if err != nil || claimed != nil {
		t.Fatalf("claim with stale applied revision = %#v, %v", claimed, err)
	}
	queued, err := database.GetWireGuardPerformanceTest(test.ID)
	if err != nil || queued.Status != domain.WireGuardPerformanceQueued {
		t.Fatalf("test after deferred claim = %#v, %v", queued, err)
	}
	if _, err := database.db.Exec(`UPDATE wireguard_tunnels SET origin_configured_revision=revision WHERE id=?`, tunnel.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`UPDATE wireguard_tunnel_nodes SET applied_revision=?
		WHERE tunnel_id=? AND node_id=?`, updated.Revision, tunnel.ID, node.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err = database.ClaimWireGuardPerformanceTest(node.ID)
	if err != nil || claimed == nil || claimed.ID != test.ID {
		t.Fatalf("claim after revision convergence = %#v, %v", claimed, err)
	}
}

func TestWireGuardClaimFailsQueuedTestAfterPeerRemoval(t *testing.T) {
	database, err := Open(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	first, err := database.CreateNode("claim-removed-edge", "203.0.113.77")
	if err != nil {
		t.Fatal(err)
	}
	second, err := database.CreateNode("claim-remaining-edge", "203.0.113.78")
	if err != nil {
		t.Fatal(err)
	}
	tunnel, err := database.CreateWireGuardTunnel(domain.WireGuardTunnel{
		Name: "claim-removed-origin", EndpointHost: "198.51.100.77", AddressCIDR: "10.253.77.0/24",
	}, []string{first.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	test, err := database.CreateWireGuardPerformanceTest(tunnel.ID, first.ID, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpdateWireGuardTunnel(tunnel, []string{second.ID}, nil); err != nil {
		t.Fatal(err)
	}
	claimed, err := database.ClaimWireGuardPerformanceTest(first.ID)
	if err != nil || claimed != nil {
		t.Fatalf("claim after peer removal = %#v, %v", claimed, err)
	}
	finished, err := database.GetWireGuardPerformanceTest(test.ID)
	if err != nil || finished.Status != domain.WireGuardPerformanceFailed ||
		!strings.Contains(finished.Error, "configuration disappeared") {
		t.Fatalf("test after peer removal = %#v, %v", finished, err)
	}
}

func wireGuardTestKey(fill byte) string {
	raw := make([]byte, 32)
	for index := range raw {
		raw[index] = fill
	}
	return base64.StdEncoding.EncodeToString(raw)
}
