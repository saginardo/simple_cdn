package domain

import (
	"math"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const (
	MaxOriginPools          = 256
	MaxOriginPoolReferences = 512
)

type OriginPoolReference struct {
	SiteID string `json:"site_id"`
	Role   string `json:"role"`
}

// OriginPool describes one transport-compatible upstream connection pool.
// ConfigPath is an agent-owned include file whose server directive can be
// switched between available and down without rebuilding the desired state.
type OriginPool struct {
	ID                   string                  `json:"id"`
	Address              string                  `json:"address"`
	Scheme               string                  `json:"scheme"`
	HTTPVersion          OriginHTTPVersion       `json:"http_version,omitempty"`
	HealthCheckMethod    OriginHealthCheckMethod `json:"health_check_method,omitempty"`
	HealthCheckPath      string                  `json:"health_check_path,omitempty"`
	HostHeader           string                  `json:"host_header"`
	TLSServerName        string                  `json:"tls_server_name,omitempty"`
	ConfigPath           string                  `json:"config_path"`
	KeepaliveConnections int                     `json:"keepalive_connections"`
	References           []OriginPoolReference   `json:"references"`
}

type OriginCircuitState string

const (
	OriginCircuitClosed     OriginCircuitState = "closed"
	OriginCircuitOpen       OriginCircuitState = "open"
	OriginCircuitRecovering OriginCircuitState = "recovering"
)

type OriginProbeKind string

const (
	OriginProbeService OriginProbeKind = "service"
	OriginProbeCold    OriginProbeKind = "cold"
)

type OriginProbeSample struct {
	Healthy          bool      `json:"healthy"`
	ConnectionReused bool      `json:"connection_reused"`
	ConnectMS        float64   `json:"connect_ms"`
	TLSHandshakeMS   float64   `json:"tls_handshake_ms"`
	HeaderMS         float64   `json:"header_ms"`
	TotalMS          float64   `json:"total_ms"`
	HTTPStatus       int       `json:"http_status,omitempty"`
	Error            string    `json:"error,omitempty"`
	CheckedAt        time.Time `json:"checked_at"`
}

// OriginProbeStatus is ephemeral edge state. It travels with the real-time
// machine snapshot and is intentionally not persisted by the control plane.
type OriginProbeStatus struct {
	PoolID                      string                `json:"pool_id"`
	Address                     string                `json:"address"`
	Scheme                      string                `json:"scheme"`
	HTTPVersion                 OriginHTTPVersion     `json:"http_version,omitempty"`
	KeepaliveConnections        int                   `json:"keepalive_connections"`
	EstablishedConnections      *int64                `json:"established_connections,omitempty"`
	References                  []OriginPoolReference `json:"references"`
	Healthy                     bool                  `json:"healthy"`
	CircuitState                OriginCircuitState    `json:"circuit_state"`
	ServiceConsecutiveFailures  int                   `json:"service_consecutive_failures"`
	ServiceConsecutiveSuccesses int                   `json:"service_consecutive_successes"`
	ColdConsecutiveFailures     int                   `json:"cold_consecutive_failures"`
	ColdConsecutiveSuccesses    int                   `json:"cold_consecutive_successes"`
	ServiceProbe                *OriginProbeSample    `json:"service_probe,omitempty"`
	ColdProbe                   *OriginProbeSample    `json:"cold_probe,omitempty"`
	CheckedAt                   time.Time             `json:"checked_at"`
}

func ValidOriginPool(pool OriginPool) bool {
	if len(pool.ID) != 24 || !lowerHex(pool.ID) || pool.KeepaliveConnections < 1 || pool.KeepaliveConnections > 128 ||
		pool.ConfigPath == "" || !filepath.IsAbs(pool.ConfigPath) || filepath.Clean(pool.ConfigPath) != pool.ConfigPath ||
		len(pool.References) == 0 || len(pool.References) > MaxOriginPoolReferences {
		return false
	}
	normalizedAddress, err := NormalizeMonitoringAddress(pool.Address)
	if err != nil || normalizedAddress != pool.Address || !ValidHostHeader(pool.HostHeader) {
		return false
	}
	useTLS := false
	httpHealthCheck := false
	switch pool.Scheme {
	case "http":
		httpHealthCheck = true
		if pool.HTTPVersion != "" && pool.HTTPVersion != OriginHTTPVersionHTTP1 && pool.HTTPVersion != OriginHTTPVersionH2C {
			return false
		}
	case "https":
		httpHealthCheck = true
		useTLS = true
		if pool.HTTPVersion != "" && pool.HTTPVersion != OriginHTTPVersionHTTP1 && pool.HTTPVersion != OriginHTTPVersionHTTP2 {
			return false
		}
	case "grpc":
		if pool.HTTPVersion != "" || pool.HealthCheckMethod != "" || pool.HealthCheckPath != "" {
			return false
		}
	case "grpcs":
		useTLS = true
		if pool.HTTPVersion != "" || pool.HealthCheckMethod != "" || pool.HealthCheckPath != "" {
			return false
		}
	default:
		return false
	}
	if httpHealthCheck {
		if err := ValidateOriginHealthCheck(pool.HealthCheckMethod, pool.HealthCheckPath); err != nil {
			return false
		}
	}
	if useTLS {
		if pool.TLSServerName == "" || !ValidHostname(pool.TLSServerName) {
			return false
		}
	} else if pool.TLSServerName != "" {
		return false
	}
	seen := make(map[string]bool, len(pool.References))
	for _, reference := range pool.References {
		if !validOriginReference(reference) {
			return false
		}
		key := reference.SiteID + "\x00" + reference.Role
		if seen[key] {
			return false
		}
		seen[key] = true
	}
	return true
}

func ValidOriginProbeStatus(status OriginProbeStatus) bool {
	if status.PoolID == "" || len(status.PoolID) != 24 || !lowerHex(status.PoolID) ||
		status.KeepaliveConnections < 1 || status.KeepaliveConnections > 128 ||
		status.EstablishedConnections != nil && (*status.EstablishedConnections < 0 || *status.EstablishedConnections > 1<<53-1) ||
		!validOriginProbeStreak(status.ServiceConsecutiveFailures) || !validOriginProbeStreak(status.ServiceConsecutiveSuccesses) ||
		!validOriginProbeStreak(status.ColdConsecutiveFailures) || !validOriginProbeStreak(status.ColdConsecutiveSuccesses) ||
		status.CheckedAt.IsZero() || status.ServiceProbe == nil && status.ColdProbe == nil ||
		len(status.References) == 0 || len(status.References) > MaxOriginPoolReferences {
		return false
	}
	normalizedAddress, err := NormalizeMonitoringAddress(status.Address)
	if err != nil || normalizedAddress != status.Address {
		return false
	}
	if status.Scheme != "http" && status.Scheme != "https" && status.Scheme != "grpc" && status.Scheme != "grpcs" {
		return false
	}
	if status.HTTPVersion != "" {
		if status.Scheme == "http" && status.HTTPVersion != OriginHTTPVersionHTTP1 && status.HTTPVersion != OriginHTTPVersionH2C ||
			status.Scheme == "https" && status.HTTPVersion != OriginHTTPVersionHTTP1 && status.HTTPVersion != OriginHTTPVersionHTTP2 ||
			(status.Scheme == "grpc" || status.Scheme == "grpcs") {
			return false
		}
	}
	if status.CircuitState != OriginCircuitClosed && status.CircuitState != OriginCircuitOpen && status.CircuitState != OriginCircuitRecovering {
		return false
	}
	if status.ServiceProbe != nil && !validOriginProbeSample(*status.ServiceProbe) ||
		status.ColdProbe != nil && !validOriginProbeSample(*status.ColdProbe) {
		return false
	}
	if status.ColdProbe != nil && status.ColdProbe.ConnectionReused {
		return false
	}
	latest := time.Time{}
	healthy := true
	for _, sample := range []*OriginProbeSample{status.ServiceProbe, status.ColdProbe} {
		if sample == nil {
			continue
		}
		healthy = healthy && sample.Healthy
		if sample.CheckedAt.After(latest) {
			latest = sample.CheckedAt
		}
	}
	if status.Healthy != healthy || !status.CheckedAt.Equal(latest) {
		return false
	}
	seenReferences := make(map[string]bool, len(status.References))
	for _, reference := range status.References {
		key := reference.SiteID + "\x00" + reference.Role
		if !validOriginReference(reference) || seenReferences[key] {
			return false
		}
		seenReferences[key] = true
	}
	return true
}

func validOriginProbeStreak(value int) bool {
	return value >= 0 && value <= 1_000_000
}

func validOriginProbeSample(sample OriginProbeSample) bool {
	if sample.CheckedAt.IsZero() || len(sample.Error) > 512 || strings.TrimSpace(sample.Error) != sample.Error {
		return false
	}
	if sample.Healthy && sample.Error != "" {
		return false
	}
	for _, value := range []float64{sample.ConnectMS, sample.TLSHandshakeMS, sample.HeaderMS, sample.TotalMS} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 60_000 {
			return false
		}
	}
	if sample.HTTPStatus != 0 && (sample.HTTPStatus < 100 || sample.HTTPStatus > 599) {
		return false
	}
	for _, character := range sample.Error {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validOriginReference(reference OriginPoolReference) bool {
	if reference.SiteID == "" || len(reference.SiteID) > 128 || strings.TrimSpace(reference.SiteID) != reference.SiteID ||
		(reference.Role != "primary" && reference.Role != "backup") {
		return false
	}
	for _, character := range reference.SiteID {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func lowerHex(value string) bool {
	for _, character := range value {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return false
	}
	return true
}
