package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"simple_cdn/internal/domain"
)

type schemaMigration struct {
	Version int
	Name    string
	Apply   func(*sql.Tx) error
}

var schemaMigrations = []schemaMigration{
	{Version: 1, Name: "core-schema", Apply: migrateCoreSchema},
	{Version: 2, Name: "task-invariants", Apply: migrateTaskInvariants},
	{Version: 3, Name: "published-state-and-security-defaults", Apply: migratePublishedState},
	{Version: 4, Name: "nginx-config-fragments", Apply: migrateNginxFragments},
	{Version: 5, Name: "message-center", Apply: migrateMessageCenter},
	{Version: 6, Name: "message-dismissal", Apply: migrateMessageDismissal},
	{Version: 7, Name: "branding-settings", Apply: migrateBrandingSettings},
	{Version: 8, Name: "ephemeral-machine-status-and-cache-limits", Apply: migrateCacheLimits},
	{Version: 9, Name: "rate-limit-ban-escalation", Apply: migrateRateLimitBanEscalation},
	{Version: 10, Name: "node-cache-limits", Apply: migrateNodeCacheLimits},
	{Version: 11, Name: "branding-logo", Apply: migrateBrandingLogo},
	{Version: 12, Name: "tcp-monitoring", Apply: migrateTCPMonitoring},
	{Version: 13, Name: "monitoring-target-names", Apply: migrateMonitoringTargetNames},
	{Version: 14, Name: "notification-preferences-and-delivery-state", Apply: migrateNotificationPreferences},
	{Version: 15, Name: "node-nginx-capacity-and-site-timeouts", Apply: migrateNodeNginxCapacityAndSiteTimeouts},
	{Version: 16, Name: "edge-agent-version", Apply: migrateEdgeAgentVersion},
	{Version: 17, Name: "smart-routing", Apply: migrateSmartRouting},
	{Version: 18, Name: "smart-routing-minimum-recovery-rounds", Apply: migrateSmartRoutingMinimumRecoveryRounds},
	{Version: 19, Name: "http3-public-udp-ports", Apply: migrateHTTP3PublicUDPPorts},
	{Version: 20, Name: "origin-connection-pools", Apply: migrateOriginConnectionPools},
	{Version: 21, Name: "site-http3-opt-in", Apply: migrateSiteHTTP3OptIn},
	{Version: 22, Name: "managed-nginx-artifacts", Apply: migrateManagedNginxArtifacts},
	{Version: 23, Name: "site-proxy-buffering-controls", Apply: migrateSiteProxyBufferingControls},
	{Version: 24, Name: "wireguard-tunnels-and-performance", Apply: migrateWireGuard},
	{Version: 25, Name: "wireguard-egress-limits", Apply: migrateWireGuardEgressLimits},
	{Version: 26, Name: "wireguard-transfer-rates", Apply: migrateWireGuardTransferRates},
	{Version: 27, Name: "waf-chain-and-proof-of-work", Apply: migrateWAFChainAndProofOfWork},
	{Version: 28, Name: "static-assets", Apply: migrateStaticAssets},
	{Version: 29, Name: "passkey-authentication", Apply: migratePasskeyAuthentication},
	{Version: 30, Name: "authentication-hardening", Apply: migrateAuthenticationHardening},
}

func migrateAuthenticationHardening(tx *sql.Tx) error {
	for _, column := range []struct {
		table      string
		name       string
		definition string
	}{
		{"admin_users", "last_totp_counter", "last_totp_counter INTEGER"},
		{"sessions", "auth_method", "auth_method TEXT NOT NULL DEFAULT 'legacy'"},
		{"sessions", "authenticator_id", "authenticator_id TEXT NOT NULL DEFAULT ''"},
		{"sessions", "authenticated_at", "authenticated_at TEXT"},
		{"sessions", "elevated_until", "elevated_until TEXT"},
	} {
		if err := addColumnIfMissing(tx, column.table, column.name, column.definition); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`UPDATE sessions SET authenticated_at = created_at WHERE authenticated_at IS NULL;
	CREATE TABLE IF NOT EXISTS authentication_attempts (
		id TEXT PRIMARY KEY,
		scope TEXT NOT NULL,
		key_hash TEXT NOT NULL,
		attempted_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_authentication_attempts_lookup
		ON authentication_attempts(scope, key_hash, attempted_at);
	CREATE INDEX IF NOT EXISTS idx_authentication_attempts_time
		ON authentication_attempts(attempted_at);`)
	if err != nil {
		return fmt.Errorf("create hardened authentication schema: %w", err)
	}
	return nil
}

func migratePasskeyAuthentication(tx *sql.Tx) error {
	if err := addColumnIfMissing(tx, "admin_users", "passkey_enabled", "passkey_enabled INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS webauthn_users (
		rpid TEXT NOT NULL,
		user_id TEXT NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
		user_handle BLOB NOT NULL,
		created_at TEXT NOT NULL,
		PRIMARY KEY(rpid, user_id),
		UNIQUE(rpid, user_handle)
	);
	CREATE TABLE IF NOT EXISTS passkey_credentials (
		rpid TEXT NOT NULL,
		credential_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		name TEXT NOT NULL,
		credential_ciphertext BLOB NOT NULL,
		last_used_at TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY(rpid, credential_id),
		FOREIGN KEY(rpid, user_id) REFERENCES webauthn_users(rpid, user_id) ON DELETE CASCADE
	);
	CREATE TABLE IF NOT EXISTS webauthn_challenges (
		token_hash TEXT PRIMARY KEY,
		purpose TEXT NOT NULL,
		user_id TEXT NOT NULL,
		rpid TEXT NOT NULL,
		label TEXT NOT NULL DEFAULT '',
		session_json BLOB NOT NULL,
		expires_at TEXT NOT NULL,
		created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_passkey_credentials_user ON passkey_credentials(rpid, user_id, created_at);
	CREATE INDEX IF NOT EXISTS idx_webauthn_challenges_expires ON webauthn_challenges(expires_at);`)
	if err != nil {
		return fmt.Errorf("create passkey authentication schema: %w", err)
	}
	return nil
}

func migrateStaticAssets(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS static_assets (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		original_name TEXT NOT NULL,
		sha256 TEXT NOT NULL UNIQUE,
		size_bytes INTEGER NOT NULL,
		content_type TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS static_asset_bindings (
		id TEXT PRIMARY KEY,
		asset_id TEXT NOT NULL REFERENCES static_assets(id) ON DELETE CASCADE,
		site_id TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
		url_path TEXT NOT NULL,
		cache_control TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		UNIQUE(site_id, url_path)
	);
	CREATE INDEX IF NOT EXISTS idx_static_asset_bindings_asset ON static_asset_bindings(asset_id, site_id);
	CREATE INDEX IF NOT EXISTS idx_static_asset_bindings_site ON static_asset_bindings(site_id, url_path);`); err != nil {
		return fmt.Errorf("create static asset schema: %w", err)
	}
	return addColumnIfMissing(tx, "node_states", "static_assets_json", "static_assets_json TEXT NOT NULL DEFAULT '[]'")
}

func migrateWAFChainAndProofOfWork(tx *sql.Tx) error {
	for _, column := range []struct {
		name       string
		definition string
	}{
		{"site_ids_json", "site_ids_json TEXT NOT NULL DEFAULT '[]'"},
		{"conditions_json", "conditions_json TEXT NOT NULL DEFAULT '[]'"},
		{"response_status", "response_status INTEGER NOT NULL DEFAULT 403"},
	} {
		if err := addColumnIfMissing(tx, "security_policies", column.name, column.definition); err != nil {
			return err
		}
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{"site_id", "site_id TEXT NOT NULL DEFAULT ''"},
		{"raw_uri", "raw_uri TEXT NOT NULL DEFAULT ''"},
		{"query_string", "query_string TEXT NOT NULL DEFAULT ''"},
		{"user_agent", "user_agent TEXT NOT NULL DEFAULT ''"},
		{"matched_field", "matched_field TEXT NOT NULL DEFAULT ''"},
	} {
		if err := addColumnIfMissing(tx, "security_events", column.name, column.definition); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS pow_policies (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		enabled INTEGER NOT NULL DEFAULT 1,
		site_ids_json TEXT NOT NULL,
		path_pattern TEXT NOT NULL,
		difficulty_bits INTEGER NOT NULL,
		challenge_ttl_seconds INTEGER NOT NULL,
		pass_ttl_seconds INTEGER NOT NULL,
		priority INTEGER NOT NULL,
		secret_ciphertext BLOB NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_pow_policies_priority ON pow_policies(priority, created_at);`); err != nil {
		return fmt.Errorf("create proof-of-work policy schema: %w", err)
	}
	if err := seedBuiltinSecurityPoliciesTx(tx); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT id, pattern FROM security_policies WHERE conditions_json = '[]'`)
	if err != nil {
		return fmt.Errorf("read legacy security policies: %w", err)
	}
	type legacyPolicy struct {
		id      string
		pattern string
	}
	legacy := make([]legacyPolicy, 0)
	for rows.Next() {
		var policy legacyPolicy
		if err := rows.Scan(&policy.id, &policy.pattern); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy security policy: %w", err)
		}
		legacy = append(legacy, policy)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read legacy security policies: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy security policies: %w", err)
	}
	for _, policy := range legacy {
		conditions, err := json.Marshal([]domain.SecurityCondition{{
			Field: domain.SecurityFieldPath, Operator: domain.SecurityOperatorRegex, Value: policy.pattern,
		}})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE security_policies SET conditions_json = ? WHERE id = ?`, string(conditions), policy.id); err != nil {
			return fmt.Errorf("backfill security policy %s: %w", policy.id, err)
		}
	}
	return nil
}

func migrateWireGuardTransferRates(tx *sql.Tx) error {
	for _, column := range []struct {
		name       string
		definition string
	}{
		{"rx_bytes_per_second", "rx_bytes_per_second REAL"},
		{"tx_bytes_per_second", "tx_bytes_per_second REAL"},
		{"transfer_sample_seconds", "transfer_sample_seconds REAL"},
	} {
		if err := addColumnIfMissing(tx, "wireguard_tunnel_nodes", column.name, column.definition); err != nil {
			return err
		}
	}
	return nil
}

func migrateWireGuardEgressLimits(tx *sql.Tx) error {
	if err := addColumnIfMissing(tx, "wireguard_tunnels", "origin_egress_limit_mbps",
		"origin_egress_limit_mbps INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	return addColumnIfMissing(tx, "wireguard_tunnel_nodes", "edge_egress_limit_mbps",
		"edge_egress_limit_mbps INTEGER NOT NULL DEFAULT 0")
}

func migrateWireGuard(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS wireguard_tunnels (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		endpoint_host TEXT NOT NULL,
		listen_port INTEGER NOT NULL,
		address_cidr TEXT NOT NULL UNIQUE,
		origin_address TEXT NOT NULL UNIQUE,
		mtu INTEGER NOT NULL,
		persistent_keepalive_seconds INTEGER NOT NULL,
		performance_port INTEGER NOT NULL,
		origin_public_key TEXT NOT NULL DEFAULT '',
		revision INTEGER NOT NULL DEFAULT 1,
		origin_configured_revision INTEGER NOT NULL DEFAULT 0,
		origin_configured_at TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS wireguard_tunnel_nodes (
		tunnel_id TEXT NOT NULL REFERENCES wireguard_tunnels(id) ON DELETE CASCADE,
		node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
		address TEXT NOT NULL,
		public_key TEXT NOT NULL DEFAULT '',
		applied_revision INTEGER NOT NULL DEFAULT 0,
		latest_handshake_at TEXT,
		rx_bytes INTEGER NOT NULL DEFAULT 0,
		tx_bytes INTEGER NOT NULL DEFAULT 0,
		rx_bytes_per_second REAL,
		tx_bytes_per_second REAL,
		transfer_sample_seconds REAL,
		last_reported_at TEXT,
		last_error TEXT NOT NULL DEFAULT '',
		PRIMARY KEY(tunnel_id, node_id),
		UNIQUE(tunnel_id, address)
	);
	CREATE TABLE IF NOT EXISTS wireguard_install_tokens (
		token_hash TEXT PRIMARY KEY,
		tunnel_id TEXT NOT NULL REFERENCES wireguard_tunnels(id) ON DELETE CASCADE,
		expires_at TEXT NOT NULL,
		used_at TEXT,
		created_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS wireguard_performance_tests (
		id TEXT PRIMARY KEY,
		tunnel_id TEXT NOT NULL REFERENCES wireguard_tunnels(id) ON DELETE CASCADE,
		node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
		target_mbps INTEGER NOT NULL,
		duration_seconds INTEGER NOT NULL,
		status TEXT NOT NULL,
		result_json TEXT,
		error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		started_at TEXT,
		finished_at TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_wireguard_nodes_node ON wireguard_tunnel_nodes(node_id, tunnel_id);
	CREATE INDEX IF NOT EXISTS idx_wireguard_tests_node_status ON wireguard_performance_tests(node_id, status, created_at);
	CREATE INDEX IF NOT EXISTS idx_wireguard_tests_created ON wireguard_performance_tests(created_at DESC);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_wireguard_tests_active_node
		ON wireguard_performance_tests(node_id)
		WHERE status IN ('queued', 'running');
	CREATE UNIQUE INDEX IF NOT EXISTS idx_wireguard_tests_active_tunnel
		ON wireguard_performance_tests(tunnel_id)
		WHERE status IN ('queued', 'running');`)
	if err != nil {
		return fmt.Errorf("create WireGuard schema: %w", err)
	}
	return nil
}

func migrateSiteProxyBufferingControls(tx *sql.Tx) error {
	for _, column := range []struct {
		name       string
		definition string
	}{
		{"request_body_buffering", "request_body_buffering INTEGER NOT NULL DEFAULT 1"},
		{"origin_response_buffering", "origin_response_buffering INTEGER NOT NULL DEFAULT 1"},
	} {
		if err := addColumnIfMissing(tx, "sites", column.name, column.definition); err != nil {
			return err
		}
	}
	return backfillSitePublicationBufferingTx(tx)
}

func backfillSitePublicationBufferingTx(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT site_id, site_json FROM site_publications`)
	if err != nil {
		return err
	}
	type publicationUpdate struct {
		siteID   string
		siteJSON string
	}
	updates := make([]publicationUpdate, 0)
	for rows.Next() {
		var siteID, siteJSON string
		if err := rows.Scan(&siteID, &siteJSON); err != nil {
			rows.Close()
			return err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(siteJSON), &fields); err != nil {
			rows.Close()
			return fmt.Errorf("decode published site %s: %w", siteID, err)
		}
		changed := false
		for _, field := range []string{"request_body_buffering", "origin_response_buffering"} {
			if _, found := fields[field]; found {
				continue
			}
			fields[field] = json.RawMessage("true")
			changed = true
		}
		if !changed {
			continue
		}
		encoded, err := json.Marshal(fields)
		if err != nil {
			rows.Close()
			return fmt.Errorf("encode published site %s: %w", siteID, err)
		}
		updates = append(updates, publicationUpdate{siteID: siteID, siteJSON: string(encoded)})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, update := range updates {
		if _, err := tx.Exec(`UPDATE site_publications SET site_json = ? WHERE site_id = ?`, update.siteJSON, update.siteID); err != nil {
			return err
		}
	}
	return nil
}

func migrateManagedNginxArtifacts(tx *sql.Tx) error {
	for _, column := range []struct {
		table      string
		name       string
		definition string
	}{
		{"nodes", "nginx_version", "nginx_version TEXT NOT NULL DEFAULT ''"},
		{"nodes", "nginx_sha256", "nginx_sha256 TEXT NOT NULL DEFAULT ''"},
		{"node_upgrade_tasks", "source_nginx_sha256", "source_nginx_sha256 TEXT NOT NULL DEFAULT ''"},
		{"node_upgrade_tasks", "target_nginx_sha256", "target_nginx_sha256 TEXT NOT NULL DEFAULT ''"},
		{"node_upgrade_tasks", "nginx_bundle_url", "nginx_bundle_url TEXT NOT NULL DEFAULT ''"},
		{"node_upgrade_tasks", "nginx_bundle_sha256", "nginx_bundle_sha256 TEXT NOT NULL DEFAULT ''"},
		{"node_upgrade_tasks", "nginx_service_url", "nginx_service_url TEXT NOT NULL DEFAULT ''"},
		{"node_upgrade_tasks", "nginx_service_sha256", "nginx_service_sha256 TEXT NOT NULL DEFAULT ''"},
	} {
		if err := addColumnIfMissing(tx, column.table, column.name, column.definition); err != nil {
			return err
		}
	}
	return nil
}

func migrateSiteHTTP3OptIn(tx *sql.Tx) error {
	return addColumnIfMissing(tx, "sites", "http3_enabled", "http3_enabled INTEGER NOT NULL DEFAULT 0")
}

func migrateOriginConnectionPools(tx *sql.Tx) error {
	return addColumnIfMissing(tx, "node_states", "origin_pools_json", "origin_pools_json TEXT NOT NULL DEFAULT '[]'")
}

func migrateHTTP3PublicUDPPorts(tx *sql.Tx) error {
	return addColumnIfMissing(tx, "node_states", "public_udp_ports_json", "public_udp_ports_json TEXT NOT NULL DEFAULT '[]'")
}

func migrateSmartRouting(tx *sql.Tx) error {
	createdAt := "1970-01-01T00:00:00Z"
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS node_smart_routing (
		node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
		enabled INTEGER NOT NULL DEFAULT 1,
		score_enabled INTEGER NOT NULL DEFAULT 1,
		score_pause_below INTEGER NOT NULL DEFAULT 80,
		score_pause_rounds INTEGER NOT NULL DEFAULT 4,
		score_resume_at INTEGER NOT NULL DEFAULT 80,
		score_resume_rounds INTEGER NOT NULL DEFAULT 3,
		score_gate TEXT NOT NULL DEFAULT 'unknown',
		score_low_streak INTEGER NOT NULL DEFAULT 0,
		score_recovery_streak INTEGER NOT NULL DEFAULT 0,
		schedule_enabled INTEGER NOT NULL DEFAULT 0,
		schedule_windows_json TEXT NOT NULL DEFAULT '[]',
		schedule_gate TEXT NOT NULL DEFAULT 'open',
		updated_at TEXT NOT NULL
	)`); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO node_smart_routing(
		node_id, enabled, score_enabled, score_pause_below, score_pause_rounds,
		score_resume_at, score_resume_rounds, score_gate, score_low_streak,
		score_recovery_streak, schedule_enabled, schedule_windows_json,
		schedule_gate, updated_at
	)
	SELECT nodes.id,
		CASE WHEN nodes.status IN (?, ?) OR nodes.monitor_auto_paused = 1 THEN 1 ELSE 0 END,
		1, 80, 4, 80, 3,
		CASE
			WHEN nodes.monitor_auto_paused = 1 THEN 'blocked'
			WHEN COALESCE(status.score, -1) >= 80 THEN 'allowed'
			ELSE 'unknown'
		END,
		CASE
			WHEN COALESCE(status.score, 100) < 80 THEN MIN(status.consecutive_abnormal, 3)
			ELSE 0
		END,
		0, 0, '[]', 'open', COALESCE(NULLIF(nodes.updated_at, ''), ?)
	FROM nodes
	LEFT JOIN node_monitoring_status AS status ON status.node_id = nodes.id
	ON CONFLICT(node_id) DO NOTHING`, domain.NodePending, domain.NodeActive, createdAt)
	return err
}

func migrateSmartRoutingMinimumRecoveryRounds(tx *sql.Tx) error {
	_, err := tx.Exec(`UPDATE node_smart_routing SET score_resume_rounds = 3 WHERE score_resume_rounds < 3`)
	return err
}

func migrateEdgeAgentVersion(tx *sql.Tx) error {
	return addColumnIfMissing(tx, "nodes", "agent_version", "agent_version TEXT NOT NULL DEFAULT ''")
}

func migrateNodeNginxCapacityAndSiteTimeouts(tx *sql.Tx) error {
	for _, column := range []struct {
		table      string
		name       string
		definition string
	}{
		{"nodes", "nginx_worker_processes", "nginx_worker_processes INTEGER NOT NULL DEFAULT 0"},
		{"nodes", "nginx_worker_connections", "nginx_worker_connections INTEGER NOT NULL DEFAULT 4096"},
		{"nodes", "nginx_worker_rlimit_nofile", "nginx_worker_rlimit_nofile INTEGER NOT NULL DEFAULT 65536"},
		{"sites", "client_keepalive_timeout_seconds", "client_keepalive_timeout_seconds INTEGER NOT NULL DEFAULT 120"},
		{"node_states", "nginx_main_config", "nginx_main_config TEXT NOT NULL DEFAULT ''"},
		{"node_states", "nginx_events_config", "nginx_events_config TEXT NOT NULL DEFAULT ''"},
	} {
		if err := addColumnIfMissing(tx, column.table, column.name, column.definition); err != nil {
			return err
		}
	}
	return nil
}

func migrateNotificationPreferences(tx *sql.Tx) error {
	if err := addColumnIfMissing(tx, "control_settings", "smtp_notification_categories_json", `smtp_notification_categories_json TEXT NOT NULL DEFAULT '["availability","monitoring","certificate","backup"]'`); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS notification_delivery_state (
		key TEXT PRIMARY KEY,
		active INTEGER NOT NULL DEFAULT 0,
		last_sent_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`)
	return err
}

func migrateMonitoringTargetNames(tx *sql.Tx) error {
	if err := addColumnIfMissing(tx, "monitoring_targets", "name", "name TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT id, name, address FROM monitoring_targets ORDER BY created_at, id`)
	if err != nil {
		return err
	}
	type targetName struct {
		id      string
		current string
		base    string
	}
	targets := make([]targetName, 0)
	for rows.Next() {
		var target targetName
		var address string
		if err := rows.Scan(&target.id, &target.current, &address); err != nil {
			rows.Close()
			return err
		}
		target.base = strings.TrimSpace(target.current)
		if target.base == "" {
			target.base = address
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	used := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		name := uniqueMigratedMonitoringTargetName(target.base, used)
		used[strings.ToLower(name)] = struct{}{}
		if name == target.current {
			continue
		}
		if _, err := tx.Exec(`UPDATE monitoring_targets SET name = ? WHERE id = ?`, name, target.id); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_monitoring_targets_name ON monitoring_targets(name COLLATE NOCASE)`)
	return err
}

func uniqueMigratedMonitoringTargetName(base string, used map[string]struct{}) string {
	baseRunes := []rune(base)
	if len(baseRunes) > domain.MaxMonitoringTargetNameLength {
		baseRunes = baseRunes[:domain.MaxMonitoringTargetNameLength]
	}
	candidate := string(baseRunes)
	for sequence := 2; ; sequence++ {
		if _, exists := used[strings.ToLower(candidate)]; !exists {
			return candidate
		}
		suffix := []rune(fmt.Sprintf(" (%d)", sequence))
		prefixLength := domain.MaxMonitoringTargetNameLength - len(suffix)
		if prefixLength > len([]rune(base)) {
			prefixLength = len([]rune(base))
		}
		candidate = string([]rune(base)[:prefixLength]) + string(suffix)
	}
}

func migrateTCPMonitoring(tx *sql.Tx) error {
	if err := addColumnIfMissing(tx, "nodes", "monitor_auto_paused", "monitor_auto_paused INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS monitoring_targets (
		id TEXT PRIMARY KEY,
		address TEXT NOT NULL UNIQUE,
		enabled INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS node_monitoring_status (
		node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
		score INTEGER NOT NULL,
		success_rate REAL NOT NULL,
		average_latency_ms REAL NOT NULL,
		consecutive_abnormal INTEGER NOT NULL DEFAULT 0,
		last_checked_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS monitoring_probe_results (
		node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
		target_id TEXT NOT NULL REFERENCES monitoring_targets(id) ON DELETE CASCADE,
		attempts INTEGER NOT NULL,
		successful_attempts INTEGER NOT NULL,
		average_latency_ms REAL NOT NULL,
		error TEXT NOT NULL DEFAULT '',
		checked_at TEXT NOT NULL,
		PRIMARY KEY(node_id, target_id)
	);
	CREATE INDEX IF NOT EXISTS idx_monitoring_probe_target ON monitoring_probe_results(target_id, checked_at DESC);`)
	return err
}

func migrateBrandingLogo(tx *sql.Tx) error {
	return addColumnIfMissing(tx, "control_settings", "brand_logo_data_url", "brand_logo_data_url TEXT NOT NULL DEFAULT ''")
}

func migrateNodeCacheLimits(tx *sql.Tx) error {
	if err := addColumnIfMissing(tx, "nodes", "cache_max_size_gb", "cache_max_size_gb INTEGER"); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE sites SET cache_max_size_gb = NULL`)
	return err
}

func migrateRateLimitBanEscalation(tx *sql.Tx) error {
	for _, column := range []struct {
		table      string
		name       string
		definition string
	}{
		{"rate_limit_policies", "ban_enabled", "ban_enabled INTEGER NOT NULL DEFAULT 0"},
		{"rate_limit_policies", "ban_after_consecutive_429", "ban_after_consecutive_429 INTEGER NOT NULL DEFAULT 3"},
		{"rate_limit_policies", "ban_duration_seconds", "ban_duration_seconds INTEGER NOT NULL DEFAULT 3600"},
		{"security_bans", "rate_limit_policy_id", "rate_limit_policy_id TEXT REFERENCES rate_limit_policies(id) ON DELETE SET NULL"},
		{"security_events", "rate_limit_policy_id", "rate_limit_policy_id TEXT REFERENCES rate_limit_policies(id) ON DELETE SET NULL"},
	} {
		if err := addColumnIfMissing(tx, column.table, column.name, column.definition); err != nil {
			return err
		}
	}
	return nil
}

func migrateCacheLimits(tx *sql.Tx) error {
	for _, column := range []struct {
		table      string
		name       string
		definition string
	}{
		{"sites", "cache_max_size_gb", "cache_max_size_gb INTEGER"},
		{"control_settings", "cache_default_size_gb", "cache_default_size_gb INTEGER NOT NULL DEFAULT 1"},
		{"node_states", "cache_max_bytes", "cache_max_bytes INTEGER NOT NULL DEFAULT 1073741824"},
	} {
		if err := addColumnIfMissing(tx, column.table, column.name, column.definition); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`DROP TABLE IF EXISTS node_machine_status`)
	return err
}

func migrateBrandingSettings(tx *sql.Tx) error {
	for _, column := range []struct {
		name       string
		definition string
	}{
		{"brand_name", "brand_name TEXT NOT NULL DEFAULT 'simple_cdn'"},
		{"brand_subtitle", "brand_subtitle TEXT NOT NULL DEFAULT '控制面板'"},
	} {
		if err := addColumnIfMissing(tx, "control_settings", column.name, column.definition); err != nil {
			return err
		}
	}
	return nil
}

func LatestSchemaVersion() int {
	return schemaMigrations[len(schemaMigrations)-1].Version
}

func (s *Store) Migrate() error {
	if err := s.ensureMigrationTable(); err != nil {
		return err
	}
	applied, err := s.appliedMigrations()
	if err != nil {
		return err
	}
	for _, migration := range schemaMigrations {
		if name, ok := applied[migration.Version]; ok {
			if name != migration.Name {
				return fmt.Errorf("database migration %d is recorded as %q, expected %q", migration.Version, name, migration.Name)
			}
			continue
		}
		for version := 1; version < migration.Version; version++ {
			if _, ok := applied[version]; !ok {
				return fmt.Errorf("database migration history has a gap before version %d", migration.Version)
			}
		}
		if err := s.applyMigration(migration); err != nil {
			return err
		}
		applied[migration.Version] = migration.Name
	}
	latest := LatestSchemaVersion()
	for version := range applied {
		if version > latest {
			return fmt.Errorf("database schema version %d is newer than supported version %d", version, latest)
		}
	}
	// Certificate workers are process-scoped. This recovery is intentionally a
	// startup action rather than a one-time schema migration.
	if _, err := s.FailActiveCertificateTasks("certificate issuance interrupted by control-plane restart; retry Issue TLS"); err != nil {
		return fmt.Errorf("recover interrupted certificate tasks: %w", err)
	}
	return nil
}

func (s *Store) ensureMigrationTable() error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration metadata transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create migration metadata: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration metadata: %w", err)
	}
	return nil
}

func (s *Store) appliedMigrations() (map[int]string, error) {
	rows, err := s.db.Query(`SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read migration history: %w", err)
	}
	defer rows.Close()
	applied := make(map[int]string)
	versions := make([]int, 0)
	for rows.Next() {
		var version int
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			return nil, fmt.Errorf("scan migration history: %w", err)
		}
		applied[version] = name
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read migration history: %w", err)
	}
	sort.Ints(versions)
	for index, version := range versions {
		if version != index+1 {
			return nil, fmt.Errorf("database migration history has a gap at version %d", index+1)
		}
	}
	return applied, nil
}

func (s *Store) applyMigration(migration schemaMigration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin database migration %d (%s): %w", migration.Version, migration.Name, err)
	}
	defer tx.Rollback()
	if err := migration.Apply(tx); err != nil {
		return fmt.Errorf("apply database migration %d (%s): %w", migration.Version, migration.Name, err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)`,
		migration.Version, migration.Name, stamp(now())); err != nil {
		return fmt.Errorf("record database migration %d (%s): %w", migration.Version, migration.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit database migration %d (%s): %w", migration.Version, migration.Name, err)
	}
	return nil
}

func migrateCoreSchema(tx *sql.Tx) error {
	if _, err := tx.Exec(initialSchema); err != nil {
		return fmt.Errorf("create core schema: %w", err)
	}
	columns := []struct {
		table      string
		name       string
		definition string
	}{
		{"sites", "published", "published INTEGER NOT NULL DEFAULT 0"},
		{"sites", "stream_paths_json", "stream_paths_json TEXT NOT NULL DEFAULT '[]'"},
		{"sites", "passthrough", "passthrough INTEGER NOT NULL DEFAULT 0"},
		{"sites", "request_body_buffering", "request_body_buffering INTEGER NOT NULL DEFAULT 1"},
		{"sites", "origin_response_buffering", "origin_response_buffering INTEGER NOT NULL DEFAULT 1"},
		{"sites", "client_max_body_size_mb", "client_max_body_size_mb INTEGER NOT NULL DEFAULT 128"},
		{"sites", "client_keepalive_timeout_seconds", "client_keepalive_timeout_seconds INTEGER NOT NULL DEFAULT 120"},
		{"sites", "read_write_timeout_seconds", "read_write_timeout_seconds INTEGER NOT NULL DEFAULT 120"},
		{"sites", "dns_ttl_seconds", "dns_ttl_seconds INTEGER"},
		{"sites", "tcp_only", "tcp_only INTEGER NOT NULL DEFAULT 0"},
		{"sites", "tcp_forwards_json", "tcp_forwards_json TEXT NOT NULL DEFAULT '[]'"},
		{"sites", "cache_max_size_gb", "cache_max_size_gb INTEGER"},
		{"sites", "deleting", "deleting INTEGER NOT NULL DEFAULT 0"},
		{"nodes", "applied_version", "applied_version INTEGER NOT NULL DEFAULT 0"},
		{"nodes", "capabilities_json", "capabilities_json TEXT NOT NULL DEFAULT '[]'"},
		{"nodes", "agent_sha256", "agent_sha256 TEXT NOT NULL DEFAULT ''"},
		{"nodes", "active_upgrade_task_id", "active_upgrade_task_id TEXT NOT NULL DEFAULT ''"},
		{"deployment_tasks", "deadline_at", "deadline_at TEXT"},
		{"control_settings", "backup_override", "backup_override INTEGER NOT NULL DEFAULT 0"},
		{"control_settings", "backup_repository", "backup_repository TEXT NOT NULL DEFAULT ''"},
		{"control_settings", "backup_access_key_id", "backup_access_key_id TEXT NOT NULL DEFAULT ''"},
		{"control_settings", "backup_region", "backup_region TEXT NOT NULL DEFAULT 'us-east-1'"},
		{"control_settings", "backup_time", "backup_time TEXT NOT NULL DEFAULT '03:25'"},
		{"control_settings", "backup_random_delay_seconds", "backup_random_delay_seconds INTEGER NOT NULL DEFAULT 1200"},
		{"control_settings", "cache_default_size_gb", "cache_default_size_gb INTEGER NOT NULL DEFAULT 1"},
		// JSON null distinguishes a pre-capability state from an intentional empty listener set.
		{"node_states", "public_ports_json", "public_ports_json TEXT NOT NULL DEFAULT 'null'"},
		{"node_states", "nginx_stream_config", "nginx_stream_config TEXT NOT NULL DEFAULT ''"},
		{"node_states", "cache_max_bytes", "cache_max_bytes INTEGER NOT NULL DEFAULT 1073741824"},
	}
	for _, column := range columns {
		if err := addColumnIfMissing(tx, column.table, column.name, column.definition); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE sites SET stream_paths_json = '[]' WHERE stream_paths_json <> '[]'`); err != nil {
		return fmt.Errorf("retire legacy stream paths: %w", err)
	}
	return nil
}

func migrateTaskInvariants(tx *sql.Tx) error {
	if _, err := tx.Exec(`UPDATE deployment_tasks
		SET status = ?, detail = ?, updated_at = ?
		WHERE kind = 'publish_site' AND status IN (?, ?, ?) AND deadline_at IS NULL`,
		domain.TaskFailed, "publish confirmation interrupted by control-plane upgrade; retry Publish", stamp(now()),
		domain.TaskQueued, domain.TaskDispatching, domain.TaskApplying); err != nil {
		return fmt.Errorf("migrate legacy publish tasks: %w", err)
	}
	indexes := []struct {
		name string
		sql  string
	}{
		{"active certificate task", `CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_active_certificate_site
			ON deployment_tasks(site_id)
			WHERE kind IN ('issue_certificate', 'renew_certificate') AND status IN ('queued', 'dispatching', 'applying')`},
		{"active publish task", `CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_active_publish_site
			ON deployment_tasks(site_id)
			WHERE kind = 'publish_site' AND status IN ('queued', 'dispatching', 'applying')`},
		{"active site deletion task", `CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_active_delete_site
			ON deployment_tasks(site_id)
			WHERE kind = 'delete_site' AND status IN ('queued', 'dispatching', 'applying')`},
		{"active node upgrade", `CREATE UNIQUE INDEX IF NOT EXISTS idx_node_upgrade_tasks_active
			ON node_upgrade_tasks(node_id)
			WHERE status IN ('queued', 'applying')`},
	}
	for _, index := range indexes {
		if _, err := tx.Exec(index.sql); err != nil {
			return fmt.Errorf("create %s index: %w", index.name, err)
		}
	}
	return nil
}

func migratePublishedState(tx *sql.Tx) error {
	// A partially migrated legacy database may reach this migration before the
	// later site migrations. The publication scanner needs these columns now.
	if err := addColumnIfMissing(tx, "sites", "client_keepalive_timeout_seconds", "client_keepalive_timeout_seconds INTEGER NOT NULL DEFAULT 120"); err != nil {
		return err
	}
	if err := addColumnIfMissing(tx, "sites", "http3_enabled", "http3_enabled INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(tx, "sites", "request_body_buffering", "request_body_buffering INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := addColumnIfMissing(tx, "sites", "origin_response_buffering", "origin_response_buffering INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := seedBuiltinSecurityPoliciesTx(tx); err != nil {
		return err
	}
	if err := backfillSitePublicationsTx(tx); err != nil {
		return err
	}
	return backfillSiteDomainsTx(tx)
}

func migrateNginxFragments(tx *sql.Tx) error {
	return addColumnIfMissing(tx, "node_states", "nginx_fragments_json", "nginx_fragments_json TEXT NOT NULL DEFAULT 'null'")
}

func migrateMessageCenter(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS messages (
		id TEXT PRIMARY KEY,
		severity TEXT NOT NULL,
		category TEXT NOT NULL,
		title TEXT NOT NULL,
		body TEXT NOT NULL DEFAULT '',
		source_type TEXT,
		source_id TEXT,
		source_status TEXT,
		resource_type TEXT NOT NULL DEFAULT '',
		resource_id TEXT NOT NULL DEFAULT '',
		read_at TEXT,
		created_at TEXT NOT NULL,
		UNIQUE(source_type, source_id, source_status)
	);
	CREATE INDEX IF NOT EXISTS idx_messages_created ON messages(created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_messages_unread ON messages(read_at, created_at DESC);`)
	if err != nil {
		return fmt.Errorf("create message center schema: %w", err)
	}
	return nil
}

func migrateMessageDismissal(tx *sql.Tx) error {
	if err := addColumnIfMissing(tx, "messages", "dismissed_at", "dismissed_at TEXT"); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_visible
		ON messages(dismissed_at, read_at, created_at DESC)`)
	if err != nil {
		return fmt.Errorf("create visible messages index: %w", err)
	}
	return nil
}

func addColumnIfMissing(tx *sql.Tx, table, column, definition string) error {
	rows, err := tx.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("inspect table %s: %w", table, err)
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("inspect table %s: %w", table, err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("inspect table %s: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("inspect table %s: %w", table, err)
	}
	if found {
		return nil
	}
	if _, err := tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + definition); err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *Store) SchemaVersion() (int, error) {
	var version int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version)
	return version, err
}
