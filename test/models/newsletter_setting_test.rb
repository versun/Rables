# frozen_string_literal: true

require "test_helper"

class NewsletterSettingTest < ActiveSupport::TestCase
  test "validates providers and reports configuration state" do
    NewsletterSetting.delete_all
    instance = NewsletterSetting.instance
    assert instance.new_record?

    invalid = NewsletterSetting.new(provider: "other")
    assert_not invalid.valid?
    assert_includes invalid.errors[:provider], "is not included in the list"

    native_setting = NewsletterSetting.new(
      provider: "native",
      enabled: true,
      smtp_address: "smtp.example.com",
      smtp_port: 587,
      smtp_user_name: "user",
      smtp_password: "secret",
      from_email: "from@example.com"
    )
    assert native_setting.native?
    assert_not native_setting.listmonk?
    assert native_setting.configured?
    assert_equal [], native_setting.missing_config_fields

    native_setting.smtp_password = nil
    assert_not native_setting.configured?
    assert_includes native_setting.missing_config_fields, "smtp_password"

    listmonk_setting = NewsletterSetting.new(provider: "listmonk", enabled: true)
    assert listmonk_setting.listmonk?
    assert_not listmonk_setting.configured?
    assert_equal [ "listmonk configuration" ], listmonk_setting.missing_config_fields

    Listmonk.create!(api_key: "key", username: "user", url: "https://example.com")
    assert listmonk_setting.configured?
    assert_equal [], listmonk_setting.missing_config_fields
  end
end
