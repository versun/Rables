# frozen_string_literal: true

require "test_helper"

class AnalyticsHelperTest < ActionView::TestCase
  test "includes analytics helper" do
    assert_includes self.class.included_modules, AnalyticsHelper
  end
end
