package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

func TestNodeUpgradeTaskLifecycleAndRetryGuard(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-upgrade", "203.0.113.90")
	if err != nil {
		t.Fatal(err)
	}
	sourceDigest := strings.Repeat("1", 64)
	targetDigest := strings.Repeat("2", 64)
	if err := database.HeartbeatWithAgent(node.ID, 0, "", nil, sourceDigest, ""); err != nil {
		t.Fatal(err)
	}
	instruction := testUpgradeInstruction(targetDigest)
	task, created, err := database.CreateOrGetNodeUpgrade(node.ID, instruction, time.Now().Add(time.Hour))
	if err != nil || !created || task.SourceSHA256 != sourceDigest || task.TargetSHA256 != targetDigest {
		t.Fatalf("create task = %#v, created=%v, err=%v", task, created, err)
	}
	duplicate, created, err := database.CreateOrGetNodeUpgrade(node.ID, instruction, time.Now().Add(time.Hour))
	if err != nil || created || duplicate.ID != task.ID {
		t.Fatalf("duplicate task = %#v, created=%v, err=%v", duplicate, created, err)
	}
	site, err := database.CreateSite(domain.Site{
		Name: "upgrade-guard", Domains: []string{"guard.example.test"}, Nodes: []string{node.ID},
		PrimaryOrigin: domain.Origin{URL: "https://origin.example.test", Enabled: true}, Enabled: false,
	}, "zone")
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.CommitSitePublication(site.ID, site.ConfigVersion, "", []NodeStateUpdate{{
		NodeID: node.ID, State: domain.DesiredState{Version: 1, NginxConfig: "events {}"},
	}}, nil)
	if !errors.Is(err, ErrNodeUpgradeActive) {
		t.Fatalf("publication during node upgrade = %v", err)
	}
	applying, err := database.RecordNodeUpgradeReport(node.ID, domain.NodeUpgradeReport{TaskID: task.ID, Status: domain.NodeUpgradeApplying, Detail: "verified"})
	if err != nil || applying.Status != domain.NodeUpgradeApplying || applying.StartedAt == nil {
		t.Fatalf("applying task = %#v, err=%v", applying, err)
	}
	if _, err := database.RecordNodeUpgradeReport(node.ID, domain.NodeUpgradeReport{TaskID: task.ID, Status: domain.NodeUpgradeSucceeded, InstalledSHA256: sourceDigest}); err == nil {
		t.Fatal("accepted a success report with the wrong installed digest")
	}
	failed, err := database.RecordNodeUpgradeReport(node.ID, domain.NodeUpgradeReport{TaskID: task.ID, Status: domain.NodeUpgradeFailed, ErrorCode: "installer_failed", Detail: "rolled back"})
	if err != nil || failed.Status != domain.NodeUpgradeFailed || failed.CompletedAt == nil {
		t.Fatalf("failed task = %#v, err=%v", failed, err)
	}
	if err := database.HeartbeatWithAgent(node.ID, 0, "", nil, sourceDigest, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := database.CreateOrGetNodeUpgrade(node.ID, instruction, time.Now().Add(time.Hour)); !errors.Is(err, ErrUpgradeRetryNotReady) {
		t.Fatalf("retry while local task active = %v", err)
	}
	if err := database.HeartbeatWithAgent(node.ID, 0, "", nil, sourceDigest, ""); err != nil {
		t.Fatal(err)
	}
	retry, created, err := database.CreateOrGetNodeUpgrade(node.ID, instruction, time.Now().Add(time.Hour))
	if err != nil || !created || retry.ID == task.ID {
		t.Fatalf("retry task = %#v, created=%v, err=%v", retry, created, err)
	}
}

func TestNodeUpgradeRejectsCrossNodeReportAndReconcilesTimeout(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	first, _ := database.CreateNode("edge-first", "203.0.113.91")
	second, _ := database.CreateNode("edge-second", "203.0.113.92")
	digest := strings.Repeat("3", 64)
	for _, node := range []domain.Node{first, second} {
		if err := database.HeartbeatWithAgent(node.ID, 0, "", nil, strings.Repeat("4", 64), ""); err != nil {
			t.Fatal(err)
		}
	}
	task, _, err := database.CreateOrGetNodeUpgrade(first.ID, testUpgradeInstruction(digest), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecordNodeUpgradeReport(second.ID, domain.NodeUpgradeReport{TaskID: task.ID, Status: domain.NodeUpgradeApplying}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-node report = %v", err)
	}
	if _, err := database.db.Exec(`UPDATE node_upgrade_tasks SET deadline_at = ? WHERE id = ?`, stamp(time.Now().Add(-time.Minute)), task.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.ReconcileNodeUpgrades(); err != nil {
		t.Fatal(err)
	}
	timedOut, err := database.NodeUpgradeTask(task.ID)
	if err != nil || timedOut.Status != domain.NodeUpgradeFailed || timedOut.ErrorCode != "upgrade_timeout" {
		t.Fatalf("timed out task = %#v, err=%v", timedOut, err)
	}
	late, err := database.RecordNodeUpgradeReport(first.ID, domain.NodeUpgradeReport{
		TaskID: task.ID, Status: domain.NodeUpgradeSucceeded, Detail: "completed late", InstalledSHA256: digest,
	})
	if err != nil || late.Status != domain.NodeUpgradeSucceeded {
		t.Fatalf("late success = %#v, err=%v", late, err)
	}
}

func TestInterruptedUpgradeIsRecoveredFromInstalledAgentDigest(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sourceDigest := strings.Repeat("5", 64)
	targetDigest := strings.Repeat("6", 64)

	immediate, err := database.CreateNode("edge-immediate-recovery", "203.0.113.95")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.HeartbeatWithAgent(immediate.ID, 0, "", nil, sourceDigest, ""); err != nil {
		t.Fatal(err)
	}
	immediateTask, _, err := database.CreateOrGetNodeUpgrade(immediate.ID, testUpgradeInstruction(targetDigest), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecordNodeUpgradeReport(immediate.ID, domain.NodeUpgradeReport{TaskID: immediateTask.ID, Status: domain.NodeUpgradeApplying}); err != nil {
		t.Fatal(err)
	}
	if err := database.HeartbeatWithAgent(immediate.ID, 0, "", nil, targetDigest, immediateTask.ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := database.RecordNodeUpgradeReport(immediate.ID, domain.NodeUpgradeReport{
		TaskID: immediateTask.ID, Status: domain.NodeUpgradeFailed, ErrorCode: "updater_interrupted",
		Detail: "edge updater stopped without recording a result",
	})
	if err != nil || recovered.Status != domain.NodeUpgradeSucceeded || recovered.ErrorCode != "" || recovered.Detail != recoveredUpgradeDetail {
		t.Fatalf("immediate recovery = %#v, err=%v", recovered, err)
	}

	historical, err := database.CreateNode("edge-historical-recovery", "203.0.113.96")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.HeartbeatWithAgent(historical.ID, 0, "", nil, sourceDigest, ""); err != nil {
		t.Fatal(err)
	}
	historicalTask, _, err := database.CreateOrGetNodeUpgrade(historical.ID, testUpgradeInstruction(targetDigest), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	failed, err := database.RecordNodeUpgradeReport(historical.ID, domain.NodeUpgradeReport{
		TaskID: historicalTask.ID, Status: domain.NodeUpgradeFailed, ErrorCode: "updater_interrupted",
		Detail: "edge updater stopped without recording a result",
	})
	if err != nil || failed.Status != domain.NodeUpgradeFailed {
		t.Fatalf("historical failure = %#v, err=%v", failed, err)
	}
	if err := database.HeartbeatWithAgent(historical.ID, 0, "", nil, targetDigest, ""); err != nil {
		t.Fatal(err)
	}
	reconciled, err := database.NodeUpgradeTask(historicalTask.ID)
	if err != nil || reconciled.Status != domain.NodeUpgradeSucceeded || reconciled.ErrorCode != "" || reconciled.Detail != recoveredUpgradeDetail {
		t.Fatalf("historical reconciliation = %#v, err=%v", reconciled, err)
	}
}

func TestNodeUpgradePersistsAndValidatesManagedNginxArtifact(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-nginx-upgrade", "203.0.113.97")
	if err != nil {
		t.Fatal(err)
	}
	sourceAgent := strings.Repeat("7", 64)
	targetAgent := strings.Repeat("8", 64)
	sourceNginx := strings.Repeat("9", 64)
	targetNginx := strings.Repeat("a", 64)
	if err := database.HeartbeatWithArtifacts(node.ID, 0, "", nil, "v1", sourceAgent, "1.30.3", sourceNginx, ""); err != nil {
		t.Fatal(err)
	}
	instruction := testUpgradeInstruction(targetAgent)
	bundle := domain.UpgradeArtifact{URL: "https://control.example.test/nginx", SHA256: targetNginx}
	service := domain.UpgradeArtifact{URL: "https://control.example.test/nginx-service", SHA256: strings.Repeat("b", 64)}
	instruction.NginxBundle = &bundle
	instruction.NginxService = &service
	task, created, err := database.CreateOrGetNodeUpgrade(node.ID, instruction, time.Now().Add(time.Hour))
	if err != nil || !created || task.SourceNginxSHA256 != sourceNginx || task.TargetNginxSHA256 != targetNginx {
		t.Fatalf("managed Nginx task = %#v, created=%v, err=%v", task, created, err)
	}
	loaded, err := database.NodeUpgradeInstruction(node.ID)
	if err != nil || loaded.NginxBundle == nil || loaded.NginxBundle.SHA256 != targetNginx ||
		loaded.NginxService == nil || loaded.NginxService.SHA256 != service.SHA256 {
		t.Fatalf("managed Nginx instruction = %#v, err=%v", loaded, err)
	}
	if _, err := database.RecordNodeUpgradeReport(node.ID, domain.NodeUpgradeReport{
		TaskID: task.ID, Status: domain.NodeUpgradeSucceeded, InstalledSHA256: targetAgent,
	}); err == nil {
		t.Fatal("accepted success without the target managed Nginx digest")
	}
	completed, err := database.RecordNodeUpgradeReport(node.ID, domain.NodeUpgradeReport{
		TaskID: task.ID, Status: domain.NodeUpgradeSucceeded, InstalledSHA256: targetAgent, InstalledNginxSHA256: targetNginx,
	})
	if err != nil || completed.Status != domain.NodeUpgradeSucceeded {
		t.Fatalf("managed Nginx completion = %#v, err=%v", completed, err)
	}
}

func testUpgradeInstruction(targetDigest string) domain.NodeUpgradeInstruction {
	return domain.NodeUpgradeInstruction{
		Binary:         domain.UpgradeArtifact{URL: "https://control.example.test/edge", SHA256: targetDigest},
		Installer:      domain.UpgradeArtifact{URL: "https://control.example.test/install", SHA256: strings.Repeat("a", 64)},
		AgentService:   domain.UpgradeArtifact{URL: "https://control.example.test/agent-service", SHA256: strings.Repeat("b", 64)},
		UpdaterService: domain.UpgradeArtifact{URL: "https://control.example.test/updater-service", SHA256: strings.Repeat("c", 64)},
	}
}
