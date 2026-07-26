package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

func TestHeartbeatReturnsControlManifestForCapableEdge(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("manifest-edge", "203.0.113.90")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}
	payload, err := json.Marshal(heartbeatRequest{Capabilities: []string{domain.EdgeCapabilityControlManifest}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/edge/v1/heartbeat", bytes.NewReader(payload))
	request = request.WithContext(context.WithValue(request.Context(), edgeContextKey{}, node.ID))
	response := httptest.NewRecorder()
	server.heartbeat(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d, body=%s", response.Code, response.Body.String())
	}
	var result domain.EdgeHeartbeatResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Control == nil || result.Control.DesiredStateVersion != 0 || !result.Control.AccessLogGzip ||
		!validSHA256Digest(result.Control.MonitoringRevision) || !validSHA256Digest(result.Control.SecurityRevision) {
		t.Fatalf("heartbeat manifest = %#v", result)
	}

	before := *result.Control
	if err := database.SaveNodeState(node.ID, domain.DesiredState{Version: 7, NginxConfig: "# state\n"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateMonitoringTarget("control", "192.0.2.20:443"); err != nil {
		t.Fatal(err)
	}
	after, err := server.edgeControlManifest(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.DesiredStateVersion != 7 || after.MonitoringRevision == before.MonitoringRevision || after.SecurityRevision != before.SecurityRevision {
		t.Fatalf("updated manifest = %#v, before=%#v", after, before)
	}
}

func TestEdgePullEndpointsSupportRevisionETags(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("etag-edge", "203.0.113.91")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SaveNodeState(node.ID, domain.DesiredState{Version: 3, NginxConfig: "# state\n"}, nil); err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: database}

	tests := []struct {
		path   string
		handle func(http.ResponseWriter, *http.Request)
	}{
		{path: "/api/edge/v1/desired-state", handle: server.desiredState},
		{path: "/api/edge/v1/monitoring-targets", handle: server.edgeMonitoringTargets},
		{path: "/api/edge/v1/security-bans", handle: server.edgeSecurityBans},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			firstRequest := httptest.NewRequest(http.MethodGet, test.path, nil)
			firstRequest = firstRequest.WithContext(context.WithValue(firstRequest.Context(), edgeContextKey{}, node.ID))
			first := httptest.NewRecorder()
			test.handle(first, firstRequest)
			if first.Code != http.StatusOK || first.Header().Get("ETag") == "" {
				t.Fatalf("first response status=%d etag=%q body=%s", first.Code, first.Header().Get("ETag"), first.Body.String())
			}
			secondRequest := httptest.NewRequest(http.MethodGet, test.path, nil)
			secondRequest.Header.Set("If-None-Match", first.Header().Get("ETag"))
			secondRequest = secondRequest.WithContext(context.WithValue(secondRequest.Context(), edgeContextKey{}, node.ID))
			second := httptest.NewRecorder()
			test.handle(second, secondRequest)
			if second.Code != http.StatusNotModified || second.Body.Len() != 0 {
				t.Fatalf("conditional response status=%d body=%q", second.Code, second.Body.String())
			}
		})
	}
}

func TestSecurityBanRevisionIgnoresOrderingAndTracksExpiry(t *testing.T) {
	now := time.Now().UTC()
	first := []domain.SecurityBan{{IP: "8.8.8.8", ExpiresAt: now.Add(time.Hour)}, {IP: "1.1.1.1", ExpiresAt: now.Add(2 * time.Hour)}}
	reordered := []domain.SecurityBan{first[1], first[0]}
	if securityBansRevision(first) != securityBansRevision(reordered) {
		t.Fatal("security revision depends on database ordering")
	}
	reordered[0].ExpiresAt = reordered[0].ExpiresAt.Add(time.Second)
	if securityBansRevision(first) == securityBansRevision(reordered) {
		t.Fatal("security revision did not track expiry changes")
	}
}
