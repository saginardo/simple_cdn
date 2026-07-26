# Edge deployment and migration

Edge nodes use the Debian Nginx package and a host systemd service. Docker is not used. Debian 13 is recommended when HTTP/3 is required; Debian 12 remains a compatible HTTP/1.1 and HTTP/2 edge. simple_cdn-owned binaries, configuration, persistent state, access logs, and cache are kept below one operational root:

```text
/opt/cdn-edge/
  .layout-version
  bin/cdn-edge-agent
  config/
    edge.env
    certs/
    nginx/
      cdn-platform.conf          # generated HTTP fragment index
      cdn-platform-stream.conf   # generated stream fragment index
      fragments/
        http-v<version>-<hash>/
          00-base.conf
          site-<site-id>.conf
        stream-v<version>-<hash>/
          00-base.conf
          site-<site-id>.conf
  data/
    edge-client.key
    edge-client.crt
    edge-ca.crt
    applied-version
    access-log-offset
    access-log-cursor.json
    access-log-queue/
      00000000000000000001.ndjson
    active-upgrade-task
    upgrades/                    # transient online-upgrade state
    security-bans.json
    security-event-queue.json
    security-log-offset
  logs/access.json
  logs/security.json
  cache/
  systemd/cdn-edge-agent.service
  systemd/cdn-edge-updater@.service
```

The access-log forwarder runs independently from the desired-state polling loop. It durably appends complete Nginx records to ordered 4 MiB queue segments, advances `access-log-offset` only after the segment has been synced, and stops advancing that offset when the physical queue reaches 256 MiB. Uploads consume up to 500 events at a time and persist `access-log-cursor.json`; partially consumed segments are not rewritten and are deleted only after their final batch is acknowledged. On first start after an upgrade, the Agent atomically migrates the legacy `access-log-queue.ndjson` file into the segmented queue. A legacy file created by a temporary binary rollback is appended after existing segments on the next upgrade.

Two system integration links remain outside this root because systemd and the Debian Nginx package discover configuration there:

```text
/etc/systemd/system/cdn-edge-agent.service -> /opt/cdn-edge/systemd/cdn-edge-agent.service
/etc/systemd/system/cdn-edge-updater@.service -> /opt/cdn-edge/systemd/cdn-edge-updater@.service
/etc/nginx/conf.d/cdn-platform.conf -> /opt/cdn-edge/config/nginx/cdn-platform.conf
/etc/nginx/modules-enabled/99-cdn-platform-stream.conf
/etc/logrotate.d/cdn-edge-platform
```

The second Nginx integration file owns a top-level `stream { include ...; }` block; HTTP virtual hosts remain in `conf.d`. The installer adds marked includes to `/etc/nginx/nginx.conf`: `/opt/cdn-edge/config/nginx/cdn-platform-main.conf` before `events {}` and `/opt/cdn-edge/config/nginx/cdn-platform-events.conf` inside it. Those files are the only project-managed worker-capacity directives; uninstall restores the original directives and removes the marked includes. Each stable index includes every `*.conf` file from one content-addressed version directory. `00-base.conf` owns shared maps, cache, log formats, resolvers, and default listeners; each site file owns only that site's upstreams and HTTP or stream servers. The Agent stages both directories before changing either index, validates the combined configuration, and restores the prior indexes, fragments, and TLS files on failure. Old version directories are cleaned only after a successful apply. The installer adds Debian's `libnginx-mod-stream`, `libnginx-mod-http-lua`, `nftables`, `logrotate`, and `lz4` packages. `/etc/logrotate.d/cdn-edge-platform` rotates access and security logs at 32 MiB, retains 16 generations, and compresses old files with lz4. The Lua module supplies the cross-worker shared dictionary and request/response phases used by rate policies; no external rate-limit service is required. Nginx itself, its global configuration outside the marked includes, system error log, and the agent journal remain managed by Debian. The agent runs as root because it atomically writes site certificates and generated configuration, validates and reloads Nginx, and manages the isolated `inet simple_cdn` firewall table. Nginx cache files are owned by `www-data`.

## Fresh installation and upgrades

Use **获取部署/升级命令** on the node page and run the generated command as root on the target Debian node. The command is bound to the configured HTTPS control and binary URLs and verifies the edge binary and both systemd units by SHA-256 before installing them.

The installer is idempotent and recognizes these states:

- No CDN deployment: installs a new `/opt/cdn-edge` layout and enrolls with the 15-minute token.
- Legacy deployment: migrates `/usr/local/bin/cdn-edge-agent`, `/etc/cdn-platform`, `/var/lib/cdn-platform`, `/var/log/cdn-platform`, `/var/cache/cdn-platform`, the Nginx include, and the systemd unit.
- Existing `/opt/cdn-edge`: replaces the checksum-verified binary and service definition, adds missing stream and worker-capacity integrations, and preserves configuration data, identity, logs, and cache.

The installed environment advertises `tcp_stream_v1`, `edge_rate_limit_v1`, `nginx_capacity_v1`, `online_upgrade_v1`, `nginx_fragments_v1`, `cache_usage_v1`, `machine_status_v1`, `control_manifest_v1`, and, when nftables is available, `edge_security_v1` in every authenticated heartbeat. It additionally advertises `http3_v1` only when `nginx -V` contains `--with-http_v3_module`. The controller refuses to publish a TCP rule to a node until the stream capability is present, renders QUIC only for HTTP/3-capable nodes, renders malicious-path policies only for nodes with the access-security capability, renders Lua rate policies only for nodes with the rate-limit capability, and renders rate-to-ban escalation only when both rate-limit and access-security capabilities are present. `control_manifest_v1` lets the heartbeat carry desired-state, monitoring, security-ban, and upgrade revisions; the Agent keeps cached targets and fetches only changed resources. `nginx_capacity_v1` is advertised only by layouts with the Nginx main/events include hooks, so older nodes can save a capacity setting but need one generated deployment before it can apply. Desired state carries both the full generated configuration and its split representation: fragment-capable Agents apply the split files, while older Agents ignore the new field and keep using the full configuration. Rerun the node's generated deployment/upgrade command before its first TCP, HTTP/3, access-security, rate-limit, capacity, fragment, cache-usage, machine-status, or manifest-driven publication.

For a node that already has a control-plane certificate fingerprint, the generated upgrade command contains no new enrollment token. The installer requires the complete local mTLS key/certificate/CA set instead. This preserves the node identity and avoids leaving an unused valid enrollment token after an upgrade.

A valid layout marker and the platform integration links determine ownership. If unrelated or ambiguous files exist in both the old and new layouts, the installer stops instead of merging them.

## HTTP/3 and QUIC

An `http3_v1` node keeps `listen 443 ssl` plus `http2 on` on TCP and adds `listen 443 quic` on UDP. HTTPS responses advertise `h3` through `Alt-Svc` for 24 hours. `quic_retry on` limits address-spoofing amplification, while TLS 0-RTT remains disabled to avoid replay semantics. A node without the capability receives none of these directives, so mixed Debian 12/13 fleets can roll forward safely. Nginx upstream still labels `ngx_http_v3_module` experimental, so keep the TCP fallback and roll out one edge at a time.

The Agent treats UDP 443 as desired state rather than an incidental Nginx socket. Before applying a configuration it rejects foreign UDP owners, after reload it waits for Nginx to own the socket, and on failure it restores the previous fragments, certificates, and applied version. Capability changes are reconciled from subsequent heartbeats, including the first heartbeat after an online upgrade. The project-owned nftables ban table drops banned IPv4 sources on UDP 443 as well as TCP 80/443.

Allow inbound UDP 443 in both the operating-system firewall and the VPS/provider security group. The controller's DNS health gate deliberately continues to test HTTPS over TCP, preserving fallback availability; it cannot prove that an external UDP firewall passes QUIC. Verify from another host with an HTTP/3-capable curl:

```bash
domain=cdn.example.com
edge_ip=203.0.113.20

sudo nginx -V 2>&1 | grep -F -- '--with-http_v3_module'
sudo grep -F http3_v1 /opt/cdn-edge/config/edge.env
sudo nginx -T 2>/dev/null | grep -E 'listen 443 quic|Alt-Svc|quic_retry'
sudo ss -H -lunp '( sport = :443 )'

curl --http3-only --fail --silent --show-error \
  --resolve "$domain:443:$edge_ip" \
  --write-out '\nHTTP %{http_version}\n' \
  "https://$domain/"
```

The final line must report HTTP version `3`. If `nginx -V` lacks the module, use Debian 13 or another package that provides an ABI-compatible HTTP/3 build together with the required Debian Lua and stream modules; do not force `http3_v1` into `edge.env`. If the local UDP socket exists but the external request fails, inspect the provider firewall before changing Nginx.

## Online upgrades

The node page enables **升级** when an active or paused node has reported `online_upgrade_v1` within ten minutes and its running binary SHA-256 differs from the controller-computed digest of `EDGE_BINARY_PATH`. The reported Agent version is displayed for operators, while the digest remains the authoritative exact-artifact identity. The node list also provides **全部升级**: it evaluates the entire fleet in one request, queues one task for every eligible outdated node, reuses an existing active task, and returns a per-node result for current, stale, revoked, uninstalled, or otherwise blocked nodes. One ineligible node does not prevent eligible nodes from being queued. Releases installed before this capability require one final manual deployment/upgrade command to install the updater unit. The generated command remains available for new-node enrollment and recovery.

An online upgrade performs these steps:

1. The controller snapshots the current binary, installer, agent-unit, and updater-unit URLs and digests into a 30-minute node task delivered over mTLS.
2. The running agent downloads every artifact below `/opt/cdn-edge/data/upgrades/<task-id>`, enforces size limits, and verifies SHA-256 before starting `cdn-edge-updater@<task-id>.service`.
3. The detached updater runs the same transactional installer used by the manual command. Package downloads and artifact checks occur before the main agent is stopped; Nginx remains in the node's current scheduling and DNS state.
4. After installing and starting the new agent, the installer waits for that exact binary to complete an authenticated heartbeat. Only then does it commit and report success. A failed checksum, package operation, Nginx validation/reload, service start, or readiness check restores the previous binary, units, Nginx integration, and service state.
5. The restarted agent reports the durable local result. A repeated instruction is not executed twice. The agent treats systemd's transitional updater states as running and waits through a terminal-state confirmation window for a durable result. If that result is missed but a later heartbeat proves the target binary is running, the controller reconciles the task to success; a genuinely interrupted updater remains retryable after a fresh heartbeat clears it.

Only one online upgrade may be active per node. Site publish/delete confirmation and node uninstall cannot start against that node until it completes. Revoking node authorization remains available as an emergency action and terminates the controller task. Online upgrade does not provide historical version selection or an operator-triggered downgrade; the controller's configured artifact is authoritative.

## Legacy node migration

Migrate one node at a time. While legacy and migrated nodes coexist, do not publish or republish sites: the new controller renders `/opt/cdn-edge` paths that an old-layout agent cannot use.

1. Confirm the node is active, has no `last_error`, and has a replacement edge serving each public site if interruption is unacceptable.
2. In the node page, generate a fresh deployment/upgrade command and run it as root on the node.
3. The installer stops only `cdn-edge-agent`; Nginx keeps serving the already loaded configuration until the integration links are switched.
4. It copies the mTLS identity, site certificates, applied version, pending log queue, queue offset, and rotated logs. It moves the active access log on the same filesystem so the offset and open Nginx file descriptor remain valid.
5. It converts the generated Nginx paths, switches the system integration links, runs `nginx -t`, and cold-restarts Nginx because the running `cdn_cache` zone cannot change its disk path during reload. This produces a short interruption on that node. It then starts the agent, waits for the mTLS identity, and checks the local health endpoint.
6. Only after all checks succeed does it remove legacy paths. The Nginx disk cache is deliberately discarded and rebuilt below `/opt/cdn-edge/cache`.

If the active access log and `/opt/cdn-edge` are on different filesystems, the installer stops before switching Nginx. This avoids silently losing or replaying request logs. Move `/opt` onto the root filesystem or perform an explicitly planned maintenance migration before retrying.

After each node, verify:

```bash
sudo systemctl is-active cdn-edge-agent nginx
sudo systemctl cat cdn-edge-agent
sudo nginx -t
curl -fsS http://127.0.0.1/__cdn_health
sudo ss -H -lunp '( sport = :443 )'
sudo find /opt/cdn-edge -maxdepth 3 -type f -o -type l
```

For a TCP-only node, replace the HTTP health request with listener checks for its published ports, for example `ss -ltnp '( sport = :9465 or sport = :9993 )'`. A TCP-only desired state intentionally has no Nginx listeners on 80/443.

After every node has migrated, run `cdn-control publish-all` through the control Compose service, wait for each node's applied version to catch up, and confirm the control UI shows recent heartbeats without `last_error`.

The edge agent advertises `cache_usage_v1` and scans `/opt/cdn-edge/cache` in the background every five minutes. All cache-enabled sites on a node share one Nginx cache path and zone. The control setting is a 1 GB default total quota for each node, and the node detail page can override it for nodes with smaller or larger disks. Updating either cache quota republishes that node's desired state immediately; the effective quota and dynamic cache-zone size are then written to Nginx on the next Agent poll. Heartbeats report that effective total limit, allocated cache-file bytes, and collection time. After a new configuration takes over, the agent removes only retired platform-generated cache directories; unknown operator-created directories are preserved. The node cache page treats reports older than 15 minutes as stale. This capacity is the managed Nginx cache limit, not the host filesystem's total size; access-log or ClickHouse failures do not hide the last cache-space report.

The agent also advertises `machine_status_v1` and `machine_status_stream_v1`. It samples Linux host state every five seconds and posts it over the same reusable mTLS HTTP/2 transport used by the normal 30-second heartbeat; the heartbeat includes the latest sample as a rolling-upgrade fallback. It reads the distribution from `os-release`, uses `/etc/debian_version` for the Debian point release, and reports uptime, 1/5/15-minute load, logical CPU count, memory use, and root-filesystem use. CPU utilization and default-route-interface RX/TX speeds are averages between adjacent samples; the first sample after an agent restart intentionally reports no interval rate. The controller retains only the newest collected snapshot in memory, marks streaming-capable data stale after 15 seconds, and does not persist machine-status history. A control-plane restart clears the in-memory snapshot until the next five-second report, and the node detail page omits the machine-status section while no snapshot is available. Collection or report failures do not block configuration sync, access-log upload, or the heartbeat itself.

The restart requirement, asynchronous reload failure mode, worker-generation check, and per-site HTTPS/SNI acceptance commands are documented in [NGINX_APPLY_SAFETY.md](NGINX_APPLY_SAFETY.md).

## Failure and rollback

Before switching paths, the installer saves the current Nginx entry and root configuration, capacity files, logrotate policy, both systemd units, default Nginx site, and service state. A failed Nginx validation/restart, agent start, enrollment, health check, or online-upgrade heartbeat readiness check restores those entries and the previous edge-agent state. Legacy rollback cold-restarts Nginx with the restored cache path so the running master cannot retain the failed migration state. Temporary transaction directories are removed on success and failure.

For a fresh node, a failure after successful enrollment may consume the one-time token. Generate a new deployment command before retrying. Do not manually combine a partial legacy layout with a marked `/opt/cdn-edge` layout.

## Backup boundary

Back up these non-recreatable paths:

- `/opt/cdn-edge/config`: agent settings and site TLS certificates.
- `/opt/cdn-edge/data`: node mTLS identity, applied version, and pending access-log delivery state.

`/opt/cdn-edge/logs` is optional if uploaded access logs are already retained by the control plane. The checksum-verified binary, generated systemd unit, generated Nginx configuration, and `/opt/cdn-edge/cache` are recreatable and normally excluded from backup.

Use the control-plane **卸载节点** workflow for permanent removal. It removes the `/opt/cdn-edge` tree and integration links while preserving the Debian Nginx package and unrelated host configuration.
