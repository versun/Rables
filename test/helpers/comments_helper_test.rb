# frozen_string_literal: true

require "test_helper"

class CommentsHelperTest < ActionView::TestCase
  include CommentsHelper

  setup do
    @article = articles(:published_article)
  end

  test "grouped_comment_items returns approved local top-level comments first" do
    items = grouped_comment_items(@article)

    local_items = items.select { |item| item[:type] == "local" }
    assert_includes local_items.map { |item| item[:data] }, comments(:approved_comment)
    assert_not_includes local_items.map { |item| item[:data] }, comments(:pending_comment)
  end

  test "grouped_comment_items groups external comments by platform" do
    items = grouped_comment_items(@article)

    mastodon_items = items.select { |item| item[:type] == "mastodon" }
    assert_includes mastodon_items.map { |item| item[:data] }, comments(:mastodon_comment)
  end

  test "grouped_comment_items excludes replies" do
    parent = comments(:approved_comment)
    reply = Comment.create!(
      commentable: @article,
      author_name: "Reply",
      content: "Reply comment",
      status: :approved,
      parent: parent
    )

    items = grouped_comment_items(@article)

    assert_not_includes items.map { |item| item[:data] }, reply
  end

  test "visible_comments_count counts approved local and external comments" do
    # published_article has 1 approved local, 1 pending local and 1 mastodon comment
    assert_equal 2, visible_comments_count(@article)
  end
end
