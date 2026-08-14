package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"simple_cdn/internal/domain"
)

const (
	SmartRoutingTimezone         = "Asia/Shanghai"
	SmartRoutingMaxWindows       = 32
	SmartRoutingMaxRounds        = 120
	SmartRoutingDefaultScore     = 80
	SmartRoutingDefaultLowRounds = 4
	SmartRoutingMinResumeRounds  = 3
)

const smartRoutingPolicyColumns = `node_id, enabled, score_enabled, score_pause_below,
	score_pause_rounds, score_resume_at, score_resume_rounds, score_gate,
	score_low_streak, score_recovery_streak, schedule_enabled,
	schedule_windows_json, schedule_gate, updated_at`

const (
	SmartRoutingScoreUnknown   = "unknown"
	SmartRoutingScoreAllowed   = "allowed"
	SmartRoutingScoreBlocked   = "blocked"
	SmartRoutingScheduleOpen   = "open"
	SmartRoutingScheduleClosed = "closed"
)

var smartRoutingLocation = time.FixedZone(SmartRoutingTimezone, 8*60*60)

type SmartRoutingWindow struct {
	Weekdays []int  `json:"weekdays"`
	Start    string `json:"start"`
	End      string `json:"end"`
}

type SmartRoutingScoreConfig struct {
	Enabled           bool `json:"enabled"`
	PauseBelowScore   int  `json:"pause_below_score"`
	PauseAfterRounds  int  `json:"pause_after_rounds"`
	ResumeAtScore     int  `json:"resume_at_score"`
	ResumeAfterRounds int  `json:"resume_after_rounds"`
}

type SmartRoutingScheduleConfig struct {
	Enabled bool                 `json:"enabled"`
	Windows []SmartRoutingWindow `json:"windows"`
}

type SmartRoutingConfig struct {
	Enabled  bool                       `json:"enabled"`
	Score    SmartRoutingScoreConfig    `json:"score"`
	Schedule SmartRoutingScheduleConfig `json:"schedule"`
}

type SmartRoutingPolicy struct {
	NodeID              string
	Enabled             bool
	ScoreEnabled        bool
	ScorePauseBelow     int
	ScorePauseRounds    int
	ScoreResumeAt       int
	ScoreResumeRounds   int
	ScoreGate           string
	ScoreLowStreak      int
	ScoreRecoveryStreak int
	ScheduleEnabled     bool
	ScheduleWindows     []SmartRoutingWindow
	ScheduleGate        string
	UpdatedAt           time.Time
}

type SmartRoutingNodeState struct {
	Node          domain.Node
	Policy        SmartRoutingPolicy
	CurrentScore  *int
	LastCheckedAt *time.Time
}

type SmartRoutingTransition struct {
	NodeID               string
	PreviousStatus       domain.NodeStatus
	NodeStatus           domain.NodeStatus
	StatusChanged        bool
	PreviousAutoPaused   bool
	AutoPaused           bool
	AutoPausedChanged    bool
	PreviousScoreGate    string
	ScoreGate            string
	ScoreGateChanged     bool
	PreviousScheduleGate string
	ScheduleGate         string
	ScheduleGateChanged  bool
	BlockedBy            []string
}

type SmartRoutingUpdateOutcome struct {
	Policy        SmartRoutingPolicy
	Transition    SmartRoutingTransition
	ConfigChanged bool
}

func DefaultSmartRoutingConfig() SmartRoutingConfig {
	return SmartRoutingConfig{
		Enabled: true,
		Score: SmartRoutingScoreConfig{
			Enabled: true, PauseBelowScore: SmartRoutingDefaultScore,
			PauseAfterRounds: SmartRoutingDefaultLowRounds,
			ResumeAtScore:    SmartRoutingDefaultScore, ResumeAfterRounds: SmartRoutingMinResumeRounds,
		},
		Schedule: SmartRoutingScheduleConfig{Windows: []SmartRoutingWindow{}},
	}
}

func NormalizeSmartRoutingConfig(input SmartRoutingConfig) (SmartRoutingConfig, error) {
	if input.Score.PauseBelowScore < 1 || input.Score.PauseBelowScore > 100 {
		return SmartRoutingConfig{}, errors.New("score pause threshold must be between 1 and 100")
	}
	if input.Score.ResumeAtScore < 1 || input.Score.ResumeAtScore > 100 {
		return SmartRoutingConfig{}, errors.New("score resume threshold must be between 1 and 100")
	}
	if input.Score.ResumeAtScore < input.Score.PauseBelowScore {
		return SmartRoutingConfig{}, errors.New("score resume threshold must not be lower than pause threshold")
	}
	if input.Score.PauseAfterRounds < 1 || input.Score.PauseAfterRounds > SmartRoutingMaxRounds {
		return SmartRoutingConfig{}, fmt.Errorf("score pause rounds must be between 1 and %d", SmartRoutingMaxRounds)
	}
	if input.Score.ResumeAfterRounds < SmartRoutingMinResumeRounds || input.Score.ResumeAfterRounds > SmartRoutingMaxRounds {
		return SmartRoutingConfig{}, fmt.Errorf("score resume rounds must be between %d and %d", SmartRoutingMinResumeRounds, SmartRoutingMaxRounds)
	}
	if len(input.Schedule.Windows) > SmartRoutingMaxWindows {
		return SmartRoutingConfig{}, fmt.Errorf("schedule supports at most %d windows", SmartRoutingMaxWindows)
	}
	if input.Enabled && !input.Score.Enabled && !input.Schedule.Enabled {
		return SmartRoutingConfig{}, errors.New("enabled smart routing requires at least one enabled rule")
	}
	if input.Schedule.Enabled && len(input.Schedule.Windows) == 0 {
		return SmartRoutingConfig{}, errors.New("enabled schedule requires at least one window")
	}
	normalized := input
	normalized.Schedule.Windows = make([]SmartRoutingWindow, len(input.Schedule.Windows))
	for index, window := range input.Schedule.Windows {
		if len(window.Weekdays) == 0 {
			return SmartRoutingConfig{}, fmt.Errorf("schedule window %d requires at least one weekday", index+1)
		}
		if _, ok := smartRoutingMinute(window.Start); !ok {
			return SmartRoutingConfig{}, fmt.Errorf("schedule window %d has an invalid start time", index+1)
		}
		if _, ok := smartRoutingMinute(window.End); !ok {
			return SmartRoutingConfig{}, fmt.Errorf("schedule window %d has an invalid end time", index+1)
		}
		seen := make(map[int]struct{}, len(window.Weekdays))
		weekdays := make([]int, 0, len(window.Weekdays))
		for _, weekday := range window.Weekdays {
			if weekday < 1 || weekday > 7 {
				return SmartRoutingConfig{}, fmt.Errorf("schedule window %d has an invalid weekday", index+1)
			}
			if _, duplicate := seen[weekday]; duplicate {
				continue
			}
			seen[weekday] = struct{}{}
			weekdays = append(weekdays, weekday)
		}
		sort.Ints(weekdays)
		normalized.Schedule.Windows[index] = SmartRoutingWindow{Weekdays: weekdays, Start: window.Start, End: window.End}
	}
	return normalized, nil
}

func SmartRoutingScheduleAllows(at time.Time, windows []SmartRoutingWindow) bool {
	local := at.In(smartRoutingLocation)
	minute := local.Hour()*60 + local.Minute()
	weekday := isoWeekday(local.Weekday())
	previousWeekday := weekday - 1
	if previousWeekday == 0 {
		previousWeekday = 7
	}
	for _, window := range windows {
		start, startOK := smartRoutingMinute(window.Start)
		end, endOK := smartRoutingMinute(window.End)
		if !startOK || !endOK {
			continue
		}
		if start == end && containsWeekday(window.Weekdays, weekday) {
			return true
		}
		if end > start && containsWeekday(window.Weekdays, weekday) && minute >= start && minute < end {
			return true
		}
		if end < start && (containsWeekday(window.Weekdays, weekday) && minute >= start || containsWeekday(window.Weekdays, previousWeekday) && minute < end) {
			return true
		}
	}
	return false
}

func SmartRoutingNextTransition(at time.Time, windows []SmartRoutingWindow) *time.Time {
	if len(windows) == 0 {
		return nil
	}
	local := at.In(smartRoutingLocation)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, smartRoutingLocation)
	candidates := make([]time.Time, 0, len(windows)*18)
	for offset := -1; offset <= 8; offset++ {
		windowDay := day.AddDate(0, 0, offset)
		weekday := isoWeekday(windowDay.Weekday())
		for _, window := range windows {
			if !containsWeekday(window.Weekdays, weekday) {
				continue
			}
			start, startOK := smartRoutingMinute(window.Start)
			end, endOK := smartRoutingMinute(window.End)
			if !startOK || !endOK {
				continue
			}
			startAt := windowDay.Add(time.Duration(start) * time.Minute)
			endAt := windowDay.Add(time.Duration(end) * time.Minute)
			if end == start {
				startAt = windowDay
				endAt = windowDay.AddDate(0, 0, 1)
			} else if end < start {
				endAt = endAt.AddDate(0, 0, 1)
			}
			if startAt.After(local) {
				candidates = append(candidates, startAt)
			}
			if endAt.After(local) {
				candidates = append(candidates, endAt)
			}
		}
	}
	sort.Slice(candidates, func(left, right int) bool { return candidates[left].Before(candidates[right]) })
	current := SmartRoutingScheduleAllows(local, windows)
	for _, candidate := range candidates {
		if SmartRoutingScheduleAllows(candidate, windows) != current {
			result := candidate.UTC()
			return &result
		}
	}
	return nil
}

func (policy SmartRoutingPolicy) Config() SmartRoutingConfig {
	return SmartRoutingConfig{
		Enabled: policy.Enabled,
		Score: SmartRoutingScoreConfig{
			Enabled: policy.ScoreEnabled, PauseBelowScore: policy.ScorePauseBelow,
			PauseAfterRounds: policy.ScorePauseRounds, ResumeAtScore: policy.ScoreResumeAt,
			ResumeAfterRounds: policy.ScoreResumeRounds,
		},
		Schedule: SmartRoutingScheduleConfig{Enabled: policy.ScheduleEnabled, Windows: policy.ScheduleWindows},
	}
}

func (policy SmartRoutingPolicy) BlockedBy() []string {
	if !policy.Enabled {
		return []string{}
	}
	blocked := make([]string, 0, 2)
	if policy.ScoreEnabled && policy.ScoreGate == SmartRoutingScoreBlocked {
		blocked = append(blocked, "score")
	}
	if policy.ScheduleEnabled && policy.ScheduleGate == SmartRoutingScheduleClosed {
		blocked = append(blocked, "schedule")
	}
	return blocked
}

func (s *Store) ListSmartRoutingNodeStates() ([]SmartRoutingNodeState, error) {
	nodes, err := s.ListNodes()
	if err != nil {
		return nil, err
	}
	policies, err := s.ListSmartRoutingPolicies()
	if err != nil {
		return nil, err
	}
	statuses, err := s.ListNodeMonitoringStatuses()
	if err != nil {
		return nil, err
	}
	policyByNode := make(map[string]SmartRoutingPolicy, len(policies))
	for _, policy := range policies {
		policyByNode[policy.NodeID] = policy
	}
	statusByNode := make(map[string]NodeMonitoringStatus, len(statuses))
	for _, status := range statuses {
		statusByNode[status.NodeID] = status
	}
	states := make([]SmartRoutingNodeState, 0, len(nodes))
	for _, node := range nodes {
		policy, found := policyByNode[node.ID]
		if !found {
			return nil, fmt.Errorf("smart routing policy missing for node %s", node.ID)
		}
		state := SmartRoutingNodeState{Node: node, Policy: policy}
		if status, found := statusByNode[node.ID]; found {
			score := status.Score
			state.CurrentScore = &score
			state.LastCheckedAt = status.LastCheckedAt
		}
		states = append(states, state)
	}
	return states, nil
}

func (s *Store) ListSmartRoutingPolicies() ([]SmartRoutingPolicy, error) {
	rows, err := s.db.Query(`SELECT ` + smartRoutingPolicyColumns + ` FROM node_smart_routing ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	policies := make([]SmartRoutingPolicy, 0)
	for rows.Next() {
		policy, err := scanSmartRoutingPolicy(rows)
		if err != nil {
			return nil, err
		}
		policies = append(policies, policy)
	}
	return policies, rows.Err()
}

func (s *Store) SmartRoutingPolicy(nodeID string) (SmartRoutingPolicy, error) {
	return scanSmartRoutingPolicy(s.db.QueryRow(`SELECT `+smartRoutingPolicyColumns+` FROM node_smart_routing WHERE node_id = ?`, nodeID))
}

func (s *Store) UpdateSmartRoutingPolicy(nodeID string, input SmartRoutingConfig, at time.Time, scoreFreshAfter time.Duration) (SmartRoutingUpdateOutcome, error) {
	config, err := NormalizeSmartRoutingConfig(input)
	if err != nil {
		return SmartRoutingUpdateOutcome{}, err
	}
	at = at.UTC().Round(0)
	tx, err := s.db.Begin()
	if err != nil {
		return SmartRoutingUpdateOutcome{}, err
	}
	defer tx.Rollback()
	if err := ensureSmartRoutingPolicyTx(tx, nodeID, at); err != nil {
		return SmartRoutingUpdateOutcome{}, err
	}
	current, err := scanSmartRoutingPolicy(tx.QueryRow(`SELECT `+smartRoutingPolicyColumns+` FROM node_smart_routing WHERE node_id = ?`, nodeID))
	if err != nil {
		return SmartRoutingUpdateOutcome{}, err
	}
	normalizedWindows, err := json.Marshal(config.Schedule.Windows)
	if err != nil {
		return SmartRoutingUpdateOutcome{}, err
	}
	currentWindows, err := json.Marshal(current.ScheduleWindows)
	if err != nil {
		return SmartRoutingUpdateOutcome{}, err
	}
	configChanged := current.Enabled != config.Enabled || current.ScoreEnabled != config.Score.Enabled ||
		current.ScorePauseBelow != config.Score.PauseBelowScore || current.ScorePauseRounds != config.Score.PauseAfterRounds ||
		current.ScoreResumeAt != config.Score.ResumeAtScore || current.ScoreResumeRounds != config.Score.ResumeAfterRounds ||
		current.ScheduleEnabled != config.Schedule.Enabled || string(currentWindows) != string(normalizedWindows)
	scoreConfigChanged := current.ScoreEnabled != config.Score.Enabled ||
		current.ScorePauseBelow != config.Score.PauseBelowScore || current.ScorePauseRounds != config.Score.PauseAfterRounds ||
		current.ScoreResumeAt != config.Score.ResumeAtScore || current.ScoreResumeRounds != config.Score.ResumeAfterRounds
	next := current
	next.Enabled = config.Enabled
	next.ScoreEnabled = config.Score.Enabled
	next.ScorePauseBelow = config.Score.PauseBelowScore
	next.ScorePauseRounds = config.Score.PauseAfterRounds
	next.ScoreResumeAt = config.Score.ResumeAtScore
	next.ScoreResumeRounds = config.Score.ResumeAfterRounds
	next.ScheduleEnabled = config.Schedule.Enabled
	next.ScheduleWindows = config.Schedule.Windows
	if scoreConfigChanged {
		next.ScoreLowStreak = 0
		next.ScoreRecoveryStreak = 0
		if !next.ScoreEnabled {
			next.ScoreGate = SmartRoutingScoreAllowed
		} else {
			if current.ScoreGate == SmartRoutingScoreBlocked {
				next.ScoreGate = SmartRoutingScoreBlocked
			} else {
				next.ScoreGate = SmartRoutingScoreUnknown
			}
			if next.ScoreGate != SmartRoutingScoreBlocked {
				var score int
				var checkedAt string
				if err := tx.QueryRow(`SELECT score, last_checked_at FROM node_monitoring_status WHERE node_id = ?`, nodeID).Scan(&score, &checkedAt); err == nil {
					checked, parseErr := parseTime(checkedAt)
					if parseErr != nil {
						return SmartRoutingUpdateOutcome{}, parseErr
					}
					if scoreFreshAfter <= 0 || !checked.Before(at.Add(-scoreFreshAfter)) {
						advanceSmartRoutingScore(&next, score)
					}
				} else if !errors.Is(err, sql.ErrNoRows) {
					return SmartRoutingUpdateOutcome{}, err
				}
			}
		}
	}
	if next.ScheduleEnabled && SmartRoutingScheduleAllows(at, next.ScheduleWindows) {
		next.ScheduleGate = SmartRoutingScheduleOpen
	} else if next.ScheduleEnabled {
		next.ScheduleGate = SmartRoutingScheduleClosed
	} else {
		next.ScheduleGate = SmartRoutingScheduleOpen
	}
	transition := SmartRoutingTransition{
		NodeID: nodeID, PreviousScoreGate: current.ScoreGate, ScoreGate: next.ScoreGate,
		ScoreGateChanged:     current.ScoreGate != next.ScoreGate,
		PreviousScheduleGate: current.ScheduleGate, ScheduleGate: next.ScheduleGate,
		ScheduleGateChanged: current.ScheduleGate != next.ScheduleGate,
	}
	if configChanged || transition.ScoreGateChanged || transition.ScheduleGateChanged {
		next.UpdatedAt = at
		if _, err := tx.Exec(`UPDATE node_smart_routing SET enabled = ?, score_enabled = ?, score_pause_below = ?,
			score_pause_rounds = ?, score_resume_at = ?, score_resume_rounds = ?, score_gate = ?,
			score_low_streak = ?, score_recovery_streak = ?, schedule_enabled = ?,
			schedule_windows_json = ?, schedule_gate = ?, updated_at = ? WHERE node_id = ?`,
			boolInt(next.Enabled), boolInt(next.ScoreEnabled), next.ScorePauseBelow, next.ScorePauseRounds,
			next.ScoreResumeAt, next.ScoreResumeRounds, next.ScoreGate, next.ScoreLowStreak,
			next.ScoreRecoveryStreak, boolInt(next.ScheduleEnabled), string(normalizedWindows),
			next.ScheduleGate, stamp(at), nodeID); err != nil {
			return SmartRoutingUpdateOutcome{}, err
		}
	}
	if err := reconcileSmartRoutingNodeTx(tx, &next, &transition, at); err != nil {
		return SmartRoutingUpdateOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return SmartRoutingUpdateOutcome{}, err
	}
	return SmartRoutingUpdateOutcome{Policy: next, Transition: transition, ConfigChanged: configChanged}, nil
}

func (s *Store) ReconcileSmartRouting(at time.Time) ([]SmartRoutingTransition, error) {
	at = at.UTC().Round(0)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT ` + smartRoutingPolicyColumns + ` FROM node_smart_routing ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	policies := make([]SmartRoutingPolicy, 0)
	for rows.Next() {
		policy, scanErr := scanSmartRoutingPolicy(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		policies = append(policies, policy)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	transitions := make([]SmartRoutingTransition, 0)
	for index := range policies {
		policy := &policies[index]
		previousScheduleGate := policy.ScheduleGate
		if !policy.ScheduleEnabled || SmartRoutingScheduleAllows(at, policy.ScheduleWindows) {
			policy.ScheduleGate = SmartRoutingScheduleOpen
		} else {
			policy.ScheduleGate = SmartRoutingScheduleClosed
		}
		transition := SmartRoutingTransition{
			NodeID: policy.NodeID, PreviousScoreGate: policy.ScoreGate, ScoreGate: policy.ScoreGate,
			PreviousScheduleGate: previousScheduleGate, ScheduleGate: policy.ScheduleGate,
			ScheduleGateChanged: previousScheduleGate != policy.ScheduleGate,
		}
		if transition.ScheduleGateChanged {
			policy.UpdatedAt = at
			if _, err := tx.Exec(`UPDATE node_smart_routing SET schedule_gate = ?, updated_at = ? WHERE node_id = ?`, policy.ScheduleGate, stamp(at), policy.NodeID); err != nil {
				return nil, err
			}
		}
		if err := reconcileSmartRoutingNodeTx(tx, policy, &transition, at); err != nil {
			return nil, err
		}
		if transition.ScheduleGateChanged || transition.StatusChanged || transition.AutoPausedChanged {
			transitions = append(transitions, transition)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return transitions, nil
}

func scanSmartRoutingPolicy(row scanner) (SmartRoutingPolicy, error) {
	var policy SmartRoutingPolicy
	var enabled, scoreEnabled, scheduleEnabled int
	var windowsJSON, updatedAt string
	if err := row.Scan(&policy.NodeID, &enabled, &scoreEnabled, &policy.ScorePauseBelow,
		&policy.ScorePauseRounds, &policy.ScoreResumeAt, &policy.ScoreResumeRounds,
		&policy.ScoreGate, &policy.ScoreLowStreak, &policy.ScoreRecoveryStreak,
		&scheduleEnabled, &windowsJSON, &policy.ScheduleGate, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SmartRoutingPolicy{}, ErrNotFound
		}
		return SmartRoutingPolicy{}, err
	}
	policy.Enabled = enabled != 0
	policy.ScoreEnabled = scoreEnabled != 0
	policy.ScheduleEnabled = scheduleEnabled != 0
	if err := json.Unmarshal([]byte(windowsJSON), &policy.ScheduleWindows); err != nil {
		return SmartRoutingPolicy{}, fmt.Errorf("decode smart routing schedule for node %s: %w", policy.NodeID, err)
	}
	if policy.ScheduleWindows == nil {
		policy.ScheduleWindows = []SmartRoutingWindow{}
	}
	parsed, err := parseTime(updatedAt)
	if err != nil {
		return SmartRoutingPolicy{}, err
	}
	policy.UpdatedAt = parsed
	return policy, nil
}

func insertDefaultSmartRoutingPolicyTx(tx *sql.Tx, nodeID string, createdAt time.Time) error {
	config := DefaultSmartRoutingConfig()
	_, err := tx.Exec(`INSERT INTO node_smart_routing(
		node_id, enabled, score_enabled, score_pause_below, score_pause_rounds,
		score_resume_at, score_resume_rounds, score_gate, score_low_streak,
		score_recovery_streak, schedule_enabled, schedule_windows_json,
		schedule_gate, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 0, 0, '[]', ?, ?)
	ON CONFLICT(node_id) DO NOTHING`, nodeID, boolInt(config.Enabled), boolInt(config.Score.Enabled),
		config.Score.PauseBelowScore, config.Score.PauseAfterRounds, config.Score.ResumeAtScore,
		config.Score.ResumeAfterRounds, SmartRoutingScoreUnknown, SmartRoutingScheduleOpen, stamp(createdAt))
	return err
}

func ensureSmartRoutingPolicyTx(tx *sql.Tx, nodeID string, createdAt time.Time) error {
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id = ?`, nodeID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrNotFound
	}
	return insertDefaultSmartRoutingPolicyTx(tx, nodeID, createdAt)
}

func advanceSmartRoutingScore(policy *SmartRoutingPolicy, score int) bool {
	previous := policy.ScoreGate
	switch policy.ScoreGate {
	case SmartRoutingScoreBlocked:
		policy.ScoreLowStreak = 0
		if score >= policy.ScoreResumeAt {
			policy.ScoreRecoveryStreak++
			resumeRounds := policy.ScoreResumeRounds
			if resumeRounds < SmartRoutingMinResumeRounds {
				resumeRounds = SmartRoutingMinResumeRounds
			}
			if policy.ScoreRecoveryStreak >= resumeRounds {
				policy.ScoreGate = SmartRoutingScoreAllowed
				policy.ScoreRecoveryStreak = 0
			}
		} else {
			policy.ScoreRecoveryStreak = 0
		}
	default:
		policy.ScoreRecoveryStreak = 0
		if score < policy.ScorePauseBelow {
			policy.ScoreLowStreak++
			if policy.ScoreLowStreak >= policy.ScorePauseRounds {
				policy.ScoreGate = SmartRoutingScoreBlocked
				policy.ScoreLowStreak = 0
			}
		} else {
			policy.ScoreLowStreak = 0
			policy.ScoreGate = SmartRoutingScoreAllowed
		}
	}
	return previous != policy.ScoreGate
}

func reconcileSmartRoutingNodeTx(tx *sql.Tx, policy *SmartRoutingPolicy, transition *SmartRoutingTransition, at time.Time) error {
	var status domain.NodeStatus
	var autoPaused int
	if err := tx.QueryRow(`SELECT status, monitor_auto_paused FROM nodes WHERE id = ?`, policy.NodeID).Scan(&status, &autoPaused); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	transition.PreviousStatus = status
	transition.PreviousAutoPaused = autoPaused != 0
	blockedBy := policy.BlockedBy()
	nextStatus := status
	nextAutoPaused := autoPaused != 0
	switch status {
	case domain.NodeActive:
		if len(blockedBy) > 0 {
			nextStatus = domain.NodeDraining
			nextAutoPaused = true
		} else if nextAutoPaused {
			nextAutoPaused = false
		}
	case domain.NodeDraining:
		if nextAutoPaused && len(blockedBy) == 0 {
			nextStatus = domain.NodeActive
			nextAutoPaused = false
		}
	default:
		if nextAutoPaused {
			nextAutoPaused = false
		}
	}
	transition.NodeStatus = nextStatus
	transition.AutoPaused = nextAutoPaused
	transition.StatusChanged = nextStatus != status
	transition.AutoPausedChanged = nextAutoPaused != (autoPaused != 0)
	transition.BlockedBy = blockedBy
	if transition.StatusChanged || transition.AutoPausedChanged {
		if _, err := tx.Exec(`UPDATE nodes SET status = ?, monitor_auto_paused = ?, updated_at = ? WHERE id = ?`, nextStatus, boolInt(nextAutoPaused), stamp(at), policy.NodeID); err != nil {
			return err
		}
	}
	return nil
}

func smartRoutingMinute(value string) (int, bool) {
	if len(value) != 5 || value[2] != ':' || value[0] < '0' || value[0] > '9' || value[1] < '0' || value[1] > '9' || value[3] < '0' || value[3] > '9' || value[4] < '0' || value[4] > '9' {
		return 0, false
	}
	hour := int(value[0]-'0')*10 + int(value[1]-'0')
	minute := int(value[3]-'0')*10 + int(value[4]-'0')
	if hour > 23 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

func isoWeekday(weekday time.Weekday) int {
	if weekday == time.Sunday {
		return 7
	}
	return int(weekday)
}

func containsWeekday(weekdays []int, wanted int) bool {
	for _, weekday := range weekdays {
		if weekday == wanted {
			return true
		}
	}
	return false
}
