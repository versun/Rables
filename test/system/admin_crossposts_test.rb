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

  test "twitter tab shows archive upload module" do
    sign_in(@user)
    visit admin_crossposts_path(platform: "twitter")

    assert_no_button "Import Archive"
    assert_no_selector "input[type='file']"
  end

  test "sidebar shows twitter archive link at the end of tools" do
    sign_in(@user)
    visit admin_crossposts_path(platform: "twitter")

    within("nav.sidebar-nav .nav-section:last-child") do
      links = all("a.nav-link").map(&:text)
      assert_equal [ "Migrate", "Crosspost", "Git", "Newsletter", "Jobs", "Twitter Archive" ], links
      assert_link "Twitter Archive", href: admin_twitter_archives_path
    end

    click_link "Twitter Archive"
    assert_current_path admin_twitter_archives_path, ignore_query: true
    assert_text "Twitter Archive"
    assert_button "Import Archive"
  end
end
