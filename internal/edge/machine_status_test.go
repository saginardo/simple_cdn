package edge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

type machineStatusReporterFunc func() (*domain.MachineStatus, error)

func (function machineStatusReporterFunc) Collect() (*domain.MachineStatus, error) {
	return function()
}

type machineNetworkStatusReporterFunc func() (*domain.MachineNetworkStatus, error)

func (function machineNetworkStatusReporterFunc) CollectNetwork() (*domain.MachineNetworkStatus, error) {
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
		nginxStatus: func() (*domain.NginxRuntimeStatus, error) {
			return &domain.NginxRuntimeStatus{
				ActiveConnections: 7, AcceptedConnections: 20, HandledConnections: 20,
				Requests: 35, Reading: 1, Writing: 1, Waiting: 2,
			}, nil
		},
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
	if first.Nginx == nil || first.Nginx.Requests != 35 || first.Nginx.ActiveConnections != 7 {
		t.Fatalf("unexpected Nginx status: %#v", first.Nginx)
	}
	networkBaseline, err := collector.CollectNetwork()
	if err != nil {
		t.Fatal(err)
	}
	if networkBaseline.SampleSeconds != 0 {
		t.Fatalf("network baseline unexpectedly included a rate: %#v", networkBaseline)
	}
	now = now.Add(time.Second)
	files["/proc/net/dev"] = "eth0: 1100 1 0 0 0 0 0 0 2200 1 0 0 0 0 0 0\n"
	network, err := collector.CollectNetwork()
	if err != nil {
		t.Fatal(err)
	}
	if network.SampleSeconds != 1 || network.NetworkRXBytesPerSec != 100 || network.NetworkTXBytesPerSec != 200 {
		t.Fatalf("unexpected one-second network report: %#v", network)
	}

	now = now.Add(29 * time.Second)
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

func TestParseNginxStubStatus(t *testing.T) {
	status, err := parseNginxStubStatus([]byte("Active connections: 3\nserver accepts handled requests\n 10 10 14\nReading: 1 Writing: 1 Waiting: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if status.ActiveConnections != 3 || status.AcceptedConnections != 10 || status.Requests != 14 || status.Waiting != 1 {
		t.Fatalf("parsed status = %#v", status)
	}
	status, err = parseNginxStubStatus([]byte("Active connections: 6\nserver accepts handled requests\n 20 20 30\nReading: 0 Writing: 1 Waiting: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if status.ActiveConnections != 6 || status.Reading+status.Writing+status.Waiting != 3 {
		t.Fatalf("parsed mixed HTTP and stream status = %#v", status)
	}
	for _, invalid := range []string{
		"Active connections: 3 trailing\nserver accepts handled requests\n10 10 14\nReading: 1 Writing: 1 Waiting: 1\n",
		"Active connections: 3\nserver accepts handled requests\n10 11 14\nReading: 1 Writing: 1 Waiting: 1\n",
		"Active connections: 2\nserver accepts handled requests\n10 10 14\nReading: 1 Writing: 1 Waiting: 1\n",
	} {
		if _, err := parseNginxStubStatus([]byte(invalid)); err == nil {
			t.Fatalf("invalid stub_status response was accepted: %q", invalid)
		}
	}
}

func TestNginxStatusReaderUsesOnlyTheConfiguredUnixSocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "status.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/stub_status" {
			t.Errorf("status path = %q", request.URL.Path)
		}
		_, _ = io.WriteString(response, "Active connections: 3\nserver accepts handled requests\n10 10 14\nReading: 1 Writing: 1 Waiting: 1\n")
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	readStatus := newNginxStatusReader(socketPath)
	status, err := readStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.ActiveConnections != 3 || status.Requests != 14 || status.Waiting != 1 {
		t.Fatalf("Unix-socket Nginx status = %#v", status)
	}
}

func TestMachineStatusCollectorOmitsUnavailableNginxStatus(t *testing.T) {
	collector := machineStatusCollector{
		readFile: func(path string) ([]byte, error) {
			fixtures := map[string]string{
				"/etc/os-release":     "ID=debian\nNAME=Debian\nVERSION_ID=13\n",
				"/etc/debian_version": "13.5\n",
				"/proc/uptime":        "60 1\n", "/proc/loadavg": "0 0 0 1/1 1\n",
				"/proc/stat":      "cpu 1 0 0 9 0 0 0 0\n",
				"/proc/net/route": "Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT\neth0 00000000 00000000 0003 0 0 0 00000000 0 0 0\n",
				"/proc/net/dev":   "eth0: 1 0 0 0 0 0 0 0 1 0 0 0 0 0 0 0\n",
				"/proc/meminfo":   "MemTotal: 2 kB\nMemAvailable: 1 kB\n",
			}
			value, found := fixtures[path]
			if !found {
				return nil, errors.New("missing fixture")
			}
			return []byte(value), nil
		},
		statFilesystem: func(string) (int64, int64, error) { return 1, 2, nil },
		now:            func() time.Time { return time.Now().UTC() }, logicalCPUs: func() int { return 1 },
		nginxStatus: func() (*domain.NginxRuntimeStatus, error) { return nil, errors.New("socket unavailable") },
	}
	status, err := collector.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if status.Nginx != nil {
		t.Fatalf("unavailable Nginx status was not omitted: %#v", status.Nginx)
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
	directory := t.TempDir()
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory, CertificateDir: directory,
		NginxConfigPath: filepath.Join(directory, "nginx.conf"),
		AgentSHA256:     strings.Repeat("a", 64), HTTPClient: client, Runner: &fakeRunner{},
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
	found, foundStream, foundAdaptive := false, false, false
	for _, capability := range heartbeat.Capabilities {
		found = found || capability == domain.EdgeCapabilityMachineStatus
		foundStream = foundStream || capability == domain.EdgeCapabilityMachineStatusStream
		foundAdaptive = foundAdaptive || capability == domain.EdgeCapabilityMachineStatusAdaptive
	}
	if !found || !foundStream || !foundAdaptive {
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

func TestMachineStatusDefaultsToLegacySamplingUntilPolicyNegotiation(t *testing.T) {
	directory := t.TempDir()
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory, CertificateDir: directory,
		NginxConfigPath: filepath.Join(directory, "nginx.conf"),
		AgentSHA256:     strings.Repeat("c", 64), HTTPClient: &http.Client{Transport: upgradeRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusAccepted, Status: "202 Accepted", Header: make(http.Header), Body: http.NoBody}, nil
		})}, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.Config.MachineStatusInterval != time.Duration(domain.LegacyMachineStatusIntervalSeconds)*time.Second {
		t.Fatalf("machine status interval = %s", agent.Config.MachineStatusInterval)
	}
}

func TestMachineNetworkLoopPrimesBeforeReportingAtRequestedInterval(t *testing.T) {
	directory := t.TempDir()
	reports := make(chan domain.MachineNetworkStatus, 4)
	client := &http.Client{Transport: upgradeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/edge/v1/machine-network" {
			return nil, errors.New("unexpected request path " + request.URL.Path)
		}
		var report domain.MachineNetworkStatus
		if err := json.NewDecoder(request.Body).Decode(&report); err != nil {
			return nil, err
		}
		reports <- report
		return &http.Response{StatusCode: http.StatusAccepted, Status: "202 Accepted", Header: make(http.Header), Body: http.NoBody}, nil
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory, CertificateDir: directory,
		NginxConfigPath: filepath.Join(directory, "nginx.conf"), AgentSHA256: strings.Repeat("e", 64),
		HTTPClient: client, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	collected := 0
	base := time.Now().UTC()
	agent.machineNetwork = machineNetworkStatusReporterFunc(func() (*domain.MachineNetworkStatus, error) {
		collected++
		report := &domain.MachineNetworkStatus{
			NetworkInterface: "eth0", NetworkRXBytesPerSec: int64(collected * 100), NetworkTXBytesPerSec: int64(collected * 50),
			CollectedAt: base.Add(time.Duration(collected) * 10 * time.Millisecond),
		}
		if collected > 1 {
			report.SampleSeconds = 0.01
		}
		return report, nil
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		agent.runMachineNetworkLoop(ctx)
		close(done)
	}()
	agent.setMachineNetworkInterval(10 * time.Millisecond)
	select {
	case report := <-reports:
		if collected < 2 || report.SampleSeconds != 0.01 {
			cancel()
			t.Fatalf("first uploaded network report = %#v, collect calls = %d", report, collected)
		}
	case <-time.After(time.Second):
		cancel()
		t.Fatal("machine network loop did not upload its first interval sample")
	}
	agent.setMachineNetworkInterval(0)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("machine network loop did not stop after cancellation")
	}
}

func TestMachineStatusPolicyStreamAppliesNegotiatedIntervals(t *testing.T) {
	directory := t.TempDir()
	client := &http.Client{Timeout: time.Millisecond, Transport: upgradeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/edge/v1/machine-status/policy/events" {
			return nil, errors.New("unexpected request path " + request.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader("event: machine-status-policy\ndata: {\"host_interval_seconds\":5,\"network_interval_seconds\":1,\"origin_interval_seconds\":5}\n\n")),
		}, nil
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory, CertificateDir: directory,
		NginxConfigPath: filepath.Join(directory, "nginx.conf"), AgentSHA256: strings.Repeat("f", 64),
		HTTPClient: client, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = agent.streamMachineStatusPolicy(t.Context())
	if !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, errMachineStatusPolicyInactive) {
		t.Fatalf("policy stream error = %v", err)
	}
	assertMachineIntervalUpdate(t, agent.machineStatusIntervals, time.Duration(domain.ActiveMachineStatusIntervalSeconds)*time.Second, "host")
	assertMachineIntervalUpdate(t, agent.machineNetworkIntervals, time.Second, "network")
	assertMachineIntervalUpdate(t, agent.machineOriginIntervals, 5*time.Second, "origin")
}

func TestMachineStatusPolicyStreamTimesOutWhenKeepalivesStop(t *testing.T) {
	directory := t.TempDir()
	reader, writer := io.Pipe()
	client := &http.Client{Transport: upgradeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: reader,
		}, nil
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory, CertificateDir: directory,
		NginxConfigPath: filepath.Join(directory, "nginx.conf"), AgentSHA256: strings.Repeat("1", 64),
		HTTPClient: client, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := io.WriteString(writer, "event: machine-status-policy\ndata: {\"host_interval_seconds\":5,\"network_interval_seconds\":1,\"origin_interval_seconds\":5}\n\n")
		writeDone <- writeErr
	}()
	err = agent.streamMachineStatusPolicyWithTimeout(t.Context(), 25*time.Millisecond)
	_ = writer.Close()
	if err == nil || !strings.Contains(err.Error(), "inactive") {
		t.Fatalf("stalled policy stream error = %v", err)
	}
	if !errors.Is(err, errMachineStatusPolicyInactive) {
		t.Fatalf("stalled policy stream was not classified as inactive: %v", err)
	}
	if writeErr := <-writeDone; writeErr != nil {
		t.Fatal(writeErr)
	}
	assertMachineIntervalUpdate(t, agent.machineStatusIntervals, time.Duration(domain.ActiveMachineStatusIntervalSeconds)*time.Second, "host")
	assertMachineIntervalUpdate(t, agent.machineNetworkIntervals, time.Second, "network")
	assertMachineIntervalUpdate(t, agent.machineOriginIntervals, 5*time.Second, "origin")
}

func TestMachineStatusPolicyLoopKeepsLastPolicyDuringTransientFailure(t *testing.T) {
	directory := t.TempDir()
	reconnected := make(chan struct{})
	requestCount := 0
	client := &http.Client{Transport: upgradeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount++
		if requestCount == 1 {
			return machineStatusPolicyResponse("{\"host_interval_seconds\":60,\"network_interval_seconds\":0,\"origin_interval_seconds\":5}"), nil
		}
		if requestCount == 2 {
			close(reconnected)
			reader, writer := io.Pipe()
			go func() {
				<-request.Context().Done()
				_ = writer.CloseWithError(request.Context().Err())
			}()
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       reader,
			}, nil
		}
		return nil, errors.New("unexpected extra policy request")
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory, CertificateDir: directory,
		NginxConfigPath: filepath.Join(directory, "nginx.conf"), AgentSHA256: strings.Repeat("3", 64),
		HTTPClient: client, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		agent.runMachineStatusPolicyLoopWithConfig(ctx, time.Millisecond, time.Hour, time.Millisecond, time.Minute, machineStatusPolicyReadTimeout)
		close(done)
	}()
	select {
	case <-reconnected:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("policy stream was not reconnected")
	}

	assertMachineIntervalUpdate(t, agent.machineStatusIntervals, time.Minute, "host")
	assertMachineIntervalUpdate(t, agent.machineNetworkIntervals, 0, "network")
	assertMachineIntervalUpdate(t, agent.machineOriginIntervals, 5*time.Second, "origin")
	if detail := agent.heartbeatError(); detail != "" {
		cancel()
		t.Fatalf("transient policy failure leaked into heartbeat: %q", detail)
	}
	stopMachineStatusPolicyLoop(t, cancel, done)
}

func TestMachineStatusPolicyLoopUsesLongBackoffForInactiveStream(t *testing.T) {
	directory := t.TempDir()
	firstReturned := make(chan struct{})
	secondStarted := make(chan struct{})
	requestCount := 0
	client := &http.Client{Transport: upgradeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount++
		if requestCount == 1 {
			close(firstReturned)
			return machineStatusPolicyResponse("{\"host_interval_seconds\":60,\"network_interval_seconds\":0,\"origin_interval_seconds\":5}"), nil
		}
		if requestCount == 2 {
			close(secondStarted)
			return nil, io.ErrUnexpectedEOF
		}
		return nil, errors.New("unexpected extra policy request")
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory, CertificateDir: directory,
		NginxConfigPath: filepath.Join(directory, "nginx.conf"), AgentSHA256: strings.Repeat("6", 64),
		HTTPClient: client, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		agent.runMachineStatusPolicyLoopWithConfig(ctx, time.Millisecond, time.Hour, 80*time.Millisecond, time.Hour, 10*time.Millisecond)
		close(done)
	}()
	select {
	case <-firstReturned:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("initial policy request was not sent")
	}
	assertMachineIntervalUpdate(t, agent.machineStatusIntervals, time.Minute, "host")
	assertMachineIntervalUpdate(t, agent.machineNetworkIntervals, 0, "network")
	assertMachineIntervalUpdate(t, agent.machineOriginIntervals, 5*time.Second, "origin")

	select {
	case <-secondStarted:
		cancel()
		t.Fatal("inactive policy stream retried before the long backoff elapsed")
	case <-time.After(30 * time.Millisecond):
	}
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("inactive policy stream was not retried")
	}
	if detail := agent.heartbeatError(); detail != "" {
		cancel()
		t.Fatalf("short inactive stream leaked into heartbeat: %q", detail)
	}
	stopMachineStatusPolicyLoop(t, cancel, done)
}

func TestMachineStatusPolicyClassifiesReadTimeoutAsInactive(t *testing.T) {
	readTimeout := &net.DNSError{Err: "read timeout", IsTimeout: true}
	if !isMachineStatusPolicyInactiveError(readTimeout) {
		t.Fatal("read timeout was not classified as an inactive policy stream")
	}
	if isMachineStatusPolicyInactiveError(errors.New("connection refused")) {
		t.Fatal("generic transport error was classified as an inactive policy stream")
	}
}

func TestMachineStatusPolicyLoopReportsPersistentInactiveFailureAfterGrace(t *testing.T) {
	directory := t.TempDir()
	requestCount := 0
	client := &http.Client{Transport: upgradeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount++
		if requestCount == 1 {
			return machineStatusPolicyResponse("{\"host_interval_seconds\":60,\"network_interval_seconds\":0,\"origin_interval_seconds\":5}"), nil
		}
		return machineStatusPolicyResponse(), nil
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory, CertificateDir: directory,
		NginxConfigPath: filepath.Join(directory, "nginx.conf"), AgentSHA256: strings.Repeat("7", 64),
		HTTPClient: client, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		agent.runMachineStatusPolicyLoopWithConfig(ctx, time.Millisecond, time.Hour, time.Millisecond, 25*time.Millisecond, 10*time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(agent.heartbeatError(), errMachineStatusPolicyInactive.Error()) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if detail := agent.heartbeatError(); !strings.Contains(detail, errMachineStatusPolicyInactive.Error()) {
		cancel()
		t.Fatalf("persistent inactive policy failure heartbeat error = %q", detail)
	}
	assertMachineIntervalUpdate(t, agent.machineStatusIntervals, time.Duration(domain.LegacyMachineStatusIntervalSeconds)*time.Second, "host")
	assertMachineIntervalUpdate(t, agent.machineNetworkIntervals, 0, "network")
	assertMachineIntervalUpdate(t, agent.machineOriginIntervals, 0, "origin")
	stopMachineStatusPolicyLoop(t, cancel, done)
}

func TestMachineStatusPolicyLoopFallsBackAfterPersistentFailure(t *testing.T) {
	directory := t.TempDir()
	recoverPolicy := make(chan struct{})
	recoveryWritten := make(chan struct{})
	requestCount := 0
	client := &http.Client{Transport: upgradeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount++
		if requestCount == 1 {
			return machineStatusPolicyResponse("{\"host_interval_seconds\":60,\"network_interval_seconds\":0,\"origin_interval_seconds\":5}"), nil
		}
		select {
		case <-recoverPolicy:
			reader, writer := io.Pipe()
			go func() {
				_, _ = io.WriteString(writer, "event: machine-status-policy\ndata: {\"host_interval_seconds\":60,\"network_interval_seconds\":0,\"origin_interval_seconds\":5}\n\n")
				close(recoveryWritten)
				<-request.Context().Done()
				_ = writer.CloseWithError(request.Context().Err())
			}()
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       reader,
			}, nil
		default:
			return nil, errors.New("policy stream unavailable")
		}
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory, CertificateDir: directory,
		NginxConfigPath: filepath.Join(directory, "nginx.conf"), AgentSHA256: strings.Repeat("4", 64),
		HTTPClient: client, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		agent.runMachineStatusPolicyLoopWithConfig(ctx, time.Millisecond, time.Hour, time.Millisecond, 25*time.Millisecond, machineStatusPolicyReadTimeout)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for agent.heartbeatError() == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if detail := agent.heartbeatError(); !strings.Contains(detail, "policy stream unavailable") {
		cancel()
		t.Fatalf("persistent policy failure heartbeat error = %q", detail)
	}

	assertMachineIntervalUpdate(t, agent.machineStatusIntervals, time.Duration(domain.LegacyMachineStatusIntervalSeconds)*time.Second, "host")
	assertMachineIntervalUpdate(t, agent.machineNetworkIntervals, 0, "network")
	assertMachineIntervalUpdate(t, agent.machineOriginIntervals, 0, "origin")

	close(recoverPolicy)
	select {
	case <-recoveryWritten:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("recovered policy was not written")
	}
	deadline = time.Now().Add(time.Second)
	for agent.heartbeatError() != "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if detail := agent.heartbeatError(); detail != "" {
		cancel()
		t.Fatalf("recovered policy did not clear heartbeat error: %q", detail)
	}
	assertMachineIntervalUpdate(t, agent.machineStatusIntervals, time.Minute, "host")
	assertMachineIntervalUpdate(t, agent.machineNetworkIntervals, 0, "network")
	assertMachineIntervalUpdate(t, agent.machineOriginIntervals, 5*time.Second, "origin")
	stopMachineStatusPolicyLoop(t, cancel, done)
}

func TestMachineStatusPolicyLoopRejectsInvalidPolicyImmediately(t *testing.T) {
	directory := t.TempDir()
	client := &http.Client{Transport: upgradeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return machineStatusPolicyResponse(
			"{\"host_interval_seconds\":60,\"network_interval_seconds\":0,\"origin_interval_seconds\":5}",
			"{\"host_interval_seconds\":-1,\"network_interval_seconds\":1,\"origin_interval_seconds\":5}",
		), nil
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory, CertificateDir: directory,
		NginxConfigPath: filepath.Join(directory, "nginx.conf"), AgentSHA256: strings.Repeat("5", 64),
		HTTPClient: client, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		agent.runMachineStatusPolicyLoopWithIntervals(ctx, time.Hour, time.Hour, time.Hour)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for agent.heartbeatError() == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if detail := agent.heartbeatError(); !strings.Contains(detail, errMachineStatusPolicyInvalid.Error()) {
		cancel()
		t.Fatalf("invalid policy heartbeat error = %q", detail)
	}

	assertMachineIntervalUpdate(t, agent.machineStatusIntervals, time.Duration(domain.LegacyMachineStatusIntervalSeconds)*time.Second, "host")
	assertMachineIntervalUpdate(t, agent.machineNetworkIntervals, 0, "network")
	assertMachineIntervalUpdate(t, agent.machineOriginIntervals, 0, "origin")
	stopMachineStatusPolicyLoop(t, cancel, done)
}

func TestUnsupportedMachineStatusPolicyKeepsLegacyModeWithoutHeartbeatError(t *testing.T) {
	directory := t.TempDir()
	requestHandled := make(chan struct{})
	client := &http.Client{Transport: upgradeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(requestHandled)
		return &http.Response{
			StatusCode: http.StatusNotFound, Status: "404 Not Found", Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("not found")),
		}, nil
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory, CertificateDir: directory,
		NginxConfigPath: filepath.Join(directory, "nginx.conf"), AgentSHA256: strings.Repeat("2", 64),
		HTTPClient: client, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.setComponentFailure("machine_status_policy", "stream machine status policy", errors.New("previous failure"))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		agent.runMachineStatusPolicyLoop(ctx)
		close(done)
	}()
	select {
	case <-requestHandled:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("policy compatibility request was not sent")
	}
	deadline := time.Now().Add(time.Second)
	for agent.heartbeatError() != "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if detail := agent.heartbeatError(); detail != "" {
		cancel()
		t.Fatalf("unsupported policy leaked into heartbeat: %q", detail)
	}
	assertMachineIntervalUpdate(t, agent.machineStatusIntervals, time.Duration(domain.LegacyMachineStatusIntervalSeconds)*time.Second, "host")
	assertMachineIntervalUpdate(t, agent.machineNetworkIntervals, 0, "network")
	assertMachineIntervalUpdate(t, agent.machineOriginIntervals, 0, "origin")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("policy loop did not stop after cancellation")
	}
}

func machineStatusPolicyResponse(policies ...string) *http.Response {
	var body strings.Builder
	for _, policy := range policies {
		body.WriteString("event: machine-status-policy\ndata: ")
		body.WriteString(policy)
		body.WriteString("\n\n")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body.String())),
	}
}

func stopMachineStatusPolicyLoop(t *testing.T, cancel context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("policy loop did not stop after cancellation")
	}
}

func assertMachineIntervalUpdate(t *testing.T, updates <-chan time.Duration, expected time.Duration, name string) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case interval := <-updates:
			if interval == expected {
				return
			}
		case <-deadline:
			t.Fatalf("machine status policy did not update the %s interval to %s", name, expected)
		}
	}
}

func TestMachineOriginStatusRoundReportsCurrentProbeAndConnectionCount(t *testing.T) {
	checkedAt := time.Now().UTC().Add(-time.Second)
	poolID := strings.Repeat("a", 24)
	probe := &domain.OriginProbeStatus{
		PoolID: poolID, Address: "203.0.113.10:8080", Scheme: "http", KeepaliveConnections: 16,
		References: []domain.OriginPoolReference{{SiteID: "site-1", Role: "primary"}},
		Healthy:    true, CircuitState: domain.OriginCircuitClosed, CheckedAt: checkedAt,
		ServiceProbe: &domain.OriginProbeSample{Healthy: true, HeaderMS: 2, TotalMS: 3, HTTPStatus: http.StatusNoContent, CheckedAt: checkedAt},
	}
	reports := make(chan domain.MachineOriginStatus, 1)
	client := &http.Client{Transport: upgradeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/edge/v1/machine-origin" {
			return nil, errors.New("unexpected request path " + request.URL.Path)
		}
		var report domain.MachineOriginStatus
		if err := json.NewDecoder(request.Body).Decode(&report); err != nil {
			return nil, err
		}
		reports <- report
		return &http.Response{StatusCode: http.StatusAccepted, Status: "202 Accepted", Header: make(http.Header), Body: http.NoBody}, nil
	})}
	agent := &Agent{
		Config: Config{ControlURL: "https://control.example.test", HTTPClient: client},
		originPools: map[string]*originPoolRuntime{
			poolID: {
				Pool: domain.OriginPool{
					ID: poolID, Address: probe.Address, Scheme: probe.Scheme, HostHeader: "origin.example.test",
					KeepaliveConnections: 16, References: append([]domain.OriginPoolReference(nil), probe.References...),
				},
				CircuitState: domain.OriginCircuitClosed, Status: probe,
			},
		},
		originConnections: originConnectionCounterFunc(func(_ context.Context, pools []domain.OriginPool) map[string]int64 {
			if len(pools) != 1 || pools[0].ID != poolID {
				t.Fatalf("origin pools = %#v", pools)
			}
			return map[string]int64{poolID: 7}
		}),
		componentFailures: make(map[string]string),
	}
	agent.runMachineOriginStatusRound(t.Context())
	select {
	case report := <-reports:
		if len(report.OriginProbes) != 1 || report.OriginProbes[0].EstablishedConnections == nil ||
			*report.OriginProbes[0].EstablishedConnections != 7 || report.CollectedAt.Before(checkedAt) {
			t.Fatalf("machine origin report = %#v", report)
		}
	default:
		t.Fatal("machine origin status was not reported")
	}
}

func TestMachineOriginStatusLoopWaitsForNegotiatedInterval(t *testing.T) {
	reports := make(chan struct{}, 2)
	client := &http.Client{Transport: upgradeRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/edge/v1/machine-origin" {
			return nil, errors.New("unexpected request path " + request.URL.Path)
		}
		reports <- struct{}{}
		return &http.Response{StatusCode: http.StatusAccepted, Status: "202 Accepted", Header: make(http.Header), Body: http.NoBody}, nil
	})}
	agent := &Agent{
		Config:                 Config{ControlURL: "https://control.example.test", HTTPClient: client},
		machineOriginIntervals: make(chan time.Duration, 1),
		originPools:            make(map[string]*originPoolRuntime),
		originConnections: originConnectionCounterFunc(func(context.Context, []domain.OriginPool) map[string]int64 {
			return map[string]int64{}
		}),
		componentFailures: make(map[string]string),
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		agent.runMachineOriginStatusLoop(ctx)
		close(done)
	}()
	select {
	case <-reports:
		cancel()
		t.Fatal("origin status reported before adaptive policy negotiation")
	case <-time.After(20 * time.Millisecond):
	}
	agent.setMachineOriginInterval(10 * time.Millisecond)
	select {
	case <-reports:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("origin status did not start after policy negotiation")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("origin status loop did not stop after cancellation")
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
	directory := t.TempDir()
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: directory, CertificateDir: directory,
		NginxConfigPath: filepath.Join(directory, "nginx.conf"),
		AgentSHA256:     strings.Repeat("d", 64), HTTPClient: client, Runner: &fakeRunner{}, MachineStatusInterval: 10 * time.Millisecond,
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
