package edge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"simple_cdn/internal/domain"
)

const (
	machineStatusPolicyRetryInterval            = 3 * time.Second
	machineStatusPolicyInactiveRetryInterval    = 15 * time.Second
	machineStatusPolicyUnsupportedRetryInterval = time.Minute
	machineStatusPolicyFallbackDelay            = time.Minute
	machineStatusPolicyReadTimeout              = 45 * time.Second
	machineNetworkReportTimeout                 = 5 * time.Second
)

var (
	errMachineStatusPolicyUnsupported = errors.New("machine status policy is unsupported")
	errMachineStatusPolicyInactive    = errors.New("machine status policy stream is inactive")
	errMachineStatusPolicyInvalid     = errors.New("machine status policy is invalid")
)

type machineStatusPolicyScan struct {
	line string
	err  error
	done bool
}

func (a *Agent) runMachineStatusPolicyLoop(ctx context.Context) {
	a.runMachineStatusPolicyLoopWithConfig(
		ctx,
		machineStatusPolicyRetryInterval,
		machineStatusPolicyUnsupportedRetryInterval,
		machineStatusPolicyInactiveRetryInterval,
		machineStatusPolicyFallbackDelay,
		machineStatusPolicyReadTimeout,
	)
}

func (a *Agent) runMachineStatusPolicyLoopWithIntervals(
	ctx context.Context,
	retryInterval time.Duration,
	unsupportedRetryInterval time.Duration,
	fallbackDelay time.Duration,
) {
	a.runMachineStatusPolicyLoopWithConfig(
		ctx,
		retryInterval,
		unsupportedRetryInterval,
		machineStatusPolicyInactiveRetryInterval,
		fallbackDelay,
		machineStatusPolicyReadTimeout,
	)
}

func (a *Agent) runMachineStatusPolicyLoopWithConfig(
	ctx context.Context,
	retryInterval time.Duration,
	unsupportedRetryInterval time.Duration,
	inactiveRetryInterval time.Duration,
	fallbackDelay time.Duration,
	readTimeout time.Duration,
) {
	a.useLegacyMachineStatusPolicy()
	defer a.useLegacyMachineStatusPolicy()
	usingLegacyPolicy := true
	var transientFailureSince time.Time
	for {
		receivedPolicy, err := a.streamMachineStatusPolicySession(ctx, readTimeout)
		if receivedPolicy {
			usingLegacyPolicy = false
			transientFailureSince = time.Time{}
		}
		if ctx.Err() != nil {
			a.setComponentFailure("machine_status_policy", "stream machine status policy", nil)
			return
		}

		nextRetryInterval := retryInterval
		switch {
		case errors.Is(err, errMachineStatusPolicyUnsupported):
			if !usingLegacyPolicy {
				a.useLegacyMachineStatusPolicy()
			}
			usingLegacyPolicy = true
			transientFailureSince = time.Time{}
			a.setComponentFailure("machine_status_policy", "stream machine status policy", nil)
			nextRetryInterval = unsupportedRetryInterval
		case errors.Is(err, errMachineStatusPolicyInvalid):
			if !usingLegacyPolicy {
				a.useLegacyMachineStatusPolicy()
			}
			usingLegacyPolicy = true
			transientFailureSince = time.Time{}
			a.setComponentFailure("machine_status_policy", "stream machine status policy", err)
		default:
			if isMachineStatusPolicyInactiveError(err) {
				nextRetryInterval = inactiveRetryInterval
			}
			if transientFailureSince.IsZero() {
				transientFailureSince = time.Now()
			}
			if fallbackDelay <= 0 || time.Since(transientFailureSince) >= fallbackDelay {
				if !usingLegacyPolicy {
					a.useLegacyMachineStatusPolicy()
				}
				usingLegacyPolicy = true
				a.setComponentFailure("machine_status_policy", "stream machine status policy", err)
			}
		}
		if !waitContext(ctx, nextRetryInterval) {
			a.setComponentFailure("machine_status_policy", "stream machine status policy", nil)
			return
		}
	}
}

func isMachineStatusPolicyInactiveError(err error) bool {
	if errors.Is(err, errMachineStatusPolicyInactive) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var timeoutError net.Error
	return errors.As(err, &timeoutError) && timeoutError.Timeout()
}

func (a *Agent) streamMachineStatusPolicy(ctx context.Context) error {
	return a.streamMachineStatusPolicyWithTimeout(ctx, machineStatusPolicyReadTimeout)
}

func (a *Agent) streamMachineStatusPolicyWithTimeout(ctx context.Context, readTimeout time.Duration) error {
	_, err := a.streamMachineStatusPolicySession(ctx, readTimeout)
	return err
}

func (a *Agent) streamMachineStatusPolicySession(ctx context.Context, readTimeout time.Duration) (bool, error) {
	receivedPolicy := false
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.Config.ControlURL+"/api/edge/v1/machine-status/policy/events", nil)
	if err != nil {
		return receivedPolicy, err
	}
	client := *a.client()
	client.Timeout = 0
	response, err := client.Do(request)
	if err != nil {
		return receivedPolicy, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusGone {
			return receivedPolicy, fmt.Errorf("%w: %s", errMachineStatusPolicyUnsupported, response.Status)
		}
		return receivedPolicy, fmt.Errorf("machine status policy: %s: %s", response.Status, strings.TrimSpace(string(body)))
	}

	if readTimeout <= 0 {
		readTimeout = machineStatusPolicyReadTimeout
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	scans := make(chan machineStatusPolicyScan, 1)
	go func() {
		for scanner.Scan() {
			select {
			case scans <- machineStatusPolicyScan{line: scanner.Text()}:
			case <-streamCtx.Done():
				return
			}
		}
		select {
		case scans <- machineStatusPolicyScan{err: scanner.Err(), done: true}:
		case <-streamCtx.Done():
		}
	}()

	readTimer := time.NewTimer(readTimeout)
	defer readTimer.Stop()
	for {
		select {
		case <-ctx.Done():
			return receivedPolicy, ctx.Err()
		case <-readTimer.C:
			return receivedPolicy, fmt.Errorf("%w: inactive for %s", errMachineStatusPolicyInactive, readTimeout)
		case scan := <-scans:
			if scan.done {
				if scan.err != nil {
					if errors.Is(scan.err, io.ErrUnexpectedEOF) {
						return receivedPolicy, fmt.Errorf("%w: %w", errMachineStatusPolicyInactive, scan.err)
					}
					return receivedPolicy, scan.err
				}
				return receivedPolicy, fmt.Errorf("%w: %w", errMachineStatusPolicyInactive, io.ErrUnexpectedEOF)
			}
			resetTimer(readTimer, readTimeout)
			line := scan.line
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var policy domain.MachineStatusSamplingPolicy
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &policy); err != nil {
				return receivedPolicy, fmt.Errorf("%w: decode policy: %v", errMachineStatusPolicyInvalid, err)
			}
			if policy.HostIntervalSeconds == 0 || policy.OriginIntervalSeconds == 0 {
				return receivedPolicy, fmt.Errorf("%w: policy cadence fields are missing", errMachineStatusPolicyUnsupported)
			}
			if !domain.ValidMachineStatusSamplingPolicy(policy) {
				return receivedPolicy, errMachineStatusPolicyInvalid
			}
			a.applyMachineStatusPolicy(policy)
			receivedPolicy = true
			a.setComponentFailure("machine_status_policy", "stream machine status policy", nil)
		}
	}
}

func (a *Agent) applyMachineStatusPolicy(policy domain.MachineStatusSamplingPolicy) {
	a.setMachineStatusInterval(time.Duration(policy.HostIntervalSeconds) * time.Second)
	a.setMachineNetworkInterval(time.Duration(policy.NetworkIntervalSeconds) * time.Second)
	a.setMachineOriginInterval(time.Duration(policy.OriginIntervalSeconds) * time.Second)
}

func (a *Agent) useLegacyMachineStatusPolicy() {
	interval := a.Config.MachineStatusInterval
	if interval <= 0 {
		interval = time.Duration(domain.LegacyMachineStatusIntervalSeconds) * time.Second
	}
	a.setMachineStatusInterval(interval)
	a.setMachineNetworkInterval(0)
	a.setMachineOriginInterval(0)
}

func (a *Agent) setMachineStatusInterval(interval time.Duration) {
	setLatestMachineStatusInterval(a.machineStatusIntervals, interval)
}

func (a *Agent) setMachineNetworkInterval(interval time.Duration) {
	setLatestMachineStatusInterval(a.machineNetworkIntervals, interval)
}

func (a *Agent) setMachineOriginInterval(interval time.Duration) {
	setLatestMachineStatusInterval(a.machineOriginIntervals, interval)
}

func setLatestMachineStatusInterval(updates chan time.Duration, interval time.Duration) {
	if updates == nil {
		return
	}
	select {
	case updates <- interval:
	default:
		select {
		case <-updates:
		default:
		}
		select {
		case updates <- interval:
		default:
		}
	}
}

func (a *Agent) runMachineNetworkLoop(ctx context.Context) {
	var interval time.Duration
	var timer *time.Timer
	var timerChannel <-chan time.Time
	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timerChannel = nil
	}
	schedule := func(delay time.Duration) {
		if delay < 0 {
			delay = 0
		}
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			timer.Reset(delay)
		}
		timerChannel = timer.C
	}
	defer func() {
		stopTimer()
		a.setComponentFailure("machine_network", "report machine network status", nil)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case next := <-a.machineNetworkIntervals:
			if next == interval {
				continue
			}
			interval = next
			stopTimer()
			if interval <= 0 {
				a.setComponentFailure("machine_network", "report machine network status", nil)
				continue
			}
			started := time.Now()
			err := a.primeMachineNetworkStatus()
			a.setComponentFailure("machine_network", "prime machine network status", err)
			schedule(interval - time.Since(started))
		case <-timerChannel:
			timerChannel = nil
			started := time.Now()
			err := a.runMachineNetworkRound(ctx)
			a.setComponentFailure("machine_network", "report machine network status", err)
			if interval > 0 {
				schedule(interval - time.Since(started))
			}
		}
	}
}

func (a *Agent) primeMachineNetworkStatus() error {
	if a.machineNetwork == nil {
		return errors.New("machine network collector is unavailable")
	}
	report, err := a.machineNetwork.CollectNetwork()
	if err != nil {
		return err
	}
	if report == nil {
		return errors.New("machine network collector returned no status")
	}
	return nil
}

func (a *Agent) runMachineNetworkRound(ctx context.Context) error {
	if a.machineNetwork == nil {
		return errors.New("machine network collector is unavailable")
	}
	report, err := a.machineNetwork.CollectNetwork()
	if err != nil {
		return err
	}
	if report == nil {
		return errors.New("machine network collector returned no status")
	}
	if report.SampleSeconds <= 0 {
		return nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, machineNetworkReportTimeout)
	defer cancel()
	return a.ReportMachineNetworkStatus(requestCtx, *report)
}

func (a *Agent) ReportMachineNetworkStatus(ctx context.Context, report domain.MachineNetworkStatus) error {
	if !domain.ValidMachineNetworkStatus(report) || report.SampleSeconds <= 0 {
		return errors.New("machine network status is invalid")
	}
	payload, err := json.Marshal(report)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Config.ControlURL+"/api/edge/v1/machine-network", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client().Do(request)
	if err != nil {
		return err
	}
	defer drainAndClose(response.Body)
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("machine network status: %s", response.Status)
	}
	return nil
}
