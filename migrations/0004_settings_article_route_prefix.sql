-- Article route prefix editable from admin settings; falls back to the
-- ARTICLE_ROUTE_PREFIX environment variable when NULL/empty.

-- +goose Up
ALTER TABLE settings ADD COLUMN article_route_prefix TEXT;

-- +goose Down
ALTER TABLE settings DROP COLUMN article_route_prefix;
