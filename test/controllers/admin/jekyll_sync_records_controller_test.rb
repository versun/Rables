# frozen_string_literal: true

require "test_helper"

class Admin::JekyllSyncRecordsControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    sign_in(@user)
  end

  test "lists sync records" do
    JekyllSyncRecord.create!(sync_type: "full", status: "completed")

    get admin_jekyll_sync_records_path
    assert_response :success
  end
end
