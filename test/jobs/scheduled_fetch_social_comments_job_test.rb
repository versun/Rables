# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class ScheduledFetchSocialCommentsJobTest < ActiveJob::TestCase
  test "skips when no enabled platforms" do
    Crosspost.for("mastodon").update!(enabled: false, auto_fetch_comments: false, comment_fetch_schedule: "daily")
    Crosspost.for("bluesky").update!(enabled: false, auto_fetch_comments: false, comment_fetch_schedule: "daily")

    notifier = RecordingNotifier.new

    assert_no_enqueued_jobs do
      with_event_notifier(notifier) { ScheduledFetchSocialCommentsJob.perform_now }
    end

    assert notifier.events.any? { |name, payload| name == "scheduled_fetch_social_comments_job.skipped" && payload[:reason] == "no_enabled_platforms" }
  end

  test "skips when schedule is not due" do
    Crosspost.for("mastodon").update!(enabled: true, auto_fetch_comments: true, comment_fetch_schedule: "daily")

    notifier = RecordingNotifier.new

    Rails.cache.stub(:read, Time.current) do
      assert_no_enqueued_jobs do
        with_event_notifier(notifier) { ScheduledFetchSocialCommentsJob.perform_now }
      end
    end

    assert notifier.events.any? { |name, payload| name == "scheduled_fetch_social_comments_job.skipped" && payload[:reason] == "not_time_yet" }
  end

  test "enqueues fetch job when schedule is due" do
    Crosspost.for("mastodon").update!(enabled: true, auto_fetch_comments: true, comment_fetch_schedule: "daily")

    Rails.cache.stub(:read, 2.days.ago) do
      Rails.cache.stub(:write, true) do
        assert_enqueued_with(job: FetchSocialCommentsJob) do
          ScheduledFetchSocialCommentsJob.perform_now
        end
      end
    end
  end

  test "unknown schedule logs warning and skips" do
    Crosspost.for("mastodon").update!(enabled: true, auto_fetch_comments: true, comment_fetch_schedule: "hourly")

    notifier = RecordingNotifier.new

    Rails.cache.stub(:read, Time.current) do
      assert_no_enqueued_jobs do
        with_event_notifier(notifier) { ScheduledFetchSocialCommentsJob.perform_now }
      end
    end

    assert notifier.events.any? { |name, payload| name == "scheduled_fetch_social_comments_job.unknown_schedule" && payload[:schedule] == "hourly" }
  end

  test "each platform follows its own schedule" do
    Crosspost.for("mastodon").update!(enabled: true, auto_fetch_comments: true, comment_fetch_schedule: "daily")
    Crosspost.for("bluesky").update!(enabled: true, auto_fetch_comments: true, comment_fetch_schedule: "monthly")

    # Mastodon fetched 2 days ago (daily -> due), bluesky just fetched (monthly -> not due)
    read_stub = ->(key, *) { key.to_s.end_with?(":mastodon") ? 2.days.ago : Time.current }

    Rails.cache.stub(:read, read_stub) do
      Rails.cache.stub(:write, true) do
        assert_enqueued_jobs 1, only: FetchSocialCommentsJob do
          ScheduledFetchSocialCommentsJob.perform_now
        end
      end
    end

    assert_enqueued_with(job: FetchSocialCommentsJob, args: [ "mastodon" ])
  end

  test "enqueues each due platform separately" do
    Crosspost.for("mastodon").update!(enabled: true, auto_fetch_comments: true, comment_fetch_schedule: "daily")
    Crosspost.for("bluesky").update!(enabled: true, auto_fetch_comments: true, comment_fetch_schedule: "weekly")

    # Neither platform fetched recently -> both are due
    Rails.cache.stub(:read, nil) do
      Rails.cache.stub(:write, true) do
        assert_enqueued_jobs 2, only: FetchSocialCommentsJob do
          ScheduledFetchSocialCommentsJob.perform_now
        end
      end
    end

    assert_enqueued_with(job: FetchSocialCommentsJob, args: [ "mastodon" ])
    assert_enqueued_with(job: FetchSocialCommentsJob, args: [ "bluesky" ])
  end

  private

  def with_event_notifier(notifier)
    original_event = Rails.event
    Rails.define_singleton_method(:event) { notifier }
    yield
  ensure
    Rails.define_singleton_method(:event) { original_event }
  end
end
