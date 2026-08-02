# frozen_string_literal: true

require "application_system_test_case"

class TwitterArchivesTest < ApplicationSystemTestCase
  def setup
    super
    TwitterArchiveImport.delete_all
    TwitterArchiveTweet.delete_all
    TwitterArchiveConnection.delete_all
    TwitterArchiveLike.delete_all
  end

  test "public archive hides follower and following while keeping other tabs" do
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

    assert_no_link "Followers"
    assert_no_link "Following"
    assert_no_text "@follower_handle"
    assert_no_text "Account ID: 901"

    click_link "Likes"
    assert_text "Liked tweet text"
    assert_link "View on X", href: "https://twitter.com/someone/status/777"

    visit twitter_archive_path(tab: "follower")
    assert_text "Original archive tweet"
    assert_no_text "@follower_handle"
    assert_no_text "Account ID: 900"
    assert_no_selector "ul.twitter-archive-connection-list"
  end
end
