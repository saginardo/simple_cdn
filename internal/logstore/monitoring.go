package logstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	MonitoringHistoryRetention     = 7 * 24 * time.Hour
	monitoringHistoryBatchSize     = 512
	monitoringHistoryQueueSize     = 256
	monitoringHistoryFlushInterval = 5 * time.Second
)

type MonitoringSample struct {
	NodeID             string
	NodeName           string
	TargetID           string
	TargetName         string
	TargetAddress      string
	Attempts           int
	SuccessfulAttempts int
	AverageLatencyMS   float64
	Error              string
	CheckedAt          time.Time
}

type MonitoringHistoryQuery struct {
	NodeID string
	From   time.Time
	To     time.Time
	Bucket time.Duration
}

type MonitoringHistoryBucket struct {
	Time               time.Time `json:"time"`
	NodeID             string    `json:"node_id"`
	NodeName           string    `json:"node_name"`
	TargetID           string    `json:"target_id"`
	TargetName         string    `json:"target_name"`
	TargetAddress      string    `json:"target_address"`
	Attempts           uint64    `json:"attempts"`
	SuccessfulAttempts uint64    `json:"successful_attempts"`
	AverageLatencyMS   *float64  `json:"average_latency_ms"`
	FailedRounds       uint64    `json:"failed_rounds"`
}

type MonitoringHistoryReader interface {
	MonitoringHistory(context.Context, MonitoringHistoryQuery) ([]MonitoringHistoryBucket, error)
}

type MonitoringHistoryStore interface {
	MonitoringHistoryReader
	AppendMonitoring(context.Context, []MonitoringSample) error
}

type MonitoringHistoryEnqueuer interface {
	EnqueueMonitoring([]MonitoringSample) bool
}

func monitoringHistoryTableStatement(database string) string {
	return `CREATE TABLE IF NOT EXISTS ` + identifier(database) + `.cdn_tcp_monitoring_history (
	 node_id String, node_name String, target_id String, target_name String, target_address String,
	 attempts UInt8, successful_attempts UInt8, average_latency_ms Float64, error String,
	 checked_at DateTime64(3, 'UTC'), ingested_at DateTime64(3, 'UTC')
	) ENGINE = ReplacingMergeTree(ingested_at)
	PARTITION BY toDate(checked_at)
	ORDER BY (node_id, target_id, checked_at)
	TTL checked_at + INTERVAL 7 DAY DELETE`
}

func (c ClickHouse) AppendMonitoring(ctx context.Context, samples []MonitoringSample) error {
	if len(samples) == 0 {
		return nil
	}
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	ingestedAt := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	for _, sample := range samples {
		row := monitoringSampleInsert{
			NodeID: sample.NodeID, NodeName: sample.NodeName,
			TargetID: sample.TargetID, TargetName: sample.TargetName, TargetAddress: sample.TargetAddress,
			Attempts: sample.Attempts, SuccessfulAttempts: sample.SuccessfulAttempts,
			AverageLatencyMS: sample.AverageLatencyMS, Error: sample.Error,
			CheckedAt: sample.CheckedAt.UTC().Format("2006-01-02 15:04:05.000"), IngestedAt: ingestedAt,
		}
		if err := encoder.Encode(row); err != nil {
			return err
		}
	}
	return c.query(ctx, "INSERT INTO "+identifier(c.database())+".cdn_tcp_monitoring_history FORMAT JSONEachRow", &body)
}

func (c ClickHouse) MonitoringHistory(ctx context.Context, query MonitoringHistoryQuery) ([]MonitoringHistoryBucket, error) {
	if strings.TrimSpace(query.NodeID) == "" || query.From.IsZero() || query.To.IsZero() || !query.From.Before(query.To) {
		return nil, errors.New("invalid monitoring history query")
	}
	if query.To.Sub(query.From) > MonitoringHistoryRetention || query.Bucket < time.Second || query.Bucket > 24*time.Hour || query.Bucket%time.Second != 0 {
		return nil, errors.New("invalid monitoring history range")
	}
	bucketSeconds := int64(query.Bucket / time.Second)
	statement := `SELECT
		toStartOfInterval(checked_at, INTERVAL ` + strconv.FormatInt(bucketSeconds, 10) + ` SECOND) AS bucket,
		node_id, argMax(node_name, checked_at) AS node_name,
		target_id, argMax(target_name, checked_at) AS target_name, argMax(target_address, checked_at) AS target_address,
		sum(attempts) AS total_attempts, sum(successful_attempts) AS total_successful_attempts,
		if(sum(successful_attempts) = 0, NULL, sum(average_latency_ms * successful_attempts) / sum(successful_attempts)) AS average_latency_ms,
		countIf(successful_attempts < attempts) AS failed_rounds
	FROM ` + identifier(c.database()) + `.cdn_tcp_monitoring_history FINAL
	PREWHERE node_id = {node_id:String} AND checked_at >= {from:DateTime64(3)} AND checked_at < {to:DateTime64(3)}
	GROUP BY bucket, node_id, target_id
	ORDER BY bucket, target_id FORMAT JSONEachRow`
	parameters := url.Values{
		"param_node_id": {query.NodeID},
		"param_from":    {query.From.UTC().Format("2006-01-02 15:04:05.000")},
		"param_to":      {query.To.UTC().Format("2006-01-02 15:04:05.000")},
	}
	response, err := c.request(ctx, c.database(), statement, nil, parameters)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	buckets := make([]MonitoringHistoryBucket, 0)
	decoder := json.NewDecoder(response.Body)
	for {
		var row monitoringHistoryRow
		if err := decoder.Decode(&row); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		bucket, err := row.bucket()
		if err != nil {
			return nil, err
		}
		buckets = append(buckets, bucket)
	}
	return buckets, nil
}

type monitoringSampleInsert struct {
	NodeID             string  `json:"node_id"`
	NodeName           string  `json:"node_name"`
	TargetID           string  `json:"target_id"`
	TargetName         string  `json:"target_name"`
	TargetAddress      string  `json:"target_address"`
	Attempts           int     `json:"attempts"`
	SuccessfulAttempts int     `json:"successful_attempts"`
	AverageLatencyMS   float64 `json:"average_latency_ms"`
	Error              string  `json:"error"`
	CheckedAt          string  `json:"checked_at"`
	IngestedAt         string  `json:"ingested_at"`
}

type monitoringHistoryRow struct {
	Time               string          `json:"bucket"`
	NodeID             string          `json:"node_id"`
	NodeName           string          `json:"node_name"`
	TargetID           string          `json:"target_id"`
	TargetName         string          `json:"target_name"`
	TargetAddress      string          `json:"target_address"`
	Attempts           json.RawMessage `json:"total_attempts"`
	SuccessfulAttempts json.RawMessage `json:"total_successful_attempts"`
	AverageLatencyMS   json.RawMessage `json:"average_latency_ms"`
	FailedRounds       json.RawMessage `json:"failed_rounds"`
}

func (r monitoringHistoryRow) bucket() (MonitoringHistoryBucket, error) {
	timestamp, err := parseClickHouseTime(r.Time)
	if err != nil {
		return MonitoringHistoryBucket{}, fmt.Errorf("decode monitoring history timestamp: %w", err)
	}
	attempts, err := parseJSONUint64(r.Attempts)
	if err != nil {
		return MonitoringHistoryBucket{}, fmt.Errorf("decode monitoring history attempts: %w", err)
	}
	successes, err := parseJSONUint64(r.SuccessfulAttempts)
	if err != nil {
		return MonitoringHistoryBucket{}, fmt.Errorf("decode monitoring history successes: %w", err)
	}
	failedRounds, err := parseJSONUint64(r.FailedRounds)
	if err != nil {
		return MonitoringHistoryBucket{}, fmt.Errorf("decode monitoring history failures: %w", err)
	}
	latency, err := parseNullableJSONFloat64(r.AverageLatencyMS)
	if err != nil {
		return MonitoringHistoryBucket{}, fmt.Errorf("decode monitoring history latency: %w", err)
	}
	return MonitoringHistoryBucket{
		Time: timestamp, NodeID: r.NodeID, NodeName: r.NodeName,
		TargetID: r.TargetID, TargetName: r.TargetName, TargetAddress: r.TargetAddress,
		Attempts: attempts, SuccessfulAttempts: successes, AverageLatencyMS: latency, FailedRounds: failedRounds,
	}, nil
}

func parseNullableJSONFloat64(contents json.RawMessage) (*float64, error) {
	value := strings.TrimSpace(string(contents))
	if value == "" || value == "null" {
		return nil, nil
	}
	parsed, err := strconv.ParseFloat(strings.Trim(value, `"`), 64)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

type AsyncMonitoringWriter struct {
	store         MonitoringHistoryStore
	logger        *slog.Logger
	queue         chan []MonitoringSample
	done          chan struct{}
	started       sync.Once
	flushInterval time.Duration
}

func NewAsyncMonitoringWriter(store MonitoringHistoryStore, logger *slog.Logger) *AsyncMonitoringWriter {
	return newAsyncMonitoringWriter(store, logger, monitoringHistoryQueueSize)
}

func newAsyncMonitoringWriter(store MonitoringHistoryStore, logger *slog.Logger, queueSize int) *AsyncMonitoringWriter {
	if queueSize < 1 {
		queueSize = 1
	}
	return &AsyncMonitoringWriter{store: store, logger: logger, queue: make(chan []MonitoringSample, queueSize), done: make(chan struct{}), flushInterval: monitoringHistoryFlushInterval}
}

func (w *AsyncMonitoringWriter) Start(ctx context.Context) {
	w.started.Do(func() { go w.run(ctx) })
}

func (w *AsyncMonitoringWriter) Wait() {
	<-w.done
}

func (w *AsyncMonitoringWriter) EnqueueMonitoring(samples []MonitoringSample) bool {
	if len(samples) == 0 {
		return true
	}
	copied := append([]MonitoringSample(nil), samples...)
	select {
	case w.queue <- copied:
		return true
	default:
		if w.logger != nil {
			w.logger.Error("monitoring history queue is full; dropping samples", "samples", len(samples), "capacity", cap(w.queue))
		}
		return false
	}
}

func (w *AsyncMonitoringWriter) run(ctx context.Context) {
	defer close(w.done)
	for {
		select {
		case samples := <-w.queue:
			batch := w.collectBatch(ctx, samples)
			if ctx.Err() != nil {
				w.flushOnShutdown(batch)
				return
			}
			if !w.writeWithRetry(ctx, batch) {
				w.flushOnShutdown(batch)
				return
			}
		case <-ctx.Done():
			w.flushOnShutdown(nil)
			return
		}
	}
}

func (w *AsyncMonitoringWriter) collectBatch(ctx context.Context, first []MonitoringSample) []MonitoringSample {
	batch := w.drainBatch(first)
	if len(batch) >= monitoringHistoryBatchSize {
		return batch
	}
	interval := w.flushInterval
	if interval <= 0 {
		interval = monitoringHistoryFlushInterval
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for len(batch) < monitoringHistoryBatchSize {
		select {
		case samples := <-w.queue:
			batch = append(batch, samples...)
		case <-timer.C:
			return batch
		case <-ctx.Done():
			return batch
		}
	}
	return batch
}

func (w *AsyncMonitoringWriter) drainBatch(first []MonitoringSample) []MonitoringSample {
	batch := append([]MonitoringSample(nil), first...)
	for len(batch) < monitoringHistoryBatchSize {
		select {
		case samples := <-w.queue:
			batch = append(batch, samples...)
		default:
			return batch
		}
	}
	return batch
}

func (w *AsyncMonitoringWriter) writeWithRetry(ctx context.Context, samples []MonitoringSample) bool {
	delay := 500 * time.Millisecond
	for {
		requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := w.store.AppendMonitoring(requestCtx, samples)
		cancel()
		if err == nil {
			return true
		}
		if w.logger != nil {
			w.logger.Warn("write monitoring history to ClickHouse", "error", err, "samples", len(samples), "retry_in", delay)
		}
		if schema, ok := w.store.(interface{ EnsureSchema(context.Context) error }); ok {
			schemaCtx, schemaCancel := context.WithTimeout(ctx, 15*time.Second)
			_ = schema.EnsureSchema(schemaCtx)
			schemaCancel()
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}

func (w *AsyncMonitoringWriter) flushOnShutdown(initial []MonitoringSample) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pending := append([]MonitoringSample(nil), initial...)
	for {
		if len(pending) == 0 {
			select {
			case samples := <-w.queue:
				pending = w.drainBatch(samples)
			default:
				return
			}
		}
		if err := w.store.AppendMonitoring(shutdownCtx, pending); err != nil {
			if w.logger != nil {
				w.logger.Warn("flush monitoring history during shutdown", "error", err, "samples", len(pending))
			}
			return
		}
		pending = nil
	}
}
