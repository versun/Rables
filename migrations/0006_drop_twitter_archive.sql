-- The Twitter Archive feature (official export ZIP import + public timeline
-- page) is removed; drop its four tables, the attachment rows of imported
-- tweet media, and any job_runs rows of the removed import kind (the worker
-- no longer registers a handler and would fail them as "unknown job kind").
-- With those attachments gone, the files rows and disk blobs of the media
-- become unreferenced and are reclaimed by the startup jobs.ReapOrphanFiles
-- sweep (it only reaps files older than 24h, so same-day media takes one
-- more restart).

-- +goose Up
DELETE FROM attachments WHERE record_type = 'TwitterArchiveTweet';
DELETE FROM job_runs WHERE kind = 'twitter_archive_import';
DROP TABLE IF EXISTS twitter_archive_tweets;
DROP TABLE IF EXISTS twitter_archive_connections;
DROP TABLE IF EXISTS twitter_archive_likes;
DROP TABLE IF EXISTS twitter_archive_imports;

-- +goose Down
-- The archive tables are not recreated on rollback: their data is gone once
-- dropped, and the feature no longer exists to populate them.
