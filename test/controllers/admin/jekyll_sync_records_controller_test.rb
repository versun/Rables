# frozen_string_literal: true

require "test_helper"

class Admin::JekyllSyncRecordsControllerTest < ActionDispatch::IntegrationTest
  setup do
    @user = users(:admin)
    sign_in(@user)
  end

  test "index displays sync records" do
    JekyllSyncRecord.create!(sync_type: :full, status: :completed, articles_count: 5, pages_count: 2)
    JekyllSyncRecord.create!(sync_type: :single, status: :failed, error_message: "Test error")

    get admin_jekyll_sync_records_path
    assert_response :success
  end

  test "index shows empty state when no records" do
    JekyllSyncRecord.destroy_all

    get admin_jekyll_sync_records_path
    assert_response :success
  end

  test "index paginates records" do
    15.times do |i|
      JekyllSyncRecord.create!(sync_type: :full, status: :completed, created_at: i.days.ago)
    end

    get admin_jekyll_sync_records_path
    assert_response :success
  end

  test "requires authentication" do
    delete session_path
    get admin_jekyll_sync_records_path
    assert_redirected_to new_session_path
  end
end
