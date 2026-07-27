package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"simple_cdn/internal/domain"
	"simple_cdn/internal/store"
)

func TestNodeOnlineUpgradeAPIQueuesInstructionAndAcceptsEdgeResult(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, err := database.CreateNode("edge-online", "203.0.113.93")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetNodeCapabilities(node.ID, []string{domain.EdgeCapabilityOnlineUpgrade, domain.EdgeCapabilityNginxBundle}); err != nil {
		t.Fatal(err)
	}
	sourceDigest := strings.Repeat("1", 64)
	targetDigest := strings.Repeat("2", 64)
	sourceNginxDigest := strings.Repeat("3", 64)
	targetNginxDigest := strings.Repeat("4", 64)
	if err := database.HeartbeatWithArtifacts(node.ID, 0, "", nil, "v0", sourceDigest, "1.30.3", sourceNginxDigest, ""); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		Store: database, ControlURL: "https://control.example.test", EdgeControlURL: "https://control.example.test:8443",
		EdgeBinaryURL: "https://control.example.test/downloads/edge", EdgeBinarySHA256: targetDigest,
		NginxBundleURL: "https://control.example.test/downloads/nginx", NginxBundleSHA256: targetNginxDigest, NginxVersion: "1.30.4",
	}
	startRequest := httptest.NewRequest(http.MethodPost, "/api/nodes/"+node.ID+"/upgrade", nil)
	startRequest.SetPathValue("id", node.ID)
	startResponse := httptest.NewRecorder()
	server.startNodeUpgrade(startResponse, startRequest)
	if startResponse.Code != http.StatusCreated {
		t.Fatalf("start upgrade status = %d, body=%s", startResponse.Code, startResponse.Body.String())
	}
	var status nodeUpgradeStatusResponse
	if err := json.Unmarshal(startResponse.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.UpgradeTask == nil || status.UpgradeTask.Status != domain.NodeUpgradeQueued || status.CanUpgrade {
		t.Fatalf("start upgrade response = %#v", status)
	}

	edgeContext := context.WithValue(context.Background(), edgeContextKey{}, node.ID)
	instructionRequest := httptest.NewRequest(http.MethodGet, "/api/edge/v1/upgrade", nil).WithContext(edgeContext)
	instructionResponse := httptest.NewRecorder()
	server.edgeUpgradeInstruction(instructionResponse, instructionRequest)
	if instructionResponse.Code != http.StatusOK {
		t.Fatalf("instruction status = %d, body=%s", instructionResponse.Code, instructionResponse.Body.String())
	}
	var instruction domain.NodeUpgradeInstruction
	if err := json.Unmarshal(instructionResponse.Body.Bytes(), &instruction); err != nil {
		t.Fatal(err)
	}
	if instruction.TaskID != status.UpgradeTask.ID || instruction.Binary.SHA256 != targetDigest || instruction.UpdaterService.SHA256 == "" ||
		instruction.NginxBundle == nil || instruction.NginxBundle.SHA256 != targetNginxDigest || instruction.NginxService == nil {
		t.Fatalf("instruction = %#v", instruction)
	}

	reportBody := strings.NewReader(`{"task_id":"` + instruction.TaskID + `","status":"succeeded","detail":"ready","installed_sha256":"` + targetDigest + `","installed_nginx_sha256":"` + targetNginxDigest + `"}`)
	reportRequest := httptest.NewRequest(http.MethodPost, "/api/edge/v1/upgrade-report", reportBody).WithContext(edgeContext)
	reportResponse := httptest.NewRecorder()
	server.edgeUpgradeReport(reportResponse, reportRequest)
	if reportResponse.Code != http.StatusOK {
		t.Fatalf("report status = %d, body=%s", reportResponse.Code, reportResponse.Body.String())
	}
	completed, err := database.NodeUpgradeTask(instruction.TaskID)
	if err != nil || completed.Status != domain.NodeUpgradeSucceeded {
		t.Fatalf("completed task = %#v, err=%v", completed, err)
	}
}

func TestNodeOnlineUpgradeStatusRequiresCapabilityAndFreshHeartbeat(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node, _ := database.CreateNode("edge-bootstrap", "203.0.113.94")
	if err := database.HeartbeatWithAgent(node.ID, 0, "", nil, strings.Repeat("1", 64), ""); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		Store: database, EdgeControlURL: "https://control.example.test:8443",
		EdgeBinaryURL: "https://control.example.test/edge", EdgeBinarySHA256: strings.Repeat("2", 64),
		NginxBundleURL: "https://control.example.test/nginx", NginxBundleSHA256: strings.Repeat("3", 64), NginxVersion: "1.30.4",
	}
	node, _ = database.GetNode(node.ID)
	status, err := server.buildNodeUpgradeStatus(node)
	if err != nil {
		t.Fatal(err)
	}
	if status.CanUpgrade || status.UpgradeCapable || !strings.Contains(status.UpgradeBlocker, "手动") {
		t.Fatalf("legacy node status = %#v", status)
	}
}

func TestNodeOnlineUpgradeCanUpdateOnlyManagedNginxAndBlocksLegacyAgent(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	targetAgent := strings.Repeat("5", 64)
	sourceNginx := strings.Repeat("6", 64)
	targetNginx := strings.Repeat("7", 64)
	managed, _ := database.CreateNode("managed-nginx", "203.0.113.105")
	legacy, _ := database.CreateNode("legacy-nginx", "203.0.113.106")
	for _, node := range []domain.Node{managed, legacy} {
		if err := database.SetNodeCapabilities(node.ID, []string{domain.EdgeCapabilityOnlineUpgrade}); err != nil {
			t.Fatal(err)
		}
		if err := database.HeartbeatWithArtifacts(node.ID, 0, "", nil, "v1", targetAgent, "1.30.3", sourceNginx, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.SetNodeCapabilities(managed.ID, []string{domain.EdgeCapabilityOnlineUpgrade, domain.EdgeCapabilityNginxBundle}); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		Store: database, EdgeControlURL: "https://control.example.test:8443",
		EdgeBinaryURL: "https://control.example.test/edge", EdgeBinarySHA256: targetAgent,
		NginxBundleURL: "https://control.example.test/nginx", NginxBundleSHA256: targetNginx, NginxVersion: "1.30.4",
	}
	managed, _ = database.GetNode(managed.ID)
	status, err := server.buildNodeUpgradeStatus(managed)
	if err != nil || !status.CanUpgrade || status.UpgradeUpToDate {
		t.Fatalf("managed Nginx-only status = %#v, err=%v", status, err)
	}
	instruction := server.nodeUpgradeInstruction(managed)
	if instruction.Binary.SHA256 != targetAgent || instruction.NginxBundle == nil || instruction.NginxBundle.SHA256 != targetNginx {
		t.Fatalf("managed Nginx-only instruction = %#v", instruction)
	}
	legacy, _ = database.GetNode(legacy.ID)
	legacyStatus, err := server.buildNodeUpgradeStatus(legacy)
	if err != nil || legacyStatus.CanUpgrade || !strings.Contains(legacyStatus.UpgradeBlocker, "受管 Nginx") {
		t.Fatalf("legacy Nginx-only status = %#v, err=%v", legacyStatus, err)
	}
}

func TestStartAllNodeUpgradesQueuesOnlyEligibleOutdatedNodes(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sourceDigest := strings.Repeat("1", 64)
	targetDigest := strings.Repeat("2", 64)
	sourceNginxDigest := strings.Repeat("3", 64)
	targetNginxDigest := strings.Repeat("4", 64)
	eligible, _ := database.CreateNode("eligible", "203.0.113.101")
	current, _ := database.CreateNode("current", "203.0.113.102")
	blocked, _ := database.CreateNode("blocked", "203.0.113.103")
	for _, node := range []domain.Node{eligible, current} {
		if err := database.SetNodeCapabilities(node.ID, []string{domain.EdgeCapabilityOnlineUpgrade, domain.EdgeCapabilityNginxBundle}); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.HeartbeatWithArtifacts(eligible.ID, 0, "", nil, "v0", sourceDigest, "1.30.3", sourceNginxDigest, ""); err != nil {
		t.Fatal(err)
	}
	if err := database.HeartbeatWithArtifacts(current.ID, 0, "", nil, "v1", targetDigest, "1.30.4", targetNginxDigest, ""); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		Store: database, EdgeControlURL: "https://control.example.test:8443",
		EdgeBinaryURL: "https://control.example.test/edge", EdgeBinarySHA256: targetDigest,
		NginxBundleURL: "https://control.example.test/nginx", NginxBundleSHA256: targetNginxDigest, NginxVersion: "1.30.4",
	}
	request := httptest.NewRequest(http.MethodPost, "/api/nodes/upgrade-all", nil)
	response := httptest.NewRecorder()
	server.startAllNodeUpgrades(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("bulk upgrade status = %d, body=%s", response.Code, response.Body.String())
	}
	var result nodeUpgradeAllResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Created != 1 || result.UpToDate != 1 || result.Blocked != 1 || len(result.Results) != 3 {
		t.Fatalf("bulk upgrade result = %#v", result)
	}
	if _, err := database.LatestNodeUpgrade(eligible.ID); err != nil {
		t.Fatalf("eligible node was not queued: %v", err)
	}
	if _, err := database.LatestNodeUpgrade(current.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("current node received an upgrade: %v", err)
	}
	if _, err := database.LatestNodeUpgrade(blocked.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("blocked node received an upgrade: %v", err)
	}
}
