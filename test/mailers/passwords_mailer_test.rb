# frozen_string_literal: true

require "test_helper"
require "ostruct"

class PasswordsMailerTest < ActionMailer::TestCase
  test "reset sends to user email" do
    user = OpenStruct.new(email_address: "mail@example.com", password_reset_token: "token")

    mail = PasswordsMailer.reset(user)

    assert_equal [ "mail@example.com" ], mail.to
    assert_equal "Reset your password", mail.subject
  end
end
