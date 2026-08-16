# simple_cdn

English | [简体中文](README_CN.md)

A small self-hosted CDN for one administrator, one Debian control VPS, and 3-10 Debian 12 or 13 edge VPSs. The controller distributes the same project-built Nginx to both edge releases, including HTTP/2, HTTP/3, stream, and Lua support. Cloudflare is authoritative DNS only: end users connect directly to the edge nodes.

Published versions are derived from `vMAJOR.MINOR.PATCH` Git tags. See the repository's [version tags](https://github.com/saginardo/simple_cdn/tags).

For the maintained documentation map, configuration reference, and operational guides, see [Documentation](docs/README.md).

## What is implemented

- Go control plane with versioned transactional SQLite migrations, Argon2id password plus TOTP login, passwordless passkey login, one-time recovery codes, persistent authentication rate limits, TOTP replay protection, CSRF protection, audit records, and a compact management UI. Login falls back to password plus TOTP when passkeys fail or the browser does not support them; **Sign-in and security** settings can replace TOTP and add or disable passkeys, while TOTP always remains enabled. The UI includes dedicated node/site detail pages, a persistent message center, per-node machine status, 24-hour cache outcomes, reported cache disk usage, and confirmation-protected workflows.
- Node-first enrollment: create a pending node, copy a 15-minute one-time bootstrap command, then bind all later edge calls to an internally issued mTLS client certificate.
- Per-node and fleet-wide online edge upgrades from the management UI, with eligibility reporting, mTLS task delivery, SHA-256 verification for every artifact, a detached systemd updater, new-agent heartbeat readiness, and transactional rollback.
- Capability-gated WAF processing chains with site scope, ordered AND conditions across paths, query strings, headers, bodies, User-Agent, and client IP; terminal allow/block/ban actions, continuing log actions, six managed attack-detection rules, durable events, and synchronized nftables IPv4 bans. Site-scoped browser proof of work and edge-local client-IP rate policies share the same Security workspace. See [docs/SECURITY_POLICIES.md](docs/SECURITY_POLICIES.md).
- Edge agent that stages versioned Nginx base, per-site HTTP, and per-site stream fragments plus certificates atomically, checks local TCP and UDP public-port ownership, validates `/opt/cdn-edge/nginx/sbin/nginx`, reloads a healthy Nginx or starts a failed/stopped Nginx, confirms a reload actually spawned a new worker generation, restores the last known-good configuration and TLS files on failure, and reports Linux host status plus `/opt/cdn-edge/cache` disk usage without blocking its heartbeat loop.
- Reproducible managed Nginx AMD64 bundles with a bundled 1.30.4 bootstrap fallback and independently released stable updates. Source hashes, hardening flags, HTTP/2, HTTP/3, stream, NDK, lua-nginx-module, ngx_brotli, zstd-nginx-module, and the private OpenResty LuaJIT/runtime remain pinned per build. The transactional installer records and purges Debian Nginx packages, installs the binary, configuration, runtime, logs, cache, and service below `/opt/cdn-edge`, and restores exact package versions and `/etc/nginx` if the attempt fails.
- Nginx OSS static-asset cache policy with one shared cache and a 1 GiB default total disk cap per edge node. The global node default can be overridden on an individual node, while sites share that node quota. Only common CSS, JavaScript, font, image, WebAssembly, and manifest suffixes select the cache; every other URI uses `proxy_cache off`. Requests carrying either Authorization or Cookie bypass reads and writes to the shared cache, while those headers are still forwarded unchanged to the origin. The policy includes normalized cache generation, cache locking, revalidation, background refresh, stale fallback, and HTTP(S) primary/backup origin failover. HTTP(S) sites automatically disable cache and response buffering for WebSocket upgrades, SSE accept headers, `X-CDN-Stream: 1`, and POST responses; full passthrough mode disables cache and buffering for the entire hostname while forwarding byte ranges. `grpc://` and `grpcs://` origins use native gRPC proxying over the client HTTP/2 listener.
- Per-site dynamic gzip, Brotli, and Zstandard compression is enabled by default, with a configurable MIME exclusion list that defaults to `text/event-stream`. Full-site, exact-URL, and path-prefix cache invalidation use cache-key generations, and an invalidation can schedule bounded local prewarm requests on every assigned edge. Access logs and 30-day minute aggregates expose `Content-Encoding`, compressed response counts, estimated or exact bytes saved, and per-codec hits. See [docs/COMPRESSION_AND_CACHE_CONTROL.md](docs/COMPRESSION_AND_CACHE_CONTROL.md).
- Managed static resources up to 32 MiB use content-addressed controller storage, mTLS-authorized edge synchronization with size and SHA-256 verification, and exact per-site URL bindings. Eligible public files are atomically precompressed as `.gz`, `.br`, and `.zst` sidecars and served directly by Nginx with selectable cache headers, MIME type, ETag, GET/HEAD enforcement, WAF/PoW/rate processing, and atomic cleanup when no longer referenced. See [docs/STATIC_ASSETS.md](docs/STATIC_ASSETS.md).
- Capability-gated shared origin pools combine sites with identical protocol, address, Host, and SNI identities. Pool size adapts to each node's `worker_connections` and reference weight. Edges reuse dedicated probe connections about every five seconds and run fresh TCP/TLS probes every 32-48 seconds; either layer can open the circuit after two failures, while recovery requires confirmation from both. Include-file changes are serialized with desired-state reloads and rolled back on failure. Access logs and 30-day minute aggregates expose real-request origin connect, first-byte, full-response, and reuse metrics, while node details show current Nginx origin TCP connection counts and the latest in-memory two-layer probe snapshot. See [docs/ORIGIN_CONNECTIONS.md](docs/ORIGIN_CONNECTIONS.md).
- Per-site, capability-gated HTTP/3 over QUIC on UDP 443, disabled by default. The installer advertises `http3_v1` only when the installed Nginx reports `--with-http_v3_module`; an opted-in site on a capable node adds a QUIC listener, `Alt-Svc`, address-validation retry, UDP conflict checks, post-reload listener verification, and automatic capability reconciliation while retaining TCP 443 HTTP/1.1 and HTTP/2 fallback. IP bans cover UDP 443 as well as TCP 80/443.
- Optional capability-gated HTTP/2 origin transport: HTTPS origins can use TLS HTTP/2 and HTTP origins can use H2C, while existing sites stay on HTTP/1.1 by default and WebSocket Upgrade traffic uses an isolated HTTP/1.1 upstream. Service and cold probes enforce the selected protocol.
- Capability-gated WireGuard origin tunnels with locally generated per-host keys, same-host protection, revision convergence, private-address publishing, per-direction production shaping, one-time origin installation, managed nftables rules, and handshake-gated direct-TCP versus tunneled TCP/UDP performance tests. A site may use HTTP/H2C or cleartext gRPC inside the encrypted tunnel to remove origin TLS certificate management, or retain HTTPS/GRPCS with its original Host and SNI. See [docs/WIREGUARD_ORIGIN.md](docs/WIREGUARD_ORIGIN.md).
- Edge-local Nginx runtime telemetry through a Unix-only `stub_status` listener, reported with the ephemeral machine snapshot. PCRE JIT, persistent QUIC host keys, shared TLS sessions, and bounded graceful worker drain are part of the managed Nginx configuration.
- Nginx stream TCP forwarding with independently selectable client TLS termination and verified upstream TLS/SNI, dynamic upstream DNS resolution, per-port timeouts, atomic multi-file rollback, and TCP-only sites that do not open ports 80/443.
- Cloudflare DNS-only A-record reconciliation after node reachability and per-site HTTPS/SNI/certificate health hysteresis. A site may predeploy to standby nodes that stay outside DNS during normal operation; healthy standbys enter DNS only after every primary is unavailable, and any recovered primary automatically replaces them. Three failed probes remove a node and five successful probes restore it. If both pools are unhealthy, DNS is deliberately left unchanged.
- Authenticated runtime settings for a 60-300 second DNS TTL, per-site published TTL overrides, encrypted Cloudflare and SMTP settings, and encrypted Restic S3/R2 backup credentials and scheduling. Database overrides take precedence over environment fallbacks without a controller restart.
- DNS-01 certificates through Certbot's Cloudflare plugin; certificate private keys remain encrypted in SQLite and are only delivered over mTLS.
- ClickHouse raw request logs with a 7-day TTL and minute aggregates with a 30-day TTL. The edge generates and returns one canonical `X-Request-ID` across HTTP, WebSocket, and gRPC primary/backup origin paths while retaining client IDs, origin response IDs, transfer completion, and origin byte counts; the console searches all three ID sources. See [docs/REQUEST_TRACING.md](docs/REQUEST_TRACING.md). Named TCP monitoring targets keep their latest score and per-node smart-routing state in SQLite; configurable score hysteresis and weekly `Asia/Shanghai` allow-windows can automatically drain or restore an edge node without changing site assignments. Per-round probe history enters ClickHouse through a bounded asynchronous queue, expires after 7 days, and is available as multi-target 1-hour through 7-day charts. See [docs/SMART_ROUTING.md](docs/SMART_ROUTING.md) for ownership and gating semantics. Edge access logs locally queue while the control plane is unavailable.
- SMTP alerts and encrypted Restic S3-compatible daily backups for SQLite, ClickHouse, control TLS, the internal CA, and certificate material, with bounded retry/status reporting, offline restore drills, and a staged online-restore workflow.

## Deliberate boundaries

- Single administrator, IPv4 only, Cloudflare DNS-only, a single Cloudflare account, no tenant/RBAC model, no GeoDNS, no managed bot-reputation/CAPTCHA or volumetric DDoS service, and no control-plane high availability.
- A control-plane outage does not interrupt already deployed edge traffic. It prevents new deployment, DNS changes, and certificate renewal until restored.
- `Publish` is intentionally separate from `Create site`. A site is staged until it has a valid certificate; a publish task succeeds only after every affected active edge reports that it loaded the target configuration.
- Site edits and replacement certificates update a draft without changing the published site snapshot. Publishing atomically promotes that site's draft and certificate, rebuilds only its old/new assigned nodes, and renders every other site from its published snapshot.
- Nodes with HTTPS sites reject an unknown TLS SNI in a dedicated default server instead of presenting another site's certificate.
- Cache invalidation changes the full-site, exact-URL, or path-prefix generation in the cache key. Existing objects are reclaimed by Nginx `inactive` and `max_size`; no unsupported OSS purge module is required and disk space is not reclaimed synchronously.

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
docs/                Architecture, configuration, deployment, and operations guides
scripts/             Compose control-plane helpers and release builds
```

## Build and test

The UI build requires Node.js 24 LTS and npm 11 or newer. Docker is required for the managed Nginx artifact. The generated Vite output is embedded in the Go control binary.

```bash
npm --prefix frontend ci
npm --prefix frontend run check

GOCACHE=/private/tmp/simple_cdn_go_cache \
GOMODCACHE=/private/tmp/simple_cdn_gomodcache \
GOPATH=/private/tmp/simple_cdn_gopath \
go test ./...

./scripts/build-release.sh dist
./scripts/test-nginx-artifact.sh dist/cdn-nginx-linux-amd64.tar.gz
./scripts/test-edge-installer-debian.sh dist/cdn-nginx-linux-amd64.tar.gz
```

An untagged local build uses `0.0.0-dev+<commit>` (plus `.dirty` for modified worktrees). A clean checkout exactly at a valid `vMAJOR.MINOR.PATCH` tag uses that tag as the release version; no version file needs editing.

Browser smoke tests live in `frontend/e2e` and cover authenticated workspaces, the login screen, responsive sidebar behavior, and the shadcn/Recharts overview chart. Run `npm --prefix frontend run test:e2e` after installing Playwright Chromium.

For UI development, run the TLS control plane on `127.0.0.1:8443`, then start `npm --prefix frontend run dev`. Vite proxies authenticated API requests to the local TLS endpoint (including its development certificate) and keeps the existing hash routes.

`dist/SHA256SUMS` lets operators independently verify the controller, edge Agent, and managed Nginx bundle. The controller hashes `EDGE_BINARY_PATH` and validates `NGINX_BUNDLE_PATH` at startup, then embeds both digests in enrollment and upgrade instructions. When the controller serves its bundled artifacts, use `/downloads/cdn-edge-agent-linux-amd64` and `/downloads/cdn-nginx-linux-amd64.tar.gz` below the configured edge-control URL.

GitHub Actions runs the same compilation and validation checks, browser smoke tests, and a complete Docker build for every pull request. Successful `main` builds publish development-versioned `main` and `sha-<commit>` images. Pushing a valid `vMAJOR.MINOR.PATCH` tag publishes the matching stable image without editing project files. The workflow never connects to production. Private deployment automation consumes an immutable digest, and the control host only pulls that image instead of compiling source or running `docker compose build`. See [the Compose deployment guide](docs/COMPOSE_DEPLOYMENT.md#github-actions-delivery).

The separate `Managed Nginx stable update` workflow checks the **Stable version** section on nginx.org once per day. When that version is newer than `deploy/nginx/VERSION`, it downloads and hashes the official source, builds the existing amd64 bundle, runs both Debian 12/13 artifact and migration suites, and publishes a persistent `nginx-v<version>` GitHub Release only after every check succeeds. Its release manifest binds the stable channel, official source URL and hash, repository commit, bundle size, and bundle SHA-256. This workflow does not rebuild or publish the control image. The complete trust chain and administrator approval procedure are in [docs/NGINX_UPDATES.md](docs/NGINX_UPDATES.md).

## Control-plane installation

Docker Compose is the supported deployment for the control plane and ClickHouse. It keeps configuration, SQLite, the internal CA, certificate state, ClickHouse data, logs, Nginx artifacts, and backup staging below `/opt/cdn-platform`. The existing public reverse proxy can remain separate; the controller still terminates TLS on its direct port for edge mTLS. See [docs/COMPOSE_DEPLOYMENT.md](docs/COMPOSE_DEPLOYMENT.md) and the [configuration reference](docs/CONFIGURATION.md) for installation, backup, and restore instructions.

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

The controller checks `NGINX_UPDATE_GITHUB_REPOSITORY` immediately at startup and then every `NGINX_UPDATE_CHECK_INTERVAL` (default `24h`). It accepts only non-draft, non-prerelease `nginx-v<version>` releases whose manifest declares `channel: stable`, then independently validates the downloaded archive and stores it under `$CONTROL_DATA_DIR/nginx-artifacts`. The existing `./data/control` volume persists these files and the normal backup includes them. A public repository needs no token; set `NGINX_UPDATE_GITHUB_TOKEN` for a private repository or additional API quota.

A validated download appears in the message center and in the **Nodes** view as a candidate. An administrator must explicitly set it as the upgrade target, then use a single-node or **Upgrade all** operation. Promotion never upgrades nodes automatically. Bundle and generated-installer URLs contain the artifact SHA-256, so switching the current target does not change files referenced by in-flight tasks. The image-bundled Nginx remains the bootstrap fallback; after deploying this controller capability once, later Nginx releases do not require a controller image update.

The bundled ClickHouse configuration is tuned for the 2-core, 4 GiB control host: it limits the background scheduler and disables high-volume internal profiling tables such as `system.metric_log` and `system.trace_log`. CDN access logs, minute aggregates, query diagnostics, part diagnostics, errors, and asynchronous-insert diagnostics remain enabled. User-level ClickHouse limits and profiler switches are installed separately under `users.d`.

Firewall policy on the control VPS:

- Public management HTTPS (normally TCP 443) from administrators and the optional reverse proxy.
- The controller's direct `CONTROL_LISTEN` port (the Compose example is TCP 8443) from edge nodes and the local certificate/health services.
- TCP 22 only from your administration source.
- ClickHouse port 8123 bound to localhost unless you deliberately use a separate log node.

When a conventional HTTPS reverse proxy terminates the management UI, do not send edge mTLS through that proxy. Bind the controller to a second direct TLS port, set `EDGE_CONTROL_URL` to that port, and keep `CONTROL_PUBLIC_URL` on the proxy's standard HTTPS port. Set `TRUSTED_PROXY_CIDRS` to the proxy's loopback or private address so setup restrictions, audits, and login rate limits use its `X-Real-IP` header safely.

On the first startup, the controller creates a 32-byte one-time initialization token at `/var/lib/cdn-platform/initialization-token` (or `CONTROL_INITIALIZATION_TOKEN_FILE`) with `0600` permissions. Read that local file as an operator and enter it with the administrator password. The controller then returns a TOTP secret and recovery codes without creating an account or consuming the token. Add the secret to an authenticator, store the recovery codes offline, and enter the current TOTP code; only that successful confirmation creates the administrator and removes the token file. The confirmation code is consumed, so use the next authenticator code or a recovery code for the first sign-in.

After the first login, add a passkey or replace TOTP under **Settings → Sign-in and security**. Authentication-factor changes require a login or password-plus-existing-TOTP/recovery verification from the last five minutes and revoke the administrator's other sessions. Passkeys are bound to the `CONTROL_PUBLIC_URL` hostname, so it must match the HTTPS address used by the browser. Credentials registered for previous hostnames remain visible and deletable after that value changes; configured Passkey state is shown separately from whether the current hostname can use it. Passkey login can be disabled, but TOTP cannot be disabled and always remains the fallback.

Before first public startup, set `SETUP_ALLOW_CIDRS` to your administrator egress CIDR whenever possible. This prevents another Internet user from racing the one-time setup endpoint.

## Edge enrollment

1. Add a node in the **Nodes** view with its fixed public IPv4.
2. Set `EDGE_BINARY_URL` to its HTTPS location. The controller derives the Agent digest from `EDGE_BINARY_PATH`; `NGINX_BUNDLE_PATH` supplies the validated bootstrap fallback, while approved managed Nginx releases use the controller's content-addressed download routes.
3. Use **Enroll** and run the generated command as root on that Debian 12 or 13 VPS.
4. The agent creates its private key locally, submits a CSR using the 15-minute one-time token, receives an internal mTLS certificate, and begins heartbeats every 30 seconds. Each heartbeat returns a compact revision manifest, so desired state, monitoring targets, security bans, and upgrade instructions are fetched only when their revision changes. Slow sync, probe, and upgrade work runs independently and cannot delay the heartbeat cadence.

The same generated command installs a fresh edge, migrates the legacy scattered layout, or upgrades an existing `/opt/cdn-edge` deployment. It checksum-verifies and installs both the Agent and managed Nginx, removes Debian Nginx after recording rollback data, and keeps the node identity, certificates, applied version, pending access-log queue, offset, and access logs. Do not publish new site state while legacy and migrated nodes coexist. See [docs/EDGE_DEPLOYMENT.md](docs/EDGE_DEPLOYMENT.md) for the layout, build, migration checks, backup boundary, and rollback behavior.

The installer detects HTTP/3 from `/opt/cdn-edge/nginx/sbin/nginx -V`; it never sends QUIC directives to an incompatible node. HTTP/3 is off for every new and existing site until it is enabled under that site's traffic settings. After an updated site setting is published, or a node capability changes, the node's desired state is rebuilt automatically. Public HTTP/3 additionally requires both the host and provider firewalls to allow inbound UDP 443. Blocking UDP does not remove TCP fallback, but clients may incur a failed QUIC attempt before falling back.

Agents with `machine_status_adaptive_v1` retain the legacy five-second complete snapshot until the controller successfully negotiates an adaptive sampling policy over the mTLS SSE stream. A negotiated edge without a visible node detail page collects the complete host snapshot every 60 seconds and reports operational origin health and connection state independently every five seconds. When a visible node detail page opens its authenticated SSE connection, the policy raises the complete host snapshot to every five seconds and enables lightweight RX/TX sampling: the edge primes the default-route counters and then reports interval rates every second. After the last visible subscriber leaves and a 15-second reconnect grace expires, the fast network sampler stops, the host snapshot returns to 60 seconds, and the five-second origin snapshots continue. A missing legacy endpoint or a controller rollback returns the edge to the five-second complete snapshot without surfacing an unsupported-feature error. After a policy has been negotiated, a transient policy-stream failure retains the last valid cadence for a 60-second recovery grace, retries EOF/read-timeout failures no more often than every 15 seconds, and is omitted from heartbeat errors; if the grace expires, the edge falls back to the five-second complete snapshot and reports the failure until a valid policy is received. Older `machine_status_stream_v1` agents retain their fixed cadence. The normal 30-second heartbeat also carries the latest complete snapshot. The controller keeps only the latest host, origin, and network snapshots in memory; a 30-second detail refresh remains as a disconnect fallback. No machine-status history is persisted, and the page omits the machine-status section until a complete snapshot is available. The page shows the Linux distribution and version, uptime, 1/5/15-minute load, logical CPU count and interval utilization, memory and root-filesystem usage, and RX/TX rates for the default-route interface.

After a node reports `online_upgrade_v1` and `nginx_bundle_v1`, the **Nodes** view compares both its running Agent SHA-256 and installed Nginx bundle SHA-256 with the controller artifacts. **Upgrade all** evaluates every node in one request and reports current, busy, offline, incapable, or outdated nodes independently. Existing agents from before these capabilities need one final generated deployment command. Online upgrades stage and verify the installer, Agent, Nginx bundle, and all three systemd units before stopping services, then require an authenticated heartbeat that reports both target digests before committing. Site publication, site deletion, and node uninstall are blocked for that node while the upgrade is active.

Upgrade artifacts use a certificate-free client and connection pool separate from the mTLS control channel. Access logs are sent when a batch reaches 200 events or 512 KiB, with a two-second maximum wait for low traffic, and payloads over 1 KiB use gzip. Controller health reconciliation uses reusable lightweight node probes every 15 seconds plus fresh-connection node, HTTPS/SNI, and TCP probes every 60 seconds.

The agent keeps the last working Nginx HTTP and stream configurations if the control plane is unavailable, a new configuration fails validation, or a signaled reload is rejected asynchronously by the running master. It checks every desired public TCP and UDP port before applying state: a non-Nginx listener is reported to the publish task with its protocol, port, PID, and process name; the agent never stops that process. Once the port is released, click **Republish** and the agent clears Nginx's failed state and starts it automatically. Do not delete `/opt/cdn-edge/data` on an active edge node; it contains the node private key, mTLS certificate, applied version, and pending access-log queue. See [docs/NGINX_APPLY_SAFETY.md](docs/NGINX_APPLY_SAFETY.md) for the reload/restart boundary and exact worker and site verification commands.

The **Security** workspace applies an editable, site-aware WAF chain before rate limiting and origin handling. It supports path, URI, query, method, host, User-Agent, client-IP, header, and bounded body conditions, plus site-scoped browser PoW. Block and ban actions stop the request before origin proxying; synchronized IPv4 bans are enforced in the Agent-owned `inet simple_cdn` nftables table on TCP 80/443 and QUIC UDP 443. Existing agents must advertise the corresponding WAF, PoW, rate, and ban capabilities before those controls are deployed. Execution order, rollout, firewall ownership, and protocol boundaries are documented in [docs/SECURITY_POLICIES.md](docs/SECURITY_POLICIES.md).

## Edge uninstall

Revoking authorization only invalidates the node certificate; it does not remove software or data from the edge host. To retire a host, use the separate **Uninstall node** workflow:

1. Pause scheduling or revoke authorization for the node.
2. Remove it from every site, assign replacement active nodes, and publish each changed site. A disabled site is exempt from the replacement-node requirement.
3. Start uninstall preparation. The controller removes only Cloudflare A records whose managed comment exactly identifies that node, then enforces a 75-second DNS safety wait.
4. Generate the 30-minute workflow command and run it as root on the edge host. Before making changes, the script displays the bound node name, UUID, and public IPv4 and requires the exact `UNINSTALL <node UUID>` phrase from the controlling terminal. Missing or mismatched confirmation leaves the host and control-plane workflow unchanged. For layout version 2, the script stops the managed Agent and Nginx, removes `/opt/cdn-edge`, its systemd links, sysctl/logrotate integration, and legacy project paths. It does not reinstall Debian Nginx or recreate `/etc/nginx`.
5. A successful callback retains the node as **Uninstalled** for audit history. Deleting that control-plane record is a separate confirmation-protected action.

If Nginx validation or reload fails before cleanup is committed, the script restores the platform configuration and the previous edge-agent service state. **Force complete** only changes control-plane state when a host is permanently unreachable; it does not verify or perform remote cleanup.

## First site

1. Add the site with its hostname(s), assigned node IDs, primary origin, and optional backup origin. The controller identifies the Cloudflare zone from the hostname automatically. Sites inherit the global DNS TTL unless their draft selects a 60-300 second override; that override becomes live only when the site is published. Generated HTTPS sites default to a 128 MiB request-body limit; the site form can raise it to the fixed 256, 512, or 1024 MiB presets. HTTP/HTTPS/WebSocket proxying defaults to a 120-second upstream read/write idle timeout, selectable as 2, 6, 15, 30, or 60 minutes. Client keepalive defaults to 120 seconds and is configurable per site. WebSocket and SSE need no path declaration: WebSocket uses `Upgrade`, browser SSE uses `Accept: text/event-stream`, [OpenAI-style streaming](https://developers.openai.com/api/docs/guides/streaming-responses) is passed through for every POST, and nonstandard clients may send `X-CDN-Stream: 1`. Use HTTP(S) origins for normal sites, passthrough mode for an entire hostname that must not use disk cache, `ws://` or `wss://` for an all-WebSocket site, and `grpc://` or `grpcs://` for an all-gRPC hostname.
2. In Cloudflare, keep these hostname records as DNS-only. The control plane only manages records tagged with `cdn-platform:site=<site-id>;...`; it refuses a hostname already occupied by an untagged or another site's A record.
3. After the site is created, the control VPS immediately queues an asynchronous DNS-01 job via the scoped Cloudflare token and stores the resulting certificate encrypted. The Sites view polls its status; reloading the page does not cancel it. Only one active certificate job may exist per site.
4. Run **Publish**. The controller first checks TLS issuance state. It does not create a publish task while issuance is incomplete and instead reports that the certificate is being requested. A failed or missing issuance task is queued again during this publish preflight. Once issuance succeeds, the controller builds each affected node's desired state and waits up to 90 seconds for assigned active nodes to validate and apply it. The Sites view shows per-node conflicts or timeout details; after resolving a conflict, click **Republish**.
5. Wait for an edge to be active and pass five node and per-site HTTPS probes. The controller then creates DNS-only A records using the site's published TTL override or the global default of 60 seconds.
6. Fetch `GET /api/sites/{site-id}/origin-allowlist` from the authenticated API and install those `/32` CIDRs in the source origin firewall/security group. This prevents direct origin bypass.

For SMTPS, IMAPS, or another TCP service, add one or more TCP rules to the same site. Each rule defines its public listen port, upstream host/port, listener TLS, upstream TLS/SNI, and timeouts. Select **TCP only** for a dedicated node that must not listen on 80/443. Publishing is rejected until every affected node reports the managed bundle's `tcp_stream_v1` capability. TCP-only and HTTP sites cannot share a node, and public ports 80/443 remain reserved for the HTTP renderer. If Nginx already owns a desired port through a hand-written file, the Agent reports it as an unmanaged conflict; remove that manual listener after retaining a rollback copy, validate Nginx, then publish from the controller. TCP session and error logs stay under `/opt/cdn-edge/logs` and use the project logrotate policy; they are not mixed into HTTP request analytics.

HTTP edge nodes expose `http://EDGE_IPV4/__cdn_health`. Published HTTP configurations also expose a site-specific `https://SITE_DOMAIN/__cdn_health`; the controller connects it directly to each assigned Edge IP while retaining the real Host, SNI, and certificate verification. TCP-only nodes are checked by connecting every desired published TCP port instead and do not require 80/443. Expose TCP 80/443 and, when at least one published site enables HTTP/3 on an `http3_v1` node, UDP 443 in the node and provider firewalls. The controller deliberately keeps health qualification on the TCP fallback path, so verify QUIC separately with the commands in the edge deployment guide. The origin itself should permit inbound traffic only from the returned edge CIDRs.

For an HTTPS/WSS/GRPCS origin reached by IP while its certificate covers only a DNS hostname, configure the origin URL, Host header, and TLS SNI independently. See [docs/ORIGIN_TLS_SNI.md](docs/ORIGIN_TLS_SNI.md) for the IP connection example, certificate requirements, and edge-side verification commands.

To keep the origin off the public application path, create a managed tunnel in the **WireGuard** workspace, run its one-time command on the origin, wait for the origin and every selected edge revision to converge, then select that tunnel under the site's origin path. Use HTTP, H2C, or `grpc://` inside WireGuard when the origin should not manage TLS; use HTTPS/GRPCS when application-layer TLS is still required. The installer does not close the application's public port, so enforce that separately in the origin firewall. Firewall, UDP-throttling, performance-test, update, and uninstall behavior is documented in [docs/WIREGUARD_ORIGIN.md](docs/WIREGUARD_ORIGIN.md).

### Range traffic and passthrough mode

For a whole-site proxy that does not need video caching and only needs reliable HTTP(S) Range forwarding, enable passthrough mode and republish the site. Passthrough mode is limited to HTTP(S) and disables the Nginx cache. Do not keep `proxy_cache` enabled and merely add `Range` / `If-Range`; that does not guarantee correct origin range semantics. See [docs/PASSTHROUGH_MODE.md](docs/PASSTHROUGH_MODE.md) for the activation conditions, limitations, failure analysis, and `206` verification commands.

Certificate jobs use `CERTIFICATE_ISSUE_TIMEOUT` (default `10m`) and wait 30 seconds for Cloudflare DNS-01 TXT propagation. When Certbot specifically reports `No TXT record found`, the issuer waits another 30 seconds and retries once. Other failures are returned immediately. A control-plane stop or restart marks an in-flight job as failed instead of replaying it immediately, avoiding duplicate ACME requests; selecting **Publish** after the controller is healthy automatically queues a new issuance task. The authenticated APIs `GET /api/sites/{site-id}/certificate-task` and `GET /api/tasks/{task-id}` expose the persisted task state and failure detail.

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

The Compose backup workflow uses SQLite's online backup API and a native ClickHouse backup, then writes the recovery set to encrypted Restic storage. It retries short-lived failures, publishes a machine-readable status consumed by the message center, and sends an SMTP alert after the final failed attempt. It retains 7 daily, 4 weekly, and 6 monthly snapshots. Repository credentials and the daily schedule can be managed from the authenticated Settings view with database-over-environment precedence; offline recovery credentials remain mandatory. The current Restic set includes Nginx artifacts and control secrets, but not managed static-resource object bytes; back up `$CONTROL_DATA_DIR/static-assets/objects` separately. The Settings view can download and validate a selected snapshot into an isolated SQLite/ClickHouse staging area while live traffic continues, then perform a confirmation-protected cutover with a short controller restart and retained rollback data. Offline verification and disaster-recovery procedures remain available in [docs/COMPOSE_DEPLOYMENT.md](docs/COMPOSE_DEPLOYMENT.md).

## Capacity and next limits

The first deployment target is less than 100 requests/second across all sites, with 7 days of raw logs. Use at least 4 vCPU, 8 GiB RAM, and a 160 GiB NVMe control VPS. Increase storage or move ClickHouse to a separate machine before retaining raw logs longer than seven days or consistently approaching 100 RPS.
