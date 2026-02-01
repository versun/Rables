# frozen_string_literal: true

require "test_helper"

class JekyllSyncJobTest < ActiveJob::TestCase
  test "sync job records completion" do
    dir = Dir.mktmpdir
    JekyllSetting.create!(jekyll_path: dir)
    create_published_article(title: "Job Article", slug: "job-article")

    assert_difference "JekyllSyncRecord.count", 1 do
      JekyllSyncJob.perform_now
    end

    record = JekyllSyncRecord.order(:created_at).last
    assert_equal "completed", record.status
    assert_equal Article.count, record.articles_count
  ensure
    FileUtils.remove_entry(dir) if dir
  end
end
