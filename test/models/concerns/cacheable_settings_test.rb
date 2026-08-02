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
    assert_instance_of Array, items
    assert_equal [ pages(:page_with_script).id, pages(:published_page).id ], items.map(&:id)

    Setting.stub(:first, nil) do
      Rails.cache.delete("site_info")
      assert_equal({}, CacheableSettings.site_info)
    end

    CacheableSettings.refresh_all
    assert_nil Rails.cache.read("site_info")
    assert_nil Rails.cache.read("navbar_items")
    assert_nil Rails.cache.read("has_tags")
  end

  test "has_tags? reflects tag existence and is refreshed on tag changes" do
    Rails.cache.clear

    assert_equal Tag.exists?, CacheableSettings.has_tags?

    Tag.destroy_all
    assert_equal false, CacheableSettings.has_tags?

    Tag.create!(name: "New Tag For Cache Test")
    assert_equal true, CacheableSettings.has_tags?
  end
end
