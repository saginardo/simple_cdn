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
	config.Score.ResumeAfterRounds = 2
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
	config.Enabled = true
	reenabled, err := database.UpdateSmartRoutingPolicy(node.ID, config, time.Now().UTC(), monitoringStaleAfterForTest)
	if err != nil {
		t.Fatal(err)
	}
	if reenabled.Policy.ScoreGate != SmartRoutingScoreBlocked || reenabled.Transition.NodeStatus != domain.NodeDraining {
		t.Fatalf("smart routing handoff did not restore score blocker: %#v", reenabled)
	}
	healthyResult := func(at time.Time) domain.MonitoringProbeResult {
		return domain.MonitoringProbeResult{TargetID: target.ID, Attempts: 3, SuccessfulAttempts: 3, AverageLatencyMS: 20, CheckedAt: at}
	}
	third, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{healthyResult(checkedAt.Add(2 * time.Second))})
	if err != nil {
		t.Fatal(err)
	}
	if third.NodeStatus != domain.NodeDraining || third.Transition.ScoreGateChanged {
		t.Fatalf("first recovery round = %#v", third)
	}
	fourth, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{healthyResult(checkedAt.Add(3 * time.Second))})
	if err != nil {
		t.Fatal(err)
	}
	if fourth.NodeStatus != domain.NodeActive || !fourth.Transition.ScoreGateChanged || fourth.Transition.ScoreGate != SmartRoutingScoreAllowed {
		t.Fatalf("second recovery round = %#v", fourth)
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
