# Rables

Rables is a Rails 8.1 CMS backend for managing content that is synced into a Jekyll site. It keeps the full admin workflow
(articles, pages, tags, comments, newsletters, crossposting) while removing the public-facing blog UI.

Key points:
- Admin UI lives under `/admin`.
- Public endpoints remain for subscriptions, comments, and static files.
- Jekyll sync writes Markdown + assets into a configured Jekyll directory, optionally committing via Git.

This repo is tailored to its owner, but the code is usable as a reference for building a Jekyll-connected CMS backend.
