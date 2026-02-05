# frozen_string_literal: true

require "test_helper"
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
