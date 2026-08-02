# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class NewsletterConfirmationJobTest < ActiveJob::TestCase
  include ActionMailer::TestHelper

  setup do
    @subscriber = subscribers(:unconfirmed_subscriber)

    # Reset ActionMailer to test delivery method
    ActionMailer::Base.delivery_method = :test
    ActionMailer::Base.deliveries.clear

    # Disable native newsletter to prevent SMTP reconfiguration
    # This allows tests to use the :test delivery method
    NewsletterSetting.instance.update!(
      enabled: false,
      provider: "native",
      smtp_address: nil,
      smtp_port: nil,
      smtp_user_name: nil,
      smtp_password: nil,
      from_email: nil
    )
  end

  test "sends confirmation email to subscriber" do
    assert_emails 1 do
      NewsletterConfirmationJob.perform_now(@subscriber.id)
    end

    delivered = ActionMailer::Base.deliveries.last
    assert_equal [ @subscriber.email ], delivered.to
  end

  test "does nothing when subscriber not found" do
    assert_no_emails do
      assert_nothing_raised do
        NewsletterConfirmationJob.perform_now(999999)
      end
    end
  end

  test "sends email when newsletter is disabled" do
    # Confirmation emails should still be sent even when newsletter is disabled
    NewsletterSetting.instance.update!(enabled: false)

    assert_emails 1 do
      NewsletterConfirmationJob.perform_now(@subscriber.id)
    end
  end

  test "raises when mail delivery fails" do
    mail = Object.new
    mail.define_singleton_method(:deliver_now) { raise "boom" }

    original_method = NewsletterMailer.method(:confirmation_email)
    NewsletterMailer.define_singleton_method(:confirmation_email) { |*_args| mail }

    notifier = RecordingNotifier.new
    with_event_notifier(notifier) do
      assert_raises RuntimeError do
        NewsletterConfirmationJob.perform_now(@subscriber.id)
      end
    end

    assert_equal 1, notifier.events.count { |name, _| name == "newsletter_confirmation_job.email_failed" }
  ensure
    NewsletterMailer.define_singleton_method(:confirmation_email, original_method) if original_method
  end

  test "logs no failure when delivery succeeds after a transient retry" do
    attempts = 0
    mail = Object.new
    mail.define_singleton_method(:deliver_now) do
      attempts += 1
      raise Net::SMTPServerBusy, "busy" if attempts == 1
    end

    notifier = RecordingNotifier.new
    NewsletterMailer.stub(:confirmation_email, mail) do
      with_event_notifier(notifier) do
        perform_enqueued_jobs { NewsletterConfirmationJob.perform_later(@subscriber.id) }
      end
    end

    assert_equal 2, attempts
    assert_equal 0, notifier.events.count { |name, _| name == "newsletter_confirmation_job.email_failed" }
    assert_equal 1, notifier.events.count { |name, _| name == "newsletter_confirmation_job.email_sent" }
  end

  test "logs a single failure when retries are exhausted" do
    attempts = 0
    mail = Object.new
    mail.define_singleton_method(:deliver_now) do
      attempts += 1
      raise Net::SMTPServerBusy, "busy"
    end

    notifier = RecordingNotifier.new
    NewsletterMailer.stub(:confirmation_email, mail) do
      with_event_notifier(notifier) do
        perform_enqueued_jobs { NewsletterConfirmationJob.perform_later(@subscriber.id) }
      end
    end

    assert_equal 3, attempts
    assert_equal 1, notifier.events.count { |name, _| name == "newsletter_confirmation_job.email_failed" }
  end

  private

  def with_event_notifier(notifier)
    original_event = Rails.event
    Rails.define_singleton_method(:event) { notifier }
    yield
  ensure
    Rails.define_singleton_method(:event) { original_event }
  end
end
