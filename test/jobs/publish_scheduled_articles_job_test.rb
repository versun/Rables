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

  # Jekyll sync integration tests
  test "triggers jekyll sync when sync_on_publish is enabled" do
    setting = JekyllSetting.instance
    temp_dir = Dir.mktmpdir("jekyll_test")
    setting.update!(
      jekyll_path: temp_dir,
      repository_type: "local",
      sync_on_publish: true,
      redirect_export_format: "netlify",
      comments_format: "yaml"
    )

    article = Article.create!(
      title: "Scheduled Jekyll Test",
      slug: "scheduled-jekyll-test-#{Time.current.to_i}",
      status: :schedule,
      scheduled_at: 1.hour.ago,
      content_type: :html,
      html_content: "<p>Test content</p>"
    )

    assert_enqueued_with(job: JekyllSingleSyncJob) do
      PublishScheduledArticlesJob.perform_now(article.id)
    end

    FileUtils.rm_rf(temp_dir)
  end

  test "does not trigger jekyll sync when sync_on_publish is disabled" do
    setting = JekyllSetting.instance
    temp_dir = Dir.mktmpdir("jekyll_test")
    setting.update!(
      jekyll_path: temp_dir,
      repository_type: "local",
      sync_on_publish: false,
      redirect_export_format: "netlify",
      comments_format: "yaml"
    )

    article = Article.create!(
      title: "Scheduled No Sync Test",
      slug: "scheduled-no-sync-test-#{Time.current.to_i}",
      status: :schedule,
      scheduled_at: 1.hour.ago,
      content_type: :html,
      html_content: "<p>Test content</p>"
    )

    # Should not enqueue JekyllSingleSyncJob
    assert_no_enqueued_jobs(only: JekyllSingleSyncJob) do
      PublishScheduledArticlesJob.perform_now(article.id)
    end

    FileUtils.rm_rf(temp_dir)
  end
end
