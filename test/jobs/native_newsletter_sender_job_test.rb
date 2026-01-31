# frozen_string_literal: true

require "test_helper"

class NativeNewsletterSenderJobTest < ActiveJob::TestCase
  setup do
    @article = create_published_article
    @subscriber = subscribers(:confirmed_subscriber)

    NewsletterSetting.instance.update!(
      enabled: true,
      provider: "native",
      smtp_address: "smtp.example.com",
      smtp_port: 587,
      smtp_user_name: "user",
      smtp_password: "password",
      from_email: "newsletter@example.com"
    )
  end

  test "does nothing when newsletter is disabled" do
    NewsletterSetting.instance.update!(enabled: false)

    assert_no_difference "ActivityLog.count" do
      NativeNewsletterSenderJob.perform_now(@article.id)
    end
  end

  test "does nothing when provider is not native" do
    NewsletterSetting.instance.update!(provider: "listmonk")

    assert_no_difference "ActivityLog.count" do
      NativeNewsletterSenderJob.perform_now(@article.id)
    end
  end

  test "does nothing when not configured" do
    NewsletterSetting.instance.update!(smtp_address: nil)

    assert_no_difference "ActivityLog.count" do
      NativeNewsletterSenderJob.perform_now(@article.id)
    end
  end

  test "does nothing when no active subscribers" do
    Subscriber.update_all(confirmed_at: nil)

    assert_no_difference "ActivityLog.count" do
      NativeNewsletterSenderJob.perform_now(@article.id)
    end
  end

  test "does not send to unsubscribed subscribers" do
    # Unsubscribe all subscribers
    Subscriber.update_all(unsubscribed_at: Time.current)

    assert_no_difference "ActivityLog.count" do
      NativeNewsletterSenderJob.perform_now(@article.id)
    end
  end

  test "filters subscribers by tag subscription" do
    tag = tags(:tech_tag)
    @article.tags << tag

    # Create subscriber with tag subscription
    tagged_subscriber = Subscriber.create!(
      email: "tagged#{Time.current.to_i}@example.com",
      confirmation_token: SecureRandom.urlsafe_base64(32),
      unsubscribe_token: SecureRandom.urlsafe_base64(32),
      confirmed_at: Time.current
    )
    tagged_subscriber.tags << tag

    # Subscriber without tag subscription should not receive
    untagged_subscriber = Subscriber.create!(
      email: "untagged#{Time.current.to_i}@example.com",
      confirmation_token: SecureRandom.urlsafe_base64(32),
      unsubscribe_token: SecureRandom.urlsafe_base64(32),
      confirmed_at: Time.current
    )
    # Add a different tag to make them not "subscribed to all"
    other_tag = Tag.create!(name: "other-tag-#{Time.current.to_i}", slug: "other-tag-#{Time.current.to_i}")
    untagged_subscriber.tags << other_tag

    # Clear other subscribers
    Subscriber.where.not(id: [ tagged_subscriber.id, untagged_subscriber.id ]).update_all(confirmed_at: nil)

    # Job creates multiple logs: started, failed (per email due to SMTP error in test), completed
    # At minimum: started + failed + completed = 3 logs
    assert_difference "ActivityLog.count", 3, "Expected started, failed, and completed logs" do
      NativeNewsletterSenderJob.perform_now(@article.id)
    end
  end

  test "sends to subscribers with no tag subscriptions (subscribed to all)" do
    tag = tags(:tech_tag)
    @article.tags << tag

    # Subscriber with no tags = subscribed to all
    all_subscriber = Subscriber.create!(
      email: "all#{Time.current.to_i}@example.com",
      confirmation_token: SecureRandom.urlsafe_base64(32),
      unsubscribe_token: SecureRandom.urlsafe_base64(32),
      confirmed_at: Time.current
    )

    # Clear other subscribers
    Subscriber.where.not(id: all_subscriber.id).update_all(confirmed_at: nil)

    # Job creates multiple logs: started + failed (SMTP error in test) + completed = 3 logs
    assert_difference "ActivityLog.count", 3, "Expected started, failed, and completed logs" do
      NativeNewsletterSenderJob.perform_now(@article.id)
    end
  end
end
