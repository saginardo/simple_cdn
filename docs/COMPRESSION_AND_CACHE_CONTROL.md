# Compression and scoped cache control

HTTP(S) sites support dynamic response compression, managed-static precompression, compression telemetry, full or scoped cache invalidation, and edge-local cache prewarming. These features are capability-gated so a controller can coexist with an older edge during an upgrade, but a newly built managed Nginx bundle provides the complete feature set.

## Compression behavior

Dynamic gzip, Brotli, and Zstandard compression is enabled by default for new sites and for legacy site rows migrated to the current schema. The site editor can disable all dynamic response compression or list up to 32 MIME types to exclude. `text/event-stream` is excluded by default because buffering a long-lived SSE response to compress it increases latency and defeats streaming behavior. `text/html` cannot be excluded because Nginx compression filters handle it independently of their configurable MIME lists.

The dynamic MIME allowlist covers common text, JavaScript, JSON, XML, SVG, font, WebAssembly, feed, and manifest content. MIME exclusions affect dynamic compression only. Eligible managed static assets retain their validated `.gz`, `.br`, and `.zst` sidecars even when dynamic compression is disabled, because serving an existing sidecar consumes no per-request compression CPU. gRPC locations never enable HTTP body compression filters.

The managed Nginx build pins and compiles ngx_brotli and zstd-nginx-module into the binary. The Agent advertises `compression_v1` only when the installed binary and `BUILD.json` prove that both modules are present. Nodes without this capability still receive core gzip directives, but the renderer omits Brotli and Zstandard directives and variables so their configuration remains valid.

## Compression telemetry

Each access event records the normalized `Content-Encoding`, compression ratio, and estimated saved bytes. For a successful, non-range GET of a managed static asset, the original object size is known, so saved bytes are exact. For dynamically compressed responses, the edge derives the original size from the module compression ratio and the transmitted body size, then rounds to bytes. Unsupported, malformed, identity, multi-encoding, or otherwise indeterminate responses report zero saved bytes instead of inventing a value.

ClickHouse stores the raw fields for seven days and maintains 30-day minute aggregates for total compressed responses, gzip/Brotli/Zstandard hits, and bytes saved. The site detail page summarizes the latest 24 hours, while individual log details expose the response encoding, ratio, and saved-byte estimate.

## Cache operations workspace

The left navigation exposes a dedicated **Cache** workspace instead of placing operational controls inside the site editor. The workspace provides three views:

- **Operation history** lists cache invalidations and prewarm retries across all sites, with site, status, scope, target, cache generation, node progress, and submission time filters.
- **Site caches** summarizes cache eligibility, current generation, scoped-rule count, assigned and reporting nodes, and the latest operation. The site detail page links here with its site filter preselected.
- **Active rules** shows the exact-URL and path-prefix generations currently contributing to cache keys.

Creating an operation supports an exact URL, a path prefix, or an entire site. Full-site invalidation requires typing the site name because it advances the site's base cache generation. The UI states explicitly that old cache files are reclaimed by normal eviction rather than removed synchronously.

## Cache invalidation API

The primary operation endpoint is `POST /api/cache/operations`:

```json
{
  "site_id": "site-1",
  "scope": "url",
  "value": "/assets/app.js?v=2",
  "prewarm": true,
  "prewarm_paths": []
}
```

Related endpoints are:

- `GET /api/cache/overview`: site eligibility, recent operations, and active scoped rules for the workspace.
- `GET /api/cache/operations?site_id={site_id}&limit=200`: persisted operation history.
- `GET /api/cache/operations/{operation_id}`: one operation and its per-node rollout and prewarm results.
- `POST /api/cache/operations/{operation_id}/retry`: create a prewarm-only retry using the original URL set. This publishes a new desired-state version but does not advance the full-site or scoped cache generation.

`POST /api/sites/{site_id}/invalidate-cache` remains available for backward compatibility and accepts the earlier body without `site_id`:

```json
{
  "scope": "url",
  "value": "/assets/app.js?v=2",
  "prewarm": true,
  "prewarm_paths": []
}
```

Supported scopes are:

- `full`: increments the site's base cache generation. `value` must be empty. An empty request body remains backward compatible and performs a full invalidation.
- `url`: assigns a new generation to one exact request URI, including its query string. When prewarming is enabled, that URL is included automatically.
- `prefix`: assigns a new generation to a clean path prefix other than `/`; query strings are not accepted in the prefix. Recent successful GET/HEAD log paths below that prefix are added to explicit prewarm paths, up to the job limit.

The renderer includes the selected generation in the Nginx cache key. New requests therefore miss old entries immediately, without depending on an unsupported cache-purge module. Old files remain on disk until normal `inactive` or `max_size` eviction; invalidation is not synchronous disk reclamation. A site retains at most 128 scoped rules. When the limit is reached, the controller compacts them by advancing the full-site generation before accepting new scoped operations.

## Operation history and node results

Cache operations and their target-node snapshots are stored in SQLite. The operation status is derived from the configuration rollout and, when requested, the warmup outcome: `queued`, `applying`, `succeeded`, `partial`, or `failed`. Each node records its configuration status separately from its prewarm status, along with attempted and successful URL counts, bounded failure details, and the report timestamp.

New Agents advertise `cache_warmup_results_v1` and report structured results after processing a job. Nodes running an older Agent remain visible as `unreported`, while nodes that cannot receive cache control are shown as `unsupported` or `not_targeted` rather than being counted as successful. This keeps mixed-version upgrades observable without blocking compatible nodes.

## Cache prewarming

An invalidation can create a prewarm job with up to 100 unique absolute request URIs. A full-site operation requires explicit paths because the controller cannot safely enumerate an entire application. A prefix operation can supplement explicit paths from the latest request logs; all selected paths must remain under that prefix. The controller retains the latest 16 jobs per site.

After a desired configuration is valid and Nginx is listening, each assigned `cache_control_v1` edge issues local HTTPS GET requests to `127.0.0.1:443` with the site's real Host and TLS SNI, `Accept-Encoding: identity`, redirect following disabled, and a bounded response/time limit. This warms that edge's own cache without depending on public DNS propagation. Each job is attempted once per edge. Individual URL failures do not stop the remaining URLs and do not roll back an otherwise healthy site configuration.

The Agent persists completed structured results in its data directory and retries heartbeat delivery until the controller acknowledges them, so a controller outage or Agent restart does not lose the result. The workspace can then retry the same prewarm URL set without performing another invalidation. Publishing a normal site edit preserves controller-owned invalidation rules, jobs, and operation history.
