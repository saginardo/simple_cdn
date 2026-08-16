# Edge security policies

The **Security** workspace manages four independent controls for HTTP sites:

- an ordered, editable WAF processing chain;
- site-scoped browser proof-of-work (PoW) policies;
- edge-local client-IP rate limits;
- active IPv4 bans and recent security events.

The request path does not depend on the controller. Policy changes are rendered into each capable edge's managed Nginx configuration, validated with `nginx -t`, applied atomically, and rolled back if the new worker generation does not become healthy.

## WAF processing chain

Enabled WAF rules run by ascending priority before rate limiting, static-file serving, redirects, or origin proxying. Rules with the same priority use their stable ID as the tie-breaker. The console can move rules up or down and shows their effective order.

A rule can target every HTTP site or an explicit set of sites. It contains up to eight conditions, all of which must match. Available fields are:

- normalized path, raw URI, and query string;
- method, host, User-Agent, and client IPv4;
- one named request header;
- the first 64 KiB of the request body.

Conditions support validated regular expressions, equality, contains, prefix, suffix, and IPv4 CIDR membership as appropriate for the selected field. String matching can be case-sensitive or insensitive, and every condition can be negated. Regex validation rejects unsafe interpolation and patterns outside the supported Nginx/OpenResty subset before publication.

Actions have explicit chain semantics:

- **Allow** stops WAF evaluation and bypasses PoW for the request. Independent rate limits still apply.
- **Log** records the match and continues with the next WAF rule. Every matching non-allow rule gets its own structured event, including when a later rule blocks or bans the same request.
- **Block** records the match and returns the configured `403`, `404`, or Nginx `444` response without contacting the origin.
- **Ban** blocks the request and emits an IPv4 ban for 1 hour, 6 hours, 12 hours, 24 hours, 3 days, or 7 days.

Six editable built-in rules provide a useful baseline: sensitive-file probes, malicious PHP probes, path traversal, SQL injection, cross-site scripting, and scanner User-Agent detection. Built-in rules can be disabled or edited but cannot be deleted. Custom rules can be added around them, including an early allow rule for a trusted site, path, or client CIDR.

## Browser proof of work

PoW policies require an explicit site selection and may further restrict requests with a path regex. A policy configures:

- difficulty from 16 through 24 leading zero bits;
- challenge lifetime from 30 through 600 seconds;
- pass-cookie lifetime from 300 through 86,400 seconds;
- priority relative to other matching PoW policies.

For a matching HTTPS browser GET, the edge returns a self-contained interstitial page. Browser WebCrypto searches for a nonce, submits it to the reserved verification endpoint, and reloads the original URL after the edge validates the proof. Challenge tokens and pass cookies are HMAC-SHA256 authenticated, time-limited, bound to policy context, and verified entirely at the edge. Each policy's random 32-byte secret is encrypted in SQLite and is sent only through the authenticated desired-state channel.

`/__cdn_health` is always excluded. The PoW flow does not attempt to interpose on plain HTTP, and a challenged non-GET, WebSocket, or gRPC request is rejected instead of being converted into an HTML response. Put PoW on browser entry routes rather than API, streaming, WebSocket, or gRPC endpoints. An earlier matching WAF **Allow** rule intentionally bypasses PoW.

This is a computational browser gate, not a CAPTCHA, browser-attestation system, bot-reputation feed, or volumetric DDoS service. Clients can still automate the work, and expensive traffic should also be controlled upstream or at the provider edge.

## Request and ban flow

1. OpenResty evaluates the WAF chain and PoW policy in the access phase. WAF and rate-limit ban events are written as structured records to `/opt/cdn-edge/logs/security.json`.
2. The Agent reads the log every 500 milliseconds. A ban is written to durable local state before nftables is updated.
3. UUID-keyed events are reported over the Agent's existing mTLS identity. The controller validates the policy, action, public IPv4, site, request metadata, and observation time, then stores each event idempotently.
4. Other edges pull the active global ban revision and reconcile it locally. Manual unban and automatic expiry converge on the next pull.

Local state is stored below `/opt/cdn-edge/data`:

```text
security-bans.json
security-event-queue.json
security-log-offset
```

An edge restart reconstructs its firewall from unexpired local state and the controller. If the control plane is unavailable, a newly detected ban still applies locally and its event remains queued.

## Rate-limit flow

Rate policies use the Nginx client IPv4 as their counting key. Each policy has an independent namespace in a bounded 20 MiB Lua shared dictionary, so all workers on one edge see the same state. The implementation combines the current and previous one-second buckets into an approximate sliding one-second rate. A rejected request receives HTTP 429 with `Retry-After: 1` and `Cache-Control: no-store` before origin proxying.

Without a response condition, attempted requests increment the counter in the access phase. With a response condition, selected 2xx, 3xx, 4xx, or 5xx final statuses increment it in the response-header phase. This means concurrently in-flight requests are not counted until their responses exist. Rate state is intentionally local to each edge rather than coordinated through the controller.

A policy whose response condition contains only 4xx and 5xx may escalate consecutive limiter-generated 429 responses into the normal global IPv4 ban flow. Limiter-generated 429 responses do not increment the underlying 4xx counter. The streak is local to one policy, client IP, and edge; the resulting ban is synchronized across the fleet.

## Firewall ownership

The installer adds Debian's `nftables` package but does not replace `/etc/nftables.conf` or enable an operator firewall policy. The Agent owns only:

```text
table inet simple_cdn
```

Its accept-policy base chain drops managed public IPv4 sources only for TCP ports 80 and 443 and QUIC UDP port 443. SSH, control traffic, custom TCP forwarding ports, other UDP traffic, outbound traffic, and unrelated nftables tables remain untouched. Uninstall removes only project-owned tables.

Private, loopback, link-local, multicast, malformed, and IPv6 addresses are not accepted as ban targets. The ban subsystem remains IPv4-only even when an opted-in site also publishes AAAA records.

## Capability rollout

The modern chain requires `waf_chain_v1`; PoW additionally requires `pow_challenge_v1`. Rate limiting requires `edge_rate_limit_v1`, and synchronized bans require `edge_security_v1`. The controller keeps unsupported policy types out of a node's desired state and blocks assignments or publication where partial enforcement would be unsafe.

Upgrade nodes first, confirm capability coverage in **Security**, then save or deploy policies. A policy change rebuilds capable nodes through the same validation, atomic apply, listener verification, and rollback path used by site changes.

Useful checks on an upgraded edge:

```bash
sudo systemctl is-active cdn-edge-agent nginx
sudo nft list table inet simple_cdn
sudo /opt/cdn-edge/nginx/sbin/nginx -T 2>/dev/null | grep -F simple_cdn_security
sudo /opt/cdn-edge/nginx/sbin/nginx -T 2>/dev/null | grep -F simple_cdn_rate_limit
sudo tail -n 20 /opt/cdn-edge/logs/security.json
sudo journalctl -u cdn-edge-agent --since '-10 minutes' --no-pager
```

Keep public site records in DNS-only/direct mode. The ban source is Nginx `$remote_addr`; placing a shared reverse proxy in front without a separately designed trusted-client-IP boundary can ban the proxy rather than the real client.

## Current limits

- At most 100 WAF policies, 100 PoW policies, and 50 rate policies can exist. A WAF rule has at most eight conditions, and a PoW policy targets between 1 and 100 sites.
- The edge queue retains the latest 10,000 security events. Edge and controller ban state is capped at 50,000 addresses; the controller retains at most 100,000 recent events.
- Request-body matching intentionally inspects only the first 64 KiB and is not a file-upload malware scanner or a full parser for JSON, XML, SQL, or application protocols.
- WAF and PoW apply only to managed HTTP sites, not `stream` TCP forwarding. Ban enforcement is IPv4-only and limited to HTTP/HTTPS ports plus QUIC UDP 443.
- WAF conditions are AND-combined. Rules are ordered; **Log** continues, while **Allow**, **Block**, and **Ban** are terminal. Every enabled rate policy is evaluated independently.
- Saving WAF, PoW, or rate policies rebuilds affected desired states and normally causes a verified Nginx reload.
- Security policies supplement application authentication, authorization, input validation, and upstream DDoS protection; they do not replace them.
