# frozen_string_literal: true

require "test_helper"

class TwitterArchivesControllerTest < ActionDispatch::IntegrationTest
  test "public archive path is article-route-safe" do
    article = create_published_article(
      slug: "twitter-archive",
      title: "Twitter Archive Article",
      html_content: "<p>Archive slug article</p>"
    )

    assert_equal "/twitter/archive", twitter_archive_path
    assert_equal "/twitter-archive", article_path(article.slug)

    get article_path(article.slug)

    assert_response :success
    assert_select "h2", text: "Twitter Archive Article"
    assert_select ".twitter-archive-page", false

    get twitter_archive_path

    assert_response :success
    assert_select "h1", text: "Twitter Archive"
  end

  test "show renders archived media attachments" do
    tweet = TwitterArchiveTweet.create!(
      tweet_id: "300",
      entry_type: "tweet",
      screen_name: "archive_owner",
      full_text: "Tweet with media",
      tweeted_at: Time.zone.parse("2024-01-01 10:00:00 UTC")
    )

    tweet.media.attach(
      io: File.open(Rails.root.join("test/fixtures/files/test_image.jpg"), "rb"),
      filename: "tweet-image.jpg",
      content_type: "image/jpeg"
    )
    tweet.media.attach(
      io: StringIO.new("fake-mp4-data"),
      filename: "tweet-video.mp4",
      content_type: "video/mp4"
    )

    get twitter_archive_path

    assert_response :success
    assert_select "img[src*='/rails/active_storage/blobs/']"
    assert_select "video[controls='controls']"
  end

  test "show treats follower and following tabs as invalid public tabs" do
    TwitterArchiveTweet.create!(
      tweet_id: "300",
      entry_type: "tweet",
      screen_name: "archive_owner",
      full_text: "Public archive tweet",
      tweeted_at: Time.zone.parse("2024-01-01 10:00:00 UTC")
    )
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

    get twitter_archive_path

    assert_response :success
    assert_select "a", text: "Followers", count: 0
    assert_select "a", text: "Following", count: 0
    assert_select "ul.twitter-archive-connection-list", false
    assert_match(/Public archive tweet/, response.body)
    assert_no_match(/@follower_handle\b/, response.body)

    get twitter_archive_path(tab: "follower")

    assert_response :success
    assert_select "ul.twitter-archive-connection-list", false
    assert_match(/Public archive tweet/, response.body)
    assert_no_match(/@follower_handle\b/, response.body)
    assert_no_match(/Account ID: 900/, response.body)

    get twitter_archive_path(tab: "following")

    assert_response :success
    assert_select "ul.twitter-archive-connection-list", false
    assert_match(/Public archive tweet/, response.body)
    assert_no_match(/@follower_handle\b/, response.body)

    get twitter_archive_path(tab: "like")

    assert_response :success
    assert_select "a[href^='data:']", count: 0
    assert_select ".twitter-archive-card--like .twitter-archive-card__body", text: /Liked tweet text/
    assert_no_match(/View on X/, response.body)
  end

  test "show paginates tweet entries" do
    21.times do |index|
      TwitterArchiveTweet.create!(
        tweet_id: "pagination-#{index + 1}",
        entry_type: "tweet",
        screen_name: "archive_owner",
        full_text: "Archive tweet #{format('%02d', index + 1)} only",
        tweeted_at: Time.zone.parse("2024-01-#{format('%02d', index + 1)} 10:00:00 UTC")
      )
    end

    get twitter_archive_path

    assert_response :success
    assert_select ".twitter-archive-pagination a[href*='page=2']"
    assert_no_match(/Archive tweet 01 only/, response.body)

    get twitter_archive_path(page: 2)

    assert_response :success
    assert_match(/Archive tweet 01 only/, response.body)
    assert_no_match(/Archive tweet 21 only/, response.body)
  end

  test "show keeps likes public while hiding follower and following tabs" do
    TwitterArchiveConnection.create!(
      account_id: "900",
      relationship_type: "follower",
      user_link: "https://twitter.com/follower_one",
      screen_name: "follower_handle"
    )
    TwitterArchiveConnection.create!(
      account_id: "901",
      relationship_type: "following",
      user_link: "https://twitter.com/intent/user?user_id=901"
    )
    TwitterArchiveLike.create!(
      tweet_id: "777",
      full_text: "Liked tweet text",
      expanded_url: "https://twitter.com/someone/status/777"
    )

    get twitter_archive_path

    assert_response :success
    assert_select "a", text: "Followers", count: 0
    assert_select "a", text: "Following", count: 0
    assert_select "ul.twitter-archive-connection-list", false
    assert_no_match(/@follower_handle\b/, response.body)
    assert_no_match(/Account ID: 901/, response.body)

    get twitter_archive_path(tab: "like")

    assert_response :success
    assert_select "a", text: "Likes"
    assert_select "a[href=?]", "https://twitter.com/someone/status/777", text: "View on X"
    assert_match "Liked tweet text", response.body
  end
end
