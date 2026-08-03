-- Twitter sync (plan section 4.9): singleton config (twitter_syncs, id = 1)
-- plus the cursor writes the syncer performs. GetTwitterSync lives in
-- jobs.sql.

-- TwitterSync.instance (first_or_create).
-- name: EnsureTwitterSync :exec
INSERT OR IGNORE INTO twitter_syncs (id, created_at, updated_at) VALUES (1, ?, ?);

-- Admin update: full overlay of the permitted twitter_sync params. The
-- handler pre-computes the cursor reset (username/start_date change clears
-- since_id/last_synced_at/last_error; username change also clears user_id).
-- name: UpdateTwitterSyncConfig :exec
UPDATE twitter_syncs SET
  enabled = ?, username = ?, user_id = ?, start_date = ?, sync_schedule = ?,
  since_id = ?, last_synced_at = ?, last_error = ?, updated_at = ?
WHERE id = 1;

-- resolve_user_id: persist the users/by/username lookup.
-- name: SetTwitterSyncUserID :exec
UPDATE twitter_syncs SET user_id = ?, updated_at = ? WHERE id = 1;

-- Successful run: advance the cursor, stamp last_synced_at, clear last_error.
-- name: SetTwitterSyncSuccess :exec
UPDATE twitter_syncs SET since_id = ?, last_synced_at = ?, last_error = NULL, updated_at = ?
WHERE id = 1;

-- Failed run: only last_error changes (update_columns(last_error:) semantics).
-- name: SetTwitterSyncFailure :exec
UPDATE twitter_syncs SET last_error = ?, updated_at = ? WHERE id = 1;
