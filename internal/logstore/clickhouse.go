package logstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/project"
)

type Store interface {
	Append(ctx context.Context, events []domain.AccessLogEvent) error
	Get(ctx context.Context, id string) (domain.AccessLogEvent, error)
	Recent(ctx context.Context, siteID string, limit int) ([]domain.AccessLogEvent, error)
	Search(ctx context.Context, query LogQuery) (LogPage, error)
	Metrics(ctx context.Context, siteID string, since time.Time) ([]MinuteMetric, error)
	Overview(ctx context.Context, from, to time.Time) ([]OverviewBucket, error)
	NodeCache(ctx context.Context, nodeID string, from, to time.Time) ([]NodeCacheBucket, error)
}

var (
	ErrUnavailable = errors.New("log store unavailable")
	ErrNotFound    = errors.New("log entry not found")
)

type LogQuery struct {
	From        time.Time
	To          time.Time
	SiteID      string
	NodeID      string
	Method      string
	StatusMin   uint16
	StatusMax   uint16
	Path        string
	ClientIP    string
	CacheStatus string
	Offset      int
	Limit       int
}

type LogPage struct {
	Events  []domain.AccessLogEvent
	HasMore bool
}

type MinuteMetric struct {
	Minute                  time.Time `json:"minute"`
	Requests                uint64    `json:"requests"`
	Bytes                   int64     `json:"bytes"`
	Errors                  uint64    `json:"errors"`
	CacheHits               uint64    `json:"cache_hits"`
	UpstreamSamples         uint64    `json:"upstream_samples"`
	UpstreamHeaderSamples   uint64    `json:"upstream_header_samples"`
	UpstreamResponseSamples uint64    `json:"upstream_response_samples"`
	UpstreamReused          uint64    `json:"upstream_reused"`
	UpstreamConnectMS       float64   `json:"upstream_connect_ms"`
	UpstreamHeaderMS        float64   `json:"upstream_header_ms"`
	UpstreamResponseMS      float64   `json:"upstream_response_ms"`
}

type OverviewBucket struct {
	Hour     time.Time `json:"hour"`
	SiteID   string    `json:"site_id"`
	Status   uint16    `json:"status"`
	Requests uint64    `json:"requests"`
	Bytes    int64     `json:"bytes"`
}

type NodeCacheBucket struct {
	Status     string    `json:"status"`
	Requests   uint64    `json:"requests"`
	Bytes      int64     `json:"bytes"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

type ClickHouse struct {
	Endpoint string
	Database string
	Username string
	Password string
	Client   *http.Client
}

func (c ClickHouse) EnsureSchema(ctx context.Context) error {
	if err := c.queryInDatabase(ctx, "default", `CREATE DATABASE IF NOT EXISTS `+identifier(c.database()), nil); err != nil {
		return err
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS ` + identifier(c.database()) + `.cdn_access_logs (
	 request_id String, timestamp DateTime64(3, 'UTC'), node_id String, site_id String, client_ip String, host String, scheme LowCardinality(String), protocol LowCardinality(String), method LowCardinality(String), path String, status UInt16, request_bytes Int64, bytes Int64, duration_ms Int64, upstream String, upstream_status String, upstream_connect_time String, upstream_header_time String, upstream_response_time String, cache_status LowCardinality(String), user_agent String, referer String, request_content_type String, response_content_type String, request_accept String, request_range String
) ENGINE = MergeTree PARTITION BY toDate(timestamp) ORDER BY (site_id, timestamp, node_id) TTL timestamp + INTERVAL 7 DAY DELETE`,
		`ALTER TABLE ` + identifier(c.database()) + `.cdn_access_logs ADD COLUMN IF NOT EXISTS request_id String`,
		`ALTER TABLE ` + identifier(c.database()) + `.cdn_access_logs ADD COLUMN IF NOT EXISTS host String`,
		`ALTER TABLE ` + identifier(c.database()) + `.cdn_access_logs ADD COLUMN IF NOT EXISTS scheme LowCardinality(String)`,
		`ALTER TABLE ` + identifier(c.database()) + `.cdn_access_logs ADD COLUMN IF NOT EXISTS protocol LowCardinality(String)`,
		`ALTER TABLE ` + identifier(c.database()) + `.cdn_access_logs ADD COLUMN IF NOT EXISTS request_bytes Int64`,
		`ALTER TABLE ` + identifier(c.database()) + `.cdn_access_logs ADD COLUMN IF NOT EXISTS upstream_status String`,
		`ALTER TABLE ` + identifier(c.database()) + `.cdn_access_logs ADD COLUMN IF NOT EXISTS upstream_connect_time String`,
		`ALTER TABLE ` + identifier(c.database()) + `.cdn_access_logs ADD COLUMN IF NOT EXISTS upstream_header_time String`,
		`ALTER TABLE ` + identifier(c.database()) + `.cdn_access_logs ADD COLUMN IF NOT EXISTS upstream_response_time String`,
		`ALTER TABLE ` + identifier(c.database()) + `.cdn_access_logs ADD COLUMN IF NOT EXISTS user_agent String`,
		`ALTER TABLE ` + identifier(c.database()) + `.cdn_access_logs ADD COLUMN IF NOT EXISTS referer String`,
		`ALTER TABLE ` + identifier(c.database()) + `.cdn_access_logs ADD COLUMN IF NOT EXISTS request_content_type String`,
		`ALTER TABLE ` + identifier(c.database()) + `.cdn_access_logs ADD COLUMN IF NOT EXISTS response_content_type String`,
		`ALTER TABLE ` + identifier(c.database()) + `.cdn_access_logs ADD COLUMN IF NOT EXISTS request_accept String`,
		`ALTER TABLE ` + identifier(c.database()) + `.cdn_access_logs ADD COLUMN IF NOT EXISTS request_range String`,
		`CREATE TABLE IF NOT EXISTS ` + identifier(c.database()) + `.cdn_site_minute (
 minute DateTime('UTC'), site_id String, node_id String, requests UInt64, bytes Int64, errors UInt64, cache_hits UInt64
) ENGINE = SummingMergeTree PARTITION BY toDate(minute) ORDER BY (site_id, minute, node_id) TTL minute + INTERVAL 30 DAY DELETE`,
		`CREATE MATERIALIZED VIEW IF NOT EXISTS ` + identifier(c.database()) + `.cdn_access_to_minute TO ` + identifier(c.database()) + `.cdn_site_minute AS
	 SELECT toStartOfMinute(timestamp) AS minute, site_id, node_id, count() AS requests, sum(bytes) AS bytes, countIf(status >= 500) AS errors, countIf(cache_status = 'HIT') AS cache_hits
	 FROM ` + identifier(c.database()) + `.cdn_access_logs GROUP BY minute, site_id, node_id`,
		`CREATE TABLE IF NOT EXISTS ` + identifier(c.database()) + `.cdn_origin_minute (
	 minute DateTime('UTC'), site_id String, node_id String, connect_samples UInt64, header_samples UInt64, response_samples UInt64, reused UInt64, connect_seconds Float64, header_seconds Float64, response_seconds Float64
) ENGINE = SummingMergeTree PARTITION BY toDate(minute) ORDER BY (site_id, minute, node_id) TTL minute + INTERVAL 30 DAY DELETE`,
		`CREATE MATERIALIZED VIEW IF NOT EXISTS ` + identifier(c.database()) + `.cdn_access_to_origin_minute TO ` + identifier(c.database()) + `.cdn_origin_minute AS
		 SELECT minute, site_id, node_id,
			sum(length(connect_values)) AS connect_samples,
			sum(length(header_values)) AS header_samples,
			sum(length(response_values)) AS response_samples,
			sum(arrayCount(value -> value = 0, connect_values)) AS reused,
			sum(arraySum(connect_values)) AS connect_seconds,
			sum(arraySum(header_values)) AS header_seconds,
			sum(arraySum(response_values)) AS response_seconds
		 FROM (
			SELECT toStartOfMinute(timestamp) AS minute, site_id, node_id,
				arrayMap(value -> toFloat64(value), extractAll(upstream_connect_time, '([0-9]+[.]?[0-9]*)')) AS connect_values,
				arrayMap(value -> toFloat64(value), extractAll(upstream_header_time, '([0-9]+[.]?[0-9]*)')) AS header_values,
				arrayMap(value -> toFloat64(value), extractAll(upstream_response_time, '([0-9]+[.]?[0-9]*)')) AS response_values
			FROM ` + identifier(c.database()) + `.cdn_access_logs
		 ) GROUP BY minute, site_id, node_id`,
		monitoringHistoryTableStatement(c.database()),
	}
	for _, statement := range statements {
		if err := c.query(ctx, statement, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c ClickHouse) Append(ctx context.Context, events []domain.AccessLogEvent) error {
	if len(events) == 0 {
		return nil
	}
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	for _, event := range events {
		if event.Timestamp.IsZero() {
			event.Timestamp = time.Now().UTC()
		}
		event.Path = stripQuery(event.Path)
		row := accessLogInsert{
			ID:                   event.ID,
			Timestamp:            event.Timestamp.UTC().Format("2006-01-02 15:04:05.000"),
			NodeID:               event.NodeID,
			SiteID:               event.SiteID,
			ClientIP:             event.ClientIP,
			Host:                 event.Host,
			Scheme:               event.Scheme,
			Protocol:             event.Protocol,
			Method:               event.Method,
			Path:                 event.Path,
			Status:               event.Status,
			RequestBytes:         event.RequestBytes,
			Bytes:                event.Bytes,
			DurationMS:           event.DurationMS,
			Upstream:             event.Upstream,
			UpstreamStatus:       event.UpstreamStatus,
			UpstreamConnectTime:  event.UpstreamConnectTime,
			UpstreamHeaderTime:   event.UpstreamHeaderTime,
			UpstreamResponseTime: event.UpstreamResponseTime,
			CacheStatus:          event.CacheStatus,
			UserAgent:            event.UserAgent, Referer: event.Referer, ContentType: event.ContentType,
			ResponseContentType: event.ResponseContentType, Accept: event.Accept, Range: event.Range,
		}
		if err := encoder.Encode(row); err != nil {
			return err
		}
	}
	return c.query(ctx, "INSERT INTO "+identifier(c.database())+".cdn_access_logs FORMAT JSONEachRow", &body)
}

const accessLogSelectColumns = `request_id, timestamp, node_id, site_id, client_ip, host, scheme, protocol, method, path, status, request_bytes, bytes, duration_ms, upstream, upstream_status, upstream_connect_time, upstream_header_time, upstream_response_time, cache_status, user_agent, referer, request_content_type, response_content_type, request_accept, request_range`

func (c ClickHouse) Get(ctx context.Context, id string) (domain.AccessLogEvent, error) {
	query := `SELECT ` + accessLogSelectColumns + ` FROM ` + identifier(c.database()) + `.cdn_access_logs WHERE request_id = {request_id:String} ORDER BY timestamp DESC LIMIT 1 FORMAT JSONEachRow`
	response, err := c.request(ctx, c.database(), query, nil, url.Values{"param_request_id": {id}})
	if err != nil {
		return domain.AccessLogEvent{}, err
	}
	defer response.Body.Close()
	var row accessLogRow
	if err := json.NewDecoder(response.Body).Decode(&row); err != nil {
		if err == io.EOF {
			return domain.AccessLogEvent{}, ErrNotFound
		}
		return domain.AccessLogEvent{}, err
	}
	return row.event()
}

func (c ClickHouse) Recent(ctx context.Context, siteID string, limit int) ([]domain.AccessLogEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + accessLogSelectColumns + ` FROM ` + identifier(c.database()) + `.cdn_access_logs WHERE site_id = {site_id:String} ORDER BY timestamp DESC LIMIT ` + strconv.Itoa(limit) + ` FORMAT JSONEachRow`
	parameters := url.Values{"param_site_id": {siteID}}
	response, err := c.request(ctx, c.database(), query, nil, parameters)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	events := make([]domain.AccessLogEvent, 0)
	decoder := json.NewDecoder(response.Body)
	for {
		var row accessLogRow
		if err := decoder.Decode(&row); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		event, err := row.event()
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

func (c ClickHouse) Search(ctx context.Context, search LogQuery) (LogPage, error) {
	limit := search.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset := search.Offset
	if offset < 0 {
		offset = 0
	}
	conditions := make([]string, 0, 8)
	parameters := url.Values{
		"param_from": {search.From.UTC().Format("2006-01-02 15:04:05.000")},
		"param_to":   {search.To.UTC().Format("2006-01-02 15:04:05.000")},
	}
	if search.SiteID != "" {
		conditions = append(conditions, "site_id = {site_id:String}")
		parameters.Set("param_site_id", search.SiteID)
	}
	if search.NodeID != "" {
		conditions = append(conditions, "node_id = {node_id:String}")
		parameters.Set("param_node_id", search.NodeID)
	}
	if search.Method != "" {
		conditions = append(conditions, "method = {method:String}")
		parameters.Set("param_method", search.Method)
	}
	if search.StatusMin != 0 {
		conditions = append(conditions, "status >= {status_min:UInt16}", "status <= {status_max:UInt16}")
		parameters.Set("param_status_min", strconv.FormatUint(uint64(search.StatusMin), 10))
		parameters.Set("param_status_max", strconv.FormatUint(uint64(search.StatusMax), 10))
	}
	if search.Path != "" {
		conditions = append(conditions, "positionCaseInsensitive(path, {path:String}) > 0")
		parameters.Set("param_path", search.Path)
	}
	if search.ClientIP != "" {
		conditions = append(conditions, "client_ip = {client_ip:String}")
		parameters.Set("param_client_ip", search.ClientIP)
	}
	if search.CacheStatus != "" {
		conditions = append(conditions, "cache_status = {cache_status:String}")
		parameters.Set("param_cache_status", search.CacheStatus)
	}

	query := `SELECT ` + accessLogSelectColumns + ` FROM ` + identifier(c.database()) + `.cdn_access_logs PREWHERE timestamp >= {from:DateTime64(3)} AND timestamp < {to:DateTime64(3)}`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += ` ORDER BY timestamp DESC, site_id, node_id, client_ip, method, path, status, bytes, duration_ms, upstream, cache_status LIMIT ` + strconv.Itoa(limit+1) + ` OFFSET ` + strconv.Itoa(offset) + ` FORMAT JSONEachRow`

	response, err := c.request(ctx, c.database(), query, nil, parameters)
	if err != nil {
		return LogPage{}, err
	}
	defer response.Body.Close()
	events := make([]domain.AccessLogEvent, 0, limit+1)
	decoder := json.NewDecoder(response.Body)
	for {
		var row accessLogRow
		if err := decoder.Decode(&row); err != nil {
			if err == io.EOF {
				break
			}
			return LogPage{}, err
		}
		event, err := row.event()
		if err != nil {
			return LogPage{}, err
		}
		events = append(events, event)
	}
	hasMore := len(events) > limit
	if hasMore {
		events = events[:limit]
	}
	return LogPage{Events: events, HasMore: hasMore}, nil
}

func (c ClickHouse) Metrics(ctx context.Context, siteID string, since time.Time) ([]MinuteMetric, error) {
	query := `SELECT metrics.minute AS minute, metrics.requests AS requests, metrics.bytes AS bytes,
		metrics.errors AS errors, metrics.cache_hits AS cache_hits,
		coalesce(origin.connect_samples, 0) AS upstream_samples,
		coalesce(origin.header_samples, 0) AS upstream_header_samples,
		coalesce(origin.response_samples, 0) AS upstream_response_samples,
		coalesce(origin.reused, 0) AS upstream_reused,
		if(coalesce(origin.connect_samples, 0) = 0, 0, coalesce(origin.connect_seconds, 0) * 1000 / origin.connect_samples) AS upstream_connect_ms,
		if(coalesce(origin.header_samples, 0) = 0, 0, coalesce(origin.header_seconds, 0) * 1000 / origin.header_samples) AS upstream_header_ms,
		if(coalesce(origin.response_samples, 0) = 0, 0, coalesce(origin.response_seconds, 0) * 1000 / origin.response_samples) AS upstream_response_ms
	FROM (
		SELECT minute, sum(requests) AS requests, sum(bytes) AS bytes, sum(errors) AS errors, sum(cache_hits) AS cache_hits
		FROM ` + identifier(c.database()) + `.cdn_site_minute
		WHERE site_id = {site_id:String} AND minute >= {since:DateTime}
		GROUP BY minute
	) AS metrics
	LEFT JOIN (
		SELECT minute, sum(connect_samples) AS connect_samples, sum(header_samples) AS header_samples,
			sum(response_samples) AS response_samples, sum(reused) AS reused,
			sum(connect_seconds) AS connect_seconds, sum(header_seconds) AS header_seconds,
			sum(response_seconds) AS response_seconds
		FROM ` + identifier(c.database()) + `.cdn_origin_minute
		WHERE site_id = {site_id:String} AND minute >= {since:DateTime}
		GROUP BY minute
	) AS origin USING minute
	ORDER BY minute FORMAT JSONEachRow`
	parameters := url.Values{"param_site_id": {siteID}, "param_since": {since.UTC().Format("2006-01-02 15:04:05")}}
	response, err := c.request(ctx, c.database(), query, nil, parameters)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	metrics := make([]MinuteMetric, 0)
	decoder := json.NewDecoder(response.Body)
	for {
		var metric MinuteMetric
		if err := decoder.Decode(&metric); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		metrics = append(metrics, metric)
	}
	return metrics, nil
}

func (c ClickHouse) Overview(ctx context.Context, from, to time.Time) ([]OverviewBucket, error) {
	query := `SELECT toStartOfHour(timestamp) AS hour, site_id, status, count() AS requests, sum(bytes) AS bytes FROM ` + identifier(c.database()) + `.cdn_access_logs WHERE timestamp >= {from:DateTime} AND timestamp < {to:DateTime} GROUP BY hour, site_id, status ORDER BY hour, site_id, status FORMAT JSONEachRow`
	parameters := url.Values{
		"param_from": {from.UTC().Format("2006-01-02 15:04:05")},
		"param_to":   {to.UTC().Format("2006-01-02 15:04:05")},
	}
	response, err := c.request(ctx, c.database(), query, nil, parameters)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	buckets := make([]OverviewBucket, 0)
	decoder := json.NewDecoder(response.Body)
	for {
		var bucket OverviewBucket
		if err := decoder.Decode(&bucket); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		buckets = append(buckets, bucket)
	}
	return buckets, nil
}

func (c ClickHouse) NodeCache(ctx context.Context, nodeID string, from, to time.Time) ([]NodeCacheBucket, error) {
	query := `SELECT upper(cache_status) AS cache_status, count() AS requests, sum(bytes) AS bytes,
		max(timestamp) AS last_seen_at FROM ` + identifier(c.database()) + `.cdn_access_logs
		PREWHERE timestamp >= {from:DateTime64(3)} AND timestamp < {to:DateTime64(3)}
		WHERE node_id = {node_id:String}
		GROUP BY cache_status ORDER BY requests DESC, cache_status FORMAT JSONEachRow`
	parameters := url.Values{
		"param_node_id": {nodeID},
		"param_from":    {from.UTC().Format("2006-01-02 15:04:05.000")},
		"param_to":      {to.UTC().Format("2006-01-02 15:04:05.000")},
	}
	response, err := c.request(ctx, c.database(), query, nil, parameters)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	buckets := make([]NodeCacheBucket, 0)
	decoder := json.NewDecoder(response.Body)
	for {
		var bucket NodeCacheBucket
		if err := decoder.Decode(&bucket); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		buckets = append(buckets, bucket)
	}
	return buckets, nil
}

func (c ClickHouse) query(ctx context.Context, query string, body *bytes.Buffer) error {
	return c.queryInDatabase(ctx, c.database(), query, body)
}

func (c ClickHouse) queryInDatabase(ctx context.Context, database, query string, body *bytes.Buffer) error {
	response, err := c.request(ctx, database, query, body, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("ClickHouse %s", response.Status)
	}
	return nil
}

func (c ClickHouse) request(ctx context.Context, database, query string, body *bytes.Buffer, parameters url.Values) (*http.Response, error) {
	values := url.Values{"query": {query}}
	if database != "" {
		values.Set("database", database)
	}
	for key, value := range parameters {
		values[key] = value
	}
	endpoint := strings.TrimRight(c.Endpoint, "/")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8123"
	}
	var reader io.Reader
	if body != nil {
		reader = body
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/?"+values.Encode(), reader)
	if err != nil {
		return nil, err
	}
	if c.Username != "" || c.Password != "" {
		request.SetBasicAuth(c.Username, c.Password)
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		response.Body.Close()
		if message := strings.TrimSpace(string(detail)); message != "" {
			return nil, fmt.Errorf("ClickHouse returned %s: %s", response.Status, message)
		}
		return nil, fmt.Errorf("ClickHouse returned %s", response.Status)
	}
	return response, nil
}

func (c ClickHouse) database() string {
	if c.Database != "" {
		return c.Database
	}
	return project.ClickHouseDatabase
}

func identifier(value string) string {
	if value == "" {
		return "`" + project.ClickHouseDatabase + "`"
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_') {
			panic("unsafe ClickHouse identifier")
		}
	}
	return "`" + value + "`"
}

func stripQuery(path string) string {
	if position := strings.IndexByte(path, '?'); position >= 0 {
		return path[:position]
	}
	return path
}

type Noop struct{}

func (Noop) Append(context.Context, []domain.AccessLogEvent) error { return nil }
func (Noop) Get(context.Context, string) (domain.AccessLogEvent, error) {
	return domain.AccessLogEvent{}, ErrNotFound
}
func (Noop) Recent(context.Context, string, int) ([]domain.AccessLogEvent, error) {
	return []domain.AccessLogEvent{}, nil
}
func (Noop) Search(context.Context, LogQuery) (LogPage, error) {
	return LogPage{Events: []domain.AccessLogEvent{}}, nil
}
func (Noop) Metrics(context.Context, string, time.Time) ([]MinuteMetric, error) {
	return []MinuteMetric{}, nil
}
func (Noop) Overview(context.Context, time.Time, time.Time) ([]OverviewBucket, error) {
	return []OverviewBucket{}, nil
}
func (Noop) NodeCache(context.Context, string, time.Time, time.Time) ([]NodeCacheBucket, error) {
	return nil, ErrUnavailable
}

type accessLogRow struct {
	ID                   string `json:"request_id"`
	Timestamp            string `json:"timestamp"`
	NodeID               string `json:"node_id"`
	SiteID               string `json:"site_id"`
	ClientIP             string `json:"client_ip"`
	Host                 string `json:"host"`
	Scheme               string `json:"scheme"`
	Protocol             string `json:"protocol"`
	Method               string `json:"method"`
	Path                 string `json:"path"`
	Status               int    `json:"status"`
	RequestBytes         int64  `json:"request_bytes"`
	Bytes                int64  `json:"bytes"`
	DurationMS           int64  `json:"duration_ms"`
	Upstream             string `json:"upstream"`
	UpstreamStatus       string `json:"upstream_status"`
	UpstreamConnectTime  string `json:"upstream_connect_time"`
	UpstreamHeaderTime   string `json:"upstream_header_time"`
	UpstreamResponseTime string `json:"upstream_response_time"`
	CacheStatus          string `json:"cache_status"`
	UserAgent            string `json:"user_agent"`
	Referer              string `json:"referer"`
	ContentType          string `json:"request_content_type"`
	ResponseContentType  string `json:"response_content_type"`
	Accept               string `json:"request_accept"`
	Range                string `json:"request_range"`
}

type accessLogInsert struct {
	ID                   string `json:"request_id"`
	Timestamp            string `json:"timestamp"`
	NodeID               string `json:"node_id"`
	SiteID               string `json:"site_id"`
	ClientIP             string `json:"client_ip"`
	Host                 string `json:"host"`
	Scheme               string `json:"scheme"`
	Protocol             string `json:"protocol"`
	Method               string `json:"method"`
	Path                 string `json:"path"`
	Status               int    `json:"status"`
	RequestBytes         int64  `json:"request_bytes"`
	Bytes                int64  `json:"bytes"`
	DurationMS           int64  `json:"duration_ms"`
	Upstream             string `json:"upstream"`
	UpstreamStatus       string `json:"upstream_status"`
	UpstreamConnectTime  string `json:"upstream_connect_time"`
	UpstreamHeaderTime   string `json:"upstream_header_time"`
	UpstreamResponseTime string `json:"upstream_response_time"`
	CacheStatus          string `json:"cache_status"`
	UserAgent            string `json:"user_agent"`
	Referer              string `json:"referer"`
	ContentType          string `json:"request_content_type"`
	ResponseContentType  string `json:"response_content_type"`
	Accept               string `json:"request_accept"`
	Range                string `json:"request_range"`
}

func (r accessLogRow) event() (domain.AccessLogEvent, error) {
	timestamp, err := parseClickHouseTime(r.Timestamp)
	if err != nil {
		return domain.AccessLogEvent{}, fmt.Errorf("decode access-log timestamp: %w", err)
	}
	return domain.AccessLogEvent{
		ID: r.ID, Timestamp: timestamp, NodeID: r.NodeID, SiteID: r.SiteID, ClientIP: r.ClientIP,
		Host: r.Host, Scheme: r.Scheme, Protocol: r.Protocol, Method: r.Method, Path: r.Path,
		Status: r.Status, RequestBytes: r.RequestBytes, Bytes: r.Bytes, DurationMS: r.DurationMS,
		Upstream: r.Upstream, UpstreamStatus: r.UpstreamStatus, UpstreamConnectTime: r.UpstreamConnectTime,
		UpstreamHeaderTime: r.UpstreamHeaderTime, UpstreamResponseTime: r.UpstreamResponseTime,
		CacheStatus: r.CacheStatus, UserAgent: r.UserAgent, Referer: r.Referer,
		ContentType: r.ContentType, ResponseContentType: r.ResponseContentType, Accept: r.Accept, Range: r.Range,
	}, nil
}

func (m *MinuteMetric) UnmarshalJSON(contents []byte) error {
	var row struct {
		Minute                  string  `json:"minute"`
		Requests                uint64  `json:"requests"`
		Bytes                   int64   `json:"bytes"`
		Errors                  uint64  `json:"errors"`
		CacheHits               uint64  `json:"cache_hits"`
		UpstreamSamples         uint64  `json:"upstream_samples"`
		UpstreamHeaderSamples   uint64  `json:"upstream_header_samples"`
		UpstreamResponseSamples uint64  `json:"upstream_response_samples"`
		UpstreamReused          uint64  `json:"upstream_reused"`
		UpstreamConnectMS       float64 `json:"upstream_connect_ms"`
		UpstreamHeaderMS        float64 `json:"upstream_header_ms"`
		UpstreamResponseMS      float64 `json:"upstream_response_ms"`
	}
	if err := json.Unmarshal(contents, &row); err != nil {
		return err
	}
	minute, err := parseClickHouseTime(row.Minute)
	if err != nil {
		return fmt.Errorf("decode minute metric timestamp: %w", err)
	}
	*m = MinuteMetric{
		Minute:                  minute,
		Requests:                row.Requests,
		Bytes:                   row.Bytes,
		Errors:                  row.Errors,
		CacheHits:               row.CacheHits,
		UpstreamSamples:         row.UpstreamSamples,
		UpstreamHeaderSamples:   row.UpstreamHeaderSamples,
		UpstreamResponseSamples: row.UpstreamResponseSamples,
		UpstreamReused:          row.UpstreamReused,
		UpstreamConnectMS:       row.UpstreamConnectMS,
		UpstreamHeaderMS:        row.UpstreamHeaderMS,
		UpstreamResponseMS:      row.UpstreamResponseMS,
	}
	return nil
}

func (b *OverviewBucket) UnmarshalJSON(contents []byte) error {
	var row struct {
		Hour     string          `json:"hour"`
		SiteID   string          `json:"site_id"`
		Status   uint16          `json:"status"`
		Requests json.RawMessage `json:"requests"`
		Bytes    json.RawMessage `json:"bytes"`
	}
	if err := json.Unmarshal(contents, &row); err != nil {
		return err
	}
	hour, err := parseClickHouseTime(row.Hour)
	if err != nil {
		return fmt.Errorf("decode overview hour: %w", err)
	}
	requests, err := parseJSONUint64(row.Requests)
	if err != nil {
		return fmt.Errorf("decode overview requests: %w", err)
	}
	bytes, err := parseJSONInt64(row.Bytes)
	if err != nil {
		return fmt.Errorf("decode overview bytes: %w", err)
	}
	*b = OverviewBucket{Hour: hour, SiteID: row.SiteID, Status: row.Status, Requests: requests, Bytes: bytes}
	return nil
}

func (b *NodeCacheBucket) UnmarshalJSON(contents []byte) error {
	var row struct {
		Status     string          `json:"cache_status"`
		Requests   json.RawMessage `json:"requests"`
		Bytes      json.RawMessage `json:"bytes"`
		LastSeenAt string          `json:"last_seen_at"`
	}
	if err := json.Unmarshal(contents, &row); err != nil {
		return err
	}
	requests, err := parseJSONUint64(row.Requests)
	if err != nil {
		return fmt.Errorf("decode node cache requests: %w", err)
	}
	bytes, err := parseJSONInt64(row.Bytes)
	if err != nil {
		return fmt.Errorf("decode node cache bytes: %w", err)
	}
	lastSeenAt, err := parseClickHouseTime(row.LastSeenAt)
	if err != nil {
		return fmt.Errorf("decode node cache timestamp: %w", err)
	}
	*b = NodeCacheBucket{Status: row.Status, Requests: requests, Bytes: bytes, LastSeenAt: lastSeenAt}
	return nil
}

func parseJSONUint64(contents json.RawMessage) (uint64, error) {
	value := strings.Trim(string(contents), `"`)
	return strconv.ParseUint(value, 10, 64)
}

func parseJSONInt64(contents json.RawMessage) (int64, error) {
	value := strings.Trim(string(contents), `"`)
	return strconv.ParseInt(value, 10, 64)
}

func parseClickHouseTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.UTC(), nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported timestamp %q", value)
}
