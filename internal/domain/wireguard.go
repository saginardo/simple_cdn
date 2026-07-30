package domain

import (
	"encoding/base64"
	"fmt"
	"math"
	"net"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	DefaultWireGuardListenPort          = 51820
	DefaultWireGuardMTU                 = 1420
	DefaultWireGuardKeepaliveSeconds    = 25
	DefaultWireGuardPerformancePort     = 5201
	DefaultWireGuardPerformanceMbps     = 100
	DefaultWireGuardPerformanceDuration = 10
	MaxWireGuardPeersPerTunnel          = 253
	MaxWireGuardEgressLimitMbps         = 10_000
	WireGuardHandshakeFreshness         = 3 * time.Minute
	wireGuardHandshakeFutureTolerance   = 30 * time.Second
)

type WireGuardTunnel struct {
	ID                       string          `json:"id"`
	Name                     string          `json:"name"`
	EndpointHost             string          `json:"endpoint_host"`
	ListenPort               int             `json:"listen_port"`
	AddressCIDR              string          `json:"address_cidr"`
	OriginAddress            string          `json:"origin_address"`
	MTU                      int             `json:"mtu"`
	PersistentKeepaliveSecs  int             `json:"persistent_keepalive_seconds"`
	PerformancePort          int             `json:"performance_port"`
	OriginEgressLimitMbps    int             `json:"origin_egress_limit_mbps"`
	OriginPublicKey          string          `json:"origin_public_key,omitempty"`
	Revision                 int64           `json:"revision"`
	OriginConfiguredRevision int64           `json:"origin_configured_revision"`
	OriginConfiguredAt       *time.Time      `json:"origin_configured_at,omitempty"`
	Peers                    []WireGuardPeer `json:"peers"`
	CreatedAt                time.Time       `json:"created_at"`
	UpdatedAt                time.Time       `json:"updated_at"`
}

type WireGuardPeer struct {
	NodeID              string     `json:"node_id"`
	NodeName            string     `json:"node_name"`
	NodePublicIPv4      string     `json:"node_public_ipv4"`
	Address             string     `json:"address"`
	EdgeEgressLimitMbps int        `json:"edge_egress_limit_mbps"`
	PublicKey           string     `json:"public_key,omitempty"`
	AppliedRevision     int64      `json:"applied_revision"`
	LatestHandshakeAt   *time.Time `json:"latest_handshake_at,omitempty"`
	RXBytes             int64      `json:"rx_bytes"`
	TXBytes             int64      `json:"tx_bytes"`
	LastReportedAt      *time.Time `json:"last_reported_at,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
}

type WireGuardEdgeConfig struct {
	TunnelID                string `json:"tunnel_id"`
	Name                    string `json:"name"`
	Revision                int64  `json:"revision"`
	InterfaceName           string `json:"interface_name"`
	Address                 string `json:"address"`
	OriginAddress           string `json:"origin_address"`
	OriginPublicKey         string `json:"origin_public_key,omitempty"`
	Endpoint                string `json:"endpoint"`
	MTU                     int    `json:"mtu"`
	PersistentKeepaliveSecs int    `json:"persistent_keepalive_seconds"`
	PerformancePort         int    `json:"performance_port"`
	DirectPerformanceHost   string `json:"direct_performance_host"`
	EdgeEgressLimitMbps     int    `json:"edge_egress_limit_mbps"`
}

type WireGuardPeerReport struct {
	TunnelID        string     `json:"tunnel_id"`
	Revision        int64      `json:"revision"`
	InterfaceName   string     `json:"interface_name"`
	PublicKey       string     `json:"public_key"`
	LatestHandshake *time.Time `json:"latest_handshake_at,omitempty"`
	RXBytes         int64      `json:"rx_bytes"`
	TXBytes         int64      `json:"tx_bytes"`
	Error           string     `json:"error,omitempty"`
}

type WireGuardPerformanceStatus string

const (
	WireGuardPerformanceQueued    WireGuardPerformanceStatus = "queued"
	WireGuardPerformanceRunning   WireGuardPerformanceStatus = "running"
	WireGuardPerformanceSucceeded WireGuardPerformanceStatus = "succeeded"
	WireGuardPerformanceFailed    WireGuardPerformanceStatus = "failed"
)

type WireGuardTCPMeasurement struct {
	Mbps        float64 `json:"mbps"`
	Retransmits int64   `json:"retransmits"`
}

type WireGuardUDPMeasurement struct {
	TargetMbps   int     `json:"target_mbps"`
	Mbps         float64 `json:"mbps"`
	LostPackets  int64   `json:"lost_packets"`
	TotalPackets int64   `json:"total_packets"`
	LossPercent  float64 `json:"loss_percent"`
	JitterMS     float64 `json:"jitter_ms"`
}

type WireGuardPerformanceResult struct {
	DirectTCP           *WireGuardTCPMeasurement `json:"direct_tcp,omitempty"`
	DirectTCPReverse    *WireGuardTCPMeasurement `json:"direct_tcp_reverse,omitempty"`
	WireGuardTCP        *WireGuardTCPMeasurement `json:"wireguard_tcp,omitempty"`
	WireGuardTCPReverse *WireGuardTCPMeasurement `json:"wireguard_tcp_reverse,omitempty"`
	WireGuardUDP        *WireGuardUDPMeasurement `json:"wireguard_udp,omitempty"`
	WireGuardUDPReverse *WireGuardUDPMeasurement `json:"wireguard_udp_reverse,omitempty"`
}

type WireGuardPerformanceTest struct {
	ID              string                      `json:"id"`
	TunnelID        string                      `json:"tunnel_id"`
	TunnelName      string                      `json:"tunnel_name"`
	NodeID          string                      `json:"node_id"`
	NodeName        string                      `json:"node_name"`
	TargetMbps      int                         `json:"target_mbps"`
	DurationSeconds int                         `json:"duration_seconds"`
	Status          WireGuardPerformanceStatus  `json:"status"`
	Result          *WireGuardPerformanceResult `json:"result,omitempty"`
	Error           string                      `json:"error,omitempty"`
	CreatedAt       time.Time                   `json:"created_at"`
	StartedAt       *time.Time                  `json:"started_at,omitempty"`
	FinishedAt      *time.Time                  `json:"finished_at,omitempty"`
}

func NormalizeAndValidateWireGuardTunnel(tunnel *WireGuardTunnel) error {
	tunnel.Name = strings.TrimSpace(tunnel.Name)
	if tunnel.Name == "" || len(tunnel.Name) > 100 || containsControl(tunnel.Name) {
		return fmt.Errorf("WireGuard tunnel name must be between 1 and 100 printable characters")
	}
	tunnel.EndpointHost = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(tunnel.EndpointHost), "."))
	if !ValidHostname(tunnel.EndpointHost) {
		return fmt.Errorf("WireGuard endpoint must be an IPv4 address or DNS hostname")
	}
	if ip := net.ParseIP(tunnel.EndpointHost); ip != nil && ip.To4() == nil {
		return fmt.Errorf("WireGuard endpoint currently supports IPv4 only")
	}
	if tunnel.ListenPort == 0 {
		tunnel.ListenPort = DefaultWireGuardListenPort
	}
	if tunnel.ListenPort < 1 || tunnel.ListenPort > 65535 {
		return fmt.Errorf("WireGuard listen port must be between 1 and 65535")
	}
	if tunnel.PerformancePort == 0 {
		tunnel.PerformancePort = DefaultWireGuardPerformancePort
	}
	if tunnel.PerformancePort < 1 || tunnel.PerformancePort > 65535 || tunnel.PerformancePort == tunnel.ListenPort {
		return fmt.Errorf("performance port must be between 1 and 65535 and differ from the WireGuard port")
	}
	if err := ValidateWireGuardEgressLimit(tunnel.OriginEgressLimitMbps); err != nil {
		return fmt.Errorf("origin egress limit: %w", err)
	}
	if tunnel.MTU == 0 {
		tunnel.MTU = DefaultWireGuardMTU
	}
	if tunnel.MTU < 1280 || tunnel.MTU > 1500 {
		return fmt.Errorf("WireGuard MTU must be between 1280 and 1500")
	}
	if tunnel.PersistentKeepaliveSecs < 0 || tunnel.PersistentKeepaliveSecs > 120 {
		return fmt.Errorf("WireGuard persistent keepalive must be between 0 and 120 seconds")
	}
	if tunnel.PersistentKeepaliveSecs == 0 {
		tunnel.PersistentKeepaliveSecs = DefaultWireGuardKeepaliveSeconds
	}
	networkIP, network, err := net.ParseCIDR(strings.TrimSpace(tunnel.AddressCIDR))
	if err != nil || networkIP.To4() == nil {
		return fmt.Errorf("WireGuard address range must be an IPv4 CIDR")
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones < 16 || ones > 28 || !networkIP.Equal(network.IP) || !networkIP.IsPrivate() {
		return fmt.Errorf("WireGuard address range must be a private IPv4 network between /16 and /28")
	}
	tunnel.AddressCIDR = network.String()
	origin := nextIPv4(network.IP)
	if origin == nil || !network.Contains(origin) {
		return fmt.Errorf("WireGuard address range has no usable origin address")
	}
	tunnel.OriginAddress = origin.String()
	if tunnel.OriginPublicKey != "" && !ValidWireGuardKey(tunnel.OriginPublicKey) {
		return fmt.Errorf("invalid WireGuard origin public key")
	}
	return nil
}

func ValidateWireGuardEgressLimit(limitMbps int) error {
	if limitMbps < 0 || limitMbps > MaxWireGuardEgressLimitMbps {
		return fmt.Errorf("must be between 0 and %d Mbps", MaxWireGuardEgressLimitMbps)
	}
	return nil
}

func WireGuardHandshakeFresh(handshake *time.Time, current time.Time) bool {
	if handshake == nil || handshake.IsZero() {
		return false
	}
	age := current.Sub(*handshake)
	return age >= -wireGuardHandshakeFutureTolerance && age <= WireGuardHandshakeFreshness
}

func ValidateWireGuardPerformanceRequest(targetMbps, durationSeconds int) error {
	if targetMbps == 0 {
		targetMbps = DefaultWireGuardPerformanceMbps
	}
	if durationSeconds == 0 {
		durationSeconds = DefaultWireGuardPerformanceDuration
	}
	if targetMbps < 1 || targetMbps > 10_000 {
		return fmt.Errorf("performance target must be between 1 and 10000 Mbps")
	}
	if durationSeconds < 3 || durationSeconds > 60 {
		return fmt.Errorf("performance duration must be between 3 and 60 seconds")
	}
	return nil
}

func ValidWireGuardKey(value string) bool {
	decoded, err := base64.StdEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.StdEncoding.EncodeToString(decoded) == value
}

func ValidWireGuardPeerReport(report WireGuardPeerReport) bool {
	if report.TunnelID == "" || report.Revision < 1 || report.InterfaceName != WireGuardInterfaceName(report.TunnelID) ||
		!ValidWireGuardKey(report.PublicKey) || report.RXBytes < 0 || report.TXBytes < 0 || len(report.Error) > 512 || containsControl(report.Error) {
		return false
	}
	return report.LatestHandshake == nil || !report.LatestHandshake.IsZero()
}

func ValidWireGuardPerformanceResult(result WireGuardPerformanceResult) bool {
	validTCP := func(value *WireGuardTCPMeasurement) bool {
		return value == nil || finiteRange(value.Mbps, 0, 1_000_000) && value.Retransmits >= 0
	}
	for _, value := range []*WireGuardTCPMeasurement{
		result.DirectTCP, result.DirectTCPReverse, result.WireGuardTCP, result.WireGuardTCPReverse,
	} {
		if !validTCP(value) {
			return false
		}
	}
	for _, value := range []*WireGuardUDPMeasurement{result.WireGuardUDP, result.WireGuardUDPReverse} {
		if value == nil {
			continue
		}
		if value.TargetMbps < 1 || value.TargetMbps > 10_000 || !finiteRange(value.Mbps, 0, 1_000_000) ||
			value.LostPackets < 0 || value.TotalPackets < 0 || value.LostPackets > value.TotalPackets ||
			!finiteRange(value.LossPercent, 0, 100) || !finiteRange(value.JitterMS, 0, 60_000) {
			return false
		}
	}
	return result.DirectTCP != nil || result.DirectTCPReverse != nil || result.WireGuardTCP != nil ||
		result.WireGuardTCPReverse != nil || result.WireGuardUDP != nil || result.WireGuardUDPReverse != nil
}

func WireGuardInterfaceName(tunnelID string) string {
	compact := strings.ReplaceAll(strings.ToLower(tunnelID), "-", "")
	if len(compact) > 10 {
		compact = compact[:10]
	}
	return "scwg" + compact
}

func WireGuardTableName(tunnelID string) string {
	return "scwg_" + strings.TrimPrefix(WireGuardInterfaceName(tunnelID), "scwg")
}

func AllocateWireGuardPeerAddresses(cidr string, nodeIDs []string, existing map[string]string) (map[string]string, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil || network.IP.To4() == nil {
		return nil, fmt.Errorf("invalid WireGuard IPv4 CIDR")
	}
	if len(nodeIDs) > MaxWireGuardPeersPerTunnel {
		return nil, fmt.Errorf("a WireGuard tunnel supports at most %d edge peers", MaxWireGuardPeersPerTunnel)
	}
	wanted := append([]string(nil), nodeIDs...)
	sort.Strings(wanted)
	used := map[string]bool{network.IP.String(): true}
	origin := nextIPv4(network.IP)
	if origin == nil {
		return nil, fmt.Errorf("invalid WireGuard address range")
	}
	used[origin.String()] = true
	result := make(map[string]string, len(wanted))
	for _, nodeID := range wanted {
		if address := net.ParseIP(existing[nodeID]); address != nil && address.To4() != nil && network.Contains(address) && !used[address.String()] {
			result[nodeID] = address.String()
			used[address.String()] = true
		}
	}
	candidate := nextIPv4(origin)
	for _, nodeID := range wanted {
		if result[nodeID] != "" {
			continue
		}
		for candidate != nil && network.Contains(candidate) && used[candidate.String()] {
			candidate = nextIPv4(candidate)
		}
		if candidate == nil || !network.Contains(candidate) || isLastIPv4(candidate, network) {
			return nil, fmt.Errorf("WireGuard address range %s is too small for %d peers", cidr, len(wanted))
		}
		result[nodeID] = candidate.String()
		used[candidate.String()] = true
		candidate = nextIPv4(candidate)
	}
	return result, nil
}

func nextIPv4(ip net.IP) net.IP {
	value := append(net.IP(nil), ip.To4()...)
	if value == nil {
		return nil
	}
	for index := len(value) - 1; index >= 0; index-- {
		value[index]++
		if value[index] != 0 {
			return value
		}
	}
	return nil
}

func isLastIPv4(ip net.IP, network *net.IPNet) bool {
	value := ip.To4()
	base := network.IP.To4()
	if value == nil || base == nil {
		return false
	}
	for index := range value {
		if value[index] != base[index]|^network.Mask[index] {
			return false
		}
	}
	return true
}

func finiteRange(value, minimum, maximum float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= minimum && value <= maximum
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}
