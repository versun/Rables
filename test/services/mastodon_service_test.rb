# frozen_string_literal: true

require "test_helper"
require "stringio"
require "uri"

class MastodonServiceTest < ActiveSupport::TestCase
  class RecordingNotifier
    attr_reader :events

    def initialize
      @events = []
    end

    def notify(name, **payload)
      @events << [ name, payload ]
    end
  end

  test "verify fails fast when access token is blank" do
    service = MastodonService.new
    result = service.verify({})

    assert_equal false, result[:success]
    assert_match "Access token", result[:error]
  end

  test "post returns nil when crosspost is disabled" do
    Crosspost.mastodon.update!(enabled: false)
    service = MastodonService.new

    assert_nil service.post(create_published_article)
  end

  test "mastodon api uri preserves server subpaths" do
    service = MastodonService.new

    uri = service.send(:mastodon_api_uri, "/api/v1/statuses", "https://example.com/masto")

    assert_equal "https://example.com/masto/api/v1/statuses", uri.to_s
  end

  test "mastodon api uri logs invalid server url" do
    notifier = RecordingNotifier.new

    with_event_notifier(notifier) do
      service = MastodonService.new
      uri = service.send(:mastodon_api_uri, "/api/v1/statuses", "file:///etc/passwd")

      assert_nil uri
    end

    assert notifier.events.any? { |name, _| name == "mastodon_service.invalid_server_url" }
  end

  test "post includes all mastodon media ids" do
    Crosspost.mastodon.update!(enabled: true, access_token: "token", server_url: "https://mastodon.social")
    article = create_published_article
    service = MastodonService.new

    images = [ Object.new, Object.new ]
    article.define_singleton_method(:all_image_attachments) { |_limit| images }

    sequence = 0
    service.define_singleton_method(:upload_image) do |_attachable|
      sequence += 1
      "media-#{sequence}"
    end

    response_body = { url: "https://mastodon.social/@user/123" }.to_json
    with_stubbed_net_http(response: FakeSuccess.new(response_body)) do |http|
      service.post(article)

      request = http.requests.last
      pairs = URI.decode_www_form(request.body)
      media_ids = pairs.select { |key, _| key == "media_ids[]" }.map(&:last)
      assert_equal %w[media-1 media-2], media_ids
    end
  end

  test "extracts status id from supported urls" do
    service = MastodonService.new

    assert_equal "123", service.send(:extract_status_id_from_url, "https://mastodon.social/@user/123")
    assert_equal "456", service.send(:extract_status_id_from_url, "https://mastodon.social/users/user/statuses/456")
    assert_nil service.send(:extract_status_id_from_url, "https://example.com/other")
  end

  test "normalized_server_uri preserves subpaths" do
    service = MastodonService.new

    uri = service.send(:normalized_server_uri, "https://example.com/masto")

    assert_equal "https://example.com/masto/", uri.to_s
  end

  test "verify succeeds with valid credentials" do
    response = Net::HTTPSuccess.new("1.1", "200", "OK")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "{}")

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| response }

    original_new = Net::HTTP.method(:new)
    Net::HTTP.define_singleton_method(:new) { |_host, *_args| fake_http }

    service = MastodonService.new
    result = service.verify(access_token: "token", server_url: "https://mastodon.social")

    assert_equal true, result[:success]
  ensure
    Net::HTTP.define_singleton_method(:new, original_new) if original_new
  end

  test "verify returns error when server url is invalid" do
    service = MastodonService.new

    result = service.verify(access_token: "token", server_url: "ftp://example.com")

    assert_equal false, result[:success]
    assert_match "Server URL must be a valid http(s) URL", result[:error]
  end

  test "fetch_comments returns parsed replies" do
    Crosspost.mastodon.update!(enabled: true)
    service = MastodonService.new

    context_data = {
      "descendants" => [
        {
          "id" => "c1",
          "account" => { "display_name" => "Alice", "username" => "alice", "acct" => "alice", "avatar" => "http://example.com/a.png" },
          "content" => "<p>Hi</p>",
          "created_at" => Time.current.iso8601,
          "url" => "https://mastodon.social/@alice/1",
          "in_reply_to_id" => "123"
        }
      ]
    }

    response = Net::HTTPSuccess.new("1.1", "200", "OK")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, context_data.to_json)
    response["X-RateLimit-Limit"] = "300"
    response["X-RateLimit-Remaining"] = "5"
    response["X-RateLimit-Reset"] = (Time.current + 60).to_i.to_s

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| response }

    original_new = Net::HTTP.method(:new)
    Net::HTTP.define_singleton_method(:new) { |_host, *_args| fake_http }

    result = service.fetch_comments("https://mastodon.social/@user/123")

    assert_equal 1, result[:comments].length
    assert_equal "c1", result[:comments].first[:external_id]
    assert_nil result[:comments].first[:parent_external_id]
  ensure
    Net::HTTP.define_singleton_method(:new, original_new) if original_new
  end

  test "fetch_comments returns rate limit info on 429" do
    Crosspost.mastodon.update!(enabled: true)
    service = MastodonService.new

    response = Net::HTTPTooManyRequests.new("1.1", "429", "Too Many Requests")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "{}")
    response["X-RateLimit-Limit"] = "300"
    response["X-RateLimit-Remaining"] = "0"
    response["X-RateLimit-Reset"] = (Time.current + 60).to_i.to_s

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| response }

    original_new = Net::HTTP.method(:new)
    Net::HTTP.define_singleton_method(:new) { |_host, *_args| fake_http }

    result = service.fetch_comments("https://mastodon.social/@user/123")

    assert_equal 0, result[:comments].length
    assert_equal 0, result[:rate_limit][:remaining]
  ensure
    Net::HTTP.define_singleton_method(:new, original_new) if original_new
  end

  test "post returns url on success" do
    Crosspost.mastodon.update!(enabled: true)
    service = MastodonService.new
    article = create_published_article
    article.define_singleton_method(:first_image_attachment) { nil }

    response = Net::HTTPSuccess.new("1.1", "200", "OK")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, { url: "https://mastodon.social/@user/1" }.to_json)

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| response }

    original_new = Net::HTTP.method(:new)
    Net::HTTP.define_singleton_method(:new) { |_host, *_args| fake_http }

    result = service.post(article)

    assert_equal "https://mastodon.social/@user/1", result
  ensure
    Net::HTTP.define_singleton_method(:new, original_new) if original_new
  end

  test "post logs failure on error response" do
    Crosspost.mastodon.update!(enabled: true)
    service = MastodonService.new
    article = create_published_article
    article.define_singleton_method(:first_image_attachment) { nil }

    response = Net::HTTPInternalServerError.new("1.1", "500", "Error")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "boom")

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| response }

    original_new = Net::HTTP.method(:new)
    Net::HTTP.define_singleton_method(:new) { |_host, *_args| fake_http }

    assert_difference "ActivityLog.count", 1 do
      assert_nil service.post(article)
    end
  ensure
    Net::HTTP.define_singleton_method(:new, original_new) if original_new
  end

  test "upload_image uploads blob and returns media id" do
    Crosspost.mastodon.update!(enabled: true)
    service = MastodonService.new

    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new("data"),
      filename: "test.jpg",
      content_type: "image/jpeg"
    )

    response = Net::HTTPSuccess.new("1.1", "200", "OK")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, { id: "media123" }.to_json)

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| response }

    original_new = Net::HTTP.method(:new)
    Net::HTTP.define_singleton_method(:new) { |_host, *_args| fake_http }

    media_id = service.send(:upload_image, blob)

    assert_equal "media123", media_id
  ensure
    Net::HTTP.define_singleton_method(:new, original_new) if original_new
  end

  test "download_remote_image returns data for relative urls" do
    service = MastodonService.new

    response = Net::HTTPSuccess.new("1.1", "200", "OK")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "image-data")
    response["content-type"] = "image/png"

    def service.fetch_with_redirect(_uri); @response; end
    service.instance_variable_set(:@response, response)
    data, content_type = service.send(:download_remote_image, "/image.png")

    assert_equal "image-data", data
    assert_equal "image/png", content_type
  end

  test "download_remote_image returns nil on failed response" do
    service = MastodonService.new

    response = Net::HTTPNotFound.new("1.1", "404", "Not Found")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "missing")

    def service.fetch_with_redirect(_uri); @response; end
    service.instance_variable_set(:@response, response)
    assert_nil service.send(:download_remote_image, "http://example.com/missing.png")
  end

  test "upload_image returns nil for unknown attachable type" do
    Crosspost.mastodon.update!(enabled: true)
    service = MastodonService.new

    assert_nil service.send(:upload_image, Object.new)
  end

  test "upload_image handles remote images" do
    Crosspost.mastodon.update!(enabled: true)
    service = MastodonService.new

    remote_image = Object.new
    remote_image.define_singleton_method(:url) { "http://example.com/remote.png" }
    remote_image.define_singleton_method(:class) { Struct.new(:name).new("ActionText::Attachables::RemoteImage") }

    response = Net::HTTPSuccess.new("1.1", "200", "OK")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, { id: "remote123" }.to_json)

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| response }

    called = false
    def service.download_remote_image(_url)
      @called = true
      [ "data", "image/png" ]
    end
    def service.called; @called; end

    original_new = Net::HTTP.method(:new)
    Net::HTTP.define_singleton_method(:new) { |_host, *_args| fake_http }

    media_id = service.send(:upload_image, remote_image)

    assert service.called
    assert_nil media_id
  ensure
    Net::HTTP.define_singleton_method(:new, original_new) if original_new
  end

  test "fetch_comments returns empty on error response" do
    Crosspost.mastodon.update!(enabled: true, server_url: "https://mastodon.social", access_token: "token")
    service = MastodonService.new

    response = Net::HTTPInternalServerError.new("1.1", "500", "Error")
    response.instance_variable_set(:@read, true)
    response.instance_variable_set(:@body, "boom")
    response["X-RateLimit-Limit"] = "300"
    response["X-RateLimit-Remaining"] = "20"
    response["X-RateLimit-Reset"] = (Time.current + 60).to_i.to_s

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| response }

    original_new = Net::HTTP.method(:new)
    Net::HTTP.define_singleton_method(:new) { |_host, *_args| fake_http }

    result = service.fetch_comments("https://mastodon.social/@user/123")

    assert_equal 0, result[:comments].length
    assert_equal 20, result[:rate_limit][:remaining]
  ensure
    Net::HTTP.define_singleton_method(:new, original_new) if original_new
  end

  test "log_rate_limit_status notifies on info threshold" do
    service = MastodonService.new

    result = service.send(:log_rate_limit_status, { remaining: 20, limit: 300, reset_at: Time.current + 5.minutes })

    assert_nil result
  end

  private

  FakeHttp = Struct.new(:response) do
    attr_accessor :use_ssl, :open_timeout, :read_timeout
    attr_reader :requests

    def initialize(response)
      super(response)
      @requests = []
    end

    def request(request)
      @requests << request
      response
    end
  end

  class FakeSuccess < Net::HTTPSuccess
    def initialize(body)
      super("1.1", "200", "OK")
      @read = true
      @body = body
    end
  end

  def with_stubbed_net_http(response:)
    original_new = Net::HTTP.method(:new)
    http = FakeHttp.new(response)
    Net::HTTP.define_singleton_method(:new) { |_host, _port| http }
    yield http
  ensure
    Net::HTTP.define_singleton_method(:new, original_new)
  end

  def with_event_notifier(notifier)
    original_event = Rails.event
    Rails.define_singleton_method(:event) { notifier }
    yield
  ensure
    Rails.define_singleton_method(:event) { original_event }
  end
end
