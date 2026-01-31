# frozen_string_literal: true

require "test_helper"

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

  private

  def with_event_notifier(notifier)
    original_event = Rails.event
    Rails.define_singleton_method(:event) { notifier }
    yield
  ensure
    Rails.define_singleton_method(:event) { original_event }
  end
end
