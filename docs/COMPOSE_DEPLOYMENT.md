# Control-plane Docker Compose deployment

Docker Compose is the supported control-plane deployment. Edge nodes use the host Nginx package and a systemd agent, with CDN-owned files consolidated below `/opt/cdn-edge`; see [EDGE_DEPLOYMENT.md](EDGE_DEPLOYMENT.md).

[`deploy/docker-compose.yaml`](../deploy/docker-compose.yaml) is the only tracked Compose definition. The installer copies it byte-for-byte to the deployment root as `compose.yaml`; production `.env`, `config/`, `data/`, `logs/`, and `backup/` content remain host-local and must never be committed.

The software, repository, Compose project, and default ClickHouse database are named `simple_cdn`. The deployment script recognizes the legacy `cdn-platform` Compose project and `cdn_platform` database, then migrates both during a guarded cutover without deleting bind-mounted data. A failed rollout reverses the database rename before restoring the old controller. Established container and edge filesystem paths retain their existing names because changing those persistent locations would break rollback or orphan state.

## Persistent layout

The installer creates one operational and backup boundary:

```text
/opt/cdn-platform/
  app/                         Compose support scripts and ClickHouse config
  compose.yaml
  .env                         pinned GHCR control image and support path
  config/
    control.env                bootstrap secrets and runtime fallbacks
    backup.env                 restic and backup schedule settings
    restic-password            restic repository password
  data/
    control/                   SQLite, internal CA, site Certbot state
    control-tls/               direct control certificate and renewal state
    clickhouse/                ClickHouse data
  logs/
    certbot-sites/
    certbot-control/
    clickhouse/
  backup/
    staging/clickhouse/        native ClickHouse backup disk
    status/backup.json         atomic scheduler status for UI/messages
    online-restore/            staged online restore jobs and locks
```

The shared host-network Caddy installation stays outside this directory. It proxies `control.example.com` from public port 443 to `https://127.0.0.1:${CONTROL_MTLS_PORT}`. The control container terminates TLS itself so edge mTLS remains end-to-end.

## GitHub Actions delivery

`.github/workflows/ci-cd.yml` owns compilation, tests, version resolution, and image construction. Pull requests run frontend checks, Go tests/vet, browser smoke tests, Compose validation, and an unpushed Linux AMD64 image build. A successful `main` build embeds `0.0.0-dev+<commit>` and publishes both `main` and `sha-<commit>` tags to `ghcr.io/saginardo/simple_cdn`. A pushed `vMAJOR.MINOR.PATCH` tag is the sole source of a stable release version and publishes only that matching image tag, so it cannot overwrite the development image for the same commit. Every published image includes provenance and SBOM attestations, and its immutable digest is recorded in the job summary. The workflow deliberately contains no production host, credential, SSH, or rollout configuration and never connects to a production environment.

Create a stable release from the tested commit without editing any version file:

```bash
git tag -a v0.1.8 -m "simple_cdn v0.1.8"
git push origin v0.1.8
```

The workflow rejects tags that do not begin with `v` followed by a semantic `MAJOR.MINOR.PATCH` version. Docker-compatible pre-release suffixes such as `-rc.1` are accepted; `+build` metadata is rejected because it cannot be preserved in a Docker tag. Untagged local release builds use a development version derived from the commit and append `.dirty` when the worktree has changes; a clean checkout exactly at a release tag resolves to that stable version.

Keep production deployment wiring in private infrastructure configuration outside this repository. That automation should pass the published `@sha256:<digest>` reference to `scripts/deploy-control-compose.sh`. The host then pulls the exact image, updates only `compose.yaml`, `.env`, and `app/`, recreates the running control/certificate/backup containers without building, checks the control health endpoint and running image ID, and restores the previous definition and image if validation fails. After a successful cutover it removes unused images belonging to this project, while leaving every other Docker repository untouched. Persistent `config/`, `data/`, `logs/`, and `backup/` content is outside this replacement boundary.

The repository is public, so [standard GitHub-hosted runners do not consume paid Actions minutes](https://docs.github.com/en/billing/concepts/product-billing/github-actions). [GHCR container-image storage and bandwidth are currently free](https://docs.github.com/en/billing/concepts/product-billing/github-packages) under GitHub's published billing policy. A [newly created personal-account package is private by default](https://docs.github.com/en/packages/learn-github-packages/configuring-a-packages-access-control-and-visibility); Actions can publish it with `GITHUB_TOKEN`, while private deployment automation must authenticate pulls separately. Change the package to Public in GitHub package settings only if administrators also need anonymous manual pulls. Otherwise authenticate manual pulls with a classic PAT scoped to `read:packages`.

## Fresh installation

Run from a trusted repository checkout on a Debian 12 host with Docker Engine and Docker Compose:

```bash
sudo ./scripts/install-control-compose.sh /opt/cdn-platform
sudoedit /opt/cdn-platform/config/control.env
cd /opt/cdn-platform
sudo docker compose config --quiet
sudo docker compose pull
sudo docker compose run --rm --no-deps control keygen
```

Put the generated key in `CONTROL_ENCRYPTION_KEY`. Set `CONTROL_TLS_DOMAIN`, `CONTROL_PUBLIC_URL`, `EDGE_CONTROL_URL`, the scoped Cloudflare token, and ACME email. Because backup is a required service, also populate `config/backup.env` and `config/restic-password` before starting the stack. Then start it normally:

```bash
sudo docker compose up -d
sudo docker compose ps
curl -fsS https://control.example.com/healthz
```

The default image is `ghcr.io/saginardo/simple_cdn:main`. For a repeatable installation, pass a `sha-<commit>` tag or `@sha256:<digest>` as the installer's second argument. The control image contains the exact edge-agent binary it serves. The controller calculates its SHA-256 from `EDGE_BINARY_PATH` at startup and uses that digest for enrollment and online-upgrade verification; no separate checksum setting is required.

The authenticated **Settings** view stores runtime overrides in SQLite. Cloudflare Token, SMTP password, S3 secret access key, and Restic repository password values are encrypted with `CONTROL_ENCRYPTION_KEY`; API responses never return them. Database overrides take precedence over their environment fallbacks, while reset actions restore those fallbacks. Retain the environment Cloudflare token because a fresh installation needs it before the UI and database exist. Control-certificate bootstrap and renewal containers mount the control database read-only and refresh their temporary Certbot credentials before each certificate operation.

When a release changes generated Nginx paths, deploy the new controller without publishing site changes, migrate every legacy edge using its generated deployment/upgrade command, and only then run `docker compose run --rm --no-deps control publish-all`. Existing desired state is retained across the controller restart, so this order keeps legacy nodes on their last working configuration during migration. Cache quotas are node-scoped and are not capability-gated: republishing converts any older per-site cache layout to the single shared node cache and applies that node's effective total quota.

To rebuild only one site's affected nodes after a renderer fix, use `docker compose run --rm --no-deps control publish-site <site-id>`. This preserves unrelated node versions and avoids a fleet-wide Nginx reload.

## Backup

The backup container uses SQLite's online backup API and a native ClickHouse `BACKUP DATABASE` operation. It does not copy either live database directory. `config/backup.env` and `config/restic-password` are bootstrap and fallback settings. After the controller is running, the authenticated **Settings > S3 backup** form can override the repository URL, S3 credentials, region, Restic password, daily time, and random delay. The scheduler reloads these effective settings at least once per minute, so saving or resetting the form does not require a container restart.

The web override is stored inside the control database, so it cannot be the only recovery copy of the credentials needed to open that database's Restic snapshot. Always retain the repository coordinates, S3 credentials, `CONTROL_ENCRYPTION_KEY`, and Restic password in a separate offline recovery record. Keep working fallback values in `config/backup.env` and `config/restic-password` when practical.

Initialize a new Restic repository once before its first scheduled run:

```bash
cd /opt/cdn-platform
sudo docker compose run --rm --entrypoint \
  /usr/local/lib/cdn-platform/compose-backup-restic.sh backup init
```

The wrapper resolves the database override first and falls back to `config/backup.env`. Saving settings does not initialize or migrate a repository. Initialize each new repository once, then use **Validate repository** in the web form. Other manual Restic operations must use the same wrapper, for example:

```bash
sudo docker compose run --rm --entrypoint \
  /usr/local/lib/cdn-platform/compose-backup-restic.sh backup \
  snapshots --tag cdn-control-compose
```

The scheduler is a required service and starts with the rest of the stack. After these values are complete, start or reconcile the stack and run an immediate backup:

```bash
cd /opt/cdn-platform
sudo docker compose up -d
sudo docker compose run --rm --entrypoint \
  /usr/local/lib/cdn-platform/compose-backup-run.sh backup
```

The default schedule is 03:25 Asia/Shanghai with up to 20 minutes of random delay. A scheduled or manual wrapper run takes a backup-only lock, then makes up to `BACKUP_MAX_ATTEMPTS` attempts (default 3), using `BACKUP_RETRY_DELAYS_SECONDS` (default `30,120`) between failures. It atomically updates `backup/status/backup.json`; the Settings view and message center expose running, retrying, successful, skipped-during-restore, and final-failure states. A final failure also sends an SMTP alert through the effective database-or-environment SMTP profile. A restore-maintenance skip is not retried or alerted. If snapshot creation succeeds but `forget --prune` fails, retries run only the retention phase so one scheduled run cannot create duplicate snapshots. Repository validation and online-restore snapshot listing use lock-free reads, and cancelled Restic subprocesses receive an interrupt grace period to remove any lock held by a stateful operation. The scheduler remains alive after a failed or skipped run and evaluates the next scheduled time.

Retention is 7 daily, 4 weekly, and 6 monthly snapshots. The backup includes the `simple_cdn` ClickHouse database, not recreatable `system` diagnostic tables, plus `compose.yaml` and `.env` so the exact GHCR image reference is recorded. Restore accepts legacy snapshots containing `cdn_platform` and promotes them under the current name. Backup and certificate containers take a shared operation lock, while a restore cutover takes the exclusive lock and publishes a maintenance marker so no new writer operation starts during the swap. After an image upgrade, ordinary `docker compose up -d --no-build` includes the required scheduler; the deployment script also recreates and verifies it automatically.

## Restore

On a replacement host, install the deployment support files and the same immutable GHCR image reference recorded in the snapshot or offline recovery record. Populate `config/backup.env`, `config/restic-password`, and `CONTROL_ENCRYPTION_KEY` from the offline recovery record. Disaster recovery intentionally uses these environment credentials because the web override is inside the snapshot being restored. Do not initialize the control plane first. Then run:

```bash
sudo SIMPLE_CDN_ROOT=/opt/cdn-platform \
  ./app/scripts/restore-control-compose.sh latest
```

Before a cutover, run the same recovery as an isolated drill:

```bash
sudo SIMPLE_CDN_ROOT=/opt/cdn-platform \
  ./app/scripts/restore-control-compose.sh --verify-only latest
```

Both modes download while the live controller remains online, reject unsafe archives, run SQLite `quick_check`, require migration history, restore the native ClickHouse backup into a uniquely named temporary `Atomic` database, verify required tables, and run `CHECK TABLE`. Download, ClickHouse readiness, and ClickHouse operations have bounded timeouts. `--verify-only` then drops the temporary database without changing live data.

A real cutover still refuses to overwrite an existing SQLite database unless `ALLOW_NONEMPTY_RESTORE=1` is explicitly set. Only after every validation succeeds does it create the same exclusive maintenance marker used by online restore, wait up to the configured readiness timeout for backup and certificate operations to quiesce, and stop the controller, certificate renewer, and backup writer. It then renames the live ClickHouse database to a rollback name; promotes the temporary database; swaps SQLite, control secrets/TLS, and `control.env`; and waits for the restored controller health check. A failed cutover attempts the reverse swap and restarts the services that had been running only if every rollback step succeeds. An incomplete rollback fails closed with all writers stopped, retains the maintenance marker, and keeps the staged paths for inspection. A successful cutover deliberately retains the old ClickHouse database and timestamped filesystem paths for an operator-controlled rollback. Review and remove them only after edge heartbeats, DNS reconciliation, certificate jobs, and log ingestion are verified.

## Online restore

The authenticated **Settings > S3 online restore** workflow uses the currently effective S3/Restic profile and does not require Docker socket access. It lists only snapshots tagged `cdn-control-compose`. Starting a job requires the selected snapshot's 8-character short ID; the controller downloads it, validates SQLite integrity and schema compatibility, proves that encrypted settings can be opened by the current `CONTROL_ENCRYPTION_KEY`, checks the internal CA and control TLS pair, hashes the artifacts, and restores ClickHouse to a temporary database while the live control plane continues to serve.

When the job reaches **Ready**, live data is still unchanged. Committing requires the exact text `RESTORE`. The controller waits for active backup and certificate operations, writes a maintenance marker, exits cleanly, and applies the verified hashes at the next startup before opening SQLite. It promotes the temporary ClickHouse database, swaps SQLite (including stale WAL/SHM handling), internal CA, site Certbot state, and control TLS, then retains the previous files and ClickHouse database under job-specific rollback names. The current deployment environment files remain authoritative and are not restored by the online path. The Compose restart policy brings the controller back; the UI reconnects and the message center records the terminal state.

Only one restore job may be active. A downloading or validating job can be cancelled without touching live data; a ready job can also be cancelled and its temporary database is dropped. A committed job cannot be cancelled. If rollback is incomplete, the maintenance marker is retained and startup fails closed so an operator can inspect both versions rather than resume writers against a mixed state. Keep the offline script and credential record: online restore is a convenience for a healthy controller, not a replacement for bare-host disaster recovery.
