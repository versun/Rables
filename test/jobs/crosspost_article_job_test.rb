require "test_helper"
require "minitest/mock"

class CrosspostArticleJobTest < ActiveJob::TestCase
  setup do
    @article = create_published_article
  end

  test "does nothing when article not found" do
    assert_nothing_raised do
      CrosspostArticleJob.perform_now(999999, "mastodon")
    end
  end

  test "posts to mastodon when platform is mastodon" do
    Crosspost.mastodon.update!(enabled: true)

    mock_service = Minitest::Mock.new
    mock_service.expect :post, "https://mastodon.social/@user/123", [ Article ]

    MastodonService.stub :new, mock_service do
      CrosspostArticleJob.perform_now(@article.id, "mastodon")
    end

    mock_service.verify
    assert @article.social_media_posts.find_by(platform: "mastodon")
  end

  test "posts to twitter when platform is twitter" do
    Crosspost.twitter.update!(enabled: true)

    mock_service = Minitest::Mock.new
    mock_service.expect :post, "https://twitter.com/user/status/123", [ Article ]

    TwitterService.stub :new, mock_service do
      CrosspostArticleJob.perform_now(@article.id, "twitter")
    end

    mock_service.verify
    assert @article.social_media_posts.find_by(platform: "twitter")
  end

  test "posts to bluesky when platform is bluesky" do
    Crosspost.bluesky.update!(enabled: true)

    mock_service = Minitest::Mock.new
    mock_service.expect :post, "https://bsky.app/profile/user/post/123", [ Article ]

    BlueskyService.stub :new, mock_service do
      CrosspostArticleJob.perform_now(@article.id, "bluesky")
    end

    mock_service.verify
    assert @article.social_media_posts.find_by(platform: "bluesky")
  end

  test "logs activity on successful crosspost" do
    Crosspost.mastodon.update!(enabled: true)

    mock_service = Minitest::Mock.new
    mock_service.expect :post, "https://mastodon.social/@user/123", [ Article ]

    MastodonService.stub :new, mock_service do
      assert_difference "ActivityLog.count", 1 do
        CrosspostArticleJob.perform_now(@article.id, "mastodon")
      end
    end

    log = ActivityLog.last
    assert_equal "posted", log.action
    assert_equal "crosspost", log.target
  end

  test "does not create social media post when service returns nil" do
    Crosspost.mastodon.update!(enabled: true)

    mock_service = Minitest::Mock.new
    mock_service.expect :post, nil, [ Article ]

    MastodonService.stub :new, mock_service do
      CrosspostArticleJob.perform_now(@article.id, "mastodon")
    end

    assert_nil @article.social_media_posts.find_by(platform: "mastodon")
  end

  test "logs error on exception" do
    Crosspost.mastodon.update!(enabled: true)

    # The job catches errors and logs them, then re-raises
    initial_log_count = ActivityLog.count

    error_service = Object.new
    error_service.define_singleton_method(:post) { |_article| raise StandardError, "API Error" }

    MastodonService.stub :new, error_service do
      begin
        CrosspostArticleJob.perform_now(@article.id, "mastodon")
      rescue StandardError
        # Expected - job re-raises after logging
      end
    end

    # Verify error was logged
    assert_operator ActivityLog.count, :>, initial_log_count
    log = ActivityLog.last
    assert_equal "failed", log.action
    assert_equal "crosspost", log.target
  end

  test "updates existing social media post instead of creating duplicate" do
    Crosspost.mastodon.update!(enabled: true)
    @article.social_media_posts.create!(platform: "mastodon", url: "https://old-url.com")

    mock_service = Minitest::Mock.new
    mock_service.expect :post, "https://mastodon.social/@user/456", [ Article ]

    MastodonService.stub :new, mock_service do
      assert_no_difference "@article.social_media_posts.count" do
        CrosspostArticleJob.perform_now(@article.id, "mastodon")
      end
    end

    assert_equal "https://mastodon.social/@user/456", @article.social_media_posts.find_by(platform: "mastodon").url
  end
end
