package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"simple_cdn/internal/domain"
)

const machineOriginReportTimeout = 5 * time.Second

func (a *Agent) runMachineOriginStatusLoop(ctx context.Context) {
	var interval time.Duration
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	var timerChannel <-chan time.Time
	defer func() {
		timer.Stop()
		a.setComponentFailure("machine_origin", "report machine origin status", nil)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case next := <-a.machineOriginIntervals:
			if next == interval {
				continue
			}
			interval = next
			if interval <= 0 {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timerChannel = nil
				a.setComponentFailure("machine_origin", "report machine origin status", nil)
				continue
			}
			started := time.Now()
			a.runMachineOriginStatusRound(ctx)
			resetTimer(timer, interval-time.Since(started))
			timerChannel = timer.C
		case <-timerChannel:
			started := time.Now()
			a.runMachineOriginStatusRound(ctx)
			if interval > 0 {
				resetTimer(timer, interval-time.Since(started))
				timerChannel = timer.C
			}
		}
	}
}

func (a *Agent) runMachineOriginStatusRound(ctx context.Context) {
	report := domain.MachineOriginStatus{OriginProbes: a.originProbeStatuses()}
	requestCtx, cancel := context.WithTimeout(ctx, machineOriginReportTimeout)
	defer cancel()
	a.attachOriginConnectionCounts(requestCtx, report.OriginProbes)
	report.CollectedAt = time.Now().UTC()
	err := a.ReportMachineOriginStatus(requestCtx, report)
	a.setComponentFailure("machine_origin", "report machine origin status", err)
}

func (a *Agent) ReportMachineOriginStatus(ctx context.Context, report domain.MachineOriginStatus) error {
	if !domain.ValidMachineOriginStatus(report) {
		return errors.New("machine origin status is invalid")
	}
	payload, err := json.Marshal(report)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Config.ControlURL+"/api/edge/v1/machine-origin", bytes.NewReader(payload))
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
		return fmt.Errorf("machine origin status: %s", response.Status)
	}
	return nil
}
