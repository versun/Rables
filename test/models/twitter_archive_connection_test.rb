# frozen_string_literal: true

require "test_helper"

class TwitterArchiveConnectionTest < ActiveSupport::TestCase
  test "persists screen_name separately from user_link" do
    assert_includes TwitterArchiveConnection.attribute_names, "screen_name"

    connection = TwitterArchiveConnection.create!(
      account_id: "900",
      relationship_type: "follower",
      user_link: "https://twitter.com/intent/user?user_id=900",
      screen_name: "resolved_handle"
    )

    assert_equal "resolved_handle", connection.reload.screen_name
  end
end
