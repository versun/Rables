# frozen_string_literal: true

require "test_helper"

class Admin::SettingsControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    sign_in(@user)
    @setting = Setting.first_or_create
  end

  test "edit and update settings" do
    get edit_admin_setting_path
    assert_response :success

    patch admin_setting_path, params: {
      setting: {
        title: "Updated Title",
        url: "https://example.com"
      }
    }
    assert_redirected_to admin_root_path
    assert_equal "Updated Title", @setting.reload.title

    patch admin_setting_path, params: {
      setting: {
        social_links_json: "{"
      }
    }
    assert_response :success
  end
end
