# frozen_string_literal: true

require "test_helper"

class UsersControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
  end

  test "new create edit and update user" do
    get new_user_path
    assert_redirected_to root_path

    assert_difference "User.count", 1 do
      post users_path, params: {
        user: {
          user_name: "newuser",
          password: "password123",
          password_confirmation: "password123"
        }
      }
    end
    assert_redirected_to new_session_path

    post users_path, params: {
      user: {
        user_name: "",
        password: "password123",
        password_confirmation: "password123"
      }
    }
    assert_response :success

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
end
