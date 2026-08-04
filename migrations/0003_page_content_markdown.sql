-- Markdown-authored pages keep the source here, same convention as
-- articles.content_markdown (0002): content_html always stores the rendered
-- (sanitized) HTML. NULL for rich_text/html pages.

-- +goose Up
ALTER TABLE pages ADD COLUMN content_markdown TEXT;

-- +goose Down
ALTER TABLE pages DROP COLUMN content_markdown;
