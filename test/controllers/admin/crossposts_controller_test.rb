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

  private

  def with_stubbed_verify(service_class, result)
    original = service_class.instance_method(:verify)
    service_class.define_method(:verify) { |_params| result }
    yield
  ensure
    service_class.define_method(:verify, original)
  end
end
