# frozen_string_literal: true

require "test_helper"

class AdminHelperTest < ActionView::TestCase
  test "pending_comments_count memoizes pending count" do
    initial_count = Comment.pending.count

    assert_equal initial_count, pending_comments_count

    Comment.create!(
      commentable: articles(:published_article),
      author_name: "New Author",
      content: "New pending comment"
    )

    assert_equal initial_count, pending_comments_count
  end
end
