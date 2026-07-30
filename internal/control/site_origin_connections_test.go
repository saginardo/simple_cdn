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

func TestSiteOriginConnectionsFiltersNodeReportsBySite(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	reporting, err := database.CreateNode("a-reporting", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := database.CreateNode("b-waiting", "203.0.113.11")
	if err != nil {
		t.Fatal(err)
	}
	for _, nodeID := range []string{reporting.ID, waiting.ID} {
		if err := database.SetNodeCapabilities(nodeID, []string{domain.EdgeCapabilityMachineStatus}); err != nil {
			t.Fatal(err)
		}
	}
	site, err := database.CreateSite(domain.Site{
		Name: "Target Site", Domains: []string{"target.example.test"}, Nodes: []string{reporting.ID, waiting.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	other, err := database.CreateSite(domain.Site{
		Name: "Other Site", Domains: []string{"other.example.test"}, Nodes: []string{reporting.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Now().UTC().Truncate(time.Millisecond)
	report := controlTestMachineStatus(checkedAt)
	establishedConnections := int64(4)
	report.OriginProbes = []domain.OriginProbeStatus{
		{
			PoolID: "shared", Address: "10.253.0.1:8443", Scheme: "http",
			EstablishedConnections: &establishedConnections,
			References: []domain.OriginPoolReference{
				{SiteID: site.ID, Role: "primary"},
				{SiteID: other.ID, Role: "backup"},
			},
			Healthy: true, CircuitState: domain.OriginCircuitClosed, CheckedAt: checkedAt,
		},
		{
			PoolID: "other-only", Address: "192.0.2.50:443", Scheme: "https",
			References: []domain.OriginPoolReference{{SiteID: other.ID, Role: "primary"}},
			Healthy:    true, CircuitState: domain.OriginCircuitClosed, CheckedAt: checkedAt,
		},
	}
	server := &Server{Store: database}
	if !server.recordNodeMachineStatus(reporting.ID, report) {
		t.Fatal("machine status was not recorded")
	}

	request := httptest.NewRequest(http.MethodGet, "/api/sites/"+site.ID+"/origin-connections", nil)
	request.SetPathValue("id", site.ID)
	response := httptest.NewRecorder()
	server.siteOriginConnections(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var result siteOriginConnectionsResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.SiteID != site.ID || len(result.Nodes) != 2 {
		t.Fatalf("result = %#v", result)
	}
	gotReporting := result.Nodes[0]
	if gotReporting.NodeID != reporting.ID || !gotReporting.Available || gotReporting.Stale || gotReporting.CollectedAt == nil {
		t.Fatalf("reporting node = %#v", gotReporting)
	}
	if len(gotReporting.Probes) != 1 || gotReporting.Probes[0].PoolID != "shared" ||
		gotReporting.Probes[0].EstablishedConnections == nil || *gotReporting.Probes[0].EstablishedConnections != 4 {
		t.Fatalf("filtered probes = %#v", gotReporting.Probes)
	}
	if references := gotReporting.Probes[0].References; len(references) != 1 || references[0].SiteID != site.ID || references[0].Role != "primary" {
		t.Fatalf("filtered references = %#v", references)
	}
	gotWaiting := result.Nodes[1]
	if gotWaiting.NodeID != waiting.ID || gotWaiting.Available || gotWaiting.UnavailableReason != "等待边缘节点首次上报机器状态" || len(gotWaiting.Probes) != 0 {
		t.Fatalf("waiting node = %#v", gotWaiting)
	}
}

func TestSiteOriginConnectionsReturnsNotFound(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	request := httptest.NewRequest(http.MethodGet, "/api/sites/missing/origin-connections", nil)
	request.SetPathValue("id", "missing")
	response := httptest.NewRecorder()
	(&Server{Store: database}).siteOriginConnections(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}
