package logstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestClickHouseOriginTimingAggregateIntegration(t *testing.T) {
	endpoint := os.Getenv("CLICKHOUSE_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("CLICKHOUSE_TEST_ENDPOINT is not set")
	}
	database := fmt.Sprintf("cdn_origin_test_%d", time.Now().UTC().UnixNano())
	clickhouse := ClickHouse{Endpoint: endpoint, Database: database}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_ = clickhouse.queryInDatabase(cleanupCtx, "default", "DROP DATABASE IF EXISTS "+identifier(database), nil)
	})
	if err := clickhouse.queryInDatabase(ctx, "default", "CREATE DATABASE "+identifier(database), nil); err != nil {
		t.Fatal(err)
	}
	legacyTable := `CREATE TABLE ` + identifier(database) + `.cdn_access_logs (
	 request_id String, timestamp DateTime64(3, 'UTC'), node_id String, site_id String, client_ip String, host String, scheme LowCardinality(String), protocol LowCardinality(String), method LowCardinality(String), path String, status UInt16, request_bytes Int64, bytes Int64, duration_ms Int64, upstream String, upstream_status String, upstream_response_time String, cache_status LowCardinality(String), user_agent String, referer String, request_content_type String, response_content_type String, request_accept String, request_range String
	) ENGINE = MergeTree PARTITION BY toDate(timestamp) ORDER BY (site_id, timestamp, node_id) TTL timestamp + INTERVAL 7 DAY DELETE`
	if err := clickhouse.query(ctx, legacyTable, nil); err != nil {
		t.Fatal(err)
	}
	if err := clickhouse.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	minute := time.Now().UTC().Truncate(time.Minute)
	events := []domain.AccessLogEvent{
		{ID: "reused", Timestamp: minute.Add(time.Second), SiteID: "site", NodeID: "node", Status: 200, Bytes: 100, UpstreamConnectTime: "0.000", UpstreamHeaderTime: "0.010", UpstreamResponseTime: "0.020", CacheStatus: "MISS"},
		{ID: "connected", Timestamp: minute.Add(2 * time.Second), SiteID: "site", NodeID: "node", Status: 200, Bytes: 200, UpstreamConnectTime: "0.004", UpstreamHeaderTime: "0.012", UpstreamResponseTime: "0.024", CacheStatus: "MISS"},
		{ID: "cached", Timestamp: minute.Add(3 * time.Second), SiteID: "site", NodeID: "node", Status: 200, Bytes: 300, CacheStatus: "HIT"},
		{ID: "retried", Timestamp: minute.Add(4 * time.Second), SiteID: "site", NodeID: "node", Status: 200, Bytes: 400, UpstreamConnectTime: "0.003 : 0.000", UpstreamHeaderTime: "0.030 : 0.010", UpstreamResponseTime: "0.050 : 0.020", CacheStatus: "MISS"},
	}
	if err := clickhouse.Append(ctx, events); err != nil {
		t.Fatal(err)
	}
	metrics, err := clickhouse.Metrics(ctx, "site", minute.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 1 {
		t.Fatalf("metrics = %#v", metrics)
	}
	metric := metrics[0]
	if metric.Requests != 4 || metric.Bytes != 1000 || metric.CacheHits != 1 ||
		metric.UpstreamSamples != 4 || metric.UpstreamHeaderSamples != 4 || metric.UpstreamResponseSamples != 4 || metric.UpstreamReused != 2 ||
		metric.UpstreamConnectMS != 1.75 || metric.UpstreamHeaderMS != 15.5 || metric.UpstreamResponseMS != 28.5 {
		t.Fatalf("origin timing metric = %#v", metric)
	}
}
