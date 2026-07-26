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
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"simple_cdn/internal/domain"
	"simple_cdn/internal/project"
)

const (
	securityEventQueueLimit = 10000
	securityLocalBanLimit   = 50000
	securityLogBatchLimit   = 1000
	securityLogLineLimit    = 16 << 10
)

type SecurityFirewall interface {
	Replace([]domain.SecurityBan) error
}

type NftablesFirewall struct {
	Binary string
}

func defaultSecurityFirewall(configured SecurityFirewall) SecurityFirewall {
	if configured != nil {
		return configured
	}
	binary, err := exec.LookPath("nft")
	if err != nil {
		return nil
	}
	return NftablesFirewall{Binary: binary}
}

func (f NftablesFirewall) Replace(bans []domain.SecurityBan) error {
	binary := f.Binary
	if binary == "" {
		binary = "nft"
	}
	currentExists := exec.Command(binary, "list", "table", "inet", project.NftablesTable).Run() == nil
	legacyExists := exec.Command(binary, "list", "table", "inet", project.LegacyNftablesTable).Run() == nil
	script := nftablesRuleset(bans, currentExists, legacyExists, time.Now().UTC())
	command := exec.Command(binary, "-f", "-")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("apply CDN nftables security table: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func nftablesRuleset(bans []domain.SecurityBan, currentExists, legacyExists bool, now time.Time) string {
	type element struct {
		ip      string
		seconds int64
	}
	elements := make([]element, 0, len(bans))
	for _, ban := range bans {
		address, err := netip.ParseAddr(ban.IP)
		seconds := int64(ban.ExpiresAt.Sub(now).Seconds())
		if err != nil || !address.Is4() || !address.IsGlobalUnicast() || address.IsPrivate() || !ban.ExpiresAt.After(now) || seconds < 1 {
			continue
		}
		elements = append(elements, element{ip: address.String(), seconds: seconds})
	}
	sort.Slice(elements, func(i, j int) bool { return elements[i].ip < elements[j].ip })
	var script strings.Builder
	if currentExists {
		script.WriteString("delete table inet " + project.NftablesTable + "\n")
	}
	if legacyExists {
		script.WriteString("delete table inet " + project.LegacyNftablesTable + "\n")
	}
	script.WriteString("table inet " + project.NftablesTable + " {\n  set banned_ipv4 {\n    type ipv4_addr\n    flags timeout\n")
	if len(elements) > 0 {
		script.WriteString("    elements = { ")
		for index, item := range elements {
			if index > 0 {
				script.WriteString(", ")
			}
			script.WriteString(item.ip)
			script.WriteString(" timeout ")
			script.WriteString(strconv.FormatInt(item.seconds, 10))
			script.WriteString("s")
		}
		script.WriteString(" }\n")
	}
	script.WriteString("  }\n  chain input {\n    type filter hook input priority -10; policy accept;\n    tcp dport { 80, 443 } ip saddr @banned_ipv4 drop\n  }\n}\n")
	return script.String()
}

type securityLogEvent struct {
	Timestamp  string                      `json:"timestamp"`
	PolicyID   string                      `json:"policy_id"`
	Action     domain.SecurityPolicyAction `json:"action"`
	BanSeconds int                         `json:"ban_seconds"`
	ClientIP   string                      `json:"client_ip"`
	Host       string                      `json:"host"`
	Method     string                      `json:"method"`
	Path       string                      `json:"path"`
}

type localSecurityBan struct {
	domain.SecurityBan
	Pending bool `json:"pending"`
}

type localSecurityState struct {
	Bans     []localSecurityBan `json:"bans"`
	Revision string             `json:"revision,omitempty"`
}

type SecurityManager struct {
	stateDir     string
	logPath      string
	pollInterval time.Duration
	firewall     SecurityFirewall

	mu              sync.Mutex
	lastError       string
	firewallDirty   bool
	desiredRevision string
	appliedRevision string
	revisionSignal  chan struct{}
	syncMu          sync.Mutex
}

func NewSecurityManager(stateDir, logPath string, pollInterval time.Duration, firewall SecurityFirewall) *SecurityManager {
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}
	return &SecurityManager{
		stateDir: stateDir, logPath: logPath, pollInterval: pollInterval, firewall: firewall,
		revisionSignal: make(chan struct{}, 1),
	}
}

func (m *SecurityManager) Run(ctx context.Context, controlURL string, clientFactory func() *http.Client) {
	if err := m.initialize(); err != nil {
		m.setError(err)
	}
	collectTicker := time.NewTicker(m.pollInterval)
	legacySyncTicker := time.NewTicker(30 * time.Second)
	fallbackTicker := time.NewTicker(5 * time.Minute)
	defer collectTicker.Stop()
	defer legacySyncTicker.Stop()
	defer fallbackTicker.Stop()
	if err := m.syncBansIfChanged(ctx, controlURL, clientFactory, false); err != nil {
		m.setError(err)
	} else {
		m.setError(nil)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-collectTicker.C:
			if err := m.collectAndFlush(ctx, controlURL, clientFactory); err != nil {
				m.setError(err)
			} else {
				m.setError(nil)
			}
		case <-m.revisionSignal:
			if err := m.syncBansIfChanged(ctx, controlURL, clientFactory, false); err != nil {
				m.setError(err)
			} else {
				m.setError(nil)
			}
		case <-legacySyncTicker.C:
			if m.usesLegacySync() {
				if err := m.syncBansIfChanged(ctx, controlURL, clientFactory, true); err != nil {
					m.setError(err)
				} else {
					m.setError(nil)
				}
			}
		case <-fallbackTicker.C:
			if err := m.syncBansIfChanged(ctx, controlURL, clientFactory, true); err != nil {
				m.setError(err)
			} else {
				m.setError(nil)
			}
		}
	}
}

func (m *SecurityManager) usesLegacySync() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.desiredRevision == ""
}

func (m *SecurityManager) NotifyRevision(revision string) {
	if revision != "" && !validDigest(revision) {
		return
	}
	m.mu.Lock()
	changed := revision != m.desiredRevision
	m.desiredRevision = revision
	m.mu.Unlock()
	if changed {
		select {
		case m.revisionSignal <- struct{}{}:
		default:
		}
	}
}

func (m *SecurityManager) LastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastError
}

func (m *SecurityManager) setError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		m.lastError = ""
		return
	}
	m.lastError = err.Error()
}

func (m *SecurityManager) initialize() error {
	if err := os.MkdirAll(m.stateDir, 0o750); err != nil {
		return err
	}
	state, err := m.loadState()
	if err != nil {
		return err
	}
	state.Bans = limitLocalSecurityBans(activeLocalBans(state.Bans, time.Now().UTC()))
	if err := m.saveState(state); err != nil {
		return err
	}
	m.mu.Lock()
	m.appliedRevision = state.Revision
	m.mu.Unlock()
	return m.replaceFirewall(localBanValues(state.Bans))
}

func (m *SecurityManager) collectAndFlush(ctx context.Context, controlURL string, clientFactory func() *http.Client) error {
	events, position, err := m.collect()
	if err != nil {
		return err
	}
	if len(events) > 0 {
		queued, err := m.loadQueue()
		if err != nil {
			return err
		}
		queued = append(queued, events...)
		if len(queued) > securityEventQueueLimit {
			queued = append([]domain.SecurityEvent(nil), queued[len(queued)-securityEventQueueLimit:]...)
		}
		if err := m.saveQueue(queued); err != nil {
			return err
		}
	}
	var firewallBans []domain.SecurityBan
	if containsSecurityBan(events) {
		firewallBans, err = m.persistLocalBans(events)
		if err != nil {
			return err
		}
	}
	if position >= 0 {
		if err := atomicWriteFile(m.offsetPath(), []byte(strconv.FormatInt(position, 10)), 0o640); err != nil {
			return err
		}
	}
	if firewallBans != nil || m.firewallNeedsRetry() {
		if firewallBans == nil {
			state, err := m.loadState()
			if err != nil {
				return err
			}
			firewallBans = localBanValues(activeLocalBans(state.Bans, time.Now().UTC()))
		}
		if err := m.replaceFirewall(firewallBans); err != nil {
			return err
		}
	}
	return m.flush(ctx, controlURL, clientFactory)
}

func (m *SecurityManager) collect() ([]domain.SecurityEvent, int64, error) {
	file, err := os.Open(m.logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, -1, nil
	}
	if err != nil {
		return nil, -1, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, -1, err
	}
	offset := readInt64File(m.offsetPath())
	if info.Size() < offset {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, -1, err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), securityLogLineLimit)
	position := offset
	events := make([]domain.SecurityEvent, 0)
	lines := 0
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		position += int64(len(line) + 1)
		lines++
		event, err := decodeSecurityLog(line)
		if err == nil {
			events = append(events, event)
		}
		if lines >= securityLogBatchLimit {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, -1, err
	}
	return events, position, nil
}

func decodeSecurityLog(line []byte) (domain.SecurityEvent, error) {
	var raw securityLogEvent
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return domain.SecurityEvent{}, err
	}
	if _, err := uuid.Parse(raw.PolicyID); err != nil {
		return domain.SecurityEvent{}, errors.New("invalid security policy ID")
	}
	address, err := netip.ParseAddr(strings.TrimSpace(raw.ClientIP))
	if err != nil || !address.Is4() || !address.IsGlobalUnicast() || address.IsPrivate() {
		return domain.SecurityEvent{}, errors.New("security event client IP is not public IPv4")
	}
	if raw.Action != domain.SecurityActionBlock && raw.Action != domain.SecurityActionBan {
		return domain.SecurityEvent{}, errors.New("invalid security action")
	}
	if raw.Action == domain.SecurityActionBan && !domain.ValidSecurityBanDuration(raw.BanSeconds) {
		return domain.SecurityEvent{}, errors.New("invalid security ban duration")
	}
	if raw.Action == domain.SecurityActionBlock {
		raw.BanSeconds = 0
	}
	observedAt, err := time.Parse(time.RFC3339, raw.Timestamp)
	if err != nil {
		return domain.SecurityEvent{}, err
	}
	path := strings.TrimSpace(raw.Path)
	if path == "" || len(path) > 2048 || len(raw.Host) > 255 || len(raw.Method) > 16 {
		return domain.SecurityEvent{}, errors.New("invalid security event fields")
	}
	return domain.SecurityEvent{
		ID: uuid.NewString(), PolicyID: raw.PolicyID, ClientIP: address.String(), Host: strings.TrimSpace(raw.Host),
		Path: path, Method: strings.ToUpper(strings.TrimSpace(raw.Method)), Action: raw.Action,
		BanDurationSeconds: raw.BanSeconds, ObservedAt: observedAt,
	}, nil
}

func containsSecurityBan(events []domain.SecurityEvent) bool {
	for _, event := range events {
		if event.Action == domain.SecurityActionBan {
			return true
		}
	}
	return false
}

func (m *SecurityManager) persistLocalBans(events []domain.SecurityEvent) ([]domain.SecurityBan, error) {
	state, err := m.loadState()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	byIP := make(map[string]localSecurityBan)
	for _, ban := range activeLocalBans(state.Bans, now) {
		byIP[ban.IP] = ban
	}
	for _, event := range events {
		if event.Action != domain.SecurityActionBan {
			continue
		}
		expiresAt := now.Add(time.Duration(event.BanDurationSeconds) * time.Second)
		candidate := localSecurityBan{SecurityBan: domain.SecurityBan{
			IP: event.ClientIP, PolicyID: event.PolicyID, Host: event.Host, Path: event.Path,
			Method: event.Method, ExpiresAt: expiresAt, CreatedAt: now, UpdatedAt: now,
		}, Pending: true}
		if existing, found := byIP[event.ClientIP]; found && existing.ExpiresAt.After(candidate.ExpiresAt) {
			candidate.ExpiresAt = existing.ExpiresAt
			candidate.CreatedAt = existing.CreatedAt
		}
		byIP[event.ClientIP] = candidate
	}
	state.Bans = limitLocalSecurityBans(localBanMapValues(byIP))
	if err := m.saveState(state); err != nil {
		return nil, err
	}
	return localBanValues(state.Bans), nil
}

func (m *SecurityManager) applyLocalBans(events []domain.SecurityEvent) error {
	bans, err := m.persistLocalBans(events)
	if err != nil {
		return err
	}
	return m.replaceFirewall(bans)
}

func (m *SecurityManager) flush(ctx context.Context, controlURL string, clientFactory func() *http.Client) error {
	queued, err := m.loadQueue()
	if err != nil || len(queued) == 0 {
		return err
	}
	count := len(queued)
	if count > 200 {
		count = 200
	}
	payload, err := json.Marshal(domain.EdgeSecurityEventBatch{Events: queued[:count]})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(controlURL, "/")+"/api/edge/v1/security-events", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	client, err := securityHTTPClient(clientFactory)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
	_, _ = io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		return readErr
	}
	if response.StatusCode != http.StatusAccepted {
		if response.StatusCode >= 400 && response.StatusCode < 500 {
			var rejection struct {
				InvalidEventIndex *int `json:"invalid_event_index"`
			}
			if json.Unmarshal(body, &rejection) == nil && rejection.InvalidEventIndex != nil &&
				*rejection.InvalidEventIndex >= 0 && *rejection.InvalidEventIndex < count {
				index := *rejection.InvalidEventIndex
				remaining := make([]domain.SecurityEvent, 0, len(queued)-1)
				remaining = append(remaining, queued[:index]...)
				remaining = append(remaining, queued[index+1:]...)
				_ = m.saveQueue(remaining)
			}
		}
		return fmt.Errorf("upload security events: %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	if closeErr != nil {
		return closeErr
	}
	if err := m.saveQueue(queued[count:]); err != nil {
		return err
	}
	if err := m.clearPending(queued[:count]); err != nil {
		return err
	}
	var result struct {
		SecurityRevision string `json:"security_revision"`
	}
	_ = json.Unmarshal(body, &result)
	if result.SecurityRevision == "" || !validDigest(result.SecurityRevision) {
		return m.syncBans(ctx, controlURL, client)
	}
	m.NotifyRevision(result.SecurityRevision)
	if m.revisionNeedsSync() {
		return m.syncBans(ctx, controlURL, client)
	}
	return nil
}

func securityHTTPClient(clientFactory func() *http.Client) (*http.Client, error) {
	if clientFactory == nil {
		return nil, errors.New("security HTTP client factory is not configured")
	}
	client := clientFactory()
	if client == nil {
		return nil, errors.New("security HTTP client factory returned nil")
	}
	return client, nil
}

func (m *SecurityManager) syncBansWithFactory(ctx context.Context, controlURL string, clientFactory func() *http.Client) error {
	client, err := securityHTTPClient(clientFactory)
	if err != nil {
		return err
	}
	return m.syncBans(ctx, controlURL, client)
}

func (m *SecurityManager) syncBansIfChanged(ctx context.Context, controlURL string, clientFactory func() *http.Client, force bool) error {
	if !force && !m.revisionNeedsSync() {
		return nil
	}
	client, err := securityHTTPClient(clientFactory)
	if err != nil {
		return err
	}
	return m.syncBans(ctx, controlURL, client)
}

func (m *SecurityManager) revisionNeedsSync() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.firewallDirty || m.desiredRevision == "" || m.desiredRevision != m.appliedRevision
}

func (m *SecurityManager) clearPending(events []domain.SecurityEvent) error {
	state, err := m.loadState()
	if err != nil {
		return err
	}
	acceptedIPs := make(map[string]struct{}, len(events))
	for _, event := range events {
		acceptedIPs[event.ClientIP] = struct{}{}
	}
	for index := range state.Bans {
		if _, found := acceptedIPs[state.Bans[index].IP]; found {
			state.Bans[index].Pending = false
		}
	}
	return m.saveState(state)
}

func (m *SecurityManager) syncBans(ctx context.Context, controlURL string, client *http.Client) error {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(controlURL, "/")+"/api/edge/v1/security-bans", nil)
	if err != nil {
		return err
	}
	m.mu.Lock()
	appliedRevision := m.appliedRevision
	desiredRevision := m.desiredRevision
	m.mu.Unlock()
	if appliedRevision != "" {
		request.Header.Set("If-None-Match", `"`+appliedRevision+`"`)
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer drainAndClose(response.Body)
	responseRevision := revisionFromETag(response.Header.Get("ETag"))
	if response.StatusCode == http.StatusNotModified {
		local, loadErr := m.loadState()
		if loadErr != nil {
			return loadErr
		}
		if responseRevision == "" {
			responseRevision = appliedRevision
		}
		m.setAppliedRevision(responseRevision)
		if m.firewallNeedsRetry() {
			return m.replaceFirewall(localBanValues(activeLocalBans(local.Bans, time.Now().UTC())))
		}
		return nil
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("pull security bans: %s", response.Status)
	}
	var remote domain.EdgeSecurityBanState
	decoder := json.NewDecoder(io.LimitReader(response.Body, 8<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&remote); err != nil {
		return err
	}
	local, err := m.loadState()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	originalBanCount := len(local.Bans)
	previousRevision := local.Revision
	activeByIP := make(map[string]localSecurityBan, originalBanCount)
	for _, ban := range activeLocalBans(append([]localSecurityBan(nil), local.Bans...), now) {
		activeByIP[ban.IP] = ban
	}
	previousBans := limitLocalSecurityBans(localBanMapValues(activeByIP))
	byIP := make(map[string]localSecurityBan, len(remote.Bans)+len(local.Bans))
	for _, ban := range remote.Bans {
		address, err := netip.ParseAddr(ban.IP)
		if err == nil && address.Is4() && address.IsGlobalUnicast() && !address.IsPrivate() && ban.ExpiresAt.After(now) {
			ban.IP = address.String()
			byIP[ban.IP] = localSecurityBan{SecurityBan: domain.SecurityBan{IP: ban.IP, ExpiresAt: ban.ExpiresAt}}
		}
	}
	for _, ban := range previousBans {
		if ban.Pending {
			byIP[ban.IP] = ban
		}
	}
	local.Bans = limitLocalSecurityBans(localBanMapValues(byIP))
	if responseRevision == "" {
		responseRevision = desiredRevision
	}
	local.Revision = responseRevision
	if originalBanCount == len(previousBans) && equivalentLocalBanSets(previousBans, local.Bans) &&
		previousRevision == responseRevision && !m.firewallNeedsRetry() {
		m.setAppliedRevision(responseRevision)
		return nil
	}
	if err := m.saveState(local); err != nil {
		return err
	}
	m.setAppliedRevision(responseRevision)
	return m.replaceFirewall(localBanValues(local.Bans))
}

func (m *SecurityManager) setAppliedRevision(revision string) {
	m.mu.Lock()
	m.appliedRevision = revision
	m.mu.Unlock()
}

func (m *SecurityManager) replaceFirewall(bans []domain.SecurityBan) error {
	err := m.firewall.Replace(bans)
	m.mu.Lock()
	m.firewallDirty = err != nil
	m.mu.Unlock()
	return err
}

func (m *SecurityManager) firewallNeedsRetry() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.firewallDirty
}

func activeLocalBans(bans []localSecurityBan, now time.Time) []localSecurityBan {
	result := bans[:0]
	for _, ban := range bans {
		if ban.ExpiresAt.After(now) {
			result = append(result, ban)
		}
	}
	return result
}

func localBanMapValues(values map[string]localSecurityBan) []localSecurityBan {
	result := make([]localSecurityBan, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].IP < result[j].IP })
	return result
}

func limitLocalSecurityBans(values []localSecurityBan) []localSecurityBan {
	if len(values) <= securityLocalBanLimit {
		return values
	}
	sort.Slice(values, func(i, j int) bool {
		if !values[i].ExpiresAt.Equal(values[j].ExpiresAt) {
			return values[i].ExpiresAt.After(values[j].ExpiresAt)
		}
		if !values[i].UpdatedAt.Equal(values[j].UpdatedAt) {
			return values[i].UpdatedAt.After(values[j].UpdatedAt)
		}
		return values[i].IP < values[j].IP
	})
	values = values[:securityLocalBanLimit]
	sort.Slice(values, func(i, j int) bool { return values[i].IP < values[j].IP })
	return values
}

func equivalentLocalBanSets(left, right []localSecurityBan) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].IP != right[index].IP || left[index].Pending != right[index].Pending ||
			!left[index].ExpiresAt.Equal(right[index].ExpiresAt) {
			return false
		}
	}
	return true
}

func localBanValues(values []localSecurityBan) []domain.SecurityBan {
	result := make([]domain.SecurityBan, 0, len(values))
	for _, value := range values {
		result = append(result, value.SecurityBan)
	}
	return result
}

func (m *SecurityManager) loadState() (localSecurityState, error) {
	contents, err := os.ReadFile(m.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return localSecurityState{Bans: []localSecurityBan{}}, nil
	}
	if err != nil {
		return localSecurityState{}, err
	}
	var state localSecurityState
	if err := json.Unmarshal(contents, &state); err != nil {
		return localSecurityState{}, err
	}
	return state, nil
}

func (m *SecurityManager) saveState(state localSecurityState) error {
	contents, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return atomicWriteFile(m.statePath(), contents, 0o600)
}

func (m *SecurityManager) loadQueue() ([]domain.SecurityEvent, error) {
	contents, err := os.ReadFile(m.queuePath())
	if errors.Is(err, os.ErrNotExist) {
		return []domain.SecurityEvent{}, nil
	}
	if err != nil {
		return nil, err
	}
	var events []domain.SecurityEvent
	if err := json.Unmarshal(contents, &events); err != nil {
		return nil, err
	}
	return events, nil
}

func (m *SecurityManager) saveQueue(events []domain.SecurityEvent) error {
	contents, err := json.Marshal(events)
	if err != nil {
		return err
	}
	return atomicWriteFile(m.queuePath(), contents, 0o600)
}

func (m *SecurityManager) statePath() string { return filepath.Join(m.stateDir, "security-bans.json") }
func (m *SecurityManager) queuePath() string {
	return filepath.Join(m.stateDir, "security-event-queue.json")
}
func (m *SecurityManager) offsetPath() string {
	return filepath.Join(m.stateDir, "security-log-offset")
}

func readInt64File(path string) int64 {
	contents, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	value, _ := strconv.ParseInt(strings.TrimSpace(string(contents)), 10, 64)
	return value
}
