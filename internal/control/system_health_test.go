package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

func healthFixture(at time.Time) store.SystemHealthInputs {
	previous := at.Add(-time.Minute)
	expiry := at.Add(80 * 24 * time.Hour)
	site := domain.Site{ID: "site", Name: "CDN", Enabled: true, Published: true, IPv6Enabled: true, Nodes: []string{"a", "b"}}
	input := store.SystemHealthInputs{Sites: []store.HealthSite{{Draft: site, Published: &site, PublishedAt: &previous, ServingNotAfter: &expiry}}, DesiredVersions: map[string]int64{"a": 2, "b": 2}, NodeProbes: map[string]store.HealthProbe{}}
	for _, id := range []string{"a", "b"} {
		input.Nodes = append(input.Nodes, domain.Node{ID: id, Name: id, Status: domain.NodeActive, PublicIPv6: "2001:db8::1", LastHeartbeatAt: &at, AppliedVersion: 2})
		input.NodeProbes[id] = store.HealthProbe{NodeID: id, Eligible: true, CheckedAt: &at}
		for _, v6 := range []bool{false, true} {
			input.SiteProbes = append(input.SiteProbes, store.HealthProbe{NodeID: id, SiteID: "site", IPv6: v6, Eligible: true, CheckedAt: &at})
		}
	}
	return input
}

func findHealth(t *testing.T, snapshot domain.SystemHealthSnapshot, id string) domain.HealthCheck {
	t.Helper()
	for _, check := range snapshot.Checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("missing check %s", id)
	return domain.HealthCheck{}
}

func TestSystemHealthRules(t *testing.T) {
	at := time.Now().UTC()
	cases := []struct {
		name, id string
		want     domain.HealthState
		change   func(*store.SystemHealthInputs)
	}{
		{"fresh heartbeat", "node:a:heartbeat", domain.HealthHealthy, func(*store.SystemHealthInputs) {}},
		{"missing heartbeat", "node:a:heartbeat", domain.HealthUnknown, func(i *store.SystemHealthInputs) { i.Nodes[0].LastHeartbeatAt = nil }},
		{"future heartbeat", "node:a:heartbeat", domain.HealthUnknown, func(i *store.SystemHealthInputs) { v := at.Add(time.Minute); i.Nodes[0].LastHeartbeatAt = &v }},
		{"expired heartbeat", "node:a:heartbeat", domain.HealthCritical, func(i *store.SystemHealthInputs) { v := at.Add(-91 * time.Second); i.Nodes[0].LastHeartbeatAt = &v }},
		{"paused node", "node:a:heartbeat", domain.HealthDisabled, func(i *store.SystemHealthInputs) { i.Nodes[0].Status = domain.NodeDraining }},
		{"version drift", "node:a:configuration", domain.HealthWarning, func(i *store.SystemHealthInputs) { i.Nodes[0].AppliedVersion = 1 }},
		{"missing desired config", "node:a:configuration", domain.HealthUnknown, func(i *store.SystemHealthInputs) { delete(i.DesiredVersions, "a") }},
		{"node error", "node:a:configuration", domain.HealthWarning, func(i *store.SystemHealthInputs) { i.Nodes[0].LastError = "nginx failed" }},
		{"all probes healthy", "site:site:IPv4", domain.HealthHealthy, func(*store.SystemHealthInputs) {}},
		{"partial probe failure", "site:site:IPv4", domain.HealthWarning, func(i *store.SystemHealthInputs) { i.SiteProbes[0].Eligible = false }},
		{"all probes failed", "site:site:IPv4", domain.HealthCritical, func(i *store.SystemHealthInputs) { i.SiteProbes[0].Eligible = false; i.SiteProbes[2].Eligible = false }},
		{"missing probe is unknown", "site:site:IPv4", domain.HealthUnknown, func(i *store.SystemHealthInputs) { i.SiteProbes = nil }},
		{"new publication needs new probe", "site:site:IPv4", domain.HealthUnknown, func(i *store.SystemHealthInputs) { v := at.Add(time.Second); i.Sites[0].PublishedAt = &v }},
		{"unpublished draft does not disable serving site", "site:site:IPv4", domain.HealthHealthy, func(i *store.SystemHealthInputs) {
			i.Sites[0].Draft.Enabled = false
			i.Sites[0].Draft.Nodes = nil
			i.Sites[0].Draft.Published = false
		}},
		{"unpublished site", "site:site:IPv4", domain.HealthDisabled, func(i *store.SystemHealthInputs) { i.Sites[0].Published = nil }},
		{"no serving nodes", "site:site:IPv4", domain.HealthCritical, func(i *store.SystemHealthInputs) { i.Nodes = nil }},
		{"IPv6 failed independently", "site:site:IPv6", domain.HealthCritical, func(i *store.SystemHealthInputs) { i.SiteProbes[1].Eligible = false; i.SiteProbes[3].Eligible = false }},
		{"IPv6 disabled", "site:site:IPv6", domain.HealthDisabled, func(i *store.SystemHealthInputs) { i.Sites[0].Published.IPv6Enabled = false }},
		{"expired serving certificate", "site:site:certificate", domain.HealthCritical, func(i *store.SystemHealthInputs) { v := at.Add(-time.Hour); i.Sites[0].ServingNotAfter = &v }},
		{"renewed but not published", "site:site:certificate", domain.HealthCritical, func(i *store.SystemHealthInputs) {
			v := at.Add(time.Hour)
			fresh := at.Add(90 * 24 * time.Hour)
			i.Sites[0].ServingNotAfter = &v
			i.Sites[0].Certificate = &store.CertificateMetadata{NotAfter: &fresh, UpdatedAt: at}
		}},
		{"renewal window", "site:site:certificate", domain.HealthWarning, func(i *store.SystemHealthInputs) { v := at.Add(20 * 24 * time.Hour); i.Sites[0].ServingNotAfter = &v }},
		{"unknown serving expiry", "site:site:certificate", domain.HealthUnknown, func(i *store.SystemHealthInputs) { i.Sites[0].ServingNotAfter = nil }},
		{"failed certificate task", "site:site:certificate", domain.HealthWarning, func(i *store.SystemHealthInputs) {
			i.Tasks = []domain.DeploymentTask{{ID: "cert", SiteID: "site", Kind: "renew_certificate", Status: domain.TaskFailed, UpdatedAt: at}}
		}},
		{"new certificate supersedes failure", "site:site:certificate", domain.HealthHealthy, func(i *store.SystemHealthInputs) {
			i.Tasks = []domain.DeploymentTask{{ID: "cert", SiteID: "site", Kind: "renew_certificate", Status: domain.TaskFailed, UpdatedAt: at.Add(-time.Minute)}}
			i.Sites[0].Certificate = &store.CertificateMetadata{UpdatedAt: at}
		}},
		{"publication failed", "site:site:publication", domain.HealthWarning, func(i *store.SystemHealthInputs) {
			i.Tasks = []domain.DeploymentTask{{ID: "pub", SiteID: "site", Kind: "publish_site", Status: domain.TaskFailed, UpdatedAt: at}}
		}},
		{"publication overdue", "site:site:publication", domain.HealthWarning, func(i *store.SystemHealthInputs) {
			i.Tasks = []domain.DeploymentTask{{ID: "pub", SiteID: "site", Kind: "publish_site", Status: domain.TaskApplying, UpdatedAt: at.Add(-11 * time.Minute)}}
		}},
		{"upgrade failed", "node:a:upgrade", domain.HealthWarning, func(i *store.SystemHealthInputs) {
			i.Upgrades = []domain.NodeUpgradeTask{{ID: "upgrade", NodeID: "a", Status: domain.NodeUpgradeFailed, UpdatedAt: at}}
		}},
		{"paused rollout", "upgrade:rollout", domain.HealthWarning, func(i *store.SystemHealthInputs) {
			i.Rollout = &domain.NodeUpgradeRollout{State: "paused", UpdatedAt: at}
		}},
		{"stale rollout", "upgrade:rollout", domain.HealthUnknown, func(i *store.SystemHealthInputs) {
			i.Rollout = &domain.NodeUpgradeRollout{State: "running", UpdatedAt: at.Add(-4 * time.Minute)}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := healthFixture(at)
			tc.change(&input)
			snapshot := evaluateSystemHealth(input, at)
			check := findHealth(t, snapshot, tc.id)
			if check.State != tc.want {
				t.Fatalf("want %s, got %s: %s", tc.want, check.State, check.Summary)
			}
		})
	}
}

func TestSystemHealthBackupAndFreshness(t *testing.T) {
	at := time.Now().UTC()
	past := at.Add(-time.Hour)
	future := at.Add(time.Hour)
	status := BackupHealthStatus{State: "healthy", ObservedAt: at, LastSucceededAt: &past, ExpiresAt: &future, VerificationDueAt: &future, Verification: BackupVerificationStatus{State: OnlineRestoreVerified, LastVerifiedAt: &past, LastVerifiedSnapshotTime: &past}}
	for _, state := range []string{"healthy", "stale", "failed", "unknown", "disabled"} {
		status.State = state
		checks := backupHealthChecks(&status, at)
		want := map[string]domain.HealthState{"healthy": domain.HealthHealthy, "stale": domain.HealthWarning, "failed": domain.HealthWarning, "unknown": domain.HealthUnknown, "disabled": domain.HealthDisabled}[state]
		if checks[0].State != want {
			t.Fatalf("backup %s: %#v", state, checks)
		}
	}
	status.State = "healthy"
	status.LastSucceededAt = nil
	status.State = "running"
	if backupHealthChecks(&status, at)[0].State != domain.HealthUnknown {
		t.Fatal("first backup in progress treated as a successful backup")
	}
	status.State = "healthy"
	status.LastSucceededAt = &past
	status.Verification.State = OnlineRestoreFailed
	if backupHealthChecks(&status, at)[1].State != domain.HealthWarning {
		t.Fatal("old verification success hid current failure")
	}
	status.Verification.State = OnlineRestoreVerified
	status.VerificationDueAt = &past
	if backupHealthChecks(&status, at)[1].State != domain.HealthWarning {
		t.Fatal("expired verification treated as valid")
	}
	snapshot := domain.SystemHealthSnapshot{CollectedAt: &past, ValidUntil: &at, Checks: []domain.HealthCheck{{ID: "good", State: domain.HealthHealthy}, {ID: "bad", State: domain.HealthCritical}}}
	prepareSystemHealthResponse(&snapshot, at, false)
	if !snapshot.Stale || snapshot.State != domain.HealthCritical || findHealth(t, snapshot, "good").State != domain.HealthUnknown {
		t.Fatal("stale snapshot falsely healthy or hid known failure")
	}
	if snapshot.Nodes == nil || snapshot.Recoveries == nil {
		t.Fatal("API arrays are null")
	}
	for _, failure := range []bool{false, true} {
		snapshot = domain.SystemHealthSnapshot{CollectedAt: &at, ValidUntil: &future, Checks: []domain.HealthCheck{{ID: "good", State: domain.HealthHealthy}}}
		prepareSystemHealthResponse(&snapshot, at, failure)
		want := domain.HealthHealthy
		if failure {
			want = domain.HealthUnknown
		}
		if snapshot.State != want {
			t.Fatal("collection failure was hidden")
		}
	}
}

func TestSystemHealthCollectionAndAPIReadOnly(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.CreateInitialAdmin("hash", "secret"); err != nil {
		t.Fatal(err)
	}
	if err = db.CreateSession("admin", "session-token", "csrf-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	node, err := db.CreateNode("edge", "203.0.113.1")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Heartbeat(node.ID, 0, "", nil); err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: db}
	server.HealthManager = &HealthManager{Server: server, SiteProbe: func(context.Context, domain.Site, domain.Node) (bool, string) {
		panic("health endpoint started a probe")
	}}
	server.SystemHealth = &SystemHealthManager{Server: server}
	read := func() domain.SystemHealthSnapshot {
		t.Helper()
		response := requestSiteResponse(t, server, http.MethodGet, "/api/system/health", nil)
		if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("API: %d %s", response.Code, response.Body.String())
		}
		var snapshot domain.SystemHealthSnapshot
		if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	initial := read()
	if initial.State != domain.HealthUnknown || !initial.Stale {
		t.Fatal("uncollected state must be unknown")
	}
	if err = server.SystemHealth.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := read()
	if snapshot.Stale || len(snapshot.Checks) < 4 {
		t.Fatalf("collection: %#v", snapshot)
	}
	first := snapshot.CollectedAt
	for i := 0; i < 3; i++ {
		if !read().CollectedAt.Equal(*first) {
			t.Fatal("API triggered collection")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = server.SystemHealth.Collect(ctx); err == nil {
		t.Fatal("cancelled collection succeeded")
	}
	failed := read()
	if !failed.CollectedAt.Equal(*first) || findHealth(t, failed, "control:collection").State != domain.HealthUnknown {
		t.Fatal("failed collection replaced the saved snapshot")
	}
	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/system/health", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("health endpoint exposed without auth: %d", unauthorized.Code)
	}
	// The existing process liveness endpoint does not depend on health snapshots.
	liveness := httptest.NewRecorder()
	server.Handler().ServeHTTP(liveness, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if liveness.Code != http.StatusOK {
		t.Fatalf("liveness changed: %d", liveness.Code)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := server.SystemHealth.Collect(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(read().Recoveries) != 0 {
		t.Fatal("transient read failure created false recoveries")
	}
}

func TestSystemHealthManagerRunStopsWithContext(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager := &SystemHealthManager{Server: &Server{Store: db}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { manager.Run(ctx); close(done) }()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("initial collection did not run")
		case <-ticker.C:
			if _, err := db.SystemHealthSnapshot(context.Background()); err == nil {
				cancel()
				select {
				case <-done:
					return
				case <-time.After(time.Second):
					t.Fatal("manager did not stop")
				}
			}
		}
	}
}
