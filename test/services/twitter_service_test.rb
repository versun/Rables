# frozen_string_literal: true

require "test_helper"
require "minitest/mock"
require "stringio"
require "vips"

class FakeTwitterVipsImage
  attr_reader :width, :height

  def initialize(width: 1600, height: 1200, base_size: 14.megabytes)
    @width = width
    @height = height
    @base_size = base_size
  end

  def write_to_file(path, **options)
    size = (@base_size * (options.fetch(:Q).to_f / 100)).to_i
    File.binwrite(path, "a" * size)
  end

  def resize(scale)
    self.class.new(
      width: (@width * scale).to_i,
      height: (@height * scale).to_i,
      base_size: (@base_size * scale * scale).to_i
    )
  end

  def autorot
    self
  end

  def has_alpha?
    false
  end
end

class TwitterServiceTest < ActiveSupport::TestCase
  private

  def with_stubbed_method(object, method_name, replacement = nil, &block)
    singleton = object.singleton_class
    own_methods = singleton.instance_methods(false) + singleton.private_instance_methods(false)
    original = singleton.instance_method(method_name) if own_methods.include?(method_name)

    object.define_singleton_method(method_name) do |*args, **kwargs, &method_block|
      if replacement.respond_to?(:call)
        replacement.call(*args, **kwargs, &method_block)
      else
        replacement
      end
    end
    yield
  ensure
    # Restore the exact prior state: re-define an own singleton method that
    # existed, otherwise remove the stub entirely. Never leave a delegating
    # wrapper behind — wrappers leak into other tests in the same worker and
    # silently drop keyword arguments.
    if original
      singleton.send(:define_method, method_name, original)
    elsif singleton.method_defined?(method_name) || singleton.private_method_defined?(method_name)
      singleton.send(:remove_method, method_name)
    end
  end

  def oversized_oriented_jpeg_data
    Tempfile.create([ "twitter-oriented", ".jpg" ]) do |file|
      noise = Vips::Image.gaussnoise(1800, 2500).cast(:uchar)
      image = noise.bandjoin([ noise, noise ]).copy
      image.set_type(GObject::GINT_TYPE, "orientation", 6)
      image.write_to_file(file.path, Q: 100)
      File.binread(file.path)
    end
  end

  def oversized_transparent_png_data
    Tempfile.create([ "twitter-transparent", ".png" ]) do |file|
      noise = Vips::Image.gaussnoise(1200, 1200).cast(:uchar)
      rgb = noise.bandjoin([ noise, noise ])
      alpha = Vips::Image.black(1200, 1200).new_from_image([ 0 ])
      rgba = rgb.bandjoin(alpha)
      rgba.write_to_file(file.path, compression: 0)
      File.binread(file.path)
    end
  end

  public

  test "verify fails fast when required fields are blank" do
    service = TwitterService.new
    result = service.verify({})

    assert_equal false, result[:success]
    assert_match "Please fill in all information", result[:error]
  end

  test "post returns nil when crosspost is disabled" do
    Crosspost.for("twitter").update!(enabled: false)
    service = TwitterService.new

    assert_nil service.post(create_published_article)
  end

  test "post uses quote_tweet_id when source_url is x.com" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    article = create_published_article(source_url: "https://x.com/example/status/1234567890")

    client = Object.new
    client.define_singleton_method(:get) { |_endpoint| { "data" => { "username" => "testuser" } } }
    client.define_singleton_method(:post) do |endpoint, body|
      { "data" => { "id" => "999" } }
    end

    service = TwitterService.new
    result = service.stub(:create_client, client) { service.post(article) }

    assert_equal "https://x.com/testuser/status/999", result
  end

  test "post re-raises transient network errors for job retry" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    article = create_published_article
    article.define_singleton_method(:all_image_attachments) { |_limit| [] }

    client = Object.new
    client.define_singleton_method(:get) { |_endpoint| { "data" => { "username" => "testuser" } } }
    client.define_singleton_method(:post) { |_endpoint, _body| raise Net::OpenTimeout }

    service = TwitterService.new
    assert_raises(Net::OpenTimeout) do
      service.stub(:create_client, client) { service.post(article) }
    end
  end

  test "post uses quote_tweet_id when source_url is twitter.com" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    article = create_published_article(source_url: "https://twitter.com/example/status/1234567890?s=20")
    article.define_singleton_method(:all_image_attachments) { |_limit| [] }

    posted_payload = nil
    client = Object.new
    client.define_singleton_method(:get) { |_endpoint| { "data" => { "username" => "testuser" } } }
    client.define_singleton_method(:post) do |_endpoint, body|
      posted_payload = JSON.parse(body)
      { "data" => { "id" => "999" } }
    end

    service = TwitterService.new
    result = service.stub(:create_client, client) { service.post(article) }

    assert_equal "https://x.com/testuser/status/999", result
    assert_equal "1234567890", posted_payload["quote_tweet_id"]
  end

  test "post returns nil when quote tweet is forbidden" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    article = create_published_article(
      source_url: "https://x.com/example/status/1234567890",
      title: "Quoted Source"
    )
    article.define_singleton_method(:all_image_attachments) { |_limit| [] }

    payloads = []
    client = Object.new
    client.define_singleton_method(:get) { |_endpoint| { "data" => { "username" => "testuser" } } }
    client.define_singleton_method(:post) do |_endpoint, body|
      payloads << JSON.parse(body)
      if payloads.one?
        {
          "errors" => [
            {
              "message" => "Forbidden: Quoting this post is not allowed because you have not been mentioned or are not part of the conversation thread of the post you are quoting."
            }
          ]
        }
      else
        { "data" => { "id" => "999" } }
      end
    end

    service = TwitterService.new
    result = service.stub(:create_client, client) { service.post(article) }

    assert_nil result
    assert_equal 1, payloads.length
    assert_equal "1234567890", payloads.first["quote_tweet_id"]
  end

  test "post skips media when quote_tweet_id is present" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    article = create_published_article(source_url: "https://x.com/example/status/1234567890")
    images = [ Object.new, Object.new ]
    article.define_singleton_method(:all_image_attachments) { |_limit| images }

    posted_payload = nil
    client = Object.new
    client.define_singleton_method(:get) { |_endpoint| { "data" => { "username" => "testuser" } } }
    client.define_singleton_method(:post) do |_endpoint, body|
      posted_payload = JSON.parse(body)
      { "data" => { "id" => "999" } }
    end

    service = TwitterService.new
    media_uploader = service.instance_variable_get(:@media_uploader)
    upload_called = false
    media_uploader.define_singleton_method(:upload) do |_client, _image|
      upload_called = true
      "media_id"
    end

    result = service.stub(:create_client, client) { service.post(article) }

    assert_equal "https://x.com/testuser/status/999", result
    refute upload_called
    assert_equal "1234567890", posted_payload["quote_tweet_id"]
    refute posted_payload.key?("media")
  end

  test "post uploads the first four images for twitter" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    article = create_published_article
    images = Array.new(5) { Object.new }
    requested_limit = nil
    article.define_singleton_method(:all_image_attachments) do |limit|
      requested_limit = limit
      images.first(limit)
    end

    client = Object.new
    client.define_singleton_method(:get) { |_endpoint| { "data" => { "username" => "testuser" } } }
    posted_payload = nil
    client.define_singleton_method(:post) do |endpoint, body|
      posted_payload = JSON.parse(body)
      { "data" => { "id" => "999" } }
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

    assert_equal 4, requested_limit
    assert_equal 4, sequence
    assert_equal %w[media-1 media-2 media-3 media-4], posted_payload.dig("media", "media_ids")
    assert_equal "https://x.com/testuser/status/999", result
  end

  test "post only uploads a single animated gif for twitter" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    gif_blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("GIF89a"),
      filename: "animated.gif",
      content_type: "image/gif"
    )
    jpeg_blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("jpeg"),
      filename: "photo.jpg",
      content_type: "image/jpeg"
    )

    article = create_published_article
    article.define_singleton_method(:all_image_attachments) { |_limit| [ gif_blob, jpeg_blob ] }

    client = Object.new
    client.define_singleton_method(:get) { |_endpoint| { "data" => { "username" => "testuser" } } }
    posted_payload = nil
    client.define_singleton_method(:post) do |_endpoint, body|
      posted_payload = JSON.parse(body)
      { "data" => { "id" => "999" } }
    end

    service = TwitterService.new
    media_uploader = service.instance_variable_get(:@media_uploader)
    uploads = 0

    service.define_singleton_method(:create_client) { client }
    media_uploader.define_singleton_method(:upload) do |_client, _image|
      uploads += 1
      "media-#{uploads}"
    end

    result = service.post(article)

    assert_equal 1, uploads
    assert_equal [ "media-1" ], posted_payload.dig("media", "media_ids")
    assert_equal "https://x.com/testuser/status/999", result
  end

  test "post only uploads a single remote animated gif for twitter" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    remote_gif = RemoteImageWrapper.new("https://example.com/animated.gif?source=test")

    jpeg_blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("jpeg"),
      filename: "photo.jpg",
      content_type: "image/jpeg"
    )

    article = create_published_article
    article.define_singleton_method(:all_image_attachments) { |_limit| [ remote_gif, jpeg_blob ] }

    client = Object.new
    client.define_singleton_method(:get) { |_endpoint| { "data" => { "username" => "testuser" } } }
    posted_payload = nil
    client.define_singleton_method(:post) do |_endpoint, body|
      posted_payload = JSON.parse(body)
      { "data" => { "id" => "999" } }
    end

    service = TwitterService.new
    media_uploader = service.instance_variable_get(:@media_uploader)
    uploads = 0

    service.define_singleton_method(:create_client) { client }
    media_uploader.define_singleton_method(:upload) do |_client, _image|
      uploads += 1
      "media-#{uploads}"
    end

    result = service.post(article)

    assert_equal 1, uploads
    assert_equal [ "media-1" ], posted_payload.dig("media", "media_ids")
    assert_equal "https://x.com/testuser/status/999", result
  end

  test "post returns url when media upload succeeds" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    article = create_published_article
    article.define_singleton_method(:all_image_attachments) { |_limit| [ :image ] }

    client = Object.new
    client.define_singleton_method(:get) { |_endpoint| { "data" => { "username" => "testuser" } } }
    client.define_singleton_method(:post) do |endpoint, body|
      { "data" => { "id" => "123" } }
    end

    service = TwitterService.new
    media_uploader = service.instance_variable_get(:@media_uploader)
    media_uploader.define_singleton_method(:upload) { |_client, _image| "media_id" }

    result = service.stub(:create_client, client) do
      service.post(article)
    end

    assert_equal "https://x.com/testuser/status/123", result
  end

  test "post falls back to text only when media tweet fails" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    article = create_published_article
    article.define_singleton_method(:all_image_attachments) { |_limit| [ :image ] }

    post_calls = 0
    client = Object.new
    client.define_singleton_method(:get) { |_endpoint| { "data" => { "username" => "testuser" } } }
    client.define_singleton_method(:post) do |endpoint, body|
      post_calls += 1
      if post_calls == 1
        { "errors" => [ { "message" => "media failed" } ] }
      else
        { "data" => { "id" => "456" } }
      end
    end

    service = TwitterService.new
    media_uploader = service.instance_variable_get(:@media_uploader)
    media_uploader.define_singleton_method(:upload) { |_client, _image| "media_id" }

    result = service.stub(:create_client, client) do
      service.post(article)
    end

    assert_equal "https://x.com/testuser/status/456", result
    assert_equal 2, post_calls
  end

  test "post re-raises transient errors from text-only fallback for job retry" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    article = create_published_article
    article.define_singleton_method(:all_image_attachments) { |_limit| [ :image ] }

    post_calls = 0
    client = Object.new
    client.define_singleton_method(:get) { |_endpoint| { "data" => { "username" => "testuser" } } }
    client.define_singleton_method(:post) do |_endpoint, _body|
      post_calls += 1
      if post_calls == 1
        { "errors" => [ { "message" => "media failed" } ] }
      else
        raise Net::OpenTimeout
      end
    end

    service = TwitterService.new
    media_uploader = service.instance_variable_get(:@media_uploader)
    media_uploader.define_singleton_method(:upload) { |_client, _image| "media_id" }

    assert_raises(Net::OpenTimeout) do
      service.stub(:create_client, client) { service.post(article) }
    end
  end

  test "post returns nil when text-only fallback hits a permanent error" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    article = create_published_article
    article.define_singleton_method(:all_image_attachments) { |_limit| [ :image ] }

    post_calls = 0
    client = Object.new
    client.define_singleton_method(:get) { |_endpoint| { "data" => { "username" => "testuser" } } }
    client.define_singleton_method(:post) do |_endpoint, _body|
      post_calls += 1
      if post_calls == 1
        { "errors" => [ { "message" => "media failed" } ] }
      else
        raise "boom"
      end
    end

    service = TwitterService.new
    media_uploader = service.instance_variable_get(:@media_uploader)
    media_uploader.define_singleton_method(:upload) { |_client, _image| "media_id" }

    result = service.stub(:create_client, client) { service.post(article) }

    assert_nil result
  end

  test "post returns nil when tweet fails without media" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    article = create_published_article
    article.define_singleton_method(:all_image_attachments) { |_limit| [] }

    client = Object.new
    client.define_singleton_method(:get) { |_endpoint| { "data" => { "username" => "testuser" } } }
    client.define_singleton_method(:post) { |_endpoint, _body| { "errors" => [ { "message" => "bad request" } ] } }

    service = TwitterService.new
    result = service.stub(:create_client, client) { service.post(article) }

    assert_nil result
  end

  test "post continues when users/me lookup raises" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    article = create_published_article
    article.define_singleton_method(:all_image_attachments) { |_limit| [] }

    client = Object.new
    client.define_singleton_method(:get) { |_endpoint| raise "boom" }
    post_calls = 0
    client.define_singleton_method(:post) do |_endpoint, _body|
      post_calls += 1
      { "data" => { "id" => "123" } }
    end

    service = TwitterService.new
    result = service.stub(:create_client, client) { service.post(article) }

    assert_equal "https://x.com/i/web/status/123", result
    assert_equal 1, post_calls
  end

  test "post returns nil when tweet creation raises" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    article = create_published_article
    article.define_singleton_method(:all_image_attachments) { |_limit| [] }

    client = Object.new
    client.define_singleton_method(:get) { |_endpoint| { "data" => { "username" => "testuser" } } }
    client.define_singleton_method(:post) { |_endpoint, _body| raise "boom" }

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

  test "quote_tweet_id_for_article accepts x.com and twitter.com status urls" do
    service = TwitterService.new

    article = create_published_article(source_url: "https://x.com/user/status/123")
    assert_equal "123", service.send(:quote_tweet_id_for_article, article)

    article.update!(source_url: "https://twitter.com/user/status/456")
    assert_equal "456", service.send(:quote_tweet_id_for_article, article)

    article.update!(source_url: "https://mobile.x.com/i/status/789")
    assert_equal "789", service.send(:quote_tweet_id_for_article, article)

    article.update!(source_url: "https://example.com/post/456")
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
    settings = Crosspost.for("twitter")
    uploader = TwitterApi::MediaUploader.new(settings)
    remote_image = RemoteImageWrapper.new("http://example.com/image.jpg")

    uploader.stub(:download_remote_image, [ "image-data", "image/png" ]) do
      temp_file = uploader.send(:create_temp_file, remote_image)

      assert temp_file.is_a?(Tempfile)
      assert_equal ".png", File.extname(temp_file.path)
      assert_equal "image-data", temp_file.read
      temp_file.close
      temp_file.unlink
    end
  end

  test "media_uploader compresses oversized images until they fit twitter limit" do
    settings = Crosspost.for("twitter")
    uploader = TwitterApi::MediaUploader.new(settings)
    image_data = "a" * (TwitterApi::MediaUploader::MAX_IMAGE_SIZE + 1)
    temp_dir = Rails.root.join("tmp", "twitter_uploads")
    FileUtils.mkdir_p(temp_dir)
    before = Dir.glob(temp_dir.join("{original,compressed}_*"))

    with_stubbed_method(Vips::Image, :new_from_file, FakeTwitterVipsImage.new) do
      result_data, result_type = uploader.send(:resize_image_if_needed, image_data, "image/png")

      assert result_data.bytesize <= TwitterApi::MediaUploader::MAX_IMAGE_SIZE
      assert_equal "image/jpeg", result_type
    end

    after = Dir.glob(temp_dir.join("{original,compressed}_*"))
    assert_equal before, after
  end

  test "media_uploader keeps gifs under twitter gif limit unchanged" do
    settings = Crosspost.for("twitter")
    uploader = TwitterApi::MediaUploader.new(settings)
    gif_data = "g" * (6.megabytes)

    result_data, result_type = uploader.send(:resize_image_if_needed, gif_data, "image/gif")

    assert_equal gif_data, result_data
    assert_equal "image/gif", result_type
  end

  test "media_uploader rejects gifs larger than twitter gif limit" do
    settings = Crosspost.for("twitter")
    uploader = TwitterApi::MediaUploader.new(settings)
    gif_data = "g" * (TwitterApi::MediaUploader::MAX_GIF_SIZE + 1)

    assert_nil uploader.send(:resize_image_if_needed, gif_data, "image/gif")
  end

  test "media_uploader autorotates oversized jpeg images before stripping metadata" do
    settings = Crosspost.for("twitter")
    uploader = TwitterApi::MediaUploader.new(settings)

    result_data, result_type = uploader.send(:resize_image_if_needed, oversized_oriented_jpeg_data, "image/jpeg")
    result_image = Vips::Image.new_from_buffer(result_data, "")
    orientation = result_image.get("orientation") rescue nil

    assert result_data.bytesize <= TwitterApi::MediaUploader::MAX_IMAGE_SIZE
    assert_equal "image/jpeg", result_type
    assert_operator result_image.width, :>, result_image.height
    assert_nil orientation
  end

  test "media_uploader flattens oversized transparent images onto white before jpeg save" do
    settings = Crosspost.for("twitter")
    uploader = TwitterApi::MediaUploader.new(settings)

    result_data, result_type = uploader.send(:resize_image_if_needed, oversized_transparent_png_data, "image/png")
    pixel = Vips::Image.new_from_buffer(result_data, "").getpoint(0, 0).first(3)

    assert result_data.bytesize <= TwitterApi::MediaUploader::MAX_IMAGE_SIZE
    assert_equal "image/jpeg", result_type
    assert pixel.all? { |channel| channel > 240 }, "Expected white background, got #{pixel.inspect}"
  end

  test "media_uploader uploads to twitter with chunked upload and returns media id" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )
    settings = Crosspost.for("twitter")
    uploader = TwitterApi::MediaUploader.new(settings)

    file = Tempfile.new([ "upload", ".jpg" ])
    file.write("data")
    file.rewind

    captured_args = nil
    X::MediaUploader.stub(:chunked_upload, ->(**kwargs) {
      captured_args = kwargs
      { "id" => "media123" }
    }) do
      media_id = uploader.send(:upload_to_twitter, nil, file.path)

      assert_equal "media123", media_id
      assert_equal "tweet_image", captured_args[:media_category]
      assert_equal "image/jpeg", captured_args[:media_type]
    end
  ensure
    file&.close
    file&.unlink
  end

  test "media_uploader waits for processing when chunked upload returns processing info" do
    settings = Crosspost.for("twitter")
    uploader = TwitterApi::MediaUploader.new(settings)

    file = Tempfile.new([ "upload", ".gif" ])
    file.write("GIF89a")
    file.rewind

    await_called = false
    captured_args = nil
    X::MediaUploader.stub(:chunked_upload, ->(**kwargs) {
      captured_args = kwargs
      { "id" => "media123", "processing_info" => { "state" => "pending", "check_after_secs" => 1 } }
    }) do
      X::MediaUploader.stub(:await_processing!, ->(client:, media:) {
        await_called = true
        assert_equal "media123", media["id"]
        { "id" => "media123", "processing_info" => { "state" => "succeeded" } }
      }) do
        media_id = uploader.send(:upload_to_twitter, Object.new, file.path)

        assert_equal "media123", media_id
        assert await_called
        assert_equal "tweet_gif", captured_args[:media_category]
        assert_equal "image/gif", captured_args[:media_type]
      end
    end
  ensure
    file&.close
    file&.unlink
  end

  test "media_uploader download_remote_image returns data for relative urls" do
    settings = Crosspost.for("twitter")
    uploader = TwitterApi::MediaUploader.new(settings)

    remote_image = Object.new
    remote_image.define_singleton_method(:url) { "/image.png" }

    response = Net::HTTPSuccess.new("1.1", "200", "OK")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "image-data")
    response["content-type"] = "image/png"

    uploader.stub(:fetch_with_redirect, response) do
      data, content_type = uploader.send(:download_remote_image, remote_image)

      assert_equal "image-data", data
      assert_equal "image/png", content_type
    end
  end

  test "verify succeeds with valid oauth1 credentials" do
    client = Object.new
    client.define_singleton_method(:get) { |_endpoint| { "data" => { "id" => "123" } } }

    X::Client.stub(:new, client) do
      service = TwitterService.new
      result = service.verify(
        access_token: "token",
        access_token_secret: "token-secret",
        api_key: "api-key",
        api_key_secret: "api-key-secret"
      )

      assert_equal true, result[:success]
    end
  end


  test "post retries when create tweet is rate limited" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    article = create_published_article
    article.define_singleton_method(:all_image_attachments) { |_limit| [] }

    service = TwitterService.new
    post_calls = 0
    client = Object.new
    client.define_singleton_method(:get) { |_endpoint| { "data" => { "username" => "testuser" } } }
    client.define_singleton_method(:post) do |_endpoint, _body|
      post_calls += 1
      raise "429 Too Many Requests" if post_calls == 1

      { "data" => { "id" => "123" } }
    end

    service.stub(:create_client, client) do
      service.stub(:sleep, nil) do
        service.stub(:calculate_backoff_time, 0) do
          result = service.post(article)

          assert_equal "https://x.com/testuser/status/123", result
          assert_equal 2, post_calls
        end
      end
    end
  end

  test "fetch_comments aggregates replies and quote tweets" do
    Crosspost.for("twitter").update!(enabled: true)
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
    Crosspost.for("twitter").update!(enabled: true)
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
        access_token_secret: "token-secret",
        api_key: "api-key",
        api_key_secret: "api-key-secret"
      )

      assert_equal false, result[:success]
      assert_match "boom", result[:error]
    end
  end

  test "lookup_users_by_ids returns account_id to username mapping" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    service = TwitterService.new
    assert_respond_to service, :lookup_users_by_ids

    captured_endpoint = nil
    response = {
      "data" => [
        { "id" => "900", "username" => "alice" },
        { "id" => "901", "username" => "bob" }
      ]
    }
    rate_limiter = service.instance_variable_get(:@rate_limiter)
    rate_limiter.define_singleton_method(:make_request) do |_client, endpoint, **_opts|
      captured_endpoint = endpoint
      response
    end

    result = service.stub(:create_client, Object.new) do
      service.lookup_users_by_ids(%w[900 901])
    end

    assert_equal({ "900" => "alice", "901" => "bob" }, result[:users])
    assert_nil result[:rate_limit]
    assert_includes captured_endpoint, "ids=900,901"
    assert_includes captured_endpoint, "user.fields=username"
  end

  test "lookup_users_by_ids returns empty hash for empty input" do
    service = TwitterService.new
    assert_respond_to service, :lookup_users_by_ids

    service.define_singleton_method(:create_client) do
      flunk "create_client should not be called for empty input"
    end

    assert_equal({ users: {}, rate_limit: nil, retry_at: nil }, service.lookup_users_by_ids([]))
  end

  test "lookup_users_by_ids surfaces persistent rate limiting" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    service = TwitterService.new
    assert_respond_to service, :lookup_users_by_ids

    rate_limit = { limit: 300, remaining: 0, reset_at: 20.minutes.from_now.change(usec: 0) }
    rate_limiter = service.instance_variable_get(:@rate_limiter)
    rate_limiter.define_singleton_method(:make_request) do |_client, _endpoint, **_opts|
      raise "429 Too Many Requests"
    end
    rate_limiter.define_singleton_method(:rate_limit_info_from_error) do |_error, _wait_time|
      rate_limit
    end

    result = service.stub(:create_client, Object.new) do
      service.lookup_users_by_ids([ "900" ])
    end

    assert_equal({}, result[:users])
    assert_equal rate_limit, result[:rate_limit]
    assert_nil result[:retry_at]
  end

  test "lookup_users_by_ids requests a retry on transient failures" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    service = TwitterService.new
    assert_respond_to service, :lookup_users_by_ids

    rate_limiter = service.instance_variable_get(:@rate_limiter)
    rate_limiter.define_singleton_method(:make_request) do |_client, _endpoint, **_opts|
      raise X::NetworkError, "temporary outage"
    end

    freeze_time do
      result = service.stub(:create_client, Object.new) do
        service.lookup_users_by_ids([ "900" ])
      end

      assert_equal({}, result[:users])
      assert_nil result[:rate_limit]
      assert_equal 15.minutes.from_now, result[:retry_at]
    end
  end

  test "lookup_users_by_ids surfaces permanent failures" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    service = TwitterService.new
    assert_respond_to service, :lookup_users_by_ids

    rate_limiter = service.instance_variable_get(:@rate_limiter)
    rate_limiter.define_singleton_method(:make_request) do |_client, _endpoint, **_opts|
      raise "boom"
    end

    result = service.stub(:create_client, Object.new) do
      service.lookup_users_by_ids([ "900" ])
    end

    assert_equal({}, result[:users])
    assert_nil result[:rate_limit]
    assert_nil result[:retry_at]
    assert_equal "boom", result[:error_message]
  end

  test "create_client builds x client with oauth1 credentials" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )

    captured_args = nil
    fake_client = Object.new

    service = TwitterService.new
    X::Client.stub(:new, lambda { |**args|
      captured_args = args
      fake_client
    }) do
      assert_equal fake_client, service.send(:create_client)
    end

    assert_equal "api_key", captured_args[:api_key]
    assert_equal "api_key_secret", captured_args[:api_key_secret]
    assert_equal "access_token", captured_args[:access_token]
    assert_equal "access_token_secret", captured_args[:access_token_secret]
  end

  test "media_uploader returns nil when file missing" do
    settings = Crosspost.for("twitter")
    uploader = TwitterApi::MediaUploader.new(settings)

    assert_nil uploader.send(:upload_to_twitter, nil, "/tmp/does-not-exist.jpg")
  end

  test "media_uploader returns nil when response missing media id" do
    Crosspost.for("twitter").update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )
    settings = Crosspost.for("twitter")
    uploader = TwitterApi::MediaUploader.new(settings)

    file = Tempfile.new([ "upload", ".jpg" ])
    file.write("data")
    file.rewind

    X::MediaUploader.stub(:chunked_upload, {}) do
      assert_nil uploader.send(:upload_to_twitter, nil, file.path)
    end
  ensure
    file&.close
    file&.unlink
  end

  test "media_uploader re-raises transient network errors for job retry" do
    settings = Crosspost.for("twitter")
    uploader = TwitterApi::MediaUploader.new(settings)

    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("data"),
      filename: "test.jpg",
      content_type: "image/jpeg"
    )

    X::MediaUploader.stub(:chunked_upload, ->(**_kwargs) { raise Net::OpenTimeout }) do
      assert_raises(Net::OpenTimeout) { uploader.upload(Object.new, blob) }
    end
  end

  test "media_uploader returns nil on permanent upload errors" do
    settings = Crosspost.for("twitter")
    uploader = TwitterApi::MediaUploader.new(settings)

    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("data"),
      filename: "test.jpg",
      content_type: "image/jpeg"
    )

    X::MediaUploader.stub(:chunked_upload, ->(**_kwargs) { raise "boom" }) do
      assert_nil uploader.upload(Object.new, blob)
    end
  end

  test "media_uploader returns nil for non-image blob" do
    settings = Crosspost.for("twitter")
    uploader = TwitterApi::MediaUploader.new(settings)

    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("data"),
      filename: "test.txt",
      content_type: "text/plain"
    )

    assert_nil uploader.send(:create_temp_file, blob)
  end

  test "media_uploader download_remote_image returns nil on non-success response" do
    settings = Crosspost.for("twitter")
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

  test "fetch_quote_tweets escapes the search query exactly once" do
    Crosspost.for("twitter").update!(enabled: true)
    service = TwitterService.new
    rate_limiter = service.instance_variable_get(:@rate_limiter)

    captured_endpoint = nil
    rate_limiter.define_singleton_method(:make_request_with_info) do |_client, endpoint, **_opts|
      captured_endpoint = endpoint
      [ nil, nil ]
    end

    post_url = "https://x.com/user/status/123"
    service.send(:fetch_quote_tweets, Object.new, post_url, "123", nil)

    escaped_query = captured_endpoint.split("query=", 2).last.split("&").first
    assert_equal "url:#{post_url} is:quote", CGI.unescape(escaped_query)
    refute_includes captured_endpoint, "%25"
  end

  test "process_tweets skips malformed tweets without dropping the batch" do
    service = TwitterService.new

    response = {
      "data" => [
        {
          "id" => "ok",
          "author_id" => "u1",
          "text" => "fine",
          "created_at" => Time.current.iso8601
        },
        {
          "id" => "bad",
          "author_id" => "u1",
          "text" => "no timestamp",
          "created_at" => nil
        }
      ],
      "includes" => {
        "users" => [
          { "id" => "u1", "username" => "alice", "name" => "Alice", "profile_image_url" => "http://example.com/a.png" }
        ]
      }
    }

    comments = service.process_tweets(response, "root")

    assert_equal 1, comments.length
    assert_equal "ok", comments.first[:external_id]
  end

  test "rate_limit_error? uses error class and status, not a bare 429 substring" do
    rate_limiter = TwitterApi::RateLimiter.new

    response = Net::HTTPTooManyRequests.new("1.1", "429", "Too Many Requests")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "{}")
    x_error = X::TooManyRequests.new(response: response)

    assert rate_limiter.rate_limit_error?(x_error)
    assert rate_limiter.rate_limit_error?(RuntimeError.new("Rate limit exceeded"))
    assert rate_limiter.rate_limit_error?(RuntimeError.new("429 Too Many Requests"))
    refute rate_limiter.rate_limit_error?(RuntimeError.new("order 429 confirmed"))
  end

  test "with_retry does not sleep when retry_after exceeds the sleep cap" do
    rate_limiter = TwitterApi::RateLimiter.new

    response = Net::HTTPTooManyRequests.new("1.1", "429", "Too Many Requests")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "{}")
    error = X::TooManyRequests.new(response: response)
    error.define_singleton_method(:retry_after) { 3600 }

    slept = []
    calls = 0
    rate_limiter.stub(:sleep, ->(seconds) { slept << seconds }) do
      assert_raises(X::TooManyRequests) do
        rate_limiter.with_retry(max_retries: 3) do
          calls += 1
          raise error
        end
      end
    end

    assert_equal 1, calls
    assert_empty slept
  end

  test "fetch_username caches the username per access token and skips nil results" do
    memory_cache = ActiveSupport::Cache::MemoryStore.new
    service = TwitterService.new
    service.instance_variable_set(:@settings, Crosspost.new(platform: "twitter", access_token: "token-a"))

    calls = 0
    fake_client = Object.new
    fake_client.define_singleton_method(:get) do |_path|
      calls += 1
      { "data" => { "username" => "alice" } }
    end

    Rails.stub(:cache, memory_cache) do
      assert_equal "alice", service.send(:fetch_username, fake_client)
      assert_equal "alice", service.send(:fetch_username, fake_client)
      assert_equal 1, calls # second call served from cache

      # nil results must not be cached (skip_nil), so a retry re-calls the API
      # (use a different access token to get a fresh cache key)
      service.instance_variable_set(:@settings, Crosspost.new(platform: "twitter", access_token: "token-nil"))
      nil_calls = 0
      nil_client = Object.new
      nil_client.define_singleton_method(:get) do |_path|
        nil_calls += 1
        {}
      end
      assert_nil service.send(:fetch_username, nil_client)
      assert_nil service.send(:fetch_username, nil_client)
      assert_equal 2, nil_calls

      # switching the access token must not reuse the old account's username
      service.instance_variable_set(:@settings, Crosspost.new(platform: "twitter", access_token: "token-b"))
      assert_equal "alice", service.send(:fetch_username, fake_client)
      assert_equal 2, calls
    end
  end
end
