package domain

import (
	"math"
	"strings"
	"time"
	"unicode"
)

// MachineStatus is the latest host-level snapshot collected by an edge agent.
// CPU and network rates are averages over SampleSeconds; a zero interval means
// the agent has not collected a second sample yet.
type MachineStatus struct {
	Distribution         string              `json:"distribution"`
	Version              string              `json:"version"`
	UptimeSeconds        int64               `json:"uptime_seconds"`
	Load1                float64             `json:"load_1"`
	Load5                float64             `json:"load_5"`
	Load15               float64             `json:"load_15"`
	CPUUsagePercent      float64             `json:"cpu_usage_percent"`
	CPULogicalCores      int                 `json:"cpu_logical_cores"`
	MemoryUsedBytes      int64               `json:"memory_used_bytes"`
	MemoryTotalBytes     int64               `json:"memory_total_bytes"`
	DiskUsedBytes        int64               `json:"disk_used_bytes"`
	DiskTotalBytes       int64               `json:"disk_total_bytes"`
	NetworkInterface     string              `json:"network_interface"`
	NetworkRXBytesPerSec int64               `json:"network_rx_bytes_per_second"`
	NetworkTXBytesPerSec int64               `json:"network_tx_bytes_per_second"`
	SampleSeconds        float64             `json:"sample_seconds"`
	OriginProbes         []OriginProbeStatus `json:"origin_probes,omitempty"`
	Nginx                *NginxRuntimeStatus `json:"nginx,omitempty"`
	CollectedAt          time.Time           `json:"collected_at"`
}

// NginxRuntimeStatus is an ephemeral ngx_http_stub_status snapshot collected
// through the edge-local Unix socket. It is never persisted by the control plane.
type NginxRuntimeStatus struct {
	ActiveConnections   int64 `json:"active_connections"`
	AcceptedConnections int64 `json:"accepted_connections"`
	HandledConnections  int64 `json:"handled_connections"`
	Requests            int64 `json:"requests"`
	Reading             int64 `json:"reading"`
	Writing             int64 `json:"writing"`
	Waiting             int64 `json:"waiting"`
}

func ValidMachineStatus(status MachineStatus) bool {
	const (
		maxBytes   int64   = 1 << 60
		maxUptime  int64   = 100 * 366 * 24 * 60 * 60
		maxLoad    float64 = 1_000_000
		maxSample  float64 = 24 * 60 * 60
		maxCPUCore         = 1 << 16
	)
	return validMachineText(status.Distribution, 128, true) &&
		validMachineText(status.Version, 128, true) &&
		validMachineText(status.NetworkInterface, 64, true) &&
		status.UptimeSeconds >= 0 && status.UptimeSeconds <= maxUptime &&
		validMachineFloat(status.Load1, 0, maxLoad) &&
		validMachineFloat(status.Load5, 0, maxLoad) &&
		validMachineFloat(status.Load15, 0, maxLoad) &&
		validMachineFloat(status.CPUUsagePercent, 0, 100) &&
		status.CPULogicalCores > 0 && status.CPULogicalCores <= maxCPUCore &&
		validMachineCapacity(status.MemoryUsedBytes, status.MemoryTotalBytes, maxBytes) &&
		validMachineCapacity(status.DiskUsedBytes, status.DiskTotalBytes, maxBytes) &&
		status.NetworkRXBytesPerSec >= 0 && status.NetworkRXBytesPerSec <= maxBytes &&
		status.NetworkTXBytesPerSec >= 0 && status.NetworkTXBytesPerSec <= maxBytes &&
		validMachineFloat(status.SampleSeconds, 0, maxSample) &&
		!status.CollectedAt.IsZero() && validOriginProbeStatuses(status.OriginProbes) &&
		(status.Nginx == nil || ValidNginxRuntimeStatus(*status.Nginx))
}

func ValidNginxRuntimeStatus(status NginxRuntimeStatus) bool {
	const maxCounter int64 = 1<<53 - 1
	values := [...]int64{
		status.ActiveConnections, status.AcceptedConnections, status.HandledConnections,
		status.Requests, status.Reading, status.Writing, status.Waiting,
	}
	for _, value := range values {
		if value < 0 || value > maxCounter {
			return false
		}
	}
	// Active connections are process-wide and can include stream clients or
	// connections still negotiating TLS. The state counters cover HTTP only.
	httpConnections := status.Reading + status.Writing + status.Waiting
	return status.HandledConnections <= status.AcceptedConnections &&
		httpConnections <= status.ActiveConnections
}

func validOriginProbeStatuses(statuses []OriginProbeStatus) bool {
	if len(statuses) > MaxOriginPools {
		return false
	}
	seen := make(map[string]bool, len(statuses))
	for _, status := range statuses {
		if !ValidOriginProbeStatus(status) || seen[status.PoolID] {
			return false
		}
		seen[status.PoolID] = true
	}
	return true
}

func validMachineText(value string, maximum int, required bool) bool {
	if value != strings.TrimSpace(value) || len(value) > maximum || required && value == "" {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validMachineFloat(value, minimum, maximum float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= minimum && value <= maximum
}

func validMachineCapacity(used, total, maximum int64) bool {
	return used >= 0 && total > 0 && used <= total && total <= maximum
}
