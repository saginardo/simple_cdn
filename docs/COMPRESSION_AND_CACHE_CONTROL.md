# Compression and scoped cache control

HTTP(S) sites support dynamic response compression, managed-static precompression, compression telemetry, full or scoped cache invalidation, and edge-local cache prewarming. These features are capability-gated so a controller can coexist with an older edge during an upgrade, but a newly built managed Nginx bundle provides the complete feature set.

## Compression behavior

Dynamic gzip, Brotli, and Zstandard compression is enabled by default for new sites and for legacy site rows migrated to the current schema. The site editor can disable all dynamic response compression or list up to 32 MIME types to exclude. `text/event-stream` is excluded by default because buffering a long-lived SSE response to compress it increases latency and defeats streaming behavior. `text/html` cannot be excluded because Nginx compression filters handle it independently of their configurable MIME lists.

The dynamic MIME allowlist covers common text, JavaScript, JSON, XML, SVG, font, WebAssembly, feed, and manifest content. MIME exclusions affect dynamic compression only. Eligible managed static assets retain their validated `.gz`, `.br`, and `.zst` sidecars even when dynamic compression is disabled, because serving an existing sidecar consumes no per-request compression CPU. gRPC locations never enable HTTP body compression filters.

The managed Nginx build pins and compiles ngx_brotli and zstd-nginx-module into the binary. The Agent advertises `compression_v1` only when the installed binary and `BUILD.json` prove that both modules are present. Nodes without this capability still receive core gzip directives, but the renderer omits Brotli and Zstandard directives and variables so their configuration remains valid.

## Compression telemetry

Each access event records the normalized `Content-Encoding`, compression ratio, and estimated saved bytes. For a successful, non-range GET of a managed static asset, the original object size is known, so saved bytes are exact. For dynamically compressed responses, the edge derives the original size from the module compression ratio and the transmitted body size, then rounds to bytes. Unsupported, malformed, identity, multi-encoding, or otherwise indeterminate responses report zero saved bytes instead of inventing a value.

ClickHouse stores the raw fields for seven days and maintains 30-day minute aggregates for total compressed responses, gzip/Brotli/Zstandard hits, and bytes saved. The site detail page summarizes the latest 24 hours, while individual log details expose the response encoding, ratio, and saved-byte estimate.

## Cache invalidation

`POST /api/sites/{site_id}/invalidate-cache` accepts this body:

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

## Cache prewarming

An invalidation can create a prewarm job with up to 100 unique absolute request URIs. A full-site operation requires explicit paths because the controller cannot safely enumerate an entire application. A prefix operation can supplement explicit paths from the latest request logs; all selected paths must remain under that prefix. The controller retains the latest 16 jobs per site.

After a desired configuration is valid and Nginx is listening, each assigned `cache_control_v1` edge issues local HTTPS GET requests to `127.0.0.1:443` with the site's real Host and TLS SNI, `Accept-Encoding: identity`, redirect following disabled, and a bounded response/time limit. This warms that edge's own cache without depending on public DNS propagation. Each job is attempted once per edge. A request failure is reported as an apply warning and does not roll back an otherwise healthy site configuration; submitting a new invalidation creates a new retryable job.

The UI exposes the same operation under the site detail page's cache action. Publishing a normal site edit preserves controller-owned invalidation rules and prewarm jobs.
