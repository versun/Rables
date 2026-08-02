# frozen_string_literal: true

require "test_helper"

class LexxySmokeTest < ActionDispatch::IntegrationTest
  def setup
    sign_in(users(:admin))
  end

  test "admin forms render lexxy editor" do
    get new_admin_page_path
    assert_response :success
    assert_includes response.body, "lexxy-editor"
    assert_includes response.body, 'name="page[content]"'

    get edit_admin_setting_path
    assert_response :success
    assert_includes response.body, "lexxy-editor"
    assert_includes response.body, 'name="setting[footer]"'
  end
end
