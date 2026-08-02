# frozen_string_literal: true

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
    Crosspost.for("mastodon").update!(enabled: true)

    mock_service = Minitest::Mock.new
    mock_service.expect :post, "https://mastodon.social/@user/123", [ Article ]

    MastodonService.stub :new, mock_service do
      CrosspostArticleJob.perform_now(@article.id, "mastodon")
    end

    mock_service.verify
    assert @article.social_media_posts.find_by(platform: "mastodon")
  end

  test "posts to twitter when platform is twitter" do
    Crosspost.for("twitter").update!(enabled: true)

    mock_service = Minitest::Mock.new
    mock_service.expect :post, "https://x.com/user/status/123", [ Article ]

    TwitterService.stub :new, mock_service do
      CrosspostArticleJob.perform_now(@article.id, "twitter")
    end

    mock_service.verify
    assert @article.social_media_posts.find_by(platform: "twitter")
  end

  test "posts to bluesky when platform is bluesky" do
    Crosspost.for("bluesky").update!(enabled: true)

    mock_service = Minitest::Mock.new
    mock_service.expect :post, "https://bsky.app/profile/user/post/123", [ Article ]

    BlueskyService.stub :new, mock_service do
      CrosspostArticleJob.perform_now(@article.id, "bluesky")
    end

    mock_service.verify
    assert @article.social_media_posts.find_by(platform: "bluesky")
  end

  test "logs activity on successful crosspost" do
    Crosspost.for("mastodon").update!(enabled: true)

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
    Crosspost.for("mastodon").update!(enabled: true)

    mock_service = Minitest::Mock.new
    mock_service.expect :post, nil, [ Article ]

    MastodonService.stub :new, mock_service do
      CrosspostArticleJob.perform_now(@article.id, "mastodon")
    end

    assert_nil @article.social_media_posts.find_by(platform: "mastodon")
  end

  test "logs error on exception" do
    Crosspost.for("mastodon").update!(enabled: true)

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

  test "skips reposting on retry when url already recorded" do
    Crosspost.for("mastodon").update!(enabled: true)
    @article.social_media_posts.create!(platform: "mastodon", url: "https://existing.url/1")

    called = false
    service = Object.new
    service.define_singleton_method(:post) { |_article| called = true; "https://new.url/2" }

    job = CrosspostArticleJob.new(@article.id, "mastodon")
    job.executions = 2

    MastodonService.stub :new, service do
      job.perform_now
    end

    refute called
    assert_equal "https://existing.url/1", @article.social_media_posts.find_by(platform: "mastodon").url
  end

  test "skips reposting on retry when url was recorded after the crosspost request" do
    Crosspost.for("mastodon").update!(enabled: true)
    @article.social_media_posts.create!(platform: "mastodon", url: "https://existing.url/1")

    called = false
    service = Object.new
    service.define_singleton_method(:post) { |_article| called = true; "https://new.url/2" }

    job = CrosspostArticleJob.new(@article.id, "mastodon", 1.hour.ago)
    job.executions = 2

    MastodonService.stub :new, service do
      job.perform_now
    end

    refute called
    assert_equal "https://existing.url/1", @article.social_media_posts.find_by(platform: "mastodon").url
  end

  test "reposts on retry when the recorded url predates the crosspost request" do
    Crosspost.for("mastodon").update!(enabled: true)
    old_post = @article.social_media_posts.create!(platform: "mastodon", url: "https://existing.url/1")
    old_post.update_column(:updated_at, 1.hour.ago)

    called = false
    service = Object.new
    service.define_singleton_method(:post) { |_article| called = true; "https://new.url/2" }

    job = CrosspostArticleJob.new(@article.id, "mastodon", Time.current)
    job.executions = 2

    MastodonService.stub :new, service do
      job.perform_now
    end

    assert called
    assert_equal "https://new.url/2", @article.social_media_posts.find_by(platform: "mastodon").url
  end

  test "updates existing social media post instead of creating duplicate" do
    Crosspost.for("mastodon").update!(enabled: true)
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

  test "retries transient errors and logs a single failure when exhausted" do
    Crosspost.for("mastodon").update!(enabled: true)

    attempts = 0
    error_service = Object.new
    error_service.define_singleton_method(:post) do |_article|
      attempts += 1
      raise Timeout::Error, "execution expired"
    end

    MastodonService.stub :new, error_service do
      assert_difference "ActivityLog.count", 1 do
        perform_enqueued_jobs { CrosspostArticleJob.perform_later(@article.id, "mastodon") }
      end
    end

    assert_equal 5, attempts
    log = ActivityLog.last
    assert_equal "failed", log.action
    assert_equal "crosspost", log.target
  end

  test "logs no failure when a transient retry eventually succeeds" do
    Crosspost.for("mastodon").update!(enabled: true)

    attempts = 0
    service = Object.new
    service.define_singleton_method(:post) do |_article|
      attempts += 1
      raise Timeout::Error, "execution expired" if attempts == 1
      "https://mastodon.social/@user/789"
    end

    MastodonService.stub :new, service do
      assert_difference "ActivityLog.count", 1 do # success log only
        perform_enqueued_jobs { CrosspostArticleJob.perform_later(@article.id, "mastodon") }
      end
    end

    assert_equal 2, attempts
    log = ActivityLog.last
    assert_equal "posted", log.action
    assert_equal "https://mastodon.social/@user/789", @article.social_media_posts.find_by(platform: "mastodon").url
  end

  test "discards permanent record errors without retrying" do
    Crosspost.for("mastodon").update!(enabled: true)

    attempts = 0
    error_service = Object.new
    error_service.define_singleton_method(:post) do |_article|
      attempts += 1
      raise ActiveRecord::RecordInvalid.new(SocialMediaPost.new)
    end

    MastodonService.stub :new, error_service do
      assert_difference "ActivityLog.count", 1 do
        perform_enqueued_jobs { CrosspostArticleJob.perform_later(@article.id, "mastodon") }
      end
    end

    assert_equal 1, attempts
    log = ActivityLog.last
    assert_equal "failed", log.action
    assert_equal "crosspost", log.target
  end
end
