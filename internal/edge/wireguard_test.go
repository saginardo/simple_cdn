package edge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"simple_cdn/internal/domain"
)

type wireGuardRoundTripFunc func(*http.Request) (*http.Response, error)

func (function wireGuardRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type fakeWireGuardManager struct {
	configs          []domain.WireGuardEdgeConfig
	reconcileCount   int
	performanceCount int
}

func (*fakeWireGuardManager) Available() (bool, bool) { return true, true }

func (manager *fakeWireGuardManager) Reconcile(_ context.Context, configs []domain.WireGuardEdgeConfig) ([]domain.WireGuardPeerReport, error) {
	manager.reconcileCount++
	manager.configs = append([]domain.WireGuardEdgeConfig(nil), configs...)
	config := configs[0]
	return []domain.WireGuardPeerReport{{
		TunnelID: config.TunnelID, Revision: config.Revision, InterfaceName: config.InterfaceName,
		PublicKey: wireGuardEdgeTestKey(2), RXBytes: 100, TXBytes: 200,
	}}, nil
}

func (manager *fakeWireGuardManager) RunPerformance(_ context.Context, _ domain.WireGuardPerformanceTest, _ domain.WireGuardEdgeConfig) (*domain.WireGuardPerformanceResult, error) {
	manager.performanceCount++
	return &domain.WireGuardPerformanceResult{
		DirectTCP: &domain.WireGuardTCPMeasurement{Mbps: 90, Retransmits: 1},
	}, errors.New("WireGuard UDP: blocked")
}

func TestWireGuardRoundCachesConfigurationReportsStatusAndPartialPerformance(t *testing.T) {
	const tunnelID = "12345678-1234-4234-8234-123456789abc"
	revision := strings.Repeat("a", 64)
	config := domain.WireGuardEdgeConfig{
		TunnelID: tunnelID, Name: "origin", Revision: 3, InterfaceName: domain.WireGuardInterfaceName(tunnelID),
		Address: "10.253.30.2/32", OriginAddress: "10.253.30.1", OriginPublicKey: wireGuardEdgeTestKey(1),
		Endpoint: "origin.example.test:51820", MTU: 1420, PersistentKeepaliveSecs: 25,
		PerformancePort: 5201, DirectPerformanceHost: "origin.example.test",
	}
	test := domain.WireGuardPerformanceTest{
		ID: "performance-test", TunnelID: tunnelID, NodeID: "node", TargetMbps: 100,
		DurationSeconds: 3, Status: domain.WireGuardPerformanceRunning,
	}
	configGets := 0
	statusReports := 0
	var performanceReport struct {
		Result *domain.WireGuardPerformanceResult `json:"result"`
		Error  string                             `json:"error"`
	}
	client := &http.Client{Transport: wireGuardRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := func(status int, body string, headers http.Header) (*http.Response, error) {
			return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(body)), Header: headers}, nil
		}
		switch request.URL.Path {
		case "/api/edge/v1/wireguard/config":
			configGets++
			if configGets == 2 {
				if request.Header.Get("If-None-Match") != `"`+revision+`"` {
					t.Fatalf("WireGuard revision request header = %q", request.Header.Get("If-None-Match"))
				}
				return response(http.StatusNotModified, "", http.Header{"ETag": {`"` + revision + `"`}})
			}
			encoded, _ := json.Marshal(map[string]any{"revision": revision, "tunnels": []domain.WireGuardEdgeConfig{config}})
			return response(http.StatusOK, string(encoded), http.Header{"ETag": {`"` + revision + `"`}})
		case "/api/edge/v1/wireguard/status":
			statusReports++
			return response(http.StatusAccepted, `{}`, make(http.Header))
		case "/api/edge/v1/wireguard/performance-test":
			if configGets > 1 {
				return response(http.StatusNoContent, "", make(http.Header))
			}
			encoded, _ := json.Marshal(map[string]any{"test": test, "config": config})
			return response(http.StatusOK, string(encoded), make(http.Header))
		case "/api/edge/v1/wireguard/performance-tests/" + test.ID:
			if err := json.NewDecoder(request.Body).Decode(&performanceReport); err != nil {
				t.Fatal(err)
			}
			return response(http.StatusAccepted, `{}`, make(http.Header))
		default:
			return nil, errors.New("unexpected request: " + request.URL.Path)
		}
	})}
	manager := &fakeWireGuardManager{}
	agent, err := New(Config{
		ControlURL: "https://control.example.test", StateDir: t.TempDir(), CertificateDir: filepath.Join(t.TempDir(), "certs"),
		AgentSHA256: strings.Repeat("b", 64), HTTPClient: client, Runner: &fakeRunner{}, WireGuardManager: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slicesContain(agent.Config.Capabilities, domain.EdgeCapabilityWireGuard) ||
		!slicesContain(agent.Config.Capabilities, domain.EdgeCapabilityWireGuardPerformance) {
		t.Fatalf("WireGuard capabilities = %#v", agent.Config.Capabilities)
	}
	if err := agent.runWireGuardRound(context.Background()); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("partial performance round error = %v", err)
	}
	if performanceReport.Result == nil || performanceReport.Result.DirectTCP == nil || !strings.Contains(performanceReport.Error, "blocked") {
		t.Fatalf("partial performance report = %#v", performanceReport)
	}
	if err := agent.runWireGuardRound(context.Background()); err != nil {
		t.Fatal(err)
	}
	if configGets != 2 || statusReports != 2 || manager.reconcileCount != 2 || manager.performanceCount != 1 || len(manager.configs) != 1 {
		t.Fatalf("round calls = configs:%d status:%d reconcile:%d performance:%d", configGets, statusReports, manager.reconcileCount, manager.performanceCount)
	}
}

func TestWireGuardKeyPersistenceConfigRenderingAndIperfParsing(t *testing.T) {
	stateDirectory := filepath.Join(t.TempDir(), "state")
	configDirectory := filepath.Join(t.TempDir(), "configs")
	manager := &linuxWireGuardManager{stateDirectory: stateDirectory, configDirectory: configDirectory}
	privateKey, publicKey, err := manager.loadOrCreateKey("12345678-1234-4234-8234-123456789abc")
	if err != nil {
		t.Fatal(err)
	}
	secondPrivateKey, secondPublicKey, err := manager.loadOrCreateKey("12345678-1234-4234-8234-123456789abc")
	if err != nil || privateKey != secondPrivateKey || publicKey != secondPublicKey || !domain.ValidWireGuardKey(publicKey) {
		t.Fatalf("persisted WireGuard key = %q %q, %v", secondPrivateKey, secondPublicKey, err)
	}
	config := domain.WireGuardEdgeConfig{
		OriginPublicKey: wireGuardEdgeTestKey(1), Address: "10.253.40.2/32", OriginAddress: "10.253.40.1",
		Endpoint: "origin.example.test:51820", MTU: 1420, PersistentKeepaliveSecs: 25,
	}
	contents := string(renderWireGuardEdgeConfig(config, privateKey))
	for _, wanted := range []string{
		"PrivateKey = " + privateKey, "Address = 10.253.40.2/32", "AllowedIPs = 10.253.40.1/32",
		"Endpoint = origin.example.test:51820", "PersistentKeepalive = 25",
	} {
		if !strings.Contains(contents, wanted) {
			t.Fatalf("rendered WireGuard config is missing %q:\n%s", wanted, contents)
		}
	}

	commandPath := filepath.Join(t.TempDir(), "iperf3")
	script := `#!/bin/sh
case " $* " in
  *" -u "*) printf '%s\n' '{"end":{"sum":{"bits_per_second":80000000,"jitter_ms":1.25,"lost_packets":2,"packets":1000,"lost_percent":0.2}}}' ;;
  *) printf '%s\n' '{"end":{"sum_sent":{"bits_per_second":100000000,"retransmits":3}}}' ;;
esac
`
	if err := os.WriteFile(commandPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager.iperf3Path = commandPath
	tcp, err := manager.runIperfTCP(context.Background(), "origin.example.test", 5201, 3)
	if err != nil || tcp.Mbps != 100 || tcp.Retransmits != 3 {
		t.Fatalf("TCP iperf result = %#v, %v", tcp, err)
	}
	udp, err := manager.runIperfUDP(context.Background(), "10.253.40.1", 5201, 100, 3)
	if err != nil || udp.Mbps != 80 || udp.LostPackets != 2 || udp.TotalPackets != 1000 || udp.LossPercent != 0.2 || udp.JitterMS != 1.25 {
		t.Fatalf("UDP iperf result = %#v, %v", udp, err)
	}
}

func TestWireGuardSameHostGuardPreservesExistingConfig(t *testing.T) {
	const tunnelID = "12345678-1234-4234-8234-123456789abc"
	stateDirectory := filepath.Join(t.TempDir(), "state")
	configDirectory := filepath.Join(t.TempDir(), "configs")
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := &linuxWireGuardManager{
		stateDirectory: stateDirectory, configDirectory: configDirectory,
		resolveHostIPs: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("203.0.113.80")}, nil
		},
		localIPs: func() ([]net.IP, error) {
			return []net.IP{net.ParseIP("203.0.113.80")}, nil
		},
	}
	config := domain.WireGuardEdgeConfig{
		TunnelID: tunnelID, Name: "same-host", Revision: 2, InterfaceName: domain.WireGuardInterfaceName(tunnelID),
		Address: "10.253.80.2/32", OriginAddress: "10.253.80.1", OriginPublicKey: wireGuardEdgeTestKey(1),
		Endpoint: "origin.example.test:51820", MTU: 1420, PersistentKeepaliveSecs: 25,
		PerformancePort: 5201, DirectPerformanceHost: "origin.example.test",
	}
	path := manager.configPath(config)
	if err := os.WriteFile(path, []byte("origin configuration must survive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := manager.reconcileTunnel(context.Background(), config)
	if err == nil || !strings.Contains(err.Error(), "same-host WireGuard is unsupported") || !strings.Contains(report.Error, "same-host") {
		t.Fatalf("same-host reconcile = %#v, %v", report, err)
	}
	contents, readErr := os.ReadFile(path)
	if readErr != nil || string(contents) != "origin configuration must survive\n" {
		t.Fatalf("existing config after guard = %q, %v", contents, readErr)
	}
}

func TestWireGuardEdgeEgressShapingIsIdempotentAndRemovable(t *testing.T) {
	directory := t.TempDir()
	commandPath := filepath.Join(directory, "tc")
	logPath := filepath.Join(directory, "commands.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + logPath + "\"\n" +
		"if [ \"$*\" = \"qdisc show dev scwg1234567812\" ]; then printf '%s\\n' 'qdisc htb 1: root'; fi\n"
	if err := os.WriteFile(commandPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := &linuxWireGuardManager{tcPath: commandPath, appliedEgressLimitsMbps: make(map[string]int)}
	config := domain.WireGuardEdgeConfig{
		TunnelID: "12345678-1234-4234-8234-123456789abc", InterfaceName: "scwg1234567812", EdgeEgressLimitMbps: 25,
	}
	if err := manager.applyEgressLimit(context.Background(), config, false); err != nil {
		t.Fatal(err)
	}
	if err := manager.applyEgressLimit(context.Background(), config, false); err != nil {
		t.Fatal(err)
	}
	config.EdgeEgressLimitMbps = 0
	if err := manager.applyEgressLimit(context.Background(), config, false); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
	if len(lines) != 5 || !strings.Contains(lines[1], "rate 25mbit ceil 25mbit") || lines[3] != "qdisc show dev scwg1234567812" || lines[4] != "qdisc delete dev scwg1234567812 root" {
		t.Fatalf("tc commands = %#v", lines)
	}
}

func wireGuardEdgeTestKey(fill byte) string {
	raw := make([]byte, 32)
	for index := range raw {
		raw[index] = fill
	}
	return base64.StdEncoding.EncodeToString(raw)
}
