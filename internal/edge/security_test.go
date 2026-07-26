package edge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"simple_cdn/internal/domain"
	"simple_cdn/internal/project"
)

type securityRoundTripFunc func(*http.Request) (*http.Response, error)

func (function securityRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type fakeSecurityFirewall struct {
	bans [][]domain.SecurityBan
	err  error
}

func (f *fakeSecurityFirewall) Replace(bans []domain.SecurityBan) error {
	copyOfBans := append([]domain.SecurityBan(nil), bans...)
	f.bans = append(f.bans, copyOfBans)
	return f.err
}

func TestDecodeSecurityLogAndApplyLocalBan(t *testing.T) {
	event, err := decodeSecurityLog([]byte(fmt.Sprintf(`{"timestamp":"%s","policy_id":"%s","action":"ban","ban_seconds":21600,"client_ip":"8.8.4.4","host":"cdn.example.test","method":"GET","path":"/.env"}`,
		time.Now().UTC().Format(time.RFC3339), domain.DefaultSecurityPolicyID)))
	if err != nil {
		t.Fatal(err)
	}
	if event.ClientIP != "8.8.4.4" || event.BanDurationSeconds != 21600 || event.Path != "/.env" {
		t.Fatalf("decoded event = %#v", event)
	}
	if _, err := uuid.Parse(event.ID); err != nil {
		t.Fatalf("event ID = %q: %v", event.ID, err)
	}
	firewall := &fakeSecurityFirewall{}
	manager := NewSecurityManager(t.TempDir(), filepath.Join(t.TempDir(), "security.json"), time.Second, firewall)
	if err := manager.initialize(); err != nil {
		t.Fatal(err)
	}
	if err := manager.applyLocalBans([]domain.SecurityEvent{event}); err != nil {
		t.Fatal(err)
	}
	if len(firewall.bans) < 2 || len(firewall.bans[len(firewall.bans)-1]) != 1 {
		t.Fatalf("firewall calls = %#v", firewall.bans)
	}
	ban := firewall.bans[len(firewall.bans)-1][0]
	if remaining := time.Until(ban.ExpiresAt); remaining < 5*time.Hour+59*time.Minute || remaining > 6*time.Hour+time.Minute {
		t.Fatalf("local ban duration = %s", remaining)
	}
}

func TestDecodeRateLimitBanLog(t *testing.T) {
	policyID := "99999999-9999-4999-8999-999999999999"
	event, err := decodeSecurityLog([]byte(fmt.Sprintf(
		`{"timestamp":"%s","policy_id":"%s","action":"ban","ban_seconds":3600,"client_ip":"9.9.9.9","host":"cdn.example.test","method":"POST","path":"/api/failures"}`,
		time.Now().UTC().Format(time.RFC3339), policyID)))
	if err != nil {
		t.Fatal(err)
	}
	if event.PolicyID != policyID || event.ClientIP != "9.9.9.9" || event.Action != domain.SecurityActionBan ||
		event.BanDurationSeconds != 3600 || event.Path != "/api/failures" {
		t.Fatalf("decoded rate limit event = %#v", event)
	}
}

func TestDecodeSecurityLogRejectsPrivateIPAndDuration(t *testing.T) {
	base := time.Now().UTC().Format(time.RFC3339)
	for _, line := range []string{
		fmt.Sprintf(`{"timestamp":"%s","policy_id":"%s","action":"ban","ban_seconds":7200,"client_ip":"8.8.8.8","path":"/.env"}`, base, domain.DefaultSecurityPolicyID),
		fmt.Sprintf(`{"timestamp":"%s","policy_id":"%s","action":"ban","ban_seconds":3600,"client_ip":"10.0.0.1","path":"/.env"}`, base, domain.DefaultSecurityPolicyID),
	} {
		if _, err := decodeSecurityLog([]byte(line)); err == nil {
			t.Fatalf("invalid log was accepted: %s", line)
		}
	}
}

func TestNftablesRulesetSyntax(t *testing.T) {
	now := time.Now().UTC()
	ruleset := nftablesRuleset([]domain.SecurityBan{{IP: "8.8.8.8", ExpiresAt: now.Add(time.Hour)}}, false, true, now)
	for _, wanted := range []string{"delete table inet " + project.LegacyNftablesTable, "table inet " + project.NftablesTable, "flags timeout", "8.8.8.8 timeout 3600s", "tcp dport { 80, 443 }", "udp dport 443", "@banned_ipv4 drop"} {
		if !strings.Contains(ruleset, wanted) {
			t.Fatalf("ruleset lacks %q:\n%s", wanted, ruleset)
		}
	}
	binary, err := exec.LookPath("nft")
	if err != nil {
		t.Skip("nft is not installed")
	}
	command := exec.Command(binary, "--check", "--file", "-")
	command.Stdin = strings.NewReader(nftablesRuleset([]domain.SecurityBan{{IP: "8.8.8.8", ExpiresAt: now.Add(time.Hour)}}, false, false, now))
	if output, err := command.CombinedOutput(); err != nil && !strings.Contains(string(output), "Operation not permitted") {
		t.Fatalf("nft --check: %v\n%s\n%s", err, output, ruleset)
	}
}

func TestSecuritySyncUsesFreshClientFactory(t *testing.T) {
	manager := NewSecurityManager(t.TempDir(), filepath.Join(t.TempDir(), "security.json"), time.Second, &fakeSecurityFirewall{})
	factoryCalls := 0
	factory := func() *http.Client {
		factoryCalls++
		return &http.Client{Transport: securityRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(strings.NewReader(`{"bans":[],"generated_at":"2026-07-16T00:00:00Z"}`)),
				Header:     make(http.Header),
			}, nil
		})}
	}
	for range 2 {
		if err := manager.syncBansWithFactory(context.Background(), "https://control.example.test", factory); err != nil {
			t.Fatal(err)
		}
	}
	if factoryCalls != 2 {
		t.Fatalf("client factory calls = %d, want 2", factoryCalls)
	}
}

func TestSecuritySyncOnlyPullsChangedRevision(t *testing.T) {
	directory := t.TempDir()
	manager := NewSecurityManager(directory, filepath.Join(directory, "security.json"), time.Second, &fakeSecurityFirewall{})
	firstRevision := strings.Repeat("a", 64)
	secondRevision := strings.Repeat("b", 64)
	if err := manager.saveState(localSecurityState{Bans: []localSecurityBan{}, Revision: firstRevision}); err != nil {
		t.Fatal(err)
	}
	manager.NotifyRevision(firstRevision)
	if err := manager.initialize(); err != nil {
		t.Fatal(err)
	}
	factoryCalls := 0
	factory := func() *http.Client {
		factoryCalls++
		return &http.Client{Transport: securityRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(strings.NewReader(`{"bans":[],"generated_at":"2026-07-16T00:00:00Z"}`)),
				Header:     http.Header{"ETag": {`"` + secondRevision + `"`}},
			}, nil
		})}
	}
	if err := manager.syncBansIfChanged(context.Background(), "https://control.example.test", factory, false); err != nil {
		t.Fatal(err)
	}
	if factoryCalls != 0 {
		t.Fatalf("unchanged revision made %d requests", factoryCalls)
	}
	manager.NotifyRevision(secondRevision)
	if err := manager.syncBansIfChanged(context.Background(), "https://control.example.test", factory, false); err != nil {
		t.Fatal(err)
	}
	state, err := manager.loadState()
	if err != nil {
		t.Fatal(err)
	}
	if factoryCalls != 1 || state.Revision != secondRevision {
		t.Fatalf("changed revision: requests=%d state=%#v", factoryCalls, state)
	}
}

func TestSecurityFirewallFailureAdvancesOffsetAndRetriesState(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "security.json")
	line := fmt.Sprintf(`{"timestamp":"%s","policy_id":"%s","action":"ban","ban_seconds":3600,"client_ip":"8.8.8.8","host":"cdn.example.test","method":"GET","path":"/.env"}`,
		time.Now().UTC().Format(time.RFC3339), domain.DefaultSecurityPolicyID)
	if err := os.WriteFile(logPath, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	firewall := &fakeSecurityFirewall{err: errors.New("temporary nft failure")}
	manager := NewSecurityManager(directory, logPath, time.Second, firewall)
	if err := manager.collectAndFlush(context.Background(), "https://control.example.test", nil); err == nil {
		t.Fatal("firewall failure was not reported")
	}
	if offset := readInt64File(manager.offsetPath()); offset != int64(len(line)+1) {
		t.Fatalf("security log offset = %d, want %d", offset, len(line)+1)
	}
	events, _, err := manager.collect()
	if err != nil || len(events) != 0 {
		t.Fatalf("events after advanced offset = %#v, err=%v", events, err)
	}
	queued, err := manager.loadQueue()
	if err != nil || len(queued) != 1 {
		t.Fatalf("durable queue = %#v, err=%v", queued, err)
	}
	firewall.err = nil
	if err := manager.collectAndFlush(context.Background(), "https://control.example.test", nil); err == nil || !strings.Contains(err.Error(), "client factory") {
		t.Fatalf("retry result = %v", err)
	}
	if len(firewall.bans) != 2 || len(firewall.bans[1]) != 1 || firewall.bans[1][0].IP != "8.8.8.8" {
		t.Fatalf("firewall retries = %#v", firewall.bans)
	}
}

func TestSecurityLogCollectionIsBounded(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "security.json")
	line := fmt.Sprintf(`{"timestamp":"%s","policy_id":"%s","action":"block","ban_seconds":0,"client_ip":"8.8.8.8","path":"/.env"}`,
		time.Now().UTC().Format(time.RFC3339), domain.DefaultSecurityPolicyID)
	var contents strings.Builder
	for range securityLogBatchLimit + 1 {
		contents.WriteString(line)
		contents.WriteByte('\n')
	}
	if err := os.WriteFile(logPath, []byte(contents.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewSecurityManager(directory, logPath, time.Second, &fakeSecurityFirewall{})
	events, position, err := manager.collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != securityLogBatchLimit || position >= int64(contents.Len()) {
		t.Fatalf("collected events=%d position=%d log_size=%d", len(events), position, contents.Len())
	}
}

func TestSecurityFlushDropsOnlyRejectedEvent(t *testing.T) {
	directory := t.TempDir()
	manager := NewSecurityManager(directory, filepath.Join(directory, "security.json"), time.Second, &fakeSecurityFirewall{})
	events := []domain.SecurityEvent{
		{ID: "11111111-1111-4111-8111-111111111111", PolicyID: domain.DefaultSecurityPolicyID, ClientIP: "8.8.8.8"},
		{ID: "22222222-2222-4222-8222-222222222222", PolicyID: domain.DefaultSecurityPolicyID, ClientIP: "1.1.1.1"},
	}
	if err := manager.saveQueue(events); err != nil {
		t.Fatal(err)
	}
	factory := func() *http.Client {
		return &http.Client{Transport: securityRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Status:     "400 Bad Request",
				Body:       io.NopCloser(strings.NewReader(`{"error":"stale policy","invalid_event_index":1}`)),
				Header:     make(http.Header),
			}, nil
		})}
	}
	if err := manager.flush(context.Background(), "https://control.example.test", factory); err == nil {
		t.Fatal("event rejection was not reported")
	}
	remaining, err := manager.loadQueue()
	if err != nil || len(remaining) != 1 || remaining[0].ID != events[0].ID {
		t.Fatalf("remaining queue = %#v, err=%v", remaining, err)
	}
}

func TestSecurityFlushSkipsBanPullWhenRevisionIsUnchanged(t *testing.T) {
	directory := t.TempDir()
	manager := NewSecurityManager(directory, filepath.Join(directory, "security.json"), time.Second, &fakeSecurityFirewall{})
	revision := strings.Repeat("d", 64)
	if err := manager.saveState(localSecurityState{Bans: []localSecurityBan{}, Revision: revision}); err != nil {
		t.Fatal(err)
	}
	manager.NotifyRevision(revision)
	if err := manager.initialize(); err != nil {
		t.Fatal(err)
	}
	event := domain.SecurityEvent{ID: uuid.NewString(), ClientIP: "8.8.8.8", Action: domain.SecurityActionBlock}
	if err := manager.saveQueue([]domain.SecurityEvent{event}); err != nil {
		t.Fatal(err)
	}
	postCount, getCount := 0, 0
	factory := func() *http.Client {
		return &http.Client{Transport: securityRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.Method == http.MethodGet {
				getCount++
			}
			if request.Method == http.MethodPost {
				postCount++
			}
			return &http.Response{
				StatusCode: http.StatusAccepted,
				Status:     "202 Accepted",
				Body:       io.NopCloser(strings.NewReader(`{"accepted":1,"security_revision":"` + revision + `"}`)),
				Header:     make(http.Header),
			}, nil
		})}
	}
	if err := manager.flush(context.Background(), "https://control.example.test", factory); err != nil {
		t.Fatal(err)
	}
	if postCount != 1 || getCount != 0 {
		t.Fatalf("security requests: post=%d get=%d", postCount, getCount)
	}
}
