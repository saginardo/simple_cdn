package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/integrations"
	"simple_cdn/internal/nginx"
	"simple_cdn/internal/store"
)

type healthRoundTripFunc func(*http.Request) (*http.Response, error)

func (f healthRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func healthyNodeClient() *http.Client {
	return nodeHealthClient(http.StatusOK)
}

func nodeHealthClient(statusCode int) *http.Client {
	return &http.Client{Transport: healthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: statusCode,
			Status:     fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
			Body:       io.NopCloser(strings.NewReader("ok\n")),
			Header:     make(http.Header),
		}, nil
	})}
}

func TestLightweightHealthProbeReusesConnection(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(response, "ok\n")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	manager := &HealthManager{}
	defer manager.resetProbeClient()
	address := strings.TrimPrefix(server.URL, "http://")
	for range 2 {
		if healthy, detail := manager.check(context.Background(), address); !healthy {
			t.Fatalf("health check failed: %s", detail)
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("two lightweight probes opened %d TCP connections", connections.Load())
	}
	for range 2 {
		if healthy, detail := manager.checkFresh(context.Background(), address); !healthy {
			t.Fatalf("fresh health check failed: %s", detail)
		}
	}
	if connections.Load() != 3 {
		t.Fatalf("fresh probes did not open independent connections: total=%d", connections.Load())
	}
	if manager.deepInterval() != defaultHealthDeepInterval {
		t.Fatalf("deep probe interval = %s", manager.deepInterval())
	}
}

func TestDisabledSiteWithdrawsManagedDNSBeforeRepublish(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.CreateSite(domain.Site{
		Name:          "disabled-site",
		Domains:       []string{"cdn.example.test"},
		Nodes:         []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true},
		Enabled:       false,
	}, "zone-1")
	if err != nil {
		t.Fatal(err)
	}
	dns := &MemoryDNS{Zones: map[string][]integrations.DNSRecord{
		"zone-1": {{Name: "cdn.example.test", Content: node.PublicIPv4}},
	}}
	manager := HealthManager{Server: &Server{Store: database, DNS: dns}}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := dns.Zones["zone-1"]; len(got) != 0 {
		t.Fatalf("disabled site DNS was not withdrawn: %#v", got)
	}
}

func TestSiteIPv6DNSUsesIndependentHealthEligibility(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNodeWithAddresses("edge-ipv6", "203.0.113.74", "2001:db8::74")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "ipv6-site", Domains: []string{"ipv6.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, IPv6Enabled: true, Enabled: true,
	}, "zone-ipv6")
	if err != nil {
		t.Fatal(err)
	}
	site, err = database.MarkSitePublished(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := nginx.Render([]domain.Site{site})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SaveNodeState(node.ID, domain.DesiredState{Version: 1, NginxConfig: configuration}, nil); err != nil {
		t.Fatal(err)
	}
	if err := database.Heartbeat(node.ID, 1, "", nil); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := database.RecordNodeHealth(node.ID, true, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := database.RecordSiteNodeHealth(site.ID, node.ID, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	nodes, err := database.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	dns := &MemoryDNS{}
	manager := HealthManager{Server: &Server{Store: database, DNS: dns}}
	if err := manager.reconcileSiteDNS(context.Background(), site, nodes); err != nil {
		t.Fatal(err)
	}
	assertAddressRecordTypes(t, dns.Zones[site.ZoneID], map[string]string{"A": node.PublicIPv4})

	for range 5 {
		if _, err := database.RecordSiteNodeIPv6Health(site.ID, node.ID, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.reconcileSiteDNS(context.Background(), site, nodes); err != nil {
		t.Fatal(err)
	}
	assertAddressRecordTypes(t, dns.Zones[site.ZoneID], map[string]string{"A": node.PublicIPv4, "AAAA": node.PublicIPv6})

	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := database.RecordSiteNodeIPv6Health(site.ID, node.ID, false, "IPv6 timeout"); err != nil {
			t.Fatal(err)
		}
		if err := manager.reconcileSiteDNS(context.Background(), site, nodes); err != nil {
			t.Fatal(err)
		}
		expected := map[string]string{"A": node.PublicIPv4, "AAAA": node.PublicIPv6}
		if attempt == 3 {
			expected = map[string]string{"A": node.PublicIPv4}
		}
		assertAddressRecordTypes(t, dns.Zones[site.ZoneID], expected)
	}

	for range 5 {
		if _, err := database.RecordSiteNodeIPv6Health(site.ID, node.ID, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	site.IPv6Enabled = false
	if err := manager.reconcileSiteDNS(context.Background(), site, nodes); err != nil {
		t.Fatal(err)
	}
	assertAddressRecordTypes(t, dns.Zones[site.ZoneID], map[string]string{"A": node.PublicIPv4})
}

func TestTCPOnlySiteBuildsIPv6HealthWithoutHTTPProbe(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNodeWithAddresses("edge-tcp-ipv6", "203.0.113.75", "2001:db8::75")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "mail-ipv6", Domains: []string{"mail-ipv6.example.test"}, Nodes: []string{node.ID},
		TCPOnly: true, IPv6Enabled: true, TCPForwards: []domain.TCPForward{{Name: "smtps", ListenPort: 9465, UpstreamHost: "mail-origin.example.test", UpstreamPort: 465}}, Enabled: true,
	}, "zone-mail-ipv6")
	if err != nil {
		t.Fatal(err)
	}
	site, err = database.MarkSitePublished(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	streamConfiguration, err := nginx.RenderStream([]domain.Site{site})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SaveNodeState(node.ID, domain.DesiredState{Version: 1, NginxStreamConfig: streamConfiguration, PublicPorts: []int{9465}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := database.Heartbeat(node.ID, 1, "", nil); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := database.RecordNodeHealth(node.ID, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	nodes, err := database.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	var httpProbeCalls, ipv6ProbeCalls int
	dns := &MemoryDNS{}
	manager := HealthManager{
		Server: &Server{Store: database, DNS: dns},
		SiteProbe: func(context.Context, domain.Site, domain.Node) (bool, string) {
			httpProbeCalls++
			return false, "HTTP probe must not run for a TCP-only site"
		},
		SiteIPv6Probe: func(context.Context, domain.Site, domain.Node) (bool, string) {
			ipv6ProbeCalls++
			return true, ""
		},
	}
	for range 5 {
		if failures := manager.reconcileSiteHealth(context.Background(), []domain.Site{site}, nodes); len(failures) != 0 {
			t.Fatalf("IPv6 health reconciliation failed: %v", failures)
		}
	}
	if httpProbeCalls != 0 || ipv6ProbeCalls != 5 {
		t.Fatalf("probe calls: HTTP=%d IPv6=%d", httpProbeCalls, ipv6ProbeCalls)
	}
	if err := manager.reconcileSiteDNS(context.Background(), site, nodes); err != nil {
		t.Fatal(err)
	}
	assertAddressRecordTypes(t, dns.Zones[site.ZoneID], map[string]string{"A": node.PublicIPv4, "AAAA": node.PublicIPv6})
}

func assertAddressRecordTypes(t *testing.T, records []integrations.DNSRecord, expected map[string]string) {
	t.Helper()
	if len(records) != len(expected) {
		t.Fatalf("DNS records = %#v, want %#v", records, expected)
	}
	for _, record := range records {
		if expected[record.Type] != record.Content {
			t.Fatalf("DNS record = %#v, want %#v", record, expected)
		}
	}
}

func TestPendingSiteDraftDoesNotChangePublishedHealthOrDNS(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-1", "203.0.113.12")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "published-site", Domains: []string{"old.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone-1")
	if err != nil {
		t.Fatal(err)
	}
	site, err = database.MarkSitePublished(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := nginx.Render([]domain.Site{site})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SaveNodeState(node.ID, domain.DesiredState{Version: 1, NginxConfig: configuration}, nil); err != nil {
		t.Fatal(err)
	}
	if err := database.Heartbeat(node.ID, 1, "", nil); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := database.RecordNodeHealth(node.ID, true, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := database.RecordSiteNodeHealth(site.ID, node.ID, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	draft, zoneID, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	draft.Domains = []string{"draft.example.test"}
	draft.Enabled = false
	draftTTL := 300
	draft.DNSTTLSeconds = &draftTTL
	if _, err := database.UpdateSite(draft, zoneID); err != nil {
		t.Fatal(err)
	}
	key, err := NewEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := NewSettingsManager(database, cipher, EnvironmentSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.SaveDNSDefaultTTL(120); err != nil {
		t.Fatal(err)
	}
	probedDomain := ""
	dns := &MemoryDNS{}
	manager := HealthManager{
		Server: &Server{Store: database, DNS: dns, Settings: settings},
		Client: healthyNodeClient(),
		SiteProbe: func(_ context.Context, published domain.Site, _ domain.Node) (bool, string) {
			probedDomain = published.Domains[0]
			return true, ""
		},
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if probedDomain != "old.example.test" {
		t.Fatalf("health probe used draft domain %q", probedDomain)
	}
	records := dns.Zones["zone-1"]
	if len(records) != 1 || records[0].Name != "old.example.test" || records[0].Content != node.PublicIPv4 || records[0].TTL != 120 {
		t.Fatalf("DNS did not retain the published snapshot: %#v", records)
	}
}

func TestHealthPreservesDNSAndSuppressesAlertsDuringStateConvergence(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-converging", "203.0.113.121")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "converging-site", Domains: []string{"converging.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone-converging")
	if err != nil {
		t.Fatal(err)
	}
	site, err = database.MarkSitePublished(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := nginx.Render([]domain.Site{site})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SaveNodeState(node.ID, domain.DesiredState{Version: 2, NginxConfig: configuration}, nil); err != nil {
		t.Fatal(err)
	}
	if err := database.Heartbeat(node.ID, 1, "", nil); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := database.RecordNodeHealth(node.ID, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	dns := &MemoryDNS{Zones: map[string][]integrations.DNSRecord{
		"zone-converging": {{Name: "converging.example.test", Content: node.PublicIPv4, Comment: integrations.ManagedRecordPrefix + "site=" + site.ID + ";node=" + node.ID}},
	}}
	notifications := 0
	notifier := notifierFunc(func(context.Context, string, string) error {
		notifications++
		return nil
	})
	nodes, err := database.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	manager := HealthManager{Server: &Server{Store: database, DNS: dns, Notifier: notifier}}
	if err := manager.reconcileSiteDNS(context.Background(), site, nodes); err != nil {
		t.Fatal(err)
	}
	records := dns.Zones["zone-converging"]
	if len(records) != 1 || records[0].Content != node.PublicIPv4 {
		t.Fatalf("DNS changed during state convergence: %#v", records)
	}
	if notifications != 0 {
		t.Fatalf("convergence emitted %d availability notifications", notifications)
	}
}

func TestOriginCircuitDoesNotWithdrawSiteNodeDNS(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-origin-circuit", "203.0.113.123")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "origin-circuit-site", Domains: []string{"origin-circuit.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone-origin-circuit")
	if err != nil {
		t.Fatal(err)
	}
	site, err = database.MarkSitePublished(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SaveNodeState(node.ID, domain.DesiredState{Version: 1, NginxConfig: "# legacy config without site health"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := database.Heartbeat(node.ID, 1, "", nil); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := database.RecordNodeHealth(node.ID, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	nodes, err := database.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	record := integrations.DNSRecord{
		Name: site.Domains[0], Content: node.PublicIPv4,
		Comment: integrations.ManagedRecordPrefix + "site=" + site.ID + ";node=" + node.ID,
	}
	dns := &MemoryDNS{Zones: map[string][]integrations.DNSRecord{site.ZoneID: {record}}}
	server := &Server{Store: database, DNS: dns}
	manager := HealthManager{Server: server}
	collectedAt := time.Now().UTC()
	report := domain.MachineStatus{CollectedAt: collectedAt, OriginProbes: []domain.OriginProbeStatus{{
		CircuitState: domain.OriginCircuitOpen,
		References:   []domain.OriginPoolReference{{SiteID: site.ID, Role: "primary"}},
	}}}
	if !server.recordNodeMachineStatus(node.ID, report) {
		t.Fatal("open circuit report was not accepted")
	}
	if err := manager.reconcileSiteDNS(context.Background(), site, nodes); err != nil {
		t.Fatal(err)
	}
	if records := dns.Zones[site.ZoneID]; len(records) != 1 || records[0].Content != node.PublicIPv4 {
		t.Fatalf("open circuit changed DNS records = %#v", records)
	}

	report.CollectedAt = collectedAt.Add(time.Second)
	report.OriginProbes[0].CircuitState = domain.OriginCircuitRecovering
	server.recordNodeMachineStatus(node.ID, report)
	if err := manager.reconcileSiteDNS(context.Background(), site, nodes); err != nil {
		t.Fatal(err)
	}
	if records := dns.Zones[site.ZoneID]; len(records) != 1 || records[0].Content != node.PublicIPv4 {
		t.Fatalf("recovering circuit changed DNS records = %#v", records)
	}

	report.CollectedAt = collectedAt.Add(2 * time.Second)
	report.OriginProbes[0].CircuitState = domain.OriginCircuitClosed
	server.recordNodeMachineStatus(node.ID, report)
	if err := manager.reconcileSiteDNS(context.Background(), site, nodes); err != nil {
		t.Fatal(err)
	}
	if records := dns.Zones[site.ZoneID]; len(records) != 1 || records[0].Content != node.PublicIPv4 {
		t.Fatalf("closed circuit changed DNS records = %#v", records)
	}
}

func TestHealthSkipsNodeProbeDuringOnlineUpgrade(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-upgrading", "203.0.113.122")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	_, _, err = database.CreateOrGetNodeUpgrade(node.ID, domain.NodeUpgradeInstruction{
		Binary: domain.UpgradeArtifact{URL: "https://control.example.test/edge", SHA256: strings.Repeat("a", 64)},
	}, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	manager := HealthManager{Server: &Server{Store: database}, Client: nodeHealthClient(http.StatusServiceUnavailable)}
	if errorsFound := manager.reconcileNodeHealth(context.Background(), []domain.Node{node}); len(errorsFound) != 0 {
		t.Fatalf("upgrade probe errors = %v", errorsFound)
	}
	health, err := database.NodeHealth(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if health.LastCheckedAt != nil || health.ConsecutiveFailures != 0 {
		t.Fatalf("node health changed during online upgrade: %#v", health)
	}
}

func TestDrainedSitePoolWithdrawsManagedDNS(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name:          "drained-site",
		Domains:       []string{"cdn.example.test"},
		Nodes:         []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true},
		Enabled:       true,
	}, "zone-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.MarkSitePublished(site.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeDraining); err != nil {
		t.Fatal(err)
	}
	dns := &MemoryDNS{Zones: map[string][]integrations.DNSRecord{
		"zone-1": {{Name: "cdn.example.test", Content: node.PublicIPv4}},
	}}
	manager := HealthManager{Server: &Server{Store: database, DNS: dns}}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := dns.Zones["zone-1"]; len(got) != 0 {
		t.Fatalf("drained site DNS was not withdrawn: %#v", got)
	}
}

func TestSiteDNSUsesBackupOnlyAfterAllPrimaryNodesFailAndStopsAfterRecovery(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	primaryA, err := database.CreateNode("primary-a", "203.0.113.101")
	if err != nil {
		t.Fatal(err)
	}
	primaryB, err := database.CreateNode("primary-b", "203.0.113.102")
	if err != nil {
		t.Fatal(err)
	}
	backup, err := database.CreateNode("backup", "203.0.113.103")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "disaster recovery", Domains: []string{"dr.example.test"},
		Nodes: []string{primaryA.ID, primaryB.ID}, BackupNodes: []string{backup.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone-dr")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.MarkSitePublished(site.ID); err != nil {
		t.Fatal(err)
	}
	for _, node := range []domain.Node{primaryA, primaryB, backup} {
		if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
			t.Fatal(err)
		}
		if err := database.SaveNodeState(node.ID, domain.DesiredState{Version: 1, NginxConfig: "# legacy config"}, nil); err != nil {
			t.Fatal(err)
		}
		if err := database.Heartbeat(node.ID, 1, "", nil); err != nil {
			t.Fatal(err)
		}
		for range 5 {
			if _, err := database.RecordNodeHealth(node.ID, true, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	nodes, err := database.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	dns := &MemoryDNS{}
	manager := HealthManager{Server: &Server{Store: database, DNS: dns}}
	assertDNS := func(want ...string) {
		t.Helper()
		if err := manager.reconcileSiteDNS(context.Background(), site, nodes); err != nil {
			t.Fatal(err)
		}
		records := dns.Zones[site.ZoneID]
		if len(records) != len(want) {
			t.Fatalf("DNS records = %#v, want addresses %#v", records, want)
		}
		for index, address := range want {
			if records[index].Content != address {
				t.Fatalf("DNS records = %#v, want addresses %#v", records, want)
			}
		}
	}
	markUnhealthy := func(nodeID string) {
		t.Helper()
		for range 3 {
			if _, err := database.RecordNodeHealth(nodeID, false, "timeout"); err != nil {
				t.Fatal(err)
			}
		}
	}

	assertDNS(primaryA.PublicIPv4, primaryB.PublicIPv4)
	markUnhealthy(primaryA.ID)
	assertDNS(primaryB.PublicIPv4)
	markUnhealthy(primaryB.ID)
	assertDNS(backup.PublicIPv4)
	for range 5 {
		if _, err := database.RecordNodeHealth(primaryA.ID, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	assertDNS(primaryA.PublicIPv4)
}

func TestSiteHTTPSHealthRemovesOnlyFailingNodeAfterThreeChecks(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	badNode, err := database.CreateNode("edge-bad", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	goodNode, err := database.CreateNode("edge-good", "203.0.113.11")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name:          "site-health",
		Domains:       []string{"cdn.example.test"},
		Nodes:         []string{badNode.ID, goodNode.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true},
		Enabled:       true,
	}, "zone-1")
	if err != nil {
		t.Fatal(err)
	}
	site, err = database.MarkSitePublished(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := nginx.Render([]domain.Site{site})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []domain.Node{badNode, goodNode} {
		if err := database.SaveNodeState(node.ID, domain.DesiredState{Version: 1, NginxConfig: configuration}, nil); err != nil {
			t.Fatal(err)
		}
		if err := database.Heartbeat(node.ID, 1, "", nil); err != nil {
			t.Fatal(err)
		}
		for range 5 {
			if _, err := database.RecordNodeHealth(node.ID, true, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := database.RecordSiteNodeHealth(site.ID, node.ID, true, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	dns := &MemoryDNS{}
	manager := HealthManager{
		Server: &Server{Store: database, DNS: dns},
		Client: healthyNodeClient(),
		SiteProbe: func(_ context.Context, _ domain.Site, node domain.Node) (bool, string) {
			if node.ID == badNode.ID {
				return false, "cdn.example.test: certificate is valid for another site"
			}
			return true, ""
		},
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if err := manager.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		records := dns.Zones["zone-1"]
		if attempt < 3 && len(records) != 2 {
			t.Fatalf("attempt %d removed a node before the failure threshold: %#v", attempt, records)
		}
	}
	records := dns.Zones["zone-1"]
	if len(records) != 1 || records[0].Content != goodNode.PublicIPv4 {
		t.Fatalf("DNS did not retain only the healthy site endpoint: %#v", records)
	}
	manager.Client = nodeHealthClient(http.StatusServiceUnavailable)
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	records = dns.Zones["zone-1"]
	if len(records) != 1 || records[0].Content != goodNode.PublicIPv4 {
		t.Fatalf("generic probe failure reintroduced a site-ineligible node: %#v", records)
	}
	siteHealth, err := database.SiteNodeHealth(site.ID, badNode.ID)
	if err != nil || siteHealth.DNSEligible || siteHealth.ConsecutiveFailures != 3 {
		t.Fatalf("failing site-node health = %#v, %v", siteHealth, err)
	}
	nodeHealth, err := database.NodeHealth(badNode.ID)
	if err != nil || !nodeHealth.DNSEligible || nodeHealth.ConsecutiveFailures != 1 {
		t.Fatalf("generic node health should remain eligible after one failure: %#v, %v", nodeHealth, err)
	}
}

func TestSiteHTTPSHealthFallsBackDuringLegacyConfigRollout(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-legacy", "203.0.113.20")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name:          "legacy-site",
		Domains:       []string{"legacy.example.test"},
		Nodes:         []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true},
		Enabled:       true,
	}, "zone-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.MarkSitePublished(site.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.SaveNodeState(node.ID, domain.DesiredState{Version: 1, NginxConfig: "# legacy configuration without site health capability\n"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := database.Heartbeat(node.ID, 1, "", nil); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := database.RecordNodeHealth(node.ID, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	probeCalls := 0
	dns := &MemoryDNS{}
	manager := HealthManager{
		Server: &Server{Store: database, DNS: dns},
		Client: healthyNodeClient(),
		SiteProbe: func(context.Context, domain.Site, domain.Node) (bool, string) {
			probeCalls++
			return false, "must not be called"
		},
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if probeCalls != 0 {
		t.Fatalf("legacy config invoked site probe %d times", probeCalls)
	}
	records := dns.Zones["zone-legacy"]
	if len(records) != 1 || records[0].Content != node.PublicIPv4 {
		t.Fatalf("legacy config did not retain node-level DNS eligibility: %#v", records)
	}
}

func TestTCPOnlySiteKeepsNodeUntilHealthFailureThreshold(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	badNode, err := database.CreateNode("edge-transient", "203.0.113.30")
	if err != nil {
		t.Fatal(err)
	}
	goodNode, err := database.CreateNode("edge-healthy", "203.0.113.31")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "mail", Domains: []string{"mail.example.test"}, Nodes: []string{badNode.ID, goodNode.ID},
		TCPOnly: true, TCPForwards: []domain.TCPForward{{Name: "smtps", ListenPort: 9465, UpstreamHost: "mail-origin.example.test", UpstreamPort: 465}}, Enabled: true,
	}, "zone-mail")
	if err != nil {
		t.Fatal(err)
	}
	site, err = database.MarkSitePublished(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := nginx.Render([]domain.Site{site})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []domain.Node{badNode, goodNode} {
		if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
			t.Fatal(err)
		}
		if err := database.SaveNodeState(node.ID, domain.DesiredState{Version: 1, NginxConfig: configuration, NginxStreamConfig: "# stream\n", PublicPorts: []int{9465}}, nil); err != nil {
			t.Fatal(err)
		}
		if err := database.Heartbeat(node.ID, 1, "", nil); err != nil {
			t.Fatal(err)
		}
		for range 5 {
			if _, err := database.RecordNodeHealth(node.ID, true, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	if health, err := database.RecordNodeHealth(badNode.ID, false, "TCP 9465: timeout"); err != nil || !health.DNSEligible {
		t.Fatalf("transiently failing node health = %#v, %v", health, err)
	}
	nodes, err := database.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	dns := &MemoryDNS{}
	manager := HealthManager{Server: &Server{Store: database, DNS: dns}}
	if err := manager.reconcileSite(context.Background(), site, nodes); err != nil {
		t.Fatal(err)
	}
	records := dns.Zones["zone-mail"]
	if len(records) != 2 {
		t.Fatalf("transient failure removed TCP endpoint before threshold: %#v", records)
	}
}

func TestHealthReconciliationBoundsConcurrentNodeProbes(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for index := range 6 {
		if _, err := database.CreateNode(fmt.Sprintf("edge-%d", index), fmt.Sprintf("203.0.113.%d", index+1)); err != nil {
			t.Fatal(err)
		}
	}
	var active, maximum atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	client := &http.Client{Transport: healthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			prior := maximum.Load()
			if current <= prior || maximum.CompareAndSwap(prior, current) {
				break
			}
		}
		select {
		case entered <- struct{}{}:
		case <-release:
		}
		<-release
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader("ok\n")), Header: make(http.Header)}, nil
	})}
	manager := HealthManager{Server: &Server{Store: database, DNS: &MemoryDNS{}}, Client: client, WorkerLimit: 2, RoundTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- manager.Reconcile(context.Background()) }()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("bounded workers did not start")
		}
	}
	select {
	case <-entered:
		t.Fatal("more probes started than the worker limit allows")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum concurrent probes = %d, want 2", got)
	}
	status := manager.LastRound()
	if status.FinishedAt.IsZero() || status.ErrorCount != 0 || status.TimedOut {
		t.Fatalf("health round status = %#v", status)
	}
}

func TestHealthRoundTimeoutDoesNotCountUnscheduledProbeAsFailure(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-timeout", "203.0.113.40")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: healthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	manager := HealthManager{
		Server: &Server{Store: database, DNS: &MemoryDNS{}}, Client: client,
		WorkerLimit: 1, RoundTimeout: 25 * time.Millisecond,
	}
	err = manager.Reconcile(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("health reconciliation error = %v", err)
	}
	health, healthErr := database.NodeHealth(node.ID)
	if healthErr != nil {
		t.Fatal(healthErr)
	}
	if health.ConsecutiveFailures != 0 || health.LastError != "" {
		t.Fatalf("round timeout changed node health = %#v", health)
	}
	status := manager.LastRound()
	if !status.TimedOut || status.ErrorCount < 1 || status.DurationMS < 20 {
		t.Fatalf("timed-out health round status = %#v", status)
	}
}

type failingHealthDNS struct {
	mu    sync.Mutex
	calls []string
}

func (d *failingHealthDNS) Reconcile(_ context.Context, _ string, owner string, _ []integrations.DNSRecord) error {
	d.mu.Lock()
	d.calls = append(d.calls, owner)
	d.mu.Unlock()
	return fmt.Errorf("DNS failure for %s", owner)
}

func (*failingHealthDNS) RemoveNode(context.Context, string, string) error { return nil }

func (*failingHealthDNS) RemoveSiteNode(context.Context, string, string, string) error {
	return nil
}

func (*failingHealthDNS) RemoveSiteNodes(context.Context, string, string, []string) error {
	return nil
}

func TestHealthReconciliationAggregatesIndependentDNSErrors(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-dns-errors", "203.0.113.50")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"disabled-a", "disabled-b"} {
		if _, err := database.CreateSite(domain.Site{
			Name: name, Domains: []string{name + ".example.test"},
			Nodes:         []string{node.ID},
			PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: false,
		}, "zone-1"); err != nil {
			t.Fatal(err)
		}
	}
	dns := &failingHealthDNS{}
	manager := HealthManager{Server: &Server{Store: database, DNS: dns}, Client: healthyNodeClient()}
	err = manager.Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "disabled-a") || !strings.Contains(err.Error(), "disabled-b") {
		t.Fatalf("aggregated DNS error = %v", err)
	}
	dns.mu.Lock()
	calls := append([]string(nil), dns.calls...)
	dns.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("DNS reconcile calls = %#v", calls)
	}
	if status := manager.LastRound(); status.ErrorCount != 2 || status.Error == "" {
		t.Fatalf("failed health round status = %#v", status)
	}
}
