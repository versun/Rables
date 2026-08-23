-- Redirects gain a simple no-regex mode: match_from is a plain string, either
-- "/path" (any host) or "host/path", matched exactly or as a prefix
-- (match_prefix), with replacement as the target. position is the explicit
-- evaluation order (drag-ordered in the admin); the first matching rule wins.
--
-- Regex rules keep the match_on column with new values: 'path' (regex against
-- the URL path, the Rails behavior) or 'host_path' (regex against host+path,
-- replacing 'host'). Existing 'host' rules matched the Host header alone with
-- implicit whole-host anchoring; rewritten to host+path form they must consume
-- the path too, so the pattern is wrapped in a non-capturing group (keeps
-- top-level alternations intact and preserves capture-group numbering) and an
-- optional path group appended, with a trailing '$' anchor dropped first.
-- Simple rules store 'simple' in match_on and an empty regex.

-- +goose Up
ALTER TABLE redirects ADD COLUMN match_from TEXT NOT NULL DEFAULT '';
ALTER TABLE redirects ADD COLUMN match_prefix INTEGER NOT NULL DEFAULT 0;
ALTER TABLE redirects ADD COLUMN position INTEGER NOT NULL DEFAULT 0;

UPDATE redirects
SET regex = '(?:' || CASE
                -- A literal trailing \$ (no host can contain one) is kept;
                -- stripping just the '$' would leave a dangling backslash.
                WHEN substr(regex, -2) = '\$' THEN regex
                WHEN substr(regex, -1) = '$' THEN substr(regex, 1, length(regex) - 1)
                ELSE regex
            END || ')(?:/.*)?$',
    match_on = 'host_path'
-- A blank regex is left as 'host': the middleware's blank-pattern skip keeps
-- it inert, while the rewritten '(?:)(?:/.*)?$' form would match every path
-- on a request without a Host header.
WHERE match_on = 'host' AND regex <> '';

-- The unified list evaluates by position. Backfill in two tiers so the
-- upgrade does not change which rule wins: the rewritten host rules, which
-- the old middleware always evaluated before every path rule, come first;
-- within each tier the relative creation order (id) is kept.
UPDATE redirects SET position = id + (SELECT MAX(id) FROM redirects) WHERE match_on <> 'host_path';
UPDATE redirects SET position = id WHERE match_on = 'host_path';

-- +goose Down
-- Reverse of the Up rewrite for rows it produced: strip the '(?:/.*)?$'
-- suffix and unwrap the '(?:...)' shell, restoring the original pattern text.
-- A trailing literal \$ kept by Up means the original had no '$' anchor, so
-- none is added back; a pattern that had no '$' at all cannot be told apart
-- from an anchored one after Up and gains a '$' (harmless: the old middleware
-- anchored host rules to the whole host anyway).
UPDATE redirects
SET regex = substr(regex, 4, length(regex) - 13) || CASE
                WHEN substr(regex, -12) = '\$)(?:/.*)?$' THEN ''
                ELSE '$'
            END,
    match_on = 'host'
WHERE match_on = 'host_path'
  AND substr(regex, 1, 3) = '(?:'
  AND substr(regex, -length('(?:/.*)?$')) = '(?:/.*)?$';
-- Rows the old schema cannot express (host+path patterns in new form, simple
-- rules) are disabled rather than reinterpreted into something wrong.
UPDATE redirects SET match_on = 'host', enabled = 0 WHERE match_on = 'host_path';
UPDATE redirects SET match_on = 'path', enabled = 0 WHERE match_on = 'simple';
ALTER TABLE redirects DROP COLUMN match_from;
ALTER TABLE redirects DROP COLUMN match_prefix;
ALTER TABLE redirects DROP COLUMN position;
