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
	report.OriginProbes = []domain.OriginProbeStatus{{
		PoolID: "0123456789abcdef01234567", Address: "203.0.113.10:8080", Scheme: "http",
		KeepaliveConnections: 32, References: []domain.OriginPoolReference{{SiteID: "site-1", Role: "primary"}},
		Healthy: true, CircuitState: domain.OriginCircuitClosed, CheckedAt: report.CollectedAt,
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
		len(stored.Report.OriginProbes) != 1 || stored.Report.OriginProbes[0].ServiceProbe == nil || stored.Report.OriginProbes[0].ServiceProbe.HeaderMS != 8.5 {
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
	if err := database.SetNodeCapabilities(node.ID, []string{domain.EdgeCapabilityMachineStatus, domain.EdgeCapabilityMachineStatusStream}); err != nil {
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

	updated := initial
	updated.CollectedAt = updated.CollectedAt.Add(5 * time.Second)
	updated.CPUUsagePercent = 73
	server.recordNodeMachineStatus(node.ID, updated)
	second := readMachineStatusEvent(t, reader)
	if second.Report == nil || second.Report.CPUUsagePercent != 73 || !second.Report.CollectedAt.Equal(updated.CollectedAt) {
		t.Fatalf("updated SSE machine status = %#v", second)
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
