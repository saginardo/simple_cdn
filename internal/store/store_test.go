package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestEnrollmentTokenSingleUse(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	node, err := store.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateEnrollmentToken(node.ID, "one-time-token", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	nodeID, err := store.ConsumeEnrollmentToken("one-time-token")
	if err != nil || nodeID != node.ID {
		t.Fatalf("unexpected consume: %q %v", nodeID, err)
	}
	if _, err := store.ConsumeEnrollmentToken("one-time-token"); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("expected token error, got %v", err)
	}
}

func TestNodeStateNginxFragmentsRoundTrip(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-fragments", "203.0.113.60")
	if err != nil {
		t.Fatal(err)
	}
	state := domain.DesiredState{
		Version: 3, NginxConfig: "legacy HTTP", NginxStreamConfig: "legacy stream",
		NginxMainConfig: "worker_processes auto;", NginxEventsConfig: "worker_connections 4096;",
		NginxFragments: &domain.NginxConfigFragments{
			HTTPBase: "HTTP base", HTTPSites: []domain.NginxConfigFragment{{Name: "site-a.conf", Content: "HTTP site"}},
			StreamBase: "stream base", StreamSites: []domain.NginxConfigFragment{{Name: "site-a.conf", Content: "stream site"}},
		},
		PublicPorts: []int{80, 443}, PublicUDPPorts: []int{443}, CacheMaxBytes: 9 << 30,
	}
	if err := database.SaveNodeState(node.ID, state, nil); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := database.NodeState(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.NginxFragments == nil || loaded.NginxFragments.HTTPBase != "HTTP base" || loaded.NginxMainConfig != "worker_processes auto;" ||
		loaded.NginxEventsConfig != "worker_connections 4096;" || loaded.CacheMaxBytes != 9<<30 ||
		len(loaded.NginxFragments.HTTPSites) != 1 || loaded.NginxFragments.StreamSites[0].Content != "stream site" ||
		len(loaded.PublicUDPPorts) != 1 || loaded.PublicUDPPorts[0] != 443 {
		t.Fatalf("stored Nginx fragments = %#v", loaded.NginxFragments)
	}
}

func TestNodeCacheLimitInheritsAndRoundTripsOverride(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-cache", "203.0.113.61")
	if err != nil {
		t.Fatal(err)
	}
	if node.CacheMaxSizeGB != nil {
		t.Fatalf("new node cache override = %#v, want inheritance", node.CacheMaxSizeGB)
	}
	override := 12
	updated, err := database.SetNodeCacheMaxSizeGB(node.ID, &override)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CacheMaxSizeGB == nil || *updated.CacheMaxSizeGB != override {
		t.Fatalf("updated cache override = %#v", updated.CacheMaxSizeGB)
	}
	loaded, err := database.GetNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CacheMaxSizeGB == nil || *loaded.CacheMaxSizeGB != override {
		t.Fatalf("stored cache override = %#v", loaded.CacheMaxSizeGB)
	}
	invalid := domain.MaxCacheMaxSizeGB + 1
	if _, err := database.SetNodeCacheMaxSizeGB(node.ID, &invalid); err == nil {
		t.Fatal("accepted node cache override above maximum")
	}
	cleared, err := database.SetNodeCacheMaxSizeGB(node.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.CacheMaxSizeGB != nil {
		t.Fatalf("cleared cache override = %#v", cleared.CacheMaxSizeGB)
	}
}

func TestNodeNginxCapacityDefaultsAndRoundTrips(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-capacity", "203.0.113.63")
	if err != nil {
		t.Fatal(err)
	}
	if node.NginxCapacity != domain.DefaultNginxCapacity() {
		t.Fatalf("default Nginx capacity = %#v", node.NginxCapacity)
	}
	updated, err := database.SetNodeNginxCapacity(node.ID, domain.NginxCapacity{
		WorkerProcesses: 4, WorkerConnections: 8192, WorkerRlimitNoFile: 16384,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.NginxCapacity.WorkerProcesses != 4 || updated.NginxCapacity.WorkerConnections != 8192 || updated.NginxCapacity.WorkerRlimitNoFile != 16384 {
		t.Fatalf("updated Nginx capacity = %#v", updated.NginxCapacity)
	}
	if _, err := database.SetNodeNginxCapacity(node.ID, domain.NginxCapacity{WorkerConnections: 8192, WorkerRlimitNoFile: 4096}); err == nil {
		t.Fatal("accepted an Nginx file limit below worker connections")
	}
}

func TestSiteCacheLimitIsDiscarded(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-site-cache", "203.0.113.62")
	if err != nil {
		t.Fatal(err)
	}
	legacyOverride := 12
	site, err := database.CreateSite(domain.Site{
		Name:           "site-cache",
		Domains:        []string{"cache.example.test"},
		Nodes:          []string{node.ID},
		PrimaryOrigin:  domain.Origin{URL: "https://origin.example.test", Enabled: true},
		CacheMaxSizeGB: &legacyOverride,
		Enabled:        true,
	}, "zone-1")
	if err != nil {
		t.Fatal(err)
	}
	if site.CacheMaxSizeGB != nil {
		t.Fatalf("created site retained cache limit %#v", site.CacheMaxSizeGB)
	}
	var stored sql.NullInt64
	if err := database.db.QueryRow(`SELECT cache_max_size_gb FROM sites WHERE id = ?`, site.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.Valid {
		t.Fatalf("stored site retained cache limit %d", stored.Int64)
	}
}

func TestHealthHysteresis(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	node, err := store.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		health, err := store.RecordNodeHealth(node.ID, true, "")
		if err != nil || health.DNSEligible {
			t.Fatalf("node became eligible too early: %#v %v", health, err)
		}
	}
	health, err := store.RecordNodeHealth(node.ID, true, "")
	if err != nil || !health.DNSEligible {
		t.Fatalf("node did not become eligible: %#v %v", health, err)
	}
	for range 2 {
		health, err = store.RecordNodeHealth(node.ID, false, "timeout")
		if err != nil || !health.DNSEligible {
			t.Fatalf("node dropped too early: %#v %v", health, err)
		}
	}
	health, err = store.RecordNodeHealth(node.ID, false, "timeout")
	if err != nil || health.DNSEligible {
		t.Fatalf("node did not drop after three failures: %#v %v", health, err)
	}
}

func TestSiteNodeHealthHysteresis(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	node, err := store.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	site, err := store.CreateSite(domain.Site{
		Name: "site-1", Domains: []string{"site.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		health, err := store.RecordSiteNodeHealth(site.ID, node.ID, true, "")
		if err != nil || health.DNSEligible {
			t.Fatalf("site node became eligible too early: %#v %v", health, err)
		}
	}
	health, err := store.RecordSiteNodeHealth(site.ID, node.ID, true, "")
	if err != nil || !health.DNSEligible {
		t.Fatalf("site node did not become eligible: %#v %v", health, err)
	}
	for range 2 {
		health, err = store.RecordSiteNodeHealth(site.ID, node.ID, false, "TLS mismatch")
		if err != nil || !health.DNSEligible {
			t.Fatalf("site node dropped too early: %#v %v", health, err)
		}
	}
	health, err = store.RecordSiteNodeHealth(site.ID, node.ID, false, "TLS mismatch")
	if err != nil || health.DNSEligible || health.LastError != "TLS mismatch" {
		t.Fatalf("site node did not drop after three failures: %#v %v", health, err)
	}
	loaded, err := store.SiteNodeHealth(site.ID, node.ID)
	if err != nil || loaded.DNSEligible || loaded.ConsecutiveFailures != 3 || loaded.LastCheckedAt == nil {
		t.Fatalf("stored site-node health = %#v, %v", loaded, err)
	}
}

func TestCreateNodeRejectsNonPublicAddress(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "0.0.0.0", "224.0.0.1"} {
		if _, err := store.CreateNode("edge-"+address, address); err == nil {
			t.Fatalf("expected %s to be rejected", address)
		}
	}
}

func TestRevokedNodeCannotReceiveEnrollmentToken(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	node, err := store.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetNodeStatus(node.ID, "revoked"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateEnrollmentToken(node.ID, "token", time.Now().Add(time.Minute)); err == nil {
		t.Fatal("expected enrollment for revoked node to fail")
	}
}

func TestRevokingNodeInvalidatesExistingEnrollmentToken(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	node, err := store.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateEnrollmentToken(node.ID, "token", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetNodeStatus(node.ID, "revoked"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConsumeEnrollmentToken("token"); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("expected revoked token to be invalid, got %v", err)
	}
}

func TestRevokingNodeInvalidatesItsClientCertificate(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	node, err := store.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetNodeCertificate(node.ID, "sha256:edge-cert"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetNodeStatus(node.ID, "revoked"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.NodeIDByFingerprint("sha256:edge-cert"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected revoked certificate to be invalid, got %v", err)
	}
	if err := store.SetNodeStatus(node.ID, "active"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.NodeIDByFingerprint("sha256:edge-cert"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old certificate revived after reactivation: %v", err)
	}
}

func TestRevokedNodeCannotReceiveCertificateFingerprint(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	node, err := store.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetNodeStatus(node.ID, "revoked"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetNodeCertificate(node.ID, "sha256:late-cert"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected revoked node certificate update to fail, got %v", err)
	}
}

func TestOpeningStoreFailsInterruptedCertificateTasks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	task, err := first.CreateTask("issue_certificate", "site", "waiting for DNS-01 validation")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.UpdateTask(task.ID, domain.TaskApplying, "waiting for DNS-01 validation"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	updated, err := second.GetTask(task.ID)
	if err != nil || updated.Status != domain.TaskFailed || !strings.Contains(updated.Detail, "restart") {
		t.Fatalf("recovered task = %#v, err=%v", updated, err)
	}
}

func TestHasSuccessfulPublishAfterCertificateTask(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	certificateTask, err := store.CreateTask("issue_certificate", "site", "certificate stored; publish the site to deploy it")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTask(certificateTask.ID, domain.TaskSucceeded, certificateTask.Detail); err != nil {
		t.Fatal(err)
	}
	completedCertificateTask, err := store.GetTask(certificateTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	published, err := store.HasSuccessfulPublishAfter("site", completedCertificateTask.UpdatedAt)
	if err != nil || published {
		t.Fatalf("publish before deployment task = %t, err=%v", published, err)
	}
	time.Sleep(time.Millisecond)
	publishTask, err := store.CreateTask("publish_site", "site", "configuration available to assigned nodes")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTask(publishTask.ID, domain.TaskSucceeded, publishTask.Detail); err != nil {
		t.Fatal(err)
	}
	published, err = store.HasSuccessfulPublishAfter("site", completedCertificateTask.UpdatedAt)
	if err != nil || !published {
		t.Fatalf("publish after certificate task = %t, err=%v", published, err)
	}
}

func TestPublishTaskTimesOutUnconfirmedNodes(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	node, err := store.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	task, created, err := store.CreateOrGetActivePublishTask("site", time.Now().Add(-time.Second))
	if err != nil || !created {
		t.Fatalf("create publish task: %#v %t %v", task, created, err)
	}
	if err := store.UpdateTask(task.ID, domain.TaskApplying, "waiting for edge"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreatePublishTaskNodes(task.ID, []PublishTaskNode{{NodeID: node.ID, TargetVersion: 3}}); err != nil {
		t.Fatal(err)
	}
	status, err := store.PublishStatus("site")
	if err != nil {
		t.Fatal(err)
	}
	if status.Task == nil || status.Task.Status != domain.TaskFailed || len(status.Nodes) != 1 || status.Nodes[0].Status != domain.PublishNodeTimedOut || status.Nodes[0].ErrorCode != "confirmation_timeout" {
		t.Fatalf("unexpected timeout status: %#v", status)
	}
}

func TestPublishApplyReportConfirmsAnOlderTargetVersion(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	node, err := store.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	task, created, err := store.CreateOrGetActivePublishTask("site", time.Now().Add(time.Minute))
	if err != nil || !created {
		t.Fatalf("create task: %#v %t %v", task, created, err)
	}
	if err := store.UpdateTask(task.ID, domain.TaskApplying, "waiting for edge"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreatePublishTaskNodes(task.ID, []PublishTaskNode{{NodeID: node.ID, TargetVersion: 4}}); err != nil {
		t.Fatal(err)
	}
	report := &domain.ApplyReport{Version: 5, Status: domain.ApplySucceeded, Detail: "Nginx is listening on TCP 80 and TCP 443"}
	if err := store.Heartbeat(node.ID, 5, "", report); err != nil {
		t.Fatal(err)
	}
	status, err := store.PublishStatus("site")
	if err != nil {
		t.Fatal(err)
	}
	if status.Task == nil || status.Task.Status != domain.TaskSucceeded || len(status.Nodes) != 1 || status.Nodes[0].Detail != report.Detail {
		t.Fatalf("higher-version apply report did not preserve detail: %#v", status)
	}
}

func TestSiteReadWriteTimeoutRoundTripAndRetiresStreamPaths(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	node, err := store.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateSite(domain.Site{
		Name: "streaming", Domains: []string{"stream.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin:           domain.Origin{URL: "https://origin.example.test", Enabled: true},
		StreamPaths:             []string{"/ws/", "/events"},
		ReadWriteTimeoutSeconds: 1800,
		Enabled:                 true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	loaded, _, err := store.GetSite(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ReadWriteTimeoutSeconds != 1800 {
		t.Fatalf("stored read/write timeout = %d", loaded.ReadWriteTimeoutSeconds)
	}
	if loaded.StreamPaths == nil || len(loaded.StreamPaths) != 0 {
		t.Fatalf("retired stream paths should be empty: %#v", loaded.StreamPaths)
	}
	loaded.ReadWriteTimeoutSeconds = 3600
	updated, err := store.UpdateSite(loaded, "zone")
	if err != nil {
		t.Fatal(err)
	}
	if updated.ReadWriteTimeoutSeconds != 3600 {
		t.Fatalf("updated read/write timeout = %d", updated.ReadWriteTimeoutSeconds)
	}
}

func TestOpenClearsRetiredStreamPathsFromExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	created, err := database.CreateSite(domain.Site{
		Name: "legacy-streaming", Domains: []string{"legacy-stream.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`UPDATE sites SET stream_paths_json = '["/events","/ws"]' WHERE id = ?`, created.ID); err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`DELETE FROM schema_migrations`); err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	loaded, _, err := migrated.GetSite(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.StreamPaths == nil || len(loaded.StreamPaths) != 0 {
		t.Fatalf("legacy stream paths were not cleared: %#v", loaded.StreamPaths)
	}
	var stored string
	if err := migrated.db.QueryRow(`SELECT stream_paths_json FROM sites WHERE id = ?`, created.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "[]" {
		t.Fatalf("retired stream paths remain in SQLite: %s", stored)
	}
}

func TestSitePassthroughRoundTripAndCacheGeneration(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	node, err := store.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateSite(domain.Site{
		Name: "passthrough", Domains: []string{"stream.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true},
		Passthrough:   true, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	loaded, zoneID, err := store.GetSite(created.ID)
	if err != nil || !loaded.Passthrough || zoneID != "zone" {
		t.Fatalf("unexpected stored passthrough site: %#v %q %v", loaded, zoneID, err)
	}
	if _, err := store.InvalidateSiteCache(created.ID); !errors.Is(err, ErrCacheDisabled) {
		t.Fatalf("expected cache-disabled error, got %v", err)
	}
	loaded.Passthrough = false
	updated, err := store.UpdateSite(loaded, zoneID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Passthrough || updated.CacheGeneration != created.CacheGeneration+1 {
		t.Fatalf("unexpected cache generation after disabling passthrough: %#v", updated)
	}
	invalidated, err := store.InvalidateSiteCache(updated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if invalidated.CacheGeneration != updated.CacheGeneration+1 {
		t.Fatalf("cache invalidation did not advance generation: %#v", invalidated)
	}
	if invalidated.Published {
		t.Fatalf("cache invalidation was not marked pending publication: %#v", invalidated)
	}
}

func TestSiteClientMaxBodySizeRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	node, err := store.CreateNode("edge-1", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateSite(domain.Site{
		Name: "large-requests", Domains: []string{"api.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin:       domain.Origin{URL: "https://origin.example.test", Enabled: true},
		ClientMaxBodySizeMB: 1024, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	loaded, zoneID, err := store.GetSite(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ClientMaxBodySizeMB != 1024 {
		t.Fatalf("stored client max body size = %d", loaded.ClientMaxBodySizeMB)
	}
	loaded.ClientMaxBodySizeMB = 256
	updated, err := store.UpdateSite(loaded, zoneID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ClientMaxBodySizeMB != 256 || updated.CacheGeneration != created.CacheGeneration || updated.ConfigVersion != created.ConfigVersion+1 {
		t.Fatalf("unexpected updated site: %#v", updated)
	}
}

func TestPublishedSnapshotAndDomainClaimsSurviveDraftChanges(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-1", "203.0.113.40")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "snapshot", Domains: []string{"old.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://old-origin.example.test", Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	oldCertificate := []byte("encrypted-old-certificate")
	oldKey := []byte("encrypted-old-key")
	if err := database.SaveCertificate(site.ID, oldCertificate, oldKey, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := database.MarkSitePublished(site.ID); err != nil {
		t.Fatal(err)
	}
	draft, zoneID, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	draft.Domains = []string{"draft.example.test"}
	draft.PrimaryOrigin.URL = "https://draft-origin.example.test"
	draft, err = database.UpdateSite(draft, zoneID)
	if err != nil {
		t.Fatal(err)
	}
	newCertificate := []byte("encrypted-new-certificate")
	newKey := []byte("encrypted-new-key")
	if err := database.SaveCertificate(site.ID, newCertificate, newKey, nil); err != nil {
		t.Fatal(err)
	}
	publication, err := database.SitePublication(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(publication.Site.Domains) != 1 || publication.Site.Domains[0] != "old.example.test" || publication.Site.PrimaryOrigin.URL != "https://old-origin.example.test" {
		t.Fatalf("published site changed with its draft: %#v", publication.Site)
	}
	if string(publication.CertificateCiphertext) != string(oldCertificate) || string(publication.KeyCiphertext) != string(oldKey) {
		t.Fatal("published certificate changed with the draft certificate")
	}
	for index, domainName := range []string{"old.example.test", "draft.example.test"} {
		_, err := database.CreateSite(domain.Site{
			Name: fmt.Sprintf("conflict-%d", index), Domains: []string{domainName}, Nodes: []string{node.ID},
			PrimaryOrigin: domain.Origin{URL: "https://other-origin.example.test", Enabled: true}, Enabled: true,
		}, "other-zone")
		if err == nil {
			t.Fatalf("domain %s was not reserved", domainName)
		}
	}
	if _, err := database.MarkSitePublished(site.ID); err != nil {
		t.Fatal(err)
	}
	publication, err = database.SitePublication(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if publication.Site.Domains[0] != "draft.example.test" || string(publication.CertificateCiphertext) != string(newCertificate) {
		t.Fatalf("draft was not promoted: %#v", publication)
	}
	if _, err := database.CreateSite(domain.Site{
		Name: "released", Domains: []string{"old.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://released-origin.example.test", Enabled: true}, Enabled: true,
	}, "released-zone"); err != nil {
		t.Fatalf("old published domain was not released after promotion: %v", err)
	}
}

func TestCommitSitePublicationRejectsAChangedDraftAtomically(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-1", "203.0.113.41")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "changing", Domains: []string{"changing.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	expectedVersion := site.ConfigVersion
	site.Name = "changed"
	if _, err := database.UpdateSite(site, "zone"); err != nil {
		t.Fatal(err)
	}
	state := domain.DesiredState{Version: 1, NginxConfig: "must-not-commit"}
	_, err = database.CommitSitePublication(site.ID, expectedVersion, "", []NodeStateUpdate{{NodeID: node.ID, State: state}}, nil)
	if !errors.Is(err, ErrSiteChanged) {
		t.Fatalf("stale publication error = %v", err)
	}
	if _, _, err := database.NodeState(node.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale node state was committed: %v", err)
	}
	if _, err := database.SitePublication(site.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale site snapshot was committed: %v", err)
	}
}

func TestOpenBackfillsPublishedSiteSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("edge-1", "203.0.113.42")
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "legacy-published", Domains: []string{"legacy-published.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	certificate := []byte("legacy-encrypted-certificate")
	key := []byte("legacy-encrypted-key")
	if err := database.SaveCertificate(site.ID, certificate, key, nil); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if _, err := database.MarkSitePublished(site.ID); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`DROP TABLE site_publications`); err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`DELETE FROM schema_migrations`); err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	publication, err := migrated.SitePublication(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if publication.Site.ID != site.ID || publication.Site.Domains[0] != site.Domains[0] || string(publication.CertificateCiphertext) != string(certificate) || string(publication.KeyCiphertext) != string(key) {
		t.Fatalf("backfilled publication = %#v", publication)
	}
}

func TestLegacyPendingPublicationBlocksOtherSitesUntilRepublished(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-1", "203.0.113.45")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "legacy-pending", Domains: []string{"legacy-pending.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	task, err := database.CreateTask("publish_site", site.ID, "legacy publication")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateTask(task.ID, domain.TaskSucceeded, task.Detail); err != nil {
		t.Fatal(err)
	}
	if err := database.CheckPublicationMigrationSafety("another-site"); err == nil || !strings.Contains(err.Error(), site.Name) {
		t.Fatalf("legacy pending publication was not blocked: %v", err)
	}
	if err := database.CheckPublicationMigrationSafety(site.ID); err != nil {
		t.Fatalf("site must be allowed to create its own snapshot: %v", err)
	}
	if _, err := database.MarkSitePublished(site.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CheckPublicationMigrationSafety("another-site"); err != nil {
		t.Fatalf("snapshot did not clear migration block: %v", err)
	}
}

func TestOpenMigratesSiteColumnsForExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE sites (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  zone_id TEXT NOT NULL,
  domains_json TEXT NOT NULL,
  node_ids_json TEXT NOT NULL,
  primary_origin_json TEXT NOT NULL,
  backup_origin_json TEXT,
  cache_generation INTEGER NOT NULL DEFAULT 1,
  config_version INTEGER NOT NULL DEFAULT 1,
  published INTEGER NOT NULL DEFAULT 0,
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
)`)
	if err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE nodes (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  public_ipv4 TEXT NOT NULL UNIQUE,
  status TEXT NOT NULL,
  cert_fingerprint TEXT,
  last_heartbeat_at TEXT,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
)`)
	if err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	_, err = legacy.Exec(`INSERT INTO nodes(id, name, public_ipv4, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"legacy-node", "legacy-node", "203.0.113.79", domain.NodePending, stamp(time.Now()), stamp(time.Now()))
	if err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	_, err = legacy.Exec(`INSERT INTO sites(id, name, zone_id, domains_json, node_ids_json, primary_origin_json, cache_generation, config_version, published, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, 1, 1, 0, 1, ?, ?)`,
		"legacy-site", "legacy", "zone", `["legacy.example.test"]`, `["node-1"]`, `{"url":"https://origin.example.test","host_header":"origin.example.test","enabled":true}`, stamp(time.Now()), stamp(time.Now()))
	if err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	rows, err := migrated.db.Query(`PRAGMA table_info(sites)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "stream_paths_json" || name == "passthrough" || name == "client_max_body_size_mb" || name == "client_keepalive_timeout_seconds" || name == "read_write_timeout_seconds" {
			found[name] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found["stream_paths_json"] || !found["passthrough"] || !found["client_max_body_size_mb"] || !found["client_keepalive_timeout_seconds"] || !found["read_write_timeout_seconds"] {
		t.Fatalf("site columns were not added to legacy table: %#v", found)
	}
	site, _, err := migrated.GetSite("legacy-site")
	if err != nil || site.Passthrough || site.ClientMaxBodySizeMB != domain.DefaultClientMaxBodySizeMB || site.ClientKeepaliveTimeoutSeconds != domain.DefaultClientKeepaliveTimeoutSeconds || site.ReadWriteTimeoutSeconds != domain.DefaultReadWriteTimeoutSeconds || site.PrimaryOrigin.TLSServerName != "" {
		t.Fatalf("legacy site defaults: passthrough=%t client_max_body_size_mb=%d client_keepalive_timeout_seconds=%d read_write_timeout_seconds=%d tls_server_name=%q err=%v", site.Passthrough, site.ClientMaxBodySizeMB, site.ClientKeepaliveTimeoutSeconds, site.ReadWriteTimeoutSeconds, site.PrimaryOrigin.TLSServerName, err)
	}
	node, err := migrated.GetNode("legacy-node")
	if err != nil || node.NginxCapacity != domain.DefaultNginxCapacity() {
		t.Fatalf("legacy node capacity = %#v, err=%v", node.NginxCapacity, err)
	}
}
