# frozen_string_literal: true

require "test_helper"
require "minitest/mock"
require "stringio"
require "vips"

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

  def autorot
    self
  end

  def has_alpha?
    false
  end
end

class BlueskyServiceTest < ActiveSupport::TestCase
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

  public

  test "verify fails fast when credentials are blank" do
    service = BlueskyService.new
    result = service.verify({})

    assert_equal false, result[:success]
    assert_match "App Password", result[:error]
  end

  test "post returns nil when crosspost is disabled" do
    Crosspost.for("bluesky").update!(enabled: false)
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

  test "resize_image_if_needed cleans temp files and returns nil on failure" do
    service = BlueskyService.new
    image_data = "a" * (BlueskyService::MAX_IMAGE_SIZE + 1)
    temp_dir = Rails.root.join("tmp", "bluesky_uploads")
    FileUtils.mkdir_p(temp_dir)
    before = Dir.glob(temp_dir.join("{original,compressed}_*"))

    with_stubbed_method(Vips::Image, :new_from_file, ->(*) { raise StandardError, "boom" }) do
      assert_nil service.send(:resize_image_if_needed, image_data, "image/png")
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
    Crosspost.for("bluesky").update!(
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

  test "link_facets returns facets for valid urls and skips invalid" do
    service = BlueskyService.new
    message = "Visit http://example.com and http://["

    facets = service.send(:link_facets, message)

    assert_equal 2, facets.length
    assert_equal "http://example.com", facets.first["features"].first["uri"]
  end

  test "extract_post_uri_from_url resolves handle" do
    response = Net::HTTPSuccess.new("1.1", "200", "OK")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, { did: "did:plc:abc123" }.to_json)

    original_method = Net::HTTP.method(:get_response)
    Net::HTTP.define_singleton_method(:get_response) { |_uri| response }

    service = BlueskyService.new
    uri = service.send(:extract_post_uri_from_url, "https://bsky.app/profile/test/post/abc")

    assert_equal "at://did:plc:abc123/app.bsky.feed.post/abc", uri
  ensure
    Net::HTTP.define_singleton_method(:get_response, original_method) if original_method
  end

  test "fetch_comments flattens nested replies" do
    Crosspost.for("bluesky").update!(enabled: true)
    service = BlueskyService.new

    thread_data = {
      thread: {
        post: { uri: "at://did:plc:root/app.bsky.feed.post/root" },
        replies: [
          {
            post: {
              uri: "at://did:plc:root/app.bsky.feed.post/reply1",
              author: { "displayName" => "Alice", "handle" => "alice" },
              record: { "text" => "First reply", "createdAt" => Time.current.iso8601 }
            }
          },
          {
            post: {
              uri: "at://did:plc:root/app.bsky.feed.post/reply2",
              author: { "displayName" => "Bob", "handle" => "bob" },
              record: { "text" => "Second reply", "createdAt" => Time.current.iso8601 }
            },
            replies: [
              {
                post: {
                  uri: "at://did:plc:root/app.bsky.feed.post/reply3",
                  author: { "displayName" => "Carol", "handle" => "carol" },
                  record: { "text" => "Nested reply", "createdAt" => Time.current.iso8601 }
                }
              }
            ]
          }
        ]
      }
    }

    response = Net::HTTPSuccess.new("1.1", "200", "OK")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, thread_data.to_json)
    response["RateLimit-Limit"] = "3000"
    response["RateLimit-Remaining"] = "50"
    response["RateLimit-Reset"] = (Time.current + 60).to_i.to_s

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:open_timeout=) { |_val| }
    fake_http.define_singleton_method(:read_timeout=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| response }

    def service.verify_tokens; true; end
    def service.extract_post_uri_from_url(_url); "at://did:plc:root/app.bsky.feed.post/root"; end

    original_new = Net::HTTP.method(:new)
    Net::HTTP.define_singleton_method(:new) { |_host, *_args| fake_http }

    result = service.fetch_comments("https://bsky.app/profile/test/post/root")

    assert_equal 3, result[:comments].length
    assert_nil result[:comments].first[:parent_external_id]
    assert_equal "reply2", result[:comments][1][:external_id]
    assert_equal "reply2", result[:comments][2][:parent_external_id]
  ensure
    Net::HTTP.define_singleton_method(:new, original_new) if original_new
  end

  test "upload_images_embed returns nil for unknown attachable" do
    service = BlueskyService.new

    assert_nil service.send(:upload_images_embed, [ Object.new ])
  end

  test "post logs activity on success" do
    Crosspost.for("bluesky").update!(enabled: true)
    service = BlueskyService.new
    article = create_published_article
    article.define_singleton_method(:first_image_attachment) { nil }

    def service.skeet(_msg, _embed); "https://bsky.app/profile/test/post/1"; end

    assert_difference "ActivityLog.count", 1 do
      result = service.post(article)
      assert_equal "https://bsky.app/profile/test/post/1", result
    end
  end

  test "post returns nil when skeet raises" do
    Crosspost.for("bluesky").update!(enabled: true)
    service = BlueskyService.new
    article = create_published_article
    article.define_singleton_method(:first_image_attachment) { nil }

    service.define_singleton_method(:skeet) { |_msg, _embed| raise "boom" }

    assert_difference "ActivityLog.count", 1 do
      assert_nil service.post(article)
    end
  end

  test "post re-raises transient network errors for job retry" do
    Crosspost.for("bluesky").update!(enabled: true)
    service = BlueskyService.new
    article = create_published_article
    article.define_singleton_method(:first_image_attachment) { nil }

    service.define_singleton_method(:skeet) { |_msg, _embed| raise Net::OpenTimeout }

    assert_raises(Net::OpenTimeout) { service.post(article) }
  end

  test "verify returns success when tokens are valid" do
    service = BlueskyService.new

    def service.verify_tokens; true; end

    result = service.verify(username: "tester", app_password: "pass", server_url: "https://bsky.social/xrpc")

    assert_equal true, result[:success]
  end

  test "verify returns failure on error" do
    service = BlueskyService.new

    def service.verify_tokens; raise "bad"; end

    result = service.verify(username: "tester", app_password: "pass", server_url: "https://bsky.social/xrpc")

    assert_equal false, result[:success]
    assert_match "Bluesky verification failed", result[:error]
  end

  test "skeet posts and returns profile url" do
    Crosspost.for("bluesky").update!(enabled: true, username: "tester")
    service = BlueskyService.new
    service.instance_variable_set(:@user_did, "did:plc:abc")

    captured_body = nil
    def service.verify_tokens; true; end
    def service.post_request(_url, body:, **_opts)
      @captured_body = body
      { "uri" => "at://did:plc:abc/app.bsky.feed.post/xyz" }
    end
    def service.captured_body; @captured_body; end

    result = service.skeet("hello world", { "$type" => "app.bsky.embed.images", "images" => [] })

    assert_equal "https://bsky.app/profile/tester/post/xyz", result
    assert service.captured_body[:record][:embed]
  end

  test "post uses embed when image present" do
    Crosspost.for("bluesky").update!(enabled: true)
    service = BlueskyService.new
    article = create_published_article
    article.define_singleton_method(:first_image_attachment) { :image }

    embed = { "$type" => "app.bsky.embed.images", "images" => [] }
    def service.skeet(_msg, _embed); "https://bsky.app/profile/test/post/1"; end

    result = service.post(article)

    assert_equal "https://bsky.app/profile/test/post/1", result
  end

  test "fetch_comments returns rate limit info on 429" do
    Crosspost.for("bluesky").update!(enabled: true)
    service = BlueskyService.new

    response = Net::HTTPTooManyRequests.new("1.1", "429", "Too Many Requests")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "{}")
    response["RateLimit-Limit"] = "3000"
    response["RateLimit-Remaining"] = "0"
    response["RateLimit-Reset"] = (Time.current + 60).to_i.to_s

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:open_timeout=) { |_val| }
    fake_http.define_singleton_method(:read_timeout=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| response }

    def service.verify_tokens; true; end
    def service.extract_post_uri_from_url(_url); "at://did:plc:abc/app.bsky.feed.post/xyz"; end

    original_new = Net::HTTP.method(:new)
    Net::HTTP.define_singleton_method(:new) { |_host, *_args| fake_http }

    result = service.fetch_comments("https://bsky.app/profile/test/post/xyz")

    assert_equal 0, result[:comments].length
    assert_equal 0, result[:rate_limit][:remaining]
  ensure
    Net::HTTP.define_singleton_method(:new, original_new) if original_new
  end

  test "post_request raises on non success response" do
    service = BlueskyService.new
    service.instance_variable_set(:@token, "token")

    response = Net::HTTPInternalServerError.new("1.1", "500", "Error")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "boom")

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:open_timeout=) { |_val| }
    fake_http.define_singleton_method(:read_timeout=) { |_val| }
    fake_http.define_singleton_method(:write_timeout=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| response }

    original_new = Net::HTTP.method(:new)
    Net::HTTP.define_singleton_method(:new) { |_host, *_args| fake_http }

    assert_raises TransientNetworkErrors::TransientServerError do
      service.send(:post_request, "https://example.com/test", body: {}, auth_token: false)
    end
  ensure
    Net::HTTP.define_singleton_method(:new, original_new) if original_new
  end

  test "post_request raises retryable error on 503 response" do
    service = BlueskyService.new
    service.instance_variable_set(:@token, "token")

    response = Net::HTTPServiceUnavailable.new("1.1", "503", "Service Unavailable")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "boom")

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:open_timeout=) { |_val| }
    fake_http.define_singleton_method(:read_timeout=) { |_val| }
    fake_http.define_singleton_method(:write_timeout=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| response }

    original_new = Net::HTTP.method(:new)
    Net::HTTP.define_singleton_method(:new) { |_host, *_args| fake_http }

    error = assert_raises(TransientNetworkErrors::TransientServerError) do
      service.send(:post_request, "https://example.com/test", body: {}, auth_token: false)
    end
    assert service.send(:transient_network_error?, error)
  ensure
    Net::HTTP.define_singleton_method(:new, original_new) if original_new
  end

  test "post_request raises retryable error on 429 response" do
    service = BlueskyService.new
    service.instance_variable_set(:@token, "token")

    response = Net::HTTPTooManyRequests.new("1.1", "429", "Too Many Requests")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "slow down")

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:open_timeout=) { |_val| }
    fake_http.define_singleton_method(:read_timeout=) { |_val| }
    fake_http.define_singleton_method(:write_timeout=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| response }

    original_new = Net::HTTP.method(:new)
    Net::HTTP.define_singleton_method(:new) { |_host, *_args| fake_http }

    error = assert_raises(TransientNetworkErrors::TransientServerError) do
      service.send(:post_request, "https://example.com/test", body: {}, auth_token: false)
    end
    assert service.send(:transient_network_error?, error)
  ensure
    Net::HTTP.define_singleton_method(:new, original_new) if original_new
  end

  test "post_request raises non-retryable error on 400 response" do
    service = BlueskyService.new
    service.instance_variable_set(:@token, "token")

    response = Net::HTTPBadRequest.new("1.1", "400", "Bad Request")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "bad request")

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:open_timeout=) { |_val| }
    fake_http.define_singleton_method(:read_timeout=) { |_val| }
    fake_http.define_singleton_method(:write_timeout=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| response }

    original_new = Net::HTTP.method(:new)
    Net::HTTP.define_singleton_method(:new) { |_host, *_args| fake_http }

    error = assert_raises(RuntimeError) do
      service.send(:post_request, "https://example.com/test", body: {}, auth_token: false)
    end
    refute service.send(:transient_network_error?, error)
  ensure
    Net::HTTP.define_singleton_method(:new, original_new) if original_new
  end

  test "verify_tokens refreshes expired token" do
    service = BlueskyService.new
    service.instance_variable_set(:@token, "token")
    service.instance_variable_set(:@token_expires_at, 1.minute.ago)

    refreshed = false
    def service.perform_token_refresh; @refreshed = true; end
    def service.refreshed; @refreshed; end

    service.send(:verify_tokens)

    assert service.refreshed
  end

  test "verify_tokens generates token when missing" do
    service = BlueskyService.new

    def service.generate_tokens; @generated = true; end
    def service.generated; @generated; end

    service.send(:verify_tokens)

    assert service.generated
  end

  test "upload_images_embed builds embed for remote image" do
    service = BlueskyService.new

    remote_image = RemoteImageWrapper.new("http://example.com/remote.png")

    def service.upload_remote_image(_url); { "ref" => "blob1" }; end
    embed = service.send(:upload_images_embed, [ remote_image ])

    assert_equal "app.bsky.embed.images", embed["$type"]
    assert_equal "blob1", embed["images"].first["image"]["ref"]
  end

  test "fetch_with_redirect follows redirects" do
    service = BlueskyService.new
    redirect = Net::HTTPFound.new("1.1", "302", "Found")
    redirect["location"] = "/next"
    success = Net::HTTPSuccess.new("1.1", "200", "OK")
    success.instance_variable_set(:@read, true)
    success.instance_variable_set(:@body, "ok")

    responses = [ redirect, success ]
    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:open_timeout=) { |_val| }
    fake_http.define_singleton_method(:read_timeout=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| responses.shift }

    original_new = Net::HTTP.method(:new)
    Net::HTTP.define_singleton_method(:new) { |_host, *_args| fake_http }

    result = service.send(:fetch_with_redirect, URI("http://example.com/start"))

    assert result.is_a?(Net::HTTPSuccess)
  ensure
    Net::HTTP.define_singleton_method(:new, original_new) if original_new
  end

  test "extract_post_uri_from_url returns nil on lookup failure" do
    response = Net::HTTPNotFound.new("1.1", "404", "Not Found")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "{}")

    original_method = Net::HTTP.method(:get_response)
    Net::HTTP.define_singleton_method(:get_response) { |_uri| response }

    service = BlueskyService.new
    result = service.send(:extract_post_uri_from_url, "https://bsky.app/profile/test/post/abc")

    assert_nil result
  ensure
    Net::HTTP.define_singleton_method(:get_response, original_method) if original_method
  end

  test "log_rate_limit_status creates activity log when low" do
    service = BlueskyService.new

    assert_difference "ActivityLog.count", 1 do
      service.send(:log_rate_limit_status, { remaining: 50, limit: 3000, reset_at: Time.current + 5.minutes })
    end
  end

  test "upload_blob uploads active storage blob" do
    Crosspost.for("bluesky").update!(enabled: true)
    service = BlueskyService.new
    service.instance_variable_set(:@token, "token")

    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("data"),
      filename: "test.jpg",
      content_type: "image/jpeg"
    )

    response = Net::HTTPSuccess.new("1.1", "200", "OK")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, { blob: { "ref" => "blob123" } }.to_json)

    fake_http = Object.new
    fake_http.define_singleton_method(:request) { |_req| response }

    def service.verify_tokens; true; end
    original_start = Net::HTTP.method(:start)
    Net::HTTP.define_singleton_method(:start) do |*_args, **_kwargs, &block|
      block.call(fake_http)
    end

    result = service.send(:upload_blob, blob)

    assert_equal({ "ref" => "blob123" }, result)
  ensure
    Net::HTTP.define_singleton_method(:start, original_start) if original_start
  end

  test "upload_remote_image returns blob data for relative urls" do
    Crosspost.for("bluesky").update!(enabled: true)
    service = BlueskyService.new
    service.instance_variable_set(:@token, "token")

    image_response = Net::HTTPSuccess.new("1.1", "200", "OK")
    image_response.instance_variable_set(:@read, true)
    image_response.instance_variable_set(:@body, "image-data")
    image_response["content-type"] = "image/png"

    upload_response = Net::HTTPSuccess.new("1.1", "200", "OK")
    upload_response.instance_variable_set(:@read, true)
    upload_response.instance_variable_set(:@body, { blob: { "ref" => "remote123" } }.to_json)

    fake_http = Object.new
    fake_http.define_singleton_method(:request) { |_req| upload_response }

    def service.verify_tokens; true; end
    def service.fetch_with_redirect(_uri); @image_response; end
    service.instance_variable_set(:@image_response, image_response)
    original_start = Net::HTTP.method(:start)
    Net::HTTP.define_singleton_method(:start) do |*_args, **_kwargs, &block|
      block.call(fake_http)
    end

    result = service.send(:upload_remote_image, "/images/test.png")

    assert_equal({ "ref" => "remote123" }, result)
  ensure
    Net::HTTP.define_singleton_method(:start, original_start) if original_start
  end

  test "post_request returns body for non-json responses" do
    service = BlueskyService.new

    response = Net::HTTPSuccess.new("1.1", "200", "OK")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "plain-ok")
    response.define_singleton_method(:content_type) { "text/plain" }

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:open_timeout=) { |_val| }
    fake_http.define_singleton_method(:read_timeout=) { |_val| }
    fake_http.define_singleton_method(:write_timeout=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| response }

    original_new = Net::HTTP.method(:new)
    Net::HTTP.define_singleton_method(:new) { |_host, *_args| fake_http }

    result = service.send(:post_request, "https://example.com/test", body: "raw", auth_token: false, content_type: "text/plain")

    assert_equal "plain-ok", result
  ensure
    Net::HTTP.define_singleton_method(:new, original_new) if original_new
  end

  test "upload_images_embed returns nil when remote image has no url" do
    service = BlueskyService.new

    remote_image = RemoteImageWrapper.new(nil)

    assert_nil service.send(:upload_images_embed, [ remote_image ])
  end

  test "upload_images_embed uses fallback filename on invalid url" do
    service = BlueskyService.new

    remote_image = RemoteImageWrapper.new("http://[")

    def service.upload_remote_image(_url); { "ref" => "blob1" }; end
    embed = service.send(:upload_images_embed, [ remote_image ])

    assert_equal "image.jpg", embed["images"].first["alt"]
  end

  test "upload_blob returns nil on non-success response" do
    Crosspost.for("bluesky").update!(enabled: true)
    service = BlueskyService.new
    service.instance_variable_set(:@token, "token")

    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("data"),
      filename: "test.jpg",
      content_type: "image/jpeg"
    )

    response = Net::HTTPInternalServerError.new("1.1", "500", "Error")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "boom")

    fake_http = Object.new
    fake_http.define_singleton_method(:request) { |_req| response }

    def service.verify_tokens; true; end
    original_start = Net::HTTP.method(:start)
    Net::HTTP.define_singleton_method(:start) do |*_args, **_kwargs, &block|
      block.call(fake_http)
    end

    assert_nil service.send(:upload_blob, blob)
  ensure
    Net::HTTP.define_singleton_method(:start, original_start) if original_start
  end

  test "upload_remote_image returns nil on failed download" do
    Crosspost.for("bluesky").update!(enabled: true)
    service = BlueskyService.new
    service.instance_variable_set(:@token, "token")

    response = Net::HTTPNotFound.new("1.1", "404", "Not Found")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "missing")

    def service.verify_tokens; true; end
    def service.fetch_with_redirect(_uri); @response; end
    service.instance_variable_set(:@response, response)
    assert_nil service.send(:upload_remote_image, "http://example.com/missing.png")
  end

  test "upload_blob re-raises transient network errors for job retry" do
    service = BlueskyService.new
    blob = Struct.new(:download, :content_type).new("raw", "image/png")

    with_stubbed_method(service, :verify_tokens, nil) do
      with_stubbed_method(service, :resize_image_if_needed, [ "raw", "image/png" ]) do
        with_stubbed_method(Net::HTTP, :start, ->(*_args, **_kwargs, &_block) { raise Net::OpenTimeout }) do
          assert_raises(Net::OpenTimeout) { service.send(:upload_blob, blob) }
        end
      end
    end
  end

  test "upload_blob returns nil on permanent errors" do
    service = BlueskyService.new
    blob = Struct.new(:download, :content_type).new("raw", "image/png")

    with_stubbed_method(service, :verify_tokens, nil) do
      with_stubbed_method(service, :resize_image_if_needed, [ "raw", "image/png" ]) do
        with_stubbed_method(Net::HTTP, :start, ->(*_args, **_kwargs, &_block) { raise "boom" }) do
          assert_nil service.send(:upload_blob, blob)
        end
      end
    end
  end

  test "upload_remote_image re-raises transient network errors for job retry" do
    service = BlueskyService.new

    with_stubbed_method(service, :verify_tokens, nil) do
      with_stubbed_method(service, :download_remote_image_with_redirect, [ "raw", "image/png" ]) do
        with_stubbed_method(service, :resize_image_if_needed, [ "raw", "image/png" ]) do
          with_stubbed_method(Net::HTTP, :start, ->(*_args, **_kwargs, &_block) { raise Net::OpenTimeout }) do
            assert_raises(Net::OpenTimeout) { service.send(:upload_remote_image, "https://example.com/image.png") }
          end
        end
      end
    end
  end

  test "upload_remote_image returns nil on permanent errors" do
    service = BlueskyService.new

    with_stubbed_method(service, :verify_tokens, nil) do
      with_stubbed_method(service, :download_remote_image_with_redirect, [ "raw", "image/png" ]) do
        with_stubbed_method(service, :resize_image_if_needed, [ "raw", "image/png" ]) do
          with_stubbed_method(Net::HTTP, :start, ->(*_args, **_kwargs, &_block) { raise "boom" }) do
            assert_nil service.send(:upload_remote_image, "https://example.com/image.png")
          end
        end
      end
    end
  end

  test "upload_images_embed re-raises transient errors from uploads" do
    service = BlueskyService.new

    remote_image = RemoteImageWrapper.new("http://example.com/remote.png")

    def service.upload_remote_image(_url); raise Net::OpenTimeout; end
    assert_raises(Net::OpenTimeout) { service.send(:upload_images_embed, [ remote_image ]) }
  end

  test "extract_post_uri_from_url returns nil on resolution error" do
    original_method = Net::HTTP.method(:get_response)
    Net::HTTP.define_singleton_method(:get_response) { |_uri| raise "boom" }

    service = BlueskyService.new
    assert_nil service.send(:extract_post_uri_from_url, "https://bsky.app/profile/test/post/abc")
  ensure
    Net::HTTP.define_singleton_method(:get_response, original_method) if original_method
  end

  test "fetch_with_redirect raises after too many redirects" do
    service = BlueskyService.new

    assert_raises RuntimeError do
      service.send(:fetch_with_redirect, URI("http://example.com/start"), 0)
    end
  end

  test "log_rate_limit_status notifies on info threshold" do
    service = BlueskyService.new

    result = service.send(:log_rate_limit_status, { remaining: 200, limit: 3000, reset_at: Time.current + 5.minutes })

    assert_nil result
  end

  test "token cache is scoped per account" do
    cache = ActiveSupport::Cache::MemoryStore.new

    Rails.stub(:cache, cache) do
      Crosspost.for("bluesky").update!(username: "alice")
      token_data = bluesky_token_data
      cache.write("bluesky_token_data:alice", token_data)

      service = BlueskyService.new
      assert_equal token_data["accessJwt"], service.instance_variable_get(:@token)

      Crosspost.for("bluesky").update!(username: "bob")
      other = BlueskyService.new
      assert_nil other.instance_variable_get(:@token)
    end
  end

  test "store_token_data writes per-account key with expiry" do
    cache = ActiveSupport::Cache::MemoryStore.new

    Rails.stub(:cache, cache) do
      Crosspost.for("bluesky").update!(username: "alice")
      service = BlueskyService.new

      service.send(:store_token_data, { "accessJwt" => "token" })

      assert_equal({ "accessJwt" => "token" }, cache.read("bluesky_token_data:alice"))
      assert_nil cache.read("bluesky_token_data")

      travel BlueskyService::TOKEN_CACHE_TTL + 1.minute do
        assert_nil cache.read("bluesky_token_data:alice")
      end
    end
  end

  test "verify_tokens falls back to generate_tokens when refresh fails" do
    service = BlueskyService.new
    service.instance_variable_set(:@token, "token")
    service.instance_variable_set(:@token_expires_at, 1.minute.ago)

    def service.perform_token_refresh; raise "refresh rejected"; end
    def service.generate_tokens; @regenerated = true; end
    def service.regenerated; @regenerated; end

    service.send(:verify_tokens)

    assert service.regenerated
  end

  test "verify_tokens raises when refresh and regeneration both fail" do
    service = BlueskyService.new
    service.instance_variable_set(:@token, "token")
    service.instance_variable_set(:@token_expires_at, 1.minute.ago)

    def service.perform_token_refresh; raise "refresh rejected"; end
    def service.generate_tokens; raise "login failed"; end

    error = assert_raises(RuntimeError) { service.send(:verify_tokens) }
    assert_equal "login failed", error.message
  end

  test "verify restores the previous cache entry for the verified account" do
    cache = ActiveSupport::Cache::MemoryStore.new

    Rails.stub(:cache, cache) do
      service = BlueskyService.new

      original = { "accessJwt" => "original-token" }
      cache.write("bluesky_token_data:new-account", original)

      # Simulates a successful login that caches fresh session data
      def service.verify_tokens
        store_token_data({ "accessJwt" => "verify-token" })
      end

      result = service.verify(username: "new-account", app_password: "pass", server_url: "https://bsky.social/xrpc")

      assert_equal true, result[:success]
      assert_equal original, cache.read("bluesky_token_data:new-account")
    end
  end

  test "verify leaves no cache entry behind when none existed before" do
    cache = ActiveSupport::Cache::MemoryStore.new

    Rails.stub(:cache, cache) do
      service = BlueskyService.new

      def service.verify_tokens
        store_token_data({ "accessJwt" => "verify-token" })
      end

      result = service.verify(username: "other-account", app_password: "pass", server_url: "https://bsky.social/xrpc")

      assert_equal true, result[:success]
      refute cache.exist?("bluesky_token_data:other-account")
    end
  end

  test "process_tokens decodes unpadded base64url jwt payload" do
    service = BlueskyService.new
    exp = 1893456000
    # This payload's base64url contains '_' and breaks Base64.decode64
    payload = Base64.urlsafe_encode64({ exp: exp, sub: "???" }.to_json, padding: false)
    jwt = "header.#{payload}.signature"

    service.send(:process_tokens, { "accessJwt" => jwt, "refreshJwt" => "renewal", "did" => "did:plc:x" })

    assert_equal Time.at(exp).utc, service.instance_variable_get(:@token_expires_at)
  end

  test "process_tokens restores stripped jwt padding" do
    service = BlueskyService.new
    exp = 1893456000
    payload = Base64.urlsafe_encode64({ exp: exp, sub: "a" }.to_json, padding: false)
    assert_equal 2, payload.length % 4
    jwt = "header.#{payload}.signature"

    service.send(:process_tokens, { "accessJwt" => jwt, "refreshJwt" => "renewal", "did" => "did:plc:x" })

    assert_equal Time.at(exp).utc, service.instance_variable_get(:@token_expires_at)
  end

  private

  def bluesky_token_data
    payload = Base64.urlsafe_encode64({ exp: 1.hour.from_now.to_i }.to_json, padding: false)
    {
      "accessJwt" => "header.#{payload}.signature",
      "refreshJwt" => "renewal-token",
      "did" => "did:plc:cached"
    }
  end
end
