# Deploying Rables (Go)

The deliverable is a single static binary plus a `data/` directory. All
templates, migrations and assets are embedded; the only runtime state is
`DATA_DIR` (SQLite `rables.db` + uploaded `files/` + extracted
`archives/` trees for html_archive posts).

Required environment:

| Var | Default | Notes |
|---|---|---|
| `ADDR` | `:8080` | listen address |
| `DATA_DIR` | `./data` | SQLite DB + uploaded files live here |
| `HMAC_SECRET` | — | **required**, signs math-captcha tokens; app refuses to boot without it |
| `LOG_LEVEL` | `info` | `debug\|info\|warn\|error` |
| `ARTICLE_ROUTE_PREFIX` | — | fallback public route prefix; the admin setting (/admin/setting/edit) takes precedence and applies live |
| `TRUST_X_FORWARDED_FOR` | `false` | `1`/`true`/`yes`: key rate limits off the rightmost `X-Forwarded-For` hop (the one the proxy appended) — enable only behind a reverse proxy that appends to the header |
| `SECURE_COOKIES` | `false` | `1`/`true`/`yes`: add `Secure` to session/flash cookies (enable when serving HTTPS) |

## Option A: systemd (bare metal / VM)

```sh
# 1. Build and install the binary
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o rables-server ./cmd/server
sudo install -m 0755 rables-server /usr/local/bin/rables-server

# 2. Create the service user and data directory
sudo useradd --system --home /var/lib/rables --shell /usr/sbin/nologin rables
sudo install -d -o rables -g rables /var/lib/rables

# 3. Install the unit, then edit in a real HMAC_SECRET
sudo install -m 0644 deploy/rables.service /etc/systemd/system/rables.service
sudoedit /etc/systemd/system/rables.service   # replace HMAC_SECRET=CHANGE_ME

sudo systemctl daemon-reload
sudo systemctl enable --now rables
curl -fsS localhost:8080/up        # -> ok
```

The unit is hardened (`ProtectSystem=strict`, `NoNewPrivileges`, ...); the
service can only write under `ReadWritePaths=/var/lib/rables`. Keep
`WorkingDirectory` and `ReadWritePaths` in sync with `DATA_DIR` if you move it.
`UMask=0077` keeps everything the service creates owner-only (`rables.db`
holds session tokens and API keys, and transfer export ZIPs land in
`DATA_DIR`); `db.Open` also chmods the DB file itself to `0600`, so existing
deployments are covered too.

## Option B: Docker

```sh
docker build -t rables .
docker run -d --name rables \
  -e HMAC_SECRET="$(openssl rand -hex 32)" \
  -p 8080:8080 \
  -v rables-data:/data \
  --restart unless-stopped \
  rables
curl -fsS localhost:8080/up        # -> ok
```

- Image: multi-stage `golang:1.26` build → `gcr.io/distroless/base-debian12:nonroot`
  (no shell; runs as uid/gid 65532; ships ca-certificates for outbound HTTPS
  crossposting and tzdata for `settings.time_zone`).
- Mount `/data` to a named volume (as above) or a bind mount, otherwise all
  state is lost when the container is replaced. For a **bind mount** the host
  directory must be
  writable by uid 65532: `sudo chown -R 65532:65532 /srv/rables`.
- Graceful shutdown can take up to ~40 s in the worst case (HTTP shutdown
  10 s + job worker 15 s + cron scheduler 15 s), which exceeds the default
  10 s `docker stop` grace period and would end in a SIGKILL. Use
  `docker stop -t 60`, or pass `--stop-timeout 60` to `docker run`.

## Upgrading: rich_text → markdown content

Installs that predate the markdown editor may still carry `rich_text`
article/page rows. Restores through the admin import convert them
automatically; for an in-place upgrade run `migrate-content` against the
database (dry-run first, `-yes` applies and takes a timestamped
`VACUUM INTO` backup next to the database):

```sh
go build -o migrate-content ./cmd/migrate-content
./migrate-content -db /var/lib/rables/rables.db        # dry-run report
./migrate-content -db /var/lib/rables/rables.db -yes   # apply
```

Restart the service afterwards to drop the in-memory render cache.

## Backups

`deploy/backup.sh` takes an online snapshot while the server keeps running:

1. `sqlite3 .backup` — consistent copy of `rables.db` (WAL-safe);
2. `tar.gz` of `DATA_DIR/files/` (uploaded media; `exports/` is regenerable);
3. `tar.gz` of `DATA_DIR/archives/` (extracted html_archive trees — the only
   copy of archive posts' content; `.staging-*` is skipped).

```sh
# cron, daily at 03:17 UTC
17 3 * * * DATA_DIR=/var/lib/rables BACKUP_DIR=/var/backups/rables /usr/local/bin/backup.sh
```

Defaults: `DATA_DIR=/var/lib/rables`, `BACKUP_DIR=/var/backups/rables`,
`KEEP=14` snapshots. Requires the `sqlite3` CLI on the host
(`apt install sqlite3`). An `rsync` off-site copy line is included, commented
out. Each snapshot is a timestamped directory with `rables.db` +
`files.tar.gz` (+ `archives.tar.gz` once an html_archive post exists); older
snapshots beyond `KEEP` are pruned.

Restore: stop the service, replace `DATA_DIR/rables.db` with the backed-up
file, untar `files.tar.gz` (and `archives.tar.gz` when present) into
`DATA_DIR/`, start the service.

## Measured (T29 smoke/stress run, 2026-08-03, Apple Silicon)

- `docker build` succeeds; image size **48.5 MB** (`rables:t29`).
- Container smoke test (`docker run -e HMAC_SECRET=... -p 18081:8080`):
  `GET /up` → 200, `GET /setup` → 200, full setup POST → 302 and `GET /` → 200
  (SQLite write into the `/data` volume works). Process runs as uid 65532
  (nonroot); `docker stop` (SIGTERM) logs `shutdown complete`. Image contains
  `/etc/ssl/certs/ca-certificates.crt` and `/usr/share/zoneinfo/*` (verified by
  copying both out of the image).
- RSS under load (native binary, seeded with 5 long articles ~55 KB HTML with
  images): fresh RSS 27.6 MB; 8000 requests over `/`, `/{slug}` and
  `/files/<key>` at 32-way concurrency (~286 req/s, all 200) → **peak RSS
  50.5 MB**, settled 50.5 MB — well under the 100 MB budget. A prior round of
  6000 requests showed the same plateau (51.5 MB), i.e. no growth trend.
