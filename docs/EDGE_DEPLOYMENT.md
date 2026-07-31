# Edge deployment and managed Nginx

Edge nodes run directly on Debian 12 or Debian 13. Docker is not required on an edge. The controller distributes the edge Agent and a project-built Nginx 1.30.4 bundle; both Debian releases therefore run the same Nginx feature set, including HTTP/2, HTTP/3, stream, and Lua support.

The installer removes Debian's Nginx packages after recording their exact versions and configuration for transaction rollback. A successful layout version 2 installation does not use `/usr/sbin/nginx` or `/etc/nginx`.

## Managed layout

All edge-owned software, configuration, runtime data, logs, and cache live below one root:

```text
/opt/cdn-edge/
  .layout-version                  # currently 2
  bin/cdn-edge-agent
  nginx/
    sbin/nginx
    conf/nginx.conf
    conf/mime.types
    lib/libluajit-5.1.so.2
    lib/lua/5.1/                   # lua-resty-core and lua-resty-lrucache
    run/nginx.pid
    tmp/{body,fastcgi,proxy,scgi,uwsgi}/
    VERSION
    BUILD.json
    .bundle-sha256
  config/
    edge.env
    certs/
    nginx/
      cdn-platform.conf
      cdn-platform-stream.conf
      cdn-platform-main.conf
      cdn-platform-events.conf
      fragments/
      origin-pools/
  data/
    edge-client.key
    edge-client.crt
    edge-ca.crt
    applied-version
    access-log-offset
    access-log-cursor.json
    access-log-queue/
    active-upgrade-task
    upgrades/
    security-bans.json
    security-event-queue.json
    origin-connections.json
  logs/
    access.json
    security.json
    nginx-error.log
    tcp-access.json
    tcp-error.log
  cache/
  systemd/
    nginx.service
    cdn-edge-agent.service
    cdn-edge-updater@.service
```

Only host integration points remain outside that root:

```text
/etc/systemd/system/nginx.service -> /opt/cdn-edge/systemd/nginx.service
/etc/systemd/system/cdn-edge-agent.service -> /opt/cdn-edge/systemd/cdn-edge-agent.service
/etc/systemd/system/cdn-edge-updater@.service -> /opt/cdn-edge/systemd/cdn-edge-updater@.service
/etc/logrotate.d/cdn-edge-platform
/usr/local/lib/sysctl.d/40-simple-cdn-edge.conf
```

The `nginx.service` name is retained for normal systemd operations, but its `ExecStart`, reload, PID, configuration, temp, library, and log paths all point into `/opt/cdn-edge`. Nginx starts with a root master and `www-data` workers. The private OpenResty LuaJIT library is loaded through the binary's fixed `/opt/cdn-edge/nginx/lib` runtime path; no Debian Lua Nginx module is installed.

The Agent runs as root because it atomically writes certificates and generated configuration, validates and reloads Nginx, and owns the isolated `inet simple_cdn` nftables table. Nginx temp and cache directories are writable by `www-data`. Logrotate handles all HTTP, security, Nginx error, and TCP stream logs under `/opt/cdn-edge/logs`.

## Reproducible Nginx build

`deploy/nginx/VERSION` pins Nginx 1.30.4. The Docker build pins SHA-256 values for Nginx, NDK, lua-nginx-module, lua-resty-core, lua-resty-lrucache, and OpenResty LuaJIT. It builds serially (`make -j1`) to stay within a 2-core, 4 GiB builder and creates a deterministic `tar.gz` with normalized ordering, ownership, timestamps, and gzip metadata.

Build only the Nginx artifact:

```bash
mkdir -p dist-nginx
docker build --target nginx-artifact \
  --output type=local,dest=dist-nginx .
sha256sum dist-nginx/cdn-nginx-linux-amd64.tar.gz
```

Build all release artifacts:

```bash
./scripts/build-release.sh dist
```

The release directory contains the controller, edge Agent, Nginx bundle, and one `SHA256SUMS` file. The full controller image also embeds the same Nginx bundle at `/usr/local/lib/cdn-platform/cdn-nginx-linux-amd64.tar.gz`.

Run the artifact and migration checks after changing Nginx, its modules, or the installer:

```bash
./scripts/test-nginx-artifact.sh dist/cdn-nginx-linux-amd64.tar.gz
./scripts/test-edge-installer-debian.sh dist/cdn-nginx-linux-amd64.tar.gz
```

Both scripts exercise Debian 12 and Debian 13. The first loads the private LuaJIT, starts the real binary, and executes Lua. The second installs Debian Nginx first, verifies successful replacement by the managed build, then forces an installation failure and checks that the exact Debian packages and original `/etc/nginx` contents are restored.

## Fresh installation and migration

Use **获取部署/升级命令** on the node page and run the generated command as root on the target Debian node. The controller renders immutable HTTPS URLs and SHA-256 digests for the Agent binary, Nginx bundle, and all three systemd units.

Before mutating the host, the installer:

1. Downloads every artifact with HTTPS and verifies its SHA-256 digest.
2. Rejects an oversized or malformed Nginx archive, unsafe/duplicate paths, links and special files, missing required files, a version mismatch, or a non-AMD64 manifest.
3. Validates that each systemd unit points only to the managed `/opt/cdn-edge` layout.
4. Records service state, the complete `/etc/nginx` tree, exact installed Debian Nginx package versions, system integration files, and the previous managed binary/bundle.

It then installs the small runtime dependency set, stops the old services, purges Debian packages named `nginx*` and `libnginx-mod-*`, installs the managed bundle, validates the real configuration, starts Nginx and the Agent, waits for the mTLS identity, and checks the local health endpoint. `/opt/cdn-edge/.layout-version` is written only after those checks pass.

The installer recognizes these states:

- Fresh host: requires a 15-minute enrollment token and creates layout version 2.
- Legacy scattered layout: migrates the Agent, identity, certificates, generated Nginx configuration, queued access logs, and retained logs from `/usr/local/bin/cdn-edge-agent`, `/etc/cdn-platform`, and `/var/{lib,log,cache}/cdn-platform`.
- Layout version 1: upgrades the existing `/opt/cdn-edge` data and replaces Debian Nginx with the managed bundle.
- Layout version 2: atomically upgrades both the Agent and Nginx while preserving identity, configuration, data, logs, and cache.

An unmanaged `/etc/nginx` tree on a fresh host is never deleted: if no matching Debian package owns the installation, the installer stops and requires operator review. Mixed layout version 1 and legacy paths are also rejected. A valid layout version 2 marker is authoritative; stale legacy paths are removed only after the new services are healthy, and a cleanup failure leaves the working v2 installation intact for a safe retry.

For an existing node with a complete local mTLS identity, the generated command contains no new enrollment token. A successful install clears the one-time token from `edge.env`.

## HTTP/3 and QUIC

The managed bundle includes `--with-http_v3_module` on both supported Debian releases, so the Agent advertises `http3_v1` after inspecting the installed binary. HTTP/3 remains disabled by default for every site. Enabling and publishing it for a site adds UDP 443 QUIC while retaining TCP 443 HTTP/1.1 and HTTP/2 fallback.

The installer creates `/opt/cdn-edge/config/nginx/quic-host.key` once with mode `0600` and preserves it across online upgrades and rollbacks. This keeps QUIC address-validation tokens valid across Nginx reloads. The generated main configuration enables PCRE JIT and gives old workers up to one hour to drain long-lived requests. TLS session state uses a shared cache with a 30-minute timeout.

Nginx exposes `stub_status` only on `/opt/cdn-edge/nginx/run/status.sock`; no TCP management port is opened. The Agent samples that socket with a sub-second timeout and includes the optional snapshot in the existing real-time machine report. A missing socket or an Nginx restart leaves the runtime section empty without blocking host status or heartbeat reporting.

Allow UDP 443 in both the host firewall and provider security group. Verify from another HTTP/3-capable host:

```bash
nginx=/opt/cdn-edge/nginx/sbin/nginx
domain=cdn.example.com
edge_ip=203.0.113.20

sudo "$nginx" -V 2>&1 | grep -F -- '--with-http_v3_module'
sudo grep -F http3_v1 /opt/cdn-edge/config/edge.env
sudo "$nginx" -T 2>/dev/null | grep -E 'listen 443 quic|Alt-Svc|quic_retry'
sudo ss -H -lunp '( sport = :443 )'

curl --http3-only --fail --silent --show-error \
  --resolve "$domain:443:$edge_ip" \
  --write-out '\nHTTP %{http_version}\n' \
  "https://$domain/"
```

The controller deliberately qualifies public health over TCP HTTPS fallback, so that check does not prove that an external UDP firewall passes QUIC.

## Online upgrades

Layout version 2 nodes advertise `nginx_bundle_v1` and report both the running Agent digest and installed Nginx bundle digest in authenticated heartbeats. The controller compares both values with its current artifacts. An Agent-only hash match does not make the node current when its Nginx bundle is old.

An online upgrade performs these steps:

1. The controller snapshots the Agent, installer, three unit files, Nginx bundle, versions, URLs, and SHA-256 digests into the node task.
2. The Agent stages all artifacts below `/opt/cdn-edge/data/upgrades/<task-id>`, applies size limits, and verifies every digest before starting the detached updater.
3. The updater runs the same transactional installer used by manual deployment.
4. The new Agent must complete an authenticated heartbeat reporting the target Agent and Nginx digests before the task can succeed.
5. Any package, validation, service, identity, health, or readiness failure restores the previous managed bundle, Agent, units, Debian packages when they were removed in this transaction, and prior service state.

Nodes that predate `nginx_bundle_v1` need one final generated manual deployment command. Only one online upgrade may be active per node; site publication, site deletion, and node uninstall remain blocked for that node until it completes.

## Operational verification

After installation or migration, verify the authoritative paths and absence of Debian Nginx:

```bash
nginx=/opt/cdn-edge/nginx/sbin/nginx

sudo systemctl is-active nginx.service cdn-edge-agent.service
sudo systemctl cat nginx.service
sudo readlink -f /etc/systemd/system/nginx.service
sudo "$nginx" -t
sudo "$nginx" -V
sudo ldd "$nginx" | grep -F /opt/cdn-edge/nginx/lib/libluajit-5.1.so.2
test ! -e /etc/nginx
test -z "$(dpkg-query -W -f='${binary:Package}\n' 2>/dev/null | awk '$1 ~ /^nginx($|-)/ || $1 ~ /^libnginx-mod-/')"
curl -fsS http://127.0.0.1/__cdn_health
sudo find /opt/cdn-edge -maxdepth 3 -type f -o -type l
```

For a TCP-only node, verify its published listener ports instead of the HTTP health endpoint. Detailed reload, worker-generation, HTTPS/SNI, and QUIC checks are in [NGINX_APPLY_SAFETY.md](NGINX_APPLY_SAFETY.md).

The access-log forwarder durably segments HTTP records below `data/access-log-queue` and advances its cursor only after acknowledgement. Machine status and origin-pool probe state use the reusable mTLS HTTP/2 transport and remain real-time only on the controller. Cache data is shared below `/opt/cdn-edge/cache`; origin-pool runtime state is below `/opt/cdn-edge/data/origin-connections.json`.

## Failure and rollback

The installation transaction keeps the previous managed Nginx directory until the replacement is healthy. When Debian Nginx is removed during the same attempt, rollback first reinstalls the exact recorded package versions (falling back to the currently available versions only if an exact version is unavailable), restores the full `/etc/nginx` tree, restores service enable/active state, and reapplies the pre-install sysctl values.

For a fresh node, a failure after enrollment can consume its one-time token even though local files are rolled back. Generate a new command before retrying. Do not manually write `.layout-version` or combine a partial v1 layout with legacy paths.

## Uninstall and backup boundary

The control-plane **卸载节点** command is bound to the selected node's name, UUID, and public IPv4. Before contacting the control plane or changing the host, the script reads from `/dev/tty` and requires the exact `UNINSTALL <node UUID>` phrase. A missing terminal, empty input, or mismatch exits without consuming the workflow token or changing services and files. After confirmation, the workflow stops the managed Agent and Nginx, removes the three project-owned systemd links, restores the pre-install sysctl baseline, and deletes `/opt/cdn-edge` plus known legacy project paths. A layout version 2 uninstall does not reinstall Debian Nginx or recreate `/etc/nginx`; install an operator-selected web server separately if the retired host needs one. The layout version 1 compatibility path preserves an external Debian Nginx installation and removes only the old project includes.

Back up these non-recreatable paths:

- `/opt/cdn-edge/config`: Agent settings and site TLS certificates.
- `/opt/cdn-edge/data`: node mTLS identity, applied version, pending log delivery state, and local runtime state.

`logs` is optional when uploaded logs are already retained by the controller. `bin`, `nginx`, `systemd`, generated Nginx fragments, and `cache` are checksum-verified or recreatable and are normally excluded.
