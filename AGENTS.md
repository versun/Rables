# AGENTS.md

This file provides guidance to coding agents (Codex CLI / Claude Code / etc.) when working in this repository.

## Scope

- Applies to the entire repository unless a more specific `AGENTS.md` exists in a subdirectory.
- Follow any deeper `AGENTS.md` instructions when editing files under that directory tree.

## Project Overview

Rables is a Rails 8.1 CMS backend for a Jekyll static site. It provides admin-only content management, social media crossposting,
email newsletters, and exports/syncs content into a Jekyll repo. Uses SQLite for all databases (primary, cache, queue, cable).

## Common Commands

```bash
# Ruby (mise) — agent runtime requirement
# Before running any `bundle`/`bin/rails`/`rake` command, ensure you are using the repo Ruby from `mise.toml`.
# Preferred (non-interactive-safe): run commands via `mise exec` so PATH is correct without shell hooks.
mise install
mise exec -- ruby -v
mise exec -- bundle -v
mise exec -- bin/rails -v

# If you need a persistent interactive shell session, you may also activate:
# eval "$(mise activate zsh)"

# Development server
bin/rails server

# Database
bin/rails db:migrate
bin/rails db:seed

# Tests
bin/rails test                    # Run all tests
bin/rails test test/models/       # Run model tests
bin/rails test test/models/article_test.rb  # Single test file
bin/rails test test/models/article_test.rb:42  # Single test at line

# Assets
bin/rails assets:precompile
bin/rails assets:clobber

# Code quality
bin/rubocop                       # Linting
bin/brakeman                      # Security scan

# Background jobs (Solid Queue)
bin/rails jobs:work               # Process jobs
```

## Working Agreements

- Make focused, minimal changes that directly address the request.
- Keep style consistent with surrounding code; prefer existing patterns over introducing new abstractions.
- Avoid unrelated refactors and do not add new dependencies unless explicitly requested.
- Test-first workflow for behavior changes:
  - Before changing code, check whether relevant automated tests already exist (unit/integration/system) and cover the requested behavior.
  - If no suitable test exists, create the test first (or extend the closest existing test) to encode the expected behavior.
  - After implementing the change, run the smallest relevant test subset first, then broaden as needed; do not consider the task done until tests pass and the goal is met.

## Architecture

### Content Model
- **Article**: Main content type with rich text (ActionText) or HTML content. Supports statuses: draft, publish, schedule, trash, shared. Has tags, comments, and social media posts.
- **Page**: Static pages with similar structure to articles
- **Tag**: Categorization with slugs, supports subscriber notifications

### Jekyll Integration
- Jekyll settings are stored in `JekyllSetting` (single record).
- Sync/export lives in `JekyllExport` + `JekyllSyncService`.
- Admin UI: `/admin/jekyll` and `/admin/jekyll_sync_records`.

### Social Media Integration
Located in `app/services/`:
- **TwitterService**: X/Twitter posting with media upload
- **MastodonService**: Mastodon posting
- **BlueskyService**: Bluesky posting with rich text facets

### Background Jobs (Solid Queue)
Key jobs in `app/jobs/`:
- `CrosspostArticleJob`: Post to social platforms
- `NativeNewsletterSenderJob`: Send newsletters to subscribers
- `FetchSocialCommentsJob`: Import comments from social posts
- `PublishScheduledArticlesJob`: Auto-publish scheduled articles
- `JekyllSyncJob`: Full Jekyll sync
- `JekyllSingleSyncJob`: Single article/page sync

### Admin Interface
All admin routes under `/admin/` namespace. Key controllers handle:
- Article/Page CRUD with batch operations
- Comment moderation (approve/reject)
- Settings, newsletter config, crosspost config
- Static file management, redirects
- Import/Export (WordPress, RSS, ZIP)

### Key Models
- `Setting`: Site-wide configuration (singleton pattern via `first_or_create`)
- `Subscriber`: Newsletter subscribers with tag-based subscriptions
- `Redirect`: URL redirect rules with regex support
- `StaticFile`: User-uploaded files for static hosting

### Frontend
- Uses Hotwire (Turbo + Stimulus)
- Importmap for JS modules
- Trix editor for rich text (ActionText)
- Admin CSS in `admin.css`
- Public endpoints use a lightweight `public.css`

### Storage
- ActiveStorage with local disk or S3 (configurable)
- Static files served from `storage/static/` or via StaticFile records
