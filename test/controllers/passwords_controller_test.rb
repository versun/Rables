# frozen_string_literal: true

require "test_helper"

class PasswordsControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
  end

  # Simulate the rate limit counter already exceeding the threshold
  def with_rate_limit_count(count)
    cache = Rails.cache
    cache.define_singleton_method(:increment) { |*| count }
    yield
  ensure
    cache.singleton_class.send(:remove_method, :increment)
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

    token = @user.password_reset_token

    get edit_password_path(token)
    assert_response :success

    patch password_path(token), params: { password: "mismatch", password_confirmation: "nope" }
    assert_redirected_to edit_password_path(token)

    patch password_path(token), params: { password: "newpassword", password_confirmation: "newpassword" }
    assert_redirected_to new_session_path
    assert @user.reload.authenticate("newpassword")
  end

  test "invalid reset token redirects to new password page" do
    get edit_password_path("invalid-token")
    assert_redirected_to new_password_path

    patch password_path("invalid-token"), params: { password: "newpassword", password_confirmation: "newpassword" }
    assert_redirected_to new_password_path
    refute @user.reload.authenticate("newpassword")
  end

  test "rate limits password reset requests per ip" do
    assert_no_enqueued_jobs do
      with_rate_limit_count(6) do
        post passwords_path, params: { user_name: @user.user_name }
      end
    end

    assert_redirected_to new_password_path
    assert_match "try again later", flash[:alert]
  end
end
