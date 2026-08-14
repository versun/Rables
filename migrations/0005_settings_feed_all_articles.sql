-- Feed size toggle editable from admin settings: 0 = only the 10 newest
-- published articles in feed.xml, 1 = all published articles.

-- +goose Up
ALTER TABLE settings ADD COLUMN feed_all_articles INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE settings DROP COLUMN feed_all_articles;
