# frozen_string_literal: true

require "test_helper"
require "minitest/mock"
require "ostruct"

class PasswordResetJobTest < ActiveJob::TestCase
  class FakeMail
    attr_reader :delivery_method_called, :delivery_settings, :delivered

    def delivery_method(method = nil, settings = nil)
      @delivery_method_called = method
      @delivery_settings = settings
    end

    def deliver_now
      @delivered = true
    end
  end

  test "handles missing user gracefully" do
    notifier = RecordingNotifier.new

    with_event_notifier(notifier) do
      assert_nothing_raised do
        PasswordResetJob.perform_now(999999)
      end
    end

    assert_equal 1, notifier.events.count { |name, _| name == "password_reset_job.user_not_found" }
  end

  test "configures smtp and delivers email" do
    user = OpenStruct.new(id: 123, email_address: "user@example.com")

    NewsletterSetting.delete_all
    NewsletterSetting.create!(
      enabled: true,
      provider: "native",
      smtp_address: "smtp.example.com",
      smtp_port: 587,
      smtp_user_name: "user",
      smtp_password: "pass",
      from_email: "no-reply@example.com"
    )

    mail = FakeMail.new

    User.stub(:find, user) do
      PasswordsMailer.stub(:reset, mail) do
        PasswordResetJob.perform_now(user.id)
      end
    end

    assert_equal :smtp, mail.delivery_method_called
    assert mail.delivered
  end

  test "raises and logs when delivery fails" do
    user = OpenStruct.new(id: 456, email_address: "fail@example.com")

    NewsletterSetting.delete_all
    NewsletterSetting.create!(
      enabled: true,
      provider: "native",
      smtp_address: "smtp.example.com",
      smtp_port: 587,
      smtp_user_name: "user",
      smtp_password: "pass",
      from_email: "no-reply@example.com"
    )

    mail = FakeMail.new
    def mail.deliver_now
      raise "delivery failed"
    end

    notifier = RecordingNotifier.new
    User.stub(:find, user) do
      PasswordsMailer.stub(:reset, mail) do
        with_event_notifier(notifier) do
          assert_raises RuntimeError do
            PasswordResetJob.perform_now(user.id)
          end
        end
      end
    end

    assert_equal 1, notifier.events.count { |name, _| name == "password_reset_job.email_failed" }
  end

  test "logs no failure when delivery succeeds after a transient retry" do
    user = OpenStruct.new(id: 789, email_address: "user@example.com")

    NewsletterSetting.delete_all
    NewsletterSetting.create!(
      enabled: true,
      provider: "native",
      smtp_address: "smtp.example.com",
      smtp_port: 587,
      smtp_user_name: "user",
      smtp_password: "pass",
      from_email: "no-reply@example.com"
    )

    attempts = 0
    mail = FakeMail.new
    mail.define_singleton_method(:deliver_now) do
      attempts += 1
      raise Net::SMTPServerBusy, "busy" if attempts == 1
    end

    notifier = RecordingNotifier.new
    User.stub(:find, user) do
      PasswordsMailer.stub(:reset, mail) do
        with_event_notifier(notifier) do
          perform_enqueued_jobs { PasswordResetJob.perform_later(user.id) }
        end
      end
    end

    assert_equal 2, attempts
    assert_equal 0, notifier.events.count { |name, _| name == "password_reset_job.email_failed" }
    assert_equal 1, notifier.events.count { |name, _| name == "password_reset_job.email_sent" }
  end

  test "logs a single failure when retries are exhausted" do
    user = OpenStruct.new(id: 101, email_address: "fail@example.com")

    NewsletterSetting.delete_all
    NewsletterSetting.create!(
      enabled: true,
      provider: "native",
      smtp_address: "smtp.example.com",
      smtp_port: 587,
      smtp_user_name: "user",
      smtp_password: "pass",
      from_email: "no-reply@example.com"
    )

    attempts = 0
    mail = FakeMail.new
    mail.define_singleton_method(:deliver_now) do
      attempts += 1
      raise Net::SMTPServerBusy, "busy"
    end

    notifier = RecordingNotifier.new
    User.stub(:find, user) do
      PasswordsMailer.stub(:reset, mail) do
        with_event_notifier(notifier) do
          perform_enqueued_jobs { PasswordResetJob.perform_later(user.id) }
        end
      end
    end

    assert_equal 3, attempts
    assert_equal 1, notifier.events.count { |name, _| name == "password_reset_job.email_failed" }
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
