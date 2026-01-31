# frozen_string_literal: true

require "application_system_test_case"

class PasswordsTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
  end

  test "requesting password reset" do
    visit new_password_path

    fill_in "user_name", with: @user.user_name
    click_button "Email reset instructions"

    assert_current_path new_session_path
    assert_text "Password reset instructions sent"
  end

  test "updating password when signed in" do
    sign_in(@user)
    visit edit_password_path(@user.id)

    fill_in "password", with: "newpassword123"
    fill_in "password_confirmation", with: "newpassword123"
    click_button "Save"

    assert_current_path new_session_path
    assert_text "Password has been reset."
  end

  test "password mismatch shows error" do
    sign_in(@user)
    visit edit_password_path(@user.id)

    fill_in "password", with: "newpassword123"
    fill_in "password_confirmation", with: "mismatch"
    click_button "Save"

    assert_text "Passwords did not match."
  end
end
