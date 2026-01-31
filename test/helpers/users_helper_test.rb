# frozen_string_literal: true

require "test_helper"

class UsersHelperTest < ActionView::TestCase
  test "includes users helper" do
    assert_includes self.class.included_modules, UsersHelper
  end
end
