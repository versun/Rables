# frozen_string_literal: true

require "application_system_test_case"

class AdminActivitiesTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
    @log = ActivityLog.log!(action: :created, target: :article, level: :info, title: "Test Activity")
  end

  test "viewing activity logs" do
    sign_in(@user)
    visit admin_activities_path

    assert_text "Activity"
    assert_text @log.action
    assert_text @log.target
    assert_text "title=\"Test Activity\""
  end
end
