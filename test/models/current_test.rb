# frozen_string_literal: true

require "test_helper"

class CurrentTest < ActiveSupport::TestCase
  test "delegates user through session and allows nil" do
    Current.session = Session.new(user: users(:admin))
    assert_equal users(:admin), Current.user

    Current.session = nil
    assert_nil Current.user
  ensure
    Current.reset
  end
end
