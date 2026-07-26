package edge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

type machineStatusReporterFunc func() (*domain.MachineStatus, error)

func (function machineStatusReporterFunc) Collect() (*domain.MachineStatus, error) {
	return function()
}

func TestMachineStatusCollectorReportsLinuxHostAndIntervalRates(t *testing.T) {
	now := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	files := map[string]string{
		"/etc/os-release":     "ID=debian\nNAME=\"Debian GNU/Linux\"\nVERSION_ID=\"13\"\n",
		"/etc/debian_version": "13.5\n",
		"/proc/uptime":        "90061.50 123.0\n",
		"/proc/loadavg":       "0.50 0.75 1.25 1/100 42\n",
		"/proc/stat":          "cpu 100 0 50 850 0 0 0 0 0 0\n",
		"/proc/meminfo":       "MemTotal: 8388608 kB\nMemAvailable: 3145728 kB\n",
		"/proc/net/route":     "Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT\neth0 00000000 0100000A 0003 0 0 100 00000000 0 0 0\n",
		"/proc/net/dev":       "Inter-| Receive | Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\neth0: 1000 1 0 0 0 0 0 0 2000 1 0 0 0 0 0 0\nlo: 10 1 0 0 0 0 0 0 10 1 0 0 0 0 0 0\n",
	}
	collector := &machineStatusCollector{
		readFile: func(path string) ([]byte, error) {
			value, found := files[path]
			if !found {
				return nil, errors.New("missing fixture " + path)
			}
			return []byte(value), nil
		},
		statFilesystem: func(string) (int64, int64, error) { return 40 << 30, 100 << 30, nil },
		now:            func() time.Time { return now },
		logicalCPUs:    func() int { return 8 },
	}

	first, err := collector.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if first.Distribution != "Debian GNU/Linux" || first.Version != "13.5" || first.UptimeSeconds != 90061 || first.CPULogicalCores != 8 {
		t.Fatalf("unexpected first machine report: %#v", first)
	}
	if first.MemoryUsedBytes != 5<<30 || first.MemoryTotalBytes != 8<<30 || first.DiskUsedBytes != 40<<30 || first.DiskTotalBytes != 100<<30 {
		t.Fatalf("unexpected capacity report: %#v", first)
	}
	if first.SampleSeconds != 0 || first.CPUUsagePercent != 0 || first.NetworkRXBytesPerSec != 0 || first.NetworkTXBytesPerSec != 0 {
		t.Fatalf("first sample unexpectedly included interval rates: %#v", first)
	}

	now = now.Add(30 * time.Second)
	files["/proc/uptime"] = "90091.50 123.0\n"
	files["/proc/stat"] = "cpu 130 0 70 900 0 0 0 0 0 0\n"
	files["/proc/net/dev"] = "eth0: 4000 1 0 0 0 0 0 0 8000 1 0 0 0 0 0 0\nlo: 20 1 0 0 0 0 0 0 20 1 0 0 0 0 0 0\n"
	second, err := collector.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if second.SampleSeconds != 30 || second.CPUUsagePercent != 50 || second.NetworkInterface != "eth0" || second.NetworkRXBytesPerSec != 100 || second.NetworkTXBytesPerSec != 200 {
		t.Fatalf("unexpected interval report: %#v", second)
	}
	if !domain.ValidMachineStatus(*second) {
		t.Fatalf("collector produced invalid report: %#v", second)
	}
}

func TestMachineStatusRoundReportsStatusAndHeartbeatCarriesLatestSnapshot(t *testing.T) {
	report := &domain.MachineStatus{
		Distribution: "Debian GNU/Linux", Version: "13.5", UptimeSeconds: 60,
		Load1: 0.1, Load5: 0.2, Load15: 0.3, CPUUsagePercent: 25, CPULogicalCores: 4,
		MemoryUsedBytes: 2 << 30, MemoryTotalBytes: 4 << 30,
		DiskUsedBytes: 10 << 30, DiskTotalBytes: 50 << 30,
		NetworkInterface: "eth0", NetworkRXBytesPerSec: 1000, NetworkTXBytesPerSec: 500,
		SampleSeconds: 30, CollectedAt: time.Now().UTC(),
	}
	var heartbeat struct {
		Capabilities []string              `json:"capabilities"`
		Machine      *domain.MachineStatus `json:"machine_status"`
	}
	var reported *domain.MachineStatus
	client := &http.Client{Transport: upgradeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/edge/v1/machine-status":
			var status domain.MachineStatus
			if err := json.NewDecoder(request.Body).Decode(&status); err != nil {
				return nil, err
			}
			reported = &status
			return &http.Response{
				StatusCode: http.StatusAccepted, Status: "202 Accepted", Header: make(http.Header), Body: http.NoBody,
			}, nil
		case "/api/edge/v1/heartbeat":
			if err := json.NewDecoder(request.Body).Decode(&heartbeat); err != nil {
				return nil, err
			}
			return &http.Response{
				StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		default:
			return nil, errors.New("unexpected request path " + request.URL.Path)
		}
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: t.TempDir(), CertificateDir: t.TempDir(),
		AgentSHA256: strings.Repeat("a", 64), HTTPClient: client, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.machineStatus = machineStatusReporterFunc(func() (*domain.MachineStatus, error) {
		copy := *report
		return &copy, nil
	})
	agent.runMachineStatusRound(t.Context())
	if reported == nil || reported.Version != "13.5" || reported.NetworkRXBytesPerSec != 1000 {
		t.Fatalf("reported machine status = %#v", reported)
	}
	if err := agent.Heartbeat(t.Context(), 1, "", nil); err != nil {
		t.Fatal(err)
	}
	if heartbeat.Machine == nil || heartbeat.Machine.Version != "13.5" || heartbeat.Machine.NetworkRXBytesPerSec != 1000 {
		t.Fatalf("heartbeat machine status = %#v", heartbeat.Machine)
	}
	found, foundStream := false, false
	for _, capability := range heartbeat.Capabilities {
		found = found || capability == domain.EdgeCapabilityMachineStatus
		foundStream = foundStream || capability == domain.EdgeCapabilityMachineStatusStream
	}
	if !found || !foundStream {
		t.Fatalf("heartbeat capabilities = %#v", heartbeat.Capabilities)
	}
	heartbeat.Machine = nil
	agent.machineStatus = machineStatusReporterFunc(func() (*domain.MachineStatus, error) {
		return nil, errors.New("procfs unavailable")
	})
	agent.runMachineStatusRound(t.Context())
	if err := agent.Heartbeat(t.Context(), 1, "", nil); err != nil {
		t.Fatalf("optional machine collection blocked heartbeat: %v", err)
	}
	if heartbeat.Machine == nil || heartbeat.Machine.CollectedAt != report.CollectedAt {
		t.Fatalf("failed collection discarded latest machine status: %#v", heartbeat.Machine)
	}
}

func TestMachineStatusDefaultsToFiveSecondSampling(t *testing.T) {
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: t.TempDir(), CertificateDir: t.TempDir(),
		AgentSHA256: strings.Repeat("c", 64), HTTPClient: &http.Client{Transport: upgradeRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusAccepted, Status: "202 Accepted", Header: make(http.Header), Body: http.NoBody}, nil
		})}, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.Config.MachineStatusInterval != 5*time.Second {
		t.Fatalf("machine status interval = %s", agent.Config.MachineStatusInterval)
	}
}

func TestMachineStatusLoopUsesConfiguredSamplingInterval(t *testing.T) {
	reports := make(chan time.Time, 4)
	client := &http.Client{Transport: upgradeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/edge/v1/machine-status" {
			return nil, errors.New("unexpected request path " + request.URL.Path)
		}
		reports <- time.Now()
		return &http.Response{StatusCode: http.StatusAccepted, Status: "202 Accepted", Header: make(http.Header), Body: http.NoBody}, nil
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: t.TempDir(), CertificateDir: t.TempDir(),
		AgentSHA256: strings.Repeat("d", 64), HTTPClient: client, Runner: &fakeRunner{}, MachineStatusInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.machineStatus = machineStatusReporterFunc(func() (*domain.MachineStatus, error) {
		return &domain.MachineStatus{
			Distribution: "Debian", Version: "13", UptimeSeconds: 60,
			Load1: 0.1, Load5: 0.2, Load15: 0.3, CPUUsagePercent: 25, CPULogicalCores: 2,
			MemoryUsedBytes: 1, MemoryTotalBytes: 2, DiskUsedBytes: 1, DiskTotalBytes: 2,
			NetworkInterface: "eth0", SampleSeconds: 5, CollectedAt: time.Now().UTC(),
		}, nil
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		agent.runMachineStatusLoop(ctx)
		close(done)
	}()
	var first, third time.Time
	for index := range 3 {
		select {
		case reportedAt := <-reports:
			if index == 0 {
				first = reportedAt
			}
			third = reportedAt
		case <-time.After(time.Second):
			cancel()
			t.Fatal("machine status loop did not report three samples")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("machine status loop did not stop after cancellation")
	}
	if elapsed := third.Sub(first); elapsed < 15*time.Millisecond {
		t.Fatalf("three machine status reports completed in %s, want configured spacing", elapsed)
	}
}
