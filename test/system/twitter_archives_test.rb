# frozen_string_literal: true

require "application_system_test_case"

class TwitterArchivesTest < ApplicationSystemTestCase
  test "public archive splits twitter archive data into six tabs" do
    assert_includes TwitterArchiveConnection.attribute_names, "screen_name"

    import_time = Time.zone.parse("2026-04-03 09:30:00 UTC")

    TwitterArchiveTweet.create!(
      tweet_id: "300",
      entry_type: "tweet",
      screen_name: "archive_owner",
      full_text: "Original archive tweet",
      tweeted_at: Time.zone.parse("2024-01-01 10:00:00 UTC"),
      created_at: import_time,
      updated_at: import_time
    )
    TwitterArchiveTweet.create!(
      tweet_id: "301",
      entry_type: "reply",
      screen_name: "archive_owner",
      full_text: "@friend Archive reply",
      tweeted_at: Time.zone.parse("2024-01-02 10:00:00 UTC"),
      created_at: import_time,
      updated_at: import_time
    )
    TwitterArchiveTweet.create!(
      tweet_id: "302",
      entry_type: "retweet_quote",
      screen_name: "archive_owner",
      full_text: "Archive quote tweet",
      tweeted_at: Time.zone.parse("2024-01-03 10:00:00 UTC"),
      created_at: import_time,
      updated_at: import_time
    )
    TwitterArchiveConnection.create!(
      account_id: "900",
      relationship_type: "follower",
      user_link: "https://twitter.com/follower_one",
      screen_name: "follower_handle",
      created_at: import_time,
      updated_at: import_time
    )
    TwitterArchiveConnection.create!(
      account_id: "901",
      relationship_type: "following",
      user_link: "https://twitter.com/intent/user?user_id=901",
      created_at: import_time,
      updated_at: import_time
    )
    TwitterArchiveLike.create!(
      tweet_id: "777",
      full_text: "Liked tweet text",
      expanded_url: "https://twitter.com/someone/status/777",
      created_at: import_time,
      updated_at: import_time
    )

    visit twitter_archive_path

    assert_text "Twitter Archive"
    assert_text "Last archive upload: April 3, 2026 09:30"
    assert_text "Original archive tweet"
    assert_no_text "@friend Archive reply"
    assert_no_text "Archive quote tweet"

    click_link "Replies"
    assert_text "@friend Archive reply"
    assert_no_text "Original archive tweet"
    assert_no_text "Archive quote tweet"

    click_link "Retweets / Quotes"
    assert_text "Archive quote tweet"
    assert_no_text "Original archive tweet"
    assert_no_text "@friend Archive reply"

    click_link "Followers"
    assert_link "@follower_handle", href: "https://twitter.com/follower_one"
    assert_selector "ul.twitter-archive-connection-list"
    assert_selector "li.twitter-archive-connection-item", count: 1
    assert_no_text "Archive quote tweet"
    assert_no_text "Liked tweet text"
    assert_no_text "Account ID: 900"

    click_link "Following"
    assert_link "Account ID: 901", href: "https://twitter.com/intent/user?user_id=901"
    assert_selector "ul.twitter-archive-connection-list"
    assert_selector "li.twitter-archive-connection-item", count: 1
    assert_no_text "@follower_one"
    assert_no_text "Liked tweet text"
    assert_no_text "Twitter name unavailable in this archive"
    assert_no_text "Open on X"

    click_link "Likes"
    assert_text "Liked tweet text"
    assert_link "View on X", href: "https://twitter.com/someone/status/777"
    assert_no_text "@following_one"
  end

  test "public archive skips unsafe imported urls" do
    TwitterArchiveConnection.create!(
      account_id: "900",
      relationship_type: "follower",
      user_link: "javascript:alert(1)",
      screen_name: "follower_handle"
    )
    TwitterArchiveLike.create!(
      tweet_id: "777",
      full_text: "Liked tweet text",
      expanded_url: "data:text/html,<script>alert(1)</script>"
    )

    visit twitter_archive_path(tab: "follower")

    assert_text "@follower_handle"
    assert_no_link "@follower_handle"
    assert_no_selector "a[href^='javascript:']"

    click_link "Likes"

    assert_text "Liked tweet text"
    assert_no_link "View on X"
    assert_no_selector "a[href^='data:']"
  end

  test "public archive paginates tweet entries and keeps tab navigation working" do
    21.times do |index|
      TwitterArchiveTweet.create!(
        tweet_id: (300 + index).to_s,
        entry_type: "tweet",
        screen_name: "archive_owner",
        full_text: "Archive tweet #{format('%02d', index + 1)} only",
        tweeted_at: Time.zone.parse("2024-01-#{format('%02d', index + 1)} 10:00:00 UTC")
      )
    end

    TwitterArchiveTweet.create!(
      tweet_id: "500",
      entry_type: "reply",
      screen_name: "archive_owner",
      full_text: "@friend Archive reply",
      tweeted_at: Time.zone.parse("2024-01-22 10:00:00 UTC")
    )

    visit twitter_archive_path

    assert_text "Archive tweet 21 only"
    assert_no_text "Archive tweet 01 only"
    assert_link "2"

    click_link "2"

    assert_text "Archive tweet 01 only"
    assert_no_text "Archive tweet 21 only"

    click_link "Replies"

    assert_text "@friend Archive reply"
    assert_no_text "Archive tweet 01 only"
  end
end
