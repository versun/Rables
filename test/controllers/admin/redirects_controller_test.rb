# frozen_string_literal: true

require "test_helper"

class Admin::RedirectsControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    sign_in(@user)
    @redirect = Redirect.create!(regex: "old-path", replacement: "/new-path", permanent: false, enabled: true)
  end

  test "admin redirects CRUD" do
    get admin_redirects_path
    assert_response :success

    get new_admin_redirect_path
    assert_response :success

    assert_difference "Redirect.count", 1 do
      post admin_redirects_path, params: {
        redirect: {
          regex: "^/from",
          replacement: "/to",
          permanent: true,
          enabled: true
        }
      }
    end
    assert_redirected_to admin_redirects_path

    post admin_redirects_path, params: {
      redirect: {
        regex: "(",
        replacement: "/bad",
        permanent: false,
        enabled: true
      }
    }
    assert_response :unprocessable_entity

    get edit_admin_redirect_path(@redirect)
    assert_response :success

    patch admin_redirect_path(@redirect), params: {
      redirect: { replacement: "/updated" }
    }
    assert_redirected_to admin_redirects_path
    assert_equal "/updated", @redirect.reload.replacement

    patch admin_redirect_path(@redirect), params: {
      redirect: { regex: "(" }
    }
    assert_response :unprocessable_entity

    assert_difference "Redirect.count", -1 do
      delete admin_redirect_path(@redirect)
    end
    assert_redirected_to admin_redirects_path
  end
end
