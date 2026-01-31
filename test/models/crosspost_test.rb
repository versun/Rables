# frozen_string_literal: true

require "test_helper"

class CrosspostTest < ActiveSupport::TestCase
  test "server_url must be http(s)" do
    crosspost = build_crosspost("file:///etc/passwd")

    assert_not crosspost.valid?
    assert_includes crosspost.errors[:server_url], "must be a valid http(s) URL"
  end

  test "server_url allows https urls" do
    crosspost = build_crosspost("https://mastodon.social")

    assert crosspost.valid?
  end

  test "server_url allows https urls with subpaths" do
    crosspost = build_crosspost("https://mastodon.social/masto")

    assert crosspost.valid?
  end

  test "server_url allows leading and trailing whitespace" do
    crosspost = build_crosspost(" https://mastodon.social ")

    assert crosspost.valid?
  end

  test "uses platform defaults for max characters and enforces credentials when enabled" do
    mastodon = Crosspost.new(platform: "mastodon", enabled: false)
    twitter = Crosspost.new(platform: "twitter", enabled: false, max_characters: 111)
    bluesky = Crosspost.new(platform: "bluesky", enabled: false)
    xiaohongshu = Crosspost.new(platform: "xiaohongshu", enabled: false)

    assert_equal 500, mastodon.default_max_characters
    assert_equal 250, twitter.default_max_characters
    assert_equal 300, bluesky.default_max_characters
    assert_equal 300, xiaohongshu.default_max_characters
    assert_equal 111, twitter.effective_max_characters
    assert_equal 500, mastodon.effective_max_characters

    mastodon.enabled = true
    assert_not mastodon.valid?
    assert_includes mastodon.errors[:client_key], "can't be blank"

    xiaohongshu.enabled = true
    assert xiaohongshu.valid?
  end

  test "validates platform presence" do
    crosspost = Crosspost.new(platform: nil)
    assert_not crosspost.valid?
    assert_includes crosspost.errors[:platform], "can't be blank"
  end

  test "validates platform inclusion in allowed platforms" do
    crosspost = Crosspost.new(platform: "invalid_platform")
    assert_not crosspost.valid?
    assert_includes crosspost.errors[:platform], "is not included in the list"
  end

  test "mastodon? returns true for mastodon platform" do
    crosspost = Crosspost.new(platform: "mastodon")
    assert crosspost.mastodon?
    assert_not crosspost.twitter?
    assert_not crosspost.bluesky?
    assert_not crosspost.xiaohongshu?
  end

  test "twitter? returns true for twitter platform" do
    crosspost = Crosspost.new(platform: "twitter")
    assert crosspost.twitter?
    assert_not crosspost.mastodon?
    assert_not crosspost.bluesky?
  end

  test "bluesky? returns true for bluesky platform" do
    crosspost = Crosspost.new(platform: "bluesky")
    assert crosspost.bluesky?
    assert_not crosspost.mastodon?
    assert_not crosspost.twitter?
  end

  test "xiaohongshu? returns true for xiaohongshu platform" do
    crosspost = Crosspost.new(platform: "xiaohongshu")
    assert crosspost.xiaohongshu?
    assert_not crosspost.mastodon?
    assert_not crosspost.twitter?
  end

  test "enabled? returns true when enabled is true" do
    crosspost = Crosspost.new(platform: "mastodon", enabled: true)
    assert crosspost.enabled?
  end

  test "enabled? returns false when enabled is false" do
    crosspost = Crosspost.new(platform: "mastodon", enabled: false)
    assert_not crosspost.enabled?
  end

  test "rejects server_url with credentials" do
    crosspost = build_crosspost("https://user:pass@mastodon.social")
    assert_not crosspost.valid?
    assert_includes crosspost.errors[:server_url], "must not include credentials"
  end

  test "validates twitter credentials when enabled" do
    crosspost = Crosspost.new(
      platform: "twitter",
      enabled: true,
      api_key: nil,
      api_key_secret: nil,
      access_token: nil,
      access_token_secret: nil
    )
    assert_not crosspost.valid?
    assert_includes crosspost.errors[:api_key], "can't be blank"
    assert_includes crosspost.errors[:api_key_secret], "can't be blank"
    assert_includes crosspost.errors[:access_token], "can't be blank"
    assert_includes crosspost.errors[:access_token_secret], "can't be blank"
  end

  test "validates bluesky credentials when enabled" do
    crosspost = Crosspost.new(
      platform: "bluesky",
      enabled: true,
      username: nil,
      app_password: nil
    )
    assert_not crosspost.valid?
    assert_includes crosspost.errors[:username], "can't be blank"
    assert_includes crosspost.errors[:app_password], "can't be blank"
  end

  test "does not validate credentials when disabled" do
    crosspost = Crosspost.new(
      platform: "mastodon",
      enabled: false,
      client_key: nil,
      client_secret: nil,
      access_token: nil
    )
    crosspost.valid?
    assert_empty crosspost.errors[:client_key]
    assert_empty crosspost.errors[:client_secret]
    assert_empty crosspost.errors[:access_token]
  end

  test "PLATFORMS constant contains all supported platforms" do
    assert_includes Crosspost::PLATFORMS, "mastodon"
    assert_includes Crosspost::PLATFORMS, "twitter"
    assert_includes Crosspost::PLATFORMS, "bluesky"
    assert_includes Crosspost::PLATFORMS, "xiaohongshu"
    assert_equal 4, Crosspost::PLATFORMS.length
  end

  test "PLATFORM_ICONS contains icons for all platforms" do
    assert_equal "fa-brands fa-mastodon", Crosspost::PLATFORM_ICONS["mastodon"]
    assert_equal "fa-brands fa-square-x-twitter", Crosspost::PLATFORM_ICONS["twitter"]
    assert_equal "fa-brands fa-square-bluesky", Crosspost::PLATFORM_ICONS["bluesky"]
    assert_equal "svg:xiaohongshu", Crosspost::PLATFORM_ICONS["xiaohongshu"]
  end

  private

  def build_crosspost(server_url)
    crosspost = Crosspost.mastodon
    crosspost.assign_attributes(server_url: server_url, enabled: false)
    crosspost
  end
end
