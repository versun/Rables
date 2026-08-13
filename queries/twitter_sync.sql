-- Twitter sync (plan section 4.9): singleton config (twitter_syncs, id = 1)
-- plus the cursor writes the syncer performs. GetTwitterSync lives in
-- jobs.sql.

-- TwitterSync.instance (first_or_create).
-- name: EnsureTwitterSync :exec
INSERT OR IGNORE INTO twitter_syncs (id, created_at, updated_at) VALUES (1, ?, ?);

-- Admin update: overlay of the permitted twitter_sync params only. The cursor
-- fields (since_id/last_synced_at/last_error) and user_id belong to the
-- syncer: writing back the values read before this statement would roll back
-- a concurrent SetTwitterSyncSuccess/SetTwitterSyncUserID, so the admin
-- clears them with the separate conditional writes below instead.
-- name: UpdateTwitterSyncConfig :exec
UPDATE twitter_syncs SET
  enabled = ?, username = ?, start_date = ?, sync_schedule = ?, updated_at = ?
WHERE id = 1;

-- Username or start_date changed: the cursor no longer matches the config.
-- name: ResetTwitterSyncCursor :exec
UPDATE twitter_syncs SET
  since_id = NULL, last_synced_at = NULL, last_error = NULL, updated_at = ?
WHERE id = 1;

-- resolve_user_id: persist the users/by/username lookup; the admin update
-- passes NULL to clear it when the username changes. The username guard makes
-- the write conditional (CAS): if the admin changed the username while the
-- lookup was in flight, the row no longer matches and the resolved (now
-- stale) user_id is dropped instead of clobbering the admin's clear.
-- name: SetTwitterSyncUserID :execrows
UPDATE twitter_syncs SET user_id = :user_id, updated_at = :updated_at
WHERE id = 1 AND username IS :expected_username;

-- Successful run: advance the cursor, stamp last_synced_at, clear last_error.
-- The config/cursor values read at run start guard the write (CAS, IS is the
-- null-safe comparison): if the admin changed username or start_date mid-run,
-- ResetTwitterSyncCursor already cleared the cursor and this write would
-- silently undo the reset, skipping the backfill the reset was meant to
-- trigger. The guard turns the late write into a no-op instead; the tweets
-- this run archived stay (slug-deduped) and the next run syncs from the new
-- config.
-- name: SetTwitterSyncSuccess :execrows
UPDATE twitter_syncs SET since_id = :since_id, last_synced_at = :last_synced_at, last_error = NULL, updated_at = :updated_at
WHERE id = 1 AND since_id IS :expected_since_id AND username IS :expected_username AND start_date IS :expected_start_date;

-- Failed run: only last_error changes (update_columns(last_error:) semantics).
-- name: SetTwitterSyncFailure :exec
UPDATE twitter_syncs SET last_error = ?, updated_at = ? WHERE id = 1;
