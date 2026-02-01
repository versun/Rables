# frozen_string_literal: true

require "test_helper"

class JekyllSyncJobTest < ActiveSupport::TestCase
  setup do
    @setting = JekyllSetting.instance
    @temp_dir = Dir.mktmpdir("jekyll_test")
    @setting.update!(
      jekyll_path: @temp_dir,
      repository_type: "local",
      redirect_export_format: "netlify",
      comments_format: "yaml"
    )
  end

  teardown do
    FileUtils.rm_rf(@temp_dir) if @temp_dir && File.exist?(@temp_dir)
  end

  test "performs full sync when configured" do
    # Just verify the job runs without error when configured
    assert_nothing_raised do
      JekyllSyncJob.perform_now("full", "manual")
    end
  end

  test "skips sync when not configured" do
    @setting.update!(jekyll_path: nil)

    # Should not raise an error, just skip
    assert_nothing_raised do
      JekyllSyncJob.perform_now("full", "manual")
    end
  end

  test "handles unknown sync type" do
    # Should not raise an error for unknown sync type
    assert_nothing_raised do
      JekyllSyncJob.perform_now("unknown", "manual")
    end
  end

  test "uses default parameters" do
    assert_nothing_raised do
      JekyllSyncJob.perform_now
    end
  end

  test "can be enqueued" do
    assert_enqueued_with(job: JekyllSyncJob, args: [ "full", "auto" ]) do
      JekyllSyncJob.perform_later("full", "auto")
    end
  end
end
