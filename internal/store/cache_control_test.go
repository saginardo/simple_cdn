package store

import (
	"path/filepath"
	"testing"

	"simple_cdn/internal/domain"
)

func TestScopedCacheInvalidationPersistsRulesWarmupsAndFullReset(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-cache", "203.0.113.81")
	if err != nil {
		t.Fatal(err)
	}
	created, err := database.CreateSite(domain.Site{
		Name: "cache", Domains: []string{"cdn.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin:        domain.Origin{URL: "https://origin.example.test", Enabled: true},
		RequestBodyBuffering: true, OriginResponseBuffering: true,
		DynamicCompressionEnabled: true, CompressionExcludedMIMETypes: domain.DefaultCompressionExcludedMIMETypes(),
		Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	urlInvalidated, err := database.InvalidateSiteCacheScope(created.ID, domain.CacheInvalidationURL, "/app.js?v=2", []string{"/app.js?v=2"})
	if err != nil {
		t.Fatal(err)
	}
	if urlInvalidated.CacheGeneration != created.CacheGeneration || len(urlInvalidated.CacheInvalidations) != 1 ||
		urlInvalidated.CacheInvalidations[0].Scope != domain.CacheInvalidationURL ||
		urlInvalidated.CacheInvalidations[0].Generation != urlInvalidated.ConfigVersion || len(urlInvalidated.CacheWarmups) != 1 {
		t.Fatalf("URL invalidation = %#v", urlInvalidated)
	}
	warmup := urlInvalidated.CacheWarmups[0]
	if warmup.SiteID != created.ID || warmup.Host != "cdn.example.test" || len(warmup.Paths) != 1 || warmup.Paths[0] != "/app.js?v=2" || warmup.ID == "" || warmup.CreatedAt.IsZero() {
		t.Fatalf("URL prewarm job = %#v", warmup)
	}

	prefixInvalidated, err := database.InvalidateSiteCacheScope(created.ID, domain.CacheInvalidationPrefix, "/assets/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(prefixInvalidated.CacheInvalidations) != 2 || prefixInvalidated.CacheInvalidations[1].Scope != domain.CacheInvalidationPrefix {
		t.Fatalf("prefix invalidation = %#v", prefixInvalidated.CacheInvalidations)
	}
	prefixInvalidated.CacheInvalidations = nil
	prefixInvalidated.CacheWarmups = nil
	updated, err := database.UpdateSite(prefixInvalidated, "zone")
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.CacheInvalidations) != 2 || len(updated.CacheWarmups) != 1 {
		t.Fatalf("general site update changed managed cache operations: %#v", updated)
	}

	fullyInvalidated, err := database.InvalidateSiteCache(updated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fullyInvalidated.CacheGeneration != updated.CacheGeneration+1 || len(fullyInvalidated.CacheInvalidations) != 0 {
		t.Fatalf("full invalidation = %#v", fullyInvalidated)
	}
}
