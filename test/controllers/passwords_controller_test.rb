# frozen_string_literal: true

require "test_helper"

class PasswordsControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
  end

  test "password reset requests and updates" do
    get new_password_path
    assert_response :success

    assert_enqueued_jobs 1 do
      post passwords_path, params: { user_name: @user.user_name }
    end
    assert_redirected_to new_session_path

    assert_enqueued_jobs 0 do
      post passwords_path, params: { user_name: "unknown" }
    end
    assert_redirected_to new_session_path

    get edit_password_path(1)
    assert_redirected_to new_session_path

    sign_in(@user)
    get edit_password_path(1)
    assert_response :success

    patch password_path(1), params: { password: "mismatch", password_confirmation: "nope" }
    assert_redirected_to edit_password_path(1)

    patch password_path(1), params: { password: "newpassword", password_confirmation: "newpassword" }
    assert_redirected_to new_session_path
    assert @user.reload.authenticate("newpassword")
  end
end
