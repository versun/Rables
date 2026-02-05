# frozen_string_literal: true

require "test_helper"

unless defined?(Vips)
  module Vips
    class Image
      def self.new_from_file(*)
      end
    end
  end
end

class FakeVipsImage
  attr_reader :width, :height

  def initialize(width: 1200, height: 800, data: "jpegdata")
    @width = width
    @height = height
    @data = data
  end

  def write_to_file(path, **_options)
    File.binwrite(path, @data)
  end

  def resize(_scale)
    self
  end
end

class BlueskyServiceTest < ActiveSupport::TestCase
  private

  def with_stubbed_method(object, method_name, replacement = nil, &block)
    original = object.method(method_name) if object.respond_to?(method_name)
    object.define_singleton_method(method_name) do |*args, &method_block|
      if replacement.respond_to?(:call)
        replacement.call(*args, &method_block)
      else
        replacement
      end
    end
    yield
  ensure
    if original
      object.define_singleton_method(method_name) { |*args, &method_block| original.call(*args, &method_block) }
    else
      object.singleton_class.remove_method(method_name)
    end
  end

  public

  test "verify fails fast when credentials are blank" do
    service = BlueskyService.new
    result = service.verify({})

    assert_equal false, result[:success]
    assert_match "App Password", result[:error]
  end

  test "post returns nil when crosspost is disabled" do
    Crosspost.bluesky.update!(enabled: false)
    service = BlueskyService.new

    assert_nil service.post(create_published_article)
  end

  test "resize_image_if_needed returns jpeg data and content type for oversized image" do
    service = BlueskyService.new
    image_data = "a" * (BlueskyService::MAX_IMAGE_SIZE + 1)
    temp_dir = Rails.root.join("tmp", "bluesky_uploads")
    FileUtils.mkdir_p(temp_dir)
    before = Dir.glob(temp_dir.join("{original,compressed}_*"))

    with_stubbed_method(Vips::Image, :new_from_file, FakeVipsImage.new(data: "jpegdata")) do
      result_data, result_type = service.send(:resize_image_if_needed, image_data, "image/png")
      assert_equal "jpegdata", result_data
      assert_equal "image/jpeg", result_type
    end

    after = Dir.glob(temp_dir.join("{original,compressed}_*"))
    assert_equal before, after
  end

  test "resize_image_if_needed cleans temp files on failure and preserves content type" do
    service = BlueskyService.new
    image_data = "a" * (BlueskyService::MAX_IMAGE_SIZE + 1)
    temp_dir = Rails.root.join("tmp", "bluesky_uploads")
    FileUtils.mkdir_p(temp_dir)
    before = Dir.glob(temp_dir.join("{original,compressed}_*"))

    with_stubbed_method(Vips::Image, :new_from_file, ->(*) { raise StandardError, "boom" }) do
      result_data, result_type = service.send(:resize_image_if_needed, image_data, "image/png")
      assert_equal image_data, result_data
      assert_equal "image/png", result_type
    end

    after = Dir.glob(temp_dir.join("{original,compressed}_*"))
    assert_equal before, after
  end

  test "upload_blob uses resized content type for upload" do
    service = BlueskyService.new
    blob = Struct.new(:download, :content_type).new("raw", "image/png")
    resized_data = "jpegdata"
    request_seen = nil

    upload_response = Struct.new(:body) do
      def is_a?(klass)
        klass == Net::HTTPSuccess
      end
    end.new('{"blob":{"ref":"cid"}}')

    fake_http = Class.new do
      def initialize(response, capture)
        @response = response
        @capture = capture
      end

      def request(req)
        @capture.call(req)
        @response
      end
    end.new(upload_response, ->(req) { request_seen = req })

    with_stubbed_method(service, :verify_tokens, nil) do
      with_stubbed_method(service, :resize_image_if_needed, [ resized_data, "image/jpeg" ]) do
        with_stubbed_method(Net::HTTP, :start, ->(*_args, &block) { block.call(fake_http) }) do
          service.send(:upload_blob, blob)
        end
      end
    end

    assert_equal "image/jpeg", request_seen["Content-Type"]
    assert_equal resized_data, request_seen.body
  end

  test "upload_remote_image uses resized content type for upload" do
    service = BlueskyService.new
    resized_data = "jpegdata"
    request_seen = nil

    download_response = Struct.new(:body, :headers) do
      def [](key)
        headers[key]
      end

      def is_a?(klass)
        klass == Net::HTTPSuccess
      end
    end.new("raw", { "content-type" => "image/png" })

    upload_response = Struct.new(:body) do
      def is_a?(klass)
        klass == Net::HTTPSuccess
      end
    end.new('{"blob":{"ref":"cid"}}')

    fake_http = Class.new do
      def initialize(response, capture)
        @response = response
        @capture = capture
      end

      def request(req)
        @capture.call(req)
        @response
      end
    end.new(upload_response, ->(req) { request_seen = req })

    with_stubbed_method(service, :verify_tokens, nil) do
      with_stubbed_method(service, :fetch_with_redirect, download_response) do
        with_stubbed_method(service, :resize_image_if_needed, [ resized_data, "image/jpeg" ]) do
          with_stubbed_method(Net::HTTP, :start, ->(*_args, &block) { block.call(fake_http) }) do
            service.send(:upload_remote_image, "https://example.com/image.png")
          end
        end
      end
    end

    assert_equal "image/jpeg", request_seen["Content-Type"]
    assert_equal resized_data, request_seen.body
  end
  test "post uploads all images for bluesky" do
    Crosspost.bluesky.update!(
      enabled: true,
      username: "tester",
      app_password: "app-password"
    )
    service = BlueskyService.new
    article = create_published_article

    images = [ Object.new, Object.new, Object.new ]
    article.define_singleton_method(:all_image_attachments) { |_limit| images }

    captured_images = nil
    embed_payload = { "$type" => "app.bsky.embed.images", "images" => [] }
    service.define_singleton_method(:upload_images_embed) do |attachables|
      captured_images = attachables
      embed_payload
    end

    captured_embed = nil
    service.define_singleton_method(:skeet) do |_message, embed = nil|
      captured_embed = embed
      "https://bsky.app/profile/tester/post/123"
    end

    result = service.post(article)

    assert_equal images, captured_images
    assert_equal embed_payload, captured_embed
    assert_equal "https://bsky.app/profile/tester/post/123", result
  end
end
