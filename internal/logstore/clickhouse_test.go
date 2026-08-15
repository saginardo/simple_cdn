package logstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestRecentDecodesJSONEachRow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !strings.Contains(request.URL.Query().Get("query"), "FORMAT JSONEachRow") {
			t.Fatalf("unexpected query: %s", request.URL.Query().Get("query"))
		}
		_, _ = io.WriteString(response, "{\"timestamp\":\"2026-01-02T03:04:05Z\",\"node_id\":\"node\",\"site_id\":\"site\",\"client_ip\":\"203.0.113.5\",\"method\":\"GET\",\"path\":\"/a\",\"status\":200,\"bytes\":10,\"duration_ms\":2,\"upstream\":\"origin\",\"cache_status\":\"HIT\"}\n")
	}))
	defer server.Close()
	clickhouse := ClickHouse{Endpoint: server.URL}
	events, err := clickhouse.Recent(context.Background(), "site", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Path != "/a" || events[0].CacheStatus != "HIT" {
		t.Fatalf("unexpected events: %#v", events)
	}
}

func TestGetReturnsExtendedRequestDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		query := request.URL.Query().Get("query")
		if !strings.Contains(query, "WHERE request_id = {request_id:String}") || request.URL.Query().Get("param_request_id") != "request-1" {
			t.Fatalf("unexpected detail query: %s", request.URL.RawQuery)
		}
		_, _ = io.WriteString(response, `{"request_id":"request-1","client_request_id":"client-1","upstream_request_id":"origin-1","timestamp":"2026-07-18 10:20:30.123","node_id":"node-1","site_id":"site-1","client_ip":"203.0.113.5","host":"cdn.example.test","scheme":"https","protocol":"HTTP/2.0","method":"GET","path":"/asset.js","status":404,"request_bytes":512,"bytes":2048,"duration_ms":37,"request_completion":"OK","upstream":"192.0.2.10:443","upstream_status":"404","upstream_connect_time":"0.004","upstream_header_time":"0.020","upstream_response_time":"0.036","upstream_bytes_sent":"640","upstream_bytes_received":"2304","cache_status":"MISS","user_agent":"test-agent","referer":"https://example.test/","request_content_type":"application/json","response_content_type":"text/javascript","content_encoding":"zstd","compression_ratio":2.25,"compression_saved_bytes":2560,"request_accept":"*/*","request_range":"bytes=0-1023"}`+"\n")
	}))
	defer server.Close()

	event, err := (ClickHouse{Endpoint: server.URL}).Get(context.Background(), "request-1")
	if err != nil {
		t.Fatal(err)
	}
	if event.ID != "request-1" || event.RequestBytes != 512 || event.Bytes != 2048 || event.UserAgent != "test-agent" || event.ResponseContentType != "text/javascript" || event.Range != "bytes=0-1023" {
		t.Fatalf("unexpected detail event: %#v", event)
	}
	if event.UpstreamConnectTime != "0.004" || event.UpstreamHeaderTime != "0.020" || event.UpstreamResponseTime != "0.036" {
		t.Fatalf("unexpected timing details: %#v", event)
	}
	if event.ClientRequestID != "client-1" || event.UpstreamRequestID != "origin-1" || event.RequestCompletion != "OK" || event.UpstreamBytesSent != "640" || event.UpstreamBytesReceived != "2304" {
		t.Fatalf("unexpected trace details: %#v", event)
	}
	if event.ContentEncoding != "zstd" || event.CompressionRatio != 2.25 || event.CompressionSavedBytes != 2560 {
		t.Fatalf("unexpected compression details: %#v", event)
	}
}

func TestGetReturnsNotFoundForEmptyResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	_, err := (ClickHouse{Endpoint: server.URL}).Get(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing detail error = %v", err)
	}
}

func TestSearchAppliesFiltersAndReportsMoreRows(t *testing.T) {
	from := time.Date(2026, 7, 15, 1, 2, 3, 4000000, time.UTC)
	to := from.Add(time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		query := request.URL.Query().Get("query")
		for _, expected := range []string{
			"PREWHERE timestamp >= {from:DateTime64(3)} AND timestamp < {to:DateTime64(3)}",
			"request_id = {request_id:String}", "client_request_id = {request_id:String}",
			"upstream_request_id = {request_id:String}", "has(upstream_request_ids, {request_id:String})",
			"splitByRegexp('[,:]', upstream_request_id)",
			"site_id = {site_id:String}", "node_id = {node_id:String}", "method = {method:String}",
			"status >= {status_min:UInt16}", "status <= {status_max:UInt16}",
			"positionCaseInsensitive(path, {path:String}) > 0", "client_ip = {client_ip:String}",
			"cache_status = {cache_status:String}", "LIMIT 3 OFFSET 100",
		} {
			if !strings.Contains(query, expected) {
				t.Fatalf("query does not contain %q: %s", expected, query)
			}
		}
		parameters := request.URL.Query()
		expectedParameters := map[string]string{
			"param_from": "2026-07-15 01:02:03.004", "param_to": "2026-07-15 02:02:03.004",
			"param_request_id": "trace-1",
			"param_site_id":    "site", "param_node_id": "node", "param_method": "GET",
			"param_status_min": "400", "param_status_max": "499", "param_path": "/api",
			"param_client_ip": "203.0.113.5", "param_cache_status": "MISS",
		}
		for key, expected := range expectedParameters {
			if got := parameters.Get(key); got != expected {
				t.Fatalf("unexpected %s: got %q, want %q", key, got, expected)
			}
		}
		for index := 0; index < 3; index++ {
			_, _ = io.WriteString(response, fmt.Sprintf("{\"timestamp\":\"2026-07-15T01:02:0%dZ\",\"node_id\":\"node\",\"site_id\":\"site\",\"client_ip\":\"203.0.113.5\",\"method\":\"GET\",\"path\":\"/api\",\"status\":404,\"bytes\":10,\"duration_ms\":2,\"upstream\":\"origin\",\"cache_status\":\"MISS\"}\n", index))
		}
	}))
	defer server.Close()

	page, err := (ClickHouse{Endpoint: server.URL}).Search(context.Background(), LogQuery{
		From: from, To: to, RequestID: "trace-1", SiteID: "site", NodeID: "node", Method: "GET",
		StatusMin: 400, StatusMax: 499, Path: "/api", ClientIP: "203.0.113.5",
		CacheStatus: "MISS", Offset: 100, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || !page.HasMore {
		t.Fatalf("unexpected page: %#v", page)
	}
}

func TestSearchUsesDefaultsAndNeverEmitsNegativeOffset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		query := request.URL.Query().Get("query")
		if !strings.Contains(query, "LIMIT 101 OFFSET 0") {
			t.Fatalf("unexpected query: %s", query)
		}
	}))
	defer server.Close()
	page, err := (ClickHouse{Endpoint: server.URL}).Search(context.Background(), LogQuery{Offset: -1})
	if err != nil {
		t.Fatal(err)
	}
	if page.Events == nil || len(page.Events) != 0 || page.HasMore {
		t.Fatalf("unexpected empty page: %#v", page)
	}
}

func TestMetricsDecodesJSONEachRow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		query := request.URL.Query().Get("query")
		for _, expected := range []string{"cdn_site_minute", "cdn_origin_minute", "cdn_compression_minute", "LEFT JOIN", "sum(connect_samples)", "upstream_connect_ms", "compressed_requests", "compression_saved_bytes"} {
			if !strings.Contains(query, expected) {
				t.Fatalf("metrics query does not contain %q: %s", expected, query)
			}
		}
		_, _ = io.WriteString(response, "{\"minute\":\"2026-01-02T03:04:00Z\",\"requests\":12,\"bytes\":1200,\"errors\":1,\"cache_hits\":9,\"upstream_samples\":10,\"upstream_header_samples\":9,\"upstream_response_samples\":8,\"upstream_reused\":8,\"upstream_connect_ms\":1.5,\"upstream_header_ms\":12.5,\"upstream_response_ms\":25.5,\"compressed_requests\":7,\"gzip_requests\":2,\"brotli_requests\":3,\"zstd_requests\":2,\"compression_saved_bytes\":4096}\n")
	}))
	defer server.Close()
	metrics, err := (ClickHouse{Endpoint: server.URL}).Metrics(context.Background(), "site", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 1 || metrics[0].Requests != 12 || metrics[0].CacheHits != 9 || metrics[0].UpstreamSamples != 10 ||
		metrics[0].UpstreamHeaderSamples != 9 || metrics[0].UpstreamResponseSamples != 8 || metrics[0].UpstreamReused != 8 ||
		metrics[0].UpstreamConnectMS != 1.5 || metrics[0].UpstreamHeaderMS != 12.5 || metrics[0].UpstreamResponseMS != 25.5 ||
		metrics[0].CompressedRequests != 7 || metrics[0].GzipRequests != 2 || metrics[0].BrotliRequests != 3 || metrics[0].ZstdRequests != 2 || metrics[0].CompressionSavedBytes != 4096 {
		t.Fatalf("unexpected metrics: %#v", metrics)
	}
}

func TestEnsureSchemaCreatesOriginTimingAggregate(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		queries = append(queries, request.URL.Query().Get("query"))
	}))
	defer server.Close()
	if err := (ClickHouse{Endpoint: server.URL}).EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(queries, "\n")
	for _, expected := range []string{
		"cdn_origin_minute", "cdn_access_to_origin_minute", "upstream_connect_time",
		"connect_samples UInt64", "upstream_connect_ms Array(Float64)", "sum(length(upstream_connect_ms))", "sum(arraySum(upstream_connect_ms)) / 1000",
		"cdn_status_minute", "cdn_access_to_status_minute", "downstream_bytes Int64", "upstream_bytes Int64",
		"cdn_cache_minute", "cdn_access_to_cache_minute",
		"cdn_aggregation_migrations", "status_minute_v1", "cache_minute_v1", "INTERVAL 7 DAY",
		"upstream_request_ids Array(String)",
		"client_request_id String", "upstream_request_id String",
		"request_completion LowCardinality(String) DEFAULT 'UNKNOWN'",
		"upstream_bytes_sent String", "upstream_bytes_received String",
		"content_encoding LowCardinality(String)", "compression_ratio Float64", "compression_saved_bytes Int64",
		"cdn_compression_minute", "cdn_access_to_compression_minute", "countIf(content_encoding = 'br')",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("origin timing schema is missing %q:\n%s", expected, joined)
		}
	}
}

func TestOverviewDecodesHourlyStatusRows(t *testing.T) {
	from := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		query := request.URL.Query().Get("query")
		if !strings.Contains(query, "toStartOfHour(minute)") || !strings.Contains(query, "GROUP BY hour, site_id, status") ||
			!strings.Contains(query, "sum(downstream_bytes) AS downstream_bytes") || !strings.Contains(query, "sum(upstream_bytes) AS upstream_bytes") ||
			!strings.Contains(query, "cdn_status_minute") {
			t.Fatalf("unexpected query: %s", query)
		}
		if request.URL.Query().Get("param_from") != "2026-01-02 03:04:05" || request.URL.Query().Get("param_to") != "2026-01-03 03:04:05" {
			t.Fatalf("unexpected time parameters: %s", request.URL.RawQuery)
		}
		_, _ = io.WriteString(response, "{\"hour\":\"2026-01-02T04:00:00Z\",\"site_id\":\"site\",\"status\":404,\"requests\":\"7\",\"downstream_bytes\":\"700\",\"upstream_bytes\":\"210\"}\n")
	}))
	defer server.Close()
	buckets, err := (ClickHouse{Endpoint: server.URL}).Overview(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 || buckets[0].SiteID != "site" || buckets[0].Status != 404 || buckets[0].Requests != 7 ||
		buckets[0].DownstreamBytes != 700 || buckets[0].UpstreamBytes != 210 {
		t.Fatalf("unexpected overview buckets: %#v", buckets)
	}
}

func TestNodeCacheAggregatesStatusRowsForRequestedWindow(t *testing.T) {
	from := time.Date(2026, 7, 16, 2, 3, 4, 5000000, time.UTC)
	to := from.Add(24 * time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		query := request.URL.Query().Get("query")
		for _, expected := range []string{
			"upper(cache_status)", "node_id = {node_id:String}", "GROUP BY cache_status",
			"minute >= {from:DateTime}", "minute < {to:DateTime}",
			"cdn_cache_minute", "max(minute) AS last_seen_at",
		} {
			if !strings.Contains(query, expected) {
				t.Fatalf("query does not contain %q: %s", expected, query)
			}
		}
		parameters := request.URL.Query()
		if parameters.Get("param_node_id") != "node-1" || parameters.Get("param_from") != "2026-07-16 02:03:04" || parameters.Get("param_to") != "2026-07-17 02:03:04" {
			t.Fatalf("unexpected parameters: %s", request.URL.RawQuery)
		}
		_, _ = io.WriteString(response, "{\"cache_status\":\"HIT\",\"requests\":\"12\",\"bytes\":\"1200\",\"last_seen_at\":\"2026-07-17 01:02:00\"}\n")
	}))
	defer server.Close()

	buckets, err := (ClickHouse{Endpoint: server.URL}).NodeCache(context.Background(), "node-1", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 || buckets[0].Status != "HIT" || buckets[0].Requests != 12 || buckets[0].Bytes != 1200 ||
		buckets[0].LastSeenAt.Minute() != 2 || buckets[0].LastSeenAt.Second() != 0 {
		t.Fatalf("unexpected node cache buckets: %#v", buckets)
	}
}

func TestClickHouseTimeDecodesNativeDateTimeFormat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(response, "{\"timestamp\":\"2026-01-02 03:04:05.123\",\"node_id\":\"node\",\"site_id\":\"site\",\"client_ip\":\"203.0.113.5\",\"method\":\"GET\",\"path\":\"/a\",\"status\":200,\"bytes\":10,\"duration_ms\":2,\"upstream\":\"origin\",\"cache_status\":\"HIT\"}\n")
	}))
	defer server.Close()
	events, err := (ClickHouse{Endpoint: server.URL}).Recent(context.Background(), "site", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Timestamp.Location() != time.UTC || events[0].Timestamp.Nanosecond() != 123000000 {
		t.Fatalf("unexpected decoded event: %#v", events)
	}
}

func TestEnsureSchemaCreatesDatabaseOutsideTargetDatabase(t *testing.T) {
	var databases []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		databases = append(databases, request.URL.Query().Get("database"))
	}))
	defer server.Close()
	if err := (ClickHouse{Endpoint: server.URL, Database: "new_database"}).EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(databases) < 2 || databases[0] != "default" || databases[1] != "new_database" {
		t.Fatalf("unexpected schema databases: %#v", databases)
	}
}

func TestRequestOmitsBasicAuthWhenCredentialsAreUnset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if _, _, ok := request.BasicAuth(); ok {
			t.Fatal("unexpected basic authentication header")
		}
	}))
	defer server.Close()

	if err := (ClickHouse{Endpoint: server.URL}).EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAppendUsesClickHouseDateTimeFormat(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		contents, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		body = string(contents)
	}))
	defer server.Close()
	err := (ClickHouse{Endpoint: server.URL}).Append(context.Background(), []domain.AccessLogEvent{{
		ID: "request-1", Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC), SiteID: "site", NodeID: "node",
		ClientRequestID: "client-1", UpstreamRequestID: "origin-1", RequestCompletion: "OK",
		RequestBytes: 512, UpstreamBytesSent: "640", UpstreamBytesReceived: "2304",
		UpstreamConnectTime: "0.004", UpstreamHeaderTime: "0.020", UpstreamResponseTime: "0.036",
		UserAgent: "test-agent", ContentType: "application/json", Range: "bytes=0-10",
		ContentEncoding: "br", CompressionRatio: 2.75, CompressionSavedBytes: 896,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"timestamp":"2026-01-02 03:04:05.123"`) {
		t.Fatalf("unexpected insert body: %s", body)
	}
	for _, expected := range []string{`"request_id":"request-1"`, `"client_request_id":"client-1"`, `"upstream_request_id":"origin-1"`, `"upstream_request_ids":["origin-1"]`, `"upstream_connect_ms":[4]`, `"upstream_header_ms":[20]`, `"upstream_response_ms":[36]`, `"request_completion":"OK"`, `"request_bytes":512`, `"upstream_bytes_sent":"640"`, `"upstream_bytes_received":"2304"`, `"user_agent":"test-agent"`, `"request_content_type":"application/json"`, `"content_encoding":"br"`, `"compression_ratio":2.75`, `"compression_saved_bytes":896`, `"request_range":"bytes=0-10"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("insert body is missing %s: %s", expected, body)
		}
	}
}

func TestAppendMarksLegacyCompletionAsUnknown(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		contents, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		body = string(contents)
	}))
	defer server.Close()
	if err := (ClickHouse{Endpoint: server.URL}).Append(context.Background(), []domain.AccessLogEvent{{ID: "legacy"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"request_completion":"UNKNOWN"`) {
		t.Fatalf("legacy insert body does not mark completion unknown: %s", body)
	}
}
