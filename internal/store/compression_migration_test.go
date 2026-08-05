package store

import (
	"path/filepath"
	"testing"

	"simple_cdn/internal/domain"
)

func TestCompressionAndCacheControlMigrationBackfillsSafeDefaults(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-compression", "203.0.113.82")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "legacy", Domains: []string{"legacy.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin:           domain.Origin{URL: "https://origin.example.test", Enabled: true},
		OriginResponseBuffering: true, DynamicCompressionEnabled: true, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SaveNodeState(node.ID, domain.DesiredState{Version: 1}, nil); err != nil {
		t.Fatal(err)
	}
	for _, column := range []struct{ table, name string }{
		{"sites", "dynamic_compression_enabled"},
		{"sites", "compression_excluded_mime_types_json"},
		{"sites", "cache_invalidations_json"},
		{"sites", "cache_warmups_json"},
		{"node_states", "cache_warmups_json"},
	} {
		if _, err := database.db.Exec(`ALTER TABLE ` + column.table + ` DROP COLUMN ` + column.name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.db.Exec(`DELETE FROM schema_migrations WHERE version >= 31`); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.DynamicCompressionEnabled || len(loaded.CompressionExcludedMIMETypes) != 1 || loaded.CompressionExcludedMIMETypes[0] != "text/event-stream" ||
		len(loaded.CacheInvalidations) != 0 || len(loaded.CacheWarmups) != 0 {
		t.Fatalf("migrated site defaults = %#v", loaded)
	}
	state, _, err := database.NodeState(node.ID)
	if err != nil || len(state.CacheWarmups) != 0 {
		t.Fatalf("migrated node state = %#v, %v", state, err)
	}
}
