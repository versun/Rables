# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class TwitterSyncServiceTest < ActiveSupport::TestCase
  setup do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )
    @sync = TwitterSync.instance
    @sync.update!(enabled: true, username: "testuser", user_id: "12345", since_id: "100")
  end

  test "does nothing unless sync, username, and crosspost are all enabled" do
    # No stubs: any API attempt would raise, so a clean return proves the guard works
    assert_no_difference "Article.count" do
      Crosspost.for("twitter").update!(enabled: false)
      service.perform

      Crosspost.for("twitter").update!(enabled: true)
      @sync.update!(enabled: false)
      service.perform

      @sync.update_columns(username: nil, enabled: true)
      service.perform
    end
  end

  test "archives an original tweet as a published article without title or source reference" do
    tweet = build_tweet(id: "200", text: "Hello from Twitter", created_at: "2026-07-20T10:00:00.000Z")
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ]))

    perform_with_stubbed_api(service, client)

    article = Article.find_by(slug: "tweet-200")
    assert article
    assert article.publish?
    assert_nil article.title
    assert_nil article.source_url
    assert_nil article.source_author
    assert_equal Time.parse("2026-07-20T10:00:00.000Z").to_i, article.created_at.to_i
    assert_includes article.content.to_plain_text, "Hello from Twitter"

    crosspost = article.social_media_posts.find_by(platform: "twitter")
    assert_equal "https://x.com/testuser/status/200", crosspost&.url
  end

  test "resolves user id when missing and caches it" do
    @sync.update!(user_id: nil)
    tweet = build_tweet(id: "200", text: "First sync")
    client = fake_client(
      "users/by/username/testuser" => { "data" => { "id" => "999" } },
      "users/999/tweets" => timeline_response([ tweet ])
    )

    perform_with_stubbed_api(service, client)

    assert_equal "999", @sync.reload.user_id
    assert Article.find_by(slug: "tweet-200")
  end

  test "skips retweets and replies but keeps quoted tweets" do
    tweets = [
      build_tweet(id: "201", text: "original"),
      build_tweet(id: "202", text: "retweet", referenced_tweets: [ { "type" => "retweeted", "id" => "50" } ]),
      build_tweet(id: "203", text: "reply", referenced_tweets: [ { "type" => "replied_to", "id" => "51" } ]),
      build_tweet(id: "204", text: "quote", referenced_tweets: [ { "type" => "quoted", "id" => "52" } ])
    ]
    client = fake_client("users/12345/tweets" => timeline_response(tweets))

    assert_difference "Article.count", 2 do
      perform_with_stubbed_api(service, client)
    end

    assert Article.find_by(slug: "tweet-201")
    assert_nil Article.find_by(slug: "tweet-202")
    assert_nil Article.find_by(slug: "tweet-203")

    quoted = Article.find_by(slug: "tweet-204")
    assert quoted
    assert_equal "https://x.com/i/web/status/52", quoted.source_url
    assert_not_includes quoted.content.body.to_s, "https://x.com/i/web/status/52"
  end

  test "uses note_tweet text when present" do
    tweet = build_tweet(id: "205", text: "short…", note_tweet: { "text" => "the full long form text" })
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ]))

    perform_with_stubbed_api(service, client)

    article = Article.find_by(slug: "tweet-205")
    assert_includes article.content.to_plain_text, "the full long form text"
  end

  test "downloads photo media and attaches it to the article content" do
    media = { "media_key" => "3_1", "type" => "photo", "url" => "https://pbs.twimg.com/media/abc.jpg" }
    tweet = build_tweet(id: "206", text: "with photo", media_keys: [ "3_1" ])
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ], media: [ media ]))

    stub_media_download(service, []) do
      perform_with_stubbed_api(service, client)
    end

    article = Article.find_by(slug: "tweet-206")
    assert_includes article.content.body.to_s, "action-text-attachment"
    assert_equal 1, article.content.embeds.count
  end

  test "picks the highest bitrate mp4 variant for videos" do
    media = {
      "media_key" => "7_1",
      "type" => "video",
      "preview_image_url" => "https://pbs.twimg.com/media/preview.jpg",
      "variants" => [
        { "content_type" => "application/x-mpegURL", "url" => "https://video.twimg.com/playlist.m3u8" },
        { "content_type" => "video/mp4", "bitrate" => 256_000, "url" => "https://video.twimg.com/low.mp4" },
        { "content_type" => "video/mp4", "bitrate" => 2_176_000, "url" => "https://video.twimg.com/high.mp4" }
      ]
    }
    tweet = build_tweet(id: "207", text: "with video", media_keys: [ "7_1" ])
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ], media: [ media ]))

    downloaded_urls = []
    stub_media_download(service, downloaded_urls) do
      perform_with_stubbed_api(service, client)
    end

    assert_equal [ "https://video.twimg.com/high.mp4" ], downloaded_urls
    assert Article.find_by(slug: "tweet-207")
  end

  test "continues archiving when a single media download fails" do
    media = { "media_key" => "3_2", "type" => "photo", "url" => "https://pbs.twimg.com/media/broken.jpg" }
    tweet = build_tweet(id: "208", text: "broken media", media_keys: [ "3_2" ])
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ], media: [ media ]))

    service.define_singleton_method(:download_media) { |*_args| raise "boom" }

    perform_with_stubbed_api(service, client)

    article = Article.find_by(slug: "tweet-208")
    assert article
    assert_equal 0, article.content.embeds.count
  end

  test "advances since_id to the newest tweet id" do
    tweets = [
      build_tweet(id: "101", text: "older"),
      build_tweet(id: "105", text: "newer")
    ]
    client = fake_client("users/12345/tweets" => timeline_response(tweets))

    perform_with_stubbed_api(service, client)

    @sync.reload
    assert_equal "105", @sync.since_id
    assert @sync.last_synced_at.present?
    assert_nil @sync.last_error
  end

  test "skips tweets that already have an article" do
    create_published_article(slug: "tweet-101", title: "existing")
    tweets = [
      build_tweet(id: "101", text: "older"),
      build_tweet(id: "105", text: "newer")
    ]
    client = fake_client("users/12345/tweets" => timeline_response(tweets))

    assert_difference "Article.count", 1 do
      perform_with_stubbed_api(service, client)
    end

    assert_equal "105", @sync.reload.since_id
  end

  test "first run only archives the latest FIRST_RUN_LIMIT tweets" do
    @sync.update!(since_id: nil)
    tweets = (1..15).map { |i| build_tweet(id: (100 + i).to_s, text: "tweet #{i}") }
    client = fake_client("users/12345/tweets" => timeline_response(tweets))

    assert_difference "Article.count", TwitterSyncService::FIRST_RUN_LIMIT do
      perform_with_stubbed_api(service, client)
    end

    assert_nil Article.find_by(slug: "tweet-101")
    assert Article.find_by(slug: "tweet-106")
    assert Article.find_by(slug: "tweet-115")
    assert_equal "115", @sync.reload.since_id
  end

  test "records last_error when the username cannot be resolved" do
    @sync.update!(user_id: nil)
    client = fake_client("users/by/username/testuser" => { "errors" => [ { "message" => "Not Found" } ] })

    assert_no_difference "Article.count" do
      perform_with_stubbed_api(service, client)
    end

    @sync.reload
    assert_nil @sync.user_id
    assert_equal "user not found: testuser", @sync.last_error
  end

  test "records last_error and does not archive when the API fails" do
    client = Object.new
    def client.get(_endpoint) = raise(StandardError, "api down")

    assert_no_difference "Article.count" do
      perform_with_stubbed_api(service, client)
    end

    assert_equal "api down", @sync.reload.last_error
  end

  test "logs activity for archived tweets" do
    tweet = build_tweet(id: "209", text: "logged tweet")
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ]))

    assert_difference -> { ActivityLog.where(target: "twitter_sync").count }, 1 do
      perform_with_stubbed_api(service, client)
    end
  end

  test "skips syncing when not due according to sync_schedule" do
    @sync.update!(sync_schedule: "daily")
    @sync.update_columns(last_synced_at: 1.hour.ago)
    client = fake_client("users/12345/tweets" => timeline_response([ build_tweet(id: "210", text: "new") ]))

    assert_no_difference "Article.count" do
      perform_with_stubbed_api(service, client)
    end
  end

  test "syncs when due according to sync_schedule" do
    @sync.update!(sync_schedule: "daily")
    @sync.update_columns(last_synced_at: 2.days.ago)
    tweet = build_tweet(id: "211", text: "due tweet")
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ]))

    perform_with_stubbed_api(service, client)

    assert Article.find_by(slug: "tweet-211")
  end

  test "force bypasses the schedule check" do
    @sync.update!(sync_schedule: "daily")
    @sync.update_columns(last_synced_at: Time.current)
    tweet = build_tweet(id: "212", text: "forced tweet")
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ]))

    perform_with_stubbed_api(service, client, force: true)

    assert Article.find_by(slug: "tweet-212")
  end

  test "skips X Article announcement tweets" do
    announcement = build_tweet(id: "213", text: "I wrote a new piece https://t.co/xyz")
    announcement["entities"] = { "urls" => [ { "expanded_url" => "https://x.com/i/article/1234567890" } ] }
    normal = build_tweet(id: "214", text: "normal tweet")
    client = fake_client("users/12345/tweets" => timeline_response([ announcement, normal ]))

    assert_difference "Article.count", 1 do
      perform_with_stubbed_api(service, client)
    end

    assert_nil Article.find_by(slug: "tweet-213")
    assert Article.find_by(slug: "tweet-214")
  end

  test "removes media attachment urls from the body since media is downloaded" do
    media = { "media_key" => "3_9", "type" => "photo", "url" => "https://pbs.twimg.com/media/xyz.jpg" }
    tweet = build_tweet(id: "224", text: "look at this\nhttps://t.co/pic1", media_keys: [ "3_9" ])
    tweet["entities"] = { "urls" => [
      { "url" => "https://t.co/pic1", "expanded_url" => "https://x.com/testuser/status/224/photo/1" }
    ] }
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ], media: [ media ]))

    stub_media_download(service, []) do
      perform_with_stubbed_api(service, client)
    end

    article = Article.find_by(slug: "tweet-224")
    plain = article.content.to_plain_text
    assert_includes plain, "look at this"
    assert_not_includes plain, "t.co"
    assert_not_includes plain, "photo/1"
    assert_equal 1, article.content.embeds.count
  end

  test "removes the quoted tweet url from the body since it is the source reference" do
    tweet = build_tweet(id: "225", text: "my take on this\nhttps://t.co/quote1",
      referenced_tweets: [ { "type" => "quoted", "id" => "60" } ])
    tweet["entities"] = { "urls" => [
      { "url" => "https://t.co/quote1", "expanded_url" => "https://twitter.com/someone/status/60" }
    ] }
    quoted = build_tweet(id: "60", text: "quoted words")
    quoted["author_id"] = "u3"
    author = { "id" => "u3", "name" => "Some One", "username" => "someone" }
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ], quoted_tweets: [ quoted ], users: [ author ]))

    perform_with_stubbed_api(service, client)

    article = Article.find_by(slug: "tweet-225")
    assert_equal "my take on this", article.content.to_plain_text.strip
    assert_equal "https://x.com/i/web/status/60", article.source_url
    assert_equal "quoted words", article.source_content
  end

  test "downloads media from quoted tweets alongside the tweet's own media" do
    media = [
      { "media_key" => "3_m1", "type" => "photo", "url" => "https://pbs.twimg.com/media/own.jpg" },
      { "media_key" => "3_q1", "type" => "photo", "url" => "https://pbs.twimg.com/media/quoted.jpg" }
    ]
    tweet = build_tweet(id: "227", text: "my take", media_keys: [ "3_m1" ],
      referenced_tweets: [ { "type" => "quoted", "id" => "62" } ])
    quoted = build_tweet(id: "62", text: "quoted words", media_keys: [ "3_q1" ])
    quoted["author_id"] = "u5"
    author = { "id" => "u5", "name" => "Quoted Author", "username" => "quoted" }
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ], media: media, quoted_tweets: [ quoted ], users: [ author ]))

    downloaded_urls = []
    stub_media_download(service, downloaded_urls) do
      perform_with_stubbed_api(service, client)
    end

    assert_equal [ "https://pbs.twimg.com/media/own.jpg", "https://pbs.twimg.com/media/quoted.jpg" ], downloaded_urls
    article = Article.find_by(slug: "tweet-227")
    assert_equal 2, article.content.embeds.count
  end

  test "removes media urls from quoted tweet content" do
    tweet = build_tweet(id: "226", text: "my take", referenced_tweets: [ { "type" => "quoted", "id" => "61" } ])
    quoted = build_tweet(id: "61", text: "quoted words https://t.co/qpic")
    quoted["author_id"] = "u4"
    quoted["entities"] = { "urls" => [
      { "url" => "https://t.co/qpic", "expanded_url" => "https://x.com/someone/status/61/photo/1" }
    ] }
    author = { "id" => "u4", "name" => "Some One", "username" => "someone" }
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ], quoted_tweets: [ quoted ], users: [ author ]))

    perform_with_stubbed_api(service, client)

    article = Article.find_by(slug: "tweet-226")
    assert_equal "quoted words", article.source_content
  end

  test "replaces t.co links with their expanded urls from entities" do
    tweet = build_tweet(id: "217", text: "read this https://t.co/abc123 now")
    tweet["entities"] = { "urls" => [
      { "url" => "https://t.co/abc123", "expanded_url" => "https://example.com/real-article" }
    ] }
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ]))

    perform_with_stubbed_api(service, client)

    article = Article.find_by(slug: "tweet-217")
    assert_includes article.content.to_plain_text, "https://example.com/real-article"
    assert_not_includes article.content.to_plain_text, "https://t.co/abc123"
  end

  test "replaces t.co links in note_tweet using note_tweet entities" do
    tweet = build_tweet(id: "218", text: "short… https://t.co/long",
      note_tweet: {
        "text" => "long form https://t.co/def456 end",
        "entities" => { "urls" => [
          { "url" => "https://t.co/def456", "expanded_url" => "https://example.com/long-form" }
        ] }
      })
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ]))

    perform_with_stubbed_api(service, client)

    article = Article.find_by(slug: "tweet-218")
    assert_includes article.content.to_plain_text, "https://example.com/long-form"
    assert_not_includes article.content.to_plain_text, "https://t.co/def456"
  end

  test "follows the redirect when a t.co link has no usable expanded url" do
    tweet = build_tweet(id: "219", text: "check https://t.co/ghi789 out")
    tweet["entities"] = { "urls" => [
      { "url" => "https://t.co/ghi789", "expanded_url" => "https://t.co/ghi789" }
    ] }
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ]))

    service.define_singleton_method(:follow_redirect) { |_url| "https://example.com/resolved" }

    perform_with_stubbed_api(service, client)

    article = Article.find_by(slug: "tweet-219")
    assert_includes article.content.to_plain_text, "https://example.com/resolved"
  end

  test "keeps the t.co link when redirect resolution fails" do
    tweet = build_tweet(id: "220", text: "broken https://t.co/jkl000 link")
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ]))

    service.define_singleton_method(:follow_redirect) { |*| nil }

    perform_with_stubbed_api(service, client)

    article = Article.find_by(slug: "tweet-220")
    assert_includes article.content.to_plain_text, "https://t.co/jkl000"
  end

  test "quoted tweet fills source reference author and content from includes" do
    tweet = build_tweet(id: "221", text: "my take", referenced_tweets: [ { "type" => "quoted", "id" => "52" } ])
    quoted = build_tweet(id: "52", text: "the quoted text https://t.co/q", created_at: "2026-07-19T10:00:00.000Z")
    quoted["author_id"] = "u1"
    quoted["entities"] = { "urls" => [
      { "url" => "https://t.co/q", "expanded_url" => "https://example.com/q" }
    ] }
    author = { "id" => "u1", "name" => "Quoted Author", "username" => "quoted" }
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ], quoted_tweets: [ quoted ], users: [ author ]))

    perform_with_stubbed_api(service, client)

    article = Article.find_by(slug: "tweet-221")
    assert_equal "https://x.com/i/web/status/52", article.source_url
    assert_equal "Quoted Author", article.source_author
    assert_equal "the quoted text https://example.com/q", article.source_content
  end

  test "quoted tweet uses note_tweet text and truncates long quoted content" do
    tweet = build_tweet(id: "223", text: "my take", referenced_tweets: [ { "type" => "quoted", "id" => "54" } ])
    long_text = "a" * 300
    quoted = build_tweet(id: "54", text: "short…", note_tweet: { "text" => long_text })
    quoted["author_id"] = "u2"
    author = { "id" => "u2", "name" => "Long Writer", "username" => "long" }
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ], quoted_tweets: [ quoted ], users: [ author ]))

    perform_with_stubbed_api(service, client)

    article = Article.find_by(slug: "tweet-223")
    assert_equal TwitterSyncService::QUOTED_CONTENT_LIMIT, article.source_content.length
  end

  test "quoted tweet archives without source reference when quoted data is unavailable" do
    tweet = build_tweet(id: "222", text: "my take", referenced_tweets: [ { "type" => "quoted", "id" => "53" } ])
    client = fake_client("users/12345/tweets" => timeline_response([ tweet ]))

    perform_with_stubbed_api(service, client)

    article = Article.find_by(slug: "tweet-222")
    assert article
    assert_equal "https://x.com/i/web/status/53", article.source_url
    assert_nil article.source_author
    assert_nil article.source_content
  end

  test "skips tweets before start_date" do
    @sync.update!(start_date: Date.new(2026, 7, 1))
    client = Object.new
    client.define_singleton_method(:get) do |_endpoint|
      { "data" => [
        { "id" => "215", "text" => "too old", "created_at" => "2026-06-30T23:59:59.000Z" },
        { "id" => "216", "text" => "in range", "created_at" => "2026-07-01T00:00:00.000Z" }
      ] }
    end

    assert_difference "Article.count", 1 do
      perform_with_stubbed_api(service, client)
    end

    assert_nil Article.find_by(slug: "tweet-215")
    assert Article.find_by(slug: "tweet-216")
  end

  test "backfills all pages since start_date instead of applying FIRST_RUN_LIMIT" do
    @sync.update!(since_id: nil, start_date: Date.new(2026, 7, 1))
    page1_tweets = (101..112).map { |i| build_tweet(id: i.to_s, text: "tweet #{i}") }
    page2_tweets = (113..118).map { |i| build_tweet(id: i.to_s, text: "tweet #{i}") }
    client = Object.new
    client.define_singleton_method(:get) do |endpoint|
      if endpoint.include?("pagination_token=page2")
        { "data" => page2_tweets, "meta" => {} }
      else
        { "data" => page1_tweets, "meta" => { "next_token" => "page2" } }
      end
    end

    assert_difference "Article.count", 18 do
      perform_with_stubbed_api(service, client)
    end

    assert Article.find_by(slug: "tweet-101")
    assert Article.find_by(slug: "tweet-118")
    assert_equal "118", @sync.reload.since_id
  end

  test "skips a poison tweet, archives the rest, and still advances since_id" do
    tweets = [
      build_tweet(id: "101", text: "fine before"),
      build_tweet(id: "102", text: "broken", created_at: "not-a-timestamp"),
      build_tweet(id: "103", text: "fine after")
    ]
    client = fake_client("users/12345/tweets" => timeline_response(tweets))

    assert_difference "Article.count", 2 do
      perform_with_stubbed_api(service, client)
    end

    assert Article.find_by(slug: "tweet-101")
    assert_nil Article.find_by(slug: "tweet-102")
    assert Article.find_by(slug: "tweet-103")
    assert_equal "103", @sync.reload.since_id
    assert @sync.last_synced_at.present?
  end

  test "incremental sync follows pagination tokens like the backfill does" do
    page1_tweets = (101..103).map { |i| build_tweet(id: i.to_s, text: "tweet #{i}") }
    page2_tweets = (104..105).map { |i| build_tweet(id: i.to_s, text: "tweet #{i}") }
    client = Object.new
    client.define_singleton_method(:get) do |endpoint|
      if endpoint.include?("pagination_token=page2")
        { "data" => page2_tweets, "meta" => {} }
      else
        { "data" => page1_tweets, "meta" => { "next_token" => "page2" } }
      end
    end

    assert_difference "Article.count", 5 do
      perform_with_stubbed_api(service, client)
    end

    assert Article.find_by(slug: "tweet-105")
    assert_equal "105", @sync.reload.since_id
  end

  test "records last_error and does not touch last_synced_at when the API returns an error payload" do
    client = fake_client("users/12345/tweets" => { "errors" => [ { "message" => "Service Unavailable" } ] })

    assert_no_difference "Article.count" do
      perform_with_stubbed_api(service, client)
    end

    @sync.reload
    assert_equal "Service Unavailable", @sync.last_error
    assert_nil @sync.last_synced_at
    assert_equal "100", @sync.since_id
  end

  test "treats an empty timeline page as a successful sync" do
    client = fake_client("users/12345/tweets" => { "meta" => { "result_count" => 0 } })

    assert_no_difference "Article.count" do
      perform_with_stubbed_api(service, client)
    end

    @sync.reload
    assert_nil @sync.last_error
    assert @sync.last_synced_at.present?
    assert_equal "100", @sync.since_id
  end

  private

  def service
    @service ||= TwitterSyncService.new(@sync)
  end

  def build_tweet(id:, text:, created_at: "2026-07-20T10:00:00.000Z", referenced_tweets: nil, media_keys: nil, note_tweet: nil)
    tweet = { "id" => id, "text" => text, "created_at" => created_at }
    tweet["referenced_tweets"] = referenced_tweets if referenced_tweets
    tweet["attachments"] = { "media_keys" => media_keys } if media_keys
    tweet["note_tweet"] = note_tweet if note_tweet
    tweet
  end

  def timeline_response(tweets, media: [], quoted_tweets: [], users: [])
    response = { "data" => tweets }
    includes = {}
    includes["media"] = media if media.any?
    includes["tweets"] = quoted_tweets if quoted_tweets.any?
    includes["users"] = users if users.any?
    response["includes"] = includes if includes.any?
    response
  end

  def fake_client(responses)
    client = Object.new
    client.define_singleton_method(:get) do |endpoint|
      match = responses.find { |prefix, _| endpoint.start_with?(prefix) }
      raise "unexpected endpoint: #{endpoint}" unless match

      match.last
    end
    client
  end

  # Bypasses the real rate limiter (6s delay) and X::Client construction
  def perform_with_stubbed_api(service, client, **kwargs)
    rate_limiter = Object.new
    rate_limiter.define_singleton_method(:make_request) { |c, endpoint, **| c.get(endpoint) }

    TwitterApi::RateLimiter.stub(:new, rate_limiter) do
      service.stub(:create_client, client) do
        service.perform(**kwargs)
      end
    end
  end

  # Replaces real HTTP downloads with an in-memory blob upload and records requested URLs
  def stub_media_download(service, downloaded_urls)
    service.define_singleton_method(:download_media) do |url, content_type, tweet_id|
      downloaded_urls << url
      ActiveStorage::Blob.create_and_upload!(
        io: StringIO.new("fake media data"),
        filename: "tweet-#{tweet_id}-test",
        content_type: content_type
      )
    end
    yield
  end
end
