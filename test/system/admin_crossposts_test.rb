# frozen_string_literal: true

require "application_system_test_case"

class AdminCrosspostsTest < ApplicationSystemTestCase
  def setup
    @user = users(:admin)
    @mastodon = Crosspost.mastodon
    @twitter = Crosspost.twitter
    @bluesky = Crosspost.bluesky
  end

  test "viewing crosspost tabs" do
    sign_in(@user)
    visit admin_crossposts_path

    assert_text "Crosspost Settings"
    assert_text "Mastodon"

    click_link "X (Twitter)"
    assert_text "X (Twitter)"
    assert_text "API Key"

    click_link "Bluesky"
    assert_text "Bluesky"
    assert_text "App Password"
  end

  test "updating mastodon settings" do
    sign_in(@user)
    visit admin_crossposts_path(platform: "mastodon")

    fill_in "Mastodon Server URL", with: "https://mastodon.social"
    fill_in "Max Characters", with: "420"
    click_button "Save"

    assert_text "CrossPost settings updated successfully."
    @mastodon.reload
    assert_equal "https://mastodon.social", @mastodon.server_url
    assert_equal 420, @mastodon.max_characters
  end

  test "updating twitter settings" do
    sign_in(@user)
    visit admin_crossposts_path(platform: "twitter")

    fill_in "Max Characters", with: "240"
    click_button "Save"

    assert_text "CrossPost settings updated successfully."
    @twitter.reload
    assert_equal 240, @twitter.max_characters
  end
end
