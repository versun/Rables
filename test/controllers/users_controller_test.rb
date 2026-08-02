# frozen_string_literal: true

require "test_helper"

class UsersControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
  end

  test "new create edit and update user" do
    get new_user_path
    assert_redirected_to root_path

    assert_no_difference "User.count" do
      post users_path, params: {
        user: {
          user_name: "newuser",
          password: "password123",
          password_confirmation: "password123"
        }
      }
    end
    assert_redirected_to root_path

    sign_in(@user)
    get edit_user_path(@user)
    assert_response :success

    patch user_path(@user), params: { user: { user_name: "updateduser" } }
    assert_redirected_to admin_articles_path
    assert_equal "updateduser", @user.reload.user_name

    patch user_path(@user), params: { user: { user_name: "" } }
    assert_response :unprocessable_entity
  end

  test "new renders form when no users exist" do
    User.delete_all

    get new_user_path
    assert_redirected_to setup_path
  end

  test "update password requires current password" do
    sign_in(@user)

    patch user_path(@user), params: { user: { password: "newpassword", password_confirmation: "newpassword" } }
    assert_response :unprocessable_entity
    assert @user.reload.authenticate("password123")

    patch user_path(@user), params: { user: { current_password: "wrong-password", password: "newpassword", password_confirmation: "newpassword" } }
    assert_response :unprocessable_entity
    assert @user.reload.authenticate("password123")

    patch user_path(@user), params: { user: { current_password: "password123", password: "newpassword", password_confirmation: "newpassword" } }
    assert_redirected_to admin_articles_path
    assert @user.reload.authenticate("newpassword")
  end

  test "update user name does not require current password" do
    sign_in(@user)

    patch user_path(@user), params: { user: { user_name: "renamed" } }
    assert_redirected_to admin_articles_path
    assert_equal "renamed", @user.reload.user_name
  end

  test "update shows success as notice not alert" do
    sign_in(@user)

    patch user_path(@user), params: { user: { user_name: "renamed" } }
    follow_redirect!
    assert_equal "Account was successfully updated.", flash[:notice]
    assert_nil flash[:alert]
  end

  test "update without user params returns bad request instead of 500" do
    sign_in(@user)

    patch user_path(@user), params: {}
    assert_response :bad_request
  end

  test "create with invalid params returns unprocessable entity" do
    User.delete_all
    Setting.first_or_create.update!(setup_completed: true)

    # Reachable through the 5-minute setup_incomplete? cache window: the cache
    # can briefly report "setup complete" after the last user was deleted,
    # letting the request through to the check-and-create guard.
    original_cache = Rails.cache
    Rails.cache = ActiveSupport::Cache.lookup_store(:memory_store)
    Rails.cache.write(Setting::SETUP_INCOMPLETE_CACHE_KEY, false)

    assert_no_difference "User.count" do
      post users_path, params: {
        user: {
          user_name: "",
          password: "password123",
          password_confirmation: "password123"
        }
      }
    end
    assert_response :unprocessable_entity
  ensure
    Rails.cache = original_cache
  end
end
