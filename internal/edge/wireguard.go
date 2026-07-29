package edge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/curve25519"
	"simple_cdn/internal/domain"
)

type WireGuardManager interface {
	Available() (wireGuard bool, performance bool)
	Reconcile(context.Context, []domain.WireGuardEdgeConfig) ([]domain.WireGuardPeerReport, error)
	RunPerformance(context.Context, domain.WireGuardPerformanceTest, domain.WireGuardEdgeConfig) (*domain.WireGuardPerformanceResult, error)
}

type linuxWireGuardManager struct {
	stateDirectory  string
	configDirectory string
	wgPath          string
	wgQuickPath     string
	ipPath          string
	iperf3Path      string
	mu              sync.Mutex
}

type wireGuardManagedState struct {
	TunnelIDs []string `json:"tunnel_ids"`
}

func newLinuxWireGuardManager(stateDirectory, configDirectory string) WireGuardManager {
	manager := &linuxWireGuardManager{
		stateDirectory:  filepath.Join(stateDirectory, "wireguard"),
		configDirectory: configDirectory,
	}
	manager.wgPath, _ = exec.LookPath("wg")
	manager.wgQuickPath, _ = exec.LookPath("wg-quick")
	manager.ipPath, _ = exec.LookPath("ip")
	manager.iperf3Path, _ = exec.LookPath("iperf3")
	return manager
}

func (m *linuxWireGuardManager) Available() (bool, bool) {
	wireGuard := m.wgPath != "" && m.wgQuickPath != "" && m.ipPath != ""
	return wireGuard, wireGuard && m.iperf3Path != ""
}

func (m *linuxWireGuardManager) Reconcile(ctx context.Context, configs []domain.WireGuardEdgeConfig) ([]domain.WireGuardPeerReport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	available, _ := m.Available()
	if !available {
		return nil, errors.New("WireGuard tooling is unavailable")
	}
	if len(configs) > domain.MaxWireGuardPeersPerTunnel {
		return nil, errors.New("control returned too many WireGuard tunnels")
	}
	if err := os.MkdirAll(m.stateDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create WireGuard state directory: %w", err)
	}
	if err := os.MkdirAll(m.configDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create WireGuard configuration directory: %w", err)
	}

	previous, stateErr := m.loadManagedState()
	desired := make(map[string]bool, len(configs))
	reports := make([]domain.WireGuardPeerReport, 0, len(configs))
	var problems []error
	if stateErr != nil {
		problems = append(problems, stateErr)
	}
	for _, config := range configs {
		if err := validateWireGuardEdgeConfig(config); err != nil {
			problems = append(problems, fmt.Errorf("invalid tunnel %q: %w", config.TunnelID, err))
			continue
		}
		if desired[config.TunnelID] {
			problems = append(problems, fmt.Errorf("control returned duplicate WireGuard tunnel %s", config.TunnelID))
			continue
		}
		desired[config.TunnelID] = true
		report, err := m.reconcileTunnel(ctx, config)
		if err != nil {
			problems = append(problems, fmt.Errorf("reconcile %s: %w", config.Name, err))
		}
		if domain.ValidWireGuardPeerReport(report) {
			reports = append(reports, report)
		}
	}
	for _, tunnelID := range previous.TunnelIDs {
		if desired[tunnelID] {
			continue
		}
		if err := m.removeTunnel(ctx, tunnelID); err != nil {
			problems = append(problems, err)
		}
	}
	wantedIDs := make([]string, 0, len(desired))
	for tunnelID := range desired {
		wantedIDs = append(wantedIDs, tunnelID)
	}
	if err := m.saveManagedState(wantedIDs); err != nil {
		problems = append(problems, err)
	}
	return reports, errors.Join(problems...)
}

func validateWireGuardEdgeConfig(config domain.WireGuardEdgeConfig) error {
	if _, err := uuid.Parse(config.TunnelID); err != nil {
		return errors.New("tunnel ID is invalid")
	}
	if config.InterfaceName != domain.WireGuardInterfaceName(config.TunnelID) {
		return errors.New("interface name does not match the tunnel ID")
	}
	if config.Revision < 1 {
		return errors.New("revision is invalid")
	}
	address, network, err := net.ParseCIDR(config.Address)
	if err != nil || address.To4() == nil {
		return errors.New("edge address is not an IPv4 CIDR")
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones != 32 || !address.IsPrivate() {
		return errors.New("edge address must be a private IPv4 /32")
	}
	origin := net.ParseIP(config.OriginAddress)
	if origin == nil || origin.To4() == nil || !origin.IsPrivate() || origin.Equal(address) {
		return errors.New("origin tunnel address is invalid")
	}
	if config.OriginPublicKey != "" && !domain.ValidWireGuardKey(config.OriginPublicKey) {
		return errors.New("origin public key is invalid")
	}
	host, portValue, err := net.SplitHostPort(config.Endpoint)
	if err != nil || !domain.ValidHostname(strings.TrimSuffix(host, ".")) {
		return errors.New("origin endpoint is invalid")
	}
	port, err := strconv.Atoi(portValue)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("origin endpoint port is invalid")
	}
	if !strings.EqualFold(strings.TrimSuffix(host, "."), strings.TrimSuffix(config.DirectPerformanceHost, ".")) {
		return errors.New("direct performance host does not match the WireGuard endpoint")
	}
	if config.MTU < 1280 || config.MTU > 1500 || config.PersistentKeepaliveSecs < 0 || config.PersistentKeepaliveSecs > 120 {
		return errors.New("MTU or persistent keepalive is invalid")
	}
	if config.PerformancePort < 1 || config.PerformancePort > 65535 || config.PerformancePort == port {
		return errors.New("performance port is invalid")
	}
	return nil
}

func (m *linuxWireGuardManager) reconcileTunnel(ctx context.Context, config domain.WireGuardEdgeConfig) (domain.WireGuardPeerReport, error) {
	privateKey, publicKey, err := m.loadOrCreateKey(config.TunnelID)
	report := domain.WireGuardPeerReport{
		TunnelID: config.TunnelID, Revision: config.Revision, InterfaceName: config.InterfaceName,
		PublicKey: publicKey,
	}
	if err != nil {
		return report, err
	}
	if config.OriginPublicKey == "" {
		if m.interfaceExists(ctx, config.InterfaceName) {
			if downErr := m.run(ctx, m.wgQuickPath, "down", m.configPath(config)); downErr != nil {
				report.Error = wireGuardErrorDetail(downErr)
				return report, downErr
			}
		}
		report.Error = "waiting for the origin WireGuard public key"
		return report, nil
	}
	contents := renderWireGuardEdgeConfig(config, privateKey)
	if err := m.applyConfig(ctx, config, contents); err != nil {
		report.Error = wireGuardErrorDetail(err)
		return report, err
	}
	handshake, rxBytes, txBytes, err := m.readPeerStatus(ctx, config)
	if err != nil {
		report.Error = wireGuardErrorDetail(err)
		return report, err
	}
	report.LatestHandshake = handshake
	report.RXBytes = rxBytes
	report.TXBytes = txBytes
	return report, nil
}

func (m *linuxWireGuardManager) loadOrCreateKey(tunnelID string) (string, string, error) {
	path := m.keyPath(tunnelID)
	contents, err := os.ReadFile(path)
	if err == nil {
		privateKey := strings.TrimSpace(string(contents))
		publicKey, keyErr := wireGuardPublicKey(privateKey)
		if keyErr != nil {
			return "", "", fmt.Errorf("read local WireGuard key: %w", keyErr)
		}
		return privateKey, publicKey, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("read local WireGuard key: %w", err)
	}
	raw := make([]byte, curve25519.ScalarSize)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate local WireGuard key: %w", err)
	}
	raw[0] &= 248
	raw[31] &= 127
	raw[31] |= 64
	privateKey := base64.StdEncoding.EncodeToString(raw)
	publicKey, err := wireGuardPublicKey(privateKey)
	if err != nil {
		return "", "", err
	}
	if err := atomicWriteFile(path, []byte(privateKey+"\n"), 0o600); err != nil {
		return "", "", fmt.Errorf("persist local WireGuard key: %w", err)
	}
	return privateKey, publicKey, nil
}

func wireGuardPublicKey(privateKey string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(privateKey))
	if err != nil || len(raw) != curve25519.ScalarSize {
		return "", errors.New("private key is invalid")
	}
	publicKey, err := curve25519.X25519(raw, curve25519.Basepoint)
	if err != nil {
		return "", fmt.Errorf("derive public key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(publicKey), nil
}

func renderWireGuardEdgeConfig(config domain.WireGuardEdgeConfig, privateKey string) []byte {
	var contents strings.Builder
	contents.WriteString("# Managed by simple_cdn. Local changes will be replaced.\n")
	contents.WriteString("[Interface]\nPrivateKey = ")
	contents.WriteString(privateKey)
	contents.WriteString("\nAddress = ")
	contents.WriteString(config.Address)
	contents.WriteString("\nMTU = ")
	contents.WriteString(strconv.Itoa(config.MTU))
	contents.WriteString("\n\n[Peer]\nPublicKey = ")
	contents.WriteString(config.OriginPublicKey)
	contents.WriteString("\nAllowedIPs = ")
	contents.WriteString(config.OriginAddress)
	contents.WriteString("/32\nEndpoint = ")
	contents.WriteString(config.Endpoint)
	contents.WriteString("\nPersistentKeepalive = ")
	contents.WriteString(strconv.Itoa(config.PersistentKeepaliveSecs))
	contents.WriteByte('\n')
	return []byte(contents.String())
}

func (m *linuxWireGuardManager) applyConfig(ctx context.Context, config domain.WireGuardEdgeConfig, contents []byte) error {
	path := m.configPath(config)
	previous, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("read current configuration: %w", readErr)
	}
	exists := m.interfaceExists(ctx, config.InterfaceName)
	if readErr == nil && bytes.Equal(previous, contents) && exists {
		return nil
	}
	if exists {
		if err := m.run(ctx, m.wgQuickPath, "down", path); err != nil {
			return fmt.Errorf("stop interface before update: %w", err)
		}
	}
	if err := atomicWriteFile(path, contents, 0o600); err != nil {
		if readErr == nil {
			_ = m.restoreConfig(ctx, path, previous)
		}
		return fmt.Errorf("write WireGuard configuration: %w", err)
	}
	if err := m.run(ctx, m.wgQuickPath, "up", path); err != nil {
		_ = m.run(ctx, m.wgQuickPath, "down", path)
		if readErr == nil {
			if rollbackErr := m.restoreConfig(ctx, path, previous); rollbackErr != nil {
				return fmt.Errorf("start WireGuard interface: %v; rollback failed: %w", err, rollbackErr)
			}
		}
		return fmt.Errorf("start WireGuard interface: %w", err)
	}
	return nil
}

func (m *linuxWireGuardManager) restoreConfig(ctx context.Context, path string, contents []byte) error {
	if err := atomicWriteFile(path, contents, 0o600); err != nil {
		return err
	}
	return m.run(ctx, m.wgQuickPath, "up", path)
}

func (m *linuxWireGuardManager) readPeerStatus(ctx context.Context, config domain.WireGuardEdgeConfig) (*time.Time, int64, int64, error) {
	handshakeOutput, err := m.runOutput(ctx, m.wgPath, "show", config.InterfaceName, "latest-handshakes")
	if err != nil {
		return nil, 0, 0, fmt.Errorf("read latest handshake: %w", err)
	}
	var handshake *time.Time
	for _, line := range strings.Split(strings.TrimSpace(string(handshakeOutput)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != config.OriginPublicKey {
			continue
		}
		seconds, parseErr := strconv.ParseInt(fields[1], 10, 64)
		if parseErr != nil || seconds < 0 {
			return nil, 0, 0, errors.New("wg returned an invalid handshake timestamp")
		}
		if seconds > 0 {
			value := time.Unix(seconds, 0).UTC()
			handshake = &value
		}
	}
	transferOutput, err := m.runOutput(ctx, m.wgPath, "show", config.InterfaceName, "transfer")
	if err != nil {
		return nil, 0, 0, fmt.Errorf("read peer transfer counters: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(transferOutput)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != config.OriginPublicKey {
			continue
		}
		rxBytes, rxErr := strconv.ParseInt(fields[1], 10, 64)
		txBytes, txErr := strconv.ParseInt(fields[2], 10, 64)
		if rxErr != nil || txErr != nil || rxBytes < 0 || txBytes < 0 {
			return nil, 0, 0, errors.New("wg returned invalid transfer counters")
		}
		return handshake, rxBytes, txBytes, nil
	}
	return nil, 0, 0, errors.New("origin peer is absent from the WireGuard interface")
}

func (m *linuxWireGuardManager) interfaceExists(ctx context.Context, interfaceName string) bool {
	command := exec.CommandContext(ctx, m.ipPath, "link", "show", "dev", interfaceName)
	return command.Run() == nil
}

func (m *linuxWireGuardManager) removeTunnel(ctx context.Context, tunnelID string) error {
	if _, err := uuid.Parse(tunnelID); err != nil {
		return fmt.Errorf("refuse to remove invalid managed WireGuard tunnel ID %q", tunnelID)
	}
	config := domain.WireGuardEdgeConfig{TunnelID: tunnelID, InterfaceName: domain.WireGuardInterfaceName(tunnelID)}
	path := m.configPath(config)
	if m.interfaceExists(ctx, config.InterfaceName) {
		if err := m.run(ctx, m.wgQuickPath, "down", path); err != nil {
			return fmt.Errorf("remove WireGuard tunnel %s: %w", tunnelID, err)
		}
	}
	for _, pathname := range []string{path, m.keyPath(tunnelID)} {
		if err := os.Remove(pathname); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove WireGuard tunnel file: %w", err)
		}
	}
	return nil
}

func (m *linuxWireGuardManager) loadManagedState() (wireGuardManagedState, error) {
	contents, err := os.ReadFile(m.managedStatePath())
	if errors.Is(err, os.ErrNotExist) {
		return wireGuardManagedState{}, nil
	}
	if err != nil {
		return wireGuardManagedState{}, fmt.Errorf("read WireGuard managed state: %w", err)
	}
	var state wireGuardManagedState
	if err := json.Unmarshal(contents, &state); err != nil {
		return wireGuardManagedState{}, fmt.Errorf("decode WireGuard managed state: %w", err)
	}
	return state, nil
}

func (m *linuxWireGuardManager) saveManagedState(tunnelIDs []string) error {
	sort.Strings(tunnelIDs)
	contents, err := json.Marshal(wireGuardManagedState{TunnelIDs: tunnelIDs})
	if err != nil {
		return err
	}
	if err := atomicWriteFile(m.managedStatePath(), append(contents, '\n'), 0o600); err != nil {
		return fmt.Errorf("persist WireGuard managed state: %w", err)
	}
	return nil
}

func (m *linuxWireGuardManager) configPath(config domain.WireGuardEdgeConfig) string {
	return filepath.Join(m.configDirectory, config.InterfaceName+".conf")
}

func (m *linuxWireGuardManager) keyPath(tunnelID string) string {
	return filepath.Join(m.stateDirectory, tunnelID+".key")
}

func (m *linuxWireGuardManager) managedStatePath() string {
	return filepath.Join(m.stateDirectory, "managed.json")
}

func (m *linuxWireGuardManager) run(ctx context.Context, name string, arguments ...string) error {
	_, err := m.runOutput(ctx, name, arguments...)
	return err
}

func (m *linuxWireGuardManager) runOutput(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, name, arguments...).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if len(detail) > 1000 {
			detail = detail[:1000]
		}
		if detail != "" {
			return nil, fmt.Errorf("%w: %s", err, detail)
		}
		return nil, err
	}
	return output, nil
}

func wireGuardErrorDetail(err error) string {
	detail := strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			return ' '
		}
		return character
	}, err.Error())
	detail = strings.Join(strings.Fields(detail), " ")
	if len(detail) > 512 {
		detail = detail[:512]
	}
	return detail
}

func (m *linuxWireGuardManager) RunPerformance(ctx context.Context, test domain.WireGuardPerformanceTest, config domain.WireGuardEdgeConfig) (*domain.WireGuardPerformanceResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	_, performanceAvailable := m.Available()
	if !performanceAvailable {
		return nil, errors.New("iperf3 performance tooling is unavailable")
	}
	if test.TunnelID != config.TunnelID || test.Status != domain.WireGuardPerformanceRunning {
		return nil, errors.New("performance test does not match the WireGuard configuration")
	}
	if err := domain.ValidateWireGuardPerformanceRequest(test.TargetMbps, test.DurationSeconds); err != nil {
		return nil, err
	}
	if err := validateWireGuardEdgeConfig(config); err != nil || config.OriginPublicKey == "" {
		if err == nil {
			err = errors.New("origin public key is unavailable")
		}
		return nil, fmt.Errorf("invalid WireGuard performance configuration: %w", err)
	}
	if !m.interfaceExists(ctx, config.InterfaceName) {
		return nil, errors.New("WireGuard interface is not active")
	}

	result := &domain.WireGuardPerformanceResult{}
	var problems []error
	if measurement, err := m.runIperfTCP(ctx, config.DirectPerformanceHost, config.PerformancePort, test.DurationSeconds); err != nil {
		problems = append(problems, fmt.Errorf("direct TCP: %w", err))
	} else {
		result.DirectTCP = measurement
	}
	if measurement, err := m.runIperfTCP(ctx, config.OriginAddress, config.PerformancePort, test.DurationSeconds); err != nil {
		problems = append(problems, fmt.Errorf("WireGuard TCP: %w", err))
	} else {
		result.WireGuardTCP = measurement
	}
	if measurement, err := m.runIperfUDP(ctx, config.OriginAddress, config.PerformancePort, test.TargetMbps, test.DurationSeconds); err != nil {
		problems = append(problems, fmt.Errorf("WireGuard UDP: %w", err))
	} else {
		result.WireGuardUDP = measurement
	}
	if !domain.ValidWireGuardPerformanceResult(*result) {
		result = nil
	}
	return result, errors.Join(problems...)
}

type iperfResult struct {
	Error string `json:"error"`
	End   struct {
		SumSent struct {
			BitsPerSecond float64 `json:"bits_per_second"`
			Retransmits   int64   `json:"retransmits"`
		} `json:"sum_sent"`
		Sum struct {
			BitsPerSecond float64 `json:"bits_per_second"`
			JitterMS      float64 `json:"jitter_ms"`
			LostPackets   int64   `json:"lost_packets"`
			Packets       int64   `json:"packets"`
			LostPercent   float64 `json:"lost_percent"`
		} `json:"sum"`
	} `json:"end"`
}

func (m *linuxWireGuardManager) runIperfTCP(ctx context.Context, host string, port, durationSeconds int) (*domain.WireGuardTCPMeasurement, error) {
	result, err := m.runIperf(ctx, durationSeconds, "-c", host, "-p", strconv.Itoa(port), "-t", strconv.Itoa(durationSeconds), "--json")
	if err != nil {
		return nil, err
	}
	measurement := &domain.WireGuardTCPMeasurement{Mbps: result.End.SumSent.BitsPerSecond / 1_000_000, Retransmits: result.End.SumSent.Retransmits}
	if !domain.ValidWireGuardPerformanceResult(domain.WireGuardPerformanceResult{DirectTCP: measurement}) {
		return nil, errors.New("iperf3 returned invalid TCP metrics")
	}
	return measurement, nil
}

func (m *linuxWireGuardManager) runIperfUDP(ctx context.Context, host string, port, targetMbps, durationSeconds int) (*domain.WireGuardUDPMeasurement, error) {
	result, err := m.runIperf(ctx, durationSeconds, "-c", host, "-p", strconv.Itoa(port), "-u", "-b", fmt.Sprintf("%dM", targetMbps), "-t", strconv.Itoa(durationSeconds), "--json")
	if err != nil {
		return nil, err
	}
	measurement := &domain.WireGuardUDPMeasurement{
		TargetMbps: targetMbps, Mbps: result.End.Sum.BitsPerSecond / 1_000_000,
		LostPackets: result.End.Sum.LostPackets, TotalPackets: result.End.Sum.Packets,
		LossPercent: result.End.Sum.LostPercent, JitterMS: result.End.Sum.JitterMS,
	}
	if !domain.ValidWireGuardPerformanceResult(domain.WireGuardPerformanceResult{WireGuardUDP: measurement}) {
		return nil, errors.New("iperf3 returned invalid UDP metrics")
	}
	return measurement, nil
}

func (m *linuxWireGuardManager) runIperf(ctx context.Context, durationSeconds int, arguments ...string) (iperfResult, error) {
	commandContext, cancel := context.WithTimeout(ctx, time.Duration(durationSeconds+20)*time.Second)
	defer cancel()
	output, commandErr := exec.CommandContext(commandContext, m.iperf3Path, arguments...).CombinedOutput()
	var result iperfResult
	if err := json.Unmarshal(output, &result); err != nil {
		detail := strings.TrimSpace(string(output))
		if len(detail) > 1000 {
			detail = detail[:1000]
		}
		if commandErr != nil {
			return iperfResult{}, fmt.Errorf("%w: %s", commandErr, detail)
		}
		return iperfResult{}, fmt.Errorf("decode iperf3 JSON: %w", err)
	}
	if result.Error != "" {
		return iperfResult{}, errors.New(result.Error)
	}
	if commandErr != nil {
		return iperfResult{}, commandErr
	}
	return result, nil
}
