# frozen_string_literal: true

require "test_helper"

class Admin::ActivitiesControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    sign_in(@user)
  end

  test "index lists recent activity logs" do
    ActivityLog.create!(
      action: "created",
      target: "article",
      level: :info,
      description: "Created article from test"
    )

    get admin_activities_path
    assert_response :success
    assert_includes response.body, "Created article from test"
  end
end
