-- Twitter archive (plan section 4.10): full-replace import tables, import
-- history rows and the public archive pages.

-- name: DeleteAllTwitterArchiveTweets :exec
DELETE FROM twitter_archive_tweets;

-- name: DeleteAllTwitterArchiveConnections :exec
DELETE FROM twitter_archive_connections;

-- name: DeleteAllTwitterArchiveLikes :exec
DELETE FROM twitter_archive_likes;

-- Upsert carries the duplicate-row merge semantics of
-- TwitterArchiveImporter#merge_tweet_rows: the higher-ranked entry type wins
-- (retweet_quote > reply > tweet), the non-"archive" screen name wins, the
-- longer text wins, and the first tweeted_at sticks.
-- name: UpsertTwitterArchiveTweet :one
INSERT INTO twitter_archive_tweets (tweet_id, screen_name, full_text, entry_type, tweeted_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(tweet_id) DO UPDATE SET
  entry_type = CASE
    WHEN (CASE excluded.entry_type WHEN 'retweet_quote' THEN 2 WHEN 'reply' THEN 1 ELSE 0 END)
       > (CASE twitter_archive_tweets.entry_type WHEN 'retweet_quote' THEN 2 WHEN 'reply' THEN 1 ELSE 0 END)
    THEN excluded.entry_type ELSE twitter_archive_tweets.entry_type END,
  screen_name = CASE
    WHEN twitter_archive_tweets.screen_name != '' AND twitter_archive_tweets.screen_name != 'archive' THEN twitter_archive_tweets.screen_name
    WHEN excluded.screen_name != '' AND excluded.screen_name != 'archive' THEN excluded.screen_name
    WHEN twitter_archive_tweets.screen_name != '' THEN twitter_archive_tweets.screen_name
    WHEN excluded.screen_name != '' THEN excluded.screen_name
    ELSE 'archive' END,
  full_text = CASE
    WHEN length(excluded.full_text) > length(twitter_archive_tweets.full_text) THEN excluded.full_text
    ELSE twitter_archive_tweets.full_text END
RETURNING id;

-- OR IGNORE mirrors the in-memory seen_connections/seen_likes dedup of the
-- Rails importer: the first occurrence of a key wins.
-- name: InsertTwitterArchiveConnection :exec
INSERT OR IGNORE INTO twitter_archive_connections (account_id, screen_name, user_link, relationship_type, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: InsertTwitterArchiveLike :exec
INSERT OR IGNORE INTO twitter_archive_likes (tweet_id, full_text, expanded_url, created_at, updated_at)
VALUES (?, ?, ?, ?, ?);

-- Media cleanup for the full replace (mirrors has_many_attached
-- dependent: :purge_later on TwitterArchiveTweet#media).
-- name: ListTwitterArchiveTweetMediaFiles :many
SELECT f.id, f.key FROM attachments a
JOIN files f ON f.id = a.file_id
WHERE a.record_type = 'TwitterArchiveTweet';

-- name: DeleteTwitterArchiveTweetAttachments :exec
DELETE FROM attachments WHERE record_type = 'TwitterArchiveTweet';

-- name: CountAttachmentsForFile :one
SELECT COUNT(*) FROM attachments WHERE file_id = ?;

-- name: DeleteFile :exec
DELETE FROM files WHERE id = ?;

-- Import history rows (twitter_archive_imports). active_slot is 1 while the
-- row is queued/running and NULL afterwards, so the partial unique index
-- guarantees a single active import.
-- name: CreateTwitterArchiveImport :one
INSERT INTO twitter_archive_imports (status, progress, source_filename, source_path, status_message, queued_at, active_slot, created_at, updated_at)
VALUES ('queued', 0, ?, ?, 'Queued', ?, 1, ?, ?)
RETURNING *;

-- name: GetTwitterArchiveImport :one
SELECT * FROM twitter_archive_imports WHERE id = ?;

-- name: HasActiveTwitterArchiveImport :one
SELECT COUNT(*) FROM twitter_archive_imports WHERE status IN ('queued', 'running');

-- name: ListTwitterArchiveImportsRecent :many
SELECT * FROM twitter_archive_imports ORDER BY created_at DESC, id DESC LIMIT ?;

-- name: MarkTwitterArchiveImportRunning :exec
UPDATE twitter_archive_imports
SET status = 'running', progress = 5, status_message = 'Reading archive',
    started_at = ?, finished_at = NULL, error_message = NULL, updated_at = ?
WHERE id = ?;

-- name: UpdateTwitterArchiveImportProgress :exec
UPDATE twitter_archive_imports
SET progress = ?, status_message = ?, updated_at = ?
WHERE id = ?;

-- name: CompleteTwitterArchiveImport :exec
UPDATE twitter_archive_imports
SET status = 'completed', progress = 100, status_message = 'Import completed',
    tweets_count = ?, followers_count = ?, following_count = ?, likes_count = ?, total_items_count = ?,
    finished_at = ?, error_message = NULL, active_slot = NULL, updated_at = ?
WHERE id = ?;

-- name: FailTwitterArchiveImport :exec
UPDATE twitter_archive_imports
SET status = 'failed', status_message = 'Import failed', error_message = ?,
    finished_at = ?, active_slot = NULL, updated_at = ?
WHERE id = ?;

-- name: ClearTwitterArchiveImportSource :exec
UPDATE twitter_archive_imports SET source_path = NULL, updated_at = ? WHERE id = ?;

-- Admin index summary.
-- name: CountTwitterArchiveTweetsByType :many
SELECT entry_type, COUNT(*) AS n FROM twitter_archive_tweets GROUP BY entry_type;

-- name: CountTwitterArchiveConnectionsByType :many
SELECT relationship_type, COUNT(*) AS n FROM twitter_archive_connections GROUP BY relationship_type;

-- name: CountTwitterArchiveLikes :one
SELECT COUNT(*) FROM twitter_archive_likes;

-- TwitterArchiveImport.last_imported_at: latest completed import finish,
-- falling back to the newest archived tweet.
-- name: TwitterArchiveLastImportedAt :one
SELECT COALESCE(
  (SELECT MAX(finished_at) FROM twitter_archive_imports WHERE status = 'completed'),
  (SELECT MAX(created_at) FROM twitter_archive_tweets)
) AS last_imported_at;

-- Public archive pages (20 per tab page).
-- name: ListTwitterArchiveTweetsByType :many
SELECT * FROM twitter_archive_tweets
WHERE entry_type = ?
ORDER BY tweeted_at DESC, tweet_id DESC
LIMIT ? OFFSET ?;

-- name: CountTwitterArchiveTweetsOfType :one
SELECT COUNT(*) FROM twitter_archive_tweets WHERE entry_type = ?;

-- name: ListTwitterArchiveLikes :many
SELECT * FROM twitter_archive_likes
ORDER BY created_at DESC, tweet_id DESC
LIMIT ? OFFSET ?;

-- name: ListTwitterArchiveTweetMedia :many
SELECT f.key, f.filename, f.content_type FROM attachments a
JOIN files f ON f.id = a.file_id
WHERE a.record_type = 'TwitterArchiveTweet' AND a.name = 'media' AND a.record_id = ?
ORDER BY a.id;
