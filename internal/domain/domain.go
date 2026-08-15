package domain

import (
	"encoding/json"
	"time"
)

type NodeStatus string

const (
	NodePending      NodeStatus = "pending"
	NodeActive       NodeStatus = "active"
	NodeDraining     NodeStatus = "draining"
	NodeRevoked      NodeStatus = "revoked"
	NodeUninstalling NodeStatus = "uninstalling"
	NodeUninstalled  NodeStatus = "uninstalled"
)

type TaskStatus string

const (
	TaskQueued      TaskStatus = "queued"
	TaskDispatching TaskStatus = "dispatching"
	TaskApplying    TaskStatus = "applying"
	TaskSucceeded   TaskStatus = "succeeded"
	TaskPartial     TaskStatus = "partial"
	TaskFailed      TaskStatus = "failed"
	TaskRolledBack  TaskStatus = "rolled_back"
)

type Node struct {
	ID                string        `json:"id"`
	Name              string        `json:"name"`
	PublicIPv4        string        `json:"public_ipv4"`
	CacheMaxSizeGB    *int          `json:"cache_max_size_gb,omitempty"`
	NginxCapacity     NginxCapacity `json:"nginx_capacity"`
	Status            NodeStatus    `json:"status"`
	MonitorAutoPaused bool          `json:"monitor_auto_paused"`
	Capabilities      []string      `json:"capabilities"`
	AgentVersion      string        `json:"agent_version,omitempty"`
	AgentSHA256       string        `json:"agent_sha256,omitempty"`
	NginxVersion      string        `json:"nginx_version,omitempty"`
	NginxSHA256       string        `json:"nginx_sha256,omitempty"`
	ActiveUpgradeID   string        `json:"active_upgrade_task_id,omitempty"`
	LastHeartbeatAt   *time.Time    `json:"last_heartbeat_at,omitempty"`
	AppliedVersion    int64         `json:"applied_version"`
	LastError         string        `json:"last_error,omitempty"`
	CreatedAt         time.Time     `json:"created_at"`
	UpdatedAt         time.Time     `json:"updated_at"`
}

// NginxCapacity contains the worker-level limits managed by the control
// plane. WorkerProcesses=0 uses Nginx's automatic CPU-aware setting.
type NginxCapacity struct {
	WorkerProcesses    int `json:"worker_processes"`
	WorkerConnections  int `json:"worker_connections"`
	WorkerRlimitNoFile int `json:"worker_rlimit_nofile"`
}

type CacheStorageUsage struct {
	UsedBytes   int64     `json:"used_bytes"`
	TotalBytes  int64     `json:"total_bytes"`
	CollectedAt time.Time `json:"collected_at"`
}

func ValidCacheStorageUsage(usage CacheStorageUsage) bool {
	const maxReportedBytes int64 = 1 << 60
	return usage.UsedBytes >= 0 && usage.UsedBytes <= maxReportedBytes &&
		usage.TotalBytes > 0 && usage.TotalBytes <= maxReportedBytes && !usage.CollectedAt.IsZero()
}

type Origin struct {
	URL               string                  `json:"url"`
	HostHeader        string                  `json:"host_header"`
	TLSServerName     string                  `json:"tls_server_name,omitempty"`
	HTTPVersion       OriginHTTPVersion       `json:"http_version,omitempty"`
	HealthCheckMethod OriginHealthCheckMethod `json:"health_check_method,omitempty"`
	HealthCheckPath   string                  `json:"health_check_path,omitempty"`
	// WireGuardTunnelID is empty for the existing direct-origin path. When set,
	// the publisher replaces only the connection host with the tunnel's origin
	// address and preserves the URL scheme, port, Host header, and TLS SNI.
	WireGuardTunnelID string `json:"wireguard_tunnel_id,omitempty"`
	Enabled           bool   `json:"enabled"`
}

type OriginHTTPVersion string

const (
	OriginHTTPVersionHTTP1 OriginHTTPVersion = "http1"
	OriginHTTPVersionHTTP2 OriginHTTPVersion = "http2"
	OriginHTTPVersionH2C   OriginHTTPVersion = "h2c"
)

type OriginHealthCheckMethod string

const (
	OriginHealthCheckMethodHEAD OriginHealthCheckMethod = "HEAD"
	OriginHealthCheckMethodGET  OriginHealthCheckMethod = "GET"
)

const (
	DefaultOriginHealthCheckMethod = OriginHealthCheckMethodHEAD
	DefaultOriginHealthCheckPath   = "/"
)

const (
	DefaultClientMaxBodySizeMB           = 128
	MaxClientMaxBodySizeMB               = 1024
	DefaultClientKeepaliveTimeoutSeconds = 120
	DefaultReadWriteTimeoutSeconds       = 120
	DefaultTCPConnectTimeoutSeconds      = 10
	DefaultTCPIdleTimeoutSeconds         = 300
	MaxTCPForwardsPerSite                = 32
	DefaultDNSTTLSeconds                 = 60
	MinDNSTTLSeconds                     = 60
	MaxDNSTTLSeconds                     = 300
	DefaultCacheMaxSizeGB                = 1
	MinCacheMaxSizeGB                    = 1
	MaxCacheMaxSizeGB                    = 1024
	DefaultNginxWorkerProcesses          = 0
	DefaultNginxWorkerConnections        = 4096
	DefaultNginxWorkerRlimitNoFile       = 65536
	MinNginxWorkerConnections            = 256
	MaxNginxWorkerConnections            = 65535
	MinNginxWorkerRlimitNoFile           = 1024
	MaxNginxWorkerRlimitNoFile           = DefaultNginxWorkerRlimitNoFile
	MaxNginxWorkerProcesses              = 128
)

const (
	EdgeCapabilityTCPStream              = "tcp_stream_v1"
	EdgeCapabilityOnlineUpgrade          = "online_upgrade_v1"
	EdgeCapabilityCacheUsage             = "cache_usage_v1"
	EdgeCapabilityMachineStatus          = "machine_status_v1"
	EdgeCapabilityMachineStatusStream    = "machine_status_stream_v1"
	EdgeCapabilityNginxFragments         = "nginx_fragments_v1"
	EdgeCapabilityRequestTracing         = "request_tracing_v1"
	EdgeCapabilityNginxCapacity          = "nginx_capacity_v1"
	EdgeCapabilityHTTP3                  = "http3_v1"
	EdgeCapabilityOriginConnection       = "origin_connection_v1"
	EdgeCapabilityOriginHTTP2            = "origin_http2_v1"
	EdgeCapabilityTCPMonitoring          = "tcp_monitoring_v1"
	EdgeCapabilityControlManifest        = "control_manifest_v1"
	EdgeCapabilityNginxBundle            = "nginx_bundle_v1"
	EdgeCapabilityWireGuard              = "wireguard_v1"
	EdgeCapabilityWireGuardPerformance   = "wireguard_performance_v1"
	EdgeCapabilityWireGuardPerformanceV2 = "wireguard_performance_v2"
)

// EdgeControlManifest is a compact change manifest returned with a heartbeat.
// Pointer use in EdgeHeartbeatResponse keeps the response compatible with
// control planes that predate manifest-driven polling.
type EdgeControlManifest struct {
	DesiredStateVersion int64  `json:"desired_state_version"`
	MonitoringRevision  string `json:"monitoring_revision"`
	SecurityRevision    string `json:"security_revision"`
	UpgradeTaskID       string `json:"upgrade_task_id,omitempty"`
	AccessLogGzip       bool   `json:"access_log_gzip,omitempty"`
}

type EdgeHeartbeatResponse struct {
	OK      bool                 `json:"ok"`
	Control *EdgeControlManifest `json:"control,omitempty"`
}

type TCPForward struct {
	Name                  string `json:"name"`
	ListenPort            int    `json:"listen_port"`
	ListenTLS             bool   `json:"listen_tls"`
	UpstreamHost          string `json:"upstream_host"`
	UpstreamPort          int    `json:"upstream_port"`
	UpstreamTLS           bool   `json:"upstream_tls"`
	UpstreamTLSServerName string `json:"upstream_tls_server_name,omitempty"`
	ConnectTimeoutSeconds int    `json:"connect_timeout_seconds"`
	IdleTimeoutSeconds    int    `json:"idle_timeout_seconds"`
}

type SiteSummary struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Domains []string `json:"domains"`
}

type Site struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	ZoneID        string   `json:"zone_id"`
	Domains       []string `json:"domains"`
	Nodes         []string `json:"node_ids"`
	BackupNodes   []string `json:"backup_node_ids"`
	PrimaryOrigin Origin   `json:"primary_origin"`
	BackupOrigin  *Origin  `json:"backup_origin,omitempty"`
	// StreamPaths is retained as an empty compatibility field for older API clients.
	StreamPaths                   []string     `json:"stream_paths"`
	Passthrough                   bool         `json:"passthrough"`
	RequestBodyBuffering          bool         `json:"request_body_buffering"`
	OriginResponseBuffering       bool         `json:"origin_response_buffering"`
	DynamicCompressionEnabled     bool         `json:"dynamic_compression_enabled"`
	CompressionExcludedMIMETypes  []string     `json:"compression_excluded_mime_types"`
	HTTP3Enabled                  bool         `json:"http3_enabled"`
	ClientMaxBodySizeMB           int          `json:"client_max_body_size_mb"`
	ClientKeepaliveTimeoutSeconds int          `json:"client_keepalive_timeout_seconds"`
	ReadWriteTimeoutSeconds       int          `json:"read_write_timeout_seconds"`
	DNSTTLSeconds                 *int         `json:"dns_ttl_seconds"`
	TCPOnly                       bool         `json:"tcp_only"`
	TCPForwards                   []TCPForward `json:"tcp_forwards"`
	// CacheMaxSizeGB is retained only for reading legacy database rows. Cache
	// quotas are node-scoped and this value is no longer exposed or rendered.
	CacheMaxSizeGB     *int                    `json:"-"`
	CacheGeneration    int64                   `json:"cache_generation"`
	CacheInvalidations []CacheInvalidationRule `json:"cache_invalidations"`
	CacheWarmups       []CacheWarmup           `json:"cache_warmups"`
	ConfigVersion      int64                   `json:"config_version"`
	Published          bool                    `json:"published"`
	Enabled            bool                    `json:"enabled"`
	Deleting           bool                    `json:"deleting"`
	CreatedAt          time.Time               `json:"created_at"`
	UpdatedAt          time.Time               `json:"updated_at"`
}

func (site Site) AssignedNodeIDs() []string {
	nodeIDs := make([]string, 0, len(site.Nodes)+len(site.BackupNodes))
	nodeIDs = append(nodeIDs, site.Nodes...)
	nodeIDs = append(nodeIDs, site.BackupNodes...)
	return nodeIDs
}

func (site *Site) UnmarshalJSON(contents []byte) error {
	type siteAlias Site
	decoded := siteAlias{
		DynamicCompressionEnabled:    true,
		CompressionExcludedMIMETypes: DefaultCompressionExcludedMIMETypes(),
	}
	if err := json.Unmarshal(contents, &decoded); err != nil {
		return err
	}
	*site = Site(decoded)
	return nil
}

type EnrollmentToken struct {
	Token     string    `json:"token"`
	NodeID    string    `json:"node_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

type DeploymentTask struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	SiteID     string     `json:"site_id,omitempty"`
	Status     TaskStatus `json:"status"`
	Detail     string     `json:"detail,omitempty"`
	DeadlineAt *time.Time `json:"deadline_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

type ApplyStatus string

const (
	ApplySucceeded ApplyStatus = "succeeded"
	ApplyFailed    ApplyStatus = "failed"
)

// PortConflict identifies a local listener that prevents edge Nginx from
// owning one of its public ports. It is reported to the authenticated control
// plane; the agent never terminates the conflicting process.
type PortConflict struct {
	Protocol string `json:"protocol,omitempty"`
	Port     int    `json:"port"`
	PID      int    `json:"pid,omitempty"`
	Process  string `json:"process"`
}

// ApplyReport is sent with an edge heartbeat after an attempt to apply a
// desired configuration. Older agents omit it and remain protocol-compatible.
type ApplyReport struct {
	Version       int64          `json:"version"`
	Status        ApplyStatus    `json:"status"`
	Code          string         `json:"code,omitempty"`
	Detail        string         `json:"detail,omitempty"`
	PortConflicts []PortConflict `json:"port_conflicts,omitempty"`
}

type PublishNodeStatus string

const (
	PublishNodePending   PublishNodeStatus = "pending"
	PublishNodeSucceeded PublishNodeStatus = "succeeded"
	PublishNodeFailed    PublishNodeStatus = "failed"
	PublishNodeTimedOut  PublishNodeStatus = "timed_out"
)

type PublishNodeResult struct {
	NodeID        string            `json:"node_id"`
	NodeName      string            `json:"node_name"`
	TargetVersion int64             `json:"target_version"`
	Status        PublishNodeStatus `json:"status"`
	ErrorCode     string            `json:"error_code,omitempty"`
	Detail        string            `json:"detail,omitempty"`
	PortConflicts []PortConflict    `json:"port_conflicts,omitempty"`
	ReportedAt    *time.Time        `json:"reported_at,omitempty"`
}

type PublishStatus struct {
	Task  *DeploymentTask     `json:"task"`
	Nodes []PublishNodeResult `json:"nodes"`
}

type DesiredState struct {
	Version           int64                  `json:"version"`
	NginxConfig       string                 `json:"nginx_config"`
	NginxStreamConfig string                 `json:"nginx_stream_config,omitempty"`
	NginxMainConfig   string                 `json:"nginx_main_config,omitempty"`
	NginxEventsConfig string                 `json:"nginx_events_config,omitempty"`
	NginxFragments    *NginxConfigFragments  `json:"nginx_fragments,omitempty"`
	PublicPorts       []int                  `json:"public_ports"`
	PublicUDPPorts    []int                  `json:"public_udp_ports,omitempty"`
	OriginPools       []OriginPool           `json:"origin_pools,omitempty"`
	StaticAssets      []StaticAssetReference `json:"static_assets,omitempty"`
	CacheWarmups      []CacheWarmup          `json:"cache_warmups,omitempty"`
	CacheMaxBytes     int64                  `json:"cache_max_bytes,omitempty"`
	Certificates      map[string]TLSBundle   `json:"certificates,omitempty"`
}

type NginxConfigFragment struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type NginxConfigFragments struct {
	HTTPBase    string                `json:"http_base"`
	HTTPSites   []NginxConfigFragment `json:"http_sites,omitempty"`
	StreamBase  string                `json:"stream_base"`
	StreamSites []NginxConfigFragment `json:"stream_sites,omitempty"`
}

type NodeUpgradeStatus string

const (
	NodeUpgradeQueued    NodeUpgradeStatus = "queued"
	NodeUpgradeApplying  NodeUpgradeStatus = "applying"
	NodeUpgradeSucceeded NodeUpgradeStatus = "succeeded"
	NodeUpgradeFailed    NodeUpgradeStatus = "failed"
)

type NodeUpgradeTask struct {
	ID                string            `json:"id"`
	NodeID            string            `json:"node_id"`
	Status            NodeUpgradeStatus `json:"status"`
	SourceSHA256      string            `json:"source_sha256,omitempty"`
	TargetSHA256      string            `json:"target_sha256"`
	SourceNginxSHA256 string            `json:"source_nginx_sha256,omitempty"`
	TargetNginxSHA256 string            `json:"target_nginx_sha256,omitempty"`
	ErrorCode         string            `json:"error_code,omitempty"`
	Detail            string            `json:"detail,omitempty"`
	DeadlineAt        time.Time         `json:"deadline_at"`
	StartedAt         *time.Time        `json:"started_at,omitempty"`
	CompletedAt       *time.Time        `json:"completed_at,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

type UpgradeArtifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type NodeUpgradeInstruction struct {
	TaskID         string           `json:"task_id"`
	DeadlineAt     time.Time        `json:"deadline_at"`
	Binary         UpgradeArtifact  `json:"binary"`
	Installer      UpgradeArtifact  `json:"installer"`
	AgentService   UpgradeArtifact  `json:"agent_service"`
	UpdaterService UpgradeArtifact  `json:"updater_service"`
	NginxBundle    *UpgradeArtifact `json:"nginx_bundle,omitempty"`
	NginxService   *UpgradeArtifact `json:"nginx_service,omitempty"`
}

type NodeUpgradeReport struct {
	TaskID               string            `json:"task_id"`
	Status               NodeUpgradeStatus `json:"status"`
	ErrorCode            string            `json:"error_code,omitempty"`
	Detail               string            `json:"detail,omitempty"`
	InstalledSHA256      string            `json:"installed_sha256,omitempty"`
	InstalledNginxSHA256 string            `json:"installed_nginx_sha256,omitempty"`
}

type TLSBundle struct {
	CertificatePEM string `json:"certificate_pem"`
	PrivateKeyPEM  string `json:"private_key_pem"`
}

type AccessLogEvent struct {
	ID                    string    `json:"id"`
	ClientRequestID       string    `json:"client_request_id"`
	UpstreamRequestID     string    `json:"upstream_request_id"`
	Timestamp             time.Time `json:"timestamp"`
	NodeID                string    `json:"node_id"`
	SiteID                string    `json:"site_id"`
	ClientIP              string    `json:"client_ip"`
	Host                  string    `json:"host"`
	Scheme                string    `json:"scheme"`
	Protocol              string    `json:"protocol"`
	Method                string    `json:"method"`
	Path                  string    `json:"path"`
	Status                int       `json:"status"`
	RequestBytes          int64     `json:"request_bytes"`
	Bytes                 int64     `json:"bytes"`
	DurationMS            int64     `json:"duration_ms"`
	RequestCompletion     string    `json:"request_completion"`
	Upstream              string    `json:"upstream"`
	UpstreamStatus        string    `json:"upstream_status"`
	UpstreamConnectTime   string    `json:"upstream_connect_time"`
	UpstreamHeaderTime    string    `json:"upstream_header_time"`
	UpstreamResponseTime  string    `json:"upstream_response_time"`
	UpstreamBytesSent     string    `json:"upstream_bytes_sent"`
	UpstreamBytesReceived string    `json:"upstream_bytes_received"`
	// Upstream timing values in milliseconds, one entry per upstream attempt.
	// The string fields above remain the source of truth for API compatibility;
	// these arrays are parsed once on the edge for ClickHouse aggregation.
	UpstreamConnectMS     []float64 `json:"upstream_connect_ms,omitempty"`
	UpstreamHeaderMS      []float64 `json:"upstream_header_ms,omitempty"`
	UpstreamResponseMS    []float64 `json:"upstream_response_ms,omitempty"`
	UpstreamRequestIDs    []string  `json:"upstream_request_ids,omitempty"`
	CacheStatus           string    `json:"cache_status"`
	UserAgent             string    `json:"user_agent"`
	Referer               string    `json:"referer"`
	ContentType           string    `json:"content_type"`
	ResponseContentType   string    `json:"response_content_type"`
	ContentEncoding       string    `json:"content_encoding"`
	CompressionRatio      float64   `json:"compression_ratio"`
	CompressionSavedBytes int64     `json:"compression_saved_bytes"`
	Accept                string    `json:"accept"`
	Range                 string    `json:"range"`
}
