# Twitter Archive Handle Sync Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep followers/following immediately visible as `Account ID` after archive import, then asynchronously resolve and persist X handles using the existing crosspost credentials so the link text upgrades to `@handle` without changing the stored link URL.

**Architecture:** Add a nullable persisted `screen_name` on `twitter_archive_connections`, introduce a dedicated sync job plus a batch lookup method on `TwitterService`, and trigger sync both after successful imports and as a catch-up when the admin Twitter archive page sees unresolved legacy rows. Keep the archive page display logic simple: persisted handle first, `Account ID` fallback otherwise.

**Tech Stack:** Rails 8.1, Active Job with Solid Queue, SQLite, `x` gem, Minitest

---

### Task 1: Persist Resolved Handles

**Files:**
- Create: `db/migrate/*_add_screen_name_to_twitter_archive_connections.rb`
- Modify: `app/models/twitter_archive_connection.rb`
- Test: `test/models/twitter_archive_connection_test.rb`

- [ ] **Step 1: Write the failing model test**

```ruby
test "screen_name is persisted and does not fall back to parsing user_link" do
  connection = TwitterArchiveConnection.create!(
    account_id: "900",
    relationship_type: "follower",
    user_link: "https://twitter.com/intent/user?user_id=900",
    screen_name: "real_handle"
  )

  assert_equal "real_handle", connection.reload.screen_name
end
```

- [ ] **Step 2: Run test to verify it fails**

Run: `mise exec -- bin/rails test test/models/twitter_archive_connection_test.rb`
Expected: FAIL because `screen_name` column does not exist yet.

- [ ] **Step 3: Add the migration and minimal model changes**

```ruby
add_column :twitter_archive_connections, :screen_name, :string
add_index :twitter_archive_connections, :screen_name
```

Keep the model validations unchanged and remove any view-path dependency on parsing `user_link` for the public display decision.

- [ ] **Step 4: Run test to verify it passes**

Run: `mise exec -- bin/rails test test/models/twitter_archive_connection_test.rb`
Expected: PASS

### Task 2: Add Batch Lookup and Sync Job

**Files:**
- Create: `app/jobs/twitter_archive_handle_sync_job.rb`
- Modify: `app/services/twitter_service.rb`
- Modify: `app/services/twitter_api/rate_limiter.rb`
- Test: `test/services/twitter_service_test.rb`
- Test: `test/jobs/twitter_archive_handle_sync_job_test.rb`

- [ ] **Step 1: Write failing service tests for user lookup**

Add tests covering:

```ruby
test "lookup_users_by_ids returns account_id to username mapping"
test "lookup_users_by_ids returns empty hash for empty input"
test "lookup_users_by_ids surfaces rate limit exhaustion with reset_at"
test "lookup_users_by_ids returns empty hash on non-rate-limit failures"
```

- [ ] **Step 2: Write failing job tests**

Add tests covering:

```ruby
test "sync job skips when twitter credentials are unavailable"
test "sync job batches unresolved account ids and updates all matching rows"
test "sync job leaves missing ids unresolved"
test "sync job re-enqueues itself at reset time after persistent rate limiting"
test "sync job does not enqueue duplicate delayed retries for the same run"
```

- [ ] **Step 3: Run targeted tests to verify they fail**

Run: `mise exec -- bin/rails test test/services/twitter_service_test.rb test/jobs/twitter_archive_handle_sync_job_test.rb`
Expected: FAIL because lookup API and sync job do not exist yet.

- [ ] **Step 4: Implement the minimal lookup contract**

Add a method on `TwitterService` that:

```ruby
def lookup_users_by_ids(account_ids)
  return { users: {}, rate_limit: nil } if account_ids.blank?
  # call GET /2/users with ids and user.fields=username
end
```

Contract:
- success => `{ users: { "900" => "alice" }, rate_limit: nil }`
- persistent rate limit => `{ users: {}, rate_limit: { remaining: 0, reset_at: ... } }`
- other failures => `{ users: {}, rate_limit: nil }`

- [ ] **Step 5: Implement the sync job**

Behavior:
- exit cleanly when no unresolved rows or credentials are missing
- select distinct unresolved `account_id`s in slices of 100
- write returned usernames to all matching rows
- when `rate_limit[:reset_at]` is present, stop processing and schedule one delayed retry job unless a later pending sync job already exists

- [ ] **Step 6: Run targeted tests to verify they pass**

Run: `mise exec -- bin/rails test test/services/twitter_service_test.rb test/jobs/twitter_archive_handle_sync_job_test.rb`
Expected: PASS

### Task 3: Trigger Sync for New Imports and Legacy Rows

**Files:**
- Modify: `app/jobs/twitter_archive_import_job.rb`
- Modify: `app/controllers/admin/twitter_archives_controller.rb`
- Modify: `db/schema.rb`
- Test: `test/jobs/twitter_archive_import_job_test.rb`
- Test: `test/controllers/admin/twitter_archives_controller_test.rb`

- [ ] **Step 1: Write failing trigger tests**

Add tests covering:

```ruby
test "twitter archive import job enqueues handle sync after successful import"
test "admin twitter archive index enqueues catch-up sync for unresolved existing rows"
test "admin twitter archive index does not enqueue duplicate catch-up sync jobs"
```

- [ ] **Step 2: Run targeted tests to verify they fail**

Run: `mise exec -- bin/rails test test/jobs/twitter_archive_import_job_test.rb test/controllers/admin/twitter_archives_controller_test.rb`
Expected: FAIL because no sync trigger exists.

- [ ] **Step 3: Implement minimal trigger behavior**

Rules:
- after successful import, always call `TwitterArchiveHandleSyncJob.enqueue_if_needed`
- on admin archive index, call the same helper to catch up unresolved legacy rows
- helper must no-op when there are no blank `screen_name` rows
- helper must no-op when X credentials are disabled or incomplete
- helper must avoid scheduling duplicates for both immediate and delayed sync jobs by checking pending/scheduled jobs outside test mode

- [ ] **Step 4: Run targeted tests to verify they pass**

Run: `mise exec -- bin/rails test test/jobs/twitter_archive_import_job_test.rb test/controllers/admin/twitter_archives_controller_test.rb`
Expected: PASS

### Task 4: Update Public Archive Display

**Files:**
- Modify: `app/views/twitter_archives/show.html.erb`
- Test: `test/controllers/twitter_archives_controller_test.rb`
- Test: `test/system/twitter_archives_test.rb`

- [ ] **Step 1: Write failing display tests**

Add assertions for:

```ruby
resolved row => link text "@resolved_handle", href unchanged
unresolved row => link text "Account ID: 901", href unchanged
blank user_link => plain text only
```

- [ ] **Step 2: Run targeted tests to verify they fail**

Run: `mise exec -- bin/rails test test/controllers/twitter_archives_controller_test.rb test/system/twitter_archives_test.rb`
Expected: FAIL because the page always renders `Account ID`.

- [ ] **Step 3: Implement the minimal view change**

Render:

```erb
label = connection.screen_name.present? ? "@#{connection.screen_name}" : "Account ID: #{connection.account_id}"
```

Keep unordered-list structure and keep `user_link` unchanged.

- [ ] **Step 4: Run targeted tests to verify they pass**

Run: `mise exec -- bin/rails test test/controllers/twitter_archives_controller_test.rb test/system/twitter_archives_test.rb`
Expected: PASS

### Task 5: Full Verification

**Files:**
- Modify: `db/schema.rb`
- Test: `test/models/twitter_archive_connection_test.rb`
- Test: `test/services/twitter_service_test.rb`
- Test: `test/jobs/twitter_archive_handle_sync_job_test.rb`
- Test: `test/jobs/twitter_archive_import_job_test.rb`
- Test: `test/controllers/admin/twitter_archives_controller_test.rb`
- Test: `test/controllers/twitter_archives_controller_test.rb`
- Test: `test/system/twitter_archives_test.rb`

- [ ] **Step 1: Run the full Twitter archive-related test set**

Run: `mise exec -- bin/rails test $(rg --files test | rg 'twitter_archive|twitter_service_test|twitter_archive_connection_test')`
Expected: all pass

- [ ] **Step 2: Spot-check migration + schema**

Run: `mise exec -- bin/rails db:migrate:status | rg twitter_archive`
Expected: new migration is `up` in the local environment after applying it

- [ ] **Step 3: Confirm tracked schema changed**

Run: `git diff -- db/schema.rb`
Expected: includes the new `screen_name` column and index for `twitter_archive_connections`

- [ ] **Step 4: Review diff for scope**

Run: `git diff -- app jobs db test docs/superpowers`
Expected: changes limited to archive handle sync, no unrelated refactors
