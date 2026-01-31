# frozen_string_literal: true

require "application_system_test_case"

class AdminNewsletterTabsTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
  end

  test "newsletter settings tabs show the right sections" do
    sign_in(@user)

    visit admin_newsletter_path
    assert_selector ".status-tab.active", text: "General"
    assert_text "Select Email Service Provider"
    assert_button "Save General Settings"

    click_link "Native Email"
    assert_selector ".status-tab.active", text: "Native Email"
    assert_text "SMTP Configuration"
    assert_button "Save Native Email Settings"

    click_link "Listmonk"
    assert_selector ".status-tab.active", text: "Listmonk"
    assert_text "Listmonk Configuration"
    assert_selector "select#list-select"
    assert_selector "select#template-select"
  end
end
