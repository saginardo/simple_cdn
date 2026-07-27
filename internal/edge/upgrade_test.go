package edge

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"simple_cdn/internal/domain"
)

type fakeUpgradeRunner struct {
	starts int
	active bool
	err    error
}

func (f *fakeUpgradeRunner) Start(string) error {
	f.starts++
	return f.err
}

func (f *fakeUpgradeRunner) Active(string) (bool, error) { return f.active, f.err }

type upgradeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f upgradeRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestUpgradeUnitStillRunningStates(t *testing.T) {
	for _, state := range []string{"active", "activating", "reloading", "refreshing", "deactivating"} {
		running, err := upgradeUnitStillRunning(state)
		if err != nil || !running {
			t.Errorf("state %q: running=%v err=%v", state, running, err)
		}
	}
	for _, state := range []string{"inactive", "failed"} {
		running, err := upgradeUnitStillRunning(state)
		if err != nil || running {
			t.Errorf("state %q: running=%v err=%v", state, running, err)
		}
	}
	if _, err := upgradeUnitStillRunning("maintenance"); err == nil {
		t.Fatal("unknown updater state was treated as terminal")
	}
}

func TestAgentWaitsForUpdaterResultAfterFirstTerminalObservation(t *testing.T) {
	stateDir := t.TempDir()
	taskID := uuid.NewString()
	directory := stateDir + "/upgrades/" + taskID
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateDir+"/active-upgrade-task", []byte(taskID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(directory+"/launched", []byte("launched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reports []domain.NodeUpgradeReport
	client := &http.Client{Transport: upgradeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodPost && request.URL.Path == "/api/edge/v1/upgrade-report" {
			var report domain.NodeUpgradeReport
			if err := json.NewDecoder(request.Body).Decode(&report); err != nil {
				return nil, err
			}
			reports = append(reports, report)
			return upgradeHTTPResponse(http.StatusOK, []byte(`{}`)), nil
		}
		return upgradeHTTPResponse(http.StatusNotFound, nil), nil
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: stateDir, CertificateDir: stateDir + "/certs",
		AgentSHA256: strings.Repeat("2", 64), HTTPClient: client, UpgradeRunner: &fakeUpgradeRunner{}, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.ProcessUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 0 || agent.activeUpgradeID() != taskID {
		t.Fatalf("first terminal observation: reports=%#v active=%q", reports, agent.activeUpgradeID())
	}
	if _, err := os.Stat(directory + "/updater-terminal"); err != nil {
		t.Fatalf("terminal observation marker: %v", err)
	}
	success := domain.NodeUpgradeReport{
		TaskID: taskID, Status: domain.NodeUpgradeSucceeded, Detail: "complete", InstalledSHA256: strings.Repeat("2", 64),
	}
	if err := writeLocalUpgradeReport(directory, success); err != nil {
		t.Fatal(err)
	}
	if err := agent.ProcessUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Status != domain.NodeUpgradeSucceeded || agent.activeUpgradeID() != "" {
		t.Fatalf("delayed updater result: reports=%#v active=%q", reports, agent.activeUpgradeID())
	}
}

func TestAgentReportsInterruptionAfterTwoTerminalObservations(t *testing.T) {
	stateDir := t.TempDir()
	taskID := uuid.NewString()
	directory := stateDir + "/upgrades/" + taskID
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateDir+"/active-upgrade-task", []byte(taskID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(directory+"/launched", []byte("launched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reports []domain.NodeUpgradeReport
	client := &http.Client{Transport: upgradeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		var report domain.NodeUpgradeReport
		if err := json.NewDecoder(request.Body).Decode(&report); err != nil {
			return nil, err
		}
		reports = append(reports, report)
		return upgradeHTTPResponse(http.StatusOK, []byte(`{}`)), nil
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: stateDir, CertificateDir: stateDir + "/certs",
		AgentSHA256: strings.Repeat("2", 64), HTTPClient: client, UpgradeRunner: &fakeUpgradeRunner{}, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.ProcessUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := agent.ProcessUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Status != domain.NodeUpgradeFailed || reports[0].ErrorCode != "updater_interrupted" {
		t.Fatalf("interruption reports = %#v", reports)
	}
}

func TestAgentStagesOnlineUpgradeAndReportsResultAfterRestart(t *testing.T) {
	stateDir := t.TempDir()
	taskID := uuid.NewString()
	sourceDigest := strings.Repeat("1", 64)
	sourceNginxDigest := strings.Repeat("3", 64)
	binary := []byte("new edge binary")
	installer := []byte("#!/usr/bin/env bash\nexit 0\n")
	service := []byte("agent service")
	updaterService := []byte("updater service")
	nginxBundle := []byte("managed nginx bundle")
	nginxService := []byte("managed nginx service")
	nginxBundleArtifact := testUpgradeArtifact("https://control.example.test/nginx", nginxBundle)
	nginxServiceArtifact := testUpgradeArtifact("https://control.example.test/nginx-service", nginxService)
	instruction := domain.NodeUpgradeInstruction{
		TaskID: taskID, DeadlineAt: time.Now().Add(time.Hour),
		Binary:         testUpgradeArtifact("https://control.example.test/binary", binary),
		Installer:      testUpgradeArtifact("https://control.example.test/installer", installer),
		AgentService:   testUpgradeArtifact("https://control.example.test/service", service),
		UpdaterService: testUpgradeArtifact("https://control.example.test/updater", updaterService),
		NginxBundle:    &nginxBundleArtifact,
		NginxService:   &nginxServiceArtifact,
	}
	artifacts := map[string][]byte{
		"/binary": binary, "/installer": installer, "/service": service, "/updater": updaterService,
		"/nginx": nginxBundle, "/nginx-service": nginxService,
	}
	var reports []domain.NodeUpgradeReport
	client := &http.Client{Transport: upgradeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/edge/v1/upgrade":
			contents, _ := json.Marshal(instruction)
			return upgradeHTTPResponse(http.StatusOK, contents), nil
		case request.Method == http.MethodGet && artifacts[request.URL.Path] != nil:
			return upgradeHTTPResponse(http.StatusOK, artifacts[request.URL.Path]), nil
		case request.Method == http.MethodPost && request.URL.Path == "/api/edge/v1/upgrade-report":
			var report domain.NodeUpgradeReport
			if err := json.NewDecoder(request.Body).Decode(&report); err != nil {
				return nil, err
			}
			reports = append(reports, report)
			return upgradeHTTPResponse(http.StatusOK, []byte(`{"ok":true}`)), nil
		default:
			return upgradeHTTPResponse(http.StatusNotFound, nil), nil
		}
	})}
	runner := &fakeUpgradeRunner{active: true}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: stateDir, CertificateDir: stateDir + "/certs",
		AgentSHA256: sourceDigest, NginxVersion: "1.30.3", NginxSHA256: sourceNginxDigest,
		HTTPClient: client, UpgradeRunner: runner, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.ProcessUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.starts != 1 || agent.activeUpgradeID() != taskID || len(reports) != 1 || reports[0].Status != domain.NodeUpgradeApplying {
		t.Fatalf("staged upgrade: starts=%d active=%q reports=%#v", runner.starts, agent.activeUpgradeID(), reports)
	}
	if contents, err := io.ReadAll(mustOpen(t, agent.upgradeDirectory(taskID)+"/cdn-edge-agent")); err != nil || string(contents) != string(binary) {
		t.Fatalf("staged binary = %q, err=%v", contents, err)
	}
	if contents, err := os.ReadFile(agent.upgradeDirectory(taskID) + "/cdn-nginx.tar.gz"); err != nil || string(contents) != string(nginxBundle) {
		t.Fatalf("staged Nginx bundle = %q, err=%v", contents, err)
	}

	agentOnly, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: stateDir, CertificateDir: stateDir + "/certs",
		AgentSHA256: instruction.Binary.SHA256, NginxVersion: "1.30.3", NginxSHA256: sourceNginxDigest,
		HTTPClient: client, UpgradeRunner: runner, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := agentOnly.markUpgradeReady(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(agentOnly.upgradeDirectory(taskID) + "/ready"); !os.IsNotExist(err) {
		t.Fatalf("agent-only update incorrectly became ready: %v", err)
	}

	upgraded, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: stateDir, CertificateDir: stateDir + "/certs",
		AgentSHA256: instruction.Binary.SHA256, NginxVersion: "1.30.4", NginxSHA256: nginxBundleArtifact.SHA256,
		HTTPClient: client, UpgradeRunner: runner, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := upgraded.markUpgradeReady(); err != nil {
		t.Fatal(err)
	}
	if ready, err := io.ReadAll(mustOpen(t, upgraded.upgradeDirectory(taskID)+"/ready")); err != nil || strings.TrimSpace(string(ready)) != instruction.Binary.SHA256 {
		t.Fatalf("readiness = %q, err=%v", ready, err)
	}
	if err := writeLocalUpgradeReport(upgraded.upgradeDirectory(taskID), domain.NodeUpgradeReport{
		TaskID: taskID, Status: domain.NodeUpgradeSucceeded, Detail: "complete", InstalledSHA256: instruction.Binary.SHA256,
		InstalledNginxSHA256: nginxBundleArtifact.SHA256,
	}); err != nil {
		t.Fatal(err)
	}
	if err := upgraded.ProcessUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if upgraded.activeUpgradeID() != "" || len(reports) != 2 || reports[1].Status != domain.NodeUpgradeSucceeded {
		t.Fatalf("completed upgrade: active=%q reports=%#v", upgraded.activeUpgradeID(), reports)
	}
}

func TestAgentRejectsUpgradeArtifactWithWrongDigest(t *testing.T) {
	binary := []byte("tampered binary")
	declaredBinary := []byte("expected binary")
	instruction := domain.NodeUpgradeInstruction{
		TaskID: uuid.NewString(), DeadlineAt: time.Now().Add(time.Hour),
		Binary:         testUpgradeArtifact("https://control.example.test/binary", declaredBinary),
		Installer:      testUpgradeArtifact("https://control.example.test/installer", []byte("installer")),
		AgentService:   testUpgradeArtifact("https://control.example.test/service", []byte("service")),
		UpdaterService: testUpgradeArtifact("https://control.example.test/updater", []byte("updater")),
	}
	var reports []domain.NodeUpgradeReport
	client := &http.Client{Transport: upgradeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/api/edge/v1/upgrade" {
			contents, _ := json.Marshal(instruction)
			return upgradeHTTPResponse(http.StatusOK, contents), nil
		}
		if request.URL.Path == "/installer" {
			return upgradeHTTPResponse(http.StatusOK, []byte("installer")), nil
		}
		if request.URL.Path == "/binary" {
			return upgradeHTTPResponse(http.StatusOK, binary), nil
		}
		if request.URL.Path == "/api/edge/v1/upgrade-report" {
			var report domain.NodeUpgradeReport
			_ = json.NewDecoder(request.Body).Decode(&report)
			reports = append(reports, report)
			return upgradeHTTPResponse(http.StatusOK, []byte(`{}`)), nil
		}
		return upgradeHTTPResponse(http.StatusNotFound, nil), nil
	})}
	runner := &fakeUpgradeRunner{}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: t.TempDir(), CertificateDir: filepath.Join(t.TempDir(), "certs"),
		AgentSHA256: strings.Repeat("1", 64), HTTPClient: client, UpgradeRunner: runner, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = agent.ProcessUpgrade(context.Background())
	if err == nil || !strings.Contains(err.Error(), "SHA-256") || runner.starts != 0 || len(reports) != 1 || reports[0].Status != domain.NodeUpgradeFailed {
		t.Fatalf("checksum failure: err=%v starts=%d reports=%#v", err, runner.starts, reports)
	}
}

func TestUpgradeDownloadsUseIsolatedCertificateFreeClient(t *testing.T) {
	directory := t.TempDir()
	identity := newTransportTestIdentity(t)
	agent := newTransportTestAgent(t, "https://control.example.test", directory, identity)
	defer agent.resetControlClient()
	defer agent.resetArtifactClient()
	controlClient := agent.client()
	downloadClient := agent.upgradeClient()
	if downloadClient == controlClient || downloadClient.Timeout != upgradeDownloadTimeout {
		t.Fatalf("upgrade client was not isolated: control=%p download=%p timeout=%s", controlClient, downloadClient, downloadClient.Timeout)
	}
	transport, ok := downloadClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("upgrade transport type = %T", downloadClient.Transport)
	}
	if len(transport.TLSClientConfig.Certificates) != 0 {
		t.Fatal("upgrade transport exposes the edge mTLS certificate")
	}
	original, _ := http.NewRequest(http.MethodGet, "https://control.example.test/artifact", nil)
	sameOrigin, _ := http.NewRequest(http.MethodGet, "https://control.example.test/releases/artifact", nil)
	if err := downloadClient.CheckRedirect(sameOrigin, []*http.Request{original}); err != nil {
		t.Fatalf("same-origin redirect was rejected: %v", err)
	}
	crossOrigin, _ := http.NewRequest(http.MethodGet, "https://downloads.example.test/artifact", nil)
	if err := downloadClient.CheckRedirect(crossOrigin, []*http.Request{original}); err == nil {
		t.Fatal("cross-origin upgrade redirect was accepted")
	}
}

func TestUpgradeHelperRunsStagedInstallerAndPersistsSuccess(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CDN_EDGE_INSTALL_ROOT", root)
	stateDir := root + "/opt/cdn-edge/data"
	taskID := uuid.NewString()
	directory := stateDir + "/upgrades/" + taskID
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := []byte("verified target binary")
	artifact := testUpgradeArtifact("https://control.example.test/binary", binary)
	nginxBundle := testUpgradeArtifact("https://control.example.test/nginx", []byte("nginx bundle"))
	nginxService := testUpgradeArtifact("https://control.example.test/nginx-service", []byte("nginx service"))
	manifest := localUpgradeManifest{ControlURL: "https://control.example.test", Instruction: domain.NodeUpgradeInstruction{
		TaskID: taskID, Binary: artifact, NginxBundle: &nginxBundle, NginxService: &nginxService,
	}}
	manifestContents, _ := json.Marshal(manifest)
	if err := os.WriteFile(directory+"/manifest.json", manifestContents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(directory+"/cdn-edge-agent", binary, 0o700); err != nil {
		t.Fatal(err)
	}
	installer := `#!/usr/bin/env bash
set -euo pipefail
nginx_digest=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --nginx-bundle-sha256) nginx_digest="$2"; shift 2 ;;
    *) shift ;;
  esac
done
mkdir -p "$CDN_EDGE_INSTALL_ROOT/opt/cdn-edge/bin"
cp "$(dirname "$0")/cdn-edge-agent" "$CDN_EDGE_INSTALL_ROOT/opt/cdn-edge/bin/cdn-edge-agent"
mkdir -p "$CDN_EDGE_INSTALL_ROOT/opt/cdn-edge/nginx"
printf '%s\n' "$nginx_digest" >"$CDN_EDGE_INSTALL_ROOT/opt/cdn-edge/nginx/.bundle-sha256"
echo "staged installer completed"
`
	if err := os.WriteFile(directory+"/installer.sh", []byte(installer), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RunUpgradeHelper(stateDir, taskID); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(directory + "/result.json")
	if err != nil {
		t.Fatal(err)
	}
	var report domain.NodeUpgradeReport
	if err := json.Unmarshal(contents, &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != domain.NodeUpgradeSucceeded || report.InstalledSHA256 != artifact.SHA256 ||
		report.InstalledNginxSHA256 != nginxBundle.SHA256 {
		t.Fatalf("helper report = %#v", report)
	}
}

func testUpgradeArtifact(rawURL string, contents []byte) domain.UpgradeArtifact {
	return domain.UpgradeArtifact{URL: rawURL, SHA256: fmt.Sprintf("%x", sha256.Sum256(contents))}
}

func upgradeHTTPResponse(status int, contents []byte) *http.Response {
	return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d status", status), Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(contents)))}
}

func mustOpen(t *testing.T, path string) io.ReadCloser {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return file
}
