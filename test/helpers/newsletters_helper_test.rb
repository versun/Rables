# frozen_string_literal: true

require "test_helper"

class NewslettersHelperTest < ActionView::TestCase
  test "includes newsletters helper" do
    assert_includes self.class.included_modules, NewslettersHelper
  end
end
