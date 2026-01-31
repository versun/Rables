# frozen_string_literal: true

require "test_helper"

class ArticlesHelperTest < ActionView::TestCase
  test "includes articles helper" do
    assert_includes self.class.included_modules, ArticlesHelper
  end
end
