# frozen_string_literal: true

require "test_helper"
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

    User.stub(:find, user) do
      PasswordsMailer.stub(:reset, mail) do
        assert_raises RuntimeError do
          PasswordResetJob.perform_now(user.id)
        end
      end
    end
  end
end
