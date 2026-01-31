# frozen_string_literal: true

require "test_helper"

class Admin::SourcesControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    sign_in(@user)
  end

  test "fetch_twitter handles validation and response parsing" do
    post admin_sources_fetch_twitter_path, params: { url: "" }, as: :json
    assert_response :unprocessable_entity
    assert_equal "URL is required", JSON.parse(response.body)["error"]

    post admin_sources_fetch_twitter_path, params: { url: "https://example.com" }, as: :json
    assert_response :unprocessable_entity
    assert_equal "Not a valid Twitter/X URL", JSON.parse(response.body)["error"]

    success_body = {
      html: "<blockquote><p>Hello world</p></blockquote>",
      author_name: "Tester"
    }.to_json

    with_stubbed_net_http(response: FakeSuccess.new(success_body)) do
      post admin_sources_fetch_twitter_path, params: { url: "https://x.com/user/status/1" }, as: :json
      assert_response :success
      payload = JSON.parse(response.body)
      assert_equal true, payload["success"]
      assert_equal "Tester", payload["author"]
      assert_equal "Hello world", payload["content"]
    end

    with_stubbed_net_http(response: Net::HTTPBadRequest.new("1.1", "400", "Bad Request")) do
      post admin_sources_fetch_twitter_path, params: { url: "https://twitter.com/user/status/1" }, as: :json
      assert_response :service_unavailable
      assert_equal "Failed to fetch tweet content", JSON.parse(response.body)["error"]
    end
  end

  private

  FakeHttp = Struct.new(:response) do
    attr_accessor :use_ssl, :open_timeout, :read_timeout

    def request(_request)
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
    Net::HTTP.define_singleton_method(:new) { |_host, _port| FakeHttp.new(response) }
    yield
  ensure
    Net::HTTP.define_singleton_method(:new, original_new)
  end
end
