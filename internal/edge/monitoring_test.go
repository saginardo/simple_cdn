package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"simple_cdn/internal/domain"
)

type monitoringRoundTripFunc func(*http.Request) (*http.Response, error)

func (function monitoringRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type fakeMonitoringDialer struct {
	mu    sync.Mutex
	calls map[string]int
}

func (dialer *fakeMonitoringDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	dialer.mu.Lock()
	dialer.calls[address]++
	dialer.mu.Unlock()
	if strings.Contains(address, "failed") {
		return nil, errors.New("connection refused\n")
	}
	return fakeMonitoringConnection{}, nil
}

type fakeMonitoringConnection struct{}

func (fakeMonitoringConnection) Read([]byte) (int, error)         { return 0, io.EOF }
func (fakeMonitoringConnection) Write(value []byte) (int, error)  { return len(value), nil }
func (fakeMonitoringConnection) Close() error                     { return nil }
func (fakeMonitoringConnection) LocalAddr() net.Addr              { return fakeMonitoringAddress("local") }
func (fakeMonitoringConnection) RemoteAddr() net.Addr             { return fakeMonitoringAddress("remote") }
func (fakeMonitoringConnection) SetDeadline(time.Time) error      { return nil }
func (fakeMonitoringConnection) SetReadDeadline(time.Time) error  { return nil }
func (fakeMonitoringConnection) SetWriteDeadline(time.Time) error { return nil }

type fakeMonitoringAddress string

func (fakeMonitoringAddress) Network() string        { return "tcp" }
func (address fakeMonitoringAddress) String() string { return string(address) }

func TestMonitorPullsTargetsProbesThreeTimesAndReports(t *testing.T) {
	targets := []domain.MonitoringTarget{
		{ID: "target-ok", Address: "ok.example.test:443", Enabled: true},
		{ID: "target-failed", Address: "failed.example.test:8443", Enabled: true},
	}
	dialer := &fakeMonitoringDialer{calls: make(map[string]int)}
	var reported []domain.MonitoringProbeResult
	client := &http.Client{Transport: monitoringRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/edge/v1/monitoring-targets":
			encoded, _ := json.Marshal(targets)
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(string(encoded))), Header: make(http.Header)}, nil
		case "/api/edge/v1/monitoring-results":
			var input struct {
				Results []domain.MonitoringProbeResult `json:"results"`
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			reported = input.Results
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(`{"accepted":true}`)), Header: make(http.Header)}, nil
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: io.NopCloser(strings.NewReader("not found")), Header: make(http.Header)}, nil
		}
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: t.TempDir(), CertificateDir: filepath.Join(t.TempDir(), "certs"),
		AgentSHA256: strings.Repeat("a", 64), HTTPClient: client, Runner: &fakeRunner{}, MonitoringDialer: dialer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.Config.PollInterval != 30*time.Second || agent.Config.MonitoringAttempts != 3 || agent.Config.MonitoringTimeout != 2*time.Second {
		t.Fatalf("monitoring cadence = poll %s, attempts %d, timeout %s", agent.Config.PollInterval, agent.Config.MonitoringAttempts, agent.Config.MonitoringTimeout)
	}
	if err := agent.Monitor(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dialer.calls[targets[0].Address] != 3 || dialer.calls[targets[1].Address] != 3 {
		t.Fatalf("dial calls = %#v", dialer.calls)
	}
	if len(reported) != 2 || reported[0].SuccessfulAttempts != 3 || reported[0].AverageLatencyMS <= 0 ||
		reported[1].SuccessfulAttempts != 0 || reported[1].AverageLatencyMS != 0 || reported[1].Error != "connection refused" {
		t.Fatalf("reported results = %#v", reported)
	}
	if !slicesContain(agent.Config.Capabilities, domain.EdgeCapabilityTCPMonitoring) {
		t.Fatalf("agent capabilities = %#v", agent.Config.Capabilities)
	}
}

func TestMonitorSkipsReportWhenNoTargetsAreConfigured(t *testing.T) {
	postCount := 0
	client := &http.Client{Transport: monitoringRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodPost {
			postCount++
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader("[]")), Header: make(http.Header)}, nil
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: t.TempDir(), CertificateDir: filepath.Join(t.TempDir(), "certs"),
		AgentSHA256: strings.Repeat("b", 64), HTTPClient: client, Runner: &fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Monitor(context.Background()); err != nil {
		t.Fatal(err)
	}
	if postCount != 0 {
		t.Fatalf("empty target set generated %d reports", postCount)
	}
}

func TestMonitorReusesTargetsUntilManifestRevisionChanges(t *testing.T) {
	revision := strings.Repeat("c", 64)
	targets := []domain.MonitoringTarget{{ID: "target-ok", Address: "ok.example.test:443", Enabled: true}}
	dialer := &fakeMonitoringDialer{calls: make(map[string]int)}
	getCount, reportCount := 0, 0
	client := &http.Client{Transport: monitoringRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/edge/v1/monitoring-targets":
			getCount++
			encoded, _ := json.Marshal(targets)
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(string(encoded))), Header: http.Header{"ETag": {`"` + revision + `"`}}}, nil
		case "/api/edge/v1/monitoring-results":
			reportCount++
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(`{"accepted":true}`)), Header: make(http.Header)}, nil
		default:
			return nil, fmt.Errorf("unexpected request %s", request.URL.Path)
		}
	})}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: t.TempDir(), CertificateDir: filepath.Join(t.TempDir(), "certs"),
		AgentSHA256: strings.Repeat("d", 64), HTTPClient: client, Runner: &fakeRunner{}, MonitoringDialer: dialer,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := agent.monitor(context.Background(), revision, true); err != nil {
			t.Fatal(err)
		}
	}
	if getCount != 1 || reportCount != 2 || dialer.calls[targets[0].Address] != 6 {
		t.Fatalf("requests: target_get=%d reports=%d probes=%d", getCount, reportCount, dialer.calls[targets[0].Address])
	}
}
