package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	"simple_cdn/internal/domain"
)

const (
	MonitoringHealthyScore   = 80
	MonitoringAutoPauseAfter = 4
)

var (
	ErrMonitoringTargetExists     = errors.New("monitoring target already exists")
	ErrMonitoringTargetNameExists = errors.New("monitoring target name already exists")
	ErrMonitoringTargetLimit      = errors.New("monitoring target limit reached")
	ErrMonitoringTargetsChanged   = errors.New("monitoring targets changed; retry with the latest targets")
	ErrMonitoringReportStale      = errors.New("monitoring report is not newer than the stored report")
)

const monitoringTargetColumns = `id, name, address, enabled, created_at, updated_at`

type NodeMonitoringStatus struct {
	NodeID              string     `json:"node_id"`
	Score               int        `json:"score"`
	SuccessRate         float64    `json:"success_rate"`
	AverageLatencyMS    float64    `json:"average_latency_ms"`
	ConsecutiveAbnormal int        `json:"consecutive_abnormal"`
	LastCheckedAt       *time.Time `json:"last_checked_at,omitempty"`
}

type MonitoringProbeSnapshot struct {
	NodeID             string    `json:"node_id"`
	TargetID           string    `json:"target_id"`
	Attempts           int       `json:"attempts"`
	SuccessfulAttempts int       `json:"successful_attempts"`
	AverageLatencyMS   float64   `json:"average_latency_ms"`
	Error              string    `json:"error,omitempty"`
	CheckedAt          time.Time `json:"checked_at"`
}

type MonitoringRoundOutcome struct {
	Status        NodeMonitoringStatus
	NodeStatus    domain.NodeStatus
	StatusChanged bool
	AutoPaused    bool
	Transition    SmartRoutingTransition
}

func (s *Store) ListMonitoringTargets(enabledOnly bool) ([]domain.MonitoringTarget, error) {
	query := `SELECT ` + monitoringTargetColumns + ` FROM monitoring_targets`
	if enabledOnly {
		query += ` WHERE enabled = 1`
	}
	query += ` ORDER BY name COLLATE NOCASE, address`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := make([]domain.MonitoringTarget, 0)
	for rows.Next() {
		target, err := scanMonitoringTarget(rows)
		if err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func (s *Store) CreateMonitoringTarget(name, address string) (domain.MonitoringTarget, error) {
	normalizedName, err := domain.NormalizeMonitoringTargetName(name)
	if err != nil {
		return domain.MonitoringTarget{}, err
	}
	normalizedAddress, err := domain.NormalizeMonitoringAddress(address)
	if err != nil {
		return domain.MonitoringTarget{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return domain.MonitoringTarget{}, err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM monitoring_targets`).Scan(&count); err != nil {
		return domain.MonitoringTarget{}, err
	}
	if count >= domain.MaxMonitoringTargets {
		return domain.MonitoringTarget{}, ErrMonitoringTargetLimit
	}
	var existing string
	if err := tx.QueryRow(`SELECT id FROM monitoring_targets WHERE address = ?`, normalizedAddress).Scan(&existing); err == nil {
		return domain.MonitoringTarget{}, ErrMonitoringTargetExists
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.MonitoringTarget{}, err
	}
	if err := tx.QueryRow(`SELECT id FROM monitoring_targets WHERE name = ? COLLATE NOCASE`, normalizedName).Scan(&existing); err == nil {
		return domain.MonitoringTarget{}, ErrMonitoringTargetNameExists
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.MonitoringTarget{}, err
	}
	created := now()
	target := domain.MonitoringTarget{ID: uuid.NewString(), Name: normalizedName, Address: normalizedAddress, Enabled: true, CreatedAt: created, UpdatedAt: created}
	if _, err := tx.Exec(`INSERT INTO monitoring_targets(id, name, address, enabled, created_at, updated_at) VALUES (?, ?, ?, 1, ?, ?)`,
		target.ID, target.Name, target.Address, stamp(created), stamp(created)); err != nil {
		return domain.MonitoringTarget{}, err
	}
	if err := resetMonitoringStateTx(tx); err != nil {
		return domain.MonitoringTarget{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.MonitoringTarget{}, err
	}
	return target, nil
}

func (s *Store) SetMonitoringTargetEnabled(targetID string, enabled bool) (domain.MonitoringTarget, error) {
	return s.UpdateMonitoringTarget(targetID, nil, &enabled)
}

func (s *Store) UpdateMonitoringTarget(targetID string, name *string, enabled *bool) (domain.MonitoringTarget, error) {
	if name == nil && enabled == nil {
		return domain.MonitoringTarget{}, errors.New("monitoring target update is empty")
	}
	var normalizedName string
	var err error
	if name != nil {
		normalizedName, err = domain.NormalizeMonitoringTargetName(*name)
		if err != nil {
			return domain.MonitoringTarget{}, err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return domain.MonitoringTarget{}, err
	}
	defer tx.Rollback()
	current, err := scanMonitoringTarget(tx.QueryRow(`SELECT `+monitoringTargetColumns+` FROM monitoring_targets WHERE id = ?`, targetID))
	if err != nil {
		return domain.MonitoringTarget{}, err
	}
	nextName, nextEnabled := current.Name, current.Enabled
	if name != nil {
		nextName = normalizedName
		var existing string
		if err := tx.QueryRow(`SELECT id FROM monitoring_targets WHERE name = ? COLLATE NOCASE AND id != ?`, nextName, targetID).Scan(&existing); err == nil {
			return domain.MonitoringTarget{}, ErrMonitoringTargetNameExists
		} else if !errors.Is(err, sql.ErrNoRows) {
			return domain.MonitoringTarget{}, err
		}
	}
	if enabled != nil {
		nextEnabled = *enabled
	}
	nameChanged := nextName != current.Name
	enabledChanged := nextEnabled != current.Enabled
	if !nameChanged && !enabledChanged {
		if err := tx.Commit(); err != nil {
			return domain.MonitoringTarget{}, err
		}
		return current, nil
	}
	result, err := tx.Exec(`UPDATE monitoring_targets SET name = ?, enabled = ?, updated_at = ? WHERE id = ?`, nextName, boolInt(nextEnabled), stamp(now()), targetID)
	if err != nil {
		return domain.MonitoringTarget{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return domain.MonitoringTarget{}, err
	}
	if changed != 1 {
		return domain.MonitoringTarget{}, ErrNotFound
	}
	if enabledChanged {
		if err := resetMonitoringStateTx(tx); err != nil {
			return domain.MonitoringTarget{}, err
		}
	}
	target, err := scanMonitoringTarget(tx.QueryRow(`SELECT `+monitoringTargetColumns+` FROM monitoring_targets WHERE id = ?`, targetID))
	if err != nil {
		return domain.MonitoringTarget{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.MonitoringTarget{}, err
	}
	return target, nil
}

func (s *Store) DeleteMonitoringTarget(targetID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`DELETE FROM monitoring_targets WHERE id = ?`, targetID)
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
	if err := resetMonitoringStateTx(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func resetMonitoringStateTx(tx *sql.Tx) error {
	if _, err := tx.Exec(`UPDATE node_smart_routing SET score_low_streak = 0, score_recovery_streak = 0, updated_at = ?
		WHERE score_low_streak != 0 OR score_recovery_streak != 0`, stamp(now())); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM monitoring_probe_results`); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM node_monitoring_status`)
	return err
}

func scanMonitoringTarget(row scanner) (domain.MonitoringTarget, error) {
	var target domain.MonitoringTarget
	var enabled int
	var createdAt, updatedAt string
	if err := row.Scan(&target.ID, &target.Name, &target.Address, &enabled, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.MonitoringTarget{}, ErrNotFound
		}
		return domain.MonitoringTarget{}, err
	}
	var err error
	target.Enabled = enabled != 0
	target.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return domain.MonitoringTarget{}, err
	}
	target.UpdatedAt, err = parseTime(updatedAt)
	return target, err
}

func (s *Store) RecordMonitoringRound(nodeID string, results []domain.MonitoringProbeResult) (MonitoringRoundOutcome, error) {
	for _, result := range results {
		if !domain.ValidMonitoringProbeResult(result) {
			return MonitoringRoundOutcome{}, errors.New("invalid monitoring probe result")
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return MonitoringRoundOutcome{}, err
	}
	defer tx.Rollback()
	targetRows, err := tx.Query(`SELECT id FROM monitoring_targets WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return MonitoringRoundOutcome{}, err
	}
	targetIDs := make([]string, 0)
	for targetRows.Next() {
		var targetID string
		if err := targetRows.Scan(&targetID); err != nil {
			targetRows.Close()
			return MonitoringRoundOutcome{}, err
		}
		targetIDs = append(targetIDs, targetID)
	}
	if err := targetRows.Err(); err != nil {
		targetRows.Close()
		return MonitoringRoundOutcome{}, err
	}
	targetRows.Close()
	if len(targetIDs) == 0 || !monitoringResultsMatchTargets(results, targetIDs) {
		return MonitoringRoundOutcome{}, ErrMonitoringTargetsChanged
	}
	recordedAt := now()
	if err := ensureSmartRoutingPolicyTx(tx, nodeID, recordedAt); err != nil {
		return MonitoringRoundOutcome{}, err
	}
	policy, err := scanSmartRoutingPolicy(tx.QueryRow(`SELECT `+smartRoutingPolicyColumns+` FROM node_smart_routing WHERE node_id = ?`, nodeID))
	if err != nil {
		return MonitoringRoundOutcome{}, err
	}
	priorAbnormal := 0
	var priorCheckedAt sql.NullString
	if err := tx.QueryRow(`SELECT consecutive_abnormal, last_checked_at FROM node_monitoring_status WHERE node_id = ?`, nodeID).Scan(&priorAbnormal, &priorCheckedAt); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return MonitoringRoundOutcome{}, err
	}
	status := scoreMonitoringRound(nodeID, results)
	if priorCheckedAt.Valid {
		prior, err := parseTime(priorCheckedAt.String)
		if err != nil {
			return MonitoringRoundOutcome{}, err
		}
		if !status.LastCheckedAt.After(prior) {
			return MonitoringRoundOutcome{}, ErrMonitoringReportStale
		}
	}
	if status.Score < MonitoringHealthyScore {
		status.ConsecutiveAbnormal = priorAbnormal + 1
	}
	transition := SmartRoutingTransition{
		NodeID: nodeID, PreviousScoreGate: policy.ScoreGate, ScoreGate: policy.ScoreGate,
		PreviousScheduleGate: policy.ScheduleGate, ScheduleGate: policy.ScheduleGate,
	}
	if policy.Enabled && policy.ScoreEnabled {
		transition.ScoreGateChanged = advanceSmartRoutingScore(&policy, status.Score)
		transition.ScoreGate = policy.ScoreGate
		policy.UpdatedAt = recordedAt
		if _, err := tx.Exec(`UPDATE node_smart_routing SET score_gate = ?, score_low_streak = ?, score_recovery_streak = ?, updated_at = ? WHERE node_id = ?`,
			policy.ScoreGate, policy.ScoreLowStreak, policy.ScoreRecoveryStreak, stamp(policy.UpdatedAt), nodeID); err != nil {
			return MonitoringRoundOutcome{}, err
		}
	}
	if err := reconcileSmartRoutingNodeTx(tx, &policy, &transition, recordedAt); err != nil {
		return MonitoringRoundOutcome{}, err
	}
	if _, err := tx.Exec(`INSERT INTO node_monitoring_status(node_id, score, success_rate, average_latency_ms, consecutive_abnormal, last_checked_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(node_id) DO UPDATE SET score=excluded.score, success_rate=excluded.success_rate,
		average_latency_ms=excluded.average_latency_ms, consecutive_abnormal=excluded.consecutive_abnormal,
		last_checked_at=excluded.last_checked_at, updated_at=excluded.updated_at`, nodeID, status.Score, status.SuccessRate,
		status.AverageLatencyMS, status.ConsecutiveAbnormal, stamp(*status.LastCheckedAt), stamp(now())); err != nil {
		return MonitoringRoundOutcome{}, err
	}
	if _, err := tx.Exec(`DELETE FROM monitoring_probe_results WHERE node_id = ?`, nodeID); err != nil {
		return MonitoringRoundOutcome{}, err
	}
	for _, result := range results {
		if _, err := tx.Exec(`INSERT INTO monitoring_probe_results(node_id, target_id, attempts, successful_attempts, average_latency_ms, error, checked_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, nodeID, result.TargetID, result.Attempts, result.SuccessfulAttempts,
			result.AverageLatencyMS, result.Error, stamp(result.CheckedAt)); err != nil {
			return MonitoringRoundOutcome{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return MonitoringRoundOutcome{}, err
	}
	return MonitoringRoundOutcome{
		Status: status, NodeStatus: transition.NodeStatus, StatusChanged: transition.StatusChanged,
		AutoPaused: transition.AutoPaused, Transition: transition,
	}, nil
}

func monitoringResultsMatchTargets(results []domain.MonitoringProbeResult, targetIDs []string) bool {
	if len(results) != len(targetIDs) {
		return false
	}
	reported := make([]string, len(results))
	for index, result := range results {
		reported[index] = result.TargetID
	}
	sort.Strings(reported)
	for index := range targetIDs {
		if reported[index] != targetIDs[index] || index > 0 && reported[index] == reported[index-1] {
			return false
		}
	}
	return true
}

func scoreMonitoringRound(nodeID string, results []domain.MonitoringProbeResult) NodeMonitoringStatus {
	totalAttempts := 0
	successfulAttempts := 0
	weightedLatency := 0.0
	checkedAt := results[0].CheckedAt.UTC()
	for _, result := range results {
		totalAttempts += result.Attempts
		successfulAttempts += result.SuccessfulAttempts
		weightedLatency += result.AverageLatencyMS * float64(result.SuccessfulAttempts)
		if result.CheckedAt.After(checkedAt) {
			checkedAt = result.CheckedAt.UTC()
		}
	}
	successRate := 100 * float64(successfulAttempts) / float64(totalAttempts)
	averageLatency := 0.0
	latencyScore := 0.0
	if successfulAttempts > 0 {
		averageLatency = weightedLatency / float64(successfulAttempts)
		latencyScore = 100
		if averageLatency > 100 {
			latencyScore = math.Max(0, 100-(averageLatency-100)*(100.0/900.0))
		}
	}
	// Reachability dominates the score. Successful TCP connects receive full
	// latency credit through 100 ms, tapering linearly to zero at 1 second.
	score := int(math.Round(successRate*0.7 + latencyScore*0.3))
	return NodeMonitoringStatus{
		NodeID: nodeID, Score: score, SuccessRate: successRate, AverageLatencyMS: averageLatency, LastCheckedAt: &checkedAt,
	}
}

func (s *Store) ListNodeMonitoringStatuses() ([]NodeMonitoringStatus, error) {
	rows, err := s.db.Query(`SELECT node_id, score, success_rate, average_latency_ms, consecutive_abnormal, last_checked_at FROM node_monitoring_status ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	statuses := make([]NodeMonitoringStatus, 0)
	for rows.Next() {
		var status NodeMonitoringStatus
		var checkedAt string
		if err := rows.Scan(&status.NodeID, &status.Score, &status.SuccessRate, &status.AverageLatencyMS, &status.ConsecutiveAbnormal, &checkedAt); err != nil {
			return nil, err
		}
		parsed, err := parseTime(checkedAt)
		if err != nil {
			return nil, err
		}
		status.LastCheckedAt = &parsed
		statuses = append(statuses, status)
	}
	return statuses, rows.Err()
}

func (s *Store) ListMonitoringProbeSnapshots() ([]MonitoringProbeSnapshot, error) {
	rows, err := s.db.Query(`SELECT node_id, target_id, attempts, successful_attempts, average_latency_ms, error, checked_at
		FROM monitoring_probe_results ORDER BY node_id, target_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := make([]MonitoringProbeSnapshot, 0)
	for rows.Next() {
		var result MonitoringProbeSnapshot
		var checkedAt string
		if err := rows.Scan(&result.NodeID, &result.TargetID, &result.Attempts, &result.SuccessfulAttempts, &result.AverageLatencyMS, &result.Error, &checkedAt); err != nil {
			return nil, err
		}
		parsed, err := parseTime(checkedAt)
		if err != nil {
			return nil, err
		}
		result.CheckedAt = parsed
		results = append(results, result)
	}
	return results, rows.Err()
}

func (status NodeMonitoringStatus) String() string {
	return fmt.Sprintf("score=%d success=%.1f%% latency=%.1fms abnormal=%d", status.Score, status.SuccessRate, status.AverageLatencyMS, status.ConsecutiveAbnormal)
}
