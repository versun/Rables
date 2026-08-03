-- PRAGMA journal_mode cannot run inside a transaction, so the whole file opts out.
-- +goose NO TRANSACTION

-- +goose Up
PRAGMA journal_mode = WAL;

CREATE TABLE users (
  id INTEGER PRIMARY KEY,
  user_name TEXT NOT NULL UNIQUE,
  password_digest TEXT NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);

CREATE TABLE sessions (
  id INTEGER PRIMARY KEY,
  token TEXT NOT NULL UNIQUE,          -- cookie value; 32B random hex
  user_id INTEGER NOT NULL REFERENCES users(id),
  ip_address TEXT, user_agent TEXT,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);

-- status: 0=draft 1=publish 2=schedule 3=trash 4=shared (same order as the Rails enum)
CREATE TABLE articles (
  id INTEGER PRIMARY KEY,
  title TEXT, slug TEXT UNIQUE,
  content_html TEXT,                   -- merged from action_text_rich_texts.body or html_content at migration time
  content_type TEXT NOT NULL DEFAULT 'rich_text',  -- 'rich_text'|'html'; only keeps the "skip sanitize" semantic
  description TEXT, excerpt TEXT,
  meta_description TEXT, meta_title TEXT, meta_image TEXT,
  source_author TEXT, source_url TEXT, source_content TEXT,
  status INTEGER NOT NULL DEFAULT 0,
  comment INTEGER NOT NULL DEFAULT 0,
  scheduled_at INTEGER,
  scheduled_crosspost_platforms TEXT NOT NULL DEFAULT '[]',
  scheduled_send_newsletter INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE INDEX idx_articles_status ON articles(status);
CREATE INDEX idx_articles_status_created ON articles(status, created_at);

CREATE TABLE pages (
  id INTEGER PRIMARY KEY,
  title TEXT, slug TEXT UNIQUE,
  content_html TEXT, content_type TEXT NOT NULL DEFAULT 'rich_text',
  redirect_url TEXT,
  page_order INTEGER NOT NULL DEFAULT 0,
  status INTEGER NOT NULL DEFAULT 0,   -- same 5 states as articles
  comment INTEGER NOT NULL DEFAULT 0,
  scheduled_at INTEGER,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE INDEX idx_pages_status ON pages(status);

CREATE TABLE tags (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE, slug TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);

CREATE TABLE article_tags (
  id INTEGER PRIMARY KEY,
  article_id INTEGER NOT NULL REFERENCES articles(id),
  tag_id INTEGER NOT NULL REFERENCES tags(id),
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  UNIQUE(article_id, tag_id)
);

CREATE TABLE comments (
  id INTEGER PRIMARY KEY,
  commentable_type TEXT, commentable_id INTEGER,   -- 'Article'|'Page'
  article_id INTEGER REFERENCES articles(id),      -- legacy association kept
  parent_id INTEGER REFERENCES comments(id) ON DELETE CASCADE,
  author_name TEXT NOT NULL, author_email TEXT, author_url TEXT,
  author_username TEXT, author_avatar_url TEXT,
  content TEXT NOT NULL,
  status INTEGER NOT NULL DEFAULT 0,               -- 0=pending 1=approved 2=rejected
  platform TEXT, external_id TEXT, url TEXT,
  published_at INTEGER,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE INDEX idx_comments_commentable ON comments(commentable_type, commentable_id);
CREATE INDEX idx_comments_parent ON comments(parent_id);
CREATE INDEX idx_comments_status ON comments(status);
CREATE INDEX idx_comments_article ON comments(article_id);
CREATE UNIQUE INDEX idx_comments_ext_article ON comments(article_id, platform, external_id)
  WHERE platform IS NOT NULL AND external_id IS NOT NULL;
CREATE UNIQUE INDEX idx_comments_ext_commentable ON comments(commentable_type, commentable_id, platform, external_id)
  WHERE external_id IS NOT NULL;

CREATE TABLE subscribers (
  id INTEGER PRIMARY KEY,
  email TEXT NOT NULL UNIQUE,
  confirmation_token TEXT UNIQUE, unsubscribe_token TEXT UNIQUE,
  confirmed_at INTEGER, unsubscribed_at INTEGER,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);

CREATE TABLE subscriber_tags (
  id INTEGER PRIMARY KEY,
  subscriber_id INTEGER NOT NULL REFERENCES subscribers(id),
  tag_id INTEGER NOT NULL REFERENCES tags(id),
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  UNIQUE(subscriber_id, tag_id)
);

CREATE TABLE social_media_posts (
  id INTEGER PRIMARY KEY,
  article_id INTEGER NOT NULL REFERENCES articles(id),
  platform TEXT NOT NULL, url TEXT NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  UNIQUE(article_id, platform)
);

CREATE TABLE redirects (
  id INTEGER PRIMARY KEY,
  regex TEXT NOT NULL, replacement TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1, permanent INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE INDEX idx_redirects_enabled ON redirects(enabled);

-- Singleton table: CHECK (id = 1). Legacy static-generation/GitHub-backup fields are not carried over.
CREATE TABLE settings (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  title TEXT, description TEXT, author TEXT, url TEXT,
  time_zone TEXT NOT NULL DEFAULT 'UTC',
  head_code TEXT, custom_css TEXT, tool_code TEXT, giscus TEXT,
  social_links TEXT,                             -- JSON
  setup_completed INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);

CREATE TABLE newsletter_settings (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  enabled INTEGER NOT NULL DEFAULT 0,
  provider TEXT NOT NULL DEFAULT 'native',       -- 'native'|'listmonk'
  from_email TEXT,
  smtp_address TEXT, smtp_port INTEGER DEFAULT 587, smtp_user_name TEXT, smtp_password TEXT,
  smtp_domain TEXT, smtp_authentication TEXT DEFAULT 'plain', smtp_enable_starttls INTEGER DEFAULT 1,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);

CREATE TABLE crossposts (
  id INTEGER PRIMARY KEY,
  platform TEXT NOT NULL UNIQUE,                 -- mastodon|twitter|bluesky|xiaohongshu
  enabled INTEGER NOT NULL DEFAULT 0,
  api_key TEXT, api_key_secret TEXT, access_token TEXT, access_token_secret TEXT,
  client_id TEXT, client_key TEXT, client_secret TEXT,
  app_password TEXT, refresh_token TEXT, token_expires_at INTEGER,
  server_url TEXT, username TEXT,
  max_characters INTEGER,
  auto_fetch_comments INTEGER NOT NULL DEFAULT 0,
  comment_fetch_schedule TEXT,                   -- daily|weekly|monthly
  settings TEXT,                                 -- JSON
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);

CREATE TABLE listmonks (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  url TEXT, username TEXT, api_key TEXT,
  list_id INTEGER, template_id INTEGER,
  enabled INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);

CREATE TABLE twitter_syncs (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  enabled INTEGER NOT NULL DEFAULT 0,
  username TEXT, user_id TEXT, since_id TEXT,
  start_date TEXT,                               -- YYYY-MM-DD
  sync_schedule TEXT NOT NULL DEFAULT 'every_15_minutes',
  last_synced_at INTEGER, last_error TEXT,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);

CREATE TABLE twitter_archive_tweets (
  id INTEGER PRIMARY KEY,
  tweet_id TEXT NOT NULL UNIQUE,
  screen_name TEXT NOT NULL, full_text TEXT NOT NULL,
  entry_type TEXT NOT NULL,                      -- tweet|reply|retweet_quote
  tweeted_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE INDEX idx_tat_type_time ON twitter_archive_tweets(entry_type, tweeted_at);
CREATE INDEX idx_tat_created ON twitter_archive_tweets(created_at);

CREATE TABLE twitter_archive_connections (
  id INTEGER PRIMARY KEY,
  account_id TEXT NOT NULL, screen_name TEXT, user_link TEXT,
  relationship_type TEXT NOT NULL,               -- follower|following
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  UNIQUE(relationship_type, account_id)
);
CREATE INDEX idx_tac_screen_name ON twitter_archive_connections(screen_name);

CREATE TABLE twitter_archive_likes (
  id INTEGER PRIMARY KEY,
  tweet_id TEXT NOT NULL UNIQUE, full_text TEXT, expanded_url TEXT,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE INDEX idx_tal_created_tweet ON twitter_archive_likes(created_at, tweet_id);

CREATE TABLE twitter_archive_imports (
  id INTEGER PRIMARY KEY,
  status TEXT NOT NULL DEFAULT 'queued',         -- queued|running|completed|failed
  progress INTEGER NOT NULL DEFAULT 0,
  total_items_count INTEGER NOT NULL DEFAULT 0,
  tweets_count INTEGER NOT NULL DEFAULT 0, followers_count INTEGER NOT NULL DEFAULT 0,
  following_count INTEGER NOT NULL DEFAULT 0, likes_count INTEGER NOT NULL DEFAULT 0,
  source_filename TEXT NOT NULL, source_path TEXT,
  status_message TEXT, error_message TEXT,
  queued_at INTEGER NOT NULL, started_at INTEGER, finished_at INTEGER,
  active_slot INTEGER,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_tai_active_slot ON twitter_archive_imports(active_slot) WHERE active_slot IS NOT NULL;
CREATE INDEX idx_tai_status ON twitter_archive_imports(status);

CREATE TABLE activity_logs (
  id INTEGER PRIMARY KEY,
  level INTEGER NOT NULL DEFAULT 0,              -- 0=info 1=warn 2=error
  action TEXT, target TEXT, description TEXT,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE INDEX idx_activity_logs_created ON activity_logs(created_at);

CREATE TABLE files (
  id INTEGER PRIMARY KEY,
  key TEXT NOT NULL UNIQUE,                      -- migrated rows keep the old blob key; new files = 16B random hex
  filename TEXT NOT NULL, content_type TEXT,
  byte_size INTEGER NOT NULL DEFAULT 0, checksum TEXT,
  variant_of INTEGER REFERENCES files(id),       -- image variants point at the original
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_files_filename ON files(filename);

CREATE TABLE attachments (
  id INTEGER PRIMARY KEY,
  file_id INTEGER NOT NULL REFERENCES files(id),
  record_type TEXT NOT NULL, record_id INTEGER NOT NULL, name TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE(record_type, record_id, name, file_id)
);

CREATE TABLE static_files (
  id INTEGER PRIMARY KEY,
  filename TEXT NOT NULL UNIQUE,
  description TEXT,
  file_id INTEGER NOT NULL REFERENCES files(id),
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);

CREATE TABLE job_runs (
  id INTEGER PRIMARY KEY,
  kind TEXT NOT NULL,                            -- publish_article|publish_page|send_newsletter|crosspost|fetch_social_comments|export|import_zip|import_rss|twitter_archive_import|comment_reply_notification|newsletter_confirmation|password_reset
  payload TEXT,                                  -- JSON
  run_at INTEGER NOT NULL,
  status TEXT NOT NULL DEFAULT 'queued',         -- queued|running|done|failed
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE INDEX idx_job_runs_due ON job_runs(status, run_at);

CREATE TABLE kv (
  key TEXT PRIMARY KEY,
  value TEXT,
  updated_at INTEGER NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS kv;
DROP TABLE IF EXISTS job_runs;
DROP TABLE IF EXISTS static_files;
DROP TABLE IF EXISTS attachments;
DROP TABLE IF EXISTS files;
DROP TABLE IF EXISTS activity_logs;
DROP TABLE IF EXISTS twitter_archive_imports;
DROP TABLE IF EXISTS twitter_archive_likes;
DROP TABLE IF EXISTS twitter_archive_connections;
DROP TABLE IF EXISTS twitter_archive_tweets;
DROP TABLE IF EXISTS twitter_syncs;
DROP TABLE IF EXISTS listmonks;
DROP TABLE IF EXISTS crossposts;
DROP TABLE IF EXISTS newsletter_settings;
DROP TABLE IF EXISTS settings;
DROP TABLE IF EXISTS redirects;
DROP TABLE IF EXISTS social_media_posts;
DROP TABLE IF EXISTS subscriber_tags;
DROP TABLE IF EXISTS subscribers;
DROP TABLE IF EXISTS comments;
DROP TABLE IF EXISTS article_tags;
DROP TABLE IF EXISTS tags;
DROP TABLE IF EXISTS pages;
DROP TABLE IF EXISTS articles;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
