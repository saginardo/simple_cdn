package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"simple_cdn/internal/domain"
)

const (
	defaultMonitoringTimeout  = 2 * time.Second
	defaultMonitoringAttempts = 3
	defaultMonitoringWorkers  = 16
)

type MonitoringDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

func configureMonitoring(config *Config) {
	if config.MonitoringTimeout <= 0 {
		config.MonitoringTimeout = defaultMonitoringTimeout
	}
	if config.MonitoringAttempts <= 0 || config.MonitoringAttempts > 10 {
		config.MonitoringAttempts = defaultMonitoringAttempts
	}
	if config.MonitoringWorkers <= 0 || config.MonitoringWorkers > 64 {
		config.MonitoringWorkers = defaultMonitoringWorkers
	}
	if config.MonitoringDialer == nil {
		config.MonitoringDialer = &net.Dialer{Timeout: config.MonitoringTimeout}
	}
}

func (a *Agent) Monitor(ctx context.Context) error {
	targets, err := a.pullMonitoringTargets(ctx)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}
	results := a.probeMonitoringTargets(ctx, targets)
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	payload, err := json.Marshal(struct {
		Results []domain.MonitoringProbeResult `json:"results"`
	}{Results: results})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Config.ControlURL+"/api/edge/v1/monitoring-results", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client().Do(request)
	if err != nil {
		return fmt.Errorf("report results: %w", err)
	}
	defer drainAndClose(response.Body)
	if response.StatusCode == http.StatusConflict {
		return nil
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("report results: %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func (a *Agent) pullMonitoringTargets(ctx context.Context) ([]domain.MonitoringTarget, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.Config.ControlURL+"/api/edge/v1/monitoring-targets", nil)
	if err != nil {
		return nil, err
	}
	response, err := a.client().Do(request)
	if err != nil {
		return nil, fmt.Errorf("pull targets: %w", err)
	}
	defer drainAndClose(response.Body)
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("pull targets: %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var targets []domain.MonitoringTarget
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&targets); err != nil {
		return nil, fmt.Errorf("decode targets: %w", err)
	}
	if len(targets) > domain.MaxMonitoringTargets {
		return nil, errors.New("too many monitoring targets")
	}
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		normalized, err := domain.NormalizeMonitoringAddress(target.Address)
		if err != nil || normalized != target.Address || target.ID == "" || seen[target.ID] || !target.Enabled {
			return nil, errors.New("control returned an invalid monitoring target")
		}
		seen[target.ID] = true
	}
	return targets, nil
}

type monitoringAttempt struct {
	latencyMS float64
	err       error
}

func (a *Agent) probeMonitoringTargets(ctx context.Context, targets []domain.MonitoringTarget) []domain.MonitoringProbeResult {
	attempts := make([][]monitoringAttempt, len(targets))
	for index := range attempts {
		attempts[index] = make([]monitoringAttempt, a.Config.MonitoringAttempts)
	}
	type job struct {
		target  int
		attempt int
	}
	jobs := make(chan job)
	workerCount := min(a.Config.MonitoringWorkers, len(targets)*a.Config.MonitoringAttempts)
	var group sync.WaitGroup
	group.Add(workerCount)
	for range workerCount {
		go func() {
			defer group.Done()
			for work := range jobs {
				attemptCtx, cancel := context.WithTimeout(ctx, a.Config.MonitoringTimeout)
				started := time.Now()
				connection, err := a.Config.MonitoringDialer.DialContext(attemptCtx, "tcp", targets[work.target].Address)
				latencyMS := time.Since(started).Seconds() * 1000
				cancel()
				if err == nil && connection == nil {
					err = errors.New("TCP dial returned no connection")
				}
				if connection != nil {
					_ = connection.Close()
				}
				if err == nil && latencyMS < 0.01 {
					latencyMS = 0.01
				}
				attempts[work.target][work.attempt] = monitoringAttempt{latencyMS: latencyMS, err: err}
			}
		}()
	}
	for targetIndex := range targets {
		for attemptIndex := 0; attemptIndex < a.Config.MonitoringAttempts; attemptIndex++ {
			select {
			case jobs <- job{target: targetIndex, attempt: attemptIndex}:
			case <-ctx.Done():
				close(jobs)
				group.Wait()
				return buildMonitoringResults(targets, attempts)
			}
		}
	}
	close(jobs)
	group.Wait()
	return buildMonitoringResults(targets, attempts)
}

func buildMonitoringResults(targets []domain.MonitoringTarget, attempts [][]monitoringAttempt) []domain.MonitoringProbeResult {
	checkedAt := time.Now().UTC()
	results := make([]domain.MonitoringProbeResult, len(targets))
	for targetIndex, target := range targets {
		result := domain.MonitoringProbeResult{TargetID: target.ID, Attempts: len(attempts[targetIndex]), CheckedAt: checkedAt}
		latencyTotal := 0.0
		for _, attempt := range attempts[targetIndex] {
			if attempt.err != nil {
				result.Error = cleanMonitoringError(attempt.err.Error())
				continue
			}
			result.SuccessfulAttempts++
			latencyTotal += attempt.latencyMS
		}
		if result.SuccessfulAttempts > 0 {
			result.AverageLatencyMS = latencyTotal / float64(result.SuccessfulAttempts)
		}
		results[targetIndex] = result
	}
	return results
}

func cleanMonitoringError(value string) string {
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return -1
		}
		return character
	}, strings.TrimSpace(value))
	if len(value) > 512 {
		value = value[:512]
	}
	return value
}
