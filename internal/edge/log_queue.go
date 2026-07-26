package edge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"simple_cdn/internal/domain"
)

type logQueueSegment struct {
	name     string
	path     string
	sequence uint64
	size     int64
}

type logQueueCursor struct {
	Segment string `json:"segment"`
	Offset  int64  `json:"offset"`
}

type logQueueBatch struct {
	segment    logQueueSegment
	end        int64
	events     []domain.AccessLogEvent
	eventBytes int64
}

func (f *LogForwarder) nextBatch() (*logQueueBatch, error) {
	segments, _, err := f.queueSegments()
	if err != nil || len(segments) == 0 {
		return nil, err
	}
	segment := segments[0]
	cursor, err := f.cursor()
	if err != nil {
		return nil, err
	}
	start := int64(0)
	if cursor.Segment != "" {
		cursorSequence, valid := parseLogSegmentName(cursor.Segment)
		if !valid {
			return nil, fmt.Errorf("invalid access-log queue cursor segment %q", cursor.Segment)
		}
		if cursorSequence > segment.sequence {
			return nil, fmt.Errorf("access-log queue cursor %q is ahead of oldest segment %q", cursor.Segment, segment.name)
		}
		if cursor.Segment == segment.name {
			start = cursor.Offset
		}
	}
	if start < 0 || start > segment.size {
		return nil, fmt.Errorf("access-log queue cursor offset %d is outside segment %q (%d bytes)", start, segment.name, segment.size)
	}
	batch := &logQueueBatch{segment: segment, end: start, events: make([]domain.AccessLogEvent, 0, f.batchLimit())}
	if start == segment.size {
		return batch, nil
	}
	file, err := os.Open(segment.path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(file, 64<<10)
	records := 0
	eventBytes := int64(0)
	for records < f.batchLimit() {
		line, consumed, err := readLogQueueRecord(reader)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		recordStart := batch.end
		batch.end += consumed
		if len(line) == 0 {
			records++
			continue
		}
		var event domain.AccessLogEvent
		if err := json.Unmarshal(line, &event); err != nil {
			records++
			continue
		}
		if len(batch.events) > 0 && eventBytes+consumed > logUploadBatchBytes {
			batch.end = recordStart
			break
		}
		batch.events = append(batch.events, event)
		eventBytes += consumed
		batch.eventBytes = eventBytes
		records++
	}
	if batch.end == start {
		return nil, fmt.Errorf("access-log queue segment %q made no read progress", segment.name)
	}
	return batch, nil
}

func readLogQueueRecord(reader *bufio.Reader) ([]byte, int64, error) {
	var line []byte
	var consumed int64
	tooLarge := false
	for {
		chunk, err := reader.ReadSlice('\n')
		consumed += int64(len(chunk))
		if !tooLarge {
			if consumed > logQueueRecordBytes {
				line = nil
				tooLarge = true
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case err == nil:
			line = bytes.TrimSuffix(line, []byte{'\n'})
			line = bytes.TrimSuffix(line, []byte{'\r'})
			return line, consumed, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && consumed == 0:
			return nil, 0, io.EOF
		case errors.Is(err, io.EOF):
			line = bytes.TrimSuffix(line, []byte{'\r'})
			return line, consumed, nil
		default:
			return nil, consumed, err
		}
	}
}

func (f *LogForwarder) ackBatch(batch *logQueueBatch) error {
	info, err := os.Stat(batch.segment.path)
	if err != nil {
		return err
	}
	if info.Size() < batch.end {
		return fmt.Errorf("access-log queue segment %q shrank before acknowledgement", batch.segment.name)
	}
	cursor := logQueueCursor{Segment: batch.segment.name, Offset: batch.end}
	contents, err := json.Marshal(cursor)
	if err != nil {
		return err
	}
	if err := atomicWriteFile(f.cursorPath, append(contents, '\n'), 0o640); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(f.cursorPath)); err != nil {
		return err
	}
	if info.Size() != batch.end {
		return nil
	}
	if err := os.Remove(batch.segment.path); err != nil {
		return err
	}
	return syncDirectory(f.queueDir)
}

func (f *LogForwarder) hasPending() (bool, error) {
	segments, _, err := f.queueSegments()
	return len(segments) > 0, err
}

func (f *LogForwarder) queueSegments() ([]logQueueSegment, int64, error) {
	if err := f.prepareQueue(); err != nil {
		return nil, 0, err
	}
	entries, err := os.ReadDir(f.queueDir)
	if err != nil {
		return nil, 0, err
	}
	segments := make([]logQueueSegment, 0, len(entries))
	var total int64
	for _, entry := range entries {
		sequence, valid := parseLogSegmentName(entry.Name())
		if !valid {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		if !info.Mode().IsRegular() {
			return nil, 0, fmt.Errorf("access-log queue segment %q is not a regular file", entry.Name())
		}
		segments = append(segments, logQueueSegment{
			name: entry.Name(), path: filepath.Join(f.queueDir, entry.Name()), sequence: sequence, size: info.Size(),
		})
		total += info.Size()
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].sequence < segments[j].sequence })
	return segments, total, nil
}

func (f *LogForwarder) prepareQueue() error {
	if err := os.MkdirAll(f.stateDir, 0o750); err != nil {
		return err
	}
	if err := os.MkdirAll(f.queueDir, 0o750); err != nil {
		return err
	}
	info, err := os.Stat(f.legacyQueuePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("legacy access-log queue %q is not a regular file", f.legacyQueuePath)
	}
	if info.Size() == 0 {
		if err := os.Remove(f.legacyQueuePath); err != nil {
			return err
		}
		return syncDirectory(f.stateDir)
	}
	sequence, err := f.nextQueueSequence()
	if err != nil {
		return err
	}
	target := filepath.Join(f.queueDir, logSegmentName(sequence))
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("cannot migrate legacy access-log queue: target %q already exists", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(f.legacyQueuePath, target); err != nil {
		return err
	}
	if err := syncDirectory(f.queueDir); err != nil {
		return err
	}
	return syncDirectory(f.stateDir)
}

func (f *LogForwarder) nextQueueSequence() (uint64, error) {
	entries, err := os.ReadDir(f.queueDir)
	if err != nil {
		return 0, err
	}
	var maximum uint64
	for _, entry := range entries {
		if sequence, valid := parseLogSegmentName(entry.Name()); valid && sequence > maximum {
			maximum = sequence
		}
	}
	cursor, err := f.cursor()
	if err != nil {
		return 0, err
	}
	if cursor.Segment != "" {
		sequence, _ := parseLogSegmentName(cursor.Segment)
		if sequence > maximum {
			maximum = sequence
		}
	}
	if maximum == ^uint64(0) {
		return 0, errors.New("access-log queue segment sequence is exhausted")
	}
	return maximum + 1, nil
}

func (f *LogForwarder) cursor() (logQueueCursor, error) {
	contents, err := os.ReadFile(f.cursorPath)
	if errors.Is(err, os.ErrNotExist) {
		return logQueueCursor{}, nil
	}
	if err != nil {
		return logQueueCursor{}, err
	}
	var cursor logQueueCursor
	if err := json.Unmarshal(contents, &cursor); err != nil {
		return logQueueCursor{}, fmt.Errorf("decode access-log queue cursor: %w", err)
	}
	if _, valid := parseLogSegmentName(cursor.Segment); !valid || cursor.Offset < 0 {
		return logQueueCursor{}, errors.New("access-log queue cursor is invalid")
	}
	return cursor, nil
}

func (f *LogForwarder) batchLimit() int {
	if f.batchSize <= 0 {
		return logUploadBatchSize
	}
	return f.batchSize
}

func logSegmentName(sequence uint64) string {
	return fmt.Sprintf("%020d.ndjson", sequence)
}

func parseLogSegmentName(name string) (uint64, bool) {
	if len(name) != 27 || !strings.HasSuffix(name, ".ndjson") {
		return 0, false
	}
	sequence, err := strconv.ParseUint(strings.TrimSuffix(name, ".ndjson"), 10, 64)
	return sequence, err == nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

type logQueueAppender struct {
	forwarder *LogForwarder
	total     int64
	next      uint64
	path      string
	size      int64
	file      *os.File
	created   bool
}

func (f *LogForwarder) newQueueAppender() (*logQueueAppender, error) {
	segments, total, err := f.queueSegments()
	if err != nil {
		return nil, err
	}
	var maximum uint64
	if len(segments) > 0 {
		maximum = segments[len(segments)-1].sequence
	}
	cursor, err := f.cursor()
	if err != nil {
		return nil, err
	}
	if cursor.Segment != "" {
		sequence, _ := parseLogSegmentName(cursor.Segment)
		if len(segments) > 0 && sequence > segments[0].sequence {
			return nil, fmt.Errorf("access-log queue cursor %q is ahead of oldest segment %q", cursor.Segment, segments[0].name)
		}
		if sequence > maximum {
			maximum = sequence
		}
	}
	appender := &logQueueAppender{forwarder: f, total: total, next: maximum}
	if len(segments) > 0 {
		latest := segments[len(segments)-1]
		if latest.size < f.segmentLimit() {
			appender.path = latest.path
			appender.size = latest.size
		}
	}
	return appender, nil
}

func (f *LogForwarder) segmentLimit() int64 {
	if f.segmentBytes <= 0 {
		return logQueueSegmentBytes
	}
	return f.segmentBytes
}

func (f *LogForwarder) queueLimit() int64 {
	if f.maxQueueBytes <= 0 {
		return maxLogQueueBytes
	}
	return f.maxQueueBytes
}

func (a *logQueueAppender) Append(record []byte) (bool, error) {
	if int64(len(record)) > a.forwarder.queueLimit()-a.total {
		return false, nil
	}
	if a.path != "" && a.size > 0 && a.size+int64(len(record)) > a.forwarder.segmentLimit() {
		if err := a.closeFile(); err != nil {
			return false, err
		}
		a.path = ""
		a.size = 0
	}
	if a.file == nil {
		if err := a.openFile(); err != nil {
			return false, err
		}
	}
	written, err := a.file.Write(record)
	if err != nil {
		return false, err
	}
	if written != len(record) {
		return false, io.ErrShortWrite
	}
	a.size += int64(written)
	a.total += int64(written)
	return true, nil
}

func (a *logQueueAppender) openFile() error {
	if a.path != "" {
		file, err := os.OpenFile(a.path, os.O_APPEND|os.O_WRONLY, 0o640)
		if err != nil {
			return err
		}
		a.file = file
		return nil
	}
	for {
		if a.next == ^uint64(0) {
			return errors.New("access-log queue segment sequence is exhausted")
		}
		a.next++
		a.path = filepath.Join(a.forwarder.queueDir, logSegmentName(a.next))
		file, err := os.OpenFile(a.path, os.O_CREATE|os.O_EXCL|os.O_APPEND|os.O_WRONLY, 0o640)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		a.file = file
		a.created = true
		a.size = 0
		return nil
	}
}

func (a *logQueueAppender) Close() error {
	return a.closeFile()
}

func (a *logQueueAppender) closeFile() error {
	if a.file == nil {
		return nil
	}
	if err := a.file.Sync(); err != nil {
		a.file.Close()
		a.file = nil
		return err
	}
	if err := a.file.Close(); err != nil {
		a.file = nil
		return err
	}
	a.file = nil
	if a.created {
		a.created = false
		return syncDirectory(a.forwarder.queueDir)
	}
	return nil
}
