package edge

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
	if detail, err := agent.runCacheWarmups([]domain.CacheWarmup{job("job-a"), job("job-b")}); err == nil || detail != "cache prewarm completed 1 of 2 job(s)" {
		t.Fatalf("first prewarm = %q, %v", detail, err)
	}
	if _, err := agent.runCacheWarmups([]domain.CacheWarmup{job("job-a"), job("job-b")}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"job-a", "job-b"}) {
		t.Fatalf("completed jobs were retried: %#v", calls)
	}
	if _, err := agent.runCacheWarmups([]domain.CacheWarmup{job("job-b")}); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.runCacheWarmups([]domain.CacheWarmup{job("job-a")}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"job-a", "job-b", "job-a"}) {
		t.Fatalf("pruned job was not eligible again: %#v", calls)
	}
	if _, err := agent.runCacheWarmups(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.runCacheWarmups([]domain.CacheWarmup{job("job-a")}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"job-a", "job-b", "job-a", "job-a"}) {
		t.Fatalf("empty desired state did not prune completed jobs: %#v", calls)
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
