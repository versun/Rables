# frozen_string_literal: true

require "test_helper"

class SettingTest < ActiveSupport::TestCase
  test "parses social links JSON and reports setup completeness" do
    setting = settings(:default)
    assert_not Setting.setup_incomplete?

    setting.social_links_json = { "twitter" => "https://example.com" }.to_json
    assert setting.save
    assert_equal({ "twitter" => "https://example.com" }, setting.reload.social_links)

    setting.social_links_json = "{invalid"
    assert_not setting.save
    assert_match(/包含无效的 JSON 格式/, setting.errors[:social_links_json].join)

    setting.social_links_json = nil
    setting.update!(setup_completed: false)
    assert Setting.setup_incomplete?
  end
end
