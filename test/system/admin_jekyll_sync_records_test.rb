# frozen_string_literal: true

require "application_system_test_case"

class AdminJekyllSyncRecordsTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
    @setting = JekyllSetting.instance
    @temp_dir = Dir.mktmpdir("jekyll_test")
    @setting.update!(
      jekyll_path: @temp_dir,
      repository_type: "local",
      redirect_export_format: "netlify",
      comments_format: "yaml"
    )
  end

  def teardown
    FileUtils.rm_rf(@temp_dir) if @temp_dir && File.exist?(@temp_dir)
  end

  test "viewing sync records index" do
    # Create some sync records
    JekyllSyncRecord.create!(
      sync_type: :full,
      status: :completed,
      articles_count: 5,
      pages_count: 2,
      started_at: 2.hours.ago,
      completed_at: 2.hours.ago + 30.seconds,
      triggered_by: "manual"
    )

    JekyllSyncRecord.create!(
      sync_type: :single,
      status: :completed,
      articles_count: 1,
      pages_count: 0,
      started_at: 1.hour.ago,
      completed_at: 1.hour.ago + 5.seconds,
      triggered_by: "publish"
    )

    sign_in(@user)
    visit admin_jekyll_sync_records_path

    assert_text "Jekyll Sync History"
    assert_text "full"
    assert_text "single"
    assert_text "completed"
  end

  test "viewing empty sync records" do
    sign_in(@user)
    visit admin_jekyll_sync_records_path

    assert_text "Jekyll Sync History"
    assert_text "No sync records yet"
  end

  test "viewing failed sync record" do
    JekyllSyncRecord.create!(
      sync_type: :full,
      status: :failed,
      articles_count: 0,
      pages_count: 0,
      started_at: 1.hour.ago,
      error_message: "Git push failed: permission denied",
      triggered_by: "manual"
    )

    sign_in(@user)
    visit admin_jekyll_sync_records_path

    assert_text "failed"
    assert_text "Git push failed"
  end

  test "navigating back to jekyll settings" do
    sign_in(@user)
    visit admin_jekyll_sync_records_path

    assert_link "Back to Jekyll Settings"
    click_link "Back to Jekyll Settings"

    assert_current_path admin_jekyll_path
  end
end
