package logstore

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestClickHouseMonitoringHistoryWritesAndAggregatesSamples(t *testing.T) {
	checkedAt := time.Date(2026, 7, 20, 10, 11, 12, 345000000, time.UTC)
	var inserted string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		query := request.URL.Query().Get("query")
		if strings.HasPrefix(query, "INSERT INTO") {
			contents, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			inserted = string(contents)
			return
		}
		for _, expected := range []string{
			"cdn_tcp_monitoring_history FINAL", "INTERVAL 120 SECOND",
			"node_id = {node_id:String}", "GROUP BY bucket, node_id, target_id",
		} {
			if !strings.Contains(query, expected) {
				t.Fatalf("history query is missing %q: %s", expected, query)
			}
		}
		if request.URL.Query().Get("param_node_id") != "node-1" {
			t.Fatalf("history node parameter = %q", request.URL.Query().Get("param_node_id"))
		}
		_, _ = io.WriteString(response, `{"bucket":"2026-07-20 10:10:00","node_id":"node-1","node_name":"香港边缘","target_id":"target-1","target_name":"主 API","target_address":"api.example.test:443","total_attempts":"12","total_successful_attempts":"9","average_latency_ms":28.5,"failed_rounds":"1"}`+"\n")
	}))
	defer server.Close()

	clickhouse := ClickHouse{Endpoint: server.URL}
	samples := []MonitoringSample{{
		NodeID: "node-1", NodeName: "香港边缘", TargetID: "target-1", TargetName: "主 API", TargetAddress: "api.example.test:443",
		Attempts: 3, SuccessfulAttempts: 2, AverageLatencyMS: 31.25, Error: "timeout", CheckedAt: checkedAt,
	}}
	if err := clickhouse.AppendMonitoring(context.Background(), samples); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"node_id":"node-1"`, `"target_name":"主 API"`, `"checked_at":"2026-07-20 10:11:12.345"`} {
		if !strings.Contains(inserted, expected) {
			t.Fatalf("monitoring insert is missing %s: %s", expected, inserted)
		}
	}
	buckets, err := clickhouse.MonitoringHistory(context.Background(), MonitoringHistoryQuery{
		NodeID: "node-1", From: checkedAt.Add(-time.Hour), To: checkedAt.Add(time.Minute), Bucket: 2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 || buckets[0].TargetName != "主 API" || buckets[0].Attempts != 12 || buckets[0].SuccessfulAttempts != 9 || buckets[0].AverageLatencyMS == nil || *buckets[0].AverageLatencyMS != 28.5 || buckets[0].FailedRounds != 1 {
		t.Fatalf("monitoring history buckets = %#v", buckets)
	}
}

func TestEnsureSchemaCreatesSevenDayMonitoringHistoryTable(t *testing.T) {
	var statements []string
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		statements = append(statements, request.URL.Query().Get("query"))
	}))
	defer server.Close()
	if err := (ClickHouse{Endpoint: server.URL}).EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(statements, "\n")
	for _, expected := range []string{"cdn_tcp_monitoring_history", "ReplacingMergeTree(ingested_at)", "TTL checked_at + INTERVAL 7 DAY DELETE"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("monitoring schema is missing %q", expected)
		}
	}
}

func TestClickHouseMonitoringHistoryIntegration(t *testing.T) {
	endpoint := strings.TrimSpace(os.Getenv("CLICKHOUSE_TEST_URL"))
	if endpoint == "" {
		t.Skip("CLICKHOUSE_TEST_URL is not configured")
	}
	database := fmt.Sprintf("cdn_monitoring_test_%d", time.Now().UnixNano())
	clickhouse := ClickHouse{Endpoint: endpoint, Database: database}
	t.Cleanup(func() {
		_ = clickhouse.queryInDatabase(context.Background(), "default", `DROP DATABASE IF EXISTS `+identifier(database), nil)
	})
	if err := clickhouse.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	if err := clickhouse.AppendMonitoring(context.Background(), []MonitoringSample{
		{
			NodeID: "node-1", NodeName: "香港边缘", TargetID: "target-1", TargetName: "主 API", TargetAddress: "api.example.test:443",
			Attempts: 3, SuccessfulAttempts: 3, AverageLatencyMS: 20, CheckedAt: checkedAt,
		},
		{
			NodeID: "node-1", NodeName: "香港边缘", TargetID: "target-1", TargetName: "主 API", TargetAddress: "api.example.test:443",
			Attempts: 3, SuccessfulAttempts: 2, AverageLatencyMS: 30, Error: "timeout", CheckedAt: checkedAt.Add(time.Second),
		},
	}); err != nil {
		t.Fatal(err)
	}
	buckets, err := clickhouse.MonitoringHistory(context.Background(), MonitoringHistoryQuery{
		NodeID: "node-1", From: checkedAt.Add(-time.Minute), To: checkedAt.Add(time.Minute), Bucket: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 {
		t.Fatalf("monitoring history buckets = %#v", buckets)
	}
	bucket := buckets[0]
	if bucket.Attempts != 6 || bucket.SuccessfulAttempts != 5 || bucket.FailedRounds != 1 || bucket.AverageLatencyMS == nil || math.Abs(*bucket.AverageLatencyMS-24) > 0.001 {
		t.Fatalf("monitoring history bucket = %#v", bucket)
	}
}

type recordingMonitoringHistoryStore struct {
	appended chan []MonitoringSample
}

func (s *recordingMonitoringHistoryStore) AppendMonitoring(_ context.Context, samples []MonitoringSample) error {
	s.appended <- append([]MonitoringSample(nil), samples...)
	return nil
}

func (*recordingMonitoringHistoryStore) MonitoringHistory(context.Context, MonitoringHistoryQuery) ([]MonitoringHistoryBucket, error) {
	return nil, nil
}

func TestAsyncMonitoringWriterQueuesWithoutBlockingReporter(t *testing.T) {
	store := &recordingMonitoringHistoryStore{appended: make(chan []MonitoringSample, 1)}
	writer := newAsyncMonitoringWriter(store, nil, 1)
	writer.flushInterval = 20 * time.Millisecond
	if !writer.EnqueueMonitoring([]MonitoringSample{{NodeID: "node-1", TargetID: "target-1"}}) {
		t.Fatal("first monitoring report was not queued")
	}
	if writer.EnqueueMonitoring([]MonitoringSample{{NodeID: "node-2", TargetID: "target-2"}}) {
		t.Fatal("full monitoring queue accepted another report")
	}
	ctx, cancel := context.WithCancel(context.Background())
	writer.Start(ctx)
	select {
	case samples := <-store.appended:
		if len(samples) != 1 || samples[0].NodeID != "node-1" {
			t.Fatalf("appended monitoring samples = %#v", samples)
		}
	case <-time.After(time.Second):
		t.Fatal("monitoring history writer did not flush queued samples")
	}
	cancel()
	writer.Wait()
}

func TestAsyncMonitoringWriterBatchesReportsUntilDeadline(t *testing.T) {
	store := &recordingMonitoringHistoryStore{appended: make(chan []MonitoringSample, 4)}
	writer := NewAsyncMonitoringWriter(store, nil)
	writer.flushInterval = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); writer.Wait() }()
	writer.Start(ctx)
	writer.EnqueueMonitoring([]MonitoringSample{{NodeID: "node-1"}})
	select {
	case <-store.appended:
		t.Fatal("a small report was written before the batch window elapsed")
	case <-time.After(20 * time.Millisecond):
	}
	writer.EnqueueMonitoring([]MonitoringSample{{NodeID: "node-2"}})
	select {
	case samples := <-store.appended:
		if len(samples) != 2 || samples[0].NodeID != "node-1" || samples[1].NodeID != "node-2" {
			t.Fatalf("batched reports = %#v", samples)
		}
	case <-time.After(time.Second):
		t.Fatal("a small batch was not flushed by its deadline")
	}
}

func TestAsyncMonitoringWriterFlushesFullBatchWithoutWaiting(t *testing.T) {
	store := &recordingMonitoringHistoryStore{appended: make(chan []MonitoringSample, 1)}
	writer := NewAsyncMonitoringWriter(store, nil)
	writer.flushInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); writer.Wait() }()
	writer.Start(ctx)
	writer.EnqueueMonitoring(make([]MonitoringSample, monitoringHistoryBatchSize))
	select {
	case samples := <-store.appended:
		if len(samples) != monitoringHistoryBatchSize {
			t.Fatalf("full batch size = %d", len(samples))
		}
	case <-time.After(time.Second):
		t.Fatal("a full batch waited for the flush timer")
	}
}

func TestAsyncMonitoringWriterFlushesPendingBatchOnShutdown(t *testing.T) {
	store := &recordingMonitoringHistoryStore{appended: make(chan []MonitoringSample, 1)}
	writer := NewAsyncMonitoringWriter(store, nil)
	writer.flushInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	writer.Start(ctx)
	writer.EnqueueMonitoring([]MonitoringSample{{NodeID: "node-1"}})
	cancel()
	done := make(chan struct{})
	go func() { writer.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown waited for the batch window")
	}
	select {
	case samples := <-store.appended:
		if len(samples) != 1 || samples[0].NodeID != "node-1" {
			t.Fatalf("shutdown batch = %#v", samples)
		}
	default:
		t.Fatal("shutdown lost the queued report")
	}
}
