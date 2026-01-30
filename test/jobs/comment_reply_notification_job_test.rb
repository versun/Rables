require "test_helper"

class CommentReplyNotificationJobTest < ActiveJob::TestCase
  include ActionMailer::TestHelper
  def setup
    super
    # Reset ActionMailer to test delivery method
    ActionMailer::Base.delivery_method = :test
    ActionMailer::Base.deliveries.clear

    # Enable native newsletter but don't configure SMTP
    # The job checks Rails.env.test? and skips SMTP configuration
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
    article = articles(:published_article)
    parent = Comment.create!(
      commentable: article,
      author_name: "Parent",
      author_email: "parent@example.com",
      content: "Parent content"
    )
    reply = Comment.create!(
      commentable: article,
      author_name: "Child",
      content: "Reply content",
      parent: parent,
      status: :approved
    )

    assert_emails 1 do
      CommentReplyNotificationJob.perform_now(reply.id)
    end

    delivered = ActionMailer::Base.deliveries.last
    assert_equal [ "parent@example.com" ], delivered.to
  end

  test "skips email for self-reply" do
    article = articles(:published_article)
    parent = Comment.create!(
      commentable: article,
      author_name: "Parent",
      author_email: "same@example.com",
      content: "Parent content"
    )
    reply = Comment.create!(
      commentable: article,
      author_name: "Child",
      author_email: "same@example.com",
      content: "Reply content",
      parent: parent,
      status: :approved
    )

    assert_emails 0 do
      CommentReplyNotificationJob.perform_now(reply.id)
    end
  end
end
