-- Redirect rules can match the request Host (subdomain redirects like
-- abc.example.com -> example.com/abc) in addition to the URL path. match_on is
-- 'path' (the Rails behavior) or 'host' (regex runs against the Host header
-- without port); existing rows stay 'path'.

-- +goose Up
ALTER TABLE redirects ADD COLUMN match_on TEXT NOT NULL DEFAULT 'path';

-- +goose Down
ALTER TABLE redirects DROP COLUMN match_on;
