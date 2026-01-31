# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class CacheableSettingsTest < ActiveSupport::TestCase
  test "caches site info and navbar items with refresh helpers" do
    Rails.cache.clear

    info = CacheableSettings.site_info
    assert_equal settings(:default).title, info[:title]
    assert_equal settings(:default).url, info[:url]

    items = CacheableSettings.navbar_items
    assert_equal [ pages(:page_with_script).id, pages(:published_page).id ], items.map(&:id)

    Setting.stub(:first, nil) do
      Rails.cache.delete("site_info")
      assert_equal({}, CacheableSettings.site_info)
    end

    CacheableSettings.refresh_all
    assert_nil Rails.cache.read("site_info")
    assert_nil Rails.cache.read("navbar_items")
  end
end
