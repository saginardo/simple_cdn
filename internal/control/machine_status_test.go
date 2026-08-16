package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

func TestEdgeMachineStatusStoresNewestSnapshot(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("realtime-machine-edge", "203.0.113.91")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}
	report := controlTestMachineStatus(time.Now().UTC().Truncate(time.Millisecond))
	establishedConnections := int64(6)
	report.OriginProbes = []domain.OriginProbeStatus{{
		PoolID: "0123456789abcdef01234567", Address: "203.0.113.10:8080", Scheme: "http",
		KeepaliveConnections: 32, EstablishedConnections: &establishedConnections,
		References: []domain.OriginPoolReference{{SiteID: "site-1", Role: "primary"}},
		Healthy:    true, CircuitState: domain.OriginCircuitClosed, CheckedAt: report.CollectedAt,
		ServiceProbe: &domain.OriginProbeSample{
			Healthy: true, ConnectionReused: true, HeaderMS: 8.5, TotalMS: 9,
			HTTPStatus: http.StatusNoContent, CheckedAt: report.CollectedAt,
		},
	}}

	response := reportMachineStatus(t, server, node.ID, report)
	if response.Code != http.StatusAccepted {
		t.Fatalf("machine status response = %d %s", response.Code, response.Body.String())
	}
	stored := server.nodeMachineStatus(node, time.Now().UTC())
	if !stored.Available || stored.Report == nil || !stored.Report.CollectedAt.Equal(report.CollectedAt) ||
		len(stored.Report.OriginProbes) != 1 || stored.Report.OriginProbes[0].ServiceProbe == nil || stored.Report.OriginProbes[0].ServiceProbe.HeaderMS != 8.5 ||
		stored.Report.OriginProbes[0].EstablishedConnections == nil || *stored.Report.OriginProbes[0].EstablishedConnections != 6 {
		t.Fatalf("stored machine status = %#v", stored)
	}
	older := report
	older.CollectedAt = older.CollectedAt.Add(-time.Second)
	older.CPUUsagePercent = 99
	response = reportMachineStatus(t, server, node.ID, older)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"accepted":false`) {
		t.Fatalf("out-of-order machine status response = %d %s", response.Code, response.Body.String())
	}
	stored = server.nodeMachineStatus(node, time.Now().UTC())
	if stored.Report == nil || stored.Report.CPUUsagePercent != report.CPUUsagePercent {
		t.Fatalf("out-of-order status replaced latest snapshot: %#v", stored)
	}

	invalid := report
	invalid.MemoryUsedBytes = invalid.MemoryTotalBytes + 1
	response = reportMachineStatus(t, server, node.ID, invalid)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid machine status response = %d %s", response.Code, response.Body.String())
	}
}

func reportMachineStatus(t *testing.T, server *Server, nodeID string, report domain.MachineStatus) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/edge/v1/machine-status", bytes.NewReader(payload))
	request = request.WithContext(context.WithValue(request.Context(), edgeContextKey{}, nodeID))
	response := httptest.NewRecorder()
	server.edgeMachineStatus(response, request)
	return response
}

func reportMachineNetworkStatus(t *testing.T, server *Server, nodeID string, report domain.MachineNetworkStatus) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/edge/v1/machine-network", bytes.NewReader(payload))
	request = request.WithContext(context.WithValue(request.Context(), edgeContextKey{}, nodeID))
	response := httptest.NewRecorder()
	server.edgeMachineNetworkStatus(response, request)
	return response
}

func reportMachineOriginStatus(t *testing.T, server *Server, nodeID string, report domain.MachineOriginStatus) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/edge/v1/machine-origin", bytes.NewReader(payload))
	request = request.WithContext(context.WithValue(request.Context(), edgeContextKey{}, nodeID))
	response := httptest.NewRecorder()
	server.edgeMachineOriginStatus(response, request)
	return response
}

func TestEdgeMachineNetworkStatusStoresNewestIntervalSample(t *testing.T) {
	server := &Server{}
	nodeID := "adaptive-network-edge"
	report := domain.MachineNetworkStatus{
		NetworkInterface: "eth0", NetworkRXBytesPerSec: 8192, NetworkTXBytesPerSec: 4096,
		SampleSeconds: 1, CollectedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
	response := reportMachineNetworkStatus(t, server, nodeID, report)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"accepted":true`) {
		t.Fatalf("machine network response = %d %s", response.Code, response.Body.String())
	}
	stored := server.machineNetworkStatuses[nodeID]
	if stored.NetworkRXBytesPerSec != report.NetworkRXBytesPerSec || !stored.CollectedAt.Equal(report.CollectedAt) {
		t.Fatalf("stored machine network status = %#v", stored)
	}

	older := report
	older.CollectedAt = older.CollectedAt.Add(-time.Second)
	older.NetworkRXBytesPerSec = 1
	response = reportMachineNetworkStatus(t, server, nodeID, older)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"accepted":false`) {
		t.Fatalf("out-of-order machine network response = %d %s", response.Code, response.Body.String())
	}
	if server.machineNetworkStatuses[nodeID].NetworkRXBytesPerSec != report.NetworkRXBytesPerSec {
		t.Fatalf("out-of-order network status replaced latest: %#v", server.machineNetworkStatuses[nodeID])
	}

	invalid := report
	invalid.SampleSeconds = 0
	response = reportMachineNetworkStatus(t, server, nodeID, invalid)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid machine network response = %d %s", response.Code, response.Body.String())
	}
}

func TestNodeMachineStatusUsesTheNewestNetworkSnapshot(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	nodeID := "adaptive-network-order"
	host := controlTestMachineStatus(now.Add(-time.Second))
	host.NetworkRXBytesPerSec = 1024
	olderNetwork := domain.MachineNetworkStatus{
		NetworkInterface: "eth0", NetworkRXBytesPerSec: 8192, NetworkTXBytesPerSec: 4096,
		SampleSeconds: 1, CollectedAt: now.Add(-2 * time.Second),
	}
	server := &Server{
		machineStatuses:        map[string]domain.MachineStatus{nodeID: host},
		machineNetworkStatuses: map[string]domain.MachineNetworkStatus{nodeID: olderNetwork},
		machineStatusDemandActive: map[string]bool{
			nodeID: true,
		},
	}
	node := domain.Node{ID: nodeID, Capabilities: []string{domain.EdgeCapabilityMachineStatusAdaptive}}
	status := server.nodeMachineStatus(node, now)
	if status.Network != nil || status.NetworkStale || status.Report == nil || status.Report.NetworkRXBytesPerSec != host.NetworkRXBytesPerSec {
		t.Fatalf("older network sample overrode the host snapshot: %#v", status)
	}

	newerNetwork := olderNetwork
	newerNetwork.CollectedAt = now
	newerNetwork.NetworkRXBytesPerSec = 16384
	server.machineNetworkStatuses[nodeID] = newerNetwork
	status = server.nodeMachineStatus(node, now)
	if status.Network == nil || status.NetworkStale || status.Network.NetworkRXBytesPerSec != newerNetwork.NetworkRXBytesPerSec {
		t.Fatalf("newer network sample was not selected: %#v", status)
	}

	legacy := domain.Node{ID: nodeID, Capabilities: []string{domain.EdgeCapabilityMachineStatusStream}}
	status = server.nodeMachineStatus(legacy, now)
	if status.Network != nil {
		t.Fatalf("legacy node reused an adaptive network sample: %#v", status)
	}
}

func TestEdgeMachineOriginStatusStoresNewestSnapshotAndMergesWithHost(t *testing.T) {
	server := &Server{}
	nodeID := "adaptive-origin-edge"
	now := time.Now().UTC().Truncate(time.Millisecond)
	host := controlTestMachineStatus(now.Add(-2 * time.Second))
	server.recordNodeMachineStatus(nodeID, host)
	report := domain.MachineOriginStatus{
		OriginProbes: []domain.OriginProbeStatus{controlTestOriginProbe(now.Add(-time.Second))},
		CollectedAt:  now,
	}
	response := reportMachineOriginStatus(t, server, nodeID, report)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"accepted":true`) {
		t.Fatalf("machine origin response = %d %s", response.Code, response.Body.String())
	}
	status := server.nodeMachineStatus(domain.Node{
		ID: nodeID, Capabilities: []string{domain.EdgeCapabilityMachineStatusAdaptive},
	}, now)
	if status.Report == nil || len(status.Report.OriginProbes) != 1 ||
		status.Report.OriginProbes[0].PoolID != report.OriginProbes[0].PoolID ||
		status.OriginCollectedAt == nil || !status.OriginCollectedAt.Equal(report.OriginProbes[0].CheckedAt) || status.OriginStale {
		t.Fatalf("combined machine origin status = %#v", status)
	}

	older := report
	older.CollectedAt = older.CollectedAt.Add(-time.Second)
	older.OriginProbes = nil
	response = reportMachineOriginStatus(t, server, nodeID, older)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"accepted":false`) {
		t.Fatalf("out-of-order machine origin response = %d %s", response.Code, response.Body.String())
	}
	if len(server.machineOriginStatuses[nodeID].OriginProbes) != 1 {
		t.Fatalf("out-of-order origin status replaced latest: %#v", server.machineOriginStatuses[nodeID])
	}

	invalid := report
	invalid.CollectedAt = time.Time{}
	response = reportMachineOriginStatus(t, server, nodeID, invalid)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid machine origin response = %d %s", response.Code, response.Body.String())
	}
}

func TestNodeMachineOriginFreshnessUsesLatestProbeTime(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	nodeID := "adaptive-origin-freshness"
	host := controlTestMachineStatus(now.Add(-time.Second))
	staleCheckedAt := now.Add(-nodeMachineRealtimeFreshness - time.Second)
	origin := domain.MachineOriginStatus{
		OriginProbes: []domain.OriginProbeStatus{controlTestOriginProbe(staleCheckedAt)},
		CollectedAt:  now,
	}
	server := &Server{
		machineStatuses: map[string]domain.MachineStatus{nodeID: host},
	}
	if !server.recordNodeMachineOriginStatus(nodeID, origin) {
		t.Fatal("initial origin report was not recorded")
	}
	node := domain.Node{ID: nodeID, Capabilities: []string{domain.EdgeCapabilityMachineStatusAdaptive}}

	status := server.nodeMachineStatus(node, now)
	if status.OriginCollectedAt == nil || !status.OriginCollectedAt.Equal(staleCheckedAt) || !status.OriginStale {
		t.Fatalf("new report envelope hid stale origin probes: %#v", status)
	}

	later := now.Add(5 * time.Second)
	origin.CollectedAt = later
	if !server.recordNodeMachineOriginStatus(nodeID, origin) {
		t.Fatal("repeated origin report was not recorded")
	}
	status = server.nodeMachineStatus(node, later)
	if status.OriginCollectedAt == nil || !status.OriginCollectedAt.Equal(staleCheckedAt) || !status.OriginStale {
		t.Fatalf("repeated report envelope refreshed stale origin probes: %#v", status)
	}

	refreshedAt := later.Add(5 * time.Second)
	freshCheckedAt := refreshedAt.Add(-time.Second)
	freshProbe := controlTestOriginProbe(freshCheckedAt)
	freshProbe.PoolID = strings.Repeat("b", 24)
	origin.OriginProbes = append(origin.OriginProbes, freshProbe)
	origin.CollectedAt = refreshedAt
	if !server.recordNodeMachineOriginStatus(nodeID, origin) {
		t.Fatal("refreshed origin report was not recorded")
	}
	status = server.nodeMachineStatus(node, refreshedAt)
	if status.OriginCollectedAt == nil || !status.OriginCollectedAt.Equal(freshCheckedAt) || status.OriginStale {
		t.Fatalf("latest origin probe did not restore freshness: %#v", status)
	}

	if collectedAt := latestOriginProbeCheckedAt(nil, origin.CollectedAt); !collectedAt.Equal(origin.CollectedAt) {
		t.Fatalf("empty origin probes collected at %s, want report time %s", collectedAt, origin.CollectedAt)
	}
}

func TestMachineStatusDemandStreamsActivePolicyUntilLastSubscriberLeaves(t *testing.T) {
	server := &Server{machineStatusDemandGrace: 30 * time.Millisecond}
	policies, initial, unsubscribePolicy := server.subscribeMachineStatusPolicy("node-1")
	defer unsubscribePolicy()
	assertMachineStatusPolicyValue(t, initial, domain.DefaultMachineStatusIntervalSeconds, 0)

	_, unsubscribeFirst := server.subscribeMachineStatus("node-1")
	assertMachineStatusPolicy(t, policies, domain.ActiveMachineStatusIntervalSeconds, domain.DefaultMachineNetworkIntervalSeconds)
	_, unsubscribeSecond := server.subscribeMachineStatus("node-1")
	assertNoMachineStatusPolicy(t, policies, 10*time.Millisecond)
	unsubscribeFirst()
	assertNoMachineStatusPolicy(t, policies, 10*time.Millisecond)
	unsubscribeSecond()

	// A reconnect inside the grace period must keep the one-second sampler active.
	time.Sleep(10 * time.Millisecond)
	_, unsubscribeReconnected := server.subscribeMachineStatus("node-1")
	assertNoMachineStatusPolicy(t, policies, 40*time.Millisecond)
	unsubscribeReconnected()
	assertMachineStatusPolicy(t, policies, domain.DefaultMachineStatusIntervalSeconds, 0)
}

func TestMachineStatusPolicySSEStreamsDemandTransitions(t *testing.T) {
	server := &Server{machineStatusDemandGrace: 20 * time.Millisecond}
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		ctx := context.WithValue(request.Context(), edgeContextKey{}, "node-1")
		server.machineStatusPolicyEvents(response, request.WithContext(ctx))
	})
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("policy SSE response = %s, content-type=%q", response.Status, response.Header.Get("Content-Type"))
	}

	reader := bufio.NewReader(response.Body)
	assertMachineStatusPolicyValue(t, readMachineStatusPolicyEvent(t, reader), domain.DefaultMachineStatusIntervalSeconds, 0)
	_, unsubscribe := server.subscribeMachineStatus("node-1")
	assertMachineStatusPolicyValue(t, readMachineStatusPolicyEvent(t, reader), domain.ActiveMachineStatusIntervalSeconds, domain.DefaultMachineNetworkIntervalSeconds)
	unsubscribe()
	assertMachineStatusPolicyValue(t, readMachineStatusPolicyEvent(t, reader), domain.DefaultMachineStatusIntervalSeconds, 0)
}

func assertMachineStatusPolicy(t *testing.T, policies <-chan domain.MachineStatusSamplingPolicy, hostIntervalSeconds, networkIntervalSeconds int) {
	t.Helper()
	select {
	case policy := <-policies:
		assertMachineStatusPolicyValue(t, policy, hostIntervalSeconds, networkIntervalSeconds)
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for machine status policy intervals host=%d network=%d", hostIntervalSeconds, networkIntervalSeconds)
	}
}

func assertMachineStatusPolicyValue(t *testing.T, policy domain.MachineStatusSamplingPolicy, hostIntervalSeconds, networkIntervalSeconds int) {
	t.Helper()
	if policy.HostIntervalSeconds != hostIntervalSeconds ||
		policy.NetworkIntervalSeconds != networkIntervalSeconds ||
		policy.OriginIntervalSeconds != domain.DefaultMachineOriginIntervalSeconds {
		t.Fatalf("machine status policy = %#v, want host=%d network=%d origin=%d", policy,
			hostIntervalSeconds, networkIntervalSeconds, domain.DefaultMachineOriginIntervalSeconds)
	}
}

func assertNoMachineStatusPolicy(t *testing.T, policies <-chan domain.MachineStatusSamplingPolicy, wait time.Duration) {
	t.Helper()
	select {
	case policy := <-policies:
		t.Fatalf("unexpected machine status policy = %#v", policy)
	case <-time.After(wait):
	}
}

func TestMachineStatusSSERequiresAdminAndStreamsUpdates(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "totp"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "machine-session", "csrf", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("sse-machine-edge", "203.0.113.92")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCapabilities(node.ID, []string{
		domain.EdgeCapabilityMachineStatus,
		domain.EdgeCapabilityMachineStatusStream,
		domain.EdgeCapabilityMachineStatusAdaptive,
	}); err != nil {
		t.Fatal(err)
	}
	node, err = database.GetNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}
	initial := controlTestMachineStatus(time.Now().UTC().Truncate(time.Millisecond))
	initial.OriginProbes = []domain.OriginProbeStatus{{
		PoolID: "0123456789abcdef01234567", Address: "203.0.113.10:8080", Scheme: "http",
		KeepaliveConnections: 16, References: []domain.OriginPoolReference{{SiteID: "site-1", Role: "primary"}},
		Healthy: false, CircuitState: domain.OriginCircuitOpen, ServiceConsecutiveFailures: 2,
		ServiceProbe: &domain.OriginProbeSample{
			Healthy: false, Error: "connection refused", CheckedAt: initial.CollectedAt,
		},
		CheckedAt: initial.CollectedAt,
	}}
	server.recordNodeMachineStatus(node.ID, initial)
	handler := server.Handler()

	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/api/nodes/"+node.ID+"/machine-status/events", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated SSE status = %d", unauthenticated.Code)
	}

	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/nodes/"+node.ID+"/machine-status/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(&http.Cookie{Name: "cdn_session", Value: "machine-session"})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("SSE response = %s, content-type=%q", response.Status, response.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(response.Body)
	first := readMachineStatusEvent(t, reader)
	if !first.Available || first.Report == nil || !first.Report.CollectedAt.Equal(initial.CollectedAt) ||
		len(first.Report.OriginProbes) != 1 || first.Report.OriginProbes[0].CircuitState != domain.OriginCircuitOpen {
		t.Fatalf("initial SSE machine status = %#v", first)
	}
	// The initial read seeds the capability profile. Subsequent telemetry events
	// must remain entirely in memory instead of querying the node on every update.
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	updated := initial
	updated.CollectedAt = updated.CollectedAt.Add(5 * time.Second)
	updated.CPUUsagePercent = 73
	server.recordNodeMachineStatus(node.ID, updated)
	second := readMachineStatusEvent(t, reader)
	if second.Report == nil || second.Report.CPUUsagePercent != 73 || !second.Report.CollectedAt.Equal(updated.CollectedAt) {
		t.Fatalf("updated SSE machine status = %#v", second)
	}

	network := domain.MachineNetworkStatus{
		NetworkInterface: "eth0", NetworkRXBytesPerSec: 8192, NetworkTXBytesPerSec: 4096,
		SampleSeconds: 1, CollectedAt: updated.CollectedAt.Add(time.Second),
	}
	server.recordNodeMachineNetworkStatus(node.ID, network)
	third := readMachineStatusEvent(t, reader)
	if third.Report == nil || third.Report.CPUUsagePercent != 73 || third.Network == nil ||
		third.Network.NetworkRXBytesPerSec != 8192 || !third.Network.CollectedAt.Equal(network.CollectedAt) {
		t.Fatalf("network SSE machine status = %#v", third)
	}

	origin := domain.MachineOriginStatus{
		OriginProbes: []domain.OriginProbeStatus{controlTestOriginProbe(network.CollectedAt.Add(time.Second))},
		CollectedAt:  network.CollectedAt.Add(time.Second),
	}
	server.recordNodeMachineOriginStatus(node.ID, origin)
	fourth := readMachineStatusEvent(t, reader)
	if fourth.Report == nil || len(fourth.Report.OriginProbes) != 1 || fourth.OriginCollectedAt == nil ||
		!fourth.OriginCollectedAt.Equal(origin.CollectedAt) {
		t.Fatalf("origin SSE machine status = %#v", fourth)
	}
}

func TestMachineStatusSSERefreshesCapabilitiesAfterAgentUpgrade(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.CreateInitialAdmin("hash", "totp"); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateSession("admin", "upgrade-session", "csrf", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("upgrading-machine-edge", "203.0.113.93")
	if err != nil {
		t.Fatal(err)
	}
	legacyCapabilities := []string{domain.EdgeCapabilityMachineStatus, domain.EdgeCapabilityMachineStatusStream}
	if err := database.SetNodeCapabilities(node.ID, legacyCapabilities); err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}
	host := controlTestMachineStatus(time.Now().UTC().Add(-20 * time.Second))
	server.recordNodeMachineStatus(node.ID, host)

	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/nodes/"+node.ID+"/machine-status/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(&http.Cookie{Name: "cdn_session", Value: "upgrade-session"})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if initial := readMachineStatusEvent(t, reader); !initial.Stale {
		t.Fatalf("legacy stream unexpectedly accepted a 20-second-old host snapshot: %#v", initial)
	}

	adaptiveCapabilities := append(append([]string(nil), legacyCapabilities...), domain.EdgeCapabilityMachineStatusAdaptive)
	payload, err := json.Marshal(heartbeatRequest{Capabilities: adaptiveCapabilities})
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := httptest.NewRequest(http.MethodPost, "/api/edge/v1/heartbeat", bytes.NewReader(payload))
	heartbeat = heartbeat.WithContext(context.WithValue(heartbeat.Context(), edgeContextKey{}, node.ID))
	heartbeatResponse := httptest.NewRecorder()
	server.heartbeat(heartbeatResponse, heartbeat)
	if heartbeatResponse.Code != http.StatusOK {
		t.Fatalf("upgrade heartbeat response = %d %s", heartbeatResponse.Code, heartbeatResponse.Body.String())
	}
	if upgraded := readMachineStatusEvent(t, reader); upgraded.Stale {
		t.Fatalf("SSE retained legacy freshness after the agent upgraded: %#v", upgraded)
	}

	payload, err = json.Marshal(heartbeatRequest{Capabilities: legacyCapabilities})
	if err != nil {
		t.Fatal(err)
	}
	heartbeat = httptest.NewRequest(http.MethodPost, "/api/edge/v1/heartbeat", bytes.NewReader(payload))
	heartbeat = heartbeat.WithContext(context.WithValue(heartbeat.Context(), edgeContextKey{}, node.ID))
	heartbeatResponse = httptest.NewRecorder()
	server.heartbeat(heartbeatResponse, heartbeat)
	if heartbeatResponse.Code != http.StatusOK {
		t.Fatalf("downgrade heartbeat response = %d %s", heartbeatResponse.Code, heartbeatResponse.Body.String())
	}
	if downgraded := readMachineStatusEvent(t, reader); !downgraded.Stale {
		t.Fatalf("SSE retained adaptive freshness after the capability was removed: %#v", downgraded)
	}
}

func readMachineStatusEvent(t *testing.T, reader *bufio.Reader) nodeMachineStatusResponse {
	t.Helper()
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var status nodeMachineStatusResponse
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: "))), &status); err != nil {
			t.Fatal(err)
		}
		return status
	}
}

func readMachineStatusPolicyEvent(t *testing.T, reader *bufio.Reader) domain.MachineStatusSamplingPolicy {
	t.Helper()
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var policy domain.MachineStatusSamplingPolicy
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: "))), &policy); err != nil {
			t.Fatal(err)
		}
		return policy
	}
}

func controlTestOriginProbe(checkedAt time.Time) domain.OriginProbeStatus {
	return domain.OriginProbeStatus{
		PoolID: strings.Repeat("a", 24), Address: "203.0.113.10:8080", Scheme: "http",
		KeepaliveConnections: 16, References: []domain.OriginPoolReference{{SiteID: "site-1", Role: "primary"}},
		Healthy: true, CircuitState: domain.OriginCircuitClosed, CheckedAt: checkedAt,
		ServiceProbe: &domain.OriginProbeSample{Healthy: true, HeaderMS: 2, TotalMS: 3, HTTPStatus: http.StatusNoContent, CheckedAt: checkedAt},
	}
}
