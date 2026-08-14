package store

import (
	"path/filepath"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestSmartRoutingScheduleSupportsWeeklyAndCrossMidnightWindows(t *testing.T) {
	windows := []SmartRoutingWindow{
		{Weekdays: []int{1}, Start: "09:00", End: "18:00"},
		{Weekdays: []int{5}, Start: "23:00", End: "02:00"},
		{Weekdays: []int{7}, Start: "00:00", End: "00:00"},
	}
	for _, test := range []struct {
		name    string
		at      string
		allowed bool
	}{
		{name: "monday start inclusive", at: "2026-07-27T09:00:00+08:00", allowed: true},
		{name: "monday end exclusive", at: "2026-07-27T18:00:00+08:00"},
		{name: "friday before midnight", at: "2026-07-31T23:30:00+08:00", allowed: true},
		{name: "saturday carry over", at: "2026-08-01T01:59:00+08:00", allowed: true},
		{name: "saturday after carry over", at: "2026-08-01T02:00:00+08:00"},
		{name: "equal times mean full sunday", at: "2026-08-02T14:30:00+08:00", allowed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, test.at)
			if err != nil {
				t.Fatal(err)
			}
			if allowed := SmartRoutingScheduleAllows(at, windows); allowed != test.allowed {
				t.Fatalf("allowed = %v, want %v", allowed, test.allowed)
			}
		})
	}

	at, _ := time.Parse(time.RFC3339, "2026-07-27T08:00:00+08:00")
	next := SmartRoutingNextTransition(at, windows)
	if next == nil || next.In(smartRoutingLocation).Format(time.RFC3339) != "2026-07-27T09:00:00+08:00" {
		t.Fatalf("next transition = %v", next)
	}
}

func TestNormalizeSmartRoutingConfigValidatesThresholdsAndWindows(t *testing.T) {
	config := DefaultSmartRoutingConfig()
	if config.Score.ResumeAfterRounds != SmartRoutingMinResumeRounds {
		t.Fatalf("default resume rounds = %d, want %d", config.Score.ResumeAfterRounds, SmartRoutingMinResumeRounds)
	}
	config.Score.ResumeAfterRounds = SmartRoutingMinResumeRounds - 1
	if _, err := NormalizeSmartRoutingConfig(config); err == nil {
		t.Fatal("resume rounds below the minimum were accepted")
	}
	config = DefaultSmartRoutingConfig()
	config.Score.ResumeAtScore = config.Score.PauseBelowScore - 1
	if _, err := NormalizeSmartRoutingConfig(config); err == nil {
		t.Fatal("resume threshold below pause threshold was accepted")
	}
	config = DefaultSmartRoutingConfig()
	config.Schedule.Enabled = true
	if _, err := NormalizeSmartRoutingConfig(config); err == nil {
		t.Fatal("enabled empty schedule was accepted")
	}
	config.Schedule.Windows = []SmartRoutingWindow{{Weekdays: []int{5, 1, 5}, Start: "22:00", End: "03:00"}}
	normalized, err := NormalizeSmartRoutingConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	weekdays := normalized.Schedule.Windows[0].Weekdays
	if len(weekdays) != 2 || weekdays[0] != 1 || weekdays[1] != 5 {
		t.Fatalf("normalized weekdays = %#v", weekdays)
	}
}

func TestSmartRoutingRecoveryEnforcesMinimumRounds(t *testing.T) {
	policy := SmartRoutingPolicy{
		ScoreGate: SmartRoutingScoreBlocked, ScoreResumeAt: SmartRoutingDefaultScore,
		ScoreResumeRounds: 1,
	}
	for round := 1; round <= SmartRoutingMinResumeRounds; round++ {
		changed := advanceSmartRoutingScore(&policy, SmartRoutingDefaultScore)
		if round < SmartRoutingMinResumeRounds && (changed || policy.ScoreGate != SmartRoutingScoreBlocked) {
			t.Fatalf("recovery round %d changed policy = %#v", round, policy)
		}
	}
	if policy.ScoreGate != SmartRoutingScoreAllowed || policy.ScoreRecoveryStreak != 0 {
		t.Fatalf("final recovery policy = %#v", policy)
	}
}

func TestSmartRoutingUsesPerNodeHysteresisAndConsecutiveRounds(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-smart", "203.0.113.121")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	target, err := database.CreateMonitoringTarget("智能路由探针", "probe-smart.example.test:443")
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultSmartRoutingConfig()
	config.Score.PauseBelowScore = 70
	config.Score.PauseAfterRounds = 2
	config.Score.ResumeAtScore = 90
	config.Score.ResumeAfterRounds = SmartRoutingMinResumeRounds
	if _, err := database.UpdateSmartRoutingPolicy(node.ID, config, time.Now().UTC(), monitoringStaleAfterForTest); err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Now().UTC().Add(time.Second)
	first, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{failedMonitoringResult(target.ID, checkedAt)})
	if err != nil {
		t.Fatal(err)
	}
	if first.NodeStatus != domain.NodeActive || first.Transition.ScoreGate == SmartRoutingScoreBlocked {
		t.Fatalf("first low round = %#v", first)
	}
	config.Schedule.Windows = []SmartRoutingWindow{{Weekdays: []int{1}, Start: "09:00", End: "10:00"}}
	updated, err := database.UpdateSmartRoutingPolicy(node.ID, config, time.Now().UTC(), monitoringStaleAfterForTest)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Policy.ScoreLowStreak != 1 || updated.Policy.ScoreGate != SmartRoutingScoreUnknown {
		t.Fatalf("schedule-only edit reset score state: %#v", updated.Policy)
	}
	second, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{failedMonitoringResult(target.ID, checkedAt.Add(time.Second))})
	if err != nil {
		t.Fatal(err)
	}
	if second.NodeStatus != domain.NodeDraining || !second.Transition.ScoreGateChanged || second.Transition.ScoreGate != SmartRoutingScoreBlocked {
		t.Fatalf("second low round = %#v", second)
	}
	config.Enabled = false
	disabled, err := database.UpdateSmartRoutingPolicy(node.ID, config, time.Now().UTC(), monitoringStaleAfterForTest)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Policy.ScoreGate != SmartRoutingScoreBlocked || disabled.Transition.NodeStatus != domain.NodeActive {
		t.Fatalf("disabled smart routing did not retain score decision: %#v", disabled)
	}
	healthyResult := func(at time.Time) domain.MonitoringProbeResult {
		return domain.MonitoringProbeResult{TargetID: target.ID, Attempts: 3, SuccessfulAttempts: 3, AverageLatencyMS: 20, CheckedAt: at}
	}
	whileDisabled, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{healthyResult(checkedAt.Add(2 * time.Second))})
	if err != nil {
		t.Fatal(err)
	}
	if whileDisabled.NodeStatus != domain.NodeActive || whileDisabled.Transition.ScoreGate != SmartRoutingScoreBlocked {
		t.Fatalf("healthy round while disabled = %#v", whileDisabled)
	}
	policy, err := database.SmartRoutingPolicy(node.ID)
	if err != nil || policy.ScoreRecoveryStreak != 1 {
		t.Fatalf("healthy round while disabled did not count recovery: policy = %#v, %v", policy, err)
	}
	config.Enabled = true
	reenabled, err := database.UpdateSmartRoutingPolicy(node.ID, config, time.Now().UTC(), monitoringStaleAfterForTest)
	if err != nil {
		t.Fatal(err)
	}
	if reenabled.Policy.ScoreGate != SmartRoutingScoreBlocked || reenabled.Policy.ScoreRecoveryStreak != 1 || reenabled.Transition.NodeStatus != domain.NodeDraining {
		t.Fatalf("smart routing handoff did not restore score blocker or dropped recovery rounds: %#v", reenabled)
	}
	firstRecovery, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{healthyResult(checkedAt.Add(3 * time.Second))})
	if err != nil {
		t.Fatal(err)
	}
	if firstRecovery.NodeStatus != domain.NodeDraining || firstRecovery.Transition.ScoreGateChanged {
		t.Fatalf("first recovery round = %#v", firstRecovery)
	}
	reset, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{failedMonitoringResult(target.ID, checkedAt.Add(4*time.Second))})
	if err != nil {
		t.Fatal(err)
	}
	policy, err = database.SmartRoutingPolicy(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reset.NodeStatus != domain.NodeDraining || policy.ScoreRecoveryStreak != 0 {
		t.Fatalf("interrupted recovery = %#v, policy = %#v", reset, policy)
	}
	for round := 1; round <= SmartRoutingMinResumeRounds; round++ {
		outcome, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{
			healthyResult(checkedAt.Add(time.Duration(4+round) * time.Second)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if round < SmartRoutingMinResumeRounds {
			if outcome.NodeStatus != domain.NodeDraining || outcome.Transition.ScoreGateChanged {
				t.Fatalf("recovery round %d = %#v", round, outcome)
			}
			continue
		}
		if outcome.NodeStatus != domain.NodeActive || !outcome.Transition.ScoreGateChanged || outcome.Transition.ScoreGate != SmartRoutingScoreAllowed {
			t.Fatalf("final recovery round = %#v", outcome)
		}
	}
}

func TestSmartRoutingManualTakeoverKeepsScoreGateRefreshing(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-takeover-refresh", "203.0.113.132")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	target, err := database.CreateMonitoringTarget("接管刷新探针", "probe-takeover.example.test:443")
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultSmartRoutingConfig()
	config.Score.PauseBelowScore = 70
	config.Score.PauseAfterRounds = 2
	config.Score.ResumeAtScore = 90
	config.Score.ResumeAfterRounds = SmartRoutingMinResumeRounds
	if _, err := database.UpdateSmartRoutingPolicy(node.ID, config, time.Now().UTC(), monitoringStaleAfterForTest); err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Now().UTC().Add(time.Second)
	for round := 1; round <= 2; round++ {
		if _, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{
			failedMonitoringResult(target.ID, checkedAt.Add(time.Duration(round)*time.Second)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	paused, err := database.GetNode(node.ID)
	if err != nil || paused.Status != domain.NodeDraining || !paused.MonitorAutoPaused {
		t.Fatalf("low rounds did not auto-pause node: %#v, %v", paused, err)
	}
	takenOver, err := database.SetNodeStatusManual(node.ID, domain.NodeActive)
	if err != nil || !takenOver {
		t.Fatalf("manual takeover = %t, %v", takenOver, err)
	}
	healthyResult := func(at time.Time) domain.MonitoringProbeResult {
		return domain.MonitoringProbeResult{TargetID: target.ID, Attempts: 3, SuccessfulAttempts: 3, AverageLatencyMS: 20, CheckedAt: at}
	}
	for round := 1; round <= SmartRoutingMinResumeRounds; round++ {
		outcome, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{
			healthyResult(checkedAt.Add(time.Duration(2+round) * time.Second)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if outcome.NodeStatus != domain.NodeActive || outcome.AutoPaused {
			t.Fatalf("manual takeover round %d changed node status: %#v", round, outcome)
		}
		if round < SmartRoutingMinResumeRounds {
			if outcome.Transition.ScoreGate != SmartRoutingScoreBlocked || outcome.Transition.ScoreGateChanged {
				t.Fatalf("takeover recovery round %d = %#v", round, outcome)
			}
			continue
		}
		if outcome.Transition.ScoreGate != SmartRoutingScoreAllowed || !outcome.Transition.ScoreGateChanged {
			t.Fatalf("takeover recovery final round = %#v", outcome)
		}
	}
	policy, err := database.SmartRoutingPolicy(node.ID)
	if err != nil || policy.ScoreGate != SmartRoutingScoreAllowed || policy.ScoreRecoveryStreak != 0 || policy.Enabled {
		t.Fatalf("gate refresh during takeover = %#v, %v", policy, err)
	}
	config.Enabled = true
	reenabled, err := database.UpdateSmartRoutingPolicy(node.ID, config, time.Now().UTC(), monitoringStaleAfterForTest)
	if err != nil {
		t.Fatal(err)
	}
	if reenabled.Policy.ScoreGate != SmartRoutingScoreAllowed || reenabled.Transition.NodeStatus != domain.NodeActive || reenabled.Transition.StatusChanged || reenabled.Transition.AutoPaused {
		t.Fatalf("reenable after recovery re-drained node: %#v", reenabled)
	}
	fresh, err := database.GetNode(node.ID)
	if err != nil || fresh.Status != domain.NodeActive || fresh.MonitorAutoPaused {
		t.Fatalf("node state after reenable = %#v, %v", fresh, err)
	}
	firstLow, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{
		failedMonitoringResult(target.ID, checkedAt.Add(6 * time.Second)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if firstLow.Transition.ScoreGate != SmartRoutingScoreAllowed || firstLow.Transition.ScoreGateChanged || firstLow.NodeStatus != domain.NodeActive {
		t.Fatalf("first low round after handover = %#v", firstLow)
	}
	secondLow, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{
		failedMonitoringResult(target.ID, checkedAt.Add(7 * time.Second)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if secondLow.Transition.ScoreGate != SmartRoutingScoreBlocked || !secondLow.Transition.ScoreGateChanged || secondLow.NodeStatus != domain.NodeDraining || !secondLow.AutoPaused {
		t.Fatalf("genuine fault after handover = %#v", secondLow)
	}
}

func TestSmartRoutingScheduleAndScoreAreCombinedWithAND(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, _ := database.CreateNode("edge-and", "203.0.113.122")
	if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	config := DefaultSmartRoutingConfig()
	config.Schedule.Enabled = true
	config.Schedule.Windows = []SmartRoutingWindow{{Weekdays: []int{1}, Start: "09:00", End: "10:00"}}
	closedAt, _ := time.Parse(time.RFC3339, "2026-07-27T10:00:00+08:00")
	outcome, err := database.UpdateSmartRoutingPolicy(node.ID, config, closedAt, monitoringStaleAfterForTest)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Transition.NodeStatus != domain.NodeDraining || len(outcome.Transition.BlockedBy) != 1 || outcome.Transition.BlockedBy[0] != "schedule" {
		t.Fatalf("closed schedule outcome = %#v", outcome)
	}
	openAt, _ := time.Parse(time.RFC3339, "2026-08-03T09:00:00+08:00")
	transitions, err := database.ReconcileSmartRouting(openAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(transitions) != 1 || transitions[0].NodeStatus != domain.NodeActive {
		t.Fatalf("open schedule transitions = %#v", transitions)
	}
}

func TestManualNodeStatusDisablesSmartRoutingButRetainsRules(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, _ := database.CreateNode("edge-owner", "203.0.113.123")
	disabled, err := database.SetNodeStatusManual(node.ID, domain.NodeDraining)
	if err != nil {
		t.Fatal(err)
	}
	if !disabled {
		t.Fatal("manual status did not report smart routing disabled")
	}
	policy, err := database.SmartRoutingPolicy(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Enabled || !policy.ScoreEnabled || policy.ScorePauseBelow != SmartRoutingDefaultScore {
		t.Fatalf("manual policy = %#v", policy)
	}
}

const monitoringStaleAfterForTest = 75 * time.Second
