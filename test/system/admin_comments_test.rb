# frozen_string_literal: true

require "application_system_test_case"

class AdminCommentsTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
  end

  test "admin comments list shows slug when title is blank" do
    article = Article.create!(
      title: nil,
      slug: "untitled-post",
      description: "Untitled description",
      status: :publish,
      content_type: :html,
      html_content: "<p>Body</p>"
    )
    comment = Comment.create!(
      commentable: article,
      author_name: "Alice",
      content: "Comment for slug display"
    )

    sign_in(@user)
    visit admin_comments_path

    within find("tr", text: comment.content) do
      assert_text "untitled-post"
    end
  end
end
