# frozen_string_literal: true

require "test_helper"

class SessionsControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
  end

  test "should get new" do
    get new_session_path
    assert_response :success
  end

  test "should create session with valid credentials" do
    post session_path, params: {
      user_name: @user.user_name,
      password: "password123"
    }

    # After login, redirects to admin (after_authentication_url)
    assert_response :redirect
  end

  test "should not create session with invalid username" do
    post session_path, params: {
      user_name: "nonexistent",
      password: "password123"
    }

    # Failed login redirects to new_session_path with flash alert
    assert_redirected_to new_session_path
  end

  test "should not create session with invalid password" do
    post session_path, params: {
      user_name: @user.user_name,
      password: "wrongpassword"
    }

    # Failed login redirects to new_session_path with flash alert
    assert_redirected_to new_session_path
  end

  test "should destroy session" do
    sign_in(@user)

    delete session_path
    assert_redirected_to root_path
  end
end
