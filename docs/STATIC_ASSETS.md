# Managed static assets

The **Static resources** workspace stores small operator-managed files on the controller and publishes each file at one or more exact site URLs. It is separate from Nginx's origin-response cache: a bound request is served directly from the edge and does not contact an origin.

## Object and binding model

An uploaded object is limited to 32 MiB. The controller computes its SHA-256 digest, stores it in a content-addressed object directory, and deduplicates identical content. The display name can be changed without changing the object or its public bindings.

A binding selects an enabled HTTP site, an exact URL path, and one cache preset:

- 1 hour;
- 1 day;
- 1 year with `immutable`;
- `no-cache` with revalidation.

Paths are normalized clean absolute paths and are limited to 1,024 characters. `/`, `/__cdn_health`, and the reserved `/_cdn/` namespace cannot be assigned. The same path cannot be bound twice on one site, while one object can be reused by multiple sites and paths.

The generated Nginx configuration uses an exact-match location. Only GET and HEAD are allowed. The response includes the stored MIME type, `X-Content-Type-Options: nosniff`, the selected `Cache-Control`, an Nginx ETag, and exact `If-Modified-Since` handling. WAF, PoW, rate limits, normal access logging, TLS, and request IDs still apply before the file is served.

For eligible public text, JavaScript, JSON, XML, SVG, font, WebAssembly, and manifest objects of at least 256 bytes, the Agent creates `.gz`, `.br`, and `.zst` sidecars next to the verified identity object. A sidecar is kept only when it is smaller than the original and decompresses back to the expected SHA-256 content. If the same digest has bindings with incompatible MIME types, no sidecars are exposed for that digest. Nginx selects a sidecar through normal `Accept-Encoding` negotiation; the identity object remains available to clients that do not advertise a supported encoding. `text/event-stream` is never precompressed.

## Edge synchronization

Static resources require the `static_assets_v1` edge capability. Before a binding is accepted, every node assigned to the site must advertise the capability; this prevents a URL from working on only part of the site's DNS pool.

Desired state contains only the digest, size, MIME type, and binding metadata. An authorized edge can download only digests referenced by its own current desired state over its existing mTLS identity. The Agent downloads into a temporary file, verifies both byte length and SHA-256, fsyncs it, atomically renames it into `/opt/cdn-edge/static/objects`, and then creates or repairs eligible compressed sidecars atomically. Existing symbolic links are replaced instead of trusted. The object directory is root-owned and read-only to Nginx workers. A configuration is not applied until all referenced objects and required sidecar checks are valid locally.

After a successful apply, the Agent removes content-addressed objects no longer referenced by that node. The controller removes an object from its own store only after its bindings have been withdrawn and all replacement desired states have been rendered and saved successfully. A render or state-save failure restores the binding metadata instead of leaving a partially removed URL; individual edges remove their local copy only after they successfully apply the new state.

## Operational boundaries

- The controller accepts at most 1,000 stored objects, each no larger than 32 MiB.
- This feature has no directory listing, archive extraction, dynamic image transformation, multipart resumability, range-specific optimization, or automatic source-origin import.
- Renaming changes only management metadata. Replacing bytes means uploading a new object and moving or recreating bindings.
- A control-plane outage does not interrupt objects already synchronized to edges. It prevents uploads, binding changes, and delivery to a newly assigned or rebuilt node.
- Content is public wherever its bound site URL is public. Do not upload secrets or rely on an unguessable path as authorization.

## Backup and restore

The controller stores object metadata and URL bindings in SQLite, but stores the bytes separately below `$CONTROL_DATA_DIR/static-assets/objects`. The current Compose Restic workflow does not archive that object directory. Back it up independently with the same recovery controls as the Restic repository; restoring SQLite without the matching object bytes preserves metadata but leaves the controller unable to redistribute those resources to a rebuilt or newly assigned edge.

Edge copies below `/opt/cdn-edge/static/objects` can keep serving while the control plane is unavailable, but they are not an authoritative replacement for the controller copy. Do not rely on edge garbage-collection state as the only backup.

## Verification

After assigning a resource, wait for the normal publish task to finish, then verify one edge directly:

```bash
curl --resolve static.example.com:443:203.0.113.10 \
  -I https://static.example.com/assets/app.js

curl --resolve static.example.com:443:203.0.113.10 \
  -H 'Accept-Encoding: br' -I https://static.example.com/assets/app.js

sudo find /opt/cdn-edge/static/objects -maxdepth 1 -type f -printf '%f %s bytes\n'
sudo /opt/cdn-edge/nginx/sbin/nginx -T 2>/dev/null | grep -F 'location = "/assets/app.js"'
```

The response should have the configured MIME type and cache policy, and repeated requests should return the same ETag. The second request should return `Content-Encoding: br` when the object is eligible and its Brotli sidecar is smaller. A POST to the same exact path must not serve the object.
