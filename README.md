# Rables

A Rails-based CMS backend for Jekyll static sites. Provides content management, social media crossposting, email newsletters, and automatic Jekyll synchronization.

The code that runs my weblog, https://versun.me

## Features

- **Content Management**: Articles, pages, and tags with rich text editing (TinyMCE)
- **Jekyll Integration**: Automatic sync to Jekyll projects with customizable front matter
- **Social Media Crossposting**: Post to Twitter/X, Mastodon, and Bluesky
- **Email Newsletters**: Native email sending or Listmonk integration
- **Comment Management**: Local comments and social media comment imports
- **URL Redirects**: Exportable to Netlify, Vercel, htaccess, or nginx formats
- **Static File Management**: Upload and manage files for your Jekyll site

## Requirements

- Ruby 3.3+ (managed via mise)
- SQLite 3
- Node.js (for asset compilation)

## Setup

```bash
# Install Ruby via mise
mise install

# Install dependencies
mise exec -- bundle install

# Setup database
mise exec -- bin/rails db:setup

# Start server
mise exec -- bin/rails server
```

Visit `http://localhost:3000/admin` to access the admin interface.

## Jekyll Integration

1. Go to Admin > Jekyll Settings
2. Configure your Jekyll project path
3. Optionally configure Git repository for automatic commits
4. Enable "Sync on Publish" for automatic synchronization

This won't work out of the box for anyone else, but you're welcome to take a look at the code to see how it works.
