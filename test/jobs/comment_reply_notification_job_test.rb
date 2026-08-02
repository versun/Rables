# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class CommentReplyNotificationJobTest < ActiveJob::TestCase
  def setup
    super
    NewsletterSetting.instance.update!(
      enabled: true,
      provider: "native",
      smtp_address: "smtp.example.com",
      smtp_port: 587,
      smtp_user_name: "user",
      smtp_password: "password",
      from_email: "noreply@example.com"
    )
  end

  test "sends email for approved reply with parent email" do
    reply = create_reply

    delivery_args = nil
    mail = Minitest::Mock.new
    mail.expect(:delivery_method, nil) { |*args| delivery_args = args }
    mail.expect(:deliver_now, true)

    CommentMailer.stub(:reply_notification, mail) do
      assert_difference "ActivityLog.count", 1 do
        CommentReplyNotificationJob.perform_now(reply.id)
      end
    end

    mail.verify
    assert_equal :smtp, delivery_args.first
    assert_equal "smtp.example.com", delivery_args.last[:address]
  end

  test "skips email for self-reply" do
    reply = create_reply(parent_email: "same@example.com", author_email: "same@example.com")

    assert_no_difference "ActivityLog.count" do
      CommentReplyNotificationJob.perform_now(reply.id)
    end
  end

  test "logs no failure when delivery succeeds after a transient retry" do
    reply = create_reply

    attempts = 0
    mail = build_mail do
      attempts += 1
      raise Net::SMTPServerBusy, "busy" if attempts == 1
    end

    CommentMailer.stub(:reply_notification, mail) do
      assert_no_difference -> { failed_logs.count } do
        assert_difference -> { sent_logs.count }, 1 do
          perform_enqueued_jobs { CommentReplyNotificationJob.perform_later(reply.id) }
        end
      end
    end

    assert_equal 2, attempts
  end

  test "logs a single failure when retries are exhausted" do
    reply = create_reply

    attempts = 0
    mail = build_mail do
      attempts += 1
      raise Net::SMTPServerBusy, "busy"
    end

    CommentMailer.stub(:reply_notification, mail) do
      assert_difference -> { failed_logs.count }, 1 do
        perform_enqueued_jobs { CommentReplyNotificationJob.perform_later(reply.id) }
      end
    end

    assert_equal 3, attempts
  end

  test "logs a single failure for non-retried errors" do
    reply = create_reply

    mail = build_mail { raise "boom" }

    CommentMailer.stub(:reply_notification, mail) do
      assert_difference -> { failed_logs.count }, 1 do
        assert_raises(RuntimeError) { CommentReplyNotificationJob.perform_now(reply.id) }
      end
    end
  end

  private

  def create_reply(parent_email: "parent@example.com", author_email: nil)
    article = articles(:published_article)
    parent = Comment.create!(
      commentable: article,
      author_name: "Parent",
      author_email: parent_email,
      content: "Parent content"
    )
    Comment.create!(
      commentable: article,
      author_name: "Child",
      author_email: author_email,
      content: "Reply content",
      parent: parent,
      status: :approved
    )
  end

  def build_mail(&delivery)
    mail = Object.new
    mail.define_singleton_method(:delivery_method) { |*| }
    mail.define_singleton_method(:deliver_now, &delivery)
    mail
  end

  def sent_logs
    ActivityLog.where(action: "sent", target: "comment_reply_notification")
  end

  def failed_logs
    ActivityLog.where(action: "failed", target: "comment_reply_notification")
  end
end
