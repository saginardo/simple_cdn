package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"simple_cdn/internal/domain"
)

var ErrUpgradeRolloutActive = errors.New("节点已纳入进行中的分批升级")
var ErrUpgradeRolloutConflict = errors.New("分批升级状态已变化，请刷新后重试")

func migrateNodeUpgradeRollouts(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS node_upgrade_rollouts (
 id TEXT PRIMARY KEY, state TEXT NOT NULL, revision INTEGER NOT NULL,
 body TEXT NOT NULL, instructions TEXT NOT NULL, target_nginx_sha256 TEXT NOT NULL,
 created_at TEXT NOT NULL);
 CREATE UNIQUE INDEX IF NOT EXISTS idx_upgrade_rollout_active ON node_upgrade_rollouts((1)) WHERE state IN ('running', 'paused');`)
	return err
}

func (s *Store) CreateUpgradeRollout(rollout domain.NodeUpgradeRollout, instructions map[string]domain.NodeUpgradeInstruction) (domain.NodeUpgradeRollout, error) {
	if len(rollout.Members) == 0 || rollout.MaxParallel < 1 || rollout.MaxParallel > 10 || rollout.HealthWindowSeconds < 30 || rollout.HealthWindowSeconds > 600 {
		return rollout, errors.New("invalid rollout limits")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return rollout, err
	}
	defer tx.Rollback()
	var active int
	if err = tx.QueryRow(`SELECT count(*) FROM node_upgrade_rollouts WHERE state IN ('running','paused')`).Scan(&active); err != nil {
		return rollout, err
	}
	if active != 0 {
		return rollout, ErrUpgradeRolloutActive
	}
	seen := map[string]bool{}
	for i := range rollout.Members {
		member := &rollout.Members[i]
		if seen[member.NodeID] {
			return rollout, errors.New("duplicate rollout node")
		}
		seen[member.NodeID] = true
		instruction, ok := instructions[member.NodeID]
		if !ok || instruction.Binary.SHA256 != rollout.TargetAgentSHA256 || (instruction.NginxBundle != nil && instruction.NginxBundle.SHA256 != rollout.TargetNginxSHA256) {
			return rollout, errors.New("rollout artifact mismatch")
		}
		if err = tx.QueryRow(`SELECT count(*) FROM nodes WHERE id=?`, member.NodeID).Scan(&active); err != nil {
			return rollout, err
		}
		if active != 1 {
			return rollout, ErrNotFound
		}
		if err = tx.QueryRow(`SELECT count(*) FROM node_upgrade_tasks WHERE node_id=? AND status IN ('queued','applying')`, member.NodeID).Scan(&active); err != nil {
			return rollout, err
		}
		if active != 0 {
			return rollout, ErrNodeUpgradeActive
		}
		member.State = "pending"
		member.TaskID = ""
	}
	rollout.ID = uuid.NewString()
	rollout.State = "running"
	rollout.Revision = 1
	rollout.CreatedAt = now()
	rollout.UpdatedAt = rollout.CreatedAt
	rollout.Detail = "等待金丝雀节点通过健康观察"
	body, err := json.Marshal(rollout)
	if err != nil {
		return rollout, err
	}
	frozen, err := json.Marshal(instructions)
	if err != nil {
		return rollout, err
	}
	_, err = tx.Exec(`INSERT INTO node_upgrade_rollouts(id,state,revision,body,instructions,target_nginx_sha256,created_at) VALUES(?,?,?,?,?,?,?)`, rollout.ID, rollout.State, rollout.Revision, string(body), string(frozen), rollout.TargetNginxSHA256, stamp(rollout.CreatedAt))
	if err != nil {
		return rollout, err
	}
	return rollout, tx.Commit()
}

func scanUpgradeRollout(row scanner) (domain.NodeUpgradeRollout, error) {
	var body string
	var result domain.NodeUpgradeRollout
	err := row.Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	err = json.Unmarshal([]byte(body), &result)
	return result, err
}

func (s *Store) LatestUpgradeRollout() (domain.NodeUpgradeRollout, error) {
	return scanUpgradeRollout(s.db.QueryRow(`SELECT body FROM node_upgrade_rollouts ORDER BY created_at DESC,id DESC LIMIT 1`))
}
func (s *Store) UpgradeRollout(id string) (domain.NodeUpgradeRollout, error) {
	return scanUpgradeRollout(s.db.QueryRow(`SELECT body FROM node_upgrade_rollouts WHERE id=?`, id))
}
func (s *Store) NodeReservedByUpgradeRollout(nodeID string) (bool, error) {
	var count int
	err := s.db.QueryRow(`SELECT count(*) FROM node_upgrade_rollouts,json_each(body,'$.members') AS member WHERE state IN ('running','paused') AND json_extract(member.value,'$.node_id')=?`, nodeID).Scan(&count)
	return count > 0, err
}

// Revision comparison prevents stale workers or API requests from overwriting a
// pause, cancellation, or another dispatch. Task insertion uses the same tx.
func saveUpgradeRolloutTx(tx *sql.Tx, rollout *domain.NodeUpgradeRollout) error {
	previous := rollout.Revision
	rollout.Revision++
	rollout.UpdatedAt = now()
	body, err := json.Marshal(rollout)
	if err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE node_upgrade_rollouts SET state=?,revision=?,body=? WHERE id=? AND revision=?`, rollout.State, rollout.Revision, string(body), rollout.ID, previous)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrUpgradeRolloutConflict
	}
	return nil
}
func (s *Store) SaveUpgradeRollout(rollout *domain.NodeUpgradeRollout) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = saveUpgradeRolloutTx(tx, rollout); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ChangeUpgradeRollout(id, action string) (domain.NodeUpgradeRollout, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return domain.NodeUpgradeRollout{}, err
	}
	defer tx.Rollback()
	rollout, err := scanUpgradeRollout(tx.QueryRow(`SELECT body FROM node_upgrade_rollouts WHERE id=?`, id))
	if err != nil {
		return rollout, err
	}
	if rollout.State != "running" && rollout.State != "paused" {
		return rollout, ErrUpgradeRolloutConflict
	}
	switch action {
	case "pause":
		rollout.State = "paused"
		rollout.Detail = "已暂停派发，正在执行的节点将继续完成"
	case "resume":
		rollout.State = "running"
		rollout.Detail = "已恢复分批升级"
		for i := range rollout.Members {
			member := &rollout.Members[i]
			if member.State == "failed" {
				member.State = "pending"
				member.HealthySince = nil
				member.LastObservedAt = nil
				member.VerificationStartedAt = nil
				member.Detail = "等待重试"
			}
		}
	case "cancel":
		rollout.State = "cancelled"
		rollout.Detail = "已取消后续派发，已下发的升级仍会执行"
		for i := range rollout.Members {
			if rollout.Members[i].State == "pending" || rollout.Members[i].State == "failed" {
				rollout.Members[i].State = "skipped"
			}
		}
	default:
		return rollout, errors.New("invalid rollout action")
	}
	if err = saveUpgradeRolloutTx(tx, &rollout); err != nil {
		return rollout, err
	}
	return rollout, tx.Commit()
}

func (s *Store) DispatchUpgradeRolloutNode(id, nodeID string, deadline time.Time) (domain.NodeUpgradeRollout, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return domain.NodeUpgradeRollout{}, err
	}
	defer tx.Rollback()
	rollout, err := scanUpgradeRollout(tx.QueryRow(`SELECT body FROM node_upgrade_rollouts WHERE id=?`, id))
	if err != nil {
		return rollout, err
	}
	if rollout.State != "running" || !deadline.After(now()) {
		return rollout, ErrUpgradeRolloutConflict
	}
	active, index := 0, -1
	for i, member := range rollout.Members {
		if member.State == "upgrading" || member.State == "verifying" {
			active++
		}
		if member.NodeID == nodeID {
			index = i
		}
		if member.State == "failed" {
			return rollout, ErrUpgradeRolloutConflict
		}
	}
	if index < 0 {
		return rollout, ErrNotFound
	}
	member := &rollout.Members[index]
	if member.State != "pending" || active >= rollout.MaxParallel || (index != 0 && rollout.Members[0].State != "succeeded") {
		return rollout, ErrUpgradeRolloutConflict
	}
	// A health-gate retry after a successful install only repeats verification.
	if member.TaskID != "" {
		task, e := scanNodeUpgradeTask(tx.QueryRow(`SELECT `+nodeUpgradeTaskColumns+` FROM node_upgrade_tasks WHERE id=?`, member.TaskID))
		if e != nil {
			return rollout, e
		}
		if task.Status == domain.NodeUpgradeSucceeded {
			member.State = "verifying"
			member.Detail = "重新观察节点健康"
			started := now()
			member.VerificationStartedAt = &started
			if err = saveUpgradeRolloutTx(tx, &rollout); err != nil {
				return rollout, err
			}
			return rollout, tx.Commit()
		}
	}
	var frozen string
	if err = tx.QueryRow(`SELECT instructions FROM node_upgrade_rollouts WHERE id=?`, id).Scan(&frozen); err != nil {
		return rollout, err
	}
	instructions := map[string]domain.NodeUpgradeInstruction{}
	if err = json.Unmarshal([]byte(frozen), &instructions); err != nil {
		return rollout, err
	}
	instruction, ok := instructions[nodeID]
	if !ok {
		return rollout, errors.New("missing frozen rollout instruction")
	}
	task, created, err := createOrGetNodeUpgradeTx(tx, nodeID, instruction, deadline, id)
	if err != nil {
		return rollout, err
	}
	if !created {
		return rollout, fmt.Errorf("%w: unrelated task %s", ErrNodeUpgradeActive, task.ID)
	}
	member.TaskID = task.ID
	member.State = "upgrading"
	member.Detail = "等待节点完成升级"
	if err = saveUpgradeRolloutTx(tx, &rollout); err != nil {
		return rollout, err
	}
	return rollout, tx.Commit()
}
