package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestMonitoringRoundAutoPausesAndRecoversNode(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-monitor", "203.0.113.91")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	target, err := database.CreateMonitoringTarget("主探针", "probe.example.test:443")
	if err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Now().UTC().Add(-time.Minute)
	for round := 1; round <= MonitoringAutoPauseAfter; round++ {
		result := failedMonitoringResult(target.ID, checkedAt.Add(time.Duration(round)*time.Second))
		outcome, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{result})
		if err != nil {
			t.Fatal(err)
		}
		wantStatus := domain.NodeActive
		if round == MonitoringAutoPauseAfter {
			wantStatus = domain.NodeDraining
		}
		if outcome.NodeStatus != wantStatus || outcome.Status.ConsecutiveAbnormal != round {
			t.Fatalf("round %d outcome = %#v", round, outcome)
		}
	}
	paused, err := database.GetNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != domain.NodeDraining || !paused.MonitorAutoPaused {
		t.Fatalf("paused node = %#v", paused)
	}
	healthy := domain.MonitoringProbeResult{
		TargetID: target.ID, Attempts: 3, SuccessfulAttempts: 3, AverageLatencyMS: 20,
	}
	for round := 1; round <= SmartRoutingMinResumeRounds; round++ {
		healthy.CheckedAt = checkedAt.Add(time.Duration(MonitoringAutoPauseAfter+round) * time.Second)
		outcome, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{healthy})
		if err != nil {
			t.Fatal(err)
		}
		wantStatus := domain.NodeDraining
		wantChanged := false
		if round == SmartRoutingMinResumeRounds {
			wantStatus = domain.NodeActive
			wantChanged = true
		}
		if outcome.NodeStatus != wantStatus || outcome.StatusChanged != wantChanged || outcome.Status.Score != 100 || outcome.Status.ConsecutiveAbnormal != 0 {
			t.Fatalf("recovery round %d outcome = %#v", round, outcome)
		}
	}
	recovered, err := database.GetNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != domain.NodeActive || recovered.MonitorAutoPaused {
		t.Fatalf("recovered node = %#v", recovered)
	}
}

func TestMonitoringNeverResumesManuallyPausedNode(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-manual-pause", "203.0.113.92")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeDraining); err != nil {
		t.Fatal(err)
	}
	target, err := database.CreateMonitoringTarget("人工暂停探针", "192.0.2.20:8443")
	if err != nil {
		t.Fatal(err)
	}
	result := domain.MonitoringProbeResult{
		TargetID: target.ID, Attempts: 3, SuccessfulAttempts: 3, AverageLatencyMS: 15, CheckedAt: time.Now().UTC(),
	}
	if _, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{result}); err != nil {
		t.Fatal(err)
	}
	loaded, err := database.GetNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != domain.NodeDraining || loaded.MonitorAutoPaused {
		t.Fatalf("manual pause was changed: %#v", loaded)
	}
}

func TestManualPauseTakesOwnershipAndMissingScoreRetainsAutoPause(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-pause-owner", "203.0.113.93")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	target, err := database.CreateMonitoringTarget("探针 A", "probe.example.test:80")
	if err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Now().UTC().Add(-time.Minute)
	for round := 0; round < MonitoringAutoPauseAfter; round++ {
		if _, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{failedMonitoringResult(target.ID, checkedAt.Add(time.Duration(round)*time.Second))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeDraining); err != nil {
		t.Fatal(err)
	}
	manual, err := database.GetNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if manual.MonitorAutoPaused {
		t.Fatal("manual pause retained automatic ownership")
	}
	if err := database.SetNodeStatus(node.ID, domain.NodeActive); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < MonitoringAutoPauseAfter; round++ {
		if _, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{failedMonitoringResult(target.ID, checkedAt.Add(time.Duration(10+round)*time.Second))}); err != nil {
			t.Fatal(err)
		}
	}
	secondTarget, err := database.CreateMonitoringTarget("探针 B", "probe-b.example.test:80")
	if err != nil {
		t.Fatal(err)
	}
	stillPaused, err := database.GetNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillPaused.Status != domain.NodeDraining || !stillPaused.MonitorAutoPaused {
		t.Fatalf("target change released pause before a healthy report: %#v", stillPaused)
	}
	if err := database.DeleteMonitoringTarget(target.ID); err != nil {
		t.Fatal(err)
	}
	stillPaused, err = database.GetNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillPaused.Status != domain.NodeDraining || !stillPaused.MonitorAutoPaused {
		t.Fatalf("remaining target did not retain automatic pause: %#v", stillPaused)
	}
	if err := database.DeleteMonitoringTarget(secondTarget.ID); err != nil {
		t.Fatal(err)
	}
	retained, err := database.GetNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Status != domain.NodeDraining || !retained.MonitorAutoPaused {
		t.Fatalf("missing score did not retain automatic pause: %#v", retained)
	}
	statuses, err := database.ListNodeMonitoringStatuses()
	if err != nil || len(statuses) != 0 {
		t.Fatalf("monitoring status after target reset = %#v, %v", statuses, err)
	}
}

func TestMonitoringRejectsChangedTargetsAndDuplicateReports(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, _ := database.CreateNode("edge-stale-report", "203.0.113.94")
	target, _ := database.CreateMonitoringTarget("重复报告探针", "probe.example.test:443")
	result := failedMonitoringResult(target.ID, time.Now().UTC())
	if _, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{result}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{result}); !errors.Is(err, ErrMonitoringReportStale) {
		t.Fatalf("duplicate report error = %v", err)
	}
	if _, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{{TargetID: "unknown", Attempts: 1, CheckedAt: time.Now().UTC()}}); !errors.Is(err, ErrMonitoringTargetsChanged) {
		t.Fatalf("changed target error = %v", err)
	}
}

func TestMonitoringTargetRenamePreservesCurrentStateAndRequiresUniqueName(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, _ := database.CreateNode("edge-rename", "203.0.113.95")
	target, _ := database.CreateMonitoringTarget("旧名称", "probe.example.test:443")
	backup, err := database.CreateMonitoringTarget("备用探针", "probe-b.example.test:443")
	if err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Now().UTC()
	results := []domain.MonitoringProbeResult{
		{TargetID: target.ID, Attempts: 3, SuccessfulAttempts: 3, AverageLatencyMS: 18, CheckedAt: checkedAt},
		{TargetID: backup.ID, Attempts: 3, SuccessfulAttempts: 3, AverageLatencyMS: 21, CheckedAt: checkedAt},
	}
	if _, err := database.RecordMonitoringRound(node.ID, results); err != nil {
		t.Fatal(err)
	}
	newName := "香港主探针"
	renamed, err := database.UpdateMonitoringTarget(target.ID, &newName, nil)
	if err != nil || renamed.Name != newName {
		t.Fatalf("renamed target = %#v, %v", renamed, err)
	}
	statuses, statusErr := database.ListNodeMonitoringStatuses()
	snapshots, snapshotErr := database.ListMonitoringProbeSnapshots()
	if statusErr != nil || snapshotErr != nil || len(statuses) != 1 || len(snapshots) != 2 {
		t.Fatalf("rename reset monitoring state: statuses=%#v snapshots=%#v errors=%v/%v", statuses, snapshots, statusErr, snapshotErr)
	}
	duplicate := "备用探针"
	if _, err := database.UpdateMonitoringTarget(target.ID, &duplicate, nil); !errors.Is(err, ErrMonitoringTargetNameExists) {
		t.Fatalf("duplicate name error = %v", err)
	}
}

func TestMonitoringTargetSetChangeResetsPendingSmartRoutingStreak(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, _ := database.CreateNode("edge-target-reset", "203.0.113.129")
	target, _ := database.CreateMonitoringTarget("主探针", "probe-reset.example.test:443")
	if _, err := database.RecordMonitoringRound(node.ID, []domain.MonitoringProbeResult{failedMonitoringResult(target.ID, time.Now().UTC())}); err != nil {
		t.Fatal(err)
	}
	before, err := database.SmartRoutingPolicy(node.ID)
	if err != nil || before.ScoreLowStreak != 1 || before.ScoreGate != SmartRoutingScoreUnknown {
		t.Fatalf("policy before target change = %#v, %v", before, err)
	}
	if _, err := database.CreateMonitoringTarget("备用探针", "probe-reset-b.example.test:443"); err != nil {
		t.Fatal(err)
	}
	after, err := database.SmartRoutingPolicy(node.ID)
	if err != nil || after.ScoreLowStreak != 0 || after.ScoreGate != SmartRoutingScoreUnknown {
		t.Fatalf("policy after target change = %#v, %v", after, err)
	}
}

func TestMonitoringScoreCombinesSuccessRateAndLatency(t *testing.T) {
	checkedAt := time.Now().UTC()
	for _, test := range []struct {
		name      string
		result    domain.MonitoringProbeResult
		wanted    int
		isHealthy bool
	}{
		{
			name: "healthy low latency", result: domain.MonitoringProbeResult{
				TargetID: "target", Attempts: 10, SuccessfulAttempts: 9, AverageLatencyMS: 100, CheckedAt: checkedAt,
			}, wanted: 93, isHealthy: true,
		},
		{
			name: "partial reachability", result: domain.MonitoringProbeResult{
				TargetID: "target", Attempts: 3, SuccessfulAttempts: 2, AverageLatencyMS: 50, CheckedAt: checkedAt,
			}, wanted: 77,
		},
		{
			name: "high latency", result: domain.MonitoringProbeResult{
				TargetID: "target", Attempts: 3, SuccessfulAttempts: 3, AverageLatencyMS: 1000, CheckedAt: checkedAt,
			}, wanted: 70,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := scoreMonitoringRound("node", []domain.MonitoringProbeResult{test.result})
			if status.Score != test.wanted || (status.Score >= MonitoringHealthyScore) != test.isHealthy {
				t.Fatalf("score = %d, healthy = %v; want %d, %v", status.Score, status.Score >= MonitoringHealthyScore, test.wanted, test.isHealthy)
			}
		})
	}
}

func failedMonitoringResult(targetID string, checkedAt time.Time) domain.MonitoringProbeResult {
	return domain.MonitoringProbeResult{TargetID: targetID, Attempts: 3, Error: "connection refused", CheckedAt: checkedAt}
}
