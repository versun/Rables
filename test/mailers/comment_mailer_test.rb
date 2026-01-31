# frozen_string_literal: true

require "test_helper"

class CommentMailerTest < ActionMailer::TestCase
  test "reply notification email includes english content and link" do
    article = articles(:published_article)
    parent = Comment.create!(
      commentable: article,
      author_name: "Parent",
      author_email: "parent@example.com",
      content: "Original comment"
    )
    reply = Comment.create!(
      commentable: article,
      author_name: "Child",
      content: "Reply content",
      parent: parent,
      status: :approved
    )

    email = CommentMailer.reply_notification(reply, CacheableSettings.site_info)

    assert_equal [ "parent@example.com" ], email.to
    assert_includes email.subject, "New reply to your comment"
    assert_includes email.subject, "Test Site"
    assert email.text_part
    assert email.html_part

    text_body = email.text_part.body.to_s
    html_body = email.html_part.body.to_s

    assert_includes text_body, "Reply:"
    assert_includes text_body, "Reply content"
    assert_includes text_body, article.title

    assert_includes html_body, "New reply to your comment"
    assert_includes html_body, "Reply content"
    assert_includes html_body, article.title
  end
end
