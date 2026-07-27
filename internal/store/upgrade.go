package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"simple_cdn/internal/domain"
)

const nodeUpgradeTaskColumns = `id, node_id, status, source_sha256, target_sha256, source_nginx_sha256, target_nginx_sha256, error_code, detail,
	deadline_at, started_at, completed_at, created_at, updated_at`

const recoveredUpgradeDetail = "edge heartbeat confirmed the target artifact after the updater result was missed"

func (s *Store) CreateOrGetNodeUpgrade(nodeID string, instruction domain.NodeUpgradeInstruction, deadline time.Time) (domain.NodeUpgradeTask, bool, error) {
	if strings.TrimSpace(nodeID) == "" || strings.TrimSpace(instruction.Binary.SHA256) == "" || !deadline.After(now()) {
		return domain.NodeUpgradeTask{}, false, errors.New("invalid node upgrade request")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return domain.NodeUpgradeTask{}, false, err
	}
	defer tx.Rollback()

	active, err := scanNodeUpgradeTask(tx.QueryRow(`SELECT `+nodeUpgradeTaskColumns+` FROM node_upgrade_tasks
		WHERE node_id = ? AND status IN (?, ?) ORDER BY created_at DESC LIMIT 1`,
		nodeID, domain.NodeUpgradeQueued, domain.NodeUpgradeApplying))
	if err == nil {
		return active, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return domain.NodeUpgradeTask{}, false, err
	}

	var status domain.NodeStatus
	var sourceSHA256, sourceNginxSHA256, activeUpgradeID string
	var activeUninstall, activePublication int
	err = tx.QueryRow(`SELECT nodes.status, nodes.agent_sha256, nodes.nginx_sha256, nodes.active_upgrade_task_id,
		EXISTS(SELECT 1 FROM node_uninstall_jobs WHERE node_id = nodes.id AND status IN (?, ?, ?, ?)),
		EXISTS(SELECT 1 FROM publish_task_nodes
			JOIN deployment_tasks ON deployment_tasks.id = publish_task_nodes.task_id
			WHERE publish_task_nodes.node_id = nodes.id AND publish_task_nodes.status = ?
			AND deployment_tasks.status IN (?, ?, ?))
		FROM nodes WHERE nodes.id = ?`,
		NodeUninstallPreparing, NodeUninstallReady, NodeUninstallRunning, NodeUninstallFailed,
		domain.PublishNodePending, domain.TaskQueued, domain.TaskDispatching, domain.TaskApplying, nodeID).
		Scan(&status, &sourceSHA256, &sourceNginxSHA256, &activeUpgradeID, &activeUninstall, &activePublication)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.NodeUpgradeTask{}, false, ErrNotFound
	}
	if err != nil {
		return domain.NodeUpgradeTask{}, false, err
	}
	if activeUpgradeID != "" {
		return domain.NodeUpgradeTask{}, false, ErrUpgradeRetryNotReady
	}
	if status != domain.NodeActive && status != domain.NodeDraining {
		return domain.NodeUpgradeTask{}, false, errors.New("only active or paused nodes can be upgraded online")
	}
	if activeUninstall != 0 || activePublication != 0 {
		return domain.NodeUpgradeTask{}, false, ErrNodeOperationActive
	}

	created := now()
	task := domain.NodeUpgradeTask{
		ID: uuid.NewString(), NodeID: nodeID, Status: domain.NodeUpgradeQueued,
		SourceSHA256: sourceSHA256, TargetSHA256: instruction.Binary.SHA256,
		SourceNginxSHA256: sourceNginxSHA256,
		Detail:            "waiting for edge to download upgrade artifacts", DeadlineAt: deadline,
		CreatedAt: created, UpdatedAt: created,
	}
	if instruction.NginxBundle != nil {
		task.TargetNginxSHA256 = instruction.NginxBundle.SHA256
	}
	nginxBundle, nginxService := domain.UpgradeArtifact{}, domain.UpgradeArtifact{}
	if instruction.NginxBundle != nil {
		nginxBundle = *instruction.NginxBundle
	}
	if instruction.NginxService != nil {
		nginxService = *instruction.NginxService
	}
	_, err = tx.Exec(`INSERT INTO node_upgrade_tasks(
		id, node_id, status, source_sha256, target_sha256, source_nginx_sha256, target_nginx_sha256, binary_url,
		installer_url, installer_sha256, agent_service_url, agent_service_sha256,
		updater_service_url, updater_service_sha256, nginx_bundle_url, nginx_bundle_sha256,
		nginx_service_url, nginx_service_sha256, error_code, detail, deadline_at,
		started_at, completed_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, NULL, NULL, ?, ?)`,
		task.ID, task.NodeID, task.Status, task.SourceSHA256, task.TargetSHA256, task.SourceNginxSHA256, task.TargetNginxSHA256, instruction.Binary.URL,
		instruction.Installer.URL, instruction.Installer.SHA256,
		instruction.AgentService.URL, instruction.AgentService.SHA256,
		instruction.UpdaterService.URL, instruction.UpdaterService.SHA256,
		nginxBundle.URL, nginxBundle.SHA256, nginxService.URL, nginxService.SHA256,
		task.Detail, stamp(task.DeadlineAt), stamp(task.CreatedAt), stamp(task.UpdatedAt))
	if err != nil {
		return domain.NodeUpgradeTask{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return domain.NodeUpgradeTask{}, false, err
	}
	return task, true, nil
}

func (s *Store) NodeUpgradeTask(taskID string) (domain.NodeUpgradeTask, error) {
	return scanNodeUpgradeTask(s.db.QueryRow(`SELECT `+nodeUpgradeTaskColumns+` FROM node_upgrade_tasks WHERE id = ?`, taskID))
}

func (s *Store) LatestNodeUpgrade(nodeID string) (domain.NodeUpgradeTask, error) {
	return scanNodeUpgradeTask(s.db.QueryRow(`SELECT `+nodeUpgradeTaskColumns+` FROM node_upgrade_tasks
		WHERE node_id = ? ORDER BY created_at DESC LIMIT 1`, nodeID))
}

func (s *Store) ListLatestNodeUpgrades() (map[string]domain.NodeUpgradeTask, error) {
	rows, err := s.db.Query(`SELECT ` + nodeUpgradeTaskColumns + ` FROM node_upgrade_tasks AS task
		WHERE NOT EXISTS(SELECT 1 FROM node_upgrade_tasks AS newer
			WHERE newer.node_id = task.node_id AND newer.created_at > task.created_at)
		ORDER BY task.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]domain.NodeUpgradeTask)
	for rows.Next() {
		task, err := scanNodeUpgradeTask(rows)
		if err != nil {
			return nil, err
		}
		result[task.NodeID] = task
	}
	return result, rows.Err()
}

func (s *Store) NodeUpgradeInstruction(nodeID string) (domain.NodeUpgradeInstruction, error) {
	if err := s.ReconcileNodeUpgrades(); err != nil {
		return domain.NodeUpgradeInstruction{}, err
	}
	var instruction domain.NodeUpgradeInstruction
	var deadline string
	var nginxBundleURL, nginxBundleSHA256, nginxServiceURL, nginxServiceSHA256 string
	err := s.db.QueryRow(`SELECT id, deadline_at, binary_url, target_sha256,
		installer_url, installer_sha256, agent_service_url, agent_service_sha256,
		updater_service_url, updater_service_sha256,
		nginx_bundle_url, nginx_bundle_sha256, nginx_service_url, nginx_service_sha256
		FROM node_upgrade_tasks WHERE node_id = ? AND status IN (?, ?)
		ORDER BY created_at DESC LIMIT 1`, nodeID, domain.NodeUpgradeQueued, domain.NodeUpgradeApplying).
		Scan(&instruction.TaskID, &deadline, &instruction.Binary.URL, &instruction.Binary.SHA256,
			&instruction.Installer.URL, &instruction.Installer.SHA256,
			&instruction.AgentService.URL, &instruction.AgentService.SHA256,
			&instruction.UpdaterService.URL, &instruction.UpdaterService.SHA256,
			&nginxBundleURL, &nginxBundleSHA256, &nginxServiceURL, &nginxServiceSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.NodeUpgradeInstruction{}, ErrNotFound
	}
	if err != nil {
		return domain.NodeUpgradeInstruction{}, err
	}
	instruction.DeadlineAt, err = parseTime(deadline)
	if nginxBundleURL != "" || nginxBundleSHA256 != "" {
		instruction.NginxBundle = &domain.UpgradeArtifact{URL: nginxBundleURL, SHA256: nginxBundleSHA256}
	}
	if nginxServiceURL != "" || nginxServiceSHA256 != "" {
		instruction.NginxService = &domain.UpgradeArtifact{URL: nginxServiceURL, SHA256: nginxServiceSHA256}
	}
	return instruction, err
}

func (s *Store) RecordNodeUpgradeReport(nodeID string, report domain.NodeUpgradeReport) (domain.NodeUpgradeTask, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return domain.NodeUpgradeTask{}, err
	}
	defer tx.Rollback()
	task, err := scanNodeUpgradeTask(tx.QueryRow(`SELECT `+nodeUpgradeTaskColumns+` FROM node_upgrade_tasks WHERE id = ? AND node_id = ?`, report.TaskID, nodeID))
	if err != nil {
		return domain.NodeUpgradeTask{}, err
	}
	var latestID string
	if err := tx.QueryRow(`SELECT id FROM node_upgrade_tasks WHERE node_id = ? ORDER BY created_at DESC LIMIT 1`, nodeID).Scan(&latestID); err != nil {
		return domain.NodeUpgradeTask{}, err
	}
	if latestID != task.ID {
		return domain.NodeUpgradeTask{}, errors.New("upgrade report does not belong to the latest node task")
	}
	updated := now()
	switch report.Status {
	case domain.NodeUpgradeApplying:
		if task.Status != domain.NodeUpgradeQueued && task.Status != domain.NodeUpgradeApplying {
			return task, tx.Commit()
		}
		_, err = tx.Exec(`UPDATE node_upgrade_tasks SET status = ?, error_code = '', detail = ?,
			started_at = COALESCE(started_at, ?), updated_at = ? WHERE id = ?`,
			domain.NodeUpgradeApplying, report.Detail, stamp(updated), stamp(updated), task.ID)
	case domain.NodeUpgradeSucceeded:
		if !strings.EqualFold(strings.TrimSpace(report.InstalledSHA256), task.TargetSHA256) {
			return domain.NodeUpgradeTask{}, errors.New("installed edge digest does not match upgrade target")
		}
		if task.TargetNginxSHA256 != "" && !strings.EqualFold(strings.TrimSpace(report.InstalledNginxSHA256), task.TargetNginxSHA256) {
			return domain.NodeUpgradeTask{}, errors.New("installed Nginx digest does not match upgrade target")
		}
		_, err = tx.Exec(`UPDATE node_upgrade_tasks SET status = ?, error_code = '', detail = ?,
			started_at = COALESCE(started_at, ?), completed_at = ?, updated_at = ? WHERE id = ?`,
			domain.NodeUpgradeSucceeded, report.Detail, stamp(updated), stamp(updated), stamp(updated), task.ID)
	case domain.NodeUpgradeFailed:
		if task.Status == domain.NodeUpgradeSucceeded {
			return task, tx.Commit()
		}
		var installedSHA256, installedNginxSHA256 string
		if report.ErrorCode == "updater_interrupted" {
			if err := tx.QueryRow(`SELECT agent_sha256, nginx_sha256 FROM nodes WHERE id = ?`, nodeID).Scan(&installedSHA256, &installedNginxSHA256); err != nil {
				return domain.NodeUpgradeTask{}, err
			}
		}
		if strings.EqualFold(strings.TrimSpace(installedSHA256), task.TargetSHA256) &&
			(task.TargetNginxSHA256 == "" || strings.EqualFold(strings.TrimSpace(installedNginxSHA256), task.TargetNginxSHA256)) {
			_, err = tx.Exec(`UPDATE node_upgrade_tasks SET status = ?, error_code = '', detail = ?,
				started_at = COALESCE(started_at, ?), completed_at = ?, updated_at = ? WHERE id = ?`,
				domain.NodeUpgradeSucceeded, recoveredUpgradeDetail, stamp(updated), stamp(updated), stamp(updated), task.ID)
		} else {
			_, err = tx.Exec(`UPDATE node_upgrade_tasks SET status = ?, error_code = ?, detail = ?,
				started_at = COALESCE(started_at, ?), completed_at = ?, updated_at = ? WHERE id = ?`,
				domain.NodeUpgradeFailed, report.ErrorCode, report.Detail, stamp(updated), stamp(updated), stamp(updated), task.ID)
		}
	default:
		return domain.NodeUpgradeTask{}, errors.New("invalid node upgrade report status")
	}
	if err != nil {
		return domain.NodeUpgradeTask{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.NodeUpgradeTask{}, err
	}
	return s.NodeUpgradeTask(task.ID)
}

func (s *Store) ReconcileNodeUpgrades() error {
	completed := now()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE node_upgrade_tasks SET status = ?, error_code = 'upgrade_timeout',
		detail = 'edge did not complete the online upgrade before the deadline', completed_at = ?, updated_at = ?
		WHERE status IN (?, ?) AND deadline_at <= ?`,
		domain.NodeUpgradeFailed, stamp(completed), stamp(completed),
		domain.NodeUpgradeQueued, domain.NodeUpgradeApplying, stamp(completed)); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE node_upgrade_tasks SET status = ?, error_code = '', detail = ?,
		completed_at = ?, updated_at = ?
		WHERE status = ? AND error_code = 'updater_interrupted'
		AND NOT EXISTS (SELECT 1 FROM node_upgrade_tasks AS newer
			WHERE newer.node_id = node_upgrade_tasks.node_id AND newer.created_at > node_upgrade_tasks.created_at)
		AND EXISTS (SELECT 1 FROM nodes
				WHERE nodes.id = node_upgrade_tasks.node_id
				AND lower(nodes.agent_sha256) = lower(node_upgrade_tasks.target_sha256)
				AND (node_upgrade_tasks.target_nginx_sha256 = '' OR lower(nodes.nginx_sha256) = lower(node_upgrade_tasks.target_nginx_sha256))
				AND nodes.active_upgrade_task_id = '')`,
		domain.NodeUpgradeSucceeded, recoveredUpgradeDetail, stamp(completed), stamp(completed), domain.NodeUpgradeFailed); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) HasActiveNodeUpgrade(nodeID string) (bool, error) {
	var active int
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM node_upgrade_tasks WHERE node_id = ? AND status IN (?, ?))`,
		nodeID, domain.NodeUpgradeQueued, domain.NodeUpgradeApplying).Scan(&active)
	return active != 0, err
}

func (s *Store) HasActiveNodePublication(nodeID string) (bool, error) {
	var active int
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM publish_task_nodes
		JOIN deployment_tasks ON deployment_tasks.id = publish_task_nodes.task_id
		WHERE publish_task_nodes.node_id = ? AND publish_task_nodes.status = ?
		AND deployment_tasks.status IN (?, ?, ?))`, nodeID, domain.PublishNodePending,
		domain.TaskQueued, domain.TaskDispatching, domain.TaskApplying).Scan(&active)
	return active != 0, err
}

func (s *Store) EnsureNodesNotUpgrading(nodeIDs []string) error {
	seen := make(map[string]bool, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if nodeID == "" || seen[nodeID] {
			continue
		}
		seen[nodeID] = true
		active, err := s.HasActiveNodeUpgrade(nodeID)
		if err != nil {
			return err
		}
		if active {
			return fmt.Errorf("node %s: %w", nodeID, ErrNodeUpgradeActive)
		}
	}
	return nil
}

func ensureNodesNotUpgradingTx(tx *sql.Tx, nodeIDs []string) error {
	seen := make(map[string]bool, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if nodeID == "" || seen[nodeID] {
			continue
		}
		seen[nodeID] = true
		var active int
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM node_upgrade_tasks
			WHERE node_id = ? AND status IN (?, ?))`, nodeID,
			domain.NodeUpgradeQueued, domain.NodeUpgradeApplying).Scan(&active); err != nil {
			return err
		}
		if active != 0 {
			return fmt.Errorf("node %s: %w", nodeID, ErrNodeUpgradeActive)
		}
	}
	return nil
}

func upgradedNodeIDs(updates []NodeStateUpdate, targets []PublishTaskNode) []string {
	nodeIDs := make([]string, 0, len(updates)+len(targets))
	for _, update := range updates {
		nodeIDs = append(nodeIDs, update.NodeID)
	}
	for _, target := range targets {
		nodeIDs = append(nodeIDs, target.NodeID)
	}
	return nodeIDs
}

func (s *Store) FailActiveNodeUpgrade(nodeID, code, detail string) error {
	completed := now()
	_, err := s.db.Exec(`UPDATE node_upgrade_tasks SET status = ?, error_code = ?, detail = ?,
		completed_at = ?, updated_at = ? WHERE node_id = ? AND status IN (?, ?)`,
		domain.NodeUpgradeFailed, code, detail, stamp(completed), stamp(completed), nodeID,
		domain.NodeUpgradeQueued, domain.NodeUpgradeApplying)
	return err
}

func scanNodeUpgradeTask(row scanner) (domain.NodeUpgradeTask, error) {
	var task domain.NodeUpgradeTask
	var deadlineAt, createdAt, updatedAt string
	var startedAt, completedAt sql.NullString
	err := row.Scan(&task.ID, &task.NodeID, &task.Status, &task.SourceSHA256, &task.TargetSHA256,
		&task.SourceNginxSHA256, &task.TargetNginxSHA256,
		&task.ErrorCode, &task.Detail, &deadlineAt, &startedAt, &completedAt, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.NodeUpgradeTask{}, ErrNotFound
	}
	if err != nil {
		return domain.NodeUpgradeTask{}, err
	}
	if task.DeadlineAt, err = parseTime(deadlineAt); err != nil {
		return domain.NodeUpgradeTask{}, err
	}
	if startedAt.Valid {
		value, parseErr := parseTime(startedAt.String)
		if parseErr != nil {
			return domain.NodeUpgradeTask{}, parseErr
		}
		task.StartedAt = &value
	}
	if completedAt.Valid {
		value, parseErr := parseTime(completedAt.String)
		if parseErr != nil {
			return domain.NodeUpgradeTask{}, parseErr
		}
		task.CompletedAt = &value
	}
	if task.CreatedAt, err = parseTime(createdAt); err != nil {
		return domain.NodeUpgradeTask{}, err
	}
	if task.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return domain.NodeUpgradeTask{}, err
	}
	return task, nil
}
