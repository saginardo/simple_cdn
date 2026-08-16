package domain

import (
	"math"
	"testing"
	"time"
)

func TestValidMachineStatus(t *testing.T) {
	valid := MachineStatus{
		Distribution: "Debian GNU/Linux", Version: "13.5", UptimeSeconds: 86400,
		Load1: 0.25, Load5: 0.5, Load15: 0.75, CPUUsagePercent: 42.5, CPULogicalCores: 8,
		MemoryUsedBytes: 4 << 30, MemoryTotalBytes: 8 << 30,
		DiskUsedBytes: 40 << 30, DiskTotalBytes: 100 << 30,
		NetworkInterface: "eth0", NetworkRXBytesPerSec: 1024, NetworkTXBytesPerSec: 2048,
		SampleSeconds: 30, CollectedAt: time.Now().UTC(),
		Nginx: &NginxRuntimeStatus{
			ActiveConnections: 3, AcceptedConnections: 10, HandledConnections: 10,
			Requests: 14, Reading: 1, Writing: 1, Waiting: 1,
		},
	}
	if !ValidMachineStatus(valid) {
		t.Fatalf("valid machine status was rejected: %#v", valid)
	}
	withStreamConnections := valid
	streamNginx := *valid.Nginx
	streamNginx.ActiveConnections = 6
	withStreamConnections.Nginx = &streamNginx
	if !ValidMachineStatus(withStreamConnections) {
		t.Fatalf("machine status with stream connections was rejected: %#v", withStreamConnections)
	}
	invalid := []MachineStatus{
		func() MachineStatus { value := valid; value.Distribution = ""; return value }(),
		func() MachineStatus { value := valid; value.Version = "13\n5"; return value }(),
		func() MachineStatus { value := valid; value.CPUUsagePercent = 101; return value }(),
		func() MachineStatus { value := valid; value.Load1 = math.NaN(); return value }(),
		func() MachineStatus { value := valid; value.MemoryUsedBytes = 9 << 30; return value }(),
		func() MachineStatus { value := valid; value.NetworkRXBytesPerSec = -1; return value }(),
		func() MachineStatus { value := valid; value.CollectedAt = time.Time{}; return value }(),
		func() MachineStatus {
			value := valid
			invalidNginx := *valid.Nginx
			invalidNginx.Waiting++
			value.Nginx = &invalidNginx
			return value
		}(),
	}
	for index, status := range invalid {
		if ValidMachineStatus(status) {
			t.Fatalf("invalid machine status %d was accepted: %#v", index, status)
		}
	}
}

func TestValidMachineNetworkStatusAndSamplingPolicy(t *testing.T) {
	valid := MachineNetworkStatus{
		NetworkInterface: "eth0", NetworkRXBytesPerSec: 1024, NetworkTXBytesPerSec: 2048,
		SampleSeconds: 1, CollectedAt: time.Now().UTC(),
	}
	if !ValidMachineNetworkStatus(valid) {
		t.Fatalf("valid machine network status was rejected: %#v", valid)
	}
	for index, status := range []MachineNetworkStatus{
		func() MachineNetworkStatus { value := valid; value.NetworkInterface = ""; return value }(),
		func() MachineNetworkStatus { value := valid; value.NetworkRXBytesPerSec = -1; return value }(),
		func() MachineNetworkStatus { value := valid; value.SampleSeconds = math.NaN(); return value }(),
		func() MachineNetworkStatus { value := valid; value.CollectedAt = time.Time{}; return value }(),
	} {
		if ValidMachineNetworkStatus(status) {
			t.Fatalf("invalid machine network status %d was accepted: %#v", index, status)
		}
	}
	basePolicy := MachineStatusSamplingPolicy{
		HostIntervalSeconds:   DefaultMachineStatusIntervalSeconds,
		OriginIntervalSeconds: DefaultMachineOriginIntervalSeconds,
	}
	for _, interval := range []int{0, DefaultMachineNetworkIntervalSeconds, MaximumMachineNetworkIntervalSeconds} {
		policy := basePolicy
		policy.NetworkIntervalSeconds = interval
		if !ValidMachineStatusSamplingPolicy(policy) {
			t.Fatalf("valid machine status policy interval %d was rejected", interval)
		}
	}
	for _, interval := range []int{-1, MaximumMachineNetworkIntervalSeconds + 1} {
		policy := basePolicy
		policy.NetworkIntervalSeconds = interval
		if ValidMachineStatusSamplingPolicy(policy) {
			t.Fatalf("invalid machine status policy interval %d was accepted", interval)
		}
	}
	for _, policy := range []MachineStatusSamplingPolicy{
		{OriginIntervalSeconds: DefaultMachineOriginIntervalSeconds},
		{HostIntervalSeconds: DefaultMachineStatusIntervalSeconds},
		{HostIntervalSeconds: MaximumMachineStatusIntervalSeconds + 1, OriginIntervalSeconds: DefaultMachineOriginIntervalSeconds},
		{HostIntervalSeconds: DefaultMachineStatusIntervalSeconds, OriginIntervalSeconds: MaximumMachineOriginIntervalSeconds + 1},
	} {
		if ValidMachineStatusSamplingPolicy(policy) {
			t.Fatalf("invalid machine status policy was accepted: %#v", policy)
		}
	}
	origin := MachineOriginStatus{OriginProbes: []OriginProbeStatus{}, CollectedAt: time.Now().UTC()}
	if !ValidMachineOriginStatus(origin) {
		t.Fatalf("valid machine origin status was rejected: %#v", origin)
	}
	origin.CollectedAt = time.Time{}
	if ValidMachineOriginStatus(origin) {
		t.Fatalf("machine origin status without timestamp was accepted: %#v", origin)
	}
}
