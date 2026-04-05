# frozen_string_literal: true

require "test_helper"

class TwitterArchiveHandleSyncJobTest < ActiveJob::TestCase
  private

  def fake_job_relation(any_result)
    Struct.new(:any_result) do
      def where(job_class_name:)
        self
      end

      def any?
        any_result
      end
    end.new(any_result)
  end

  def with_production_job_statuses(pending: false, scheduled: false, in_progress: false)
    fake_jobs = Struct.new(:pending, :scheduled, :in_progress).new(
      fake_job_relation(pending),
      fake_job_relation(scheduled),
      fake_job_relation(in_progress)
    )

    original_env = Rails.method(:env)
    original_jobs = ActiveJob::Base.method(:jobs)
    Rails.define_singleton_method(:env) { ActiveSupport::StringInquirer.new("production") }
    ActiveJob::Base.define_singleton_method(:jobs) { fake_jobs }
    yield
  ensure
    Rails.define_singleton_method(:env) { original_env.call }
    ActiveJob::Base.define_singleton_method(:jobs) { original_jobs.call }
  end

  def with_stubbed_twitter_service(service)
    original = TwitterService.method(:new)
    TwitterService.define_singleton_method(:new) { service }
    yield
  ensure
    TwitterService.define_singleton_method(:new) { original.call }
  end

  public

  test "enqueue_if_needed does not enqueue when the only matching job is in progress" do
    assert Object.const_defined?(:TwitterArchiveHandleSyncJob), "TwitterArchiveHandleSyncJob is not defined"
    assert_includes TwitterArchiveConnection.attribute_names, "screen_name"

    Crosspost.twitter.update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )
    TwitterArchiveConnection.create!(
      account_id: "900",
      relationship_type: "follower",
      user_link: "https://twitter.com/intent/user?user_id=900"
    )

    retry_time = 15.minutes.from_now.change(usec: 0)

    with_production_job_statuses(in_progress: true) do
      assert_no_enqueued_jobs only: TwitterArchiveHandleSyncJob do
        TwitterArchiveHandleSyncJob.enqueue_if_needed(wait_until: retry_time, retry_scheduled: true)
      end
    end
  end

  test "sync job skips when twitter credentials are unavailable" do
    assert Object.const_defined?(:TwitterArchiveHandleSyncJob), "TwitterArchiveHandleSyncJob is not defined"
    assert_includes TwitterArchiveConnection.attribute_names, "screen_name"

    Crosspost.twitter.update!(
      enabled: false,
      api_key: nil,
      api_key_secret: nil,
      access_token: nil,
      access_token_secret: nil
    )
    TwitterArchiveConnection.create!(
      account_id: "900",
      relationship_type: "follower",
      user_link: "https://twitter.com/intent/user?user_id=900"
    )

    assert_no_changes -> { TwitterArchiveConnection.where.not(screen_name: [ nil, "" ]).count } do
      TwitterArchiveHandleSyncJob.perform_now
    end
  end

  test "sync job batches unresolved account ids and updates all matching rows" do
    assert Object.const_defined?(:TwitterArchiveHandleSyncJob), "TwitterArchiveHandleSyncJob is not defined"
    assert_includes TwitterArchiveConnection.attribute_names, "screen_name"
    Crosspost.twitter.update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    101.times do |index|
      TwitterArchiveConnection.create!(
        account_id: (1000 + index).to_s,
        relationship_type: index.even? ? "follower" : "following",
        user_link: "https://twitter.com/intent/user?user_id=#{1000 + index}"
      )
    end
    TwitterArchiveConnection.create!(
      account_id: "1000",
      relationship_type: "following",
      user_link: "https://twitter.com/intent/user?user_id=1000"
    )

    lookup_calls = []
    fake_service = Object.new
    fake_service.define_singleton_method(:lookup_users_by_ids) do |account_ids|
      lookup_calls << account_ids
      {
        users: account_ids.index_with { |account_id| "handle_#{account_id}" },
        rate_limit: nil
      }
    end

    with_stubbed_twitter_service(fake_service) do
      TwitterArchiveHandleSyncJob.perform_now
    end

    assert_equal [ 100, 1 ], lookup_calls.map(&:length)
    assert_equal "handle_1000", TwitterArchiveConnection.find_by!(account_id: "1000", relationship_type: "follower").screen_name
    assert_equal "handle_1000", TwitterArchiveConnection.find_by!(account_id: "1000", relationship_type: "following").screen_name
    assert_equal "handle_1100", TwitterArchiveConnection.find_by!(account_id: "1100").screen_name
  end

  test "sync job leaves missing ids unresolved" do
    assert Object.const_defined?(:TwitterArchiveHandleSyncJob), "TwitterArchiveHandleSyncJob is not defined"
    assert_includes TwitterArchiveConnection.attribute_names, "screen_name"
    Crosspost.twitter.update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    TwitterArchiveConnection.create!(
      account_id: "900",
      relationship_type: "follower",
      user_link: "https://twitter.com/intent/user?user_id=900"
    )
    TwitterArchiveConnection.create!(
      account_id: "901",
      relationship_type: "following",
      user_link: "https://twitter.com/intent/user?user_id=901"
    )

    fake_service = Object.new
    fake_service.define_singleton_method(:lookup_users_by_ids) do |_account_ids|
      { users: { "900" => "resolved_900" }, rate_limit: nil }
    end

    with_stubbed_twitter_service(fake_service) do
      TwitterArchiveHandleSyncJob.perform_now
    end

    assert_equal "resolved_900", TwitterArchiveConnection.find_by!(account_id: "900").screen_name
    assert_nil TwitterArchiveConnection.find_by!(account_id: "901").screen_name
  end

  test "sync job re-enqueues itself at reset time after persistent rate limiting" do
    assert Object.const_defined?(:TwitterArchiveHandleSyncJob), "TwitterArchiveHandleSyncJob is not defined"
    assert_includes TwitterArchiveConnection.attribute_names, "screen_name"
    Crosspost.twitter.update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    TwitterArchiveConnection.create!(
      account_id: "900",
      relationship_type: "follower",
      user_link: "https://twitter.com/intent/user?user_id=900"
    )

    reset_time = 20.minutes.from_now.change(usec: 0)
    fake_service = Object.new
    fake_service.define_singleton_method(:lookup_users_by_ids) do |_account_ids|
      { users: {}, rate_limit: { limit: 300, remaining: 0, reset_at: reset_time } }
    end

    with_stubbed_twitter_service(fake_service) do
      assert_enqueued_with(job: TwitterArchiveHandleSyncJob, at: reset_time, args: [ { retry_scheduled: true } ]) do
        TwitterArchiveHandleSyncJob.perform_now
      end
    end
  end

  test "sync job still schedules a delayed retry while the current run is in progress" do
    assert Object.const_defined?(:TwitterArchiveHandleSyncJob), "TwitterArchiveHandleSyncJob is not defined"
    assert_includes TwitterArchiveConnection.attribute_names, "screen_name"
    Crosspost.twitter.update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    TwitterArchiveConnection.create!(
      account_id: "900",
      relationship_type: "follower",
      user_link: "https://twitter.com/intent/user?user_id=900"
    )

    reset_time = 20.minutes.from_now.change(usec: 0)
    fake_service = Object.new
    fake_service.define_singleton_method(:lookup_users_by_ids) do |_account_ids|
      { users: {}, rate_limit: { limit: 300, remaining: 0, reset_at: reset_time } }
    end

    with_stubbed_twitter_service(fake_service) do
      with_production_job_statuses(in_progress: true) do
        assert_enqueued_with(job: TwitterArchiveHandleSyncJob, at: reset_time, args: [ { retry_scheduled: true } ]) do
          TwitterArchiveHandleSyncJob.perform_now
        end
      end
    end
  end

  test "sync job re-enqueues itself again when a delayed retry is rate limited again" do
    assert Object.const_defined?(:TwitterArchiveHandleSyncJob), "TwitterArchiveHandleSyncJob is not defined"
    assert_includes TwitterArchiveConnection.attribute_names, "screen_name"
    Crosspost.twitter.update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    TwitterArchiveConnection.create!(
      account_id: "900",
      relationship_type: "follower",
      user_link: "https://twitter.com/intent/user?user_id=900"
    )

    reset_time = 20.minutes.from_now.change(usec: 0)
    fake_service = Object.new
    fake_service.define_singleton_method(:lookup_users_by_ids) do |_account_ids|
      { users: {}, rate_limit: { limit: 300, remaining: 0, reset_at: reset_time } }
    end

    with_stubbed_twitter_service(fake_service) do
      assert_enqueued_with(job: TwitterArchiveHandleSyncJob, at: reset_time, args: [ { retry_scheduled: true } ]) do
        TwitterArchiveHandleSyncJob.perform_now(retry_scheduled: true)
      end
    end
  end

  test "sync job re-enqueues itself after a transient lookup failure" do
    assert Object.const_defined?(:TwitterArchiveHandleSyncJob), "TwitterArchiveHandleSyncJob is not defined"
    assert_includes TwitterArchiveConnection.attribute_names, "screen_name"
    Crosspost.twitter.update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    TwitterArchiveConnection.create!(
      account_id: "900",
      relationship_type: "follower",
      user_link: "https://twitter.com/intent/user?user_id=900"
    )

    retry_time = 10.minutes.from_now.change(usec: 0)
    fake_service = Object.new
    fake_service.define_singleton_method(:lookup_users_by_ids) do |_account_ids|
      { users: {}, rate_limit: nil, retry_at: retry_time }
    end

    with_stubbed_twitter_service(fake_service) do
      assert_enqueued_with(job: TwitterArchiveHandleSyncJob, at: retry_time, args: [ { retry_scheduled: true } ]) do
        TwitterArchiveHandleSyncJob.perform_now
      end
    end
  end

  test "sync job stops and logs a failure after a permanent lookup error" do
    assert Object.const_defined?(:TwitterArchiveHandleSyncJob), "TwitterArchiveHandleSyncJob is not defined"
    assert_includes TwitterArchiveConnection.attribute_names, "screen_name"
    Crosspost.twitter.update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    101.times do |index|
      TwitterArchiveConnection.create!(
        account_id: (1000 + index).to_s,
        relationship_type: index.even? ? "follower" : "following",
        user_link: "https://twitter.com/intent/user?user_id=#{1000 + index}"
      )
    end

    lookup_calls = 0
    fake_service = Object.new
    fake_service.define_singleton_method(:lookup_users_by_ids) do |_account_ids|
      lookup_calls += 1
      { users: {}, rate_limit: nil, retry_at: nil, error_message: "401 Unauthorized" }
    end

    assert_difference -> { ActivityLog.where(target: "twitter_archive", action: "failed").count }, 1 do
      with_stubbed_twitter_service(fake_service) do
        assert_no_enqueued_jobs only: TwitterArchiveHandleSyncJob do
          TwitterArchiveHandleSyncJob.perform_now
        end
      end
    end

    assert_equal 1, lookup_calls
    log = ActivityLog.where(target: "twitter_archive", action: "failed").order(:created_at).last
    assert_includes log.description, 'error="401 Unauthorized"'
  end

  test "enqueue_if_needed avoids duplicate delayed retries" do
    assert Object.const_defined?(:TwitterArchiveHandleSyncJob), "TwitterArchiveHandleSyncJob is not defined"
    assert_includes TwitterArchiveConnection.attribute_names, "screen_name"
    Crosspost.twitter.update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    TwitterArchiveConnection.create!(
      account_id: "900",
      relationship_type: "follower",
      user_link: "https://twitter.com/intent/user?user_id=900"
    )

    reset_time = 15.minutes.from_now.change(usec: 0)

    assert_enqueued_jobs 1 do
      TwitterArchiveHandleSyncJob.enqueue_if_needed(wait_until: reset_time, retry_scheduled: true)
      TwitterArchiveHandleSyncJob.enqueue_if_needed(wait_until: reset_time, retry_scheduled: true)
    end
  end
end
