package edge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"simple_cdn/internal/domain"
)

const (
	maxLogQueueBytes     int64 = 256 << 20
	logQueueSegmentBytes int64 = 4 << 20
	logUploadBatchSize         = 500
	logUploadBatchBytes  int64 = 4 << 20
	logQueueRecordBytes        = 2 << 20
	logForwarderInterval       = time.Second
	logDrainBudget             = 900 * time.Millisecond
)

var errAccessLogQueueFull = errors.New("access-log queue is full")

type LogForwarder struct {
	stateDir        string
	logPath         string
	queueDir        string
	legacyQueuePath string
	offsetPath      string
	cursorPath      string

	maxQueueBytes int64
	segmentBytes  int64
	batchSize     int
	interval      time.Duration
	drainBudget   time.Duration

	errorMu   sync.RWMutex
	lastError string
}

func NewLogForwarder(stateDir, logPath string) *LogForwarder {
	return &LogForwarder{
		stateDir:        stateDir,
		logPath:         logPath,
		queueDir:        filepath.Join(stateDir, "access-log-queue"),
		legacyQueuePath: filepath.Join(stateDir, "access-log-queue.ndjson"),
		offsetPath:      filepath.Join(stateDir, "access-log-offset"),
		cursorPath:      filepath.Join(stateDir, "access-log-cursor.json"),
		maxQueueBytes:   maxLogQueueBytes,
		segmentBytes:    logQueueSegmentBytes,
		batchSize:       logUploadBatchSize,
		interval:        logForwarderInterval,
		drainBudget:     logDrainBudget,
	}
}

func (f *LogForwarder) Run(ctx context.Context, controlURL string, clientFactory func() *http.Client) {
	interval := f.interval
	if interval <= 0 {
		interval = logForwarderInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	runCycle := func() {
		_, collectErr := f.Collect()
		pending, uploadErr := f.hasPending()
		if uploadErr == nil && pending {
			var client *http.Client
			if clientFactory == nil {
				uploadErr = errors.New("access-log HTTP client factory is not configured")
			} else {
				client = clientFactory()
				if client == nil {
					uploadErr = errors.New("access-log HTTP client factory returned nil")
				}
			}
			if uploadErr == nil {
				uploadErr = f.drain(ctx, controlURL, client)
			}
		}
		f.setErrors(collectErr, uploadErr)
	}

	runCycle()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runCycle()
		}
	}
}

func (f *LogForwarder) LastError() string {
	f.errorMu.RLock()
	defer f.errorMu.RUnlock()
	return f.lastError
}

func (f *LogForwarder) setErrors(collectErr, uploadErr error) {
	parts := make([]string, 0, 2)
	if collectErr != nil {
		parts = append(parts, "collect access logs: "+collectErr.Error())
	}
	if uploadErr != nil && !errors.Is(uploadErr, context.Canceled) {
		parts = append(parts, "upload access logs: "+uploadErr.Error())
	}
	f.errorMu.Lock()
	f.lastError = strings.Join(parts, "; ")
	f.errorMu.Unlock()
}

func (f *LogForwarder) Collect() (int, error) {
	if err := f.prepareQueue(); err != nil {
		return 0, err
	}
	file, err := os.Open(f.logPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	offset := f.offset()
	if info.Size() < offset {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	appender, err := f.newQueueAppender()
	if err != nil {
		return 0, err
	}

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	scanner.Split(splitCompleteLogLine)
	count := 0
	position := offset
	var collectErr error
	queueFull := false
	for scanner.Scan() {
		line := scanner.Bytes()
		nextPosition := position + int64(len(line)+1)
		event, err := decodeNginxLog(line)
		if err != nil {
			position = nextPosition
			continue
		}
		serialized, err := json.Marshal(event)
		if err != nil {
			collectErr = err
			break
		}
		record := append(serialized, '\n')
		appended, err := appender.Append(record)
		if err != nil {
			collectErr = err
			break
		}
		if !appended {
			queueFull = true
			break
		}
		position = nextPosition
		count++
	}
	if err := scanner.Err(); err != nil && collectErr == nil {
		collectErr = err
	}
	if err := appender.Close(); err != nil {
		return count, err
	}
	if position != offset {
		if err := atomicWriteFile(f.offsetPath, []byte(strconv.FormatInt(position, 10)), 0o640); err != nil {
			return count, err
		}
	}
	if collectErr != nil {
		return count, collectErr
	}
	if queueFull {
		return count, fmt.Errorf("%w at %d bytes; collection paused until delivery resumes", errAccessLogQueueFull, f.queueLimit())
	}
	return count, nil
}

func splitCompleteLogLine(data []byte, _ bool) (advance int, token []byte, err error) {
	if index := bytes.IndexByte(data, '\n'); index >= 0 {
		return index + 1, data[:index], nil
	}
	return 0, nil, nil
}

func (f *LogForwarder) Flush(ctx context.Context, controlURL string, client *http.Client) error {
	_, err := f.flushBatch(ctx, controlURL, client)
	return err
}

func (f *LogForwarder) drain(ctx context.Context, controlURL string, client *http.Client) error {
	budget := f.drainBudget
	if budget <= 0 {
		budget = logDrainBudget
	}
	deadline := time.Now().Add(budget)
	for {
		progressed, err := f.flushBatch(ctx, controlURL, client)
		if err != nil {
			return err
		}
		if !progressed {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return nil
		}
	}
}

func (f *LogForwarder) flushBatch(ctx context.Context, controlURL string, client *http.Client) (bool, error) {
	batch, err := f.nextBatch()
	if err != nil || batch == nil {
		return false, err
	}
	if len(batch.events) > 0 {
		if client == nil {
			return false, errors.New("access-log HTTP client is not configured")
		}
		payload, err := json.Marshal(batch.events)
		if err != nil {
			return false, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(controlURL, "/")+"/api/edge/v1/logs", bytes.NewReader(payload))
		if err != nil {
			return false, err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return false, err
		}
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		_, _ = io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		if response.StatusCode != http.StatusAccepted {
			return false, fmt.Errorf("log upload: %s: %s", response.Status, strings.TrimSpace(string(body)))
		}
		if closeErr != nil {
			return false, closeErr
		}
	}
	if err := f.ackBatch(batch); err != nil {
		return false, err
	}
	return true, nil
}

func (f *LogForwarder) offset() int64 {
	contents, err := os.ReadFile(f.offsetPath)
	if err != nil {
		return 0
	}
	value, _ := strconv.ParseInt(strings.TrimSpace(string(contents)), 10, 64)
	return value
}

type nginxLog struct {
	RequestID            string      `json:"request_id"`
	Timestamp            string      `json:"timestamp"`
	SiteID               string      `json:"site_id"`
	ClientIP             string      `json:"client_ip"`
	Host                 string      `json:"host"`
	Scheme               string      `json:"scheme"`
	Protocol             string      `json:"protocol"`
	Method               string      `json:"method"`
	Path                 string      `json:"path"`
	Status               int         `json:"status"`
	RequestBytes         int64       `json:"request_bytes"`
	Bytes                int64       `json:"bytes"`
	DurationSeconds      json.Number `json:"duration_seconds"`
	Upstream             string      `json:"upstream"`
	UpstreamStatus       string      `json:"upstream_status"`
	UpstreamResponseTime string      `json:"upstream_response_time"`
	CacheStatus          string      `json:"cache_status"`
	UserAgent            string      `json:"user_agent"`
	Referer              string      `json:"referer"`
	ContentType          string      `json:"content_type"`
	ResponseContentType  string      `json:"response_content_type"`
	Accept               string      `json:"accept"`
	Range                string      `json:"range"`
}

func decodeNginxLog(line []byte) (domain.AccessLogEvent, error) {
	var raw nginxLog
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return domain.AccessLogEvent{}, err
	}
	timestamp, err := time.Parse(time.RFC3339, raw.Timestamp)
	if err != nil {
		return domain.AccessLogEvent{}, err
	}
	duration, _ := raw.DurationSeconds.Float64()
	requestID := strings.TrimSpace(raw.RequestID)
	if requestID == "" {
		requestID = uuid.NewString()
	}
	return domain.AccessLogEvent{
		ID: requestID, Timestamp: timestamp, SiteID: raw.SiteID, ClientIP: raw.ClientIP,
		Host: raw.Host, Scheme: raw.Scheme, Protocol: raw.Protocol, Method: raw.Method,
		Path: strings.SplitN(raw.Path, "?", 2)[0], Status: raw.Status, RequestBytes: raw.RequestBytes,
		Bytes: raw.Bytes, DurationMS: int64(duration * 1000), Upstream: raw.Upstream,
		UpstreamStatus: raw.UpstreamStatus, UpstreamResponseTime: raw.UpstreamResponseTime,
		CacheStatus: raw.CacheStatus, UserAgent: raw.UserAgent, Referer: raw.Referer,
		ContentType: raw.ContentType, ResponseContentType: raw.ResponseContentType,
		Accept: raw.Accept, Range: raw.Range,
	}, nil
}
