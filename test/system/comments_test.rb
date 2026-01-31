# frozen_string_literal: true

require "application_system_test_case"

class CommentsTest < ApplicationSystemTestCase
  def solve_comment_captcha
    question_text = find("[data-math-captcha-target='question']", visible: :all).text
    match = question_text.match(/(\d+)\s*([+-])\s*(\d+)/)
    raise "Could not parse captcha question: #{question_text.inspect}" unless match

    a = match[1].to_i
    op = match[2]
    b = match[3].to_i
    expected = op == "+" ? (a + b) : (a - b)

    find("input[name='captcha[answer]']", visible: true).set(expected.to_s)
  end

  test "submitting a comment on article" do
    article = create_published_article(title: "Commentable Article", content: "Commentable content", comment: true)

    visit article_path(article)
    fill_in "Name *", with: "Commenter"
    fill_in "Email (optional, for reply notifications)", with: "commenter@example.com"
    fill_in "Comment *", with: "This is a test comment"
    solve_comment_captcha
    click_button "Submit"

    assert_text "Your comment will be reviewed before being published."
    assert Comment.exists?(author_name: "Commenter", commentable: article)
  end

  test "invalid captcha shows error" do
    article = create_published_article(title: "Captcha Article", content: "Body", comment: true)

    visit article_path(article)
    fill_in "Name *", with: "Wrong Captcha"
    fill_in "Comment *", with: "Captcha failure"
    find("input[name='captcha[answer]']", visible: true).set("999")
    click_button "Submit"

    assert_text "验证失败：请回答数学题。"
  end
end
