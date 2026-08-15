package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/logstore"
	"simple_cdn/internal/store"
)

type overviewLogStore struct {
	logstore.Noop
	siteID string
}

func (s overviewLogStore) Overview(_ context.Context, _, to time.Time) ([]logstore.OverviewBucket, error) {
	return []logstore.OverviewBucket{{
		Hour: to.Add(-time.Hour).Truncate(time.Hour), SiteID: s.siteID, Status: 404, Requests: 4,
		DownstreamBytes: 400, UpstreamBytes: 120,
	}}, nil
}

func TestOverviewRouteRequiresAdmin(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	(&Server{}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unexpected response status: %d", response.Code)
	}
}

func TestOverviewHandlerReturnsConfiguredSitesAndLogData(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("overview-edge", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{Name: "Overview Site", Domains: []string{"cdn.example.test"}, Nodes: []string{node.ID}, PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true}, "zone")
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{Store: database, Logs: overviewLogStore{siteID: site.ID}}
	response := httptest.NewRecorder()
	server.overview(response, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected response status: %d, body: %s", response.Code, response.Body.String())
	}
	var payload overviewPayload
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Totals.Requests != 4 || payload.Totals.DownstreamBytes != 400 || payload.Totals.UpstreamBytes != 120 ||
		payload.Totals.ErrorRequests != 4 || len(payload.Sites) != 1 || payload.Sites[0].ID != site.ID ||
		payload.Sites[0].Requests != 4 || payload.Sites[0].DownstreamBytes != 400 || payload.Sites[0].UpstreamBytes != 120 ||
		payload.Sites[0].ErrorRequests != 4 {
		t.Fatalf("unexpected overview payload: %#v", payload)
	}
	if len(payload.Sites[0].StatusCodes) != 1 || payload.Sites[0].StatusCodes[0].Code != 404 || payload.Sites[0].StatusCodes[0].Requests != 4 {
		t.Fatalf("unexpected site status codes: %#v", payload.Sites[0].StatusCodes)
	}
}

func TestBuildOverviewPayloadAggregatesAndZeroFills(t *testing.T) {
	from := time.Date(2026, 7, 13, 12, 30, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	sites := []domain.SiteSummary{
		{ID: "quiet", Name: "Quiet", Domains: []string{"quiet.example.test"}},
		{ID: "busy", Name: "Busy", Domains: []string{"busy.example.test"}},
	}
	buckets := []logstore.OverviewBucket{
		{Hour: time.Date(2026, 7, 13, 13, 0, 0, 0, time.UTC), SiteID: "busy", Status: 200, Requests: 90, DownstreamBytes: 9000, UpstreamBytes: 900},
		{Hour: time.Date(2026, 7, 13, 13, 0, 0, 0, time.UTC), SiteID: "busy", Status: 404, Requests: 8, DownstreamBytes: 800, UpstreamBytes: 80},
		{Hour: time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC), SiteID: "busy", Status: 500, Requests: 2, DownstreamBytes: 200, UpstreamBytes: 20},
	}

	payload := buildOverviewPayload(from, to, sites, buckets)
	if payload.BucketSeconds != 3600 || len(payload.Series) != 25 {
		t.Fatalf("unexpected bucket metadata: seconds=%d points=%d", payload.BucketSeconds, len(payload.Series))
	}
	if payload.Totals.Requests != 100 || payload.Totals.DownstreamBytes != 10000 || payload.Totals.UpstreamBytes != 1000 || payload.Totals.ErrorRequests != 10 {
		t.Fatalf("unexpected totals: %#v", payload.Totals)
	}
	if len(payload.StatusCodes) != 3 || payload.StatusCodes[0].Code != 200 || payload.StatusCodes[1].Code != 404 || payload.StatusCodes[2].Code != 500 {
		t.Fatalf("unexpected status sorting: %#v", payload.StatusCodes)
	}
	if len(payload.Sites) != 2 || payload.Sites[0].ID != "busy" || payload.Sites[0].Requests != 100 ||
		payload.Sites[0].DownstreamBytes != 10000 || payload.Sites[0].UpstreamBytes != 1000 ||
		payload.Sites[0].ErrorRequests != 10 || payload.Sites[1].ID != "quiet" {
		t.Fatalf("unexpected site sorting: %#v", payload.Sites)
	}
	if len(payload.Sites[0].StatusCodes) != 3 || payload.Sites[0].StatusCodes[0].Code != 200 || payload.Sites[0].StatusCodes[1].Code != 404 || payload.Sites[0].StatusCodes[2].Code != 500 {
		t.Fatalf("unexpected site status sorting: %#v", payload.Sites[0].StatusCodes)
	}
	busyFirstPoint := payload.Sites[0].Series[1]
	if busyFirstPoint.Requests != 98 || busyFirstPoint.DownstreamBytes != 9800 || busyFirstPoint.UpstreamBytes != 980 || busyFirstPoint.ErrorRequests != 8 {
		t.Fatalf("unexpected site hourly point: %#v", busyFirstPoint)
	}
	if len(payload.Sites[1].Series) != 25 || payload.Sites[1].Series[0].Requests != 0 {
		t.Fatalf("quiet site was not zero-filled: %#v", payload.Sites[1].Series)
	}
	if payload.Sites[1].StatusCodes == nil || len(payload.Sites[1].StatusCodes) != 0 {
		t.Fatalf("quiet site status codes must be an empty array: %#v", payload.Sites[1].StatusCodes)
	}
}

func TestBuildOverviewPayloadUsesStatusCodeAsTieBreaker(t *testing.T) {
	from := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	payload := buildOverviewPayload(from, to, nil, []logstore.OverviewBucket{
		{Hour: from, Status: 500, Requests: 2},
		{Hour: from, Status: 404, Requests: 2},
	})
	if len(payload.StatusCodes) != 2 || payload.StatusCodes[0].Code != 404 || payload.StatusCodes[1].Code != 500 {
		t.Fatalf("unexpected status ordering: %#v", payload.StatusCodes)
	}
}
