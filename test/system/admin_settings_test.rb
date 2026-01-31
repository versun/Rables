# frozen_string_literal: true

require "application_system_test_case"

class AdminSettingsTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
    @setting = settings(:default)
  end

  test "viewing settings edit page" do
    sign_in(@user)
    visit edit_admin_setting_path

    assert_text "Site Information"
    assert_field "Site Title", with: @setting.title
    assert_field "Site Description", with: @setting.description
  end

  test "updating site settings" do
    sign_in(@user)
    visit edit_admin_setting_path

    fill_in "Site Title", with: "Updated Site Title"
    fill_in "Site Description", with: "Updated description"
    fill_in "Site URL", with: "https://example.com"
    fill_in "Social Links (JSON)", with: "{}"
    click_button "Save"

    assert_current_path admin_root_path
    assert_text "Setting was successfully updated."
    @setting.reload
    assert_equal "Updated Site Title", @setting.title
  end

  test "invalid social links json shows errors" do
    sign_in(@user)
    visit edit_admin_setting_path

    fill_in "Social Links (JSON)", with: "{"
    click_button "Save"

    assert_text "包含无效的 JSON 格式"
  end
end
