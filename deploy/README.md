# Deploying Rables (Go)

The deliverable is a single static binary plus a `data/` directory. All
templates, migrations and assets are embedded; the only runtime state is
`DATA_DIR` (SQLite `rables.db` + uploaded `files/`).

Required environment:

| Var | Default | Notes |
|---|---|---|
| `ADDR` | `:8080` | listen address |
| `DATA_DIR` | `./data` | SQLite DB + uploaded files live here |
| `HMAC_SECRET` | — | **required**, signs math-captcha tokens; app refuses to boot without it |
| `LOG_LEVEL` | `info` | `debug\|info\|warn\|error` |
| `ARTICLE_ROUTE_PREFIX` | — | optional public route prefix |

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
- `/data` is a volume. For a **bind mount** the host directory must be
  writable by uid 65532: `sudo chown -R 65532:65532 /srv/rables`.
- Migrating from the Rails app: point the volume at a copy of the old
  `storage/` tree as `/data/files/` (key layout `xx/yy/<key>` matches) and run
  `migrate-rails` against the old DB (see `cmd/migrate-rails`).

## Backups

`deploy/backup.sh` takes an online snapshot while the server keeps running:

1. `sqlite3 .backup` — consistent copy of `rables.db` (WAL-safe);
2. `tar.gz` of `DATA_DIR/files/` (uploaded media; `exports/` is regenerable).

```sh
# cron, daily at 03:17 UTC
17 3 * * * DATA_DIR=/var/lib/rables BACKUP_DIR=/var/backups/rables /usr/local/bin/backup.sh
```

Defaults: `DATA_DIR=/var/lib/rables`, `BACKUP_DIR=/var/backups/rables`,
`KEEP=14` snapshots. Requires the `sqlite3` CLI on the host
(`apt install sqlite3`). An `rsync` off-site copy line is included, commented
out. Each snapshot is a timestamped directory with `rables.db` +
`files.tar.gz`; older snapshots beyond `KEEP` are pruned.

Restore: stop the service, replace `DATA_DIR/rables.db` with the backed-up
file, untar `files.tar.gz` into `DATA_DIR/`, start the service.

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
