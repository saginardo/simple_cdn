package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
	"simple_cdn/internal/domain"
)

var (
	ErrNotFound             = errors.New("not found")
	ErrTokenInvalid         = errors.New("enrollment token is invalid or expired")
	ErrCacheDisabled        = errors.New("site cache is disabled for this site mode or buffering setting")
	ErrNodeAssigned         = errors.New("node is still assigned to a site")
	ErrSiteDeleting         = errors.New("site deletion is in progress")
	ErrSiteTaskActive       = errors.New("site has an active publish or certificate task")
	ErrSiteChanged          = errors.New("site changed while publication was being prepared; retry publish")
	ErrNodeUpgradeActive    = errors.New("node has an active online upgrade")
	ErrNodeOperationActive  = errors.New("node has an active publish, deletion, or uninstall operation")
	ErrUpgradeRetryNotReady = errors.New("edge has not confirmed that the previous local upgrade stopped")
)

type Store struct {
	db       *sql.DB
	readOnly bool

	siteCacheMu sync.Mutex
	siteCache   siteSnapshotCache
}

type siteSnapshotCache struct {
	marker    string
	sites     []domain.Site
	summaries []domain.SiteSummary
	loaded    bool
}

func (s *Store) ReadOnly() bool { return s.readOnly }

const legacyDefaultSecurityPolicyPattern = `(?i)^/+(?:\.env(?:[._~-]?[A-Za-z0-9-]*)?(?:\.php)?|(?:[^/]+/)*\.env(?:[._~-]?[A-Za-z0-9-]*)?(?:\.php)?|\.git(?:/|$|-)|\.aws(?:/|$)|\.docker/(?:config\.json|)|\.svn(?:/|$)|\.hg(?:/|$)|\.ht(?:access|passwd)|\.DS_Store$)`

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// A single controller process has a small write rate. One connection keeps SQLite's
	// connection-scoped foreign-key and busy-timeout settings consistent.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{"PRAGMA busy_timeout = 5000", "PRAGMA foreign_keys = ON", "PRAGMA journal_mode = WAL", "PRAGMA synchronous = NORMAL"} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("configure sqlite: %w", err)
		}
	}
	s := &Store{db: db}
	if err := s.Migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

const initialSchema = `
CREATE TABLE IF NOT EXISTS admin_users (
  id TEXT PRIMARY KEY,
  password_hash TEXT NOT NULL,
  totp_secret TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL UNIQUE,
  csrf_token TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS nodes (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  public_ipv4 TEXT NOT NULL UNIQUE,
  status TEXT NOT NULL,
	monitor_auto_paused INTEGER NOT NULL DEFAULT 0,
	capabilities_json TEXT NOT NULL DEFAULT '[]',
	cache_max_size_gb INTEGER,
	nginx_worker_processes INTEGER NOT NULL DEFAULT 0,
	nginx_worker_connections INTEGER NOT NULL DEFAULT 4096,
	nginx_worker_rlimit_nofile INTEGER NOT NULL DEFAULT 65536,
  cert_fingerprint TEXT,
	  last_heartbeat_at TEXT,
	  applied_version INTEGER NOT NULL DEFAULT 0,
	  agent_version TEXT NOT NULL DEFAULT '',
	  agent_sha256 TEXT NOT NULL DEFAULT '',
	  nginx_version TEXT NOT NULL DEFAULT '',
	  nginx_sha256 TEXT NOT NULL DEFAULT '',
	  active_upgrade_task_id TEXT NOT NULL DEFAULT '',
	  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS enrollment_tokens (
  token_hash TEXT PRIMARY KEY,
  node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  expires_at TEXT NOT NULL,
  used_at TEXT,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sites (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  zone_id TEXT NOT NULL,
  domains_json TEXT NOT NULL,
  node_ids_json TEXT NOT NULL,
	backup_node_ids_json TEXT NOT NULL DEFAULT '[]',
  primary_origin_json TEXT NOT NULL,
  backup_origin_json TEXT,
	stream_paths_json TEXT NOT NULL DEFAULT '[]',
	passthrough INTEGER NOT NULL DEFAULT 0,
	request_body_buffering INTEGER NOT NULL DEFAULT 1,
	origin_response_buffering INTEGER NOT NULL DEFAULT 1,
	dynamic_compression_enabled INTEGER NOT NULL DEFAULT 1,
	compression_excluded_mime_types_json TEXT NOT NULL DEFAULT '["text/event-stream"]',
	http3_enabled INTEGER NOT NULL DEFAULT 0,
	client_max_body_size_mb INTEGER NOT NULL DEFAULT 128,
	client_keepalive_timeout_seconds INTEGER NOT NULL DEFAULT 120,
	read_write_timeout_seconds INTEGER NOT NULL DEFAULT 120,
	dns_ttl_seconds INTEGER,
	tcp_only INTEGER NOT NULL DEFAULT 0,
	tcp_forwards_json TEXT NOT NULL DEFAULT '[]',
	cache_max_size_gb INTEGER,
  cache_generation INTEGER NOT NULL DEFAULT 1,
	cache_invalidations_json TEXT NOT NULL DEFAULT '[]',
	cache_warmups_json TEXT NOT NULL DEFAULT '[]',
  config_version INTEGER NOT NULL DEFAULT 1,
  published INTEGER NOT NULL DEFAULT 0,
  enabled INTEGER NOT NULL DEFAULT 1,
  deleting INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS site_domains (
  domain_name TEXT PRIMARY KEY,
  site_id TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS certificates (
  site_id TEXT PRIMARY KEY REFERENCES sites(id) ON DELETE CASCADE,
  certificate_ciphertext BLOB NOT NULL,
  private_key_ciphertext BLOB NOT NULL,
  not_after TEXT,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS site_publications (
  site_id TEXT PRIMARY KEY REFERENCES sites(id) ON DELETE CASCADE,
  site_json TEXT NOT NULL,
  certificate_ciphertext BLOB,
  private_key_ciphertext BLOB,
  certificate_not_after TEXT,
  published_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS node_states (
  node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
  version INTEGER NOT NULL,
  nginx_config TEXT NOT NULL,
	nginx_stream_config TEXT NOT NULL DEFAULT '',
	nginx_main_config TEXT NOT NULL DEFAULT '',
	nginx_events_config TEXT NOT NULL DEFAULT '',
	nginx_fragments_json TEXT NOT NULL DEFAULT 'null',
	public_ports_json TEXT NOT NULL DEFAULT '[]',
	public_udp_ports_json TEXT NOT NULL DEFAULT '[]',
	origin_pools_json TEXT NOT NULL DEFAULT '[]',
	static_assets_json TEXT NOT NULL DEFAULT '[]',
	cache_warmups_json TEXT NOT NULL DEFAULT '[]',
	cache_max_bytes INTEGER NOT NULL DEFAULT 1073741824,
  certificate_ciphertext BLOB,
  private_key_ciphertext BLOB,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS deployment_tasks (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  site_id TEXT,
  status TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  deadline_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS publish_task_nodes (
  task_id TEXT NOT NULL REFERENCES deployment_tasks(id) ON DELETE CASCADE,
  node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  target_version INTEGER NOT NULL,
  status TEXT NOT NULL,
  error_code TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '',
  port_conflicts_json TEXT NOT NULL DEFAULT '[]',
  reported_at TEXT,
  PRIMARY KEY(task_id, node_id)
);
CREATE TABLE IF NOT EXISTS cache_operations (
  id TEXT PRIMARY KEY,
  site_id TEXT NOT NULL,
  site_name TEXT NOT NULL,
  kind TEXT NOT NULL,
  retry_of_id TEXT REFERENCES cache_operations(id) ON DELETE SET NULL,
  publish_task_id TEXT REFERENCES deployment_tasks(id) ON DELETE SET NULL,
  scope TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  prewarm_paths_json TEXT NOT NULL DEFAULT '[]',
  cache_generation INTEGER NOT NULL,
  config_version INTEGER NOT NULL,
  status TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  actor TEXT NOT NULL DEFAULT '',
  remote_addr TEXT NOT NULL DEFAULT '',
  completed_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS cache_operation_nodes (
  operation_id TEXT NOT NULL REFERENCES cache_operations(id) ON DELETE CASCADE,
  node_id TEXT NOT NULL,
  node_name TEXT NOT NULL,
  target_version INTEGER NOT NULL DEFAULT 0,
  configuration_status TEXT NOT NULL,
  warmup_status TEXT NOT NULL,
  attempted_urls INTEGER NOT NULL DEFAULT 0,
  succeeded_urls INTEGER NOT NULL DEFAULT 0,
  failures_json TEXT NOT NULL DEFAULT '[]',
  reported_at TEXT,
  PRIMARY KEY(operation_id, node_id)
);
CREATE TABLE IF NOT EXISTS site_deletion_jobs (
  site_id TEXT PRIMARY KEY REFERENCES sites(id) ON DELETE CASCADE,
  task_id TEXT NOT NULL UNIQUE REFERENCES deployment_tasks(id) ON DELETE CASCADE,
  phase TEXT NOT NULL,
  actor TEXT NOT NULL,
  remote_addr TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS dns_bindings (
  id TEXT PRIMARY KEY,
  site_id TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
  node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  domain_name TEXT NOT NULL,
  provider_record_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(site_id, node_id, domain_name)
);
CREATE TABLE IF NOT EXISTS audit_logs (
  id TEXT PRIMARY KEY,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,
  resource_type TEXT NOT NULL,
  resource_id TEXT NOT NULL,
  remote_addr TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS recovery_codes (
  code_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
  used_at TEXT,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS secrets (
  name TEXT PRIMARY KEY,
  ciphertext BLOB NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS control_settings (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  dns_default_ttl_seconds INTEGER NOT NULL DEFAULT 60,
  smtp_override INTEGER NOT NULL DEFAULT 0,
  smtp_enabled INTEGER NOT NULL DEFAULT 0,
  smtp_host TEXT NOT NULL DEFAULT '',
  smtp_port INTEGER NOT NULL DEFAULT 587,
  smtp_username TEXT NOT NULL DEFAULT '',
  smtp_from_address TEXT NOT NULL DEFAULT '',
  smtp_recipients_json TEXT NOT NULL DEFAULT '[]',
	 smtp_notification_categories_json TEXT NOT NULL DEFAULT '["availability","monitoring","certificate","backup"]',
  smtp_security TEXT NOT NULL DEFAULT 'starttls',
  backup_override INTEGER NOT NULL DEFAULT 0,
  backup_repository TEXT NOT NULL DEFAULT '',
  backup_access_key_id TEXT NOT NULL DEFAULT '',
  backup_region TEXT NOT NULL DEFAULT 'us-east-1',
  backup_time TEXT NOT NULL DEFAULT '03:25',
  backup_random_delay_seconds INTEGER NOT NULL DEFAULT 1200,
	cache_default_size_gb INTEGER NOT NULL DEFAULT 1,
  brand_name TEXT NOT NULL DEFAULT 'simple_cdn',
  brand_subtitle TEXT NOT NULL DEFAULT '控制面板',
  brand_logo_data_url TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS node_health (
  node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  consecutive_successes INTEGER NOT NULL DEFAULT 0,
  dns_eligible INTEGER NOT NULL DEFAULT 0,
  last_checked_at TEXT,
  last_error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS node_cache_storage (
  node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
  used_bytes INTEGER NOT NULL,
  total_bytes INTEGER NOT NULL,
  collected_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS monitoring_targets (
  id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
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
CREATE TABLE IF NOT EXISTS notification_delivery_state (
  key TEXT PRIMARY KEY,
  active INTEGER NOT NULL DEFAULT 0,
  last_sent_at TEXT NOT NULL,
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
CREATE TABLE IF NOT EXISTS site_node_health (
  site_id TEXT NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
  node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  consecutive_successes INTEGER NOT NULL DEFAULT 0,
  dns_eligible INTEGER NOT NULL DEFAULT 0,
  last_checked_at TEXT,
  last_error TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(site_id, node_id)
);
CREATE TABLE IF NOT EXISTS node_uninstall_jobs (
  node_id TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
  status TEXT NOT NULL,
  previous_status TEXT NOT NULL,
  token_hash TEXT UNIQUE,
  token_expires_at TEXT,
  ready_at TEXT NOT NULL,
  affected_site_ids_json TEXT NOT NULL DEFAULT '[]',
  detail TEXT NOT NULL DEFAULT '',
  forced INTEGER NOT NULL DEFAULT 0,
  started_at TEXT,
  completed_at TEXT,
  created_at TEXT NOT NULL,
	  updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS node_upgrade_tasks (
	  id TEXT PRIMARY KEY,
	  node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	  status TEXT NOT NULL,
	  source_sha256 TEXT NOT NULL DEFAULT '',
	  target_sha256 TEXT NOT NULL,
	  source_nginx_sha256 TEXT NOT NULL DEFAULT '',
	  target_nginx_sha256 TEXT NOT NULL DEFAULT '',
	  binary_url TEXT NOT NULL,
	  installer_url TEXT NOT NULL,
	  installer_sha256 TEXT NOT NULL,
	  agent_service_url TEXT NOT NULL,
	  agent_service_sha256 TEXT NOT NULL,
	  updater_service_url TEXT NOT NULL,
	  updater_service_sha256 TEXT NOT NULL,
	  nginx_bundle_url TEXT NOT NULL DEFAULT '',
	  nginx_bundle_sha256 TEXT NOT NULL DEFAULT '',
	  nginx_service_url TEXT NOT NULL DEFAULT '',
	  nginx_service_sha256 TEXT NOT NULL DEFAULT '',
	  error_code TEXT NOT NULL DEFAULT '',
	  detail TEXT NOT NULL DEFAULT '',
	  deadline_at TEXT NOT NULL,
	  started_at TEXT,
	  completed_at TEXT,
	  created_at TEXT NOT NULL,
	  updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS security_policies (
	  id TEXT PRIMARY KEY,
	  name TEXT NOT NULL UNIQUE,
	  enabled INTEGER NOT NULL DEFAULT 1,
	  site_ids_json TEXT NOT NULL DEFAULT '[]',
	  conditions_json TEXT NOT NULL DEFAULT '[]',
	  pattern TEXT NOT NULL,
	  action TEXT NOT NULL,
	  ban_duration_seconds INTEGER NOT NULL DEFAULT 0,
	  response_status INTEGER NOT NULL DEFAULT 403,
	  priority INTEGER NOT NULL,
	  created_at TEXT NOT NULL,
	  updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS pow_policies (
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
	CREATE TABLE IF NOT EXISTS static_assets (
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
	CREATE TABLE IF NOT EXISTS nginx_artifacts (
	  sha256 TEXT PRIMARY KEY,
	  version TEXT NOT NULL UNIQUE,
	  state TEXT NOT NULL CHECK (state IN ('candidate', 'current', 'retired')),
	  release_tag TEXT NOT NULL UNIQUE,
	  source_url TEXT NOT NULL,
	  official_source_url TEXT NOT NULL,
	  source_sha256 TEXT NOT NULL,
	  build_commit TEXT NOT NULL,
	  size_bytes INTEGER NOT NULL,
	  downloaded_at TEXT NOT NULL,
	  promoted_at TEXT
	);
	CREATE TABLE IF NOT EXISTS rate_limit_policies (
	  id TEXT PRIMARY KEY,
	  name TEXT NOT NULL UNIQUE,
	  enabled INTEGER NOT NULL DEFAULT 1,
	  requests_per_second INTEGER NOT NULL,
	  response_condition_enabled INTEGER NOT NULL DEFAULT 0,
	  response_status_classes_json TEXT NOT NULL DEFAULT '[]',
	  ban_enabled INTEGER NOT NULL DEFAULT 0,
	  ban_after_consecutive_429 INTEGER NOT NULL DEFAULT 3,
	  ban_duration_seconds INTEGER NOT NULL DEFAULT 3600,
	  created_at TEXT NOT NULL,
	  updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS security_bans (
	  ip TEXT PRIMARY KEY,
	  policy_id TEXT REFERENCES security_policies(id) ON DELETE SET NULL,
	  rate_limit_policy_id TEXT REFERENCES rate_limit_policies(id) ON DELETE SET NULL,
	  policy_name TEXT NOT NULL,
	  trigger_node_id TEXT REFERENCES nodes(id) ON DELETE SET NULL,
	  host TEXT NOT NULL DEFAULT '',
	  path TEXT NOT NULL DEFAULT '',
	  method TEXT NOT NULL DEFAULT '',
	  expires_at TEXT NOT NULL,
	  created_at TEXT NOT NULL,
	  updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS security_events (
	  id TEXT PRIMARY KEY,
	  node_id TEXT REFERENCES nodes(id) ON DELETE SET NULL,
	  policy_id TEXT REFERENCES security_policies(id) ON DELETE SET NULL,
	  rate_limit_policy_id TEXT REFERENCES rate_limit_policies(id) ON DELETE SET NULL,
	  policy_name TEXT NOT NULL,
	  site_id TEXT NOT NULL DEFAULT '',
	  client_ip TEXT NOT NULL,
	  host TEXT NOT NULL DEFAULT '',
	  path TEXT NOT NULL,
	  raw_uri TEXT NOT NULL DEFAULT '',
	  query_string TEXT NOT NULL DEFAULT '',
	  user_agent TEXT NOT NULL DEFAULT '',
	  matched_field TEXT NOT NULL DEFAULT '',
	  method TEXT NOT NULL DEFAULT '',
	  action TEXT NOT NULL,
	  observed_at TEXT NOT NULL,
	  ban_expires_at TEXT,
	  created_at TEXT NOT NULL
	);
CREATE INDEX IF NOT EXISTS idx_tasks_created_at ON deployment_tasks(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_publish_task_nodes_node ON publish_task_nodes(node_id, status);
CREATE INDEX IF NOT EXISTS idx_cache_operations_created ON cache_operations(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_cache_operations_site_created ON cache_operations(site_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_cache_operations_status ON cache_operations(status, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_cache_operation_nodes_node ON cache_operation_nodes(node_id, operation_id);
CREATE INDEX IF NOT EXISTS idx_nodes_status ON nodes(status);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);
	CREATE INDEX IF NOT EXISTS idx_site_node_health_node ON site_node_health(node_id);
	CREATE INDEX IF NOT EXISTS idx_node_upgrade_tasks_node ON node_upgrade_tasks(node_id, created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_security_policies_priority ON security_policies(priority, created_at);
	CREATE INDEX IF NOT EXISTS idx_pow_policies_priority ON pow_policies(priority, created_at);
	CREATE INDEX IF NOT EXISTS idx_static_asset_bindings_asset ON static_asset_bindings(asset_id, site_id);
	CREATE INDEX IF NOT EXISTS idx_static_asset_bindings_site ON static_asset_bindings(site_id, url_path);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_nginx_artifacts_current ON nginx_artifacts(state) WHERE state = 'current';
	CREATE INDEX IF NOT EXISTS idx_nginx_artifacts_state_downloaded ON nginx_artifacts(state, downloaded_at DESC);
	CREATE INDEX IF NOT EXISTS idx_security_bans_expires ON security_bans(expires_at);
	CREATE INDEX IF NOT EXISTS idx_security_events_created ON security_events(created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_monitoring_probe_target ON monitoring_probe_results(target_id, checked_at DESC);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_monitoring_targets_name ON monitoring_targets(name COLLATE NOCASE);
`

func seedBuiltinSecurityPoliciesTx(tx *sql.Tx) error {
	seededAt := stamp(now())
	if _, err := tx.Exec(`UPDATE security_policies SET pattern = ?, updated_at = ?
		WHERE id = ? AND pattern = ?`, domain.DefaultSecurityPolicyPattern, seededAt,
		domain.DefaultSecurityPolicyID, legacyDefaultSecurityPolicyPattern); err != nil {
		return fmt.Errorf("upgrade built-in sensitive-file security policy: %w", err)
	}
	policies := []struct {
		id         string
		name       string
		condition  domain.SecurityCondition
		action     domain.SecurityPolicyAction
		banSeconds int
		priority   int
	}{
		{
			id: domain.DefaultSecurityPolicyID, name: "敏感文件扫描",
			condition:  domain.SecurityCondition{Field: domain.SecurityFieldPath, Operator: domain.SecurityOperatorRegex, Value: domain.DefaultSecurityPolicyPattern},
			action:     domain.SecurityActionBan,
			banSeconds: 21600, priority: 100,
		},
		{
			id: domain.DefaultPHPSecurityPolicyID, name: "PHP 恶意文件探测",
			condition: domain.SecurityCondition{Field: domain.SecurityFieldPath, Operator: domain.SecurityOperatorRegex, Value: domain.DefaultPHPSecurityPolicyPattern},
			action:    domain.SecurityActionBlock,
			priority:  200,
		},
		{
			id: domain.DefaultPathTraversalPolicyID, name: "路径穿越探测",
			condition: domain.SecurityCondition{Field: domain.SecurityFieldRawURI, Operator: domain.SecurityOperatorRegex, Value: domain.DefaultPathTraversalPolicyPattern},
			action:    domain.SecurityActionBlock, priority: 300,
		},
		{
			id: domain.DefaultSQLInjectionPolicyID, name: "SQL 注入探测",
			condition: domain.SecurityCondition{Field: domain.SecurityFieldQuery, Operator: domain.SecurityOperatorRegex, Value: domain.DefaultSQLInjectionPolicyPattern},
			action:    domain.SecurityActionBlock, priority: 400,
		},
		{
			id: domain.DefaultXSSPolicyID, name: "跨站脚本探测",
			condition: domain.SecurityCondition{Field: domain.SecurityFieldQuery, Operator: domain.SecurityOperatorRegex, Value: domain.DefaultXSSPolicyPattern},
			action:    domain.SecurityActionBlock, priority: 500,
		},
		{
			id: domain.DefaultScannerUAPolicyID, name: "扫描器 User-Agent",
			condition: domain.SecurityCondition{Field: domain.SecurityFieldUserAgent, Operator: domain.SecurityOperatorRegex, Value: domain.DefaultScannerUAPolicyPattern},
			action:    domain.SecurityActionBlock, priority: 600,
		},
	}
	for _, policy := range policies {
		conditions, err := json.Marshal([]domain.SecurityCondition{policy.condition})
		if err != nil {
			return fmt.Errorf("encode built-in security policy %q: %w", policy.name, err)
		}
		if _, err := tx.Exec(`INSERT INTO security_policies(
			id, name, enabled, site_ids_json, conditions_json, pattern, action, ban_duration_seconds,
			response_status, priority, created_at, updated_at)
			VALUES (?, ?, 1, '[]', ?, ?, ?, ?, 403, ?, ?, ?) ON CONFLICT(id) DO NOTHING`, policy.id,
			policy.name, string(conditions), policy.condition.Value, policy.action, policy.banSeconds,
			policy.priority, seededAt, seededAt); err != nil {
			return fmt.Errorf("seed built-in security policy %q: %w", policy.name, err)
		}
	}
	return nil
}

func backfillSiteDomainsTx(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id, domains_json FROM sites`)
	if err != nil {
		return err
	}
	type existingSite struct {
		id      string
		domains []string
	}
	var sites []existingSite
	for rows.Next() {
		var siteID, encoded string
		if err := rows.Scan(&siteID, &encoded); err != nil {
			rows.Close()
			return err
		}
		var domains []string
		if err := json.Unmarshal([]byte(encoded), &domains); err != nil {
			rows.Close()
			return fmt.Errorf("decode domains for site %s: %w", siteID, err)
		}
		sites = append(sites, existingSite{id: siteID, domains: domains})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	publicationRows, err := tx.Query(`SELECT site_json FROM site_publications`)
	if err != nil {
		return err
	}
	for publicationRows.Next() {
		var encoded string
		if err := publicationRows.Scan(&encoded); err != nil {
			publicationRows.Close()
			return err
		}
		var site domain.Site
		if err := json.Unmarshal([]byte(encoded), &site); err != nil {
			publicationRows.Close()
			return fmt.Errorf("decode published site domains: %w", err)
		}
		sites = append(sites, existingSite{id: site.ID, domains: site.Domains})
	}
	if err := publicationRows.Err(); err != nil {
		publicationRows.Close()
		return err
	}
	if err := publicationRows.Close(); err != nil {
		return err
	}
	for _, site := range sites {
		for _, domainName := range site.domains {
			var owner string
			err := tx.QueryRow(`SELECT site_id FROM site_domains WHERE domain_name = ?`, domainName).Scan(&owner)
			if errors.Is(err, sql.ErrNoRows) {
				if _, err := tx.Exec(`INSERT INTO site_domains(domain_name, site_id) VALUES (?, ?)`, domainName, site.id); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			if owner != site.id {
				return fmt.Errorf("domain %s belongs to multiple existing sites", domainName)
			}
		}
	}
	return nil
}

func now() time.Time           { return time.Now().UTC().Round(0) }
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(value string) (time.Time, error) { return time.Parse(time.RFC3339Nano, value) }

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) HasAdmin() (bool, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM admin_users").Scan(&count)
	return count > 0, err
}

func (s *Store) CreateInitialAdmin(passwordHash, totpSecret string) error {
	return s.CreateInitialAdminWithRecoveryCodes(passwordHash, totpSecret, nil)
}

func (s *Store) CreateInitialAdminWithRecoveryCodes(passwordHash, totpSecret string, recoveryCodeHashes []string) error {
	return s.createInitialAdminWithRecoveryCodes(passwordHash, totpSecret, recoveryCodeHashes, nil)
}

func (s *Store) CreateInitialAdminWithRecoveryCodesAndTOTPCounter(passwordHash, totpSecret string, recoveryCodeHashes []string, totpCounter int64) error {
	return s.createInitialAdminWithRecoveryCodes(passwordHash, totpSecret, recoveryCodeHashes, &totpCounter)
}

func (s *Store) createInitialAdminWithRecoveryCodes(passwordHash, totpSecret string, recoveryCodeHashes []string, totpCounter *int64) error {
	if passwordHash == "" || totpSecret == "" {
		return errors.New("password hash and totp secret are required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow("SELECT COUNT(*) FROM admin_users").Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return errors.New("an admin account already exists")
	}
	ts := stamp(now())
	_, err = tx.Exec("INSERT INTO admin_users(id, password_hash, totp_secret, last_totp_counter, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)", "admin", passwordHash, totpSecret, totpCounter, ts, ts)
	if err != nil {
		return err
	}
	for _, hash := range recoveryCodeHashes {
		if strings.TrimSpace(hash) == "" {
			return errors.New("recovery code hash is required")
		}
		if _, err := tx.Exec(`INSERT INTO recovery_codes(code_hash, user_id, created_at) VALUES (?, ?, ?)`, hash, "admin", ts); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ReplacePassword(userID, passwordHash string) error {
	result, err := s.db.Exec(`UPDATE admin_users SET password_hash = ?, updated_at = ? WHERE id = ?`, passwordHash, stamp(now()), userID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ReplaceAdminTOTPSecret(userID, totpSecret string) error {
	if strings.TrimSpace(totpSecret) == "" {
		return errors.New("TOTP secret is required")
	}
	result, err := s.db.Exec(`UPDATE admin_users SET totp_secret = ?, updated_at = ? WHERE id = ?`, totpSecret, stamp(now()), userID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ReplaceAdminTOTPSecretWithCounter(userID, totpSecret string, totpCounter int64) error {
	if strings.TrimSpace(totpSecret) == "" {
		return errors.New("TOTP secret is required")
	}
	result, err := s.db.Exec(`UPDATE admin_users SET totp_secret = ?, last_totp_counter = ?, updated_at = ? WHERE id = ?`, totpSecret, totpCounter, stamp(now()), userID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ReplaceRecoveryCodes(userID string, hashes []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for _, hash := range hashes {
		if _, err := tx.Exec(`INSERT INTO recovery_codes(code_hash, user_id, created_at) VALUES (?, ?, ?)`, hash, userID, stamp(now())); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ConsumeRecoveryCode(codeHash string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var userID string
	var usedAt sql.NullString
	err = tx.QueryRow(`SELECT user_id, used_at FROM recovery_codes WHERE code_hash = ?`, codeHash).Scan(&userID, &usedAt)
	if errors.Is(err, sql.ErrNoRows) || usedAt.Valid {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	result, err := tx.Exec(`UPDATE recovery_codes SET used_at = ? WHERE code_hash = ? AND used_at IS NULL`, stamp(now()), codeHash)
	if err != nil {
		return "", err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return "", ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return userID, nil
}

type Admin struct {
	ID              string
	PasswordHash    string
	TOTPSecret      string
	PasskeyEnabled  bool
	LastTOTPCounter *int64
}

func (s *Store) Admin() (Admin, error) {
	var admin Admin
	var lastTOTPCounter sql.NullInt64
	err := s.db.QueryRow("SELECT id, password_hash, totp_secret, passkey_enabled, last_totp_counter FROM admin_users LIMIT 1").Scan(&admin.ID, &admin.PasswordHash, &admin.TOTPSecret, &admin.PasskeyEnabled, &lastTOTPCounter)
	if errors.Is(err, sql.ErrNoRows) {
		return Admin{}, ErrNotFound
	}
	if err == nil && lastTOTPCounter.Valid {
		value := lastTOTPCounter.Int64
		admin.LastTOTPCounter = &value
	}
	return admin, err
}

func (s *Store) CreateSession(userID, token, csrf string, expiresAt time.Time) error {
	_, err := s.db.Exec("INSERT INTO sessions(id, user_id, token_hash, csrf_token, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?)", uuid.NewString(), userID, hashToken(token), csrf, stamp(expiresAt), stamp(now()))
	return err
}

type Session struct {
	UserID          string
	CSRFToken       string
	AuthMethod      string
	AuthenticatorID string
	AuthenticatedAt *time.Time
	ElevatedUntil   *time.Time
	ExpiresAt       time.Time
}

func (s *Store) Session(token string) (Session, error) {
	var session Session
	var authenticatedAt, elevatedUntil sql.NullString
	var expiresAt string
	err := s.db.QueryRow(`SELECT user_id, csrf_token, auth_method, authenticator_id, authenticated_at, elevated_until, expires_at
		FROM sessions WHERE token_hash = ?`, hashToken(token)).Scan(&session.UserID, &session.CSRFToken, &session.AuthMethod, &session.AuthenticatorID, &authenticatedAt, &elevatedUntil, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	session.ExpiresAt, err = parseTime(expiresAt)
	if err != nil {
		return Session{}, err
	}
	if !session.ExpiresAt.After(now()) {
		_ = s.DeleteSession(token)
		return Session{}, ErrNotFound
	}
	if authenticatedAt.Valid {
		value, parseErr := parseTime(authenticatedAt.String)
		if parseErr != nil {
			return Session{}, parseErr
		}
		session.AuthenticatedAt = &value
	}
	if elevatedUntil.Valid {
		value, parseErr := parseTime(elevatedUntil.String)
		if parseErr != nil {
			return Session{}, parseErr
		}
		session.ElevatedUntil = &value
	}
	return session, nil
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE token_hash = ?", hashToken(token))
	return err
}

func (s *Store) CreateNode(name, publicIPv4 string) (domain.Node, error) {
	if strings.TrimSpace(name) == "" {
		return domain.Node{}, errors.New("node name is required")
	}
	parsed := net.ParseIP(strings.TrimSpace(publicIPv4))
	if parsed == nil || parsed.To4() == nil || parsed.IsUnspecified() || parsed.IsLoopback() || parsed.IsLinkLocalUnicast() || parsed.IsMulticast() || parsed.IsPrivate() {
		return domain.Node{}, errors.New("a valid public IPv4 address is required")
	}
	created := now()
	node := domain.Node{ID: uuid.NewString(), Name: strings.TrimSpace(name), PublicIPv4: parsed.String(), NginxCapacity: domain.DefaultNginxCapacity(), Status: domain.NodePending, Capabilities: []string{}, CreatedAt: created, UpdatedAt: created}
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Node{}, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO nodes(id, name, public_ipv4, status, nginx_worker_processes, nginx_worker_connections, nginx_worker_rlimit_nofile, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, node.ID, node.Name, node.PublicIPv4, node.Status, node.NginxCapacity.WorkerProcesses, node.NginxCapacity.WorkerConnections, node.NginxCapacity.WorkerRlimitNoFile, stamp(created), stamp(created)); err != nil {
		return domain.Node{}, fmt.Errorf("create node: %w", err)
	}
	if err := insertDefaultSmartRoutingPolicyTx(tx, node.ID, created); err != nil {
		return domain.Node{}, fmt.Errorf("create node smart routing policy: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Node{}, err
	}
	return node, nil
}

func (s *Store) GetNode(id string) (domain.Node, error) {
	return scanNode(s.db.QueryRow(`SELECT id, name, public_ipv4, status, monitor_auto_paused, capabilities_json, cache_max_size_gb, nginx_worker_processes, nginx_worker_connections, nginx_worker_rlimit_nofile, agent_version, agent_sha256, nginx_version, nginx_sha256, active_upgrade_task_id, last_heartbeat_at, applied_version, last_error, created_at, updated_at FROM nodes WHERE id = ?`, id))
}

func (s *Store) ListNodes() ([]domain.Node, error) {
	rows, err := s.db.Query(`SELECT id, name, public_ipv4, status, monitor_auto_paused, capabilities_json, cache_max_size_gb, nginx_worker_processes, nginx_worker_connections, nginx_worker_rlimit_nofile, agent_version, agent_sha256, nginx_version, nginx_sha256, active_upgrade_task_id, last_heartbeat_at, applied_version, last_error, created_at, updated_at FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes := make([]domain.Node, 0)
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

func (s *Store) SetNodeCacheMaxSizeGB(nodeID string, size *int) (domain.Node, error) {
	if size != nil {
		if err := domain.ValidateCacheMaxSizeGB(*size); err != nil {
			return domain.Node{}, err
		}
	}
	result, err := s.db.Exec(`UPDATE nodes SET cache_max_size_gb = ?, updated_at = ? WHERE id = ?`, size, stamp(now()), nodeID)
	if err != nil {
		return domain.Node{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.Node{}, err
	}
	if changed != 1 {
		return domain.Node{}, ErrNotFound
	}
	return s.GetNode(nodeID)
}

func (s *Store) SetNodeNginxCapacity(nodeID string, capacity domain.NginxCapacity) (domain.Node, error) {
	capacity, err := domain.NormalizeNginxCapacity(capacity)
	if err != nil {
		return domain.Node{}, err
	}
	result, err := s.db.Exec(`UPDATE nodes SET nginx_worker_processes = ?, nginx_worker_connections = ?, nginx_worker_rlimit_nofile = ?, updated_at = ? WHERE id = ?`, capacity.WorkerProcesses, capacity.WorkerConnections, capacity.WorkerRlimitNoFile, stamp(now()), nodeID)
	if err != nil {
		return domain.Node{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.Node{}, err
	}
	if changed != 1 {
		return domain.Node{}, ErrNotFound
	}
	return s.GetNode(nodeID)
}

type scanner interface{ Scan(...any) error }

func scanNode(row scanner) (domain.Node, error) {
	var node domain.Node
	var capabilities string
	var monitorAutoPaused int
	var cacheMaxSizeGB sql.NullInt64
	var heartbeat sql.NullString
	var createdAt, updatedAt string
	err := row.Scan(&node.ID, &node.Name, &node.PublicIPv4, &node.Status, &monitorAutoPaused, &capabilities, &cacheMaxSizeGB, &node.NginxCapacity.WorkerProcesses, &node.NginxCapacity.WorkerConnections, &node.NginxCapacity.WorkerRlimitNoFile, &node.AgentVersion, &node.AgentSHA256, &node.NginxVersion, &node.NginxSHA256, &node.ActiveUpgradeID, &heartbeat, &node.AppliedVersion, &node.LastError, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Node{}, ErrNotFound
	}
	if err != nil {
		return domain.Node{}, err
	}
	node.MonitorAutoPaused = monitorAutoPaused != 0
	if err := json.Unmarshal([]byte(capabilities), &node.Capabilities); err != nil {
		return domain.Node{}, fmt.Errorf("decode node capabilities: %w", err)
	}
	if cacheMaxSizeGB.Valid {
		value := int(cacheMaxSizeGB.Int64)
		node.CacheMaxSizeGB = &value
	}
	node.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return domain.Node{}, err
	}
	node.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return domain.Node{}, err
	}
	if heartbeat.Valid {
		value, err := parseTime(heartbeat.String)
		if err != nil {
			return domain.Node{}, err
		}
		node.LastHeartbeatAt = &value
	}
	return node, nil
}

func (s *Store) CreateEnrollmentToken(nodeID, token string, expiresAt time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status domain.NodeStatus
	var activeUninstall int
	err = tx.QueryRow(`SELECT nodes.status, EXISTS(
		SELECT 1 FROM node_uninstall_jobs WHERE node_id = nodes.id AND status IN (?, ?, ?, ?)
	) FROM nodes WHERE nodes.id = ?`, NodeUninstallPreparing, NodeUninstallReady, NodeUninstallRunning, NodeUninstallFailed, nodeID).Scan(&status, &activeUninstall)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status == domain.NodeRevoked || status == domain.NodeUninstalling || status == domain.NodeUninstalled {
		return errors.New("cannot enroll a revoked, uninstalling, or uninstalled node")
	}
	if activeUninstall != 0 {
		return ErrUninstallActive
	}
	if _, err := tx.Exec(`INSERT INTO enrollment_tokens(token_hash, node_id, expires_at, created_at) VALUES (?, ?, ?, ?)`, hashToken(token), nodeID, stamp(expiresAt), stamp(now())); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) NodeRequiresEnrollment(nodeID string) (bool, error) {
	var status domain.NodeStatus
	var fingerprint sql.NullString
	var activeUninstall int
	err := s.db.QueryRow(`SELECT nodes.status, nodes.cert_fingerprint, EXISTS(
		SELECT 1 FROM node_uninstall_jobs WHERE node_id = nodes.id AND status IN (?, ?, ?, ?)
	) FROM nodes WHERE nodes.id = ?`, NodeUninstallPreparing, NodeUninstallReady, NodeUninstallRunning, NodeUninstallFailed, nodeID).Scan(&status, &fingerprint, &activeUninstall)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if status == domain.NodeRevoked || status == domain.NodeUninstalling || status == domain.NodeUninstalled {
		return false, errors.New("cannot deploy a revoked, uninstalling, or uninstalled node")
	}
	if activeUninstall != 0 {
		return false, ErrUninstallActive
	}
	return !fingerprint.Valid || strings.TrimSpace(fingerprint.String) == "", nil
}

func (s *Store) ConsumeEnrollmentToken(token string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var nodeID, expiresAt string
	var nodeStatus domain.NodeStatus
	var activeUninstall int
	var usedAt sql.NullString
	err = tx.QueryRow(`SELECT enrollment_tokens.node_id, enrollment_tokens.expires_at, enrollment_tokens.used_at, nodes.status, EXISTS(
		SELECT 1 FROM node_uninstall_jobs WHERE node_id = nodes.id AND status IN (?, ?, ?, ?)
	) FROM enrollment_tokens JOIN nodes ON nodes.id = enrollment_tokens.node_id WHERE enrollment_tokens.token_hash = ?`,
		NodeUninstallPreparing, NodeUninstallReady, NodeUninstallRunning, NodeUninstallFailed, hashToken(token)).Scan(&nodeID, &expiresAt, &usedAt, &nodeStatus, &activeUninstall)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrTokenInvalid
	}
	if err != nil {
		return "", err
	}
	expires, err := parseTime(expiresAt)
	if err != nil || nodeStatus == domain.NodeRevoked || nodeStatus == domain.NodeUninstalling || nodeStatus == domain.NodeUninstalled || activeUninstall != 0 || usedAt.Valid || !expires.After(now()) {
		return "", ErrTokenInvalid
	}
	result, err := tx.Exec(`UPDATE enrollment_tokens SET used_at = ? WHERE token_hash = ? AND used_at IS NULL`, stamp(now()), hashToken(token))
	if err != nil {
		return "", err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return "", ErrTokenInvalid
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return nodeID, nil
}

func (s *Store) SetNodeCertificate(nodeID, fingerprint string) error {
	result, err := s.db.Exec(`UPDATE nodes SET cert_fingerprint = ?, updated_at = ? WHERE id = ? AND status NOT IN (?, ?, ?)
		AND NOT EXISTS (SELECT 1 FROM node_uninstall_jobs WHERE node_id = nodes.id AND status IN (?, ?, ?, ?))`,
		fingerprint, stamp(now()), nodeID, domain.NodeRevoked, domain.NodeUninstalling, domain.NodeUninstalled,
		NodeUninstallPreparing, NodeUninstallReady, NodeUninstallRunning, NodeUninstallFailed)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetNodeCapabilities(nodeID string, capabilities []string) error {
	if len(capabilities) > 32 {
		return errors.New("too many edge capabilities")
	}
	normalized := make([]string, 0, len(capabilities))
	seen := make(map[string]bool, len(capabilities))
	for _, capability := range capabilities {
		capability = strings.TrimSpace(capability)
		if capability == "" || len(capability) > 64 {
			return errors.New("invalid edge capability")
		}
		for _, character := range capability {
			if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '-') {
				return errors.New("invalid edge capability")
			}
		}
		if !seen[capability] {
			seen[capability] = true
			normalized = append(normalized, capability)
		}
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	result, err := s.db.Exec(`UPDATE nodes SET capabilities_json = ?, updated_at = ? WHERE id = ?`, string(encoded), stamp(now()), nodeID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) NodeIDByFingerprint(fingerprint string) (string, error) {
	var nodeID string
	err := s.db.QueryRow(`SELECT id FROM nodes WHERE cert_fingerprint = ? AND status NOT IN (?, ?, ?)`, fingerprint, domain.NodeRevoked, domain.NodeUninstalling, domain.NodeUninstalled).Scan(&nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return nodeID, err
}

func (s *Store) Heartbeat(nodeID string, appliedVersion int64, lastError string, report *domain.ApplyReport) error {
	return s.HeartbeatWithAgent(nodeID, appliedVersion, lastError, report, "", "")
}

func (s *Store) HeartbeatWithAgent(nodeID string, appliedVersion int64, lastError string, report *domain.ApplyReport, agentSHA256, activeUpgradeID string) error {
	return s.HeartbeatWithAgentVersion(nodeID, appliedVersion, lastError, report, "", agentSHA256, activeUpgradeID)
}

func (s *Store) HeartbeatWithAgentVersion(nodeID string, appliedVersion int64, lastError string, report *domain.ApplyReport, agentVersion, agentSHA256, activeUpgradeID string) error {
	return s.HeartbeatWithArtifacts(nodeID, appliedVersion, lastError, report, agentVersion, agentSHA256, "", "", activeUpgradeID)
}

func (s *Store) HeartbeatWithArtifacts(nodeID string, appliedVersion int64, lastError string, report *domain.ApplyReport, agentVersion, agentSHA256, nginxVersion, nginxSHA256, activeUpgradeID string) error {
	// Record a structured apply result before advancing applied_version. The
	// latter is a compatibility signal for old agents, and a concurrent health
	// reconciliation could otherwise consume it first and replace the richer
	// report with a generic confirmation.
	if report != nil {
		if err := s.RecordPublishApply(nodeID, *report); err != nil {
			return err
		}
	}
	status := domain.NodeActive
	result, err := s.db.Exec(`UPDATE nodes SET status = CASE WHEN status = ? THEN ? ELSE status END,
		last_heartbeat_at = ?, applied_version = CASE WHEN ? > applied_version THEN ? ELSE applied_version END,
		agent_version = ?,
		agent_sha256 = CASE WHEN ? = '' THEN agent_sha256 ELSE ? END,
		nginx_version = CASE WHEN ? = '' THEN nginx_version ELSE ? END,
		nginx_sha256 = CASE WHEN ? = '' THEN nginx_sha256 ELSE ? END, active_upgrade_task_id = ?,
		last_error = ?, updated_at = ? WHERE id = ?`, domain.NodePending, status, stamp(now()), appliedVersion, appliedVersion,
		agentVersion, agentSHA256, agentSHA256, nginxVersion, nginxVersion, nginxSHA256, nginxSHA256,
		activeUpgradeID, lastError, stamp(now()), nodeID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrNotFound
	}
	if err := s.ReconcileNodeUpgrades(); err != nil {
		return err
	}
	return s.ReconcilePublishTasks()
}

type NodeHealth struct {
	NodeID               string
	ConsecutiveFailures  int
	ConsecutiveSuccesses int
	DNSEligible          bool
	LastCheckedAt        *time.Time
	LastError            string
}

func (s *Store) RecordNodeHealth(nodeID string, healthy bool, lastError string) (NodeHealth, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return NodeHealth{}, err
	}
	defer tx.Rollback()
	var health NodeHealth
	var checked sql.NullString
	err = tx.QueryRow(`SELECT node_id, consecutive_failures, consecutive_successes, dns_eligible, last_checked_at, last_error FROM node_health WHERE node_id = ?`, nodeID).Scan(&health.NodeID, &health.ConsecutiveFailures, &health.ConsecutiveSuccesses, &health.DNSEligible, &checked, &health.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		health.NodeID = nodeID
		health.LastError = ""
	} else if err != nil {
		return NodeHealth{}, err
	}
	if healthy {
		health.ConsecutiveSuccesses++
		health.ConsecutiveFailures = 0
		health.LastError = ""
		if health.ConsecutiveSuccesses >= 5 {
			health.DNSEligible = true
		}
	} else {
		health.ConsecutiveFailures++
		health.ConsecutiveSuccesses = 0
		health.LastError = lastError
		if health.ConsecutiveFailures >= 3 {
			health.DNSEligible = false
		}
	}
	checkedAt := now()
	_, err = tx.Exec(`INSERT INTO node_health(node_id, consecutive_failures, consecutive_successes, dns_eligible, last_checked_at, last_error) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(node_id) DO UPDATE SET consecutive_failures=excluded.consecutive_failures, consecutive_successes=excluded.consecutive_successes, dns_eligible=excluded.dns_eligible, last_checked_at=excluded.last_checked_at, last_error=excluded.last_error`, nodeID, health.ConsecutiveFailures, health.ConsecutiveSuccesses, boolInt(health.DNSEligible), stamp(checkedAt), health.LastError)
	if err != nil {
		return NodeHealth{}, err
	}
	if err := tx.Commit(); err != nil {
		return NodeHealth{}, err
	}
	health.LastCheckedAt = &checkedAt
	return health, nil
}

func (s *Store) NodeHealth(nodeID string) (NodeHealth, error) {
	var health NodeHealth
	var checked sql.NullString
	err := s.db.QueryRow(`SELECT node_id, consecutive_failures, consecutive_successes, dns_eligible, last_checked_at, last_error FROM node_health WHERE node_id = ?`, nodeID).Scan(&health.NodeID, &health.ConsecutiveFailures, &health.ConsecutiveSuccesses, &health.DNSEligible, &checked, &health.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return NodeHealth{NodeID: nodeID}, nil
	}
	if err != nil {
		return NodeHealth{}, err
	}
	if checked.Valid {
		parsed, err := parseTime(checked.String)
		if err != nil {
			return NodeHealth{}, err
		}
		health.LastCheckedAt = &parsed
	}
	return health, nil
}

type SiteNodeHealth struct {
	SiteID               string
	NodeID               string
	ConsecutiveFailures  int
	ConsecutiveSuccesses int
	DNSEligible          bool
	LastCheckedAt        *time.Time
	LastError            string
}

func (s *Store) RecordSiteNodeHealth(siteID, nodeID string, healthy bool, lastError string) (SiteNodeHealth, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return SiteNodeHealth{}, err
	}
	defer tx.Rollback()
	var health SiteNodeHealth
	var checked sql.NullString
	err = tx.QueryRow(`SELECT site_id, node_id, consecutive_failures, consecutive_successes, dns_eligible, last_checked_at, last_error FROM site_node_health WHERE site_id = ? AND node_id = ?`, siteID, nodeID).
		Scan(&health.SiteID, &health.NodeID, &health.ConsecutiveFailures, &health.ConsecutiveSuccesses, &health.DNSEligible, &checked, &health.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		health.SiteID = siteID
		health.NodeID = nodeID
	} else if err != nil {
		return SiteNodeHealth{}, err
	}
	if healthy {
		health.ConsecutiveSuccesses++
		health.ConsecutiveFailures = 0
		health.LastError = ""
		if health.ConsecutiveSuccesses >= 5 {
			health.DNSEligible = true
		}
	} else {
		health.ConsecutiveFailures++
		health.ConsecutiveSuccesses = 0
		health.LastError = lastError
		if health.ConsecutiveFailures >= 3 {
			health.DNSEligible = false
		}
	}
	checkedAt := now()
	_, err = tx.Exec(`INSERT INTO site_node_health(site_id, node_id, consecutive_failures, consecutive_successes, dns_eligible, last_checked_at, last_error) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(site_id, node_id) DO UPDATE SET consecutive_failures=excluded.consecutive_failures, consecutive_successes=excluded.consecutive_successes, dns_eligible=excluded.dns_eligible, last_checked_at=excluded.last_checked_at, last_error=excluded.last_error`,
		siteID, nodeID, health.ConsecutiveFailures, health.ConsecutiveSuccesses, boolInt(health.DNSEligible), stamp(checkedAt), health.LastError)
	if err != nil {
		return SiteNodeHealth{}, err
	}
	if err := tx.Commit(); err != nil {
		return SiteNodeHealth{}, err
	}
	health.LastCheckedAt = &checkedAt
	return health, nil
}

func (s *Store) SiteNodeHealth(siteID, nodeID string) (SiteNodeHealth, error) {
	var health SiteNodeHealth
	var checked sql.NullString
	err := s.db.QueryRow(`SELECT site_id, node_id, consecutive_failures, consecutive_successes, dns_eligible, last_checked_at, last_error FROM site_node_health WHERE site_id = ? AND node_id = ?`, siteID, nodeID).
		Scan(&health.SiteID, &health.NodeID, &health.ConsecutiveFailures, &health.ConsecutiveSuccesses, &health.DNSEligible, &checked, &health.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return SiteNodeHealth{SiteID: siteID, NodeID: nodeID}, nil
	}
	if err != nil {
		return SiteNodeHealth{}, err
	}
	if checked.Valid {
		parsed, err := parseTime(checked.String)
		if err != nil {
			return SiteNodeHealth{}, err
		}
		health.LastCheckedAt = &parsed
	}
	return health, nil
}

func (s *Store) SetSecret(name string, ciphertext []byte) error {
	if strings.TrimSpace(name) == "" || len(ciphertext) == 0 {
		return errors.New("secret name and ciphertext are required")
	}
	_, err := s.db.Exec(`INSERT INTO secrets(name, ciphertext, updated_at) VALUES (?, ?, ?) ON CONFLICT(name) DO UPDATE SET ciphertext=excluded.ciphertext, updated_at=excluded.updated_at`, name, ciphertext, stamp(now()))
	return err
}

func (s *Store) Secret(name string) ([]byte, error) {
	var ciphertext []byte
	err := s.db.QueryRow(`SELECT ciphertext FROM secrets WHERE name = ?`, name).Scan(&ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return ciphertext, err
}

func (s *Store) SetNodeStatus(nodeID string, status domain.NodeStatus) error {
	_, err := s.setNodeStatus(nodeID, status, false)
	return err
}

func (s *Store) SetNodeStatusManual(nodeID string, status domain.NodeStatus) (bool, error) {
	return s.setNodeStatus(nodeID, status, true)
}

func (s *Store) setNodeStatus(nodeID string, status domain.NodeStatus, manual bool) (bool, error) {
	if status != domain.NodeActive && status != domain.NodeDraining && status != domain.NodeRevoked && status != domain.NodePending {
		return false, errors.New("invalid node status")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var currentStatus domain.NodeStatus
	var activeUninstall int
	err = tx.QueryRow(`SELECT nodes.status, EXISTS(
		SELECT 1 FROM node_uninstall_jobs WHERE node_id = nodes.id AND status IN (?, ?, ?, ?)
	) FROM nodes WHERE nodes.id = ?`, NodeUninstallPreparing, NodeUninstallReady, NodeUninstallRunning, NodeUninstallFailed, nodeID).Scan(&currentStatus, &activeUninstall)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if currentStatus == domain.NodeUninstalling || currentStatus == domain.NodeUninstalled {
		return false, errors.New("uninstalling or uninstalled nodes cannot change status")
	}
	if activeUninstall != 0 && status != currentStatus && status != domain.NodeRevoked {
		return false, ErrUninstallActive
	}
	smartRoutingDisabled := false
	if manual {
		result, err := tx.Exec(`UPDATE node_smart_routing SET enabled = 0, updated_at = ? WHERE node_id = ? AND enabled = 1`, stamp(now()), nodeID)
		if err != nil {
			return false, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		smartRoutingDisabled = changed != 0
	}
	if status == domain.NodeRevoked {
		result, err := tx.Exec(`UPDATE nodes SET status = ?, monitor_auto_paused = 0, cert_fingerprint = NULL, active_upgrade_task_id = '', updated_at = ? WHERE id = ?`, status, stamp(now()), nodeID)
		if err != nil {
			return false, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		if changed != 1 {
			return false, ErrNotFound
		}
		if _, err := tx.Exec(`DELETE FROM enrollment_tokens WHERE node_id = ? AND used_at IS NULL`, nodeID); err != nil {
			return false, err
		}
		if _, err := tx.Exec(`UPDATE node_uninstall_jobs SET previous_status = ?, updated_at = ? WHERE node_id = ? AND status IN (?, ?, ?, ?)`,
			status, stamp(now()), nodeID, NodeUninstallPreparing, NodeUninstallReady, NodeUninstallRunning, NodeUninstallFailed); err != nil {
			return false, err
		}
		if _, err := tx.Exec(`UPDATE node_upgrade_tasks SET status = ?, error_code = 'authorization_revoked',
			detail = 'node authorization was revoked during online upgrade', completed_at = ?, updated_at = ?
			WHERE node_id = ? AND status IN (?, ?)`, domain.NodeUpgradeFailed, stamp(now()), stamp(now()), nodeID,
			domain.NodeUpgradeQueued, domain.NodeUpgradeApplying); err != nil {
			return false, err
		}
	} else {
		result, err := tx.Exec(`UPDATE nodes SET status = ?, monitor_auto_paused = 0, updated_at = ? WHERE id = ?`, status, stamp(now()), nodeID)
		if err != nil {
			return false, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		if changed != 1 {
			return false, ErrNotFound
		}
	}
	if _, err := tx.Exec(`UPDATE node_monitoring_status SET consecutive_abnormal = 0, updated_at = ? WHERE node_id = ?`, stamp(now()), nodeID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return smartRoutingDisabled, nil
}

const siteSelectColumns = `id, name, zone_id, domains_json, node_ids_json, backup_node_ids_json, primary_origin_json,
	backup_origin_json, stream_paths_json, passthrough, request_body_buffering,
	origin_response_buffering, dynamic_compression_enabled, compression_excluded_mime_types_json,
	http3_enabled, client_max_body_size_mb, client_keepalive_timeout_seconds,
	read_write_timeout_seconds, dns_ttl_seconds, tcp_only, tcp_forwards_json,
	cache_max_size_gb, cache_generation, cache_invalidations_json, cache_warmups_json,
	config_version, published, enabled, deleting, created_at, updated_at`

func (s *Store) CreateSite(site domain.Site, zoneID string) (domain.Site, error) {
	zoneID = strings.TrimSpace(zoneID)
	if zoneID == "" {
		return domain.Site{}, errors.New("Cloudflare zone ID is required")
	}
	if err := domain.NormalizeAndValidateSite(&site); err != nil {
		return domain.Site{}, err
	}
	created := now()
	site.ID = uuid.NewString()
	site.ZoneID = zoneID
	site.CacheGeneration = 1
	site.ConfigVersion = 1
	site.Published = false
	site.CreatedAt = created
	site.UpdatedAt = created
	return site, s.insertSite(site, zoneID)
}

func (s *Store) insertSite(site domain.Site, zoneID string) error {
	domains, err := json.Marshal(site.Domains)
	if err != nil {
		return err
	}
	nodes, err := json.Marshal(site.Nodes)
	if err != nil {
		return err
	}
	backupNodes, err := json.Marshal(site.BackupNodes)
	if err != nil {
		return err
	}
	primary, err := json.Marshal(site.PrimaryOrigin)
	if err != nil {
		return err
	}
	var backup any
	if site.BackupOrigin != nil {
		backup, err = json.Marshal(site.BackupOrigin)
		if err != nil {
			return err
		}
	}
	streamPaths, err := json.Marshal(site.StreamPaths)
	if err != nil {
		return err
	}
	tcpForwards, err := json.Marshal(site.TCPForwards)
	if err != nil {
		return err
	}
	compressionExclusions, err := json.Marshal(site.CompressionExcludedMIMETypes)
	if err != nil {
		return err
	}
	cacheInvalidations, err := json.Marshal(site.CacheInvalidations)
	if err != nil {
		return err
	}
	cacheWarmups, err := json.Marshal(site.CacheWarmups)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateSiteNodes(tx, site.AssignedNodeIDs()); err != nil {
		return err
	}
	if err := validateSiteWireGuardTunnels(tx, site); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO sites(id, name, zone_id, domains_json, node_ids_json, backup_node_ids_json, primary_origin_json, backup_origin_json, stream_paths_json, passthrough, request_body_buffering, origin_response_buffering, dynamic_compression_enabled, compression_excluded_mime_types_json, http3_enabled, client_max_body_size_mb, client_keepalive_timeout_seconds, read_write_timeout_seconds, dns_ttl_seconds, tcp_only, tcp_forwards_json, cache_max_size_gb, cache_generation, cache_invalidations_json, cache_warmups_json, config_version, published, enabled, deleting, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		site.ID, site.Name, zoneID, string(domains), string(nodes), string(backupNodes), string(primary), backup, string(streamPaths), boolInt(site.Passthrough), boolInt(site.RequestBodyBuffering), boolInt(site.OriginResponseBuffering), boolInt(site.DynamicCompressionEnabled), string(compressionExclusions), boolInt(site.HTTP3Enabled), site.ClientMaxBodySizeMB, site.ClientKeepaliveTimeoutSeconds, site.ReadWriteTimeoutSeconds, site.DNSTTLSeconds, boolInt(site.TCPOnly), string(tcpForwards), site.CacheMaxSizeGB, site.CacheGeneration, string(cacheInvalidations), string(cacheWarmups), site.ConfigVersion, boolInt(site.Published), boolInt(site.Enabled), boolInt(site.Deleting), stamp(site.CreatedAt), stamp(site.UpdatedAt))
	if err != nil {
		return err
	}
	if err := claimDomains(tx, site.ID, site.Domains); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetSite(id string) (domain.Site, string, error) {
	return scanSite(s.db.QueryRow(`SELECT `+siteSelectColumns+` FROM sites WHERE id = ?`, id))
}

func (s *Store) ListSites() ([]domain.Site, error) {
	marker, err := s.siteSnapshotMarker()
	if err != nil {
		return nil, err
	}
	s.siteCacheMu.Lock()
	if s.siteCache.loaded && s.siteCache.marker == marker && s.siteCache.sites != nil {
		sites := cloneSites(s.siteCache.sites)
		s.siteCacheMu.Unlock()
		return sites, nil
	}
	s.siteCacheMu.Unlock()

	rows, err := s.db.Query(`SELECT ` + siteSelectColumns + ` FROM sites ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sites := make([]domain.Site, 0)
	for rows.Next() {
		site, _, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		sites = append(sites, site)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	s.siteCacheMu.Lock()
	s.siteCache = siteSnapshotCache{marker: marker, sites: sites, summaries: s.siteCache.summaries, loaded: true}
	s.siteCacheMu.Unlock()
	return cloneSites(sites), nil
}

// ListSiteSummaries returns only the fields the overview and node pages need,
// avoiding full origin/policy JSON deserialization on every poll.
func (s *Store) ListSiteSummaries() ([]domain.SiteSummary, error) {
	marker, err := s.siteSnapshotMarker()
	if err != nil {
		return nil, err
	}
	s.siteCacheMu.Lock()
	if s.siteCache.loaded && s.siteCache.marker == marker && s.siteCache.summaries != nil {
		summaries := cloneSiteSummaries(s.siteCache.summaries)
		s.siteCacheMu.Unlock()
		return summaries, nil
	}
	s.siteCacheMu.Unlock()

	rows, err := s.db.Query(`SELECT id, name, domains_json FROM sites ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	summaries := make([]domain.SiteSummary, 0)
	for rows.Next() {
		var summary domain.SiteSummary
		var domains string
		if err := rows.Scan(&summary.ID, &summary.Name, &domains); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(domains), &summary.Domains); err != nil {
			return nil, err
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	s.siteCacheMu.Lock()
	s.siteCache = siteSnapshotCache{marker: marker, sites: s.siteCache.sites, summaries: summaries, loaded: true}
	s.siteCacheMu.Unlock()
	return cloneSiteSummaries(summaries), nil
}

// ListSitesForNode returns only sites whose primary or backup node assignment
// contains the node, without deserializing every site in the database.
func (s *Store) ListSitesForNode(nodeID string) ([]domain.Site, error) {
	pattern := `%"` + nodeID + `"%`
	rows, err := s.db.Query(`SELECT `+siteSelectColumns+` FROM sites
		WHERE CAST(node_ids_json AS TEXT) LIKE ? OR CAST(backup_node_ids_json AS TEXT) LIKE ?
		ORDER BY name`, pattern, pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sites := make([]domain.Site, 0)
	for rows.Next() {
		site, _, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		sites = append(sites, site)
	}
	return sites, rows.Err()
}

func (s *Store) siteSnapshotMarker() (string, error) {
	var count int64
	var latest sql.NullString
	if err := s.db.QueryRow(`SELECT COUNT(*), MAX(updated_at) FROM sites`).Scan(&count, &latest); err != nil {
		return "", err
	}
	if !latest.Valid {
		return strconv.FormatInt(count, 10) + ":", nil
	}
	return strconv.FormatInt(count, 10) + ":" + latest.String, nil
}

func cloneSites(sites []domain.Site) []domain.Site {
	cloned := make([]domain.Site, len(sites))
	for index, site := range sites {
		cloned[index] = cloneSite(site)
	}
	return cloned
}

func cloneSite(site domain.Site) domain.Site {
	site.Domains = append([]string(nil), site.Domains...)
	site.Nodes = append([]string(nil), site.Nodes...)
	site.BackupNodes = append([]string(nil), site.BackupNodes...)
	site.StreamPaths = append([]string(nil), site.StreamPaths...)
	site.CompressionExcludedMIMETypes = append([]string(nil), site.CompressionExcludedMIMETypes...)
	site.TCPForwards = append([]domain.TCPForward(nil), site.TCPForwards...)
	site.CacheInvalidations = append([]domain.CacheInvalidationRule(nil), site.CacheInvalidations...)
	if len(site.CacheWarmups) > 0 {
		site.CacheWarmups = append([]domain.CacheWarmup(nil), site.CacheWarmups...)
		for index := range site.CacheWarmups {
			site.CacheWarmups[index].Paths = append([]string(nil), site.CacheWarmups[index].Paths...)
		}
	}
	if site.BackupOrigin != nil {
		backup := *site.BackupOrigin
		site.BackupOrigin = &backup
	}
	if site.DNSTTLSeconds != nil {
		value := *site.DNSTTLSeconds
		site.DNSTTLSeconds = &value
	}
	if site.CacheMaxSizeGB != nil {
		value := *site.CacheMaxSizeGB
		site.CacheMaxSizeGB = &value
	}
	return site
}

func cloneSiteSummaries(summaries []domain.SiteSummary) []domain.SiteSummary {
	cloned := make([]domain.SiteSummary, len(summaries))
	for index, summary := range summaries {
		cloned[index] = summary
		cloned[index].Domains = append([]string(nil), summary.Domains...)
	}
	return cloned
}

func scanSite(row scanner) (domain.Site, string, error) {
	var site domain.Site
	var zoneID, domains, nodes, backupNodes, primary, streamPaths, tcpForwards, compressionExclusions, cacheInvalidations, cacheWarmups string
	var backup sql.NullString
	var dnsTTL, cacheMaxSizeGB sql.NullInt64
	var passthrough, requestBodyBuffering, originResponseBuffering, dynamicCompressionEnabled, http3Enabled, tcpOnly, published, enabled, deleting int
	var createdAt, updatedAt string
	err := row.Scan(&site.ID, &site.Name, &zoneID, &domains, &nodes, &backupNodes, &primary, &backup, &streamPaths, &passthrough, &requestBodyBuffering, &originResponseBuffering, &dynamicCompressionEnabled, &compressionExclusions, &http3Enabled, &site.ClientMaxBodySizeMB, &site.ClientKeepaliveTimeoutSeconds, &site.ReadWriteTimeoutSeconds, &dnsTTL, &tcpOnly, &tcpForwards, &cacheMaxSizeGB, &site.CacheGeneration, &cacheInvalidations, &cacheWarmups, &site.ConfigVersion, &published, &enabled, &deleting, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Site{}, "", ErrNotFound
	}
	if err != nil {
		return domain.Site{}, "", err
	}
	site.ZoneID = zoneID
	if err := json.Unmarshal([]byte(domains), &site.Domains); err != nil {
		return domain.Site{}, "", err
	}
	if err := json.Unmarshal([]byte(nodes), &site.Nodes); err != nil {
		return domain.Site{}, "", err
	}
	if err := json.Unmarshal([]byte(backupNodes), &site.BackupNodes); err != nil {
		return domain.Site{}, "", err
	}
	if err := json.Unmarshal([]byte(primary), &site.PrimaryOrigin); err != nil {
		return domain.Site{}, "", err
	}
	if backup.Valid {
		var value domain.Origin
		if err := json.Unmarshal([]byte(backup.String), &value); err != nil {
			return domain.Site{}, "", err
		}
		site.BackupOrigin = &value
	}
	if err := json.Unmarshal([]byte(streamPaths), &site.StreamPaths); err != nil {
		return domain.Site{}, "", err
	}
	if err := json.Unmarshal([]byte(tcpForwards), &site.TCPForwards); err != nil {
		return domain.Site{}, "", err
	}
	if err := json.Unmarshal([]byte(compressionExclusions), &site.CompressionExcludedMIMETypes); err != nil {
		return domain.Site{}, "", err
	}
	if err := json.Unmarshal([]byte(cacheInvalidations), &site.CacheInvalidations); err != nil {
		return domain.Site{}, "", err
	}
	if err := json.Unmarshal([]byte(cacheWarmups), &site.CacheWarmups); err != nil {
		return domain.Site{}, "", err
	}
	var parseErr error
	site.CreatedAt, parseErr = parseTime(createdAt)
	if parseErr != nil {
		return domain.Site{}, "", parseErr
	}
	site.UpdatedAt, parseErr = parseTime(updatedAt)
	if parseErr != nil {
		return domain.Site{}, "", parseErr
	}
	site.Passthrough = passthrough != 0
	site.RequestBodyBuffering = requestBodyBuffering != 0
	site.OriginResponseBuffering = originResponseBuffering != 0
	site.DynamicCompressionEnabled = dynamicCompressionEnabled != 0
	site.HTTP3Enabled = http3Enabled != 0
	site.TCPOnly = tcpOnly != 0
	if dnsTTL.Valid {
		value := int(dnsTTL.Int64)
		site.DNSTTLSeconds = &value
	}
	if cacheMaxSizeGB.Valid {
		value := int(cacheMaxSizeGB.Int64)
		site.CacheMaxSizeGB = &value
	}
	site.Published = published != 0
	site.Enabled = enabled != 0
	site.Deleting = deleting != 0
	return site, zoneID, nil
}

func (s *Store) UpdateSite(site domain.Site, zoneID string) (domain.Site, error) {
	current, currentZoneID, err := s.GetSite(site.ID)
	if err != nil {
		return domain.Site{}, err
	}
	if current.Deleting {
		return domain.Site{}, ErrSiteDeleting
	}
	zoneID = strings.TrimSpace(zoneID)
	if zoneID == "" {
		return domain.Site{}, errors.New("Cloudflare zone ID is required")
	}
	if zoneID != currentZoneID {
		return domain.Site{}, errors.New("Cloudflare zone ID cannot be changed after site creation; create a new site instead")
	}
	site.ZoneID = currentZoneID
	// Cache-control rules and prewarm jobs are mutated only through the cache
	// operation API, not through general site edits.
	site.CacheInvalidations = append([]domain.CacheInvalidationRule(nil), current.CacheInvalidations...)
	site.CacheWarmups = append([]domain.CacheWarmup(nil), current.CacheWarmups...)
	if err := domain.NormalizeAndValidateSite(&site); err != nil {
		return domain.Site{}, err
	}
	site.CacheGeneration = current.CacheGeneration
	if site.Passthrough != current.Passthrough || site.OriginResponseBuffering != current.OriginResponseBuffering {
		site.CacheGeneration++
	}
	site.ConfigVersion = current.ConfigVersion + 1
	// Changing any site input requires a new desired-state publication before DNS may change.
	site.Published = false
	site.CreatedAt = current.CreatedAt
	site.UpdatedAt = now()
	domains, _ := json.Marshal(site.Domains)
	nodes, _ := json.Marshal(site.Nodes)
	backupNodes, _ := json.Marshal(site.BackupNodes)
	primary, _ := json.Marshal(site.PrimaryOrigin)
	streamPaths, _ := json.Marshal(site.StreamPaths)
	tcpForwards, _ := json.Marshal(site.TCPForwards)
	compressionExclusions, _ := json.Marshal(site.CompressionExcludedMIMETypes)
	cacheInvalidations, _ := json.Marshal(site.CacheInvalidations)
	cacheWarmups, _ := json.Marshal(site.CacheWarmups)
	var backup any
	if site.BackupOrigin != nil {
		backup, _ = json.Marshal(site.BackupOrigin)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Site{}, err
	}
	defer tx.Rollback()
	if err := validateSiteNodes(tx, site.AssignedNodeIDs()); err != nil {
		return domain.Site{}, err
	}
	if err := validateSiteWireGuardTunnels(tx, site); err != nil {
		return domain.Site{}, err
	}
	reservedDomains := append([]string(nil), site.Domains...)
	publishedDomains, err := publishedSiteDomains(tx, site.ID)
	if err != nil {
		return domain.Site{}, err
	}
	reservedDomains = append(reservedDomains, publishedDomains...)
	if err := replaceSiteDomainClaims(tx, site.ID, reservedDomains); err != nil {
		return domain.Site{}, err
	}
	_, err = tx.Exec(`UPDATE sites SET name=?, zone_id=?, domains_json=?, node_ids_json=?, backup_node_ids_json=?, primary_origin_json=?, backup_origin_json=?, stream_paths_json=?, passthrough=?, request_body_buffering=?, origin_response_buffering=?, dynamic_compression_enabled=?, compression_excluded_mime_types_json=?, http3_enabled=?, client_max_body_size_mb=?, client_keepalive_timeout_seconds=?, read_write_timeout_seconds=?, dns_ttl_seconds=?, tcp_only=?, tcp_forwards_json=?, cache_max_size_gb=?, cache_generation=?, cache_invalidations_json=?, cache_warmups_json=?, config_version=?, published=?, enabled=?, deleting=?, updated_at=? WHERE id=?`, site.Name, zoneID, string(domains), string(nodes), string(backupNodes), string(primary), backup, string(streamPaths), boolInt(site.Passthrough), boolInt(site.RequestBodyBuffering), boolInt(site.OriginResponseBuffering), boolInt(site.DynamicCompressionEnabled), string(compressionExclusions), boolInt(site.HTTP3Enabled), site.ClientMaxBodySizeMB, site.ClientKeepaliveTimeoutSeconds, site.ReadWriteTimeoutSeconds, site.DNSTTLSeconds, boolInt(site.TCPOnly), string(tcpForwards), site.CacheMaxSizeGB, site.CacheGeneration, string(cacheInvalidations), string(cacheWarmups), site.ConfigVersion, boolInt(site.Published), boolInt(site.Enabled), boolInt(site.Deleting), stamp(site.UpdatedAt), site.ID)
	if err != nil {
		return domain.Site{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Site{}, err
	}
	return site, nil
}

type rowQueryer interface {
	QueryRow(string, ...any) *sql.Row
}

func validateSiteNodes(queryer rowQueryer, nodeIDs []string) error {
	for _, nodeID := range nodeIDs {
		var name string
		var status domain.NodeStatus
		var activeUninstall int
		err := queryer.QueryRow(`SELECT nodes.name, nodes.status, EXISTS(
			SELECT 1 FROM node_uninstall_jobs WHERE node_id = nodes.id AND status IN (?, ?, ?, ?)
		) FROM nodes WHERE nodes.id = ?`, NodeUninstallPreparing, NodeUninstallReady, NodeUninstallRunning, NodeUninstallFailed, nodeID).Scan(&name, &status, &activeUninstall)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("site node %s: %w", nodeID, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if status == domain.NodeRevoked || status == domain.NodeUninstalling || status == domain.NodeUninstalled {
			return fmt.Errorf("site node %s is revoked, uninstalling, or uninstalled", name)
		}
		if activeUninstall != 0 {
			return fmt.Errorf("site node %s: %w", name, ErrUninstallActive)
		}
	}
	return nil
}

func claimDomains(tx *sql.Tx, siteID string, domains []string) error {
	claimed := make(map[string]struct{}, len(domains))
	for _, domainName := range domains {
		if _, exists := claimed[domainName]; exists {
			continue
		}
		claimed[domainName] = struct{}{}
		if _, err := tx.Exec(`INSERT INTO site_domains(domain_name, site_id) VALUES (?, ?)`, domainName, siteID); err != nil {
			return fmt.Errorf("domain %s is already assigned to another site: %w", domainName, err)
		}
	}
	return nil
}

func (s *Store) MarkSitePublished(siteID string) (domain.Site, error) {
	return s.CommitSitePublication(siteID, 0, "", nil, nil)
}

func (s *Store) InvalidateSiteCache(siteID string) (domain.Site, error) {
	return s.InvalidateSiteCacheScope(siteID, domain.CacheInvalidationFull, "", nil)
}

func (s *Store) InvalidateSiteCacheScope(siteID string, scope domain.CacheInvalidationScope, value string, prewarmPaths []string) (domain.Site, error) {
	if scope != domain.CacheInvalidationFull {
		var err error
		value, err = domain.NormalizeCacheInvalidationTarget(scope, value)
		if err != nil {
			return domain.Site{}, err
		}
	}
	prewarmPaths, err := domain.NormalizeCacheWarmupPaths(prewarmPaths)
	if err != nil {
		return domain.Site{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Site{}, err
	}
	defer tx.Rollback()
	site, _, err := scanSite(tx.QueryRow(`SELECT `+siteSelectColumns+` FROM sites WHERE id = ?`, siteID))
	if err != nil {
		return domain.Site{}, err
	}
	if site.Deleting {
		return domain.Site{}, ErrSiteDeleting
	}
	if !cacheSiteAvailable(site) {
		return domain.Site{}, ErrCacheDisabled
	}
	nextConfigVersion := site.ConfigVersion + 1
	if scope == domain.CacheInvalidationFull {
		site.CacheGeneration++
		site.CacheInvalidations = []domain.CacheInvalidationRule{}
	} else {
		filtered := make([]domain.CacheInvalidationRule, 0, len(site.CacheInvalidations))
		for _, rule := range site.CacheInvalidations {
			if rule.Scope != scope || rule.Value != value {
				filtered = append(filtered, rule)
			}
		}
		if len(filtered) >= domain.MaxCacheInvalidationRules {
			// Advancing the whole-site generation makes it safe to compact old
			// scoped rules without reviving an older cache key.
			site.CacheGeneration++
			filtered = nil
		}
		site.CacheInvalidations = append(filtered, domain.CacheInvalidationRule{
			Scope: scope, Value: value, Generation: nextConfigVersion,
		})
	}
	if len(prewarmPaths) != 0 {
		warmup := domain.CacheWarmup{
			ID: uuid.NewString(), SiteID: site.ID, Host: site.Domains[0], Paths: prewarmPaths, CreatedAt: now(),
		}
		if len(site.CacheWarmups) >= domain.MaxCacheWarmups {
			site.CacheWarmups = append([]domain.CacheWarmup(nil), site.CacheWarmups[len(site.CacheWarmups)-domain.MaxCacheWarmups+1:]...)
		}
		site.CacheWarmups = append(site.CacheWarmups, warmup)
	}
	site.ConfigVersion = nextConfigVersion
	site.Published = false
	site.UpdatedAt = now()
	cacheInvalidations, err := json.Marshal(site.CacheInvalidations)
	if err != nil {
		return domain.Site{}, err
	}
	cacheWarmups, err := json.Marshal(site.CacheWarmups)
	if err != nil {
		return domain.Site{}, err
	}
	result, err := tx.Exec(`UPDATE sites SET cache_generation = ?, cache_invalidations_json = ?, cache_warmups_json = ?, config_version = ?, published = 0, updated_at = ? WHERE id = ?`, site.CacheGeneration, string(cacheInvalidations), string(cacheWarmups), site.ConfigVersion, stamp(site.UpdatedAt), siteID)
	if err != nil {
		return domain.Site{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.Site{}, err
	}
	if changed != 1 {
		return domain.Site{}, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return domain.Site{}, err
	}
	return site, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *Store) CreateTask(kind, siteID, detail string) (domain.DeploymentTask, error) {
	created := now()
	task := domain.DeploymentTask{ID: uuid.NewString(), Kind: kind, SiteID: siteID, Status: domain.TaskQueued, Detail: detail, CreatedAt: created, UpdatedAt: created}
	_, err := s.db.Exec(`INSERT INTO deployment_tasks(id, kind, site_id, status, detail, deadline_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, NULL, ?, ?)`, task.ID, task.Kind, task.SiteID, task.Status, task.Detail, stamp(task.CreatedAt), stamp(task.UpdatedAt))
	return task, err
}

// CreateOrGetActivePublishTask makes repeated Publish clicks idempotent while
// an existing publication is still waiting for its edge confirmations.
func (s *Store) CreateOrGetActivePublishTask(siteID string, deadline time.Time) (domain.DeploymentTask, bool, error) {
	if strings.TrimSpace(siteID) == "" {
		return domain.DeploymentTask{}, false, errors.New("site ID is required")
	}
	if site, _, err := s.GetSite(siteID); err == nil && site.Deleting {
		return domain.DeploymentTask{}, false, ErrSiteDeleting
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return domain.DeploymentTask{}, false, err
	}
	if err := s.ReconcilePublishTasks(); err != nil {
		return domain.DeploymentTask{}, false, err
	}
	for range 2 {
		created := now()
		task := domain.DeploymentTask{ID: uuid.NewString(), Kind: "publish_site", SiteID: siteID, Status: domain.TaskQueued, Detail: "building node configurations", DeadlineAt: &deadline, CreatedAt: created, UpdatedAt: created}
		result, err := s.db.Exec(`INSERT OR IGNORE INTO deployment_tasks(id, kind, site_id, status, detail, deadline_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, task.ID, task.Kind, task.SiteID, task.Status, task.Detail, stamp(deadline), stamp(task.CreatedAt), stamp(task.UpdatedAt))
		if err != nil {
			return domain.DeploymentTask{}, false, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return domain.DeploymentTask{}, false, err
		}
		if changed == 1 {
			return task, true, nil
		}
		existing, err := s.ActivePublishTask(siteID)
		if err == nil {
			return existing, false, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return domain.DeploymentTask{}, false, err
		}
	}
	return domain.DeploymentTask{}, false, errors.New("publish task changed while being queued; retry request")
}

// CreateOrGetActiveCertificateTask atomically prevents more than one DNS-01
// issuance from running for a site. The partial unique index is the durable
// concurrency guard; INSERT OR IGNORE makes repeated UI requests idempotent.
func (s *Store) CreateOrGetActiveCertificateTask(kind, siteID, detail string) (domain.DeploymentTask, bool, error) {
	if strings.TrimSpace(siteID) == "" {
		return domain.DeploymentTask{}, false, errors.New("site ID is required")
	}
	if kind != "issue_certificate" && kind != "renew_certificate" {
		return domain.DeploymentTask{}, false, errors.New("invalid certificate task kind")
	}
	if site, _, err := s.GetSite(siteID); err != nil {
		return domain.DeploymentTask{}, false, err
	} else if site.Deleting {
		return domain.DeploymentTask{}, false, ErrSiteDeleting
	}
	for range 2 {
		created := now()
		task := domain.DeploymentTask{
			ID:        uuid.NewString(),
			Kind:      kind,
			SiteID:    siteID,
			Status:    domain.TaskQueued,
			Detail:    detail,
			CreatedAt: created,
			UpdatedAt: created,
		}
		result, err := s.db.Exec(`INSERT OR IGNORE INTO deployment_tasks(id, kind, site_id, status, detail, deadline_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, NULL, ?, ?)`, task.ID, task.Kind, task.SiteID, task.Status, task.Detail, stamp(task.CreatedAt), stamp(task.UpdatedAt))
		if err != nil {
			return domain.DeploymentTask{}, false, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return domain.DeploymentTask{}, false, err
		}
		if changed == 1 {
			return task, true, nil
		}
		existing, err := s.ActiveCertificateTask(siteID)
		if err == nil {
			return existing, false, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return domain.DeploymentTask{}, false, err
		}
		// The conflicting task completed between INSERT OR IGNORE and the
		// lookup. Retry once so the caller gets a new task rather than a spurious
		// not-found error.
	}
	return domain.DeploymentTask{}, false, errors.New("certificate task changed while being queued; retry request")
}

func (s *Store) UpdateTask(id string, status domain.TaskStatus, detail string) error {
	result, err := s.db.Exec(`UPDATE deployment_tasks SET status = ?, detail = ?, updated_at = ? WHERE id = ?`, status, detail, stamp(now()), id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetTask(id string) (domain.DeploymentTask, error) {
	return scanTask(s.db.QueryRow(`SELECT id, kind, site_id, status, detail, deadline_at, created_at, updated_at FROM deployment_tasks WHERE id = ?`, id))
}

func (s *Store) ActiveCertificateTask(siteID string) (domain.DeploymentTask, error) {
	return scanTask(s.db.QueryRow(`SELECT id, kind, site_id, status, detail, deadline_at, created_at, updated_at
		FROM deployment_tasks
		WHERE site_id = ? AND kind IN ('issue_certificate', 'renew_certificate') AND status IN (?, ?, ?)
		ORDER BY created_at DESC LIMIT 1`, siteID, domain.TaskQueued, domain.TaskDispatching, domain.TaskApplying))
}

func (s *Store) LatestCertificateTask(siteID string) (domain.DeploymentTask, error) {
	return scanTask(s.db.QueryRow(`SELECT id, kind, site_id, status, detail, deadline_at, created_at, updated_at
		FROM deployment_tasks
		WHERE site_id = ? AND kind IN ('issue_certificate', 'renew_certificate')
		ORDER BY created_at DESC LIMIT 1`, siteID))
}

func (s *Store) ActivePublishTask(siteID string) (domain.DeploymentTask, error) {
	return scanTask(s.db.QueryRow(`SELECT id, kind, site_id, status, detail, deadline_at, created_at, updated_at
		FROM deployment_tasks
		WHERE site_id = ? AND kind = 'publish_site' AND status IN (?, ?, ?)
		ORDER BY created_at DESC LIMIT 1`, siteID, domain.TaskQueued, domain.TaskDispatching, domain.TaskApplying))
}

func (s *Store) LatestPublishTask(siteID string) (domain.DeploymentTask, error) {
	return scanTask(s.db.QueryRow(`SELECT id, kind, site_id, status, detail, deadline_at, created_at, updated_at
		FROM deployment_tasks
		WHERE site_id = ? AND kind = 'publish_site'
		ORDER BY created_at DESC LIMIT 1`, siteID))
}

type PublishTaskNode struct {
	NodeID        string
	TargetVersion int64
}

func (s *Store) CreatePublishTaskNodes(taskID string, targets []PublishTaskNode) error {
	if len(targets) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := createPublishTaskNodesTx(tx, taskID, targets); err != nil {
		return err
	}
	return tx.Commit()
}

func createPublishTaskNodesTx(tx *sql.Tx, taskID string, targets []PublishTaskNode) error {
	if len(targets) != 0 && strings.TrimSpace(taskID) == "" {
		return errors.New("publish task node targets are missing a task ID")
	}
	for _, target := range targets {
		if target.NodeID == "" || target.TargetVersion < 1 {
			return errors.New("invalid publish task node target")
		}
		if _, err := tx.Exec(`INSERT INTO publish_task_nodes(task_id, node_id, target_version, status) VALUES (?, ?, ?, ?)`, taskID, target.NodeID, target.TargetVersion, domain.PublishNodePending); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RecordPublishApply(nodeID string, report domain.ApplyReport) error {
	if report.Version < 1 || (report.Status != domain.ApplySucceeded && report.Status != domain.ApplyFailed) {
		return errors.New("invalid edge apply report")
	}
	conflicts, err := json.Marshal(report.PortConflicts)
	if err != nil {
		return err
	}
	status := domain.PublishNodeFailed
	if report.Status == domain.ApplySucceeded {
		status = domain.PublishNodeSucceeded
	}
	_, err = s.db.Exec(`UPDATE publish_task_nodes
		SET status = ?, error_code = ?, detail = ?, port_conflicts_json = ?, reported_at = ?
		WHERE node_id = ? AND target_version <= ? AND status = ?
		  AND task_id IN (SELECT id FROM deployment_tasks WHERE kind IN ('publish_site', 'delete_site') AND status IN (?, ?, ?))`,
		status, report.Code, report.Detail, string(conflicts), stamp(now()), nodeID, report.Version, domain.PublishNodePending,
		domain.TaskQueued, domain.TaskDispatching, domain.TaskApplying)
	return err
}

// ReconcilePublishTasks advances edge-confirmed publication jobs. It also
// accepts an applied_version heartbeat from a pre-reporting agent, which keeps
// rollout compatibility while newer agents provide structured failure detail.
func (s *Store) ReconcilePublishTasks() error {
	rows, err := s.db.Query(`SELECT id, deadline_at FROM deployment_tasks
		WHERE kind = 'publish_site' AND status IN (?, ?, ?)
		ORDER BY created_at`, domain.TaskQueued, domain.TaskDispatching, domain.TaskApplying)
	if err != nil {
		return err
	}
	type activeTask struct {
		id       string
		deadline sql.NullString
	}
	var tasks []activeTask
	for rows.Next() {
		var task activeTask
		if err := rows.Scan(&task.id, &task.deadline); err != nil {
			rows.Close()
			return err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, task := range tasks {
		if err := s.reconcilePublishTask(task.id, task.deadline); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) reconcilePublishTask(taskID string, deadline sql.NullString) error {
	// Older agents do not send an apply report, but their durable applied
	// version proves that the desired configuration was loaded successfully.
	if _, err := s.db.Exec(`UPDATE publish_task_nodes
		SET status = ?, error_code = '', detail = 'edge confirmed applied version', port_conflicts_json = '[]', reported_at = ?
		WHERE task_id = ? AND status = ?
		  AND EXISTS (SELECT 1 FROM nodes WHERE nodes.id = publish_task_nodes.node_id AND nodes.applied_version >= publish_task_nodes.target_version)`,
		domain.PublishNodeSucceeded, stamp(now()), taskID, domain.PublishNodePending); err != nil {
		return err
	}

	var total, pending, succeeded, failed int
	if err := s.db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status IN (?, ?) THEN 1 ELSE 0 END), 0)
		FROM publish_task_nodes WHERE task_id = ?`,
		domain.PublishNodePending, domain.PublishNodeSucceeded, domain.PublishNodeFailed, domain.PublishNodeTimedOut, taskID).Scan(&total, &pending, &succeeded, &failed); err != nil {
		return err
	}
	if total == 0 {
		if deadline.Valid {
			parsed, err := parseTime(deadline.String)
			if err != nil {
				return err
			}
			if !parsed.After(now()) {
				return s.UpdateTask(taskID, domain.TaskFailed, "publish task did not create edge confirmation targets; retry Publish")
			}
		}
		return nil
	}
	expired := true
	if deadline.Valid {
		parsed, err := parseTime(deadline.String)
		if err != nil {
			return err
		}
		expired = !parsed.After(now())
	}
	if pending > 0 && !expired {
		return nil
	}
	if pending > 0 {
		if _, err := s.db.Exec(`UPDATE publish_task_nodes SET status = ?, error_code = 'confirmation_timeout', detail = 'edge did not confirm the target configuration before the publish deadline' WHERE task_id = ? AND status = ?`, domain.PublishNodeTimedOut, taskID, domain.PublishNodePending); err != nil {
			return err
		}
		failed += pending
		pending = 0
	}
	status := domain.TaskFailed
	detail := fmt.Sprintf("%d edge node(s) did not apply the configuration", failed)
	if succeeded == total {
		status = domain.TaskSucceeded
		detail = fmt.Sprintf("configuration applied by %d active edge node(s)", succeeded)
	} else if succeeded > 0 {
		status = domain.TaskPartial
		detail = fmt.Sprintf("configuration applied by %d of %d active edge node(s)", succeeded, total)
	}
	return s.UpdateTask(taskID, status, detail)
}

func (s *Store) PublishStatus(siteID string) (domain.PublishStatus, error) {
	if err := s.ReconcilePublishTasks(); err != nil {
		return domain.PublishStatus{}, err
	}
	task, err := s.LatestPublishTask(siteID)
	if errors.Is(err, ErrNotFound) {
		return domain.PublishStatus{}, nil
	}
	if err != nil {
		return domain.PublishStatus{}, err
	}
	return s.deploymentStatus(task)
}

// HasSuccessfulPublishAfter reports whether a successful publication completed
// after the supplied certificate task completed. It is used for presentation
// status only; agents still report their own applied configuration version.
func (s *Store) HasSuccessfulPublishAfter(siteID string, after time.Time) (bool, error) {
	var exists int
	err := s.db.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM deployment_tasks
		WHERE site_id = ? AND kind = 'publish_site' AND status = ? AND updated_at > ?
	)`, siteID, domain.TaskSucceeded, stamp(after)).Scan(&exists)
	return exists != 0, err
}

// FailActiveCertificateTasks is used during shutdown and startup recovery so
// canceled work is visible and never silently replayed after a restart.
func (s *Store) FailActiveCertificateTasks(detail string) (int64, error) {
	result, err := s.db.Exec(`UPDATE deployment_tasks
		SET status = ?, detail = ?, updated_at = ?
		WHERE kind IN ('issue_certificate', 'renew_certificate') AND status IN (?, ?, ?)`,
		domain.TaskFailed, detail, stamp(now()), domain.TaskQueued, domain.TaskDispatching, domain.TaskApplying)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func scanTask(row scanner) (domain.DeploymentTask, error) {
	var task domain.DeploymentTask
	var deadlineAt sql.NullString
	var createdAt, updatedAt string
	err := row.Scan(&task.ID, &task.Kind, &task.SiteID, &task.Status, &task.Detail, &deadlineAt, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DeploymentTask{}, ErrNotFound
	}
	if err != nil {
		return domain.DeploymentTask{}, err
	}
	if deadlineAt.Valid {
		deadline, err := parseTime(deadlineAt.String)
		if err != nil {
			return domain.DeploymentTask{}, err
		}
		task.DeadlineAt = &deadline
	}
	task.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return domain.DeploymentTask{}, err
	}
	task.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return domain.DeploymentTask{}, err
	}
	return task, nil
}

func (s *Store) SaveNodeState(nodeID string, state domain.DesiredState, certificatesCiphertext []byte) error {
	return s.SaveNodeStates([]NodeStateUpdate{{NodeID: nodeID, State: state, CertificatesCiphertext: certificatesCiphertext}})
}

type NodeStateUpdate struct {
	NodeID                 string
	State                  domain.DesiredState
	CertificatesCiphertext []byte
}

func (s *Store) SaveNodeStates(updates []NodeStateUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := saveNodeStatesTx(tx, updates, stamp(now())); err != nil {
		return err
	}
	return tx.Commit()
}

func saveNodeStatesTx(tx *sql.Tx, updates []NodeStateUpdate, updatedAt string) error {
	for _, update := range updates {
		if update.NodeID == "" {
			return errors.New("node state is missing a node ID")
		}
		ports, err := json.Marshal(update.State.PublicPorts)
		if err != nil {
			return err
		}
		udpPorts, err := json.Marshal(update.State.PublicUDPPorts)
		if err != nil {
			return err
		}
		originPools, err := json.Marshal(update.State.OriginPools)
		if err != nil {
			return err
		}
		staticAssets, err := json.Marshal(update.State.StaticAssets)
		if err != nil {
			return err
		}
		cacheWarmups, err := json.Marshal(update.State.CacheWarmups)
		if err != nil {
			return err
		}
		fragments, err := json.Marshal(update.State.NginxFragments)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO node_states(node_id, version, nginx_config, nginx_stream_config,
			nginx_main_config, nginx_events_config, nginx_fragments_json, public_ports_json,
			public_udp_ports_json, origin_pools_json, static_assets_json, cache_warmups_json, cache_max_bytes,
			certificate_ciphertext, private_key_ciphertext, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
			ON CONFLICT(node_id) DO UPDATE SET version=excluded.version,
			nginx_config=excluded.nginx_config, nginx_stream_config=excluded.nginx_stream_config,
			nginx_main_config=excluded.nginx_main_config, nginx_events_config=excluded.nginx_events_config,
			nginx_fragments_json=excluded.nginx_fragments_json, public_ports_json=excluded.public_ports_json,
			public_udp_ports_json=excluded.public_udp_ports_json, origin_pools_json=excluded.origin_pools_json,
			static_assets_json=excluded.static_assets_json, cache_warmups_json=excluded.cache_warmups_json,
			cache_max_bytes=excluded.cache_max_bytes,
			certificate_ciphertext=excluded.certificate_ciphertext, private_key_ciphertext=NULL,
			updated_at=excluded.updated_at`, update.NodeID, update.State.Version, update.State.NginxConfig,
			update.State.NginxStreamConfig, update.State.NginxMainConfig, update.State.NginxEventsConfig,
			string(fragments), string(ports), string(udpPorts), string(originPools), string(staticAssets),
			string(cacheWarmups), update.State.CacheMaxBytes, update.CertificatesCiphertext, updatedAt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) NodeState(nodeID string) (domain.DesiredState, []byte, error) {
	var state domain.DesiredState
	var certificates []byte
	var fragments, ports, udpPorts, originPools, staticAssets, cacheWarmups string
	err := s.db.QueryRow(`SELECT version, nginx_config, nginx_stream_config, nginx_main_config,
		nginx_events_config, nginx_fragments_json, public_ports_json, public_udp_ports_json,
		origin_pools_json, static_assets_json, cache_warmups_json, cache_max_bytes, certificate_ciphertext
		FROM node_states WHERE node_id = ?`, nodeID).Scan(&state.Version, &state.NginxConfig,
		&state.NginxStreamConfig, &state.NginxMainConfig, &state.NginxEventsConfig, &fragments,
		&ports, &udpPorts, &originPools, &staticAssets, &cacheWarmups, &state.CacheMaxBytes, &certificates)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DesiredState{}, nil, ErrNotFound
	}
	if err != nil {
		return domain.DesiredState{}, nil, err
	}
	if err := json.Unmarshal([]byte(ports), &state.PublicPorts); err != nil {
		return domain.DesiredState{}, nil, fmt.Errorf("decode desired public ports: %w", err)
	}
	if err := json.Unmarshal([]byte(udpPorts), &state.PublicUDPPorts); err != nil {
		return domain.DesiredState{}, nil, fmt.Errorf("decode desired public UDP ports: %w", err)
	}
	if err := json.Unmarshal([]byte(originPools), &state.OriginPools); err != nil {
		return domain.DesiredState{}, nil, fmt.Errorf("decode desired origin pools: %w", err)
	}
	if err := json.Unmarshal([]byte(staticAssets), &state.StaticAssets); err != nil {
		return domain.DesiredState{}, nil, fmt.Errorf("decode desired static assets: %w", err)
	}
	if err := json.Unmarshal([]byte(cacheWarmups), &state.CacheWarmups); err != nil {
		return domain.DesiredState{}, nil, fmt.Errorf("decode desired cache prewarm jobs: %w", err)
	}
	if err := json.Unmarshal([]byte(fragments), &state.NginxFragments); err != nil {
		return domain.DesiredState{}, nil, fmt.Errorf("decode desired Nginx fragments: %w", err)
	}
	return state, certificates, nil
}

func (s *Store) DesiredVersion(nodeID string) (int64, error) {
	var version int64
	err := s.db.QueryRow(`SELECT version FROM node_states WHERE node_id = ?`, nodeID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return version, err
}

func (s *Store) SaveCertificate(siteID string, certificateCiphertext, keyCiphertext []byte, notAfter *time.Time) error {
	var expires any
	if notAfter != nil {
		expires = stamp(*notAfter)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	updatedAt := stamp(now())
	result, err := tx.Exec(`UPDATE sites SET config_version = config_version + 1, published = 0, updated_at = ?
		WHERE id = ? AND deleting = 0`, updatedAt, siteID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		var deleting int
		lookupErr := tx.QueryRow(`SELECT deleting FROM sites WHERE id = ?`, siteID).Scan(&deleting)
		if lookupErr == nil && deleting != 0 {
			return ErrSiteDeleting
		}
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			return lookupErr
		}
		return ErrNotFound
	}
	if _, err := tx.Exec(`INSERT INTO certificates(site_id, certificate_ciphertext, private_key_ciphertext, not_after, updated_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(site_id) DO UPDATE SET
		certificate_ciphertext=excluded.certificate_ciphertext,
		private_key_ciphertext=excluded.private_key_ciphertext,
		not_after=excluded.not_after, updated_at=excluded.updated_at`,
		siteID, certificateCiphertext, keyCiphertext, expires, updatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Certificate(siteID string) ([]byte, []byte, *time.Time, error) {
	var cert, key []byte
	var notAfter sql.NullString
	err := s.db.QueryRow(`SELECT certificate_ciphertext, private_key_ciphertext, not_after FROM certificates WHERE site_id = ?`, siteID).Scan(&cert, &key, &notAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, nil, err
	}
	if !notAfter.Valid {
		return cert, key, nil, nil
	}
	parsed, err := parseTime(notAfter.String)
	if err != nil {
		return nil, nil, nil, err
	}
	return cert, key, &parsed, nil
}

type CertificateMetadata struct {
	NotAfter  *time.Time
	UpdatedAt time.Time
}

func (s *Store) CertificateMetadata(siteID string) (CertificateMetadata, error) {
	var notAfter sql.NullString
	var updatedAt string
	err := s.db.QueryRow(`SELECT not_after, updated_at FROM certificates WHERE site_id = ?`, siteID).Scan(&notAfter, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CertificateMetadata{}, ErrNotFound
	}
	if err != nil {
		return CertificateMetadata{}, err
	}
	metadata := CertificateMetadata{}
	metadata.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return CertificateMetadata{}, err
	}
	if notAfter.Valid {
		parsed, parseErr := parseTime(notAfter.String)
		if parseErr != nil {
			return CertificateMetadata{}, parseErr
		}
		metadata.NotAfter = &parsed
	}
	return metadata, nil
}

func (s *Store) Audit(actor, action, resourceType, resourceID, remoteAddr, detail string) error {
	_, err := s.db.Exec(`INSERT INTO audit_logs(id, actor, action, resource_type, resource_id, remote_addr, detail, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, uuid.NewString(), actor, action, resourceType, resourceID, remoteAddr, detail, stamp(now()))
	return err
}
