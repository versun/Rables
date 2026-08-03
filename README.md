# Rables (Go)

A pure-Go rewrite of [Rables](https://github.com/versun/Rables), the Rails personal blog system that runs https://versun.me.

Single static binary + a `data/` directory. No Node.js, no frontend build step, no Redis/Postgres — SQLite (pure Go, no cgo) for everything.

The Rails app in the parent directory remains the behavioral source of truth. The full execution spec, task breakdown (T01–T30), and the decision log live in [`../docs/plans/go-rewrite-plan.md`](../docs/plans/go-rewrite-plan.md).

## Status

All implementation tasks (T01–T29) are complete: core blog, admin, comments, subscriptions, newsletters, crossposting, Twitter sync/archive, import/export, Rails migration tool, vanilla JS frontend, and deployment artifacts. Remaining: T30 (production cutover), which must be executed against the live environment.

## Features

- **Content**: articles & pages with draft/publish/schedule/trash/shared states, dual editor modes (Lexxy rich text / raw HTML), tags, scheduled publishing with crosspost & newsletter snapshots
- **Comments**: threaded comments with math captcha + per-IP rate limiting, admin moderation/reply, social comment import (Mastodon/Bluesky/X)
- **Newsletter**: native SMTP or listmonk, tag-scoped subscriptions, double opt-in confirm/unsubscribe
- **Crossposting**: Mastodon, Bluesky (hand-written XRPC + facets), X (OAuth1.0a, chunked media upload, quote-tweet/GIF rules); Xiaohongshu is log-only
- **Twitter**: account sync (tweets archived as articles) + official archive ZIP import (streaming, memory-flat) with a public timeline page
- **Transfer**: full-site ZIP export/import (CSV manifests, credential redaction), Markdown export, RSS import (SSRF-hardened)
- **Ops**: background jobs (`job_runs` table + in-process worker), cron scheduler, activity log, regex redirects, static file hosting

## Quick start

Requires Go ≥ 1.24 (developed on 1.26).

```bash
go build -o server ./cmd/server

HMAC_SECRET=change-me ./server
# open http://localhost:8080/setup
```

Configuration via environment variables:

| Var | Default | Notes |
|---|---|---|
| `ADDR` | `:8080` | Listen address |
| `DATA_DIR` | `./data` | SQLite DB + uploaded files live here |
| `HMAC_SECRET` | *(required)* | Signs math-captcha tokens |
| `ARTICLE_ROUTE_PREFIX` | *(empty)* | Optional prefix for article URLs (e.g. `blog`) |
| `LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` (JSON logs to stdout) |

## Development

```bash
# quality gate (run before handoff — must be all green)
gofmt -l . && go vet ./... && go test ./...

# regenerate sqlc code after editing queries/*.sql
go tool sqlc generate

# database migrations are embedded and applied automatically at startup
# (migrations/*.sql via goose)
```

Conventions: timestamps are INTEGER unix seconds (UTC) everywhere; HTML forms are GET/POST only (Rails PATCH/DELETE actions map to POST path variants); each HTTP feature lives in `internal/httpd/<feature>.go` and exposes `RegisterXxxRoutes`, wired centrally in `router.go`.

## Deployment

See [`deploy/README.md`](deploy/README.md). In short:

```bash
docker build -t rables .
docker run -e HMAC_SECRET=change-me -v rables-data:/data -p 8080:8080 rables
```

- `Dockerfile` — multi-stage build → distroless nonroot image (~48MB)
- `deploy/rables.service` — hardened systemd unit
- `deploy/backup.sh` — online SQLite backup (`.backup`) + files tarball with retention

## Migrating from the Rails app

```bash
go build -o migrate-rails ./cmd/migrate-rails
./migrate-rails -old /path/to/rails/db/production.sqlite3 -data ./data --verify-files
```

The tool is idempotent (safe to re-run to catch up before cutover), migrates all content and rewrites ActionText attachments to `/files/<key>` URLs, and prints a per-table report (old/inserted/skipped counts). It exits non-zero if row counts mismatch. Disk files are **not** copied — mount the Rails `storage/` directory as `DATA_DIR/files/` (the `xx/yy/<key>` layout is identical). Sessions are not migrated; everyone logs in again after cutover.

## Layout

```
cmd/server/         the one long-running process (HTTP + job worker + cron)
cmd/migrate-rails/  one-shot Rails → Go migration tool
internal/config/    env config + logger
internal/db/        goose-embedded migrations, connection, sqlc-generated queries
internal/domain/    pure functions (state machine, slug, excerpt, sanitize, contentbuilder)
internal/httpd/     chi router, middleware, all HTTP handlers
internal/jobs/      job_runs worker + cron scheduler
internal/service/   articles, comments, crosspost, media, newsletter, transfer,
                    twitterarchive, twittersync, railsmigrate, ...
internal/templates/ embedded html/template pages (+ `_`-prefixed partials)
internal/assets/    embedded app.js (vanilla, no build) / app.css / Lexxy editor
migrations/         full DDL (goose)
queries/            sqlc sources (pure ASCII only)
deploy/             Dockerfile companions: systemd unit, backup script, docs
```
