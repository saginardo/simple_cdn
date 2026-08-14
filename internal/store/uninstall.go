package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"simple_cdn/internal/domain"
)

type NodeUninstallJobStatus string

const (
	NodeUninstallPreparing NodeUninstallJobStatus = "preparing"
	NodeUninstallReady     NodeUninstallJobStatus = "ready"
	NodeUninstallRunning   NodeUninstallJobStatus = "running"
	NodeUninstallFailed    NodeUninstallJobStatus = "failed"
	NodeUninstallSucceeded NodeUninstallJobStatus = "succeeded"
	NodeUninstallForced    NodeUninstallJobStatus = "forced"
	NodeUninstallCanceled  NodeUninstallJobStatus = "canceled"
)

var (
	ErrUninstallActive    = errors.New("node has an active uninstall workflow")
	ErrUninstallNotActive = errors.New("node does not have an active uninstall workflow")
)

type NodeUninstallJob struct {
	NodeID          string                 `json:"node_id"`
	Status          NodeUninstallJobStatus `json:"status"`
	PreviousStatus  domain.NodeStatus      `json:"previous_status"`
	TokenExpiresAt  *time.Time             `json:"token_expires_at,omitempty"`
	ReadyAt         time.Time              `json:"ready_at"`
	AffectedSiteIDs []string               `json:"affected_site_ids"`
	Detail          string                 `json:"detail,omitempty"`
	Forced          bool                   `json:"forced"`
	StartedAt       *time.Time             `json:"started_at,omitempty"`
	CompletedAt     *time.Time             `json:"completed_at,omitempty"`
	CreatedAt       time.Time              `json:"created_at"`
	UpdatedAt       time.Time              `json:"updated_at"`
}

func (s *Store) PrepareNodeUninstall(nodeID string, affectedSiteIDs []string, readyAt time.Time) (NodeUninstallJob, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return NodeUninstallJob{}, err
	}
	defer tx.Rollback()
	var nodeStatus domain.NodeStatus
	var activeUpgradeID string
	var activeUpgrade int
	err = tx.QueryRow(`SELECT status, active_upgrade_task_id, EXISTS(SELECT 1 FROM node_upgrade_tasks
		WHERE node_id = nodes.id AND status IN (?, ?)) FROM nodes WHERE id = ?`,
		domain.NodeUpgradeQueued, domain.NodeUpgradeApplying, nodeID).Scan(&nodeStatus, &activeUpgradeID, &activeUpgrade)
	if errors.Is(err, sql.ErrNoRows) {
		return NodeUninstallJob{}, ErrNotFound
	}
	if err != nil {
		return NodeUninstallJob{}, err
	}
	if activeUpgrade != 0 {
		return NodeUninstallJob{}, ErrNodeUpgradeActive
	}
	if activeUpgradeID != "" {
		return NodeUninstallJob{}, ErrUpgradeRetryNotReady
	}
	if nodeStatus != domain.NodeDraining && nodeStatus != domain.NodeRevoked {
		return NodeUninstallJob{}, errors.New("node must be paused or revoked before uninstall")
	}
	existing, existingErr := scanNodeUninstallJob(tx.QueryRow(`SELECT node_id, status, previous_status, token_expires_at, ready_at,
		affected_site_ids_json, detail, forced, started_at, completed_at, created_at, updated_at
		FROM node_uninstall_jobs WHERE node_id = ?`, nodeID))
	if existingErr == nil && uninstallJobBlocksStatus(existing.Status) {
		return existing, nil
	}
	if existingErr != nil && !errors.Is(existingErr, ErrNotFound) {
		return NodeUninstallJob{}, existingErr
	}

	rows, err := tx.Query(`SELECT id, node_ids_json, backup_node_ids_json FROM sites`)
	if err != nil {
		return NodeUninstallJob{}, err
	}
	for rows.Next() {
		var siteID, nodeIDsJSON, backupNodeIDsJSON string
		if err := rows.Scan(&siteID, &nodeIDsJSON, &backupNodeIDsJSON); err != nil {
			rows.Close()
			return NodeUninstallJob{}, err
		}
		var nodeIDs []string
		if err := json.Unmarshal([]byte(nodeIDsJSON), &nodeIDs); err != nil {
			rows.Close()
			return NodeUninstallJob{}, fmt.Errorf("decode site nodes for uninstall: %w", err)
		}
		var backupNodeIDs []string
		if err := json.Unmarshal([]byte(backupNodeIDsJSON), &backupNodeIDs); err != nil {
			rows.Close()
			return NodeUninstallJob{}, fmt.Errorf("decode site backup nodes for uninstall: %w", err)
		}
		nodeIDs = append(nodeIDs, backupNodeIDs...)
		for _, assignedNodeID := range nodeIDs {
			if assignedNodeID == nodeID {
				affectedSiteIDs = append(affectedSiteIDs, siteID)
				break
			}
		}
	}
	if err := rows.Close(); err != nil {
		return NodeUninstallJob{}, err
	}
	if err := rows.Err(); err != nil {
		return NodeUninstallJob{}, err
	}
	rows, err = tx.Query(`SELECT site_json FROM site_publications`)
	if err != nil {
		return NodeUninstallJob{}, err
	}
	for rows.Next() {
		var siteJSON string
		if err := rows.Scan(&siteJSON); err != nil {
			rows.Close()
			return NodeUninstallJob{}, err
		}
		var site domain.Site
		if err := json.Unmarshal([]byte(siteJSON), &site); err != nil {
			rows.Close()
			return NodeUninstallJob{}, fmt.Errorf("decode published site nodes for uninstall: %w", err)
		}
		for _, assignedNodeID := range site.AssignedNodeIDs() {
			if assignedNodeID == nodeID {
				affectedSiteIDs = append(affectedSiteIDs, site.ID)
				break
			}
		}
	}
	if err := rows.Close(); err != nil {
		return NodeUninstallJob{}, err
	}
	if err := rows.Err(); err != nil {
		return NodeUninstallJob{}, err
	}

	affectedSiteIDs = uniqueStrings(affectedSiteIDs)
	affectedJSON, err := json.Marshal(affectedSiteIDs)
	if err != nil {
		return NodeUninstallJob{}, err
	}
	created := now()
	_, err = tx.Exec(`INSERT INTO node_uninstall_jobs(
		node_id, status, previous_status, token_hash, token_expires_at, ready_at,
		affected_site_ids_json, detail, forced, started_at, completed_at, created_at, updated_at
	) VALUES (?, ?, ?, NULL, NULL, ?, ?, '', 0, NULL, NULL, ?, ?)
	ON CONFLICT(node_id) DO UPDATE SET
		status=excluded.status, previous_status=excluded.previous_status,
		token_hash=NULL, token_expires_at=NULL, ready_at=excluded.ready_at,
		affected_site_ids_json=excluded.affected_site_ids_json, detail='', forced=0,
		started_at=NULL, completed_at=NULL, created_at=excluded.created_at, updated_at=excluded.updated_at`,
		nodeID, NodeUninstallPreparing, nodeStatus, stamp(readyAt), string(affectedJSON), stamp(created), stamp(created))
	if err != nil {
		return NodeUninstallJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return NodeUninstallJob{}, err
	}
	return s.NodeUninstallJob(nodeID)
}

func (s *Store) NodeUninstallJob(nodeID string) (NodeUninstallJob, error) {
	return scanNodeUninstallJob(s.db.QueryRow(`SELECT node_id, status, previous_status, token_expires_at, ready_at,
		affected_site_ids_json, detail, forced, started_at, completed_at, created_at, updated_at
		FROM node_uninstall_jobs WHERE node_id = ?`, nodeID))
}

func (s *Store) IssueNodeUninstallToken(nodeID, token string, expiresAt time.Time) (NodeUninstallJob, error) {
	if strings.TrimSpace(token) == "" {
		return NodeUninstallJob{}, errors.New("uninstall token is required")
	}
	result, err := s.db.Exec(`UPDATE node_uninstall_jobs
		SET status = ?, token_hash = ?, token_expires_at = ?, detail = '', updated_at = ?
		WHERE node_id = ? AND status IN (?, ?, ?)`,
		NodeUninstallReady, hashToken(token), stamp(expiresAt), stamp(now()), nodeID,
		NodeUninstallPreparing, NodeUninstallReady, NodeUninstallFailed)
	if err != nil {
		return NodeUninstallJob{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return NodeUninstallJob{}, err
	}
	if changed != 1 {
		return NodeUninstallJob{}, ErrUninstallActive
	}
	return s.NodeUninstallJob(nodeID)
}

func (s *Store) StartNodeUninstall(token string) (NodeUninstallJob, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return NodeUninstallJob{}, err
	}
	defer tx.Rollback()
	job, err := uninstallJobByToken(tx, token)
	if err != nil {
		return NodeUninstallJob{}, err
	}
	if job.TokenExpiresAt == nil || !job.TokenExpiresAt.After(now()) {
		return NodeUninstallJob{}, ErrTokenInvalid
	}
	if job.Status == NodeUninstallSucceeded {
		return job, tx.Commit()
	}
	if job.Status == NodeUninstallForced {
		started := now()
		if _, err := tx.Exec(`UPDATE node_uninstall_jobs SET started_at = COALESCE(started_at, ?), updated_at = ? WHERE node_id = ?`,
			stamp(started), stamp(started), job.NodeID); err != nil {
			return NodeUninstallJob{}, err
		}
		if err := tx.Commit(); err != nil {
			return NodeUninstallJob{}, err
		}
		return s.NodeUninstallJob(job.NodeID)
	}
	if job.Status != NodeUninstallReady && job.Status != NodeUninstallRunning && job.Status != NodeUninstallFailed {
		return NodeUninstallJob{}, ErrTokenInvalid
	}
	started := now()
	if _, err := tx.Exec(`UPDATE node_uninstall_jobs SET status = ?, started_at = COALESCE(started_at, ?), detail = '', updated_at = ? WHERE node_id = ?`,
		NodeUninstallRunning, stamp(started), stamp(started), job.NodeID); err != nil {
		return NodeUninstallJob{}, err
	}
	if _, err := tx.Exec(`UPDATE nodes SET status = ?, updated_at = ? WHERE id = ?`, domain.NodeUninstalling, stamp(started), job.NodeID); err != nil {
		return NodeUninstallJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return NodeUninstallJob{}, err
	}
	return s.NodeUninstallJob(job.NodeID)
}

func (s *Store) FailNodeUninstall(token, detail string) (NodeUninstallJob, error) {
	if len(detail) > 2000 {
		detail = detail[:2000]
	}
	tx, err := s.db.Begin()
	if err != nil {
		return NodeUninstallJob{}, err
	}
	defer tx.Rollback()
	job, err := uninstallJobByToken(tx, token)
	if err != nil {
		return NodeUninstallJob{}, err
	}
	if job.TokenExpiresAt == nil || !job.TokenExpiresAt.After(now()) {
		return NodeUninstallJob{}, ErrTokenInvalid
	}
	if job.Status == NodeUninstallSucceeded || job.Status == NodeUninstallForced {
		return job, tx.Commit()
	}
	if job.Status != NodeUninstallRunning && job.Status != NodeUninstallFailed {
		return NodeUninstallJob{}, ErrTokenInvalid
	}
	updated := now()
	if _, err := tx.Exec(`UPDATE node_uninstall_jobs SET status = ?, detail = ?, updated_at = ? WHERE node_id = ?`,
		NodeUninstallFailed, detail, stamp(updated), job.NodeID); err != nil {
		return NodeUninstallJob{}, err
	}
	if _, err := tx.Exec(`UPDATE nodes SET status = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		job.PreviousStatus, detail, stamp(updated), job.NodeID); err != nil {
		return NodeUninstallJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return NodeUninstallJob{}, err
	}
	return s.NodeUninstallJob(job.NodeID)
}

func (s *Store) CompleteNodeUninstall(token string) (NodeUninstallJob, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return NodeUninstallJob{}, err
	}
	defer tx.Rollback()
	job, err := uninstallJobByToken(tx, token)
	if err != nil {
		return NodeUninstallJob{}, err
	}
	if job.TokenExpiresAt == nil || !job.TokenExpiresAt.After(now()) {
		return NodeUninstallJob{}, ErrTokenInvalid
	}
	if job.Status == NodeUninstallSucceeded {
		return job, tx.Commit()
	}
	if job.Status == NodeUninstallForced {
		if job.StartedAt == nil {
			return NodeUninstallJob{}, ErrTokenInvalid
		}
		if err := completeNodeUninstall(tx, job.NodeID, NodeUninstallSucceeded, false, ""); err != nil {
			return NodeUninstallJob{}, err
		}
		if err := tx.Commit(); err != nil {
			return NodeUninstallJob{}, err
		}
		return s.NodeUninstallJob(job.NodeID)
	}
	if job.Status != NodeUninstallRunning {
		return NodeUninstallJob{}, ErrTokenInvalid
	}
	if err := completeNodeUninstall(tx, job.NodeID, NodeUninstallSucceeded, false, ""); err != nil {
		return NodeUninstallJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return NodeUninstallJob{}, err
	}
	return s.NodeUninstallJob(job.NodeID)
}

func (s *Store) ForceCompleteNodeUninstall(nodeID, detail string) (NodeUninstallJob, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return NodeUninstallJob{}, err
	}
	defer tx.Rollback()
	job, err := scanNodeUninstallJob(tx.QueryRow(`SELECT node_id, status, previous_status, token_expires_at, ready_at,
		affected_site_ids_json, detail, forced, started_at, completed_at, created_at, updated_at
		FROM node_uninstall_jobs WHERE node_id = ?`, nodeID))
	if err != nil {
		return NodeUninstallJob{}, err
	}
	if !uninstallJobBlocksStatus(job.Status) {
		return NodeUninstallJob{}, ErrUninstallNotActive
	}
	if err := completeNodeUninstall(tx, nodeID, NodeUninstallForced, true, detail); err != nil {
		return NodeUninstallJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return NodeUninstallJob{}, err
	}
	return s.NodeUninstallJob(nodeID)
}

func (s *Store) CancelNodeUninstall(nodeID string) (NodeUninstallJob, error) {
	result, err := s.db.Exec(`UPDATE node_uninstall_jobs SET status = ?, token_hash = NULL, token_expires_at = NULL, detail = '', updated_at = ?
		WHERE node_id = ? AND status IN (?, ?, ?)`,
		NodeUninstallCanceled, stamp(now()), nodeID, NodeUninstallPreparing, NodeUninstallReady, NodeUninstallFailed)
	if err != nil {
		return NodeUninstallJob{}, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return NodeUninstallJob{}, err
		}
		job, lookupErr := s.NodeUninstallJob(nodeID)
		if errors.Is(lookupErr, ErrNotFound) {
			return NodeUninstallJob{}, ErrNotFound
		} else if lookupErr != nil {
			return NodeUninstallJob{}, lookupErr
		}
		if !uninstallJobBlocksStatus(job.Status) {
			return NodeUninstallJob{}, ErrUninstallNotActive
		}
		return NodeUninstallJob{}, ErrUninstallActive
	}
	return s.NodeUninstallJob(nodeID)
}

func (s *Store) DeleteNode(nodeID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT node_ids_json, backup_node_ids_json FROM sites`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var nodeIDsJSON, backupNodeIDsJSON string
		if err := rows.Scan(&nodeIDsJSON, &backupNodeIDsJSON); err != nil {
			rows.Close()
			return err
		}
		var nodeIDs []string
		if err := json.Unmarshal([]byte(nodeIDsJSON), &nodeIDs); err != nil {
			rows.Close()
			return fmt.Errorf("decode site nodes before deleting node: %w", err)
		}
		var backupNodeIDs []string
		if err := json.Unmarshal([]byte(backupNodeIDsJSON), &backupNodeIDs); err != nil {
			rows.Close()
			return fmt.Errorf("decode site backup nodes before deleting node: %w", err)
		}
		nodeIDs = append(nodeIDs, backupNodeIDs...)
		for _, assignedNodeID := range nodeIDs {
			if assignedNodeID == nodeID {
				rows.Close()
				return ErrNodeAssigned
			}
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = tx.Query(`SELECT site_json FROM site_publications`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var siteJSON string
		if err := rows.Scan(&siteJSON); err != nil {
			rows.Close()
			return err
		}
		var site domain.Site
		if err := json.Unmarshal([]byte(siteJSON), &site); err != nil {
			rows.Close()
			return fmt.Errorf("decode published site nodes before deleting node: %w", err)
		}
		for _, assignedNodeID := range site.AssignedNodeIDs() {
			if assignedNodeID == nodeID {
				rows.Close()
				return ErrNodeAssigned
			}
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	result, err := tx.Exec(`DELETE FROM nodes WHERE id = ?`, nodeID)
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
	return tx.Commit()
}

func (s *Store) PendingNodeCanBeDeleted(nodeID string) (bool, error) {
	var allowed int
	err := s.db.QueryRow(`SELECT CASE WHEN status = ? AND cert_fingerprint IS NULL AND last_heartbeat_at IS NULL
		AND applied_version = 0 AND NOT EXISTS (SELECT 1 FROM node_states WHERE node_id = nodes.id)
		THEN 1 ELSE 0 END FROM nodes WHERE id = ?`, domain.NodePending, nodeID).Scan(&allowed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	return allowed == 1, err
}

func (s *Store) HasActiveNodeUninstall(nodeID string) (bool, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM node_uninstall_jobs WHERE node_id = ? AND status IN (?, ?, ?, ?)`,
		nodeID, NodeUninstallPreparing, NodeUninstallReady, NodeUninstallRunning, NodeUninstallFailed).Scan(&count)
	return count > 0, err
}

func completeNodeUninstall(tx *sql.Tx, nodeID string, status NodeUninstallJobStatus, forced bool, detail string) error {
	completed := now()
	if _, err := tx.Exec(`UPDATE node_uninstall_jobs SET status = ?, detail = ?, forced = ?, completed_at = ?, updated_at = ? WHERE node_id = ?`,
		status, detail, boolInt(forced), stamp(completed), stamp(completed), nodeID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE nodes SET status = ?, cert_fingerprint = NULL, last_error = '', updated_at = ? WHERE id = ?`,
		domain.NodeUninstalled, stamp(completed), nodeID); err != nil {
		return err
	}
	for _, query := range []string{
		`DELETE FROM enrollment_tokens WHERE node_id = ?`,
		`DELETE FROM node_states WHERE node_id = ?`,
		`DELETE FROM node_health WHERE node_id = ?`,
		`DELETE FROM site_node_health WHERE node_id = ?`,
		`DELETE FROM dns_bindings WHERE node_id = ?`,
	} {
		if _, err := tx.Exec(query, nodeID); err != nil {
			return err
		}
	}
	return nil
}

func uninstallJobByToken(queryer interface {
	QueryRow(string, ...any) *sql.Row
}, token string) (NodeUninstallJob, error) {
	if strings.TrimSpace(token) == "" {
		return NodeUninstallJob{}, ErrTokenInvalid
	}
	job, err := scanNodeUninstallJob(queryer.QueryRow(`SELECT node_id, status, previous_status, token_expires_at, ready_at,
		affected_site_ids_json, detail, forced, started_at, completed_at, created_at, updated_at
		FROM node_uninstall_jobs WHERE token_hash = ?`, hashToken(token)))
	if errors.Is(err, ErrNotFound) {
		return NodeUninstallJob{}, ErrTokenInvalid
	}
	return job, err
}

func scanNodeUninstallJob(row scanner) (NodeUninstallJob, error) {
	var job NodeUninstallJob
	var tokenExpiresAt, startedAt, completedAt sql.NullString
	var readyAt, affectedSites, createdAt, updatedAt string
	var forced int
	err := row.Scan(&job.NodeID, &job.Status, &job.PreviousStatus, &tokenExpiresAt, &readyAt,
		&affectedSites, &job.Detail, &forced, &startedAt, &completedAt, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return NodeUninstallJob{}, ErrNotFound
	}
	if err != nil {
		return NodeUninstallJob{}, err
	}
	job.Forced = forced != 0
	if err := json.Unmarshal([]byte(affectedSites), &job.AffectedSiteIDs); err != nil {
		return NodeUninstallJob{}, fmt.Errorf("decode uninstall affected sites: %w", err)
	}
	for source, target := range map[*sql.NullString]**time.Time{
		&tokenExpiresAt: &job.TokenExpiresAt,
		&startedAt:      &job.StartedAt,
		&completedAt:    &job.CompletedAt,
	} {
		if source.Valid {
			value, err := parseTime(source.String)
			if err != nil {
				return NodeUninstallJob{}, err
			}
			*target = &value
		}
	}
	if job.ReadyAt, err = parseTime(readyAt); err != nil {
		return NodeUninstallJob{}, err
	}
	if job.CreatedAt, err = parseTime(createdAt); err != nil {
		return NodeUninstallJob{}, err
	}
	if job.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return NodeUninstallJob{}, err
	}
	return job, nil
}

func uninstallJobBlocksStatus(status NodeUninstallJobStatus) bool {
	return status == NodeUninstallPreparing || status == NodeUninstallReady || status == NodeUninstallRunning || status == NodeUninstallFailed
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}
