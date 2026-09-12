package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"simple_cdn/internal/domain"
)

func migrateSystemHealth(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS system_health_snapshot (
		id INTEGER PRIMARY KEY CHECK (id = 1), body TEXT NOT NULL)`)
	return err
}

type HealthSite struct {
	Draft           domain.Site
	Published       *domain.Site
	PublishedAt     *time.Time
	ServingNotAfter *time.Time
	Certificate     *CertificateMetadata
}

type HealthProbe struct {
	SiteID, NodeID string
	IPv6           bool
	Eligible       bool
	CheckedAt      *time.Time
	Error          string
}

type SystemHealthInputs struct {
	Nodes           []domain.Node
	Sites           []HealthSite
	DesiredVersions map[string]int64
	NodeProbes      map[string]HealthProbe
	SiteProbes      []HealthProbe
	Tasks           []domain.DeploymentTask
	Upgrades        []domain.NodeUpgradeTask
	Rollout         *domain.NodeUpgradeRollout
}

// Read one consistent, bounded SQLite snapshot. Collection never invokes a
// reconciler, performs an external probe, or reads certificate private keys.
func (s *Store) SystemHealthInputs(ctx context.Context) (SystemHealthInputs, error) {
	result := SystemHealthInputs{DesiredVersions: map[string]int64{}, NodeProbes: map[string]HealthProbe{}}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	query := func(statement string, scan func(*sql.Rows) error) error {
		rows, err := tx.QueryContext(ctx, statement)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err = query(`SELECT id, name, public_ipv4, public_ipv6, status, monitor_auto_paused, capabilities_json, cache_max_size_gb,
		nginx_worker_processes, nginx_worker_connections, nginx_worker_rlimit_nofile, agent_version, agent_sha256,
		nginx_version, nginx_sha256, active_upgrade_task_id, last_heartbeat_at, applied_version, last_error, created_at, updated_at FROM nodes ORDER BY name`, func(rows *sql.Rows) error {
		node, err := scanNode(rows)
		result.Nodes = append(result.Nodes, node)
		return err
	}); err != nil {
		return result, err
	}
	if err = query(`SELECT node_id, version FROM node_states`, func(rows *sql.Rows) error {
		var id string
		var version int64
		if err := rows.Scan(&id, &version); err != nil {
			return err
		}
		result.DesiredVersions[id] = version
		return nil
	}); err != nil {
		return result, err
	}
	if err = query(`SELECT `+siteSelectColumns+` FROM sites ORDER BY name`, func(rows *sql.Rows) error {
		site, _, err := scanSite(rows)
		result.Sites = append(result.Sites, HealthSite{Draft: site})
		return err
	}); err != nil {
		return result, err
	}
	siteIndex := map[string]int{}
	for i, site := range result.Sites {
		siteIndex[site.Draft.ID] = i
	}
	if err = query(`SELECT site_id, site_json, published_at, certificate_not_after FROM site_publications`, func(rows *sql.Rows) error {
		var id, body, published string
		var expiry sql.NullString
		if err := rows.Scan(&id, &body, &published, &expiry); err != nil {
			return err
		}
		i, found := siteIndex[id]
		if !found {
			return nil
		}
		var site domain.Site
		if err := json.Unmarshal([]byte(body), &site); err != nil {
			return err
		}
		at, err := parseTime(published)
		if err != nil {
			return err
		}
		result.Sites[i].Published, result.Sites[i].PublishedAt = &site, &at
		result.Sites[i].ServingNotAfter, err = healthOptionalTime(expiry)
		return err
	}); err != nil {
		return result, err
	}
	if err = query(`SELECT site_id, not_after, updated_at FROM certificates`, func(rows *sql.Rows) error {
		var id, updated string
		var expiry sql.NullString
		if err := rows.Scan(&id, &expiry, &updated); err != nil {
			return err
		}
		i, found := siteIndex[id]
		if !found {
			return nil
		}
		at, err := parseTime(updated)
		if err != nil {
			return err
		}
		notAfter, err := healthOptionalTime(expiry)
		if err != nil {
			return err
		}
		result.Sites[i].Certificate = &CertificateMetadata{NotAfter: notAfter, UpdatedAt: at}
		return nil
	}); err != nil {
		return result, err
	}
	if err = query(`SELECT node_id, dns_eligible, last_checked_at, last_error FROM node_health`, func(rows *sql.Rows) error {
		var p HealthProbe
		var checked sql.NullString
		if err := rows.Scan(&p.NodeID, &p.Eligible, &checked, &p.Error); err != nil {
			return err
		}
		var err error
		p.CheckedAt, err = healthOptionalTime(checked)
		result.NodeProbes[p.NodeID] = p
		return err
	}); err != nil {
		return result, err
	}
	if err = query(`SELECT site_id, node_id, dns_eligible, last_checked_at, last_error,
		ipv6_dns_eligible, ipv6_last_checked_at, ipv6_last_error FROM site_node_health`, func(rows *sql.Rows) error {
		var v4, v6 HealthProbe
		var checked4, checked6 sql.NullString
		if err := rows.Scan(&v4.SiteID, &v4.NodeID, &v4.Eligible, &checked4, &v4.Error, &v6.Eligible, &checked6, &v6.Error); err != nil {
			return err
		}
		v6.SiteID, v6.NodeID, v6.IPv6 = v4.SiteID, v4.NodeID, true
		var err error
		v4.CheckedAt, err = healthOptionalTime(checked4)
		if err != nil {
			return err
		}
		v6.CheckedAt, err = healthOptionalTime(checked6)
		result.SiteProbes = append(result.SiteProbes, v4, v6)
		return err
	}); err != nil {
		return result, err
	}
	if err = query(`SELECT id, kind, COALESCE(site_id,''), status, detail, deadline_at, created_at, updated_at FROM (
		SELECT *, ROW_NUMBER() OVER (PARTITION BY site_id, CASE WHEN kind = 'publish_site' THEN 'publish' ELSE 'certificate' END ORDER BY created_at DESC, rowid DESC) AS position
		FROM deployment_tasks WHERE kind IN ('publish_site','issue_certificate','renew_certificate')) WHERE position = 1`, func(rows *sql.Rows) error {
		task, err := scanTask(rows)
		result.Tasks = append(result.Tasks, task)
		return err
	}); err != nil {
		return result, err
	}
	if err = query(`SELECT `+nodeUpgradeTaskColumns+` FROM (SELECT *, ROW_NUMBER() OVER (PARTITION BY node_id ORDER BY created_at DESC, rowid DESC) AS position FROM node_upgrade_tasks) WHERE position = 1`, func(rows *sql.Rows) error {
		task, err := scanNodeUpgradeTask(rows)
		result.Upgrades = append(result.Upgrades, task)
		return err
	}); err != nil {
		return result, err
	}
	if err = query(`SELECT body FROM node_upgrade_rollouts ORDER BY created_at DESC, rowid DESC LIMIT 1`, func(rows *sql.Rows) error {
		var body string
		if err := rows.Scan(&body); err != nil {
			return err
		}
		var rollout domain.NodeUpgradeRollout
		if err := json.Unmarshal([]byte(body), &rollout); err != nil {
			return err
		}
		result.Rollout = &rollout
		return nil
	}); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func healthOptionalTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid || value.String == "" {
		return nil, nil
	}
	parsed, err := parseTime(value.String)
	return &parsed, err
}

func (s *Store) SystemHealthSnapshot(ctx context.Context) (domain.SystemHealthSnapshot, error) {
	var body string
	err := s.db.QueryRowContext(ctx, `SELECT body FROM system_health_snapshot WHERE id = 1`).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SystemHealthSnapshot{}, ErrNotFound
	}
	if err != nil {
		return domain.SystemHealthSnapshot{}, err
	}
	var snapshot domain.SystemHealthSnapshot
	err = json.Unmarshal([]byte(body), &snapshot)
	return snapshot, err
}

// Persist first-observed times and a bounded recovery history together. Removed
// or disabled checks are retired, never reported as a successful recovery.
func (s *Store) SaveSystemHealthSnapshot(ctx context.Context, snapshot domain.SystemHealthSnapshot) error {
	if snapshot.CollectedAt == nil {
		return errors.New("health collection time is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var body string
	var previous domain.SystemHealthSnapshot
	if err = tx.QueryRowContext(ctx, `SELECT body FROM system_health_snapshot WHERE id = 1`).Scan(&body); err == nil {
		if err = json.Unmarshal([]byte(body), &previous); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	at := *snapshot.CollectedAt
	if previous.CollectedAt != nil && !at.After(*previous.CollectedAt) {
		return fmt.Errorf("health snapshot must advance collection time")
	}
	old := map[string]domain.HealthCheck{}
	for _, check := range previous.Checks {
		old[check.ID] = check
	}
	seen := map[string]bool{}
	snapshot.Recoveries = append([]domain.HealthRecovery{}, previous.Recoveries...)
	record := func(check domain.HealthCheck, outcome string) {
		snapshot.Recoveries = append([]domain.HealthRecovery{{CheckID: check.ID, Title: check.Title, PreviousState: check.State,
			Outcome: outcome, StartedAt: check.Since, FinishedAt: at, NodeIDs: check.NodeIDs, SiteIDs: check.SiteIDs, ActionURL: check.ActionURL}}, snapshot.Recoveries...)
	}
	for i := range snapshot.Checks {
		check := &snapshot.Checks[i]
		if check.ID == "" || seen[check.ID] {
			return errors.New("health checks need unique IDs")
		}
		seen[check.ID] = true
		check.Since = at
		if before, found := old[check.ID]; found {
			if check.State == before.State || (domain.HealthNeedsAttention(check.State) && domain.HealthNeedsAttention(before.State)) {
				check.Since = before.Since
			}
			if domain.HealthNeedsAttention(before.State) && !domain.HealthNeedsAttention(check.State) {
				outcome := "resolved"
				if check.State == domain.HealthDisabled {
					outcome = "retired"
				}
				record(before, outcome)
			}
		}
	}
	for _, check := range previous.Checks {
		if !seen[check.ID] && domain.HealthNeedsAttention(check.State) {
			record(check, "retired")
		}
	}
	retained := make([]domain.HealthRecovery, 0, 100)
	for _, event := range snapshot.Recoveries {
		if len(retained) < 100 && event.FinishedAt.After(at.AddDate(0, 0, -30)) {
			retained = append(retained, event)
		}
	}
	snapshot.Recoveries = retained
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO system_health_snapshot(id,body) VALUES (1,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body`, string(encoded)); err != nil {
		return err
	}
	return tx.Commit()
}
