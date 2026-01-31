# frozen_string_literal: true

require "test_helper"

class Admin::CommentsControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    @pending_comment = comments(:pending_comment)
    @approved_comment = comments(:approved_comment)
    sign_in(@user)
  end

  test "admin comment workflows" do
    get admin_comments_path
    assert_response :success

    get admin_comments_path, params: { status: "approved" }
    assert_response :success

    get admin_comment_path(@pending_comment)
    assert_response :not_acceptable

    get edit_admin_comment_path(@pending_comment)
    assert_response :success

    patch admin_comment_path(@pending_comment), params: { comment: { content: "Updated content" } }
    assert_redirected_to admin_comments_path
    assert_equal "Updated content", @pending_comment.reload.content

    patch admin_comment_path(@pending_comment), params: { comment: { author_name: "" } }
    assert_response :unprocessable_entity

    patch approve_admin_comment_path(@pending_comment)
    assert_redirected_to admin_comments_path
    assert @pending_comment.reload.approved?

    patch reject_admin_comment_path(@approved_comment)
    assert_redirected_to admin_comments_path
    assert @approved_comment.reload.rejected?

    batch_comment = Comment.create!(
      commentable: articles(:published_article),
      author_name: "Batch User",
      content: "Batch content",
      status: :pending
    )

    post batch_approve_admin_comments_path, params: { ids: [ batch_comment.id ] }
    assert_redirected_to admin_comments_path
    assert batch_comment.reload.approved?

    post batch_reject_admin_comments_path, params: { ids: [ batch_comment.id ] }
    assert_redirected_to admin_comments_path
    assert batch_comment.reload.rejected?

    assert_difference "Comment.count", -1 do
      post batch_destroy_admin_comments_path, params: { ids: [ batch_comment.id ] }
    end
  end

  test "admin comments index shows newest first" do
    newer = Comment.create!(
      commentable: articles(:published_article),
      author_name: "Newest Author",
      content: "Newest comment content",
      published_at: Time.current
    )
    older = Comment.create!(
      commentable: articles(:published_article),
      author_name: "Older Author",
      content: "Older comment content",
      published_at: 3.days.ago
    )

    get admin_comments_path
    assert_response :success

    body = response.body
    newer_index = body.index(newer.content)
    older_index = body.index(older.content)
    assert newer_index, "Expected to find newer comment content in response"
    assert older_index, "Expected to find older comment content in response"
    assert_operator newer_index, :<, older_index
  end

  test "admin can reply to a local comment using settings" do
    parent_comment = comments(:approved_comment)
    setting = settings(:default)

    assert_difference "Comment.count", 1 do
      post reply_admin_comment_path(parent_comment), params: { comment: { content: "Admin reply content" } }
    end

    reply = Comment.order(:created_at).last
    assert_equal parent_comment, reply.parent
    assert_equal parent_comment.commentable, reply.commentable
    assert_equal setting.author, reply.author_name
    assert_equal setting.url, reply.author_url
    assert_equal "Admin reply content", reply.content
    assert reply.approved?
  end

  test "admin cannot reply to rejected comments" do
    parent_comment = comments(:approved_comment)
    parent_comment.update!(status: :rejected)

    assert_no_difference "Comment.count" do
      post reply_admin_comment_path(parent_comment), params: { comment: { content: "Admin reply content" } }
    end

    assert_redirected_to admin_comments_path
    assert_equal "Cannot reply to rejected comments.", flash[:alert]
  end
end
