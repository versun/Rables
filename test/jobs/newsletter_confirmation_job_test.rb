# frozen_string_literal: true

require "test_helper"

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
end
