# Edge security policies

The **Security** workspace manages global HTTP access policies, client-IP rate policies, and active edge bans. Access policies are ordered by ascending priority and match Nginx's normalized `$uri` value before a request is redirected or proxied. The supported expression language is the RE2-compatible subset of PCRE plus non-capturing groups. Additional validation rejects Nginx variable interpolation, high-backtracking repeated groups, and overly complex expressions before a policy can reach an edge.

Two built-in policies are enabled by default. **Sensitive file scanning** detects segment-bounded probes for environment files, repository metadata, cloud and developer credentials, shell history, private keys, Terraform state, and `wp-config.php`; it uses the **IP ban** action for six hours. **PHP malicious file probing** blocks high-risk diagnostic and web-shell file names such as `phpinfo`, `shell`, `webshell`, `cmd`, `c99`, `r57`, and `wso`, while deliberately excluding normal entry points such as `index.php`, `api.php`, `admin.php`, and `config.php`. The PHP policy defaults to request-only blocking. Built-in policies can be edited or disabled but not deleted. Additional policies can either reject only the matching request or reject it and ban the source IPv4 for 1, 6, 12, or 24 hours.

## Request and ban flow

1. The controller renders enabled policies into an Nginx `map` only for nodes advertising `edge_security_v1`.
2. A match writes a structured event to `/opt/cdn-edge/logs/security.json` and closes the request with Nginx status 444 before origin proxying.
3. The Agent reads the security log every 500 milliseconds. A ban action is written to durable local state before the Agent updates nftables.
4. The Agent reports UUID-keyed events over its existing mTLS identity. The controller validates the policy, action, public IPv4, normalized path, and timestamp, and records each event idempotently. An event delayed while the controller is unavailable receives only the ban time remaining from its original observation.
5. Other edge agents pull the active global ban set every 30 seconds. Manual unban and automatic expiry are reconciled on the next pull.

Local state is stored below `/opt/cdn-edge/data`:

```text
security-bans.json
security-event-queue.json
security-log-offset
```

An edge restart reconstructs the firewall from unexpired local state and the controller. If the control plane is unavailable, a newly detected ban still applies locally and its event remains queued.

## Rate-limit flow

Rate policies use `client_ip` as their first-version counting key. Each policy has an independent counter namespace, while the client value comes from Nginx `$remote_addr`. Counters live in a bounded 20 MiB Nginx Lua shared dictionary, so every Nginx worker on one edge observes the same state. The implementation combines the current and previous one-second buckets into an approximate sliding one-second rate. Exceeding the configured requests-per-second threshold returns HTTP 429 with `Retry-After: 1` and `Cache-Control: no-store` before origin proxying.

Without a response condition, an attempted request is counted in the access phase. When the response condition is enabled, one or more of 2xx, 3xx, 4xx, and 5xx can be selected; the access phase checks the existing rate, and only a completed response whose final HTTP status belongs to a selected class increments the counter in the response-header phase. This deliberately means concurrently in-flight requests are not counted before their responses exist. Rate state is local to each edge node rather than coordinated across the fleet, avoiding a control-plane or network dependency in the request path.

Policies whose response condition contains only 4xx and 5xx may optionally escalate repeated rate rejections into an IP ban. The client still receives HTTP 429 for each rejected request. A separate per-policy and per-client streak counts consecutive limiter-generated 429 responses, resets when that policy admits a request or after an idle interval, and emits one ban event when the configured count is reached. Limiter-generated 429 responses are excluded from the underlying 4xx response counter, so only the original 4xx/5xx traffic establishes the rate breach. The default escalation is three consecutive 429 responses followed by a one-hour ban; both the consecutive count and the existing 1-hour through 7-day ban durations are configurable in the Security workspace.

The ban event uses the same durable edge path as malicious-request bans. The Agent records local state before updating nftables, reports the event to the controller, and other edges receive the global ban on their next reconciliation. One shared-dictionary marker suppresses duplicate events for the configured ban duration. Nodes without both `edge_rate_limit_v1` and `edge_security_v1` continue to enforce HTTP 429 but do not receive the escalation settings.

The managed Nginx bundle includes lua-nginx-module, lua-resty-core, lua-resty-lrucache, and a private OpenResty LuaJIT under `/opt/cdn-edge/nginx`; it does not install Debian's Lua module. Only nodes advertising `edge_rate_limit_v1` receive rate-limit configuration. A policy save rebuilds desired states for capable nodes through the same managed Nginx validation, atomic apply, listener verification, and rollback path used by site and access-policy changes.

## Firewall ownership

The installer adds Debian's `nftables` package but does not replace `/etc/nftables.conf` or enable an operator firewall policy. The Agent owns only this runtime object:

```text
table inet simple_cdn
```

Its base chain has an accept policy and adds one restriction: source IPv4 addresses in the managed timeout set are dropped for TCP ports 80 and 443 and QUIC UDP port 443. SSH, control traffic, custom TCP forwarding ports, other UDP traffic, outbound traffic, and every other nftables table remain untouched. On first apply after an upgrade, the Agent removes its legacy `table inet cdn_platform` before creating the current table. Uninstall removes either project-owned table and leaves all unrelated tables untouched.

Only public IPv4 addresses are accepted as ban targets. Private, loopback, link-local, multicast, malformed, and IPv6 addresses are ignored. This matches the platform's current A-record and `public_ipv4` deployment model.

## Rollout

Existing agents do not receive malicious-path configuration until they advertise `edge_security_v1`, and they do not receive rate-limit configuration until they advertise `edge_rate_limit_v1`. Upgrade one node at a time, verify both capability columns in **Security**, then select **Deploy policies**. Nodes without a required capability retain compatible Nginx configuration without the unsupported policy type.

Use these checks on an upgraded edge:

```bash
sudo systemctl is-active cdn-edge-agent nginx
sudo nft list table inet simple_cdn
sudo /opt/cdn-edge/nginx/sbin/nginx -T 2>/dev/null | grep -F cdn_security_policy_id
sudo /opt/cdn-edge/nginx/sbin/nginx -T 2>/dev/null | grep -F cdn_rate_limit
sudo tail -n 20 /opt/cdn-edge/logs/security.json
sudo journalctl -u cdn-edge-agent --since '-10 minutes' --no-pager
```

Keep public site DNS records in DNS-only/direct mode. The ban source is Nginx `$remote_addr`; putting a shared reverse proxy in front without a separately designed trusted-client-IP boundary would ban that proxy instead of the originating client.

## Current limits

- Policies are global rather than scoped to individual sites. Rate counters are per policy, client IP, and edge node; they are not a fleet-wide aggregate.
- At most 100 access policies and 50 rate policies can exist at once. The edge queue retains the latest 10,000 access-policy events and both the edge and controller cap active ban state at 50,000 addresses.
- Matching covers normalized request paths, not query strings, headers, request bodies, or TCP forwarding traffic.
- Ban enforcement is IPv4-only and scoped to HTTP/HTTPS traffic on TCP 80/443 and QUIC UDP 443.
- The first matching path policy wins; every enabled rate policy is evaluated independently. Saving either policy type rebuilds capable node desired states and therefore causes a normal verified Nginx reload. A rate-ban streak is local to one policy, client IP, and edge node; the resulting IP ban is synchronized globally.
- Rate conditions use final HTTP status classes, not response bodies, gRPC trailer status, or application-specific error fields. Security policies do not replace application authentication or a dedicated upstream DDoS service.
- The controller retains at most 100,000 recent events and 50,000 simultaneous global bans; oldest entries are evicted first when these safety limits are reached. The console shows the 500 bans with the latest expiry while reporting the full active count.
