package edge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		DirectTCP:        &domain.WireGuardTCPMeasurement{Mbps: 90, Retransmits: 1},
		DirectTCPReverse: &domain.WireGuardTCPMeasurement{Mbps: 95, Retransmits: 0},
	}, errors.New("WireGuard UDP: blocked")
}

func TestWireGuardRoundsDecouplePerformanceFromConfiguration(t *testing.T) {
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
	performanceClaims := 0
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
			performanceClaims++
			if performanceClaims > 1 {
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
		OriginPoolConfigDirectory: filepath.Join(t.TempDir(), "origin-pools"),
		AgentSHA256:               strings.Repeat("b", 64), HTTPClient: client, Runner: &fakeRunner{}, WireGuardManager: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slicesContain(agent.Config.Capabilities, domain.EdgeCapabilityWireGuard) ||
		!slicesContain(agent.Config.Capabilities, domain.EdgeCapabilityWireGuardPerformance) ||
		!slicesContain(agent.Config.Capabilities, domain.EdgeCapabilityWireGuardPerformanceV2) {
		t.Fatalf("WireGuard capabilities = %#v", agent.Config.Capabilities)
	}
	if err := agent.runWireGuardPerformanceRound(context.Background()); err != nil {
		t.Fatal(err)
	}
	if performanceClaims != 0 || manager.performanceCount != 0 {
		t.Fatalf("performance ran before initial reconciliation: claims:%d runs:%d", performanceClaims, manager.performanceCount)
	}
	if err := agent.runWireGuardRound(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := agent.runWireGuardRound(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := agent.runWireGuardPerformanceRound(context.Background()); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("partial performance round error = %v", err)
	}
	if err := agent.runWireGuardPerformanceRound(context.Background()); err != nil {
		t.Fatal(err)
	}
	if performanceReport.Result == nil || performanceReport.Result.DirectTCP == nil || !strings.Contains(performanceReport.Error, "blocked") {
		t.Fatalf("partial performance report = %#v", performanceReport)
	}
	if configGets != 2 || statusReports != 2 || performanceClaims != 2 || manager.reconcileCount != 2 || manager.performanceCount != 1 || len(manager.configs) != 1 {
		t.Fatalf("round calls = configs:%d status:%d claims:%d reconcile:%d performance:%d", configGets, statusReports, performanceClaims, manager.reconcileCount, manager.performanceCount)
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

	directory := t.TempDir()
	commandPath := filepath.Join(directory, "iperf3")
	commandLog := filepath.Join(directory, "iperf3.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + commandLog + `"
case " $* " in
  *" -u "*) printf '%s\n' '{"end":{"sum":{"bits_per_second":80000000,"jitter_ms":1.25,"lost_packets":2,"packets":1000,"lost_percent":0.2}}}' ;;
  *) printf '%s\n' '{"end":{"sum_sent":{"bits_per_second":100000000,"retransmits":3}}}' ;;
esac
`
	if err := os.WriteFile(commandPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager.iperf3Path = commandPath
	tcp, err := manager.runIperfTCP(context.Background(), "origin.example.test", 5201, 3, false)
	if err != nil || tcp.Mbps != 100 || tcp.Retransmits != 3 {
		t.Fatalf("TCP iperf result = %#v, %v", tcp, err)
	}
	reverseTCP, err := manager.runIperfTCP(context.Background(), "origin.example.test", 5201, 3, true)
	if err != nil || reverseTCP.Mbps != 100 || reverseTCP.Retransmits != 3 {
		t.Fatalf("reverse TCP iperf result = %#v, %v", reverseTCP, err)
	}
	udp, err := manager.runIperfUDP(context.Background(), "10.253.40.1", 5201, 100, 3, false)
	if err != nil || udp.Mbps != 80 || udp.LostPackets != 2 || udp.TotalPackets != 1000 || udp.LossPercent != 0.2 || udp.JitterMS != 1.25 {
		t.Fatalf("UDP iperf result = %#v, %v", udp, err)
	}
	reverseUDP, err := manager.runIperfUDP(context.Background(), "10.253.40.1", 5201, 100, 3, true)
	if err != nil || reverseUDP.Mbps != 80 || reverseUDP.LostPackets != 2 || reverseUDP.TotalPackets != 1000 || reverseUDP.LossPercent != 0.2 || reverseUDP.JitterMS != 1.25 {
		t.Fatalf("reverse UDP iperf result = %#v, %v", reverseUDP, err)
	}
	commands, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(commands)), "\n")
	if len(lines) != 4 || strings.Contains(lines[0], " -R ") || !strings.Contains(" "+lines[1]+" ", " -R ") ||
		strings.Contains(lines[2], " -R ") || !strings.Contains(" "+lines[3]+" ", " -R ") {
		t.Fatalf("iperf direction commands = %#v", lines)
	}
}

func TestWireGuardActiveConfigUsesLiveSyncAndPreservesPreviousConfigOnFailure(t *testing.T) {
	directory := t.TempDir()
	configDirectory := filepath.Join(directory, "configs")
	if err := os.MkdirAll(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	commandLog := filepath.Join(directory, "commands.log")
	runtimeConfig := filepath.Join(directory, "runtime.conf")
	failSync := filepath.Join(directory, "fail-sync")
	ipPath := writeWireGuardTestCommand(t, directory, "ip", commandLog, "exit 0")
	wgPath := writeWireGuardTestCommand(t, directory, "wg", commandLog, fmt.Sprintf(`
if [ "$1" = "syncconf" ]; then
  if [ -f %q ]; then exit 1; fi
  cp "$3" %q
fi
exit 0`, failSync, runtimeConfig))
	wgQuickPath := writeWireGuardTestCommand(t, directory, "wg-quick", commandLog, "exit 0")
	manager := &linuxWireGuardManager{
		configDirectory: configDirectory, ipPath: ipPath, wgPath: wgPath, wgQuickPath: wgQuickPath,
	}
	config := domain.WireGuardEdgeConfig{
		TunnelID: "12345678-1234-4234-8234-123456789abc", InterfaceName: "scwg1234567812",
		Address: "10.253.40.2/32", OriginAddress: "10.253.40.1", OriginPublicKey: wireGuardEdgeTestKey(1),
		Endpoint: "origin.example.test:51820", MTU: 1420, PersistentKeepaliveSecs: 25,
	}
	privateKey := wireGuardEdgeTestKey(9)
	path := manager.configPath(config)
	previous := renderWireGuardEdgeConfig(config, privateKey)
	if err := os.WriteFile(path, previous, 0o600); err != nil {
		t.Fatal(err)
	}

	config.Address = "10.253.41.2/32"
	config.OriginAddress = "10.253.41.1"
	config.Endpoint = "origin.example.test:51821"
	config.MTU = 1380
	desired := renderWireGuardEdgeConfig(config, privateKey)
	started, err := manager.applyConfig(context.Background(), config, desired)
	if err != nil || started {
		t.Fatalf("live update = started:%t err:%v", started, err)
	}
	log := readWireGuardTestFile(t, commandLog)
	for _, expected := range []string{
		"ip link show dev scwg1234567812",
		"wg syncconf scwg1234567812 ",
		"ip -4 address replace 10.253.41.2/32 dev scwg1234567812",
		"ip -4 route replace 10.253.41.1/32 dev scwg1234567812",
		"ip link set dev scwg1234567812 mtu 1380",
		"ip -4 route delete 10.253.40.1/32 dev scwg1234567812",
		"ip -4 address delete 10.253.40.2/32 dev scwg1234567812",
	} {
		if !strings.Contains(log, expected) {
			t.Fatalf("live update command log is missing %q:\n%s", expected, log)
		}
	}
	if strings.Contains(log, "wg-quick ") {
		t.Fatalf("live update restarted the WireGuard interface:\n%s", log)
	}
	if got := readWireGuardTestFile(t, path); got != string(desired) {
		t.Fatalf("persisted live configuration = %q, want %q", got, desired)
	}
	runtime := readWireGuardTestFile(t, runtimeConfig)
	if strings.Contains(runtime, "Address =") || strings.Contains(runtime, "MTU =") ||
		!strings.Contains(runtime, "Endpoint = origin.example.test:51821") {
		t.Fatalf("wg syncconf configuration contains wg-quick directives or misses the peer:\n%s", runtime)
	}

	if err := os.WriteFile(commandLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if started, err := manager.applyConfig(context.Background(), config, desired); err != nil || started {
		t.Fatalf("idempotent live update = started:%t err:%v", started, err)
	}
	if log := strings.TrimSpace(readWireGuardTestFile(t, commandLog)); log != "ip link show dev scwg1234567812" {
		t.Fatalf("idempotent live update commands = %q", log)
	}

	if err := os.WriteFile(failSync, []byte("fail\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	failedConfig := config
	failedConfig.Endpoint = "origin.example.test:51822"
	failedContents := renderWireGuardEdgeConfig(failedConfig, privateKey)
	if _, err := manager.applyConfig(context.Background(), failedConfig, failedContents); err == nil || !strings.Contains(err.Error(), "synchronize active WireGuard interface") {
		t.Fatalf("failed live update error = %v", err)
	}
	if got := readWireGuardTestFile(t, path); got != string(desired) {
		t.Fatalf("failed live update replaced the previous config: %q", got)
	}
	if log := readWireGuardTestFile(t, commandLog); strings.Contains(log, "wg-quick ") {
		t.Fatalf("failed live update fell back to a disruptive restart:\n%s", log)
	}
}

func TestWireGuardMissingInterfaceUsesInitialWgQuickStart(t *testing.T) {
	directory := t.TempDir()
	configDirectory := filepath.Join(directory, "configs")
	if err := os.MkdirAll(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	commandLog := filepath.Join(directory, "commands.log")
	ipPath := writeWireGuardTestCommand(t, directory, "ip", commandLog, "exit 1")
	wgQuickPath := writeWireGuardTestCommand(t, directory, "wg-quick", commandLog, "exit 0")
	manager := &linuxWireGuardManager{configDirectory: configDirectory, ipPath: ipPath, wgQuickPath: wgQuickPath}
	config := domain.WireGuardEdgeConfig{
		TunnelID: "12345678-1234-4234-8234-123456789abc", InterfaceName: "scwg1234567812",
		Address: "10.253.40.2/32", OriginAddress: "10.253.40.1", OriginPublicKey: wireGuardEdgeTestKey(1),
		Endpoint: "origin.example.test:51820", MTU: 1420, PersistentKeepaliveSecs: 25,
	}
	contents := renderWireGuardEdgeConfig(config, wireGuardEdgeTestKey(9))
	started, err := manager.applyConfig(context.Background(), config, contents)
	if err != nil || !started {
		t.Fatalf("initial WireGuard start = started:%t err:%v", started, err)
	}
	log := readWireGuardTestFile(t, commandLog)
	if !strings.Contains(log, "wg-quick up "+manager.configPath(config)) || strings.Contains(log, "wg syncconf") {
		t.Fatalf("initial WireGuard start commands:\n%s", log)
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
	report, err := manager.reconcileTunnel(context.Background(), config, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "same-host WireGuard is unsupported") || !strings.Contains(report.Error, "same-host") {
		t.Fatalf("same-host reconcile = %#v, %v", report, err)
	}
	contents, readErr := os.ReadFile(path)
	if readErr != nil || string(contents) != "origin configuration must survive\n" {
		t.Fatalf("existing config after guard = %q, %v", contents, readErr)
	}
}

func TestWireGuardReconcileFastPathUsesAllPeerStatusWithoutConfigurationCommands(t *testing.T) {
	const tunnelID = "12345678-1234-4234-8234-123456789abc"
	stateDirectory := filepath.Join(t.TempDir(), "state")
	configDirectory := filepath.Join(t.TempDir(), "configs")
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	commandLog := filepath.Join(t.TempDir(), "commands.log")
	wgPath := writeWireGuardTestCommand(t, filepath.Dir(commandLog), "wg", commandLog, fmt.Sprintf(`
if [ "$1" = "show" ] && [ "$2" = "all" ]; then
  case "$3" in
    latest-handshakes) printf 'scwg1234567812\t%s\t1700000000\n' %q ;;
    transfer) printf 'scwg1234567812\t%s\t1234\t5678\n' %q ;;
  esac
fi
exit 0`, wireGuardEdgeTestKey(1), wireGuardEdgeTestKey(1), wireGuardEdgeTestKey(1), wireGuardEdgeTestKey(1)))
	manager := &linuxWireGuardManager{
		stateDirectory: stateDirectory, configDirectory: configDirectory, wgPath: wgPath,
		wgQuickPath: "/usr/bin/true", ipPath: "/usr/bin/true", tcPath: "/usr/bin/true",
		resolveHostIPs: func(context.Context, string) ([]net.IP, error) {
			return nil, errors.New("fast path must not resolve the endpoint")
		},
		linkProbe: func(string) bool { return true },
	}
	config := domain.WireGuardEdgeConfig{
		TunnelID: tunnelID, Name: "origin", Revision: 3, InterfaceName: domain.WireGuardInterfaceName(tunnelID),
		Address: "10.253.30.2/32", OriginAddress: "10.253.30.1", OriginPublicKey: wireGuardEdgeTestKey(1),
		Endpoint: "origin.example.test:51820", MTU: 1420, PersistentKeepaliveSecs: 25,
		PerformancePort: 5201, DirectPerformanceHost: "origin.example.test",
	}
	manager.appliedEgressLimitsMbps = map[string]int{tunnelID: 0}
	privateKey := wireGuardEdgeTestKey(9)
	if err := os.WriteFile(manager.keyPath(tunnelID), []byte(privateKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.configPath(config), renderWireGuardEdgeConfig(config, privateKey), 0o600); err != nil {
		t.Fatal(err)
	}

	reports, err := manager.Reconcile(context.Background(), []domain.WireGuardEdgeConfig{config})
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].RXBytes != 1234 || reports[0].TXBytes != 5678 ||
		reports[0].LatestHandshake == nil || !reports[0].LatestHandshake.Equal(time.Unix(1700000000, 0).UTC()) {
		t.Fatalf("fast path reports = %#v", reports)
	}
	log := readWireGuardTestFile(t, commandLog)
	for _, expected := range []string{
		"wg show all latest-handshakes",
		"wg show all transfer",
	} {
		if !strings.Contains(log, expected) {
			t.Fatalf("fast path command log is missing %q:\n%s", expected, log)
		}
	}
	for _, forbidden := range []string{"ip link show", "wg syncconf", "wg show scwg1234567812 latest-handshakes", "wg show scwg1234567812 transfer"} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("fast path command log contains %q:\n%s", forbidden, log)
		}
	}
}

func TestWireGuardPeerStatusFromAllParsesInterfacePrefixedOutput(t *testing.T) {
	config := domain.WireGuardEdgeConfig{
		InterfaceName: "scwg1234567812", OriginPublicKey: wireGuardEdgeTestKey(1),
	}
	handshakes := []byte("scwg1234567812\t" + wireGuardEdgeTestKey(1) + "\t1700000000\n")
	transfers := []byte("scwg1234567812\t" + wireGuardEdgeTestKey(1) + "\t1234\t5678\n")
	handshake, rxBytes, txBytes, err := readPeerStatusFromAll(config, handshakes, transfers)
	if err != nil {
		t.Fatal(err)
	}
	if handshake == nil || !handshake.Equal(time.Unix(1700000000, 0).UTC()) || rxBytes != 1234 || txBytes != 5678 {
		t.Fatalf("all-interface peer status = %v %d %d", handshake, rxBytes, txBytes)
	}
	if _, _, _, err := readPeerStatusFromAll(config, nil, nil); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("missing transfer error = %v", err)
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

func writeWireGuardTestCommand(t *testing.T, directory, name, logPath, body string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"%s $*\" >> %q\n%s\n", name, logPath, body)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func readWireGuardTestFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}
