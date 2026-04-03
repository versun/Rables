# Twitter Archive Handle Sync Design

## Goal

When a Twitter archive ZIP is imported, followers and following should continue to render immediately using `Account ID`. After import succeeds, a background task should gradually fetch handles for imported account IDs using the existing X crosspost credentials and store them locally. Once a handle is available, the public archive should display that handle as the link text while keeping the existing link URL unchanged.

## Current State

- `TwitterArchiveConnection` stores `account_id`, `relationship_type`, and `user_link`.
- The public archive followers/following view currently renders a single linked `Account ID` string per row.
- Twitter/X credentials already exist in `Crosspost.twitter` and are used by `TwitterService`.
- `TwitterApi::RateLimiter` provides per-service request spacing and 429 retry behavior, but there is no user lookup method yet.

## Recommended Approach

Add a nullable persisted handle field to `twitter_archive_connections`, queue a dedicated background job after a successful archive import, batch unresolved account IDs through the existing X client, and write returned handles back onto matching rows. The public archive view should prefer the stored handle for link text and fall back to `Account ID` until a handle has been resolved.

This keeps the archive page fast, avoids real-time X API lookups, and makes the sync process resumable and rate-limit aware.

## Scope

### In Scope

- Persist a resolved handle for each follower/following row.
- Automatically enqueue handle sync after a successful archive import.
- Use existing X crosspost credentials for lookup.
- Batch requests conservatively and retry on rate limiting.
- Keep `user_link` unchanged.
- Show handle text when present; otherwise show `Account ID`.

### Out of Scope

- Rebuilding `user_link` to `https://x.com/<handle>`.
- Real-time lookup during page rendering.
- Fetching display names, avatars, or other profile metadata.
- Adding a manual admin sync button.
- Cross-process global rate-limit coordination beyond the single sync job flow.

## Data Model

### `twitter_archive_connections`

Add a nullable `screen_name` column.

Rationale:

- The codebase already uses `screen_name` for tweet records.
- The view can use one stable persisted field instead of parsing from `user_link`.
- Existing rows remain valid and continue to fall back to `Account ID`.

### Model Behavior

`TwitterArchiveConnection#screen_name` should become a real attribute-backed value. Any URL parsing helper should move to a separate method only if it remains needed for backward compatibility outside the public archive display path. The public archive should read only the persisted value when deciding whether to show a handle or `Account ID`.

Import must not populate `screen_name` from `user_link`. Newly imported rows should always begin with a blank `screen_name` and continue to show `Account ID` until the background sync writes a resolved handle.

## Background Sync Flow

### Trigger

After `TwitterArchiveImportJob` finishes a successful archive import, enqueue a new job dedicated to handle synchronization.

### Preconditions

The sync job should exit cleanly without raising if:

- X crosspost is disabled.
- Any required X credential is missing.
- There are no archive connections with a blank `screen_name`.

This prevents import success from being treated as a failure just because handle sync is unavailable.

### Batch Selection

- Select only `TwitterArchiveConnection` rows with blank `screen_name`.
- Group by `account_id` so the same ID is never requested twice in one run.
- Request IDs in fixed-size batches.

Recommended batch size: `100` IDs per request.

### Lookup Request

Add a new lookup method in `TwitterService` that calls the X API users-by-IDs endpoint using the existing `X::Client` plus `TwitterApi::RateLimiter`.

The method should:

- Accept an array of account IDs.
- Request `username` for those IDs.
- Return a mapping of `account_id => username`.
- Return an empty mapping on non-rate-limit lookup failures.
- Surface persistent rate-limit exhaustion to the job after limiter retries are exhausted.

### Persistence

For each returned mapping:

- Update all matching `TwitterArchiveConnection` rows for that `account_id`.
- Write the returned username into `screen_name`.
- Leave unmatched IDs untouched so they continue to render as `Account ID`.

### Rate-Limit Behavior

The sync job should remain conservative:

- Use the existing rate limiter for request spacing.
- Reuse 429 retry behavior.
- Process batches sequentially in one job execution.
- Stop early if a rate-limit failure persists past retries.
- Re-enqueue itself once with a delayed run at the limiter reset time (or `15.minutes.from_now` when no reset time is available) if unresolved rows still remain.

This avoids turning import into a long blocking task and keeps API usage predictable.

## Failure Handling

### Import Path

Archive import completion must not depend on handle sync success. The import should remain completed even if the follow-up sync job later fails or is skipped.

### Sync Path

- Missing credentials: skip quietly and log a warning/activity.
- Partial API response: persist only returned handles.
- 429 after retries: stop the current run and self-requeue once at the next reset window if unresolved rows remain.
- Invalid or suspended IDs: leave `screen_name` blank.
- Other lookup failures: leave the current batch unresolved and continue to rely on `Account ID`.

The archive page must always remain usable with fallback text.

## UI Behavior

### Public Archive

Followers/following rows keep the current unordered-list structure and existing `user_link`.

Display rules:

- If `screen_name` is present, show `@screen_name` as the link text.
- If `screen_name` is blank, show `Account ID: <id>` as the link text.
- If `user_link` is blank, render the same text without a link.

### Admin Archive Page

No UI change is required for the first version. The import history remains focused on archive import status, not on background handle resolution progress.

## Architecture Changes

### New/Changed Responsibilities

- `TwitterArchiveConnection`
  - Stores resolved `screen_name`.
- `TwitterService`
  - Gains a batch user lookup method for account IDs.
- `TwitterArchiveImportJob`
  - Enqueues handle sync after successful import.
- New handle sync job
  - Orchestrates unresolved ID selection, batched lookup, and persistence.

## Testing Strategy

### Migration / Model

- Verify `screen_name` can be stored and read.
- Verify unresolved rows remain valid.

### Service

- Add tests for user lookup success returning an ID-to-handle mapping.
- Add tests for empty input.
- Add tests for rate-limited or failed lookup returning no updates.

### Job

- Verify archive import enqueues handle sync after success.
- Verify sync job skips when credentials are unavailable.
- Verify sync job batches unresolved IDs and updates all matching rows.
- Verify unresolved IDs remain untouched when the API omits them.

### Public Page

- Verify unresolved rows still show linked `Account ID`.
- Verify resolved rows show linked `@handle`.
- Verify the link target remains the original `user_link`.

## File Impact

### Create

- `app/jobs/twitter_archive_handle_sync_job.rb`
- `db/migrate/*_add_screen_name_to_twitter_archive_connections.rb`
- `test/jobs/twitter_archive_handle_sync_job_test.rb`

### Modify

- `app/models/twitter_archive_connection.rb`
- `app/services/twitter_service.rb`
- `app/services/twitter_api/rate_limiter.rb` if user lookup defaults need explicit support
- `app/jobs/twitter_archive_import_job.rb`
- `app/views/twitter_archives/show.html.erb`
- `test/jobs/twitter_archive_import_job_test.rb`
- `test/services/twitter_service_test.rb`
- `test/system/twitter_archives_test.rb`
- `test/controllers/twitter_archives_controller_test.rb`

## Tradeoffs

### Why store the handle instead of deriving it from the link

Many imported links are `intent/user?user_id=...`, which do not contain a usable handle. Persisting the looked-up value is the only stable way to improve the display text without rewriting links.

### Why async instead of during import

Import should finish quickly and deterministically from the ZIP contents alone. X API lookups are slower, rate-limited, and can fail independently, so they belong in a follow-up job.

### Why keep the original link

The requirement is to improve only the display text. Leaving the original `user_link` unchanged avoids rewriting imported data and minimizes scope.
