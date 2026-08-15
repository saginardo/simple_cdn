package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"simple_cdn/internal/domain"
)

func (a *Agent) runWireGuardLoop(ctx context.Context) {
	interval := min(a.Config.PollInterval, 10*time.Second)
	if interval <= 0 {
		interval = 10 * time.Second
	}
	for {
		a.setComponentFailure("wireguard", "synchronize WireGuard tunnels", a.runWireGuardRound(ctx))
		if !waitContext(ctx, interval) {
			return
		}
	}
}

func (a *Agent) runWireGuardPerformanceLoop(ctx context.Context) {
	interval := min(a.Config.PollInterval, 10*time.Second)
	if interval <= 0 {
		interval = 10 * time.Second
	}
	for {
		a.setComponentFailure("wireguard_performance", "run WireGuard performance test", a.runWireGuardPerformanceRound(ctx))
		if !waitContext(ctx, interval) {
			return
		}
	}
}

func (a *Agent) runWireGuardRound(ctx context.Context) error {
	a.setWireGuardAppliedRevision("")
	configs, revision, err := a.pullWireGuardConfigs(ctx)
	if err != nil {
		return err
	}
	reports, reconcileErr := a.wireGuard.Reconcile(ctx, configs)
	reportErr := a.reportWireGuardStatus(ctx, reports)
	roundErr := errors.Join(reconcileErr, reportErr)
	if roundErr == nil {
		a.setWireGuardAppliedRevision(revision)
	}
	return roundErr
}

func (a *Agent) runWireGuardPerformanceRound(ctx context.Context) error {
	_, performanceAvailable := a.wireGuard.Available()
	if !performanceAvailable {
		return nil
	}
	if a.appliedWireGuardRevision() == "" {
		return nil
	}
	job, err := a.claimWireGuardPerformanceTest(ctx)
	if err != nil || job == nil {
		return err
	}
	result, performanceErr := a.wireGuard.RunPerformance(ctx, job.Test, job.Config)
	detail := ""
	if performanceErr != nil {
		detail = wireGuardPerformanceError(performanceErr)
	}
	if err := a.reportWireGuardPerformance(ctx, job.Test.ID, result, detail); err != nil {
		return errors.Join(performanceErr, err)
	}
	return performanceErr
}

func (a *Agent) pullWireGuardConfigs(ctx context.Context) ([]domain.WireGuardEdgeConfig, string, error) {
	cached, revision, loaded := a.cachedWireGuardConfigs()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.Config.ControlURL+"/api/edge/v1/wireguard/config", nil)
	if err != nil {
		return nil, "", err
	}
	if revision != "" {
		request.Header.Set("If-None-Match", `"`+revision+`"`)
	}
	response, err := a.client().Do(request)
	if err != nil {
		return nil, "", fmt.Errorf("pull WireGuard configuration: %w", err)
	}
	defer drainAndClose(response.Body)
	responseRevision := revisionFromETag(response.Header.Get("ETag"))
	if response.StatusCode == http.StatusNotModified {
		if !loaded {
			return nil, "", errors.New("control returned unchanged WireGuard configuration before a local snapshot existed")
		}
		return cached, revision, nil
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, "", fmt.Errorf("pull WireGuard configuration: %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Revision string                       `json:"revision"`
		Tunnels  []domain.WireGuardEdgeConfig `json:"tunnels"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, "", fmt.Errorf("decode WireGuard configuration: %w", err)
	}
	if !validDigest(payload.Revision) || responseRevision != "" && responseRevision != payload.Revision {
		return nil, "", errors.New("control returned an invalid WireGuard configuration revision")
	}
	if len(payload.Tunnels) > domain.MaxWireGuardPeersPerTunnel {
		return nil, "", errors.New("control returned too many WireGuard tunnels")
	}
	a.cacheWireGuardConfigs(payload.Tunnels, payload.Revision)
	return append([]domain.WireGuardEdgeConfig(nil), payload.Tunnels...), payload.Revision, nil
}

func (a *Agent) cachedWireGuardConfigs() ([]domain.WireGuardEdgeConfig, string, bool) {
	a.wireGuardMu.RLock()
	defer a.wireGuardMu.RUnlock()
	return append([]domain.WireGuardEdgeConfig(nil), a.wireGuardConfigs...), a.wireGuardRevision, a.wireGuardLoaded
}

func (a *Agent) cacheWireGuardConfigs(configs []domain.WireGuardEdgeConfig, revision string) {
	a.wireGuardMu.Lock()
	a.wireGuardConfigs = append([]domain.WireGuardEdgeConfig(nil), configs...)
	a.wireGuardRevision = revision
	a.wireGuardLoaded = true
	a.wireGuardMu.Unlock()
}

func (a *Agent) setWireGuardAppliedRevision(revision string) {
	a.wireGuardMu.Lock()
	a.wireGuardAppliedRevision = revision
	a.wireGuardMu.Unlock()
}

func (a *Agent) appliedWireGuardRevision() string {
	a.wireGuardMu.RLock()
	defer a.wireGuardMu.RUnlock()
	return a.wireGuardAppliedRevision
}

func (a *Agent) reportWireGuardStatus(ctx context.Context, reports []domain.WireGuardPeerReport) error {
	payload, err := json.Marshal(struct {
		Reports []domain.WireGuardPeerReport `json:"reports"`
	}{Reports: reports})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Config.ControlURL+"/api/edge/v1/wireguard/status", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client().Do(request)
	if err != nil {
		return fmt.Errorf("report WireGuard status: %w", err)
	}
	defer drainAndClose(response.Body)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("report WireGuard status: %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

type wireGuardPerformanceJob struct {
	Test   domain.WireGuardPerformanceTest `json:"test"`
	Config domain.WireGuardEdgeConfig      `json:"config"`
}

func (a *Agent) claimWireGuardPerformanceTest(ctx context.Context) (*wireGuardPerformanceJob, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.Config.ControlURL+"/api/edge/v1/wireguard/performance-test", nil)
	if err != nil {
		return nil, err
	}
	response, err := a.client().Do(request)
	if err != nil {
		return nil, fmt.Errorf("claim WireGuard performance test: %w", err)
	}
	defer drainAndClose(response.Body)
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("claim WireGuard performance test: %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var job wireGuardPerformanceJob
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&job); err != nil {
		return nil, fmt.Errorf("decode WireGuard performance test: %w", err)
	}
	if job.Test.ID == "" || job.Test.TunnelID != job.Config.TunnelID || job.Test.Status != domain.WireGuardPerformanceRunning {
		return nil, errors.New("control returned an invalid WireGuard performance test")
	}
	return &job, nil
}

func (a *Agent) reportWireGuardPerformance(ctx context.Context, testID string, result *domain.WireGuardPerformanceResult, detail string) error {
	payload, err := json.Marshal(struct {
		Result *domain.WireGuardPerformanceResult `json:"result"`
		Error  string                             `json:"error"`
	}{Result: result, Error: detail})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Config.ControlURL+"/api/edge/v1/wireguard/performance-tests/"+testID, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client().Do(request)
	if err != nil {
		return fmt.Errorf("report WireGuard performance test: %w", err)
	}
	defer drainAndClose(response.Body)
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("report WireGuard performance test: %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func wireGuardPerformanceError(err error) string {
	detail := strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			return ' '
		}
		return character
	}, err.Error())
	detail = strings.Join(strings.Fields(detail), " ")
	if len(detail) > 1000 {
		detail = detail[:1000]
	}
	return detail
}
