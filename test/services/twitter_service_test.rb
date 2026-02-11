# frozen_string_literal: true

require "test_helper"
require "minitest/mock"
require "stringio"

class TwitterServiceTest < ActiveSupport::TestCase
  test "verify fails fast when required fields are blank" do
    service = TwitterService.new
    result = service.verify({})

    assert_equal false, result[:success]
    assert_match "Please provide complete OAuth 2.0 credentials", result[:error]
  end

  test "post returns nil when crosspost is disabled" do
    Crosspost.twitter.update!(enabled: false)
    service = TwitterService.new

    assert_nil service.post(create_published_article)
  end

  test "post uses quote_tweet_id when source_url is x.com" do
    Crosspost.twitter.update!(
      enabled: true,
      client_id: "client_id",
      client_secret: "client_secret",
      access_token: "access_token",
      refresh_token: "refresh_token"
    )

    article = create_published_article(source_url: "https://x.com/example/status/1234567890")

    client = Minitest::Mock.new
    client.expect(:post, { "data" => { "id" => "999" } }) do |endpoint, body|
      assert_equal "tweets", endpoint

      payload = JSON.parse(body)
      assert_equal "1234567890", payload["quote_tweet_id"]
      refute_includes payload["text"], article.source_url
      true
    end

    service = TwitterService.new
    result = service.stub(:create_client, client) { service.post(article) }

    assert_equal "https://x.com/i/web/status/999", result
    client.verify
  end

  test "post uploads all images for twitter" do
    Crosspost.twitter.update!(
      enabled: true,
      client_id: "client_id",
      client_secret: "client_secret",
      access_token: "access_token",
      refresh_token: "refresh_token"
    )

    article = create_published_article
    images = [ Object.new, Object.new ]
    article.define_singleton_method(:all_image_attachments) { |_limit| images }

    client = Minitest::Mock.new
    client.expect(:post, { "data" => { "id" => "999" } }) do |endpoint, body|
      assert_equal "tweets", endpoint

      payload = JSON.parse(body)
      assert_equal %w[media-1 media-2], payload.dig("media", "media_ids")
      true
    end

    service = TwitterService.new
    media_uploader = service.instance_variable_get(:@media_uploader)
    sequence = 0

    service.define_singleton_method(:create_client) { client }
    media_uploader.define_singleton_method(:upload) do |_client, _image|
      sequence += 1
      "media-#{sequence}"
    end

    result = service.post(article)

    assert_equal 2, sequence
    assert_equal "https://x.com/i/web/status/999", result
    client.verify
  end

  test "post returns url when media upload succeeds" do
    Crosspost.twitter.update!(
      enabled: true,
      client_id: "client_id",
      client_secret: "client_secret",
      access_token: "access_token",
      refresh_token: "refresh_token"
    )

    article = create_published_article
    article.define_singleton_method(:all_image_attachments) { |_limit| [ :image ] }

    client = Minitest::Mock.new
    client.expect(:post, { "data" => { "id" => "123" } }) do |endpoint, body|
      payload = JSON.parse(body)
      assert_equal "tweets", endpoint
      assert_equal [ "media_id" ], payload.dig("media", "media_ids")
      true
    end

    service = TwitterService.new
    media_uploader = service.instance_variable_get(:@media_uploader)
    media_uploader.define_singleton_method(:upload) { |_client, _image| "media_id" }

    result = service.stub(:create_client, client) do
      service.post(article)
    end

    assert_equal "https://x.com/i/web/status/123", result
    client.verify
  end

  test "post falls back to text only when media tweet fails" do
    Crosspost.twitter.update!(
      enabled: true,
      client_id: "client_id",
      client_secret: "client_secret",
      access_token: "access_token",
      refresh_token: "refresh_token"
    )

    article = create_published_article
    article.define_singleton_method(:all_image_attachments) { |_limit| [ :image ] }

    client = Minitest::Mock.new
    client.expect(:post, { "errors" => [ { "message" => "media failed" } ] }) do |endpoint, body|
      payload = JSON.parse(body)
      assert_equal "tweets", endpoint
      assert_equal [ "media_id" ], payload.dig("media", "media_ids")
      true
    end
    client.expect(:post, { "data" => { "id" => "456" } }) do |endpoint, body|
      payload = JSON.parse(body)
      assert_equal "tweets", endpoint
      assert_nil payload["media"]
      true
    end

    service = TwitterService.new
    media_uploader = service.instance_variable_get(:@media_uploader)
    media_uploader.define_singleton_method(:upload) { |_client, _image| "media_id" }

    result = service.stub(:create_client, client) do
      service.post(article)
    end

    assert_equal "https://x.com/i/web/status/456", result
    client.verify
  end

  test "post returns nil when tweet fails without media" do
    Crosspost.twitter.update!(
      enabled: true,
      client_id: "client_id",
      client_secret: "client_secret",
      access_token: "access_token",
      refresh_token: "refresh_token"
    )

    article = create_published_article
    article.define_singleton_method(:all_image_attachments) { |_limit| [] }

    client = Minitest::Mock.new
    client.expect(:post, { "errors" => [ { "message" => "bad request" } ] }) { |_endpoint, _body| true }

    service = TwitterService.new
    result = service.stub(:create_client, client) { service.post(article) }

    assert_nil result
    client.verify
  end

  test "post returns nil when client raises" do
    Crosspost.twitter.update!(
      enabled: true,
      client_id: "client_id",
      client_secret: "client_secret",
      access_token: "access_token",
      refresh_token: "refresh_token"
    )

    article = create_published_article
    client = Minitest::Mock.new
    client.expect(:post, nil) { |_endpoint, _body| raise "boom" }

    service = TwitterService.new
    result = service.stub(:create_client, client) { service.post(article) }

    assert_nil result
  end

  test "extracts tweet id from supported urls" do
    service = TwitterService.new

    assert_equal "123", service.send(:extract_tweet_id_from_url, "https://twitter.com/user/status/123")
    assert_equal "456", service.send(:extract_tweet_id_from_url, "https://x.com/user/status/456")
    assert_equal "789", service.send(:extract_tweet_id_from_url, "https://x.com/i/web/status/789")
    assert_nil service.send(:extract_tweet_id_from_url, "https://example.com/other")
  end

  test "quote_tweet_id_for_article only accepts x.com hosts" do
    service = TwitterService.new

    article = create_published_article(source_url: "https://x.com/user/status/123")
    assert_equal "123", service.send(:quote_tweet_id_for_article, article)

    article.update!(source_url: "https://twitter.com/user/status/456")
    assert_nil service.send(:quote_tweet_id_for_article, article)
  end

  test "process_tweets builds comment data with parent ids" do
    service = TwitterService.new

    response = {
      "data" => [
        {
          "id" => "1",
          "author_id" => "u1",
          "text" => "Reply",
          "created_at" => Time.current.iso8601,
          "referenced_tweets" => [ { "type" => "replied_to", "id" => "root" } ],
          "conversation_id" => "conv-1"
        },
        {
          "id" => "2",
          "author_id" => "u2",
          "text" => "Quote",
          "created_at" => Time.current.iso8601,
          "referenced_tweets" => [ { "type" => "quoted", "id" => "root" } ]
        }
      ],
      "includes" => {
        "users" => [
          { "id" => "u1", "username" => "alice", "name" => "Alice", "profile_image_url" => "http://example.com/a.png" },
          { "id" => "u2", "username" => "bob", "name" => "Bob", "profile_image_url" => "http://example.com/b.png" }
        ]
      }
    }

    comments = service.process_tweets(response, "root")

    assert_equal 2, comments.length
    assert_equal "root", comments.first[:parent_external_id]
    assert_equal "conv-1", comments.first[:conversation_id]
    assert_equal "root", comments.last[:parent_external_id]
  end

  test "rate_limiter returns nil after max retries" do
    rate_limiter = TwitterApi::RateLimiter.new
    client = Minitest::Mock.new
    response = { "errors" => [ { "title" => "Too Many Requests" } ] }
    client.expect(:get, response, [ String ])

    rate_limiter.stub(:sleep, nil) do
      result, rate_limit = rate_limiter.make_request_with_info(client, "tweets/1", max_retries: 0)

      assert_nil result
      assert_equal 0, rate_limit[:remaining]
    end
  end

  test "rate_limiter raises after max retries" do
    rate_limiter = TwitterApi::RateLimiter.new
    client = Minitest::Mock.new
    client.expect(:get, { "errors" => [ { "title" => "Too Many Requests" } ] }, [ "tweets/1" ])

    rate_limiter.stub(:sleep, nil) do
      assert_raises RuntimeError do
        rate_limiter.make_request(client, "tweets/1", max_retries: 0)
      end
    end
  end

  test "media_uploader creates temp file for remote images" do
    settings = Crosspost.twitter
    uploader = TwitterApi::MediaUploader.new(settings)
    remote_image = Object.new
    remote_image.define_singleton_method(:url) { "http://example.com/image.jpg" }
    remote_image.define_singleton_method(:class) { Struct.new(:name).new("ActionText::Attachables::RemoteImage") }

    uploader.stub(:download_remote_image, "image-data") do
      temp_file = uploader.send(:create_temp_file, remote_image)

      assert temp_file.is_a?(Tempfile)
      assert_equal "image-data", temp_file.read
      temp_file.close
      temp_file.unlink
    end
  end

  test "media_uploader uploads to twitter and returns media id" do
    Crosspost.twitter.update!(
      enabled: true,
      client_id: "client_id",
      client_secret: "client_secret",
      access_token: "access_token",
      refresh_token: "refresh_token"
    )
    settings = Crosspost.twitter
    uploader = TwitterApi::MediaUploader.new(settings)

    file = Tempfile.new([ "upload", ".jpg" ])
    file.write("data")
    file.rewind

    X::MediaUploader.stub(:upload, { "id" => "media123" }) do
      media_id = uploader.send(:upload_to_twitter, nil, file.path)

      assert_equal "media123", media_id
    end
  ensure
    file&.close
    file&.unlink
  end

  test "fetch_with_redirect follows redirects" do
    service = TwitterService.new
    redirect = Net::HTTPFound.new("1.1", "302", "Found")
    redirect["location"] = "/new"
    success = Net::HTTPSuccess.new("1.1", "200", "OK")
    success.instance_variable_set(:@read, true)
    success.instance_variable_set(:@body, "ok")

    responses = [ redirect, success ]
    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:open_timeout=) { |_val| }
    fake_http.define_singleton_method(:read_timeout=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| responses.shift }

    Net::HTTP.stub(:new, fake_http) do
      result = service.send(:fetch_with_redirect, URI("http://example.com/start"))

      assert result.is_a?(Net::HTTPSuccess)
    end
  end

  test "media_uploader download_remote_image returns data for relative urls" do
    settings = Crosspost.twitter
    uploader = TwitterApi::MediaUploader.new(settings)

    remote_image = Object.new
    remote_image.define_singleton_method(:url) { "/image.png" }

    response = Net::HTTPSuccess.new("1.1", "200", "OK")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "image-data")

    uploader.stub(:fetch_with_redirect, response) do
      data = uploader.send(:download_remote_image, remote_image)

      assert_equal "image-data", data
    end
  end

  test "verify succeeds with valid oauth2 credentials" do
    client = Object.new
    client.define_singleton_method(:get) { |_endpoint| { "data" => { "id" => "123" } } }

    X::Client.stub(:new, client) do
      service = TwitterService.new
      result = service.verify(
        access_token: "token",
        refresh_token: "refresh-token",
        client_id: "client-id",
        client_secret: "client-secret"
      )

      assert_equal true, result[:success]
    end
  end


  test "post retries when create tweet is rate limited" do
    Crosspost.twitter.update!(
      enabled: true,
      client_id: "client_id",
      client_secret: "client_secret",
      access_token: "access_token",
      refresh_token: "refresh_token"
    )

    article = create_published_article
    article.define_singleton_method(:all_image_attachments) { |_limit| [] }

    service = TwitterService.new
    calls = 0
    client = Object.new
    client.define_singleton_method(:post) do |_endpoint, _body|
      calls += 1
      raise "429 Too Many Requests" if calls == 1

      { "data" => { "id" => "123" } }
    end

    service.stub(:create_client, client) do
      service.stub(:sleep, nil) do
        service.stub(:calculate_backoff_time, 0) do
          result = service.post(article)

          assert_equal "https://x.com/i/web/status/123", result
          assert_equal 2, calls
        end
      end
    end
  end

  test "fetch_comments aggregates replies and quote tweets" do
    Crosspost.twitter.update!(enabled: true)
    service = TwitterService.new
    rate_limiter = service.instance_variable_get(:@rate_limiter)

    replies_response = {
      "data" => [
        {
          "id" => "r1",
          "author_id" => "u1",
          "text" => "reply",
          "created_at" => Time.current.iso8601,
          "referenced_tweets" => [ { "type" => "replied_to", "id" => "123" } ]
        }
      ],
      "includes" => {
        "users" => [
          { "id" => "u1", "username" => "alice", "name" => "Alice", "profile_image_url" => "http://example.com/a.png" }
        ]
      }
    }

    quote_response = {
      "data" => [
        {
          "id" => "q1",
          "author_id" => "u2",
          "text" => "quote",
          "created_at" => Time.current.iso8601,
          "conversation_id" => "conv-q1"
        }
      ],
      "includes" => {
        "users" => [
          { "id" => "u2", "username" => "bob", "name" => "Bob", "profile_image_url" => "http://example.com/b.png" }
        ]
      }
    }

    quote_replies_response = {
      "data" => [
        {
          "id" => "qr1",
          "author_id" => "u3",
          "text" => "quote reply",
          "created_at" => Time.current.iso8601,
          "referenced_tweets" => [ { "type" => "replied_to", "id" => "q1" } ]
        }
      ],
      "includes" => {
        "users" => [
          { "id" => "u3", "username" => "cara", "name" => "Cara", "profile_image_url" => "http://example.com/c.png" }
        ]
      }
    }

    rate_limiter.define_singleton_method(:make_request) do |_client, _endpoint, **_opts|
      { "data" => { "conversation_id" => "conv-root" } }
    end

    rate_limiter.define_singleton_method(:make_request_with_info) do |_client, endpoint, **_opts|
      if endpoint.include?("is%3Areply") && endpoint.include?("conv-q1")
        [ quote_replies_response, { limit: 180, remaining: 50, reset_at: Time.current + 15.minutes } ]
      elsif endpoint.include?("is%3Areply")
        [ replies_response, { limit: 180, remaining: 50, reset_at: Time.current + 15.minutes } ]
      else
        [ quote_response, { limit: 180, remaining: 50, reset_at: Time.current + 15.minutes } ]
      end
    end

    service.stub(:create_client, Object.new) do
      result = service.fetch_comments("https://x.com/user/status/123")

      assert_equal 3, result[:comments].length
      assert result[:comments].any? { |comment| comment[:external_id] == "q1" }
      assert result[:comments].any? { |comment| comment[:external_id] == "qr1" }
    end
  end

  test "fetch_comments preserves rate_limit when quote tweet has no conversation id" do
    Crosspost.twitter.update!(enabled: true)
    service = TwitterService.new
    rate_limiter = service.instance_variable_get(:@rate_limiter)

    replies_response = {
      "data" => [
        {
          "id" => "r1",
          "author_id" => "u1",
          "text" => "reply",
          "created_at" => Time.current.iso8601,
          "referenced_tweets" => [ { "type" => "replied_to", "id" => "123" } ]
        }
      ],
      "includes" => {
        "users" => [
          { "id" => "u1", "username" => "alice", "name" => "Alice", "profile_image_url" => "http://example.com/a.png" }
        ]
      }
    }

    quote_response = {
      "data" => [
        {
          "id" => "q1",
          "author_id" => "u2",
          "text" => "quote",
          "created_at" => Time.current.iso8601
        }
      ],
      "includes" => {
        "users" => [
          { "id" => "u2", "username" => "bob", "name" => "Bob", "profile_image_url" => "http://example.com/b.png" }
        ]
      }
    }

    expected_rate_limit = { limit: 180, remaining: 42, reset_at: Time.current + 15.minutes }

    rate_limiter.define_singleton_method(:make_request) do |_client, _endpoint, **_opts|
      { "data" => { "conversation_id" => "conv-root" } }
    end

    rate_limiter.define_singleton_method(:make_request_with_info) do |_client, endpoint, **_opts|
      if endpoint.include?("is%3Areply")
        [ replies_response, expected_rate_limit ]
      else
        [ quote_response, expected_rate_limit ]
      end
    end

    service.stub(:create_client, Object.new) do
      result = service.fetch_comments("https://x.com/user/status/123")

      assert_equal 2, result[:comments].length
      assert_equal expected_rate_limit[:limit], result.dig(:rate_limit, :limit)
      assert_equal expected_rate_limit[:remaining], result.dig(:rate_limit, :remaining)
      assert_in_delta expected_rate_limit[:reset_at].to_i, result.dig(:rate_limit, :reset_at).to_i, 1
    end
  end

  test "verify returns failure when client raises" do
    service = TwitterService.new

    X::Client.stub(:new, ->(**_args) { raise "boom" }) do
      result = service.verify(
        access_token: "token",
        refresh_token: "refresh-token",
        client_id: "client-id",
        client_secret: "client-secret"
      )

      assert_equal false, result[:success]
      assert_match "boom", result[:error]
    end
  end

  test "oauth_service token_needs_refresh returns true when expires_at is nil" do
    Crosspost.twitter.update!(
      enabled: true,
      client_id: "client_id",
      client_secret: "client_secret",
      access_token: "access_token",
      refresh_token: "refresh_token",
      token_expires_at: nil
    )

    oauth_service = TwitterApi::OauthService.new(Crosspost.twitter)

    assert oauth_service.token_needs_refresh?
  end

  test "oauth_service token_needs_refresh returns true when token is about to expire" do
    Crosspost.twitter.update!(
      enabled: true,
      client_id: "client_id",
      client_secret: "client_secret",
      access_token: "access_token",
      refresh_token: "refresh_token",
      token_expires_at: Time.current + 2.minutes # Less than 5 minute buffer
    )

    oauth_service = TwitterApi::OauthService.new(Crosspost.twitter)

    assert oauth_service.token_needs_refresh?
  end

  test "oauth_service token_needs_refresh returns false when token is still valid" do
    Crosspost.twitter.update!(
      enabled: true,
      client_id: "client_id",
      client_secret: "client_secret",
      access_token: "access_token",
      refresh_token: "refresh_token",
      token_expires_at: Time.current + 1.hour
    )

    oauth_service = TwitterApi::OauthService.new(Crosspost.twitter)

    refute oauth_service.token_needs_refresh?
  end

  test "oauth_service refresh_token updates tokens in database" do
    Crosspost.twitter.update!(
      enabled: true,
      client_id: "client_id",
      client_secret: "client_secret",
      access_token: "old_access_token",
      refresh_token: "old_refresh_token",
      token_expires_at: Time.current - 1.hour
    )

    oauth_service = TwitterApi::OauthService.new(Crosspost.twitter)

    success_response = Net::HTTPSuccess.new("1.1", "200", "OK")
    success_response.instance_variable_set(:@read, true)
    success_response.instance_variable_set(:@body, {
      access_token: "new_access_token",
      refresh_token: "new_refresh_token",
      expires_in: 7200
    }.to_json)

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| success_response }

    Net::HTTP.stub(:new, fake_http) do
      oauth_service.refresh_token!
    end

    @settings = Crosspost.twitter.reload
    assert_equal "new_access_token", @settings.access_token
    assert_equal "new_refresh_token", @settings.refresh_token
    assert_in_delta Time.current + 7200, @settings.token_expires_at, 5
  end

  test "oauth_service refresh_token raises on failure response" do
    Crosspost.twitter.update!(
      enabled: true,
      client_id: "client_id",
      client_secret: "client_secret",
      access_token: "access_token",
      refresh_token: "refresh_token",
      token_expires_at: Time.current - 1.hour
    )

    oauth_service = TwitterApi::OauthService.new(Crosspost.twitter)

    error_response = Net::HTTPBadRequest.new("1.1", "400", "Bad Request")
    error_response.instance_variable_set(:@read, true)
    error_response.instance_variable_set(:@body, {
      error: "invalid_grant",
      error_description: "Refresh token has been revoked"
    }.to_json)

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| error_response }

    Net::HTTP.stub(:new, fake_http) do
      error = assert_raises(RuntimeError) do
        oauth_service.refresh_token!
      end

      assert_match "Refresh token has been revoked", error.message
    end
  end

  test "create_client refreshes token when needed via oauth_service" do
    Crosspost.twitter.update!(
      enabled: true,
      client_id: "client_id",
      client_secret: "client_secret",
      access_token: "access_token",
      refresh_token: "refresh_token",
      token_expires_at: Time.current - 1.hour # Expired
    )

    service = TwitterService.new
    oauth_service = service.instance_variable_get(:@oauth_service)
    refresh_called = false

    oauth_service.define_singleton_method(:refresh_if_needed!) do
      refresh_called = true
    end
    oauth_service.define_singleton_method(:build_client) do
      Object.new
    end

    service.send(:create_client)

    assert refresh_called
  end

  test "media_uploader returns nil when file missing" do
    settings = Crosspost.twitter
    uploader = TwitterApi::MediaUploader.new(settings)

    assert_nil uploader.send(:upload_to_twitter, nil, "/tmp/does-not-exist.jpg")
  end

  test "media_uploader returns nil when response missing media id" do
    Crosspost.twitter.update!(
      enabled: true,
      client_id: "client_id",
      client_secret: "client_secret",
      access_token: "access_token",
      refresh_token: "refresh_token"
    )
    settings = Crosspost.twitter
    uploader = TwitterApi::MediaUploader.new(settings)

    file = Tempfile.new([ "upload", ".jpg" ])
    file.write("data")
    file.rewind

    X::MediaUploader.stub(:upload, {}) do
      assert_nil uploader.send(:upload_to_twitter, nil, file.path)
    end
  ensure
    file&.close
    file&.unlink
  end

  test "media_uploader returns nil for non-image blob" do
    settings = Crosspost.twitter
    uploader = TwitterApi::MediaUploader.new(settings)

    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("data"),
      filename: "test.txt",
      content_type: "text/plain"
    )

    assert_nil uploader.send(:create_temp_file, blob)
  end

  test "media_uploader download_remote_image returns nil on non-success response" do
    settings = Crosspost.twitter
    uploader = TwitterApi::MediaUploader.new(settings)

    remote_image = Object.new
    remote_image.define_singleton_method(:url) { "http://example.com/bad.png" }

    response = Net::HTTPNotFound.new("1.1", "404", "Not Found")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "missing")

    uploader.stub(:fetch_with_redirect, response) do
      assert_nil uploader.send(:download_remote_image, remote_image)
    end
  end

  test "fetch_with_redirect returns response for non-success status" do
    service = TwitterService.new
    not_found = Net::HTTPNotFound.new("1.1", "404", "Not Found")
    not_found.instance_variable_set(:@read, true)
    not_found.instance_variable_set(:@body, "missing")

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:open_timeout=) { |_val| }
    fake_http.define_singleton_method(:read_timeout=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| not_found }

    Net::HTTP.stub(:new, fake_http) do
      result = service.send(:fetch_with_redirect, URI("http://example.com/start"))

      assert result.is_a?(Net::HTTPNotFound)
    end
  end

  test "fetch_with_redirect raises after too many redirects" do
    service = TwitterService.new

    assert_raises RuntimeError do
      service.send(:fetch_with_redirect, URI("http://example.com/start"), 0)
    end
  end

  test "rate_limiter retries and succeeds" do
    rate_limiter = TwitterApi::RateLimiter.new
    calls = 0
    client = Object.new
    client.define_singleton_method(:get) do |_endpoint|
      calls += 1
      if calls == 1
        { "errors" => [ { "title" => "Too Many Requests" } ] }
      else
        { "data" => { "id" => "ok" } }
      end
    end

    rate_limiter.stub(:sleep, nil) do
      response = rate_limiter.make_request(client, "tweets/1", max_retries: 1)

      assert_equal({ "data" => { "id" => "ok" } }, response)
    end
  end

  test "rate_limiter logs activity on exceeded" do
    rate_limiter = TwitterApi::RateLimiter.new

    ActivityLog.stub(:log!, :logged) do
      result = rate_limiter.send(:log_rate_limit_exceeded, { limit: 180, remaining: 0, reset_at: Time.current + 15.minutes })

      assert_equal :logged, result
    end
  end
end
