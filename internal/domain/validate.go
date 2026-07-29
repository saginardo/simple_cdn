package domain

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

func NormalizeAndValidateSite(site *Site) error {
	site.CacheMaxSizeGB = nil
	site.Name = strings.TrimSpace(site.Name)
	if site.Name == "" || len(site.Name) > 100 {
		return fmt.Errorf("site name must be between 1 and 100 characters")
	}
	clientMaxBodySizeMB, err := NormalizeClientMaxBodySizeMB(site.ClientMaxBodySizeMB)
	if err != nil {
		return err
	}
	site.ClientMaxBodySizeMB = clientMaxBodySizeMB
	clientKeepaliveTimeoutSeconds, err := NormalizeClientKeepaliveTimeoutSeconds(site.ClientKeepaliveTimeoutSeconds)
	if err != nil {
		return err
	}
	site.ClientKeepaliveTimeoutSeconds = clientKeepaliveTimeoutSeconds
	readWriteTimeoutSeconds, err := NormalizeReadWriteTimeoutSeconds(site.ReadWriteTimeoutSeconds)
	if err != nil {
		return err
	}
	site.ReadWriteTimeoutSeconds = readWriteTimeoutSeconds
	if site.DNSTTLSeconds != nil {
		if err := ValidateDNSTTLSeconds(*site.DNSTTLSeconds); err != nil {
			return err
		}
	}
	if len(site.Domains) == 0 {
		return fmt.Errorf("at least one domain is required")
	}
	seenDomains := make(map[string]struct{}, len(site.Domains))
	domains := make([]string, 0, len(site.Domains))
	for _, domainName := range site.Domains {
		domainName = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domainName), "."))
		if net.ParseIP(domainName) != nil {
			return fmt.Errorf("site domain %q must be a DNS hostname, not an IP address", domainName)
		}
		if !ValidHostname(domainName) {
			return fmt.Errorf("invalid domain %q", domainName)
		}
		if _, found := seenDomains[domainName]; found {
			return fmt.Errorf("duplicate domain %q", domainName)
		}
		seenDomains[domainName] = struct{}{}
		domains = append(domains, domainName)
	}
	site.Domains = domains
	if len(site.Nodes) == 0 {
		return fmt.Errorf("at least one node is required")
	}
	seenNodes := make(map[string]struct{}, len(site.Nodes))
	nodes := make([]string, 0, len(site.Nodes))
	for _, nodeID := range site.Nodes {
		nodeID = strings.TrimSpace(nodeID)
		if nodeID == "" {
			return fmt.Errorf("node ID is required")
		}
		if _, found := seenNodes[nodeID]; found {
			return fmt.Errorf("duplicate node ID %q", nodeID)
		}
		seenNodes[nodeID] = struct{}{}
		nodes = append(nodes, nodeID)
	}
	site.Nodes = nodes
	if err := normalizeTCPForwards(site); err != nil {
		return err
	}
	if site.TCPOnly {
		site.HTTP3Enabled = false
		if strings.TrimSpace(site.PrimaryOrigin.WireGuardTunnelID) != "" ||
			site.BackupOrigin != nil && strings.TrimSpace(site.BackupOrigin.WireGuardTunnelID) != "" {
			return fmt.Errorf("WireGuard origins are not supported for TCP-only sites")
		}
	}
	var primary *url.URL
	if !site.TCPOnly {
		if err := ValidateOrigin(&site.PrimaryOrigin); err != nil {
			return fmt.Errorf("primary origin: %w", err)
		}
		primary, _ = url.Parse(site.PrimaryOrigin.URL)
		if site.Passthrough && primary.Scheme != "http" && primary.Scheme != "https" {
			return fmt.Errorf("passthrough mode is only supported for HTTP and HTTPS origins")
		}
	} else if len(site.TCPForwards) == 0 {
		return fmt.Errorf("TCP-only sites require at least one TCP forward")
	}
	// Path-specific streaming was retired. Keep the API field stable while making
	// old values inert and consistently returning an empty JSON array.
	site.StreamPaths = []string{}
	if !site.TCPOnly && site.BackupOrigin != nil {
		if err := ValidateOrigin(site.BackupOrigin); err != nil {
			return fmt.Errorf("backup origin: %w", err)
		}
		backup, _ := url.Parse(site.BackupOrigin.URL)
		if primary.Scheme != backup.Scheme {
			return fmt.Errorf("primary and backup origin must use the same scheme")
		}
	}
	return nil
}

func normalizeTCPForwards(site *Site) error {
	if site.TCPForwards == nil {
		site.TCPForwards = []TCPForward{}
	}
	if len(site.TCPForwards) > MaxTCPForwardsPerSite {
		return fmt.Errorf("a site can define at most %d TCP forwards", MaxTCPForwardsPerSite)
	}
	seenPorts := make(map[int]struct{}, len(site.TCPForwards))
	for index := range site.TCPForwards {
		forward := &site.TCPForwards[index]
		forward.Name = strings.TrimSpace(forward.Name)
		if forward.Name == "" || len(forward.Name) > 100 {
			return fmt.Errorf("TCP forward name must be between 1 and 100 characters")
		}
		if forward.ListenPort < 1 || forward.ListenPort > 65535 {
			return fmt.Errorf("TCP forward %q listen port must be between 1 and 65535", forward.Name)
		}
		if forward.ListenPort == 80 || forward.ListenPort == 443 {
			return fmt.Errorf("TCP forward %q cannot listen on reserved HTTP port %d", forward.Name, forward.ListenPort)
		}
		if _, found := seenPorts[forward.ListenPort]; found {
			return fmt.Errorf("duplicate TCP listen port %d", forward.ListenPort)
		}
		seenPorts[forward.ListenPort] = struct{}{}
		forward.UpstreamHost = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(forward.UpstreamHost), "."))
		if !ValidHostname(forward.UpstreamHost) {
			return fmt.Errorf("TCP forward %q has an invalid upstream hostname", forward.Name)
		}
		if forward.UpstreamPort < 1 || forward.UpstreamPort > 65535 {
			return fmt.Errorf("TCP forward %q upstream port must be between 1 and 65535", forward.Name)
		}
		if forward.ConnectTimeoutSeconds == 0 {
			forward.ConnectTimeoutSeconds = DefaultTCPConnectTimeoutSeconds
		}
		if forward.ConnectTimeoutSeconds < 1 || forward.ConnectTimeoutSeconds > 60 {
			return fmt.Errorf("TCP forward %q connect timeout must be between 1 and 60 seconds", forward.Name)
		}
		if forward.IdleTimeoutSeconds == 0 {
			forward.IdleTimeoutSeconds = DefaultTCPIdleTimeoutSeconds
		}
		if forward.IdleTimeoutSeconds < 30 || forward.IdleTimeoutSeconds > 3600 {
			return fmt.Errorf("TCP forward %q idle timeout must be between 30 and 3600 seconds", forward.Name)
		}
		forward.UpstreamTLSServerName = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(forward.UpstreamTLSServerName), "."))
		if !forward.UpstreamTLS {
			if forward.UpstreamTLSServerName != "" {
				return fmt.Errorf("TCP forward %q TLS server name requires upstream TLS", forward.Name)
			}
			continue
		}
		if forward.UpstreamTLSServerName == "" {
			if net.ParseIP(forward.UpstreamHost) != nil {
				return fmt.Errorf("TCP forward %q requires a TLS server name when the upstream is an IP address", forward.Name)
			}
			forward.UpstreamTLSServerName = forward.UpstreamHost
		}
		if net.ParseIP(forward.UpstreamTLSServerName) != nil || !ValidHostname(forward.UpstreamTLSServerName) {
			return fmt.Errorf("TCP forward %q has an invalid upstream TLS server name", forward.Name)
		}
	}
	return nil
}

func SiteNeedsCertificate(site Site) bool {
	if !site.TCPOnly {
		return true
	}
	for _, forward := range site.TCPForwards {
		if forward.ListenTLS {
			return true
		}
	}
	return false
}

func NormalizeClientMaxBodySizeMB(value int) (int, error) {
	if value == 0 {
		value = DefaultClientMaxBodySizeMB
	}
	if err := ValidateClientMaxBodySizeMB(value); err != nil {
		return 0, err
	}
	return value, nil
}

func ValidateClientMaxBodySizeMB(value int) error {
	switch value {
	case DefaultClientMaxBodySizeMB, 256, 512, MaxClientMaxBodySizeMB:
		return nil
	default:
		return fmt.Errorf("client max body size must be one of 128, 256, 512, or 1024 MiB")
	}
}

func NormalizeClientKeepaliveTimeoutSeconds(value int) (int, error) {
	if value == 0 {
		value = DefaultClientKeepaliveTimeoutSeconds
	}
	if err := ValidateClientKeepaliveTimeoutSeconds(value); err != nil {
		return 0, err
	}
	return value, nil
}

func ValidateClientKeepaliveTimeoutSeconds(value int) error {
	if value < 15 || value > 3600 {
		return fmt.Errorf("client keepalive timeout must be between 15 and 3600 seconds")
	}
	return nil
}

func NormalizeReadWriteTimeoutSeconds(value int) (int, error) {
	if value == 0 {
		value = DefaultReadWriteTimeoutSeconds
	}
	if err := ValidateReadWriteTimeoutSeconds(value); err != nil {
		return 0, err
	}
	return value, nil
}

func ValidateReadWriteTimeoutSeconds(value int) error {
	switch value {
	case 120, 360, 900, 1800, 3600:
		return nil
	default:
		return fmt.Errorf("read/write timeout must be one of 120, 360, 900, 1800, or 3600 seconds")
	}
}

func DefaultNginxCapacity() NginxCapacity {
	return NginxCapacity{
		WorkerProcesses:    DefaultNginxWorkerProcesses,
		WorkerConnections:  DefaultNginxWorkerConnections,
		WorkerRlimitNoFile: DefaultNginxWorkerRlimitNoFile,
	}
}

func NormalizeNginxCapacity(value NginxCapacity) (NginxCapacity, error) {
	if value.WorkerConnections == 0 {
		value.WorkerConnections = DefaultNginxWorkerConnections
	}
	if value.WorkerRlimitNoFile == 0 {
		value.WorkerRlimitNoFile = DefaultNginxWorkerRlimitNoFile
	}
	if value.WorkerProcesses < 0 || value.WorkerProcesses > MaxNginxWorkerProcesses {
		return NginxCapacity{}, fmt.Errorf("Nginx worker processes must be auto or between 1 and %d", MaxNginxWorkerProcesses)
	}
	if value.WorkerConnections < MinNginxWorkerConnections || value.WorkerConnections > MaxNginxWorkerConnections {
		return NginxCapacity{}, fmt.Errorf("Nginx worker connections must be between %d and %d", MinNginxWorkerConnections, MaxNginxWorkerConnections)
	}
	if value.WorkerRlimitNoFile < MinNginxWorkerRlimitNoFile || value.WorkerRlimitNoFile > MaxNginxWorkerRlimitNoFile {
		return NginxCapacity{}, fmt.Errorf("Nginx worker file limit must be between %d and %d", MinNginxWorkerRlimitNoFile, MaxNginxWorkerRlimitNoFile)
	}
	if value.WorkerRlimitNoFile < value.WorkerConnections {
		return NginxCapacity{}, fmt.Errorf("Nginx worker file limit must be at least the worker connection count")
	}
	return value, nil
}

func ValidateDNSTTLSeconds(value int) error {
	if value < MinDNSTTLSeconds || value > MaxDNSTTLSeconds {
		return fmt.Errorf("DNS TTL must be between %d and %d seconds", MinDNSTTLSeconds, MaxDNSTTLSeconds)
	}
	return nil
}

func ValidateCacheMaxSizeGB(value int) error {
	if value < MinCacheMaxSizeGB || value > MaxCacheMaxSizeGB {
		return fmt.Errorf("cache maximum size must be between %d and %d GB", MinCacheMaxSizeGB, MaxCacheMaxSizeGB)
	}
	return nil
}

func EffectiveNodeCacheMaxSizeGB(node Node, defaultSize int) (int, error) {
	if err := ValidateCacheMaxSizeGB(defaultSize); err != nil {
		return 0, err
	}
	if node.CacheMaxSizeGB == nil {
		return defaultSize, nil
	}
	if err := ValidateCacheMaxSizeGB(*node.CacheMaxSizeGB); err != nil {
		return 0, err
	}
	return *node.CacheMaxSizeGB, nil
}

func ValidateOrigin(origin *Origin) error {
	origin.URL = strings.TrimSpace(origin.URL)
	origin.WireGuardTunnelID = strings.TrimSpace(origin.WireGuardTunnelID)
	parsed, err := url.Parse(origin.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("must be an absolute HTTP(S), WebSocket, or gRPC URL")
	}
	if !ValidOriginScheme(parsed.Scheme) {
		return fmt.Errorf("scheme must be http, https, ws, wss, grpc, or grpcs")
	}
	origin.HTTPVersion = OriginHTTPVersion(strings.ToLower(strings.TrimSpace(string(origin.HTTPVersion))))
	switch parsed.Scheme {
	case "grpc", "grpcs":
		if origin.HTTPVersion != "" {
			return fmt.Errorf("HTTP version is managed by the gRPC upstream")
		}
	case "ws", "wss":
		if origin.HTTPVersion == "" {
			origin.HTTPVersion = OriginHTTPVersionHTTP1
		}
		if origin.HTTPVersion != OriginHTTPVersionHTTP1 {
			return fmt.Errorf("WebSocket origins require HTTP/1.1")
		}
	case "http":
		if origin.HTTPVersion == "" {
			origin.HTTPVersion = OriginHTTPVersionHTTP1
		}
		if origin.HTTPVersion != OriginHTTPVersionHTTP1 && origin.HTTPVersion != OriginHTTPVersionH2C {
			return fmt.Errorf("HTTP origins support only HTTP/1.1 or H2C")
		}
	case "https":
		if origin.HTTPVersion == "" {
			origin.HTTPVersion = OriginHTTPVersionHTTP1
		}
		if origin.HTTPVersion != OriginHTTPVersionHTTP1 && origin.HTTPVersion != OriginHTTPVersionHTTP2 {
			return fmt.Errorf("HTTPS origins support only HTTP/1.1 or HTTP/2")
		}
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return fmt.Errorf("must not include credentials, path, query, or fragment")
	}
	if !ValidHostname(parsed.Hostname()) {
		return fmt.Errorf("invalid origin hostname")
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return fmt.Errorf("invalid origin port")
		}
	}
	origin.HostHeader = strings.TrimSpace(origin.HostHeader)
	if origin.HostHeader == "" {
		origin.HostHeader = parsed.Hostname()
	}
	if !ValidHostHeader(origin.HostHeader) {
		return fmt.Errorf("invalid origin Host header")
	}
	origin.TLSServerName = strings.ToLower(strings.TrimSpace(origin.TLSServerName))
	if origin.TLSServerName != "" {
		if !OriginUsesTLS(parsed.Scheme) {
			return fmt.Errorf("TLS server name is only supported for HTTPS, WSS, or GRPCS origins")
		}
		if net.ParseIP(origin.TLSServerName) != nil || !ValidHostname(origin.TLSServerName) {
			return fmt.Errorf("invalid TLS server name; use a DNS hostname without a port or wildcard")
		}
	}
	return nil
}

func EffectiveOriginHTTPVersion(origin Origin) OriginHTTPVersion {
	if origin.HTTPVersion == "" {
		return OriginHTTPVersionHTTP1
	}
	return origin.HTTPVersion
}

func ValidOriginScheme(scheme string) bool {
	switch scheme {
	case "http", "https", "ws", "wss", "grpc", "grpcs":
		return true
	default:
		return false
	}
}

func IsGRPCScheme(scheme string) bool { return scheme == "grpc" || scheme == "grpcs" }

func IsWebSocketScheme(scheme string) bool { return scheme == "ws" || scheme == "wss" }

func OriginUsesTLS(scheme string) bool {
	return scheme == "https" || scheme == "wss" || scheme == "grpcs"
}

func ProxyScheme(scheme string) string {
	switch scheme {
	case "ws":
		return "http"
	case "wss":
		return "https"
	default:
		return scheme
	}
}

func ValidHostname(value string) bool {
	if net.ParseIP(value) != nil {
		return true
	}
	if len(value) == 0 || len(value) > 253 || strings.Contains(value, "..") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-') {
				return false
			}
		}
	}
	return true
}

func ValidHostHeader(value string) bool {
	if strings.ContainsAny(value, " \t\r\n/@?#") {
		return false
	}
	if host, port, err := net.SplitHostPort(value); err == nil {
		if !ValidHostname(strings.Trim(host, "[]")) {
			return false
		}
		parsed, err := strconv.Atoi(port)
		return err == nil && parsed >= 1 && parsed <= 65535
	}
	return ValidHostname(value)
}
