# frozen_string_literal: true

require "test_helper"

class SessionTest < ActiveSupport::TestCase
  test "requires a user association" do
    session = Session.new(user: users(:admin))
    assert session.valid?

    session.user = nil
    assert_not session.valid?
    assert_includes session.errors[:user], "must exist"
  end
end
