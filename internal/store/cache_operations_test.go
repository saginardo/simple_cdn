package store

import (
	"path/filepath"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestCacheOperationHistoryAndNodeSnapshotSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("edge-history", "203.0.113.91")
	if err != nil {
		t.Fatal(err)
	}
	capabilities := []string{domain.EdgeCapabilityCacheControl, domain.EdgeCapabilityCacheWarmupResults}
	if err := database.SetNodeCapabilities(node.ID, capabilities); err != nil {
		t.Fatal(err)
	}
	if err := database.Heartbeat(node.ID, 0, "", nil); err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "history", Domains: []string{"history.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin:        domain.Origin{URL: "https://origin.example.test", Enabled: true},
		RequestBodyBuffering: true, OriginResponseBuffering: true, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := database.CreateCacheInvalidationOperation(CacheOperationInput{
		SiteID: site.ID, Scope: domain.CacheInvalidationURL, Target: "/app.js", PrewarmPaths: []string{"/app.js"},
		Actor: "admin", RemoteAddr: "192.0.2.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	operations, err := reopened.ListCacheOperations(site.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].ID != created.ID || operations[0].SiteName != site.Name ||
		operations[0].Actor != "admin" || operations[0].RemoteAddr != "192.0.2.10" ||
		len(operations[0].Nodes) != 1 || operations[0].Nodes[0].NodeID != node.ID ||
		operations[0].Nodes[0].WarmupStatus != domain.CacheWarmupPending {
		t.Fatalf("reopened cache operation history = %#v", operations)
	}
}

func TestCacheOperationPresentationIncludesWarmupResults(t *testing.T) {
	task := &domain.DeploymentTask{
		Status: domain.TaskSucceeded, Detail: "configuration applied", UpdatedAt: time.Now().UTC(),
	}
	operation := domain.CacheOperation{
		PrewarmPaths: []string{"/one.js", "/two.js"},
		Nodes: []domain.CacheOperationNode{{
			ConfigurationStatus: domain.CacheConfigurationSucceeded,
			WarmupStatus:        domain.CacheWarmupPartial,
			AttemptedURLs:       2,
			SucceededURLs:       1,
		}},
	}
	status, _, completedAt := cacheOperationPresentation(operation, task)
	if status != domain.CacheOperationPartial || completedAt == nil {
		t.Fatalf("partial warmup presentation = %s, %v", status, completedAt)
	}

	operation.Nodes[0].WarmupStatus = domain.CacheWarmupPending
	status, _, completedAt = cacheOperationPresentation(operation, task)
	if status != domain.CacheOperationApplying || completedAt != nil {
		t.Fatalf("pending warmup presentation = %s, %v", status, completedAt)
	}

	staleTask := *task
	staleTask.UpdatedAt = time.Now().UTC().Add(-3 * time.Minute)
	status, _, completedAt = cacheOperationPresentation(operation, &staleTask)
	if status != domain.CacheOperationPartial || completedAt == nil ||
		operation.Nodes[0].WarmupStatus != domain.CacheWarmupUnreported {
		t.Fatalf("stale warmup presentation = %s, %v, %#v", status, completedAt, operation.Nodes[0])
	}

	operation.Nodes[0].WarmupStatus = domain.CacheWarmupSucceeded
	operation.Nodes[0].SucceededURLs = 2
	status, _, completedAt = cacheOperationPresentation(operation, task)
	if status != domain.CacheOperationSucceeded || completedAt == nil {
		t.Fatalf("successful warmup presentation = %s, %v", status, completedAt)
	}
}

func TestInitialCacheWarmupStatusDistinguishesMixedVersionNodes(t *testing.T) {
	current := []string{domain.EdgeCapabilityCacheControl, domain.EdgeCapabilityCacheWarmupResults}
	legacy := []string{domain.EdgeCapabilityCacheControl}
	for _, test := range []struct {
		name         string
		status       domain.NodeStatus
		capabilities []string
		requested    bool
		want         domain.CacheWarmupStatus
	}{
		{"not requested", domain.NodeActive, current, false, domain.CacheWarmupNotRequested},
		{"inactive", domain.NodePending, current, true, domain.CacheWarmupNotTargeted},
		{"unsupported", domain.NodeActive, nil, true, domain.CacheWarmupUnsupported},
		{"legacy result protocol", domain.NodeActive, legacy, true, domain.CacheWarmupUnreported},
		{"structured result protocol", domain.NodeActive, current, true, domain.CacheWarmupPending},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := initialCacheWarmupStatus(test.status, test.capabilities, test.requested); got != test.want {
				t.Fatalf("initial cache warmup status = %s, want %s", got, test.want)
			}
		})
	}
}
