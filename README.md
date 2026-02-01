# Rables

A Rails-based CMS (Content Management System) for Jekyll static sites with article management, social media crossposting, and email newsletter subscriptions.

## Overview

Rables is a pure CMS backend that manages content for Jekyll static sites. Instead of rendering a public frontend, Rables exports content to Jekyll-compatible Markdown files with Front Matter.

## Features

- **Article Management**: Rich text editing with TinyMCE, scheduled publishing, drafts
- **Jekyll Integration**: Automatic export to Jekyll format (Front Matter + Markdown)
- **Social Media Crossposting**: Post to Twitter/X, Mastodon, Bluesky
- **Email Newsletter**: Subscribe/unsubscribe, email notifications
- **Comment Management**: Local and social media comment aggregation
- **Git Integration**: Auto-commit and push to Git repository
- **Static File Management**: Upload and manage assets

## Architecture

- **Backend**: Rails 8.1 with SQLite
- **Frontend**: None (Jekyll handles public site)
- **Content Export**: Markdown with YAML Front Matter
- **Asset Pipeline**: ActiveStorage with local or S3 storage

## Jekyll Export Format

Articles are exported as:

```markdown
---
layout: post
title: "Article Title"
date: 2024-01-01 10:00:00 +0800
categories: [tag1, tag2]
tags: [tag1, tag2]
description: "Article description"
---

Content in Markdown or HTML...
```
