package domain

import (
	"testing"
	"time"
)

func TestWireGuardTunnelValidationAndAddressAllocation(t *testing.T) {
	for _, cidr := range []string{"10.10.0.0/16", "172.20.4.0/24", "192.168.90.0/28"} {
		tunnel := WireGuardTunnel{Name: "origin", EndpointHost: "origin.example.test", AddressCIDR: cidr}
		if err := NormalizeAndValidateWireGuardTunnel(&tunnel); err != nil {
			t.Fatalf("validate %s: %v", cidr, err)
		}
		if tunnel.ListenPort != DefaultWireGuardListenPort || tunnel.PerformancePort != DefaultWireGuardPerformancePort || tunnel.MTU != DefaultWireGuardMTU {
			t.Fatalf("defaults for %s = %#v", cidr, tunnel)
		}
	}
	invalid := WireGuardTunnel{Name: "origin", EndpointHost: "origin.example.test", AddressCIDR: "198.51.100.0/24"}
	if err := NormalizeAndValidateWireGuardTunnel(&invalid); err == nil {
		t.Fatal("public WireGuard address range was accepted")
	}
	addresses, err := AllocateWireGuardPeerAddresses("10.253.50.0/29", []string{"node-b", "node-a"}, map[string]string{"node-b": "10.253.50.5"})
	if err != nil {
		t.Fatal(err)
	}
	if addresses["node-b"] != "10.253.50.5" || addresses["node-a"] != "10.253.50.2" {
		t.Fatalf("allocated addresses = %#v", addresses)
	}
	interfaceName := WireGuardInterfaceName("12345678-1234-4234-8234-123456789abc")
	if interfaceName != "scwg1234567812" || len(interfaceName) > 15 {
		t.Fatalf("interface name = %q", interfaceName)
	}
}

func TestWireGuardEgressLimitAndHandshakeFreshness(t *testing.T) {
	if err := ValidateWireGuardEgressLimit(0); err != nil {
		t.Fatalf("unlimited egress: %v", err)
	}
	if err := ValidateWireGuardEgressLimit(MaxWireGuardEgressLimitMbps); err != nil {
		t.Fatalf("maximum egress: %v", err)
	}
	for _, invalid := range []int{-1, MaxWireGuardEgressLimitMbps + 1} {
		if err := ValidateWireGuardEgressLimit(invalid); err == nil {
			t.Fatalf("egress limit %d was accepted", invalid)
		}
	}
	current := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	fresh := current.Add(-WireGuardHandshakeFreshness)
	stale := fresh.Add(-time.Second)
	future := current.Add(time.Minute)
	if !WireGuardHandshakeFresh(&fresh, current) || WireGuardHandshakeFresh(&stale, current) || WireGuardHandshakeFresh(&future, current) {
		t.Fatal("WireGuard handshake freshness boundaries are incorrect")
	}
}

func TestWireGuardPerformanceResultAcceptsBidirectionalMeasurements(t *testing.T) {
	result := WireGuardPerformanceResult{
		DirectTCP:           &WireGuardTCPMeasurement{Mbps: 100, Retransmits: 1},
		DirectTCPReverse:    &WireGuardTCPMeasurement{Mbps: 90, Retransmits: 2},
		WireGuardTCP:        &WireGuardTCPMeasurement{Mbps: 95, Retransmits: 0},
		WireGuardTCPReverse: &WireGuardTCPMeasurement{Mbps: 85, Retransmits: 1},
		WireGuardUDP:        &WireGuardUDPMeasurement{TargetMbps: 50, Mbps: 49, LostPackets: 1, TotalPackets: 1000, LossPercent: 0.1, JitterMS: 0.4},
		WireGuardUDPReverse: &WireGuardUDPMeasurement{TargetMbps: 50, Mbps: 48, LostPackets: 2, TotalPackets: 1000, LossPercent: 0.2, JitterMS: 0.5},
	}
	if !ValidWireGuardPerformanceResult(result) {
		t.Fatalf("bidirectional result rejected: %#v", result)
	}
	result.WireGuardTCPReverse.Retransmits = -1
	if ValidWireGuardPerformanceResult(result) {
		t.Fatal("invalid reverse TCP result was accepted")
	}
}
