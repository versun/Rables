# Rables

A self-hosted personal blog and publishing platform, written in Go. The whole
app ships as a **single static binary** — templates, assets and migrations are
embedded — with **SQLite** as the only datastore. It is a Go port of a Rails
original; the schema and routes deliberately mirror the Rails behavior.

## Features

**Writing & publishing**
- Articles and standalone pages with a Markdown editor (EasyMDE) and image/file uploads
- Draft / publish / schedule / trash / shared states; scheduled publishing runs as background jobs
- Optional public route prefix for article URLs (per-request live setting)
- Uploaded HTML archives served under `/archives/{kind}/{id}/`

**Feed & discovery**
- Full-site RSS at `/feed.xml`, per-tag RSS at `/tags/{slug}.rss`, `sitemap.xml`
- Tags with published-article counts

**Engagement**
- Threaded comments with math captcha (HMAC-signed tokens) and admin moderation
- Newsletter: native SMTP sender or listmonk, double opt-in subscribe/confirm/unsubscribe, per-tag subscriptions
- Crossposting to Mastodon, Twitter/X, Bluesky and Xiaohongshu, with automatic comment fetch-back on a schedule
- Ongoing Twitter/X timeline sync
- Optional giscus integration, custom head code and custom CSS

**Site management (admin)**
- Regex redirects (URL path and host/subdomain matching), static file hosting, social links, timezone, meta/SEO fields
- Background job dashboard (`/admin/jobs`) with stale-job recovery, activity log
- Import/export: full-site transfer (DB + files ZIP), RSS import, Markdown import;
  `cmd/migrate-content` converts legacy `rich_text` content to Markdown in place

**Security**
- Session-cookie auth, rate-limited login/password/subscription endpoints
- Origin checks on state-changing requests, SSRF guard for outbound fetches
- Strict file permissions: data dir `0700`, database `0600`

## Quick start

Requires Go 1.26+.

```sh
HMAC_SECRET="$(openssl rand -hex 32)" go run ./cmd/server
```

Open http://localhost:8080/setup to create the admin account, then log in at
`/session/new`. All runtime state lives in `./data` (SQLite `rables.db` +
uploaded `files/`); migrations run automatically at boot.

## Configuration

| Var | Default | Notes |
|---|---|---|
| `ADDR` | `:8080` | Listen address |
| `DATA_DIR` | `./data` | SQLite DB + uploaded files |
| `HMAC_SECRET` | — | **Required**; signs math-captcha tokens |
| `LOG_LEVEL` | `info` | `debug\|info\|warn\|error` |
| `ARTICLE_ROUTE_PREFIX` | — | Fallback public route prefix; the admin setting takes precedence |
| `TRUST_X_FORWARDED_FOR` | `false` | Key rate limits off `X-Forwarded-For`; only behind a trusted proxy |
| `SECURE_COOKIES` | `false` | Add `Secure` to session/flash cookies; enable for HTTPS |

## Deployment

See [deploy/README.md](deploy/README.md) for the full guide: hardened systemd
unit, Docker (distroless, ~48 MB image), backups with `deploy/backup.sh`, and
the rich-text → Markdown upgrade path.

```sh
# Binary
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o rables-server ./cmd/server

# Docker
docker build -t rables .
docker run -d -e HMAC_SECRET="$(openssl rand -hex 32)" -p 8080:8080 \
  -v rables-data:/data --restart unless-stopped rables
```

## Development

```
cmd/server            HTTP server entrypoint (wiring, graceful shutdown)
cmd/migrate-content   One-off rich_text → Markdown content migration tool
internal/httpd        chi router, handlers, middleware, rate limiting
internal/service      Domain services (articles, comments, crosspost,
                      newsletter, subscribers, transfer, twittersync, ...)
internal/domain       Pure content logic (markdown, sanitize, slug, excerpt)
internal/jobs         DB-backed job queue + cron scheduler
internal/db           SQLite open/migrate; sqlc-generated queries in db/query
internal/templates    Embedded html/template files
internal/assets       Embedded CSS/JS
migrations            Embedded goose migrations (run at boot)
queries               sqlc query sources
```

- **Run tests:** `go test ./...`
- **Regenerate sqlc code** after editing `queries/` or `migrations/`:
  `go tool sqlc generate`
- **Add a migration:** create `migrations/NNNN_name.sql` with goose
  `-- +goose Up` / `-- +goose Down` sections; it applies on next boot.

The database is pure-Go SQLite (`modernc.org/sqlite`), so builds are CGO-free
and cross-compile cleanly.

## License

See [LICENSE](LICENSE).
