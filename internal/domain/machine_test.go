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
