# frozen_string_literal: true

require "test_helper"

class CommentTest < ActiveSupport::TestCase
  def setup
    @article = articles(:published_article)
    @comment = Comment.new(
      commentable: @article,
      author_name: "Test User",
      content: "Test comment content"
    )
  end

  test "should be valid with valid attributes" do
    assert @comment.valid?, "Comment should be valid: #{@comment.errors.full_messages.join(', ')}"
  end

  test "should require author_name" do
    @comment.author_name = nil
    assert_not @comment.valid?
    assert_includes @comment.errors[:author_name], "can't be blank"
  end

  test "should require content" do
    @comment.content = nil
    assert_not @comment.valid?
    assert_includes @comment.errors[:content], "can't be blank"
  end

  test "should require commentable_id" do
    @comment.commentable_id = nil
    assert_not @comment.valid?
    assert_includes @comment.errors[:commentable_id], "can't be blank"
  end

  test "should require platform for external comments" do
    @comment.platform = "mastodon"
    @comment.external_id = "123"
    # Platform is required if external_comment?
    assert @comment.valid?, "Comment should be valid with platform and external_id"
  end

  test "should require external_id for external comments" do
    @comment.platform = "mastodon"
    @comment.external_id = nil
    assert_not @comment.valid?
    assert_includes @comment.errors[:external_id], "can't be blank"
  end

  test "should require unique external_id per commentable and platform" do
    existing_comment = comments(:mastodon_comment)
    @comment.platform = existing_comment.platform
    @comment.external_id = existing_comment.external_id
    @comment.commentable = existing_comment.commentable

    assert_not @comment.valid?
    assert_includes @comment.errors[:external_id], "has already been taken"
  end

  test "should validate author_url format" do
    @comment.author_url = "not-a-url"
    assert_not @comment.valid?
  end

  test "should validate url format" do
    @comment.url = "not-a-url"
    assert_not @comment.valid?
    assert_includes @comment.errors[:url], "must be a valid URL"
  end

  test "should accept valid url" do
    @comment.url = "https://example.com/comment/1"
    assert @comment.valid?
  end

  test "should allow blank url" do
    @comment.url = ""
    assert @comment.valid?
  end

  test "should accept valid URLs" do
    @comment.author_url = "https://example.com"
    assert @comment.valid?
  end

  test "should allow blank author_url" do
    @comment.author_url = ""
    assert @comment.valid?
  end

  test "should validate author_email format" do
    @comment.author_email = "not-an-email"
    assert_not @comment.valid?
    assert_includes @comment.errors[:author_email], "must be a valid email"
  end

  test "should allow blank author_email" do
    @comment.author_email = ""
    assert @comment.valid?
  end

  test "enqueues reply notification when approved" do
    parent_comment = Comment.create!(
      commentable: @article,
      author_name: "Parent",
      author_email: "parent@example.com",
      content: "Parent comment"
    )
    reply = Comment.create!(
      commentable: @article,
      author_name: "Reply",
      content: "Reply comment",
      parent: parent_comment
    )

    assert_enqueued_with(job: CommentReplyNotificationJob, args: [ reply.id ]) do
      reply.update!(status: :approved)
    end
  end

  test "does not enqueue reply notification for self-reply" do
    parent_comment = Comment.create!(
      commentable: @article,
      author_name: "Parent",
      author_email: "same@example.com",
      content: "Parent comment"
    )
    reply = Comment.create!(
      commentable: @article,
      author_name: "Reply",
      author_email: "same@example.com",
      content: "Reply comment",
      parent: parent_comment
    )

    assert_no_enqueued_jobs do
      reply.update!(status: :approved)
    end
  end

  test "should validate parent belongs to same commentable" do
    article1 = create_published_article
    article2 = create_published_article

    parent_comment = Comment.create!(
      commentable: article1,
      author_name: "Parent",
      content: "Parent comment"
    )

    @comment.commentable = article2
    @comment.parent_id = parent_comment.id

    assert_not @comment.valid?
    assert_includes @comment.errors[:parent_id], "must belong to the same Article"
  end

  test "should allow parent from same commentable" do
    parent_comment = Comment.create!(
      commentable: @article,
      author_name: "Parent",
      content: "Parent comment"
    )

    @comment.parent_id = parent_comment.id
    assert @comment.valid?
  end

  test "display_commentable falls back to parent commentable" do
    parent_comment = Comment.create!(
      commentable: @article,
      author_name: "Parent",
      content: "Parent comment"
    )

    reply = Comment.new(
      parent: parent_comment,
      author_name: "Reply",
      content: "Reply comment"
    )

    assert_equal parent_comment.commentable, reply.display_commentable
  end

  test "local scope should return comments without platform" do
    local_comment = comments(:approved_comment)
    external_comment = comments(:mastodon_comment)

    local_comments = Comment.local
    assert_includes local_comments, local_comment
    assert_not_includes local_comments, external_comment
  end

  test "mastodon scope should return only mastodon comments" do
    mastodon_comment = comments(:mastodon_comment)
    local_comment = comments(:approved_comment)

    mastodon_comments = Comment.mastodon
    assert_includes mastodon_comments, mastodon_comment
    assert_not_includes mastodon_comments, local_comment
  end

  test "top_level scope should return comments without parent" do
    parent_comment = Comment.create!(
      commentable: @article,
      author_name: "Parent",
      content: "Parent comment"
    )

    reply_comment = Comment.create!(
      commentable: @article,
      author_name: "Reply",
      content: "Reply comment",
      parent: parent_comment
    )

    top_level = Comment.top_level
    assert_includes top_level, parent_comment
    assert_not_includes top_level, reply_comment
  end

  test "should have replies" do
    parent_comment = Comment.create!(
      commentable: @article,
      author_name: "Parent",
      content: "Parent comment"
    )

    reply = Comment.create!(
      commentable: @article,
      author_name: "Reply",
      content: "Reply comment",
      parent: parent_comment
    )

    assert_includes parent_comment.replies, reply
  end

  test "should default to pending status" do
    comment = Comment.new(
      commentable: @article,
      author_name: "Test",
      content: "Test"
    )
    assert_equal "pending", comment.status
  end

  test "should belong to commentable" do
    assert_respond_to @comment, :commentable
  end

  test "should belong to parent comment" do
    assert_respond_to @comment, :parent
  end

  test "should destroy replies when destroyed" do
    parent_comment = Comment.create!(
      commentable: @article,
      author_name: "Parent",
      content: "Parent comment"
    )

    Comment.create!(
      commentable: @article,
      author_name: "Reply",
      content: "Reply comment",
      parent: parent_comment
    )

    assert_difference "Comment.count", -2 do
      parent_comment.destroy
    end
  end

  test "upsert_from_external creates a new comment" do
    comment_data = {
      external_id: "ext-1",
      author_name: "External User",
      author_username: "external_user",
      author_avatar_url: "https://example.com/avatar.png",
      content: "External comment",
      published_at: Time.current,
      url: "https://example.com/comment/1"
    }

    comment, result = nil, nil
    assert_difference "Comment.count", 1 do
      comment, result = Comment.upsert_from_external(@article, "mastodon", comment_data)
    end

    assert_equal :created, result
    assert_equal "mastodon", comment.platform
    assert_equal "ext-1", comment.external_id
    assert_equal "External comment", comment.content
    assert_equal @article, comment.commentable
    assert_equal "pending", comment.status
  end

  test "upsert_from_external assigns status when given" do
    comment_data = {
      external_id: "ext-2",
      author_name: "External User",
      content: "External comment",
      published_at: Time.current
    }

    comment, result = Comment.upsert_from_external(@article, "bluesky", comment_data, status: :approved)

    assert_equal :created, result
    assert_equal "approved", comment.status
  end

  test "upsert_from_external keeps existing comment status on update" do
    existing = comments(:mastodon_comment)
    existing.update!(status: :rejected)

    comment, result = Comment.upsert_from_external(
      existing.commentable,
      existing.platform,
      {
        external_id: existing.external_id,
        author_name: existing.author_name,
        content: "Updated content",
        published_at: existing.published_at,
        url: existing.url
      },
      status: :approved
    )

    assert_equal :updated, result
    assert_equal "rejected", comment.reload.status
  end

  test "upsert_from_external updates an existing comment" do
    existing = comments(:mastodon_comment)

    comment, result = nil, nil
    assert_no_difference "Comment.count" do
      comment, result = Comment.upsert_from_external(
        existing.commentable,
        existing.platform,
        {
          external_id: existing.external_id,
          author_name: existing.author_name,
          content: "Updated content",
          published_at: existing.published_at,
          url: existing.url
        }
      )
    end

    assert_equal :updated, result
    assert_equal existing.id, comment.id
    assert_equal "Updated content", comment.reload.content
  end

  test "upsert_from_external returns unchanged when nothing changed" do
    existing = comments(:mastodon_comment)

    comment, result = Comment.upsert_from_external(
      existing.commentable,
      existing.platform,
      {
        external_id: existing.external_id,
        author_name: existing.author_name,
        author_username: existing.author_username,
        author_avatar_url: existing.author_avatar_url,
        content: existing.content,
        published_at: existing.published_at,
        url: existing.url
      }
    )

    assert_equal :unchanged, result
    assert_equal existing.id, comment.id
  end
end
