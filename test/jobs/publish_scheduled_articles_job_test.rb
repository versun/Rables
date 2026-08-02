# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class PublishScheduledArticlesJobTest < ActiveJob::TestCase
  test "publishes scheduled article when time has passed" do
    article = Article.create!(
      title: "Scheduled Test",
      slug: "scheduled-test-#{Time.current.to_i}",
      status: :schedule,
      scheduled_at: 1.hour.ago,
      content_type: :html,
      html_content: "<p>Test content</p>"
    )

    PublishScheduledArticlesJob.perform_now(article.id)

    article.reload
    assert article.publish?
    assert_nil article.scheduled_at
  end

  test "does not publish article scheduled for future" do
    article = Article.create!(
      title: "Future Scheduled",
      slug: "future-scheduled-#{Time.current.to_i}",
      status: :schedule,
      scheduled_at: 1.hour.from_now,
      content_type: :html,
      html_content: "<p>Test content</p>"
    )

    PublishScheduledArticlesJob.perform_now(article.id)

    article.reload
    assert article.schedule?
    assert_not_nil article.scheduled_at
  end

  test "handles missing article gracefully" do
    assert_nothing_raised do
      PublishScheduledArticlesJob.perform_now(999999)
    end
  end

  test "schedule_at creates job for scheduled article" do
    article = Article.create!(
      title: "To Schedule",
      slug: "to-schedule-#{Time.current.to_i}",
      status: :schedule,
      scheduled_at: 1.hour.from_now,
      content_type: :html,
      html_content: "<p>Test content</p>"
    )

    assert_enqueued_with(job: PublishScheduledArticlesJob) do
      PublishScheduledArticlesJob.schedule_at(article)
    end
  end

  test "schedule_at does nothing for non-scheduled article" do
    article = create_published_article

    assert_no_enqueued_jobs do
      PublishScheduledArticlesJob.schedule_at(article)
    end
  end

  test "schedule_at does nothing when scheduled_at is nil" do
    article = Article.create!(
      title: "No Schedule Time",
      slug: "no-schedule-time-#{Time.current.to_i}",
      status: :draft,
      content_type: :html,
      html_content: "<p>Test content</p>"
    )
    # Force status and nil scheduled_at without validation
    article.update_columns(status: Article.statuses[:schedule], scheduled_at: nil)

    assert_no_enqueued_jobs do
      PublishScheduledArticlesJob.schedule_at(article)
    end
  end

  test "skips publishing when scheduled_at is nil" do
    article = Article.create!(
      title: "Nil Schedule Time",
      slug: "nil-schedule-time-#{Time.current.to_i}",
      status: :draft,
      content_type: :html,
      html_content: "<p>Test content</p>"
    )
    # Force a stale state: scheduled status without a scheduled_at time
    article.update_columns(status: Article.statuses[:schedule], scheduled_at: nil)

    notifier = RecordingNotifier.new

    assert_nothing_raised do
      with_event_notifier(notifier) { PublishScheduledArticlesJob.perform_now(article.id) }
    end

    article.reload
    assert article.schedule?
    assert notifier.events.any? { |name, payload| name == "publish_scheduled_articles_job.skipped" && payload[:reason] == "scheduled_at_missing" }
  end

  test "cancel_old_jobs is a no-op when the adapter cannot list jobs" do
    # The test adapter has no mission_control-jobs querying support
    assert_nothing_raised do
      PublishScheduledArticlesJob.cancel_old_jobs(123)
    end
  end

  test "cancel_old_jobs discards scheduled jobs matching the article id" do
    article_id = 123

    fake_job_class = Struct.new(:arguments, :discarded) do
      def discard
        self.discarded = true
      end
    end

    # Job proxies expose deserialized positional arguments, e.g. [ 123 ]
    matching_job = fake_job_class.new([ article_id ], false)
    other_job = fake_job_class.new([ 999 ], false)

    fake_job_set = Class.new do
      def initialize(jobs)
        @jobs = jobs
      end

      def scheduled
        self
      end

      def where(job_class_name:)
        @jobs
      end
    end.new([ matching_job, other_job ])

    fake_adapter = Object.new
    def fake_adapter.fetch_jobs(*)
      []
    end

    ActiveJob::Base.stub(:queue_adapter, fake_adapter) do
      ActiveJob::Base.stub(:jobs, fake_job_set) do
        PublishScheduledArticlesJob.cancel_old_jobs(article_id)
      end
    end

    assert_equal true, matching_job.discarded
    assert_equal false, other_job.discarded
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
