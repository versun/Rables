# frozen_string_literal: true

require "test_helper"

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
end
