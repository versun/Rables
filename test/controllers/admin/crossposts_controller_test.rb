# frozen_string_literal: true

require "test_helper"

class Admin::CrosspostsControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    sign_in(@user)
  end

  test "index update and verify flows" do
    get admin_crossposts_path
    assert_response :success
    assert_select ".status-tab.active", text: "Mastodon"

    get admin_crossposts_path(platform: "twitter")
    assert_response :success
    assert_select ".status-tab.active", text: "X (Twitter)"

    patch admin_crosspost_path("mastodon"), params: {
      crosspost: {
        platform: "mastodon",
        enabled: "0",
        server_url: "https://mastodon.example"
      }
    }
    assert_redirected_to admin_crossposts_path(platform: "mastodon")

    patch admin_crosspost_path("mastodon"), params: {
      crosspost: {
        platform: "mastodon",
        enabled: "1",
        server_url: "https://mastodon.example"
      }
    }
    assert_redirected_to admin_crossposts_path(platform: "mastodon")

    with_stubbed_verify(MastodonService, { success: true }) do
      post verify_admin_crosspost_path("mastodon"), params: {
        crosspost: { platform: "mastodon" }
      }, as: :json
      assert_response :success
      assert_equal "success", JSON.parse(response.body)["status"]
    end

    post verify_admin_crosspost_path("mastodon"), params: {
      crosspost: { platform: "twitter" }
    }, as: :json
    assert_response :success
    assert_equal "error", JSON.parse(response.body)["status"]

    with_stubbed_verify(TwitterService, { success: false, error: "bad" }) do
      post verify_admin_crosspost_path("twitter"), params: {
        crosspost: { platform: "twitter" }
      }, as: :json
      assert_response :success
      assert_equal "error", JSON.parse(response.body)["status"]
    end

    with_stubbed_verify(BlueskyService, { success: true }) do
      post verify_admin_crosspost_path("bluesky"), params: {
        crosspost: { platform: "bluesky" }
      }, as: :json
      assert_response :success
      assert_equal "success", JSON.parse(response.body)["status"]
    end

    post verify_admin_crosspost_path("unknown"), params: {
      crosspost: { platform: "unknown" }
    }, as: :json
    assert_response :success
    assert_equal "error", JSON.parse(response.body)["status"]
  end

  test "twitter tab shows oauth1 credential fields without authorize flow" do
    get admin_crossposts_path(platform: "twitter")

    assert_response :success
    assert_select "input[name='crosspost[access_token]']"
    assert_select "input[name='crosspost[access_token_secret]']"
    assert_select "input[name='crosspost[api_key]']"
    assert_select "input[name='crosspost[api_key_secret]']"
    assert_select "a", text: "Authorize with X", count: 0
    assert_select ".authorization-status", count: 0
  end

  test "twitter verify uses submitted oauth1 params" do
    captured_params = nil

    with_stubbed_verify(TwitterService, lambda { |params|
      captured_params = params
      { success: true }
    }) do
      post verify_admin_crosspost_path("twitter"), params: {
        crosspost: {
          platform: "twitter",
          access_token: "submitted-access-token",
          access_token_secret: "submitted-access-token-secret",
          api_key: "submitted-api-key",
          api_key_secret: "submitted-api-key-secret"
        }
      }, as: :json
    end

    assert_response :success
    assert_equal "success", JSON.parse(response.body)["status"]
    assert_equal "submitted-access-token", captured_params[:access_token]
    assert_equal "submitted-access-token-secret", captured_params[:access_token_secret]
    assert_equal "submitted-api-key", captured_params[:api_key]
    assert_equal "submitted-api-key-secret", captured_params[:api_key_secret]
  end

  private

  def with_stubbed_verify(service_class, result)
    original = service_class.instance_method(:verify)
    service_class.define_method(:verify) do |params|
      result.respond_to?(:call) ? result.call(params) : result
    end
    yield
  ensure
    service_class.define_method(:verify, original)
  end
end
