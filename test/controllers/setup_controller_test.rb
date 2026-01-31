# frozen_string_literal: true

require "test_helper"

class SetupControllerTest < ActionDispatch::IntegrationTest
  test "setup requires site url" do
    User.delete_all
    Setting.delete_all

    post setup_path, params: {
      user: {
        user_name: "admin",
        password: "password123",
        password_confirmation: "password123"
      },
      setting: {
        title: "Test Site",
        url: ""
      }
    }

    assert_response :unprocessable_entity
    assert_match(/Url can't be blank/, response.body)
  end

  test "setup creates user and settings when valid" do
    User.delete_all
    Setting.delete_all

    post setup_path, params: {
      user: {
        user_name: "admin",
        password: "password123",
        password_confirmation: "password123"
      },
      setting: {
        title: "Test Site",
        url: "https://example.com",
        time_zone: "UTC"
      }
    }

    assert_redirected_to new_session_path
    assert User.find_by(user_name: "admin")
    assert_equal true, Setting.first.setup_completed
  end

  test "show redirects when setup already completed" do
    Setting.first_or_create.update!(setup_completed: true, url: "https://example.com")

    get setup_path
    assert_redirected_to admin_root_path
  end

  test "setup handles invalid user" do
    User.delete_all
    Setting.delete_all

    post setup_path, params: {
      user: {
        user_name: "",
        password: "password123",
        password_confirmation: "password123"
      },
      setting: {
        title: "Test Site",
        url: "https://example.com",
        time_zone: "UTC"
      }
    }

    assert_response :unprocessable_entity
    assert_equal 0, User.count
  end

  test "show renders when setup incomplete" do
    User.delete_all
    Setting.delete_all

    get setup_path
    assert_response :success
    assert_select "form"
  end
end
