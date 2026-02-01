# frozen_string_literal: true

require "test_helper"

class JekyllSingleSyncJobTest < ActiveSupport::TestCase
  setup do
    @setting = JekyllSetting.instance
    @temp_dir = Dir.mktmpdir("jekyll_test")
    @setting.update!(
      jekyll_path: @temp_dir,
      repository_type: "local",
      sync_on_publish: true,
      redirect_export_format: "netlify",
      comments_format: "yaml"
    )
    @article = Article.create!(
      title: "Test Article",
      slug: "test-article-#{Time.current.to_i}",
      description: "Test description",
      status: :publish,
      content_type: :html,
      html_content: "<p>Test content</p>"
    )
    @page = Page.create!(
      title: "Test Page",
      slug: "test-page-#{Time.current.to_i}",
      status: :publish,
      content_type: :html,
      html_content: "<p>Test content</p>"
    )
  end

  teardown do
    FileUtils.rm_rf(@temp_dir) if @temp_dir && File.exist?(@temp_dir)
  end

  test "syncs published article without error" do
    assert_nothing_raised do
      JekyllSingleSyncJob.perform_now("Article", @article.id, "publish")
    end
  end

  test "handles unpublished article" do
    @article.update!(status: :draft)
    assert_nothing_raised do
      JekyllSingleSyncJob.perform_now("Article", @article.id, "publish")
    end
  end

  test "syncs published page without error" do
    assert_nothing_raised do
      JekyllSingleSyncJob.perform_now("Page", @page.id, "publish")
    end
  end

  test "handles unpublished page" do
    @page.update!(status: :draft)
    assert_nothing_raised do
      JekyllSingleSyncJob.perform_now("Page", @page.id, "publish")
    end
  end

  test "skips when not configured" do
    @setting.update!(jekyll_path: nil)
    assert_nothing_raised do
      JekyllSingleSyncJob.perform_now("Article", @article.id, "publish")
    end
  end

  test "skips when sync_on_publish is disabled" do
    @setting.update!(sync_on_publish: false)
    assert_nothing_raised do
      JekyllSingleSyncJob.perform_now("Article", @article.id, "publish")
    end
  end

  test "skips for nonexistent article" do
    assert_nothing_raised do
      JekyllSingleSyncJob.perform_now("Article", -1, "publish")
    end
  end

  test "skips for nonexistent page" do
    assert_nothing_raised do
      JekyllSingleSyncJob.perform_now("Page", -1, "publish")
    end
  end

  test "handles unknown record type" do
    assert_nothing_raised do
      JekyllSingleSyncJob.perform_now("Unknown", 1, "publish")
    end
  end

  test "can be enqueued" do
    assert_enqueued_with(job: JekyllSingleSyncJob, args: [ "Article", @article.id, "manual" ]) do
      JekyllSingleSyncJob.perform_later("Article", @article.id, "manual")
    end
  end
end
