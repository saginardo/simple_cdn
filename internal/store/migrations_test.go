package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"simple_cdn/internal/domain"
)

func TestMigrationsRecordCurrentVersionAndRemainIdempotent(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	version, err := database.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if want := schemaMigrations[len(schemaMigrations)-1].Version; version != want {
		t.Fatalf("schema version = %d, want %d", version, want)
	}
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(schemaMigrations) {
		t.Fatalf("migration history count = %d, want %d", count, len(schemaMigrations))
	}
}

func TestHTTP3MigrationAddsPublicUDPPorts(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.db.Exec(`ALTER TABLE node_states DROP COLUMN public_udp_ports_json`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`DELETE FROM schema_migrations WHERE version >= 19`); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	found, err := columnExists(database.db, "node_states", "public_udp_ports_json")
	if err != nil || !found {
		t.Fatalf("public UDP ports column = %v, %v", found, err)
	}
}

func TestOriginConnectionMigrationAddsPoolState(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.db.Exec(`ALTER TABLE node_states DROP COLUMN origin_pools_json`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`DELETE FROM schema_migrations WHERE version >= 20`); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	found, err := columnExists(database.db, "node_states", "origin_pools_json")
	if err != nil || !found {
		t.Fatalf("origin pool state column = %v, %v", found, err)
	}
}

func TestSiteHTTP3MigrationDefaultsExistingSitesOff(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("migration-edge", "203.0.113.29")
	if err != nil {
		t.Fatal(err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "migration-site", Domains: []string{"migration.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: true,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`ALTER TABLE sites DROP COLUMN http3_enabled`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`DELETE FROM schema_migrations WHERE version >= 21`); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	var enabled int
	if err := database.db.QueryRow(`SELECT http3_enabled FROM sites WHERE id = ?`, site.ID).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 {
		t.Fatalf("migrated HTTP/3 default = %d, want 0", enabled)
	}
}

func TestSmartRoutingMigrationBackfillsExistingNodeOwnership(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	active, _ := database.CreateNode("active-edge", "203.0.113.126")
	autoPaused, _ := database.CreateNode("auto-paused-edge", "203.0.113.127")
	manualPaused, _ := database.CreateNode("manual-paused-edge", "203.0.113.128")
	if _, err := database.db.Exec(`UPDATE nodes SET status = ? WHERE id = ?`, domain.NodeActive, active.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`UPDATE nodes SET status = ?, monitor_auto_paused = 1 WHERE id = ?`, domain.NodeDraining, autoPaused.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`UPDATE nodes SET status = ?, monitor_auto_paused = 0 WHERE id = ?`, domain.NodeDraining, manualPaused.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`DROP TABLE node_smart_routing`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`DELETE FROM schema_migrations WHERE version >= 17`); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	activePolicy, err := database.SmartRoutingPolicy(active.ID)
	if err != nil || !activePolicy.Enabled || activePolicy.ScoreGate != SmartRoutingScoreUnknown || activePolicy.ScoreResumeRounds != SmartRoutingMinResumeRounds {
		t.Fatalf("active policy = %#v, %v", activePolicy, err)
	}
	autoPolicy, err := database.SmartRoutingPolicy(autoPaused.ID)
	if err != nil || !autoPolicy.Enabled || autoPolicy.ScoreGate != SmartRoutingScoreBlocked {
		t.Fatalf("auto-paused policy = %#v, %v", autoPolicy, err)
	}
	manualPolicy, err := database.SmartRoutingPolicy(manualPaused.ID)
	if err != nil || manualPolicy.Enabled || !manualPolicy.ScoreEnabled {
		t.Fatalf("manual policy = %#v, %v", manualPolicy, err)
	}
}

func TestSmartRoutingMinimumRecoveryRoundsMigrationUpgradesExistingPolicies(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("legacy-smart-edge", "203.0.113.129")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`UPDATE node_smart_routing SET score_resume_rounds = 1, score_recovery_streak = 1 WHERE node_id = ?`, node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`DELETE FROM schema_migrations WHERE version >= 18`); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	policy, err := database.SmartRoutingPolicy(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if policy.ScoreResumeRounds != SmartRoutingMinResumeRounds || policy.ScoreRecoveryStreak != 1 {
		t.Fatalf("migrated policy = %#v", policy)
	}
}

func TestEdgeAgentVersionMigrationUpgradesLegacySchema(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.db.Exec(`ALTER TABLE nodes DROP COLUMN agent_version`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`DELETE FROM schema_migrations WHERE version >= 16`); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	node, err := database.CreateNode("versioned-edge", "203.0.113.90")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.HeartbeatWithAgentVersion(node.ID, 0, "", nil, "9.8.7", strings.Repeat("a", 64), ""); err != nil {
		t.Fatal(err)
	}
	node, err = database.GetNode(node.ID)
	if err != nil || node.AgentVersion != "9.8.7" {
		t.Fatalf("migrated Agent version = %q, err=%v", node.AgentVersion, err)
	}
}

func TestNotificationPreferencesMigrationUpgradesLegacySchema(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.db.Exec(`ALTER TABLE control_settings DROP COLUMN smtp_notification_categories_json`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`DROP TABLE notification_delivery_state`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`DELETE FROM schema_migrations WHERE version >= 14`); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	found, err := columnExists(database.db, "control_settings", "smtp_notification_categories_json")
	if err != nil || !found {
		t.Fatalf("notification categories column = %v, %v", found, err)
	}
	var count int
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'notification_delivery_state'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("notification delivery state table was not created")
	}
}

func TestBrandingLogoMigrationAddsLegacyColumn(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.db.Exec(`ALTER TABLE control_settings DROP COLUMN brand_logo_data_url`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`DELETE FROM schema_migrations WHERE version >= 11`); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	found, err := columnExists(database.db, "control_settings", "brand_logo_data_url")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("branding logo migration did not add control_settings.brand_logo_data_url")
	}
}

func TestTCPMonitoringMigrationAddsSchema(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	found, err := columnExists(database.db, "nodes", "monitor_auto_paused")
	if err != nil || !found {
		t.Fatalf("monitor_auto_paused column = %v, %v", found, err)
	}
	for _, table := range []string{"monitoring_targets", "node_monitoring_status", "monitoring_probe_results"} {
		var count int
		if err := database.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("missing monitoring table %s", table)
		}
	}
}

func TestTCPMonitoringMigrationUpgradesLegacyNodesTable(t *testing.T) {
	database, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`CREATE TABLE nodes (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateTCPMonitoring(tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	found, err := columnExists(database, "nodes", "monitor_auto_paused")
	if err != nil || !found {
		t.Fatalf("monitor_auto_paused column = %v, %v", found, err)
	}
	for _, table := range []string{"monitoring_targets", "node_monitoring_status", "monitoring_probe_results"} {
		var count int
		if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("migration did not create %s", table)
		}
	}
}

func TestMonitoringTargetNameMigrationBackfillsLegacyTargets(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.db.Exec(`DROP INDEX idx_monitoring_targets_name`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`ALTER TABLE monitoring_targets DROP COLUMN name`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO monitoring_targets(id, address, enabled, created_at, updated_at) VALUES ('legacy', 'probe.example.test:443', 1, '2026-07-20T00:00:00Z', '2026-07-20T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`DELETE FROM schema_migrations WHERE version >= 13`); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := database.db.QueryRow(`SELECT name FROM monitoring_targets WHERE id = 'legacy'`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "probe.example.test:443" {
		t.Fatalf("backfilled monitoring target name = %q", name)
	}
}

func TestMonitoringTargetNameMigrationDisambiguatesLongLegacyAddresses(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.db.Exec(`DROP INDEX idx_monitoring_targets_name`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`ALTER TABLE monitoring_targets DROP COLUMN name`); err != nil {
		t.Fatal(err)
	}
	prefix := strings.Repeat("a", domain.MaxMonitoringTargetNameLength)
	for _, target := range []struct{ id, address string }{
		{"legacy-a", prefix + "-one.example.test:443"},
		{"legacy-b", prefix + "-two.example.test:443"},
	} {
		if _, err := database.db.Exec(`INSERT INTO monitoring_targets(id, address, enabled, created_at, updated_at) VALUES (?, ?, 1, '2026-07-20T00:00:00Z', '2026-07-20T00:00:00Z')`, target.id, target.address); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.db.Exec(`DELETE FROM schema_migrations WHERE version >= 13`); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	rows, err := database.db.Query(`SELECT name FROM monitoring_targets ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || strings.EqualFold(names[0], names[1]) {
		t.Fatalf("migrated monitoring names = %#v", names)
	}
	for _, name := range names {
		if utf8.RuneCountInString(name) > domain.MaxMonitoringTargetNameLength {
			t.Fatalf("migrated monitoring name is too long: %q", name)
		}
	}
}

func TestFailedMigrationRollsBackSchemaAndVersionRecord(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	wanted := errors.New("stop migration")
	migration := schemaMigration{
		Version: 99,
		Name:    "transaction-rollback-test",
		Apply: func(tx *sql.Tx) error {
			if _, err := tx.Exec(`CREATE TABLE migration_rollback_probe (id INTEGER PRIMARY KEY)`); err != nil {
				return err
			}
			return wanted
		},
	}
	if err := database.applyMigration(migration); !errors.Is(err, wanted) {
		t.Fatalf("failed migration error = %v", err)
	}
	var tableCount int
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'migration_rollback_probe'`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if tableCount != 0 {
		t.Fatal("failed migration left its table behind")
	}
	var versionCount int
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 99`).Scan(&versionCount); err != nil {
		t.Fatal(err)
	}
	if versionCount != 0 {
		t.Fatal("failed migration recorded a version")
	}
}

func TestMigrationHistoryRejectsGaps(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.db.Exec(`DELETE FROM schema_migrations WHERE version = 2`); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(); err == nil {
		t.Fatal("migration history gap was accepted")
	}
}

func TestLatestMigrationDropsMachineStatusAndAddsCacheDefaults(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.db.Exec(`CREATE TABLE node_machine_status (node_id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`DELETE FROM schema_migrations WHERE version >= 8`); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	var machineTableCount int
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'node_machine_status'`).Scan(&machineTableCount); err != nil {
		t.Fatal(err)
	}
	if machineTableCount != 0 {
		t.Fatal("machine status table remains after migration")
	}
	settings, err := database.ControlSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.CacheDefaultSizeGB != 1 {
		t.Fatalf("cache default = %d, want 1", settings.CacheDefaultSizeGB)
	}
	for table, column := range map[string]string{
		"sites": "cache_max_size_gb", "nodes": "cache_max_size_gb", "node_states": "cache_max_bytes",
	} {
		found, err := columnExists(database.db, table, column)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatalf("missing %s.%s after migration", table, column)
		}
	}
}

func TestRateLimitBanEscalationMigrationAddsLegacyColumns(t *testing.T) {
	database, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`PRAGMA foreign_keys = ON;
		CREATE TABLE rate_limit_policies (id TEXT PRIMARY KEY);
		CREATE TABLE security_bans (ip TEXT PRIMARY KEY);
		CREATE TABLE security_events (id TEXT PRIMARY KEY);`); err != nil {
		t.Fatal(err)
	}
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateRateLimitBanEscalation(tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for table, columns := range map[string][]string{
		"rate_limit_policies": {"ban_enabled", "ban_after_consecutive_429", "ban_duration_seconds"},
		"security_bans":       {"rate_limit_policy_id"},
		"security_events":     {"rate_limit_policy_id"},
	} {
		for _, column := range columns {
			found, err := columnExists(database, table, column)
			if err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatalf("missing migrated column %s.%s", table, column)
			}
		}
	}
	if _, err := database.Exec(`INSERT INTO rate_limit_policies(id) VALUES ('policy')`); err != nil {
		t.Fatal(err)
	}
	var enabled, after, duration int
	if err := database.QueryRow(`SELECT ban_enabled, ban_after_consecutive_429, ban_duration_seconds
		FROM rate_limit_policies WHERE id = 'policy'`).Scan(&enabled, &after, &duration); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 || after != 3 || duration != 3600 {
		t.Fatalf("migrated defaults = enabled:%d after:%d duration:%d", enabled, after, duration)
	}
}

func TestNodeCacheLimitMigrationAddsNodeOverrideAndClearsSiteOverrides(t *testing.T) {
	database, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`CREATE TABLE nodes (id TEXT PRIMARY KEY);
		CREATE TABLE sites (id TEXT PRIMARY KEY, cache_max_size_gb INTEGER);
		INSERT INTO sites(id, cache_max_size_gb) VALUES ('site-1', 7);`); err != nil {
		t.Fatal(err)
	}
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateNodeCacheLimits(tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	found, err := columnExists(database, "nodes", "cache_max_size_gb")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("node cache override column was not added")
	}
	var legacyOverride sql.NullInt64
	if err := database.QueryRow(`SELECT cache_max_size_gb FROM sites WHERE id = 'site-1'`).Scan(&legacyOverride); err != nil {
		t.Fatal(err)
	}
	if legacyOverride.Valid {
		t.Fatalf("legacy site cache override remains set to %d", legacyOverride.Int64)
	}
}

func columnExists(database *sql.DB, table, column string) (bool, error) {
	rows, err := database.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
