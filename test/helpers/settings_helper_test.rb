# frozen_string_literal: true

require "test_helper"

class SettingsHelperTest < ActionView::TestCase
  include ActiveSupport::Testing::TimeHelpers

  test "timezone_options returns formatted labels and names" do
    travel_to Time.utc(2024, 1, 25, 14, 30) do
      options = timezone_options

      assert options.any?

      label, name = options.detect { |(_, zone_name)| zone_name == "UTC" }

      assert_equal "UTC", name
      assert_match(/\A2024\/01\/25 14:30 - \(\+00:00\) UTC\z/, label)
    end
  end
end
