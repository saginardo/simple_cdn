export type NodeStatus =
  | "pending"
  | "active"
  | "draining"
  | "revoked"
  | "uninstalling"
  | "uninstalled";

export interface SystemInfo {
  name: string;
  version: string;
}

export interface NodeUpgradeTask {
  id: string;
  node_id: string;
  status: "queued" | "applying" | "succeeded" | "failed";
  source_sha256?: string;
  target_sha256: string;
  error_code?: string;
  detail?: string;
  deadline_at: string;
  started_at?: string;
  completed_at?: string;
  created_at: string;
  updated_at: string;
}

export interface Node {
  id: string;
  name: string;
  public_ipv4: string;
  cache_max_size_gb?: number;
  nginx_capacity: NginxCapacity;
  status: NodeStatus;
  monitor_auto_paused: boolean;
  capabilities: string[];
  agent_version?: string;
  agent_sha256?: string;
  active_upgrade_task_id?: string;
  last_heartbeat_at?: string;
  applied_version: number;
  last_error?: string;
  created_at: string;
  updated_at: string;
  target_agent_sha256?: string;
  target_agent_version?: string;
  upgrade_capable: boolean;
  upgrade_up_to_date: boolean;
  can_upgrade: boolean;
  upgrade_blocker?: string;
  upgrade_task?: NodeUpgradeTask;
}

export interface NginxCapacity {
  worker_processes: number;
  worker_connections: number;
  worker_rlimit_nofile: number;
}

export interface MonitoringTarget {
  id: string;
  name: string;
  address: string;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface MonitoringProbeResult {
  target_id: string;
  target_name: string;
  address: string;
  attempts: number;
  successful_attempts: number;
  average_latency_ms: number;
  error?: string;
  checked_at: string;
}

export interface MonitoringNode {
  node_id: string;
  name: string;
  public_ipv4: string;
  status: NodeStatus;
  monitor_auto_paused: boolean;
  capable: boolean;
  score?: number;
  success_rate?: number;
  average_latency_ms?: number;
  consecutive_abnormal: number;
  last_checked_at?: string;
  stale: boolean;
  results: MonitoringProbeResult[];
}

export interface MonitoringOverview {
  targets: MonitoringTarget[];
  nodes: MonitoringNode[];
  interval_seconds: number;
  attempts_per_round: number;
  healthy_score: number;
  auto_pause_after: number;
}

export interface SmartRoutingWindow {
  weekdays: number[];
  start: string;
  end: string;
}

export interface SmartRoutingNode {
  node_id: string;
  name: string;
  public_ipv4: string;
  status: NodeStatus;
  capable: boolean;
  auto_paused: boolean;
  enabled: boolean;
  blocked_by: Array<"score" | "schedule">;
  score: {
    enabled: boolean;
    pause_below_score: number;
    pause_after_rounds: number;
    resume_at_score: number;
    resume_after_rounds: number;
    gate: "unknown" | "allowed" | "blocked";
    current_score?: number;
    last_checked_at?: string;
    stale: boolean;
    low_streak: number;
    recovery_streak: number;
  };
  schedule: {
    enabled: boolean;
    windows: SmartRoutingWindow[];
    gate: "open" | "closed";
    next_transition_at?: string;
  };
  updated_at: string;
}

export interface SmartRoutingOverview {
  timezone: string;
  nodes: SmartRoutingNode[];
}

export interface SmartRoutingConfig {
  enabled: boolean;
  score: {
    enabled: boolean;
    pause_below_score: number;
    pause_after_rounds: number;
    resume_at_score: number;
    resume_after_rounds: number;
  };
  schedule: {
    enabled: boolean;
    windows: SmartRoutingWindow[];
  };
}

export type MonitoringHistoryRange = "1h" | "6h" | "12h" | "24h" | "7d";

export interface MonitoringHistoryPoint {
  time: string;
  attempts: number;
  successful_attempts: number;
  success_rate: number;
  average_latency_ms: number | null;
  failed_rounds: number;
}

export interface MonitoringHistorySeries {
  target_id: string;
  name: string;
  address: string;
  points: MonitoringHistoryPoint[];
}

export interface MonitoringHistory {
  available: boolean;
  unavailable_reason?: string;
  node: {
    id: string;
    name: string;
    public_ipv4: string;
    status: NodeStatus;
    monitor_auto_paused: boolean;
  };
  range: MonitoringHistoryRange;
  from: string;
  to: string;
  bucket_seconds: number;
  series: MonitoringHistorySeries[];
}

export interface MachineReport {
  distribution: string;
  version: string;
  uptime_seconds: number;
  load_1: number;
  load_5: number;
  load_15: number;
  cpu_usage_percent: number;
  cpu_logical_cores: number;
  memory_used_bytes: number;
  memory_total_bytes: number;
  disk_used_bytes: number;
  disk_total_bytes: number;
  network_interface: string;
  network_rx_bytes_per_second: number;
  network_tx_bytes_per_second: number;
  sample_seconds: number;
  origin_probes?: OriginProbeStatus[];
  nginx?: NginxRuntimeStatus;
  collected_at: string;
}

export interface NginxRuntimeStatus {
  active_connections: number;
  accepted_connections: number;
  handled_connections: number;
  requests: number;
  reading: number;
  writing: number;
  waiting: number;
}

export interface OriginPoolReference {
  site_id: string;
  role: "primary" | "backup";
}

export interface OriginProbeSample {
  healthy: boolean;
  connection_reused: boolean;
  connect_ms: number;
  tls_handshake_ms: number;
  header_ms: number;
  total_ms: number;
  http_status?: number;
  error?: string;
  checked_at: string;
}

export interface OriginProbeStatus {
  pool_id: string;
  address: string;
  scheme: "http" | "https" | "grpc" | "grpcs";
  http_version?: OriginHTTPVersion;
  keepalive_connections: number;
  established_connections?: number;
  references: OriginPoolReference[];
  healthy: boolean;
  circuit_state: "closed" | "open" | "recovering";
  service_consecutive_failures: number;
  service_consecutive_successes: number;
  cold_consecutive_failures: number;
  cold_consecutive_successes: number;
  service_probe?: OriginProbeSample;
  cold_probe?: OriginProbeSample;
  checked_at: string;
}

export interface SiteOriginConnectionNode {
  node_id: string;
  node_name: string;
  public_ipv4: string;
  status: NodeStatus;
  available: boolean;
  unavailable_reason?: string;
  stale: boolean;
  collected_at?: string;
  probes: OriginProbeStatus[];
}

export interface SiteOriginConnections {
  site_id: string;
  nodes: SiteOriginConnectionNode[];
}

export interface NodeMachineStatus {
  available: boolean;
  unavailable_reason?: string;
  stale: boolean;
  report?: MachineReport;
}

export interface NodeDetail {
  node: Node;
  machine: NodeMachineStatus;
  cache: NodeCacheSettings;
  sites: Array<{
    id: string;
    name: string;
    domains: string[];
    enabled: boolean;
    published: boolean;
    cache_enabled: boolean;
  }>;
}

export interface NodeCacheSettings {
  default_size_gb: number;
  override_size_gb: number | null;
  effective_size_gb: number;
}

export interface NodeCacheStatus {
  available: boolean;
  unavailable_reason?: string;
  from: string;
  to: string;
  last_seen_at?: string;
  requests: number;
  bytes: number;
  cache_lookups: number;
  cache_hits: number;
  cache_misses: number;
  bypasses: number;
  uncached: number;
  hit_rate: number;
  statuses: Array<{ status: string; requests: number; bytes: number }>;
  storage: {
    available: boolean;
    unavailable_reason?: string;
    used_bytes: number;
    total_bytes: number;
    collected_at?: string;
    stale: boolean;
  };
}

export interface Origin {
  url: string;
  host_header: string;
  tls_server_name?: string;
  http_version?: OriginHTTPVersion;
  wireguard_tunnel_id?: string;
  enabled: boolean;
}

export type OriginHTTPVersion = "http1" | "http2" | "h2c";

export interface WireGuardPeer {
  node_id: string;
  node_name: string;
  node_public_ipv4: string;
  address: string;
  edge_egress_limit_mbps: number;
  public_key?: string;
  applied_revision: number;
  latest_handshake_at?: string;
  rx_bytes: number;
  tx_bytes: number;
  rx_bytes_per_second?: number;
  tx_bytes_per_second?: number;
  transfer_sample_seconds?: number;
  last_reported_at?: string;
  last_error?: string;
}

export interface WireGuardTunnel {
  id: string;
  name: string;
  endpoint_host: string;
  listen_port: number;
  address_cidr: string;
  origin_address: string;
  mtu: number;
  persistent_keepalive_seconds: number;
  performance_port: number;
  origin_egress_limit_mbps: number;
  origin_public_key?: string;
  revision: number;
  origin_configured_revision: number;
  origin_configured_at?: string;
  peers: WireGuardPeer[];
  created_at: string;
  updated_at: string;
}

export type WireGuardOriginServiceStatus =
  "unknown" | "healthy" | "partial" | "degraded" | "unreachable";

export interface WireGuardOriginServiceReference {
  site_id: string;
  site_name: string;
  domains: string[];
  role: "primary" | "backup";
  published: boolean;
}

export interface WireGuardOriginService {
  port: number;
  scheme: "http" | "https" | "grpc" | "grpcs";
  http_version?: OriginHTTPVersion;
  status: WireGuardOriginServiceStatus;
  reachable_nodes: number;
  observed_nodes: number;
  total_nodes: number;
  last_reported_at?: string;
  sites: WireGuardOriginServiceReference[];
}

export interface WireGuardPeerRuntime {
  node_id: string;
  established_connections?: number;
  collected_at?: string;
}

export interface WireGuardTunnelDetail {
  tunnel: WireGuardTunnel;
  origin_services: WireGuardOriginService[];
  peer_runtime?: WireGuardPeerRuntime[];
}

export interface WireGuardTCPMeasurement {
  mbps: number;
  retransmits: number;
}

export interface WireGuardUDPMeasurement {
  target_mbps: number;
  mbps: number;
  lost_packets: number;
  total_packets: number;
  loss_percent: number;
  jitter_ms: number;
}

export interface WireGuardPerformanceTest {
  id: string;
  tunnel_id: string;
  tunnel_name: string;
  node_id: string;
  node_name: string;
  target_mbps: number;
  duration_seconds: number;
  status: "queued" | "running" | "succeeded" | "failed";
  result?: {
    direct_tcp?: WireGuardTCPMeasurement;
    direct_tcp_reverse?: WireGuardTCPMeasurement;
    wireguard_tcp?: WireGuardTCPMeasurement;
    wireguard_tcp_reverse?: WireGuardTCPMeasurement;
    wireguard_udp?: WireGuardUDPMeasurement;
    wireguard_udp_reverse?: WireGuardUDPMeasurement;
  };
  error?: string;
  created_at: string;
  started_at?: string;
  finished_at?: string;
}

export interface TCPForward {
  name: string;
  listen_port: number;
  listen_tls: boolean;
  upstream_host: string;
  upstream_port: number;
  upstream_tls: boolean;
  upstream_tls_server_name?: string;
  connect_timeout_seconds: number;
  idle_timeout_seconds: number;
}

export interface Site {
  id: string;
  name: string;
  zone_id: string;
  domains: string[];
  node_ids: string[];
  primary_origin: Origin;
  backup_origin?: Origin;
  stream_paths: string[];
  passthrough: boolean;
  request_body_buffering: boolean;
  origin_response_buffering: boolean;
  http3_enabled: boolean;
  client_max_body_size_mb: number;
  client_keepalive_timeout_seconds: number;
  read_write_timeout_seconds: number;
  dns_ttl_seconds: number | null;
  tcp_only: boolean;
  tcp_forwards: TCPForward[];
  cache_generation: number;
  config_version: number;
  published: boolean;
  enabled: boolean;
  deleting: boolean;
  created_at: string;
  updated_at: string;
}

export interface DeploymentTask {
  id: string;
  kind: string;
  site_id?: string;
  status:
    | "queued"
    | "dispatching"
    | "applying"
    | "succeeded"
    | "partial"
    | "failed"
    | "rolled_back";
  detail?: string;
  deadline_at?: string;
  created_at: string;
  updated_at: string;
}

export interface CertificateSiteStatus {
  site_id: string;
  site_name: string;
  domains: string[];
  enabled: boolean;
  published: boolean;
  deleting: boolean;
  needs_certificate: boolean;
  certificate_present: boolean;
  certificate_updated_at?: string;
  not_after?: string;
  renewal_due_at?: string;
  published_after_certificate: boolean;
  task: DeploymentTask | null;
}

export interface CertificateOverview {
  renewal_window_days: number;
  reconcile_interval_seconds: number;
  sites: CertificateSiteStatus[];
}

export interface PublishStatus {
  task: DeploymentTask | null;
  nodes: Array<{
    node_id: string;
    node_name: string;
    target_version: number;
    status: "pending" | "succeeded" | "failed" | "timed_out";
    error_code?: string;
    detail?: string;
  }>;
}

export interface OverviewPoint {
  time: string;
  requests: number;
  downstream_bytes: number;
  upstream_bytes: number;
  error_requests: number;
}

export interface OverviewStatusCode {
  code: number;
  requests: number;
}

export interface OverviewSite {
  id: string;
  name: string;
  domains: string[];
  requests: number;
  downstream_bytes: number;
  upstream_bytes: number;
  error_requests: number;
  status_codes: OverviewStatusCode[];
  series: OverviewPoint[];
}

export interface Overview {
  from: string;
  to: string;
  bucket_seconds: number;
  totals: {
    requests: number;
    downstream_bytes: number;
    upstream_bytes: number;
    error_requests: number;
  };
  series: OverviewPoint[];
  status_codes: OverviewStatusCode[];
  sites: OverviewSite[];
}

export interface AccessLog {
  id: string;
  client_request_id: string;
  upstream_request_id: string;
  timestamp: string;
  node_id: string;
  site_id: string;
  client_ip: string;
  host: string;
  scheme: string;
  protocol: string;
  method: string;
  path: string;
  status: number;
  request_bytes: number;
  bytes: number;
  duration_ms: number;
  request_completion: string;
  upstream: string;
  upstream_status: string;
  upstream_connect_time: string;
  upstream_header_time: string;
  upstream_response_time: string;
  upstream_bytes_sent: string;
  upstream_bytes_received: string;
  cache_status: string;
  user_agent: string;
  referer: string;
  content_type: string;
  response_content_type: string;
  accept: string;
  range: string;
}

export interface SiteMinuteMetric {
  minute: string;
  requests: number;
  bytes: number;
  errors: number;
  cache_hits: number;
  upstream_samples: number;
  upstream_header_samples: number;
  upstream_response_samples: number;
  upstream_reused: number;
  upstream_connect_ms: number;
  upstream_header_ms: number;
  upstream_response_ms: number;
}

export interface LogPage {
  logs: AccessLog[];
  from: string;
  to: string;
  offset: number;
  page_size: number;
  has_more: boolean;
}

export interface SecurityPolicy {
  id: string;
  builtin: boolean;
  name: string;
  enabled: boolean;
  pattern: string;
  action: "block" | "ban";
  ban_duration_seconds?: number;
  priority: number;
  created_at: string;
  updated_at: string;
}

export interface RateLimitPolicy {
  id: string;
  name: string;
  enabled: boolean;
  key: string;
  requests_per_second: number;
  response_condition_enabled: boolean;
  response_status_classes?: number[];
  ban_enabled: boolean;
  ban_after_consecutive_429: number;
  ban_duration_seconds: number;
  created_at: string;
  updated_at: string;
}

export interface SecurityOverview {
  policies: SecurityPolicy[];
  rate_limit_policies: RateLimitPolicy[];
  bans: Array<{
    ip: string;
    policy_id?: string;
    policy_name?: string;
    trigger_node_id?: string;
    host?: string;
    path?: string;
    method?: string;
    expires_at: string;
    created_at: string;
    updated_at: string;
  }>;
  active_ban_count: number;
  events: Array<{
    id?: string;
    node_id?: string;
    policy_id: string;
    policy_name?: string;
    client_ip: string;
    host?: string;
    path: string;
    method?: string;
    action: "block" | "ban";
    observed_at: string;
    ban_expires_at?: string;
  }>;
  nodes: Array<{
    id: string;
    name: string;
    status: NodeStatus;
    capable: boolean;
    configured: boolean;
    rate_limit_capable: boolean;
    rate_limit_configured: boolean;
    desired_version: number;
    applied_version: number;
    last_error?: string;
  }>;
  deployment_error?: string;
}

export interface Settings {
  branding: {
    name: string;
    subtitle: string;
    logo_data_url: string;
  };
  cache: { default_size_gb: number };
  dns: { default_ttl_seconds: number };
  cloudflare: {
    source: string;
    configured: boolean;
    override_configured: boolean;
    environment_configured: boolean;
  };
  smtp: {
    enabled: boolean;
    host: string;
    port: number;
    username: string;
    from_address: string;
    recipients: string[];
    notification_categories: string[];
    security: string;
    source: string;
    override_configured: boolean;
    password_configured: boolean;
    environment_configured: boolean;
  };
  backup: {
    repository: string;
    access_key_id: string;
    region: string;
    backup_time: string;
    random_delay_seconds: number;
    source: string;
    configured: boolean;
    override_configured: boolean;
    secret_access_key_configured: boolean;
    restic_password_configured: boolean;
    environment_configured: boolean;
  };
}

export interface Message {
  id: string;
  severity: "info" | "success" | "warning" | "error";
  category: string;
  title: string;
  body?: string;
  resource_type?: string;
  resource_id?: string;
  read_at?: string;
  created_at: string;
}

export interface MessagePage {
  messages: Message[];
  unread_count: number;
}

export interface BackupRunStatus {
  version: number;
  state: string;
  attempt: number;
  max_attempts: number;
  host?: string;
  started_at: string;
  updated_at: string;
  finished_at?: string;
  error?: string;
}

export interface RestoreSnapshot {
  id: string;
  short_id: string;
  time: string;
  hostname?: string;
  paths?: string[];
  tags?: string[];
}

export interface RestoreJob {
  version: number;
  id: string;
  snapshot_id: string;
  snapshot_short_id: string;
  state: string;
  phase?: string;
  detail?: string;
  error?: string;
  schema_version?: number;
  created_at: string;
  updated_at: string;
  ready_at?: string;
  finished_at?: string;
}

export interface NodeUninstallStatus {
  node: Node;
  job?: {
    node_id: string;
    status: string;
    previous_status: NodeStatus;
    token_expires_at?: string;
    ready_at: string;
    affected_site_ids: string[];
    detail?: string;
    forced: boolean;
    created_at: string;
    updated_at: string;
  };
  blockers: Array<{
    code: string;
    site_id?: string;
    site_name?: string;
    detail: string;
  }>;
  can_generate_command: boolean;
  ready_in_seconds: number;
  uninstall_command?: string;
}
