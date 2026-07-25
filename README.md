# simple_cdn

English | [简体中文](README_CN.md)

A small self-hosted CDN for one administrator, one Debian 12 control VPS, and 3-10 Debian 12 edge VPSs. Cloudflare is authoritative DNS only: end users connect directly to the edge nodes.

Current version: `0.1.7` (defined by [`VERSION`](VERSION)).

## What is implemented

- Go control plane with versioned transactional SQLite migrations, Argon2id password login, TOTP, one-time recovery codes, CSRF protection, audit records, and a compact management UI with dedicated node/site detail pages, a persistent message center, per-node machine status, 24-hour cache outcomes, reported cache disk usage, and confirmation-protected workflows.
- Node-first enrollment: create a pending node, copy a 15-minute one-time bootstrap command, then bind all later edge calls to an internally issued mTLS client certificate.
- Per-node and fleet-wide online edge upgrades from the management UI, with eligibility reporting, mTLS task delivery, SHA-256 verification for every artifact, a detached systemd updater, new-agent heartbeat readiness, and transactional rollback.
- Global malicious-path access policies with pre-origin Nginx rejection, durable edge events, native nftables IPv4 bans, fleet reconciliation, automatic expiry, and manual unban from the management UI; plus edge-local client-IP rate policies with configurable requests-per-second thresholds and optional 2xx/3xx/4xx/5xx response-count conditions.
- Edge agent that stages versioned Nginx base, per-site HTTP, and per-site stream fragments plus certificates atomically, checks local public-port ownership, runs `nginx -t`, reloads a healthy Nginx or starts a failed/stopped Nginx, confirms a reload actually spawned a new worker generation, restores the last known-good configuration and TLS files on failure, and reports Linux host status plus `/opt/cdn-edge/cache` disk usage without blocking its heartbeat loop.
- Nginx OSS static-asset cache policy with one shared cache and a 1 GiB default total disk cap per edge node. The global node default can be overridden on an individual node, while sites share that node quota. Only common CSS, JavaScript, font, image, WebAssembly, and manifest suffixes select the cache; every other URI uses `proxy_cache off`. Cache eligibility does not inspect Authorization or Cookie request headers, which are forwarded unchanged. The policy includes normalized cache generation, cache locking, revalidation, background refresh, stale fallback, and HTTP(S) primary/backup origin failover. HTTP(S) sites automatically disable cache and response buffering for WebSocket upgrades, SSE accept headers, `X-CDN-Stream: 1`, and POST responses; full passthrough mode disables cache and buffering for the entire hostname while forwarding byte ranges. `grpc://` and `grpcs://` origins use native gRPC proxying over the client HTTP/2 listener.
- Nginx stream TCP forwarding with independently selectable client TLS termination and verified upstream TLS/SNI, dynamic upstream DNS resolution, per-port timeouts, atomic multi-file rollback, and TCP-only sites that do not open ports 80/443.
- Cloudflare DNS-only A-record reconciliation after node reachability and per-site HTTPS/SNI/certificate health hysteresis: 3 failed probes remove a node; 5 successful probes restore it. If every node is bad, DNS is deliberately left unchanged.
- Authenticated runtime settings for a 60-300 second DNS TTL, per-site published TTL overrides, encrypted Cloudflare and SMTP settings, and encrypted Restic S3/R2 backup credentials and scheduling. Database overrides take precedence over environment fallbacks without a controller restart.
- DNS-01 certificates through Certbot's Cloudflare plugin; certificate private keys remain encrypted in SQLite and are only delivered over mTLS.
- ClickHouse raw request logs with a 7-day TTL and minute aggregates with a 30-day TTL. Named TCP monitoring targets keep their latest score and per-node smart-routing state in SQLite; configurable score hysteresis and weekly `Asia/Shanghai` allow-windows can automatically drain or restore an edge node without changing site assignments. Per-round probe history enters ClickHouse through a bounded asynchronous queue, expires after 7 days, and is available as multi-target 1-hour through 7-day charts. See [docs/SMART_ROUTING.md](docs/SMART_ROUTING.md) for ownership and gating semantics. Edge access logs locally queue while the control plane is unavailable.
- SMTP alerts and encrypted Restic S3-compatible daily backups for SQLite, ClickHouse, control TLS, the internal CA, and certificate material, with bounded retry/status reporting, offline restore drills, and a staged online-restore workflow.

## Deliberate boundaries

- Single administrator, IPv4 only, Cloudflare DNS-only, a single Cloudflare account, no tenant/RBAC model, no GeoDNS, no URL-level purge, no WAF/DDoS service, and no control-plane high availability.
- A control-plane outage does not interrupt already deployed edge traffic. It prevents new deployment, DNS changes, and certificate renewal until restored.
- `Publish` is intentionally separate from `Create site`. A site is staged until it has a valid certificate; a publish task succeeds only after every affected active edge reports that it loaded the target configuration.
- Site edits and replacement certificates update a draft without changing the published site snapshot. Publishing atomically promotes that site's draft and certificate, rebuilds only its old/new assigned nodes, and renders every other site from its published snapshot.
- Nodes with HTTPS sites reject an unknown TLS SNI in a dedicated default server instead of presenting another site's certificate.
- Site-level cache invalidation increments the cache generation in the key. Existing objects are reclaimed by Nginx `inactive` and `max_size`; no unsupported OSS purge module is required.

Before upgrading an existing database to a release with published snapshots, publish or revert every pending site. Legacy rows do not contain the previous live inputs needed to reconstruct a snapshot. The controller detects historical publications without a snapshot and refuses to rebuild another site around that ambiguous state.

## Repository layout

```text
cmd/control          Control-plane executable
cmd/edge-agent       Edge agent executable
internal/control     API, auth, CA, publish, health/DNS orchestration
frontend/            React/Vite/Tailwind/shadcn management console source
internal/edge        Enrollment, mTLS polling, atomic apply, local log queue
internal/nginx       Generated Nginx cache and origin configuration
internal/integrations Cloudflare, Certbot, SMTP adapters
internal/logstore    ClickHouse access-log, monitoring-history, and aggregate storage
deploy/              Compose and environment templates
scripts/             Compose control-plane helpers and release builds
```

## Build and test

The UI build requires Node.js 24 LTS and npm 11 or newer. The generated Vite output is embedded in the Go control binary.

```bash
npm --prefix frontend ci
npm --prefix frontend run check

GOCACHE=/private/tmp/simple_cdn_go_cache \
GOMODCACHE=/private/tmp/simple_cdn_gomodcache \
GOPATH=/private/tmp/simple_cdn_gopath \
go test ./...

./scripts/build-release.sh dist
```

Browser smoke tests live in `frontend/e2e` and cover authenticated workspaces, the login screen, responsive sidebar behavior, and the shadcn/Recharts overview chart. Run `npm --prefix frontend run test:e2e` after installing Playwright Chromium.

For UI development, run the TLS control plane on `127.0.0.1:8443`, then start `npm --prefix frontend run dev`. Vite proxies authenticated API requests to the local TLS endpoint (including its development certificate) and keeps the existing hash routes.

`dist/SHA256SUMS` lets operators independently verify release artifacts. The controller hashes `EDGE_BINARY_PATH` at startup and embeds that digest in every enrollment and upgrade instruction, so no separately maintained checksum setting is required. When the controller serves the bundled edge binary, use `https://CONTROL_PUBLIC_URL/downloads/cdn-edge-agent-linux-amd64` as `EDGE_BINARY_URL`.

GitHub Actions runs the same compilation and validation checks, browser smoke tests, and a complete Docker build for every pull request. Successful `main` builds publish `ghcr.io/saginardo/simple_cdn`; the workflow never connects to production. Private deployment automation consumes the immutable digest, and the control host only pulls that image instead of compiling source or running `docker compose build`. See [the Compose deployment guide](docs/COMPOSE_DEPLOYMENT.md#github-actions-delivery).

## Control-plane installation

Docker Compose is the supported deployment for the control plane and ClickHouse. It keeps configuration, SQLite, the internal CA, certificate state, ClickHouse data, logs, and backup staging below `/opt/cdn-platform`. The existing public reverse proxy can remain separate; the controller still terminates TLS on its direct port for edge mTLS. See [docs/COMPOSE_DEPLOYMENT.md](docs/COMPOSE_DEPLOYMENT.md) for installation, backup, and restore instructions.

On a fresh Debian 12 control VPS with Docker Engine and Docker Compose, run from a trusted checkout:

```bash
sudo ./scripts/install-control-compose.sh /opt/cdn-platform
sudoedit /opt/cdn-platform/config/control.env
cd /opt/cdn-platform
sudo docker compose config --quiet
sudo docker compose pull
sudo docker compose run --rm --no-deps control keygen
```

Put the generated key in `CONTROL_ENCRYPTION_KEY`. Use a Cloudflare API token scoped only to the zones this system manages, with `Zone:Read` and `DNS:Edit`. Configure `CONTROL_TLS_DOMAIN`, `CONTROL_PUBLIC_URL`, `EDGE_CONTROL_URL`, and `ACME_EMAIL`, then start the stack:

```bash
sudo docker compose up -d
sudo docker compose ps
```

Keep `CLOUDFLARE_API_TOKEN` in `control.env` for the first control-certificate bootstrap and rollback. After administrator setup, the **Settings** view can store an encrypted runtime override. That override is used by DNS reconciliation, site certificate jobs, and subsequent control-certificate renewals; deleting it restores the environment value. SMTP follows the same whole-profile override and reset model.

The bundled ClickHouse configuration is tuned for the 2-core, 4 GiB control host: it limits the background scheduler and disables high-volume internal profiling tables such as `system.metric_log` and `system.trace_log`. CDN access logs, minute aggregates, query diagnostics, part diagnostics, errors, and asynchronous-insert diagnostics remain enabled. User-level ClickHouse limits and profiler switches are installed separately under `users.d`.

Firewall policy on the control VPS:

- TCP 443 from administrators and edge nodes.
- TCP 22 only from your administration source.
- ClickHouse port 8123 bound to localhost unless you deliberately use a separate log node.

When a conventional HTTPS reverse proxy terminates the management UI, do not send edge mTLS through that proxy. Bind the controller to a second direct TLS port, set `EDGE_CONTROL_URL` to that port, and keep `CONTROL_PUBLIC_URL` on the proxy's standard HTTPS port. Set `TRUSTED_PROXY_CIDRS` to the proxy's loopback or private address so setup restrictions, audits, and login rate limits use its `X-Real-IP` header safely.

Open `https://control.example.com/`, initialize the single administrator, add the TOTP secret to an authenticator, and store the returned recovery codes offline. The setup route becomes unavailable after the first account is created.

Before first public startup, set `SETUP_ALLOW_CIDRS` to your administrator egress CIDR whenever possible. This prevents another Internet user from racing the one-time setup endpoint.

## Edge enrollment

1. Add a node in the **Nodes** view with its fixed public IPv4.
2. Set `EDGE_BINARY_URL` to the HTTPS location of the `cdn-edge-agent-linux-amd64` release. The controller derives its SHA-256 digest from `EDGE_BINARY_PATH`.
3. Use **Enroll** and run the generated command as root on that Debian 12 VPS.
4. The agent creates its private key locally, submits a CSR using the 15-minute one-time token, receives an internal mTLS certificate, and begins heartbeats every 30 seconds.

The same generated command installs a fresh edge, migrates the legacy scattered layout, or upgrades an existing `/opt/cdn-edge` deployment. It keeps the node identity, certificates, applied version, pending access-log queue, offset, and access logs during migration; the recreatable Nginx cache starts empty. Do not publish new site state while legacy and migrated nodes coexist. See [docs/EDGE_DEPLOYMENT.md](docs/EDGE_DEPLOYMENT.md) for the layout, migration checks, backup boundary, and rollback behavior.

Agents with `machine_status_v1` attach a host snapshot to the normal heartbeat. The node detail page shows the Linux distribution and version, uptime, 1/5/15-minute load, logical CPU count and interval utilization, memory and root-filesystem usage, and RX/TX rates for the default-route interface. CPU and network rates use the interval between adjacent heartbeat samples, so a restarted agent shows a short sampling state until its second heartbeat.

After a node reports the `online_upgrade_v1` capability, the **Nodes** view compares the running agent SHA-256 with the controller's current edge artifact and exposes a per-node **Upgrade** action when they differ. **Upgrade all** evaluates every node in one request, queues every eligible outdated node, and reports nodes that are current, busy, offline, or missing the capability without aborting the rest of the fleet. Existing agents from releases before this capability must run the generated deployment/upgrade command one final time; subsequent releases can be installed entirely from the UI. Online upgrades keep the node in its current scheduling state, stage and verify the installer, binary, and both systemd units before stopping the agent, then require the new binary to complete an authenticated heartbeat before committing. Site publication, site deletion, and node uninstall are blocked for that node while the upgrade is active.

The agent keeps the last working Nginx HTTP and stream configurations if the control plane is unavailable, a new configuration fails validation, or a signaled reload is rejected asynchronously by the running master. It checks every desired public TCP port before applying state: a non-Nginx listener is reported to the publish task with its port, PID, and process name; the agent never stops that process. Once the port is released, click **Republish** and the agent clears Nginx's failed state and starts it automatically. Do not delete `/opt/cdn-edge/data` on an active edge node; it contains the node private key, mTLS certificate, applied version, and pending access-log queue. See [docs/NGINX_APPLY_SAFETY.md](docs/NGINX_APPLY_SAFETY.md) for the reload/restart boundary and exact worker and site verification commands.

The **Security** workspace applies ordered global path policies to capable HTTP edge nodes. Matching requests are closed before origin proxying; ban actions are enforced in the Agent-owned `inet simple_cdn` nftables table on ports 80/443 and synchronized across the fleet. An upgraded Agent removes the legacy table before applying the new one. Existing agents require an upgrade before policy deployment. Firewall ownership, rollout, diagnostics, and proxy-boundary constraints are documented in [docs/SECURITY_POLICIES.md](docs/SECURITY_POLICIES.md).

## Edge uninstall

Revoking authorization only invalidates the node certificate; it does not remove software or data from the edge host. To retire a host, use the separate **Uninstall node** workflow:

1. Pause scheduling or revoke authorization for the node.
2. Remove it from every site, assign replacement active nodes, and publish each changed site. A disabled site is exempt from the replacement-node requirement.
3. Start uninstall preparation. The controller removes only Cloudflare A records whose managed comment exactly identifies that node, then enforces a 75-second DNS safety wait.
4. Generate the 30-minute workflow command and run it as root on the edge host. The script stops the agent and removes `/opt/cdn-edge`, its systemd/Nginx integration links, and any legacy project paths. It validates and reloads Nginx, but preserves the Nginx package, service, system logs, and unrelated configuration.
5. A successful callback retains the node as **Uninstalled** for audit history. Deleting that control-plane record is a separate confirmation-protected action.

If Nginx validation or reload fails before cleanup is committed, the script restores the platform configuration and the previous edge-agent service state. **Force complete** only changes control-plane state when a host is permanently unreachable; it does not verify or perform remote cleanup.

## First site

1. Add the site with its hostname(s), assigned node IDs, primary origin, and optional backup origin. The controller identifies the Cloudflare zone from the hostname automatically. Sites inherit the global DNS TTL unless their draft selects a 60-300 second override; that override becomes live only when the site is published. Generated HTTPS sites default to a 128 MiB request-body limit; the site form can raise it to the fixed 256, 512, or 1024 MiB presets. HTTP/HTTPS/WebSocket proxying defaults to a 120-second upstream read/write idle timeout, selectable as 2, 6, 15, 30, or 60 minutes. Client keepalive defaults to 120 seconds and is configurable per site. WebSocket and SSE need no path declaration: WebSocket uses `Upgrade`, browser SSE uses `Accept: text/event-stream`, [OpenAI-style streaming](https://developers.openai.com/api/docs/guides/streaming-responses) is passed through for every POST, and nonstandard clients may send `X-CDN-Stream: 1`. Use HTTP(S) origins for normal sites, passthrough mode for an entire hostname that must not use disk cache, `ws://` or `wss://` for an all-WebSocket site, and `grpc://` or `grpcs://` for an all-gRPC hostname.
2. In Cloudflare, keep these hostname records as DNS-only. The control plane only manages records tagged with `cdn-platform:site=<site-id>;...`; it refuses a hostname already occupied by an untagged or another site's A record.
3. Run **Issue TLS**. The control VPS queues an asynchronous DNS-01 job via the scoped Cloudflare token and stores the resulting certificate encrypted. The Sites view polls its status; reloading the page does not cancel it. Only one active certificate job may exist per site, so repeated clicks reuse that job.
4. Run **Publish**. The controller builds each affected node's desired state and waits up to 90 seconds for assigned active nodes to validate and apply it. The Sites view shows per-node conflicts or timeout details; after resolving a conflict, click **Republish**.
5. Wait for an edge to be active and pass five node and per-site HTTPS probes. The controller then creates DNS-only A records using the site's published TTL override or the global default of 60 seconds.
6. Fetch `GET /api/sites/{site-id}/origin-allowlist` from the authenticated API and install those `/32` CIDRs in the source origin firewall/security group. This prevents direct origin bypass.

For SMTPS, IMAPS, or another TCP service, add one or more TCP rules to the same site. Each rule defines its public listen port, upstream host/port, listener TLS, upstream TLS/SNI, and timeouts. Select **TCP only** for a dedicated node that must not listen on 80/443. Before the first TCP publication on a node, rerun its generated deployment/upgrade command; the installer adds `libnginx-mod-stream`, the main-context stream include, and an Agent that advertises `tcp_stream_v1`. Publishing is rejected until every affected node reports that capability. TCP-only and HTTP sites cannot share a node, and public ports 80/443 remain reserved for the HTTP renderer. If Nginx already owns a desired port through a hand-written file, the Agent reports it as an unmanaged conflict; remove that manual listener after retaining a rollback copy, validate Nginx, then publish from the controller. TCP session and error logs stay on the edge under `/var/log/nginx/cdn-platform-tcp-*.log` and use the host's Nginx log rotation; they are not mixed into HTTP request analytics.

HTTP edge nodes expose `http://EDGE_IPV4/__cdn_health`. Published HTTP configurations also expose a site-specific `https://SITE_DOMAIN/__cdn_health`; the controller connects it directly to each assigned Edge IP while retaining the real Host, SNI, and certificate verification. TCP-only nodes are checked by connecting every desired published TCP port instead and do not require 80/443. Expose the desired public ports in the node firewall. The origin itself should permit inbound traffic only from the returned edge CIDRs.

For an HTTPS/WSS/GRPCS origin reached by IP while its certificate covers only a DNS hostname, configure the origin URL, Host header, and TLS SNI independently. See [docs/ORIGIN_TLS_SNI.md](docs/ORIGIN_TLS_SNI.md) for the IP connection example, certificate requirements, and edge-side verification commands.

### Range traffic and passthrough mode

For a whole-site proxy that does not need video caching and only needs reliable HTTP(S) Range forwarding, enable passthrough mode and republish the site. Passthrough mode is limited to HTTP(S) and disables the Nginx cache. Do not keep `proxy_cache` enabled and merely add `Range` / `If-Range`; that does not guarantee correct origin range semantics. See [docs/PASSTHROUGH_MODE.md](docs/PASSTHROUGH_MODE.md) for the activation conditions, limitations, failure analysis, and `206` verification commands.

Certificate jobs use `CERTIFICATE_ISSUE_TIMEOUT` (default `10m`) and wait 30 seconds for Cloudflare DNS-01 TXT propagation. When Certbot specifically reports `No TXT record found`, the issuer waits another 30 seconds and retries once. Other failures are returned immediately. A control-plane stop or restart marks an in-flight job as failed rather than retrying it automatically, to avoid duplicate ACME requests; click **Issue TLS** again after the controller is healthy. The authenticated APIs `GET /api/sites/{site-id}/certificate-task` and `GET /api/tasks/{task-id}` expose the persisted task state and failure detail.

## Site deletion

Deleting a site is a persisted retirement workflow rather than a metadata-only operation. Enter the exact site name in the management UI to start it. The controller disables the site, removes only Cloudflare A records whose managed comment identifies that site, publishes desired states without the site, and waits for every currently assigned active edge to confirm the new configuration. It then removes the local Certbot lineage and deletes the site metadata and encrypted certificate from SQLite.

If an active edge fails or times out, the site remains disabled in **Deleting** state and managed DNS stays withdrawn. Repair, drain, or uninstall the affected node, then retry deletion; there is no force-delete path. Audit records and deployment tasks are retained, and ClickHouse access logs continue to expire under their existing TTL. Local Certbot cleanup does not revoke the already-issued certificate at the ACME CA.

## Runtime behavior

```text
Client -> Cloudflare DNS-only A -> Edge Nginx -> primary origin -> optional backup origin
                                      |
                                      +-> disk cache / stale on upstream failure
                                          or full passthrough for configured sites

Administrator -> Control API/UI -> Cloudflare DNS / Certbot DNS-01 / SMTP / SQLite / ClickHouse
Edge agent --- mTLS ---> desired state, heartbeat, batched access logs
```

The failure window is roughly 1-2 minutes with a 60-second TTL and can approach 6 minutes with a 300-second TTL: a node needs three 15-second failed probes before it is removed, then recursive resolver caching applies to the effective TTL. When every assigned node appears unhealthy, records are retained and an alert is sent rather than intentionally publishing an empty answer set.

## Backup and restore

The Compose backup workflow uses SQLite's online backup API and a native ClickHouse backup, then writes the complete recovery set to encrypted Restic storage. It retries short-lived failures, publishes a machine-readable status consumed by the message center, and sends an SMTP alert after the final failed attempt. It retains 7 daily, 4 weekly, and 6 monthly snapshots. Repository credentials and the daily schedule can be managed from the authenticated Settings view with database-over-environment precedence; offline recovery credentials remain mandatory. The Settings view can download and validate a selected snapshot into an isolated SQLite/ClickHouse staging area while live traffic continues, then perform a confirmation-protected cutover with a short controller restart and retained rollback data. Offline verification and disaster-recovery procedures remain available in [docs/COMPOSE_DEPLOYMENT.md](docs/COMPOSE_DEPLOYMENT.md).

## Capacity and next limits

The first deployment target is less than 100 requests/second across all sites, with 7 days of raw logs. Use at least 4 vCPU, 8 GiB RAM, and a 160 GiB NVMe control VPS. Increase storage or move ClickHouse to a separate machine before retaining raw logs longer than seven days or consistently approaching 100 RPS.
