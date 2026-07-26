package edge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"simple_cdn/internal/domain"
)

const (
	upgradeResourceLimit   = 1 << 20
	upgradeBinaryLimit     = 128 << 20
	upgradeLogLimit        = 64 << 10
	upgradeDownloadTimeout = 5 * time.Minute
)

type UpgradeRunner interface {
	Start(taskID string) error
	Active(taskID string) (bool, error)
}

type SystemdUpgradeRunner struct{}

func (SystemdUpgradeRunner) Start(taskID string) error {
	return exec.Command("systemctl", "start", "--no-block", upgradeUnitName(taskID)).Run()
}

func (SystemdUpgradeRunner) Active(taskID string) (bool, error) {
	output, err := exec.Command("systemctl", "show", "--property=ActiveState", "--value", upgradeUnitName(taskID)).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("inspect edge updater state: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return upgradeUnitStillRunning(string(output))
}

func upgradeUnitStillRunning(value string) (bool, error) {
	switch strings.TrimSpace(value) {
	case "active", "activating", "reloading", "refreshing", "deactivating":
		return true, nil
	case "inactive", "failed":
		return false, nil
	default:
		return false, fmt.Errorf("unrecognized edge updater state %q", strings.TrimSpace(value))
	}
}

func upgradeUnitName(taskID string) string {
	return "cdn-edge-updater@" + taskID + ".service"
}

type localUpgradeManifest struct {
	ControlURL  string                        `json:"control_url"`
	Instruction domain.NodeUpgradeInstruction `json:"instruction"`
}

func appendCapability(values []string, wanted string) []string {
	for _, value := range values {
		if value == wanted {
			return values
		}
	}
	return append(values, wanted)
}

func executableSHA256() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	return fileSHA256(path)
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (a *Agent) ProcessUpgrade(ctx context.Context) error {
	if taskID := a.activeUpgradeID(); taskID != "" {
		return a.resumeUpgrade(ctx, taskID)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.Config.ControlURL+"/api/edge/v1/upgrade", nil)
	if err != nil {
		return err
	}
	response, err := a.client().Do(request)
	if err != nil {
		return fmt.Errorf("pull online upgrade: %w", err)
	}
	defer drainAndClose(response.Body)
	if response.StatusCode == http.StatusNoContent {
		return nil
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("pull online upgrade: %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var instruction domain.NodeUpgradeInstruction
	decoder := json.NewDecoder(io.LimitReader(response.Body, upgradeResourceLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&instruction); err != nil {
		return fmt.Errorf("decode online upgrade: %w", err)
	}
	if err := validateUpgradeInstruction(instruction); err != nil {
		return err
	}
	if strings.EqualFold(instruction.Binary.SHA256, a.Config.AgentSHA256) {
		return a.sendUpgradeReport(ctx, domain.NodeUpgradeReport{
			TaskID: instruction.TaskID, Status: domain.NodeUpgradeSucceeded,
			Detail: "edge already runs the requested artifact", InstalledSHA256: a.Config.AgentSHA256,
		})
	}
	return a.stageAndStartUpgrade(ctx, instruction)
}

func validateUpgradeInstruction(instruction domain.NodeUpgradeInstruction) error {
	if _, err := uuid.Parse(instruction.TaskID); err != nil {
		return errors.New("online upgrade instruction has an invalid task ID")
	}
	if !instruction.DeadlineAt.After(time.Now().UTC()) {
		return errors.New("online upgrade instruction has expired")
	}
	for name, artifact := range map[string]domain.UpgradeArtifact{
		"binary": instruction.Binary, "installer": instruction.Installer,
		"agent service": instruction.AgentService, "updater service": instruction.UpdaterService,
	} {
		parsed, err := url.Parse(strings.TrimSpace(artifact.URL))
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
			return fmt.Errorf("online upgrade %s URL must be absolute HTTPS", name)
		}
		if !validDigest(artifact.SHA256) {
			return fmt.Errorf("online upgrade %s digest is invalid", name)
		}
	}
	return nil
}

func validDigest(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (a *Agent) stageAndStartUpgrade(ctx context.Context, instruction domain.NodeUpgradeInstruction) error {
	directory := a.upgradeDirectory(instruction.TaskID)
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	manifest := localUpgradeManifest{ControlURL: a.Config.ControlURL, Instruction: instruction}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := atomicWriteFile(filepath.Join(directory, "manifest.json"), encoded, 0o600); err != nil {
		return err
	}
	if err := atomicWriteFile(a.activeUpgradePath(), []byte(instruction.TaskID+"\n"), 0o600); err != nil {
		return err
	}
	fail := func(code string, cause error) error {
		report := domain.NodeUpgradeReport{TaskID: instruction.TaskID, Status: domain.NodeUpgradeFailed, ErrorCode: code, Detail: cause.Error()}
		_ = writeLocalUpgradeReport(directory, report)
		if a.sendUpgradeReport(ctx, report) == nil {
			_ = a.clearLocalUpgrade(directory, report)
		}
		return cause
	}
	artifacts := []struct {
		artifact domain.UpgradeArtifact
		name     string
		mode     os.FileMode
		limit    int64
	}{
		{instruction.Installer, "installer.sh", 0o700, upgradeResourceLimit},
		{instruction.Binary, "cdn-edge-agent", 0o700, upgradeBinaryLimit},
		{instruction.AgentService, "cdn-edge-agent.service", 0o600, upgradeResourceLimit},
		{instruction.UpdaterService, "cdn-edge-updater@.service", 0o600, upgradeResourceLimit},
	}
	for _, item := range artifacts {
		if err := a.downloadUpgradeArtifact(ctx, item.artifact, filepath.Join(directory, item.name), item.mode, item.limit); err != nil {
			return fail("artifact_download_failed", err)
		}
	}
	applying := domain.NodeUpgradeReport{TaskID: instruction.TaskID, Status: domain.NodeUpgradeApplying, Detail: "upgrade artifacts verified; starting edge installer"}
	if err := a.sendUpgradeReport(ctx, applying); err != nil {
		return fail("start_report_failed", err)
	}
	if err := atomicWriteFile(filepath.Join(directory, "launched"), []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		return err
	}
	if err := a.Config.UpgradeRunner.Start(instruction.TaskID); err != nil {
		return fail("updater_start_failed", fmt.Errorf("start edge updater: %w", err))
	}
	return nil
}

func (a *Agent) downloadUpgradeArtifact(ctx context.Context, artifact domain.UpgradeArtifact, destination string, mode os.FileMode, limit int64) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, artifact.URL, nil)
	if err != nil {
		return err
	}
	response, err := a.upgradeClient().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", artifact.URL, response.Status)
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".upgrade-artifact-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(response.Body, limit+1))
	if copyErr != nil {
		temporary.Close()
		return copyErr
	}
	if written > limit {
		temporary.Close()
		return fmt.Errorf("download %s exceeds %d bytes", artifact.URL, limit)
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), artifact.SHA256) {
		temporary.Close()
		return fmt.Errorf("download %s failed SHA-256 verification", artifact.URL)
	}
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, destination)
}

func (a *Agent) upgradeClient() *http.Client {
	if a.Config.ArtifactHTTPClient != nil {
		return a.Config.ArtifactHTTPClient
	}
	// An injected control client is also used for artifacts in unit tests and
	// custom embeddings. Production agents leave both injection points nil.
	if a.Config.HTTPClient != nil {
		return a.Config.HTTPClient
	}
	a.artifactMu.Lock()
	defer a.artifactMu.Unlock()
	if a.artifactClient != nil {
		return a.artifactClient
	}
	roots, _ := x509.SystemCertPool()
	if roots == nil {
		roots = x509.NewCertPool()
	}
	if internalCA, err := os.ReadFile(a.Config.CAPath); err == nil {
		roots.AppendCertsFromPEM(internalCA)
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	a.artifactTransport = transport
	a.artifactClient = &http.Client{
		Transport: transport,
		Timeout:   upgradeDownloadTimeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("online upgrade download exceeded the redirect limit")
			}
			if request.URL.Scheme != "https" || len(via) == 0 || !strings.EqualFold(request.URL.Host, via[0].URL.Host) {
				return errors.New("online upgrade download refused a cross-origin redirect")
			}
			return nil
		},
	}
	return a.artifactClient
}

func (a *Agent) resetArtifactClient() {
	if a.Config.ArtifactHTTPClient != nil || a.Config.HTTPClient != nil {
		return
	}
	a.artifactMu.Lock()
	defer a.artifactMu.Unlock()
	if a.artifactTransport != nil {
		a.artifactTransport.CloseIdleConnections()
	}
	a.artifactClient = nil
	a.artifactTransport = nil
}

func (a *Agent) resumeUpgrade(ctx context.Context, taskID string) error {
	directory := a.upgradeDirectory(taskID)
	resultPath := filepath.Join(directory, "result.json")
	contents, resultErr := os.ReadFile(resultPath)
	if resultErr == nil {
		var report domain.NodeUpgradeReport
		if err := json.Unmarshal(contents, &report); err != nil {
			return fmt.Errorf("decode local upgrade result: %w", err)
		}
		if err := a.sendUpgradeReport(ctx, report); err != nil {
			return err
		}
		return a.clearLocalUpgrade(directory, report)
	}
	if !errors.Is(resultErr, os.ErrNotExist) {
		return resultErr
	}
	if _, err := os.Stat(filepath.Join(directory, "launched")); errors.Is(err, os.ErrNotExist) {
		report := domain.NodeUpgradeReport{TaskID: taskID, Status: domain.NodeUpgradeFailed, ErrorCode: "updater_interrupted", Detail: "edge upgrade was interrupted before the updater started"}
		if err := writeLocalUpgradeReport(directory, report); err != nil {
			return err
		}
		return a.resumeUpgrade(ctx, taskID)
	} else if err != nil {
		return err
	}
	active, err := a.Config.UpgradeRunner.Active(taskID)
	if err != nil {
		return err
	}
	if active {
		if err := os.Remove(filepath.Join(directory, "updater-terminal")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	terminalPath := filepath.Join(directory, "updater-terminal")
	if _, err := os.Stat(terminalPath); errors.Is(err, os.ErrNotExist) {
		return atomicWriteFile(terminalPath, []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o600)
	} else if err != nil {
		return err
	}
	report := domain.NodeUpgradeReport{TaskID: taskID, Status: domain.NodeUpgradeFailed, ErrorCode: "updater_interrupted", Detail: "edge updater stopped without recording a result"}
	if err := writeLocalUpgradeReport(directory, report); err != nil {
		return err
	}
	return a.resumeUpgrade(ctx, taskID)
}

func (a *Agent) clearLocalUpgrade(directory string, report domain.NodeUpgradeReport) error {
	if report.Status == domain.NodeUpgradeFailed {
		if logContents, err := os.ReadFile(filepath.Join(directory, "upgrade.log")); err == nil {
			_ = atomicWriteFile(filepath.Join(a.Config.StateDir, "last-upgrade-failure.log"), logContents, 0o600)
		}
	}
	if err := os.Remove(a.activeUpgradePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.RemoveAll(directory)
}

func (a *Agent) sendUpgradeReport(ctx context.Context, report domain.NodeUpgradeReport) error {
	payload, err := json.Marshal(report)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Config.ControlURL+"/api/edge/v1/upgrade-report", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client().Do(request)
	if err != nil {
		return err
	}
	defer drainAndClose(response.Body)
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("report online upgrade: %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func (a *Agent) activeUpgradeID() string {
	contents, err := os.ReadFile(a.activeUpgradePath())
	if err != nil {
		return ""
	}
	value := strings.TrimSpace(string(contents))
	if _, err := uuid.Parse(value); err != nil {
		return ""
	}
	return value
}

func (a *Agent) activeUpgradePath() string {
	return filepath.Join(a.Config.StateDir, "active-upgrade-task")
}

func (a *Agent) upgradeDirectory(taskID string) string {
	return filepath.Join(a.Config.StateDir, "upgrades", taskID)
}

func (a *Agent) markUpgradeReady() error {
	taskID := a.activeUpgradeID()
	if taskID == "" {
		return nil
	}
	manifest, err := readLocalUpgradeManifest(a.upgradeDirectory(taskID))
	if err != nil {
		return err
	}
	if !strings.EqualFold(manifest.Instruction.Binary.SHA256, a.Config.AgentSHA256) {
		return nil
	}
	return atomicWriteFile(filepath.Join(a.upgradeDirectory(taskID), "ready"), []byte(a.Config.AgentSHA256+"\n"), 0o600)
}

func readLocalUpgradeManifest(directory string) (localUpgradeManifest, error) {
	contents, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		return localUpgradeManifest{}, err
	}
	var manifest localUpgradeManifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		return localUpgradeManifest{}, err
	}
	return manifest, nil
}

func writeLocalUpgradeReport(directory string, report domain.NodeUpgradeReport) error {
	contents, err := json.Marshal(report)
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(directory, "result.json"), contents, 0o600)
}

func RunUpgradeHelper(stateDir, taskID string) error {
	if _, err := uuid.Parse(taskID); err != nil {
		return errors.New("invalid online upgrade task ID")
	}
	directory := filepath.Join(stateDir, "upgrades", taskID)
	manifest, err := readLocalUpgradeManifest(directory)
	if err != nil {
		return err
	}
	logPath := filepath.Join(directory, "upgrade.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	buffer := &boundedBuffer{limit: upgradeLogLimit}
	output := io.MultiWriter(os.Stdout, logFile, buffer)
	arguments := []string{
		filepath.Join(directory, "installer.sh"),
		"--control-url", manifest.ControlURL,
		"--binary-file", filepath.Join(directory, "cdn-edge-agent"),
		"--binary-sha256", manifest.Instruction.Binary.SHA256,
		"--service-file", filepath.Join(directory, "cdn-edge-agent.service"),
		"--service-sha256", manifest.Instruction.AgentService.SHA256,
		"--updater-service-file", filepath.Join(directory, "cdn-edge-updater@.service"),
		"--updater-service-sha256", manifest.Instruction.UpdaterService.SHA256,
		"--readiness-file", filepath.Join(directory, "ready"),
	}
	command := exec.Command("/bin/bash", arguments...)
	command.Stdout = output
	command.Stderr = output
	command.Env = os.Environ()
	runErr := command.Run()
	installedPath := rootPath("/opt/cdn-edge/bin/cdn-edge-agent")
	installedSHA256, digestErr := fileSHA256(installedPath)
	report := domain.NodeUpgradeReport{TaskID: taskID, Status: domain.NodeUpgradeSucceeded, Detail: "edge online upgrade completed", InstalledSHA256: installedSHA256}
	if runErr != nil || digestErr != nil || !strings.EqualFold(installedSHA256, manifest.Instruction.Binary.SHA256) {
		report.Status = domain.NodeUpgradeFailed
		report.ErrorCode = "installer_failed"
		report.InstalledSHA256 = installedSHA256
		report.Detail = strings.TrimSpace(buffer.String())
		if report.Detail == "" {
			report.Detail = fmt.Sprintf("edge installer failed: %v", runErr)
		}
	}
	if len(report.Detail) > 4096 {
		report.Detail = report.Detail[len(report.Detail)-4096:]
	}
	if err := writeLocalUpgradeReport(directory, report); err != nil {
		return err
	}
	if report.Status == domain.NodeUpgradeFailed && runErr == nil {
		return errors.New(report.Detail)
	}
	return runErr
}

func rootPath(path string) string {
	prefix := strings.TrimRight(os.Getenv("CDN_EDGE_INSTALL_ROOT"), "/")
	return prefix + path
}

type boundedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = b.buffer.Write(value)
	}
	return original, nil
}

func (b *boundedBuffer) String() string { return b.buffer.String() }
