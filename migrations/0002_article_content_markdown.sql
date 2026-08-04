-- Articles written in Markdown keep the source here; content_html always
-- stores the rendered (sanitized) HTML so every reader (public page, feed,
-- newsletter, crosspost) stays unchanged. NULL for rich_text/html articles.

-- +goose Up
ALTER TABLE articles ADD COLUMN content_markdown TEXT;

-- +goose Down
ALTER TABLE articles DROP COLUMN content_markdown;
