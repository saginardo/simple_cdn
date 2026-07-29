package domain

import "testing"

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
