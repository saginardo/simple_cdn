package edge

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"simple_cdn/internal/domain"
)

func TestRunCacheWarmupsAttemptsEachDesiredJobOnceAndPrunesOldState(t *testing.T) {
	calls := make([]string, 0)
	agent := &Agent{Config: Config{
		StateDir: t.TempDir(),
		CacheWarmer: func(_ context.Context, warmup domain.CacheWarmup) error {
			calls = append(calls, warmup.ID)
			if warmup.ID == "job-b" {
				return errors.New("origin unavailable")
			}
			return nil
		},
	}}
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	job := func(id string) domain.CacheWarmup {
		return domain.CacheWarmup{ID: id, SiteID: "site", Host: "cdn.example.test", Paths: []string{"/app.js"}, CreatedAt: now}
	}
	detail, results, err := agent.runCacheWarmups([]domain.CacheWarmup{job("job-a"), job("job-b")})
	if err == nil || detail != "cache prewarm completed 1 of 2 job(s)" || len(results) != 2 ||
		results[0].Status != domain.CacheWarmupSucceeded || results[1].Status != domain.CacheWarmupFailed {
		t.Fatalf("first prewarm = %q, %#v, %v", detail, results, err)
	}
	if _, _, err := agent.runCacheWarmups([]domain.CacheWarmup{job("job-a"), job("job-b")}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"job-a", "job-b"}) {
		t.Fatalf("completed jobs were retried: %#v", calls)
	}
	if _, _, err := agent.runCacheWarmups([]domain.CacheWarmup{job("job-b")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.runCacheWarmups([]domain.CacheWarmup{job("job-a")}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"job-a", "job-b", "job-a"}) {
		t.Fatalf("pruned job was not eligible again: %#v", calls)
	}
	if _, _, err := agent.runCacheWarmups(nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.runCacheWarmups([]domain.CacheWarmup{job("job-a")}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"job-a", "job-b", "job-a", "job-a"}) {
		t.Fatalf("empty desired state did not prune completed jobs: %#v", calls)
	}
}

func TestCacheWarmupResultsSurviveRestartUntilAcknowledged(t *testing.T) {
	directory := t.TempDir()
	agent := &Agent{Config: Config{StateDir: directory}}
	completedAt := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	wanted := domain.CacheWarmupResult{
		WarmupID: "11111111-1111-4111-8111-111111111111", SiteID: "site", Status: domain.CacheWarmupPartial,
		AttemptedURLs: 2, SucceededURLs: 1,
		Failures: []domain.CacheWarmupFailure{{Path: "/missing.js", Detail: "origin returned 404"}}, CompletedAt: completedAt,
	}
	if err := agent.queueCacheWarmupResults([]domain.CacheWarmupResult{wanted}); err != nil {
		t.Fatal(err)
	}
	restarted := &Agent{Config: Config{StateDir: directory}}
	pending, err := restarted.pendingCacheWarmupResults()
	if err != nil || len(pending) != 1 || pending[0].WarmupID != wanted.WarmupID || pending[0].SucceededURLs != 1 {
		t.Fatalf("restarted pending results = %#v, %v", pending, err)
	}
	if err := restarted.acknowledgeCacheWarmupResults(pending); err != nil {
		t.Fatal(err)
	}
	pending, err = restarted.pendingCacheWarmupResults()
	if err != nil || len(pending) != 0 {
		t.Fatalf("acknowledged pending results = %#v, %v", pending, err)
	}
}

func TestCacheWarmupAcknowledgementDoesNotDropANewerResult(t *testing.T) {
	agent := &Agent{Config: Config{StateDir: t.TempDir()}}
	older := domain.CacheWarmupResult{
		WarmupID: "job", SiteID: "site", Status: domain.CacheWarmupSucceeded,
		AttemptedURLs: 1, SucceededURLs: 1, CompletedAt: time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC),
	}
	if err := agent.queueCacheWarmupResults([]domain.CacheWarmupResult{older}); err != nil {
		t.Fatal(err)
	}
	sent, err := agent.pendingCacheWarmupResults()
	if err != nil {
		t.Fatal(err)
	}
	newer := older
	newer.CompletedAt = newer.CompletedAt.Add(time.Second)
	if err := agent.queueCacheWarmupResults([]domain.CacheWarmupResult{newer}); err != nil {
		t.Fatal(err)
	}
	if err := agent.acknowledgeCacheWarmupResults(sent); err != nil {
		t.Fatal(err)
	}
	pending, err := agent.pendingCacheWarmupResults()
	if err != nil || len(pending) != 1 || !pending[0].CompletedAt.Equal(newer.CompletedAt) {
		t.Fatalf("pending result after stale acknowledgement = %#v, %v", pending, err)
	}
}

func TestCacheWarmupFailureDetailPreservesUTF8AtLimit(t *testing.T) {
	detail := cacheWarmupFailureDetail(errors.New(strings.Repeat("界", 400)))
	if len(detail) > 1024 || !utf8.ValidString(detail) {
		t.Fatalf("failure detail length=%d valid_utf8=%v", len(detail), utf8.ValidString(detail))
	}
}

func TestApplyRunsCacheWarmupAfterNginxAndReportsWarningsWithoutRollingBack(t *testing.T) {
	directory := t.TempDir()
	calls := 0
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory,
		NginxConfigPath: filepath.Join(directory, "nginx.conf"),
		CertificateDir:  filepath.Join(directory, "certs"),
		Runner:          &fakeRunner{},
		CacheWarmer: func(context.Context, domain.CacheWarmup) error {
			calls++
			return errors.New("origin returned 503")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	state := domain.DesiredState{
		Version: 1, NginxConfig: "# valid test configuration\n", PublicPorts: []int{},
		CacheWarmups: []domain.CacheWarmup{{
			ID: "11111111-1111-4111-8111-111111111111", SiteID: "site", Host: "cdn.example.test",
			Paths: []string{"/app.js"}, CreatedAt: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC),
		}},
	}
	if err := agent.apply(state); err != nil {
		t.Fatal(err)
	}
	report, _ := agent.heartbeatReport()
	if calls != 1 || report == nil || report.Status != domain.ApplySucceeded || !strings.Contains(report.Detail, "cache prewarm warning") {
		t.Fatalf("apply warmup calls=%d report=%#v", calls, report)
	}
	state.Version = 2
	if err := agent.apply(state); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("failed warmup was retried on the next configuration: %d calls", calls)
	}
}
