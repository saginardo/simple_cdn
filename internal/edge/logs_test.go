package edge

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"simple_cdn/internal/domain"
)

func TestDecodeNginxLogIncludesRequestDetails(t *testing.T) {
	line := []byte(`{"request_id":"request-1","client_request_id":"client-1","upstream_request_id":"origin-1","timestamp":"2026-07-18T10:20:30Z","site_id":"site-1","client_ip":"203.0.113.5","host":"cdn.example.test","scheme":"https","protocol":"HTTP/2.0","method":"GET","path":"/asset.js?token=secret","status":404,"request_bytes":512,"bytes":2048,"duration_seconds":0.037,"request_completion":"OK","upstream":"192.0.2.10:443","upstream_status":"404","upstream_connect_time":"0.004","upstream_header_time":"0.020","upstream_response_time":"0.036","upstream_bytes_sent":"640","upstream_bytes_received":"2304","cache_status":"MISS","user_agent":"test-agent","referer":"https://example.test/","content_type":"application/json","response_content_type":"text/javascript","accept":"*/*","range":"bytes=0-1023"}`)
	event, err := decodeNginxLog(line)
	if err != nil {
		t.Fatal(err)
	}
	if event.ID != "request-1" || event.Timestamp != time.Date(2026, 7, 18, 10, 20, 30, 0, time.UTC) || event.Path != "/asset.js" || event.RequestBytes != 512 || event.Bytes != 2048 || event.DurationMS != 37 {
		t.Fatalf("decoded core event = %#v", event)
	}
	if event.Host != "cdn.example.test" || event.Protocol != "HTTP/2.0" || event.UserAgent != "test-agent" || event.UpstreamStatus != "404" || event.ResponseContentType != "text/javascript" || event.Range != "bytes=0-1023" {
		t.Fatalf("decoded request details = %#v", event)
	}
	if event.UpstreamConnectTime != "0.004" || event.UpstreamHeaderTime != "0.020" || event.UpstreamResponseTime != "0.036" {
		t.Fatalf("decoded upstream timings = %#v", event)
	}
	if event.ClientRequestID != "client-1" || event.UpstreamRequestID != "origin-1" || event.RequestCompletion != "OK" || event.UpstreamBytesSent != "640" || event.UpstreamBytesReceived != "2304" {
		t.Fatalf("decoded trace details = %#v", event)
	}
}

func TestDecodeNginxLogGeneratesMissingRequestID(t *testing.T) {
	event, err := decodeNginxLog([]byte(`{"timestamp":"2026-07-18T10:20:30Z","duration_seconds":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(event.ID); err != nil {
		t.Fatalf("generated request ID %q is invalid: %v", event.ID, err)
	}
	if event.RequestCompletion != "UNKNOWN" {
		t.Fatalf("legacy request completion = %q, want UNKNOWN", event.RequestCompletion)
	}
}

func TestDecodeNginxLogMarksEmptyCompletionAsInterrupted(t *testing.T) {
	event, err := decodeNginxLog([]byte(`{"timestamp":"2026-07-18T10:20:30Z","duration_seconds":0,"request_completion":""}`))
	if err != nil {
		t.Fatal(err)
	}
	if event.RequestCompletion != "INTERRUPTED" {
		t.Fatalf("request completion = %q, want INTERRUPTED", event.RequestCompletion)
	}
}

func TestLogForwarderMigratesLegacyQueueAndFlushes(t *testing.T) {
	directory := t.TempDir()
	forwarder := NewLogForwarder(directory, filepath.Join(directory, "access.json"))
	event := domain.AccessLogEvent{ID: "legacy-request", Timestamp: time.Date(2026, 7, 18, 10, 20, 30, 0, time.UTC), SiteID: "site-1"}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	legacyContents := append([]byte("invalid legacy record\n"), encoded...)
	legacyContents = append(legacyContents, '\n')
	if err := os.WriteFile(forwarder.legacyQueuePath, legacyContents, 0o640); err != nil {
		t.Fatal(err)
	}

	var received []domain.AccessLogEvent
	client := newAccessLogClient(t, func(events []domain.AccessLogEvent) int {
		received = events
		return http.StatusAccepted
	})
	if err := forwarder.Flush(t.Context(), "https://control.example.test", client); err != nil {
		t.Fatal(err)
	}

	if len(received) != 1 || received[0].ID != event.ID {
		t.Fatalf("received events = %#v", received)
	}
	if _, err := os.Stat(forwarder.legacyQueuePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy queue was not removed: %v", err)
	}
	segments, _, err := forwarder.queueSegments()
	if err != nil || len(segments) != 0 {
		t.Fatalf("queue segments after flush = %#v, %v", segments, err)
	}
	cursor, err := forwarder.cursor()
	if err != nil || cursor.Segment != logSegmentName(1) || cursor.Offset != int64(len(legacyContents)) {
		t.Fatalf("migration cursor = %#v, %v", cursor, err)
	}
}

func TestLogForwarderAppendsRollbackLegacyQueueAfterExistingSegments(t *testing.T) {
	directory := t.TempDir()
	forwarder := NewLogForwarder(directory, filepath.Join(directory, "access.json"))
	if err := os.MkdirAll(forwarder.queueDir, 0o750); err != nil {
		t.Fatal(err)
	}
	queued := func(id string) []byte {
		contents, err := json.Marshal(domain.AccessLogEvent{ID: id, Timestamp: time.Date(2026, 7, 18, 10, 20, 30, 0, time.UTC), SiteID: "site-1"})
		if err != nil {
			t.Fatal(err)
		}
		return append(contents, '\n')
	}
	if err := os.WriteFile(filepath.Join(forwarder.queueDir, logSegmentName(1)), queued("segmented-request"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forwarder.legacyQueuePath, queued("rollback-request"), 0o640); err != nil {
		t.Fatal(err)
	}

	var received []string
	client := newAccessLogClient(t, func(events []domain.AccessLogEvent) int {
		for _, event := range events {
			received = append(received, event.ID)
		}
		return http.StatusAccepted
	})
	if err := forwarder.Flush(t.Context(), "https://control.example.test", client); err != nil {
		t.Fatal(err)
	}
	if err := forwarder.Flush(t.Context(), "https://control.example.test", client); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(received) != "[segmented-request rollback-request]" {
		t.Fatalf("rollback migration order = %v", received)
	}
}

func TestLogForwarderResumesFromCursorWithoutRewritingSegment(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "access.json")
	writeAccessLog(t, logPath, "request-1", "request-2", "request-3")
	forwarder := NewLogForwarder(directory, logPath)
	forwarder.batchSize = 2
	if count, err := forwarder.Collect(); err != nil || count != 3 {
		t.Fatalf("collected = %d, %v", count, err)
	}
	segments, _, err := forwarder.queueSegments()
	if err != nil || len(segments) != 1 {
		t.Fatalf("initial segments = %#v, %v", segments, err)
	}
	segmentPath := segments[0].path
	before, err := os.ReadFile(segmentPath)
	if err != nil {
		t.Fatal(err)
	}

	var received []string
	client := newAccessLogClient(t, func(events []domain.AccessLogEvent) int {
		for _, event := range events {
			received = append(received, event.ID)
		}
		return http.StatusAccepted
	})
	if err := forwarder.Flush(t.Context(), "https://control.example.test", client); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(segmentPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("partially acknowledged segment was rewritten")
	}
	cursor, err := forwarder.cursor()
	if err != nil || cursor.Offset <= 0 || cursor.Offset >= int64(len(before)) {
		t.Fatalf("partial cursor = %#v, %v", cursor, err)
	}

	restarted := NewLogForwarder(directory, logPath)
	restarted.batchSize = 2
	if err := restarted.Flush(t.Context(), "https://control.example.test", client); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(received) != "[request-1 request-2 request-3]" {
		t.Fatalf("received order = %v", received)
	}
	segments, _, err = restarted.queueSegments()
	if err != nil || len(segments) != 0 {
		t.Fatalf("segments after resumed flush = %#v, %v", segments, err)
	}
}

func TestLogForwarderRotatesSegmentsAndFlushesInOrder(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "access.json")
	writeAccessLog(t, logPath, "request-1", "request-2", "request-3")
	firstEvent, err := decodeNginxLog(accessLogLine("request-1"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(firstEvent)
	if err != nil {
		t.Fatal(err)
	}
	forwarder := NewLogForwarder(directory, logPath)
	forwarder.segmentBytes = int64(len(encoded) + 1)
	if count, err := forwarder.Collect(); err != nil || count != 3 {
		t.Fatalf("collected = %d, %v", count, err)
	}
	segments, _, err := forwarder.queueSegments()
	if err != nil || len(segments) != 3 {
		t.Fatalf("rotated segments = %#v, %v", segments, err)
	}
	for _, segment := range segments {
		if segment.size > forwarder.segmentBytes {
			t.Fatalf("segment %q has %d bytes, limit %d", segment.name, segment.size, forwarder.segmentBytes)
		}
	}

	var received []string
	client := newAccessLogClient(t, func(events []domain.AccessLogEvent) int {
		for _, event := range events {
			received = append(received, event.ID)
		}
		return http.StatusAccepted
	})
	forwarder.drainBudget = time.Second
	if err := forwarder.drain(t.Context(), "https://control.example.test", client); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(received) != "[request-1 request-2 request-3]" {
		t.Fatalf("received order = %v", received)
	}
}

func TestLogForwarderFailedUploadDoesNotAdvanceCursor(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "access.json")
	writeAccessLog(t, logPath, "request-1")
	forwarder := NewLogForwarder(directory, logPath)
	if _, err := forwarder.Collect(); err != nil {
		t.Fatal(err)
	}

	var attempts [][]domain.AccessLogEvent
	client := newAccessLogClient(t, func(events []domain.AccessLogEvent) int {
		attempts = append(attempts, events)
		if len(attempts) == 1 {
			return http.StatusServiceUnavailable
		}
		return http.StatusAccepted
	})

	if err := forwarder.Flush(t.Context(), "https://control.example.test", client); err == nil {
		t.Fatal("failed upload unexpectedly succeeded")
	}
	if _, err := os.Stat(forwarder.cursorPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed upload advanced cursor: %v", err)
	}
	if err := forwarder.Flush(t.Context(), "https://control.example.test", client); err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || len(attempts[0]) != 1 || len(attempts[1]) != 1 || attempts[0][0].ID != attempts[1][0].ID {
		t.Fatalf("upload attempts = %#v", attempts)
	}
}

func TestLogForwarderQueueLimitBackpressuresSourceOffset(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "access.json")
	firstLine := accessLogLine("request-1")
	writeAccessLog(t, logPath, "request-1", "request-2")
	firstEvent, err := decodeNginxLog(firstLine)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(firstEvent)
	if err != nil {
		t.Fatal(err)
	}
	forwarder := NewLogForwarder(directory, logPath)
	forwarder.maxQueueBytes = int64(len(encoded) + 1)

	count, err := forwarder.Collect()
	if count != 1 || !errors.Is(err, errAccessLogQueueFull) {
		t.Fatalf("first collect = %d, %v", count, err)
	}
	offsetContents, err := os.ReadFile(forwarder.offsetPath)
	if err != nil {
		t.Fatal(err)
	}
	wantOffset := int64(len(firstLine) + 1)
	if offset, err := strconv.ParseInt(string(offsetContents), 10, 64); err != nil || offset != wantOffset {
		t.Fatalf("source offset = %q, want %d (%v)", offsetContents, wantOffset, err)
	}
	_, queuedBytes, err := forwarder.queueSegments()
	if err != nil || queuedBytes > forwarder.maxQueueBytes {
		t.Fatalf("queued bytes = %d, limit = %d, err = %v", queuedBytes, forwarder.maxQueueBytes, err)
	}

	client := newAccessLogClient(t, func([]domain.AccessLogEvent) int { return http.StatusAccepted })
	if err := forwarder.Flush(t.Context(), "https://control.example.test", client); err != nil {
		t.Fatal(err)
	}
	if count, err := forwarder.Collect(); err != nil || count != 1 {
		t.Fatalf("second collect = %d, %v", count, err)
	}
}

func TestLogForwarderRunContinuouslyDrainsBacklog(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "access.json")
	ids := make([]string, 1201)
	for index := range ids {
		ids[index] = fmt.Sprintf("request-%04d", index)
	}
	writeAccessLog(t, logPath, ids...)
	forwarder := NewLogForwarder(directory, logPath)
	forwarder.interval = 10 * time.Millisecond
	forwarder.drainBudget = 5 * time.Second

	var mu sync.Mutex
	batches := make([]int, 0, 3)
	total := 0
	allReceived := make(chan struct{})
	var receivedOnce sync.Once
	client := newAccessLogClient(t, func(events []domain.AccessLogEvent) int {
		mu.Lock()
		batches = append(batches, len(events))
		total += len(events)
		if total == len(ids) {
			receivedOnce.Do(func() { close(allReceived) })
		}
		mu.Unlock()
		return http.StatusAccepted
	})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		forwarder.Run(ctx, "https://control.example.test", func() *http.Client { return client })
		close(runDone)
	}()
	select {
	case <-allReceived:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for access-log backlog to drain")
	}
	deadline := time.Now().Add(time.Second)
	for {
		pending, err := forwarder.hasPending()
		if err != nil {
			t.Fatal(err)
		}
		if !pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queue remained pending after all events were accepted")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("log forwarder did not stop after cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(batches) != "[500 500 201]" || total != len(ids) {
		t.Fatalf("uploaded batches = %v, total = %d", batches, total)
	}
}

func TestLogForwarderAdaptiveBatchingAndCompression(t *testing.T) {
	newQueuedForwarder := func(count int) *LogForwarder {
		directory := t.TempDir()
		logPath := filepath.Join(directory, "access.json")
		ids := make([]string, count)
		for index := range ids {
			ids[index] = fmt.Sprintf("adaptive-%03d", index)
		}
		writeAccessLog(t, logPath, ids...)
		forwarder := NewLogForwarder(directory, logPath)
		if collected, err := forwarder.Collect(); err != nil || collected != count {
			t.Fatalf("collected=%d err=%v", collected, err)
		}
		return forwarder
	}
	uploads := 0
	compressed := false
	client := &http.Client{Transport: accessLogRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		uploads++
		compressed = request.Header.Get("Content-Encoding") == "gzip"
		reader := io.Reader(request.Body)
		if compressed {
			gzipReader, err := gzip.NewReader(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			defer gzipReader.Close()
			reader = gzipReader
		}
		var events []domain.AccessLogEvent
		if err := json.NewDecoder(reader).Decode(&events); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusAccepted, Status: "202 Accepted", Header: make(http.Header), Body: http.NoBody}, nil
	})}

	lowTraffic := newQueuedForwarder(logAdaptiveBatchSize - 1)
	lowTraffic.SetCompressionEnabled(true)
	progressed, deferred, err := lowTraffic.flushBatchReady(t.Context(), "https://control.example.test", client, false)
	if err != nil || progressed || !deferred || uploads != 0 {
		t.Fatalf("low-traffic decision: progressed=%v deferred=%v uploads=%d err=%v", progressed, deferred, uploads, err)
	}
	progressed, deferred, err = lowTraffic.flushBatchReady(t.Context(), "https://control.example.test", client, true)
	if err != nil || !progressed || deferred || uploads != 1 || !compressed {
		t.Fatalf("forced upload: progressed=%v deferred=%v uploads=%d compressed=%v err=%v", progressed, deferred, uploads, compressed, err)
	}

	thresholdTraffic := newQueuedForwarder(logAdaptiveBatchSize)
	thresholdTraffic.SetCompressionEnabled(true)
	progressed, deferred, err = thresholdTraffic.flushBatchReady(t.Context(), "https://control.example.test", client, false)
	if err != nil || !progressed || deferred || uploads != 2 {
		t.Fatalf("threshold upload: progressed=%v deferred=%v uploads=%d err=%v", progressed, deferred, uploads, err)
	}
}

func TestLogForwarderWaitsForCompleteSourceLine(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "access.json")
	line := accessLogLine("request-1")
	if err := os.WriteFile(logPath, line, 0o640); err != nil {
		t.Fatal(err)
	}
	forwarder := NewLogForwarder(directory, logPath)
	if count, err := forwarder.Collect(); err != nil || count != 0 {
		t.Fatalf("partial-line collect = %d, %v", count, err)
	}
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{'\n'}); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if count, err := forwarder.Collect(); err != nil || count != 1 {
		t.Fatalf("complete-line collect = %d, %v", count, err)
	}
}

func accessLogLine(id string) []byte {
	return []byte(fmt.Sprintf(`{"request_id":%q,"timestamp":"2026-07-18T10:20:30Z","site_id":"site-1","duration_seconds":0}`, id))
}

func writeAccessLog(t *testing.T, path string, ids ...string) {
	t.Helper()
	var contents bytes.Buffer
	for _, id := range ids {
		contents.Write(accessLogLine(id))
		contents.WriteByte('\n')
	}
	if err := os.WriteFile(path, contents.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
}

type accessLogRoundTripFunc func(*http.Request) (*http.Response, error)

func (function accessLogRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func newAccessLogClient(t *testing.T, receive func([]domain.AccessLogEvent) int) *http.Client {
	t.Helper()
	return &http.Client{Transport: accessLogRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/edge/v1/logs" {
			t.Errorf("upload path = %q", request.URL.Path)
		}
		reader := io.Reader(request.Body)
		if request.Header.Get("Content-Encoding") == "gzip" {
			compressed, err := gzip.NewReader(request.Body)
			if err != nil {
				t.Errorf("open compressed upload: %v", err)
			} else {
				defer compressed.Close()
				reader = compressed
			}
		}
		var events []domain.AccessLogEvent
		if err := json.NewDecoder(reader).Decode(&events); err != nil {
			t.Errorf("decode uploaded events: %v", err)
		}
		status := receive(events)
		return &http.Response{
			StatusCode: status,
			Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    request,
		}, nil
	})}
}
