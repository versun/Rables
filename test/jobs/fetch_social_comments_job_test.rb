# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class FetchSocialCommentsJobTest < ActiveJob::TestCase
  setup do
    @article = create_published_article
  end

  test "does nothing when both platforms are disabled" do
    Crosspost.mastodon.update!(enabled: false, auto_fetch_comments: false)
    Crosspost.bluesky.update!(enabled: false, auto_fetch_comments: false)

    assert_nothing_raised do
      FetchSocialCommentsJob.perform_now
    end
  end

  test "fetches mastodon comments when enabled" do
    Crosspost.mastodon.update!(enabled: true, auto_fetch_comments: true)
    Crosspost.bluesky.update!(enabled: false, auto_fetch_comments: false)

    @article.social_media_posts.create!(
      platform: "mastodon",
      url: "https://mastodon.social/@user/123"
    )

    mock_service = Minitest::Mock.new
    mock_service.expect :fetch_comments, { comments: [], rate_limit: nil }, [ String ]

    MastodonService.stub :new, mock_service do
      FetchSocialCommentsJob.perform_now
    end

    assert mock_service.verify, "MastodonService.fetch_comments should be called"
  end

  test "fetches bluesky comments when enabled" do
    Crosspost.mastodon.update!(enabled: false, auto_fetch_comments: false)
    Crosspost.bluesky.update!(enabled: true, auto_fetch_comments: true)

    @article.social_media_posts.create!(
      platform: "bluesky",
      url: "https://bsky.app/profile/user/post/123"
    )

    mock_service = Minitest::Mock.new
    mock_service.expect :fetch_comments, { comments: [], rate_limit: nil }, [ String ]

    BlueskyService.stub :new, mock_service do
      FetchSocialCommentsJob.perform_now
    end

    assert mock_service.verify, "BlueskyService.fetch_comments should be called"
  end

  test "creates new comments from fetched data" do
    Crosspost.mastodon.update!(enabled: true, auto_fetch_comments: true)
    Crosspost.bluesky.update!(enabled: false, auto_fetch_comments: false)

    @article.social_media_posts.create!(
      platform: "mastodon",
      url: "https://mastodon.social/@user/123"
    )

    mock_service = Object.new
    mock_service.define_singleton_method(:fetch_comments) do |_url|
      {
        comments: [ {
          external_id: "456",
          author_name: "Test User",
          author_username: "@testuser",
          author_avatar_url: "https://example.com/avatar.png",
          content: "Great article!",
          published_at: Time.current,
          url: "https://mastodon.social/@testuser/456"
        } ],
        rate_limit: nil
      }
    end

    MastodonService.stub :new, mock_service do
      assert_difference "@article.comments.count", 1 do
        FetchSocialCommentsJob.perform_now
      end
    end

    comment = @article.comments.last
    assert_equal "mastodon", comment.platform
    assert_equal "456", comment.external_id
    assert_equal "Test User", comment.author_name
  end

  test "updates existing comments instead of creating duplicates" do
    Crosspost.mastodon.update!(enabled: true, auto_fetch_comments: true)
    Crosspost.bluesky.update!(enabled: false, auto_fetch_comments: false)

    @article.social_media_posts.create!(
      platform: "mastodon",
      url: "https://mastodon.social/@user/123"
    )

    # Create existing comment
    @article.comments.create!(
      platform: "mastodon",
      external_id: "456",
      author_name: "Old Name",
      content: "Old content"
    )

    mock_service = Object.new
    mock_service.define_singleton_method(:fetch_comments) do |_url|
      {
        comments: [ {
          external_id: "456",
          author_name: "New Name",
          author_username: "@newuser",
          content: "Updated content",
          published_at: Time.current,
          url: "https://mastodon.social/@newuser/456"
        } ],
        rate_limit: nil
      }
    end

    MastodonService.stub :new, mock_service do
      assert_no_difference "@article.comments.count" do
        FetchSocialCommentsJob.perform_now
      end
    end

    comment = @article.comments.find_by(external_id: "456")
    assert_equal "New Name", comment.author_name
    assert_equal "Updated content", comment.content
  end

  test "stops processing when rate limit is critically low" do
    Crosspost.mastodon.update!(enabled: true, auto_fetch_comments: true)
    Crosspost.bluesky.update!(enabled: false, auto_fetch_comments: false)

    @article.social_media_posts.create!(
      platform: "mastodon",
      url: "https://mastodon.social/@user/123"
    )

    mock_service = Object.new
    mock_service.define_singleton_method(:fetch_comments) do |_url|
      {
        comments: [],
        rate_limit: { remaining: 3, limit: 300, reset_at: Time.current + 5.minutes }
      }
    end

    MastodonService.stub :new, mock_service do
      FetchSocialCommentsJob.perform_now
    end

    # Should log activity about rate limit pause
    log = ActivityLog.find_by(action: "paused", target: "fetch_comments")
    assert_not_nil log
  end

  test "logs activity on completion" do
    Crosspost.mastodon.update!(enabled: true, auto_fetch_comments: true)
    Crosspost.bluesky.update!(enabled: false, auto_fetch_comments: false)

    @article.social_media_posts.create!(
      platform: "mastodon",
      url: "https://mastodon.social/@user/123"
    )

    mock_service = Object.new
    mock_service.define_singleton_method(:fetch_comments) do |_url|
      { comments: [], rate_limit: nil }
    end

    MastodonService.stub :new, mock_service do
      FetchSocialCommentsJob.perform_now
    end

    log = ActivityLog.find_by(action: "completed", target: "fetch_comments")
    assert_not_nil log
    # Description contains platform info as formatted string
    assert_includes log.description, "mastodon"
  end
end
