package control

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/logstore"
	"simple_cdn/internal/store"
)

type searchLogStore struct {
	logstore.Noop
	query logstore.LogQuery
	page  logstore.LogPage
	err   error
}

func (s *searchLogStore) Get(_ context.Context, id string) (domain.AccessLogEvent, error) {
	for _, event := range s.page.Events {
		if event.ID == id {
			return event, nil
		}
	}
	return domain.AccessLogEvent{}, logstore.ErrNotFound
}

type appendLogStore struct {
	logstore.Noop
	events []domain.AccessLogEvent
}

func (s *appendLogStore) Append(_ context.Context, events []domain.AccessLogEvent) error {
	s.events = append(s.events, events...)
	return nil
}

func (s *searchLogStore) Search(_ context.Context, query logstore.LogQuery) (logstore.LogPage, error) {
	s.query = query
	return s.page, s.err
}

func TestLogSearchRouteRequiresAdmin(t *testing.T) {
	for _, path := range []string{"/api/logs", "/api/logs/request-1"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		(&Server{}).Handler().ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s: unexpected response status: %d", path, response.Code)
		}
	}
}

func TestLogDetailReturnsEntry(t *testing.T) {
	entry := domain.AccessLogEvent{ID: "request-1", ClientRequestID: "client-1", UpstreamRequestID: "origin-1", Method: "GET", Path: "/long/path", UserAgent: "test-agent", RequestCompletion: "OK"}
	logs := &searchLogStore{page: logstore.LogPage{Events: []domain.AccessLogEvent{entry}}}
	request := httptest.NewRequest(http.MethodGet, "/api/logs/request-1", nil)
	request.SetPathValue("id", "request-1")
	response := httptest.NewRecorder()
	(&Server{Logs: logs}).getLog(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"user_agent":"test-agent"`) || !strings.Contains(response.Body.String(), `"upstream_request_id":"origin-1"`) || !strings.Contains(response.Body.String(), `"request_completion":"OK"`) {
		t.Fatalf("unexpected detail response: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestLogSearchParsesFiltersAndReturnsPage(t *testing.T) {
	from := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	store := &searchLogStore{page: logstore.LogPage{
		Events:  []domain.AccessLogEvent{{Timestamp: to.Add(-time.Minute), SiteID: "site", Status: 404}},
		HasMore: true,
	}}
	values := url.Values{
		"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)},
		"site_id": {"site"}, "node_id": {"node"}, "method": {"get"}, "status": {"4xx"},
		"request_id": {"trace.id:123"}, "path": {"/API"}, "client_ip": {"2001:0db8::1"}, "cache_status": {"miss"}, "offset": {"100"},
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/logs?"+values.Encode(), nil)
	(&Server{Logs: store}).searchLogs(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected response status: %d, body: %s", response.Code, response.Body.String())
	}
	if store.query.From != from || store.query.To != to || store.query.RequestID != "trace.id:123" || store.query.SiteID != "site" || store.query.NodeID != "node" || store.query.Method != "GET" || store.query.StatusMin != 400 || store.query.StatusMax != 499 || store.query.Path != "/API" || store.query.ClientIP != "2001:db8::1" || store.query.CacheStatus != "MISS" || store.query.Offset != 100 || store.query.Limit != logSearchPageSize {
		t.Fatalf("unexpected query: %#v", store.query)
	}
	var payload logSearchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Logs) != 1 || payload.Logs[0].Status != 404 || !payload.HasMore || payload.Offset != 100 || payload.PageSize != logSearchPageSize || payload.From != from || payload.To != to {
		t.Fatalf("unexpected response: %#v", payload)
	}
}

func TestLogSearchDefaultsToPreviousHour(t *testing.T) {
	now := time.Date(2026, 7, 15, 8, 9, 10, 0, time.FixedZone("test", 8*60*60))
	query, err := parseLogSearchQuery(httptest.NewRequest(http.MethodGet, "/api/logs", nil), now)
	if err != nil {
		t.Fatal(err)
	}
	if query.To != now.UTC() || query.From != now.UTC().Add(-time.Hour) || query.Offset != 0 || query.Limit != logSearchPageSize {
		t.Fatalf("unexpected defaults: %#v", query)
	}
}

func TestLogSearchRejectsInvalidFilters(t *testing.T) {
	tests := map[string]string{
		"invalid from":   "from=nope",
		"reversed time":  "from=2026-07-15T02%3A00%3A00Z&to=2026-07-15T01%3A00%3A00Z",
		"range too long": "from=2026-07-01T00%3A00%3A00Z&to=2026-07-15T00%3A00%3A00Z",
		"invalid status": "status=700",
		"invalid IP":     "client_ip=not-an-ip",
		"invalid offset": "offset=-1",
		"invalid method": "method=GET%20POST",
		"invalid cache":  "cache_status=UNKNOWN",
		"invalid trace":  "request_id=bad%20id",
	}
	for name, query := range tests {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/api/logs?"+query, nil)
			(&Server{}).searchLogs(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("unexpected response: code=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestLogSearchReturnsEmptyPageWithoutLogStore(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/logs?from=2026-07-15T00%3A00%3A00Z&to=2026-07-15T01%3A00%3A00Z", nil)
	(&Server{}).searchLogs(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"logs":[]`) {
		t.Fatalf("unexpected response: code=%d body=%s", response.Code, response.Body.String())
	}
}

func TestLogSearchMapsStoreFailureToBadGateway(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/logs?from=2026-07-15T00%3A00%3A00Z&to=2026-07-15T01%3A00%3A00Z", nil)
	(&Server{Logs: &searchLogStore{err: errors.New("clickhouse unavailable")}}).searchLogs(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("unexpected response: code=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEdgeLogsAcceptPublishedAssignmentWhileDraftMoveIsPending(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	oldNode, err := database.CreateNode("edge-old", "203.0.113.70")
	if err != nil {
		t.Fatal(err)
	}
	newNode, err := database.CreateNode("edge-new", "203.0.113.71")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "logs", Domains: []string{"logs.example.test"}, Nodes: []string{oldNode.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.MarkSitePublished(site.ID); err != nil {
		t.Fatal(err)
	}
	draft, zoneID, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	draft.Nodes = []string{newNode.ID}
	if _, err := database.UpdateSite(draft, zoneID); err != nil {
		t.Fatal(err)
	}

	logs := &appendLogStore{}
	server := &Server{Store: database, Logs: logs}
	body, err := json.Marshal([]domain.AccessLogEvent{{
		ID: "invalid/id", SiteID: site.ID, Method: http.MethodGet, Path: "/asset.js?token=secret",
		Status: 200, RequestBytes: 512, Bytes: 1024, UserAgent: strings.Repeat("x", 5000),
	}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/edge/v1/logs", bytes.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), edgeContextKey{}, oldNode.ID))
	response := httptest.NewRecorder()
	server.writeLogs(response, request)
	if response.Code != http.StatusAccepted || len(logs.events) != 1 || logs.events[0].NodeID != oldNode.ID || logs.events[0].SiteID != site.ID {
		t.Fatalf("published-assignment logs were rejected: status=%d events=%#v body=%s", response.Code, logs.events, response.Body.String())
	}
	if !validLogEntryID(logs.events[0].ID) || logs.events[0].ID == "invalid/id" || logs.events[0].Path != "/asset.js" || len([]rune(logs.events[0].UserAgent)) != 4096 {
		t.Fatalf("uploaded log was not normalized: %#v", logs.events[0])
	}
}

func TestEdgeLogsAcceptGzipAndRejectUnknownEncoding(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("gzip-edge", "203.0.113.72")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "gzip-logs", Domains: []string{"gzip.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	logs := &appendLogStore{}
	server := &Server{Store: database, Logs: logs}
	payload, err := json.Marshal([]domain.AccessLogEvent{{ID: "gzip-request", SiteID: site.ID, Path: "/asset.js"}})
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/edge/v1/logs", bytes.NewReader(compressed.Bytes()))
	request.Header.Set("Content-Encoding", "gzip")
	request = request.WithContext(context.WithValue(request.Context(), edgeContextKey{}, node.ID))
	response := httptest.NewRecorder()
	server.writeLogs(response, request)
	if response.Code != http.StatusAccepted || len(logs.events) != 1 || logs.events[0].ID != "gzip-request" {
		t.Fatalf("gzip response status=%d events=%#v body=%s", response.Code, logs.events, response.Body.String())
	}

	unsupported := httptest.NewRequest(http.MethodPost, "/api/edge/v1/logs", bytes.NewReader(payload))
	unsupported.Header.Set("Content-Encoding", "br")
	unsupported = unsupported.WithContext(context.WithValue(unsupported.Context(), edgeContextKey{}, node.ID))
	unsupportedResponse := httptest.NewRecorder()
	server.writeLogs(unsupportedResponse, unsupported)
	if unsupportedResponse.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("unsupported encoding status=%d body=%s", unsupportedResponse.Code, unsupportedResponse.Body.String())
	}
}
