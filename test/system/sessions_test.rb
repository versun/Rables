# frozen_string_literal: true

require "application_system_test_case"

class SessionsTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
  end

  test "signing in with valid credentials" do
    visit new_session_path

    fill_in "user_name", with: @user.user_name
    fill_in "password", with: "password123"
    click_button "Sign in"

    assert_current_path admin_root_path
    assert_text "Logout"
  end

  test "signing in with invalid credentials" do
    visit new_session_path

    fill_in "user_name", with: @user.user_name
    fill_in "password", with: "wrongpassword"
    click_button "Sign in"

    assert_text "Try another username or password."
  end

  test "signing out" do
    sign_in(@user)
    visit admin_root_path

    # Click the logout link/button in the page
    click_link "Logout"

    assert_current_path root_path
  end
end
