package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestUpgradeRolloutCanaryConcurrencyPersistenceAndRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	rollout := domain.NodeUpgradeRollout{MaxParallel: 2, HealthWindowSeconds: 30, TargetAgentSHA256: strings.Repeat("2", 64), TargetNginxSHA256: strings.Repeat("3", 64)}
	instructions := map[string]domain.NodeUpgradeInstruction{}
	for i := 0; i < 4; i++ {
		node, err := database.CreateNode(fmt.Sprintf("edge-%d", i), fmt.Sprintf("203.0.113.%d", 100+i))
		if err != nil {
			t.Fatal(err)
		}
		if err = database.HeartbeatWithAgent(node.ID, 0, "", nil, strings.Repeat("1", 64), ""); err != nil {
			t.Fatal(err)
		}
		rollout.Members = append(rollout.Members, domain.NodeUpgradeRolloutMember{NodeID: node.ID, Name: node.Name})
		instruction := testUpgradeInstruction(rollout.TargetAgentSHA256)
		instruction.NginxBundle = &domain.UpgradeArtifact{URL: "https://control.test/nginx", SHA256: rollout.TargetNginxSHA256}
		instructions[node.ID] = instruction
	}
	rollout, err = database.CreateUpgradeRollout(rollout, instructions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.CreateUpgradeRollout(rollout, instructions); !errors.Is(err, ErrUpgradeRolloutActive) {
		t.Fatalf("second rollout: %v", err)
	}
	if _, _, err = database.CreateOrGetNodeUpgrade(rollout.Members[1].NodeID, instructions[rollout.Members[1].NodeID], time.Now().Add(time.Hour)); !errors.Is(err, ErrUpgradeRolloutActive) {
		t.Fatalf("individual bypass: %v", err)
	}
	pins, err := database.ReferencedUpgradeNginxSHA256s()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pins[rollout.TargetNginxSHA256]; !ok {
		t.Fatal("pending artifact not pinned")
	}
	if _, err = database.DispatchUpgradeRolloutNode(rollout.ID, rollout.Members[1].NodeID, time.Now().Add(time.Hour)); !errors.Is(err, ErrUpgradeRolloutConflict) {
		t.Fatalf("canary bypass: %v", err)
	}
	rollout, err = database.DispatchUpgradeRolloutNode(rollout.ID, rollout.Members[0].NodeID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	canaryID := rollout.Members[0].TaskID
	if _, err = database.DispatchUpgradeRolloutNode(rollout.ID, rollout.Members[0].NodeID, time.Now().Add(time.Hour)); !errors.Is(err, ErrUpgradeRolloutConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	rollout, err = database.LatestUpgradeRollout()
	if err != nil || rollout.Members[0].TaskID != canaryID {
		t.Fatalf("restart: %#v %v", rollout, err)
	}
	stale := rollout
	rollout.Members[0].State = "succeeded"
	if err = database.SaveUpgradeRollout(&rollout); err != nil {
		t.Fatal(err)
	}
	if err = database.SaveUpgradeRollout(&stale); !errors.Is(err, ErrUpgradeRolloutConflict) {
		t.Fatalf("stale write: %v", err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 3)
	for _, member := range rollout.Members[1:] {
		wg.Add(1)
		go func(nodeID string) {
			defer wg.Done()
			_, e := database.DispatchUpgradeRolloutNode(rollout.ID, nodeID, time.Now().Add(time.Hour))
			results <- e
		}(member.NodeID)
	}
	wg.Wait()
	close(results)
	created := 0
	for err := range results {
		if err == nil {
			created++
		} else if !errors.Is(err, ErrUpgradeRolloutConflict) {
			t.Fatal(err)
		}
	}
	if created != 2 {
		t.Fatalf("parallel cap: dispatched %d", created)
	}
	rollout, err = database.ChangeUpgradeRollout(rollout.ID, "pause")
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range rollout.Members {
		if member.State == "pending" {
			if _, err = database.DispatchUpgradeRolloutNode(rollout.ID, member.NodeID, time.Now().Add(time.Hour)); !errors.Is(err, ErrUpgradeRolloutConflict) {
				t.Fatalf("pause bypass: %v", err)
			}
		}
	}
	// A failed installer is retried as a new task after explicit resume.
	for i := range rollout.Members {
		member := &rollout.Members[i]
		if member.State == "upgrading" {
			if _, err = database.RecordNodeUpgradeReport(member.NodeID, domain.NodeUpgradeReport{TaskID: member.TaskID, Status: domain.NodeUpgradeFailed, Detail: "rolled back"}); err != nil {
				t.Fatal(err)
			}
			member.State = "failed"
			break
		}
	}
	if err = database.SaveUpgradeRollout(&rollout); err != nil {
		t.Fatal(err)
	}
	rollout, err = database.ChangeUpgradeRollout(rollout.ID, "resume")
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range rollout.Members {
		if member.State == "pending" && member.TaskID != "" {
			next, e := database.DispatchUpgradeRolloutNode(rollout.ID, member.NodeID, time.Now().Add(time.Hour))
			if e != nil {
				t.Fatal(e)
			}
			for _, m := range next.Members {
				if m.NodeID == member.NodeID && m.TaskID == member.TaskID {
					t.Fatal("failed task reused")
				}
			}
		}
	}
	rollout, err = database.ChangeUpgradeRollout(rollout.ID, "cancel")
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range rollout.Members {
		if member.State == "pending" {
			t.Fatal("pending work survived cancel")
		}
	}
	reserved, err := database.NodeReservedByUpgradeRollout(rollout.Members[0].NodeID)
	if err != nil || reserved {
		t.Fatalf("cancel reservation: %v %v", reserved, err)
	}
}

func TestUpgradeRolloutDispatchRollsBackTaskOnSaveFailure(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, _ := database.CreateNode("atomic", "203.0.113.110")
	instruction := testUpgradeInstruction(strings.Repeat("2", 64))
	rollout, err := database.CreateUpgradeRollout(domain.NodeUpgradeRollout{MaxParallel: 1, HealthWindowSeconds: 30, TargetAgentSHA256: instruction.Binary.SHA256, Members: []domain.NodeUpgradeRolloutMember{{NodeID: node.ID}}}, map[string]domain.NodeUpgradeInstruction{node.ID: instruction})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.db.Exec(`CREATE TRIGGER reject_rollout_save BEFORE UPDATE ON node_upgrade_rollouts BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = database.DispatchUpgradeRolloutNode(rollout.ID, node.ID, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("injected save unexpectedly succeeded")
	}
	if _, err = database.LatestNodeUpgrade(node.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan task: %v", err)
	}
}
