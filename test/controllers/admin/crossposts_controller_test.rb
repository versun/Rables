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

  test "twitter_authorize redirects when client_id is missing" do
    Crosspost.twitter.update!(client_id: nil)

    get twitter_authorize_admin_crossposts_path
    assert_redirected_to admin_crossposts_path(platform: "twitter")
    assert_match "Please enter Client ID", flash[:alert]
  end

  test "twitter_authorize redirects when client_secret is missing" do
    Crosspost.twitter.update!(client_id: "test_client_id", client_secret: nil)

    get twitter_authorize_admin_crossposts_path
    assert_redirected_to admin_crossposts_path(platform: "twitter")
    assert_match "Please enter Client ID and Client Secret", flash[:alert]
  end

  test "twitter_authorize redirects to twitter with PKCE params" do
    Crosspost.twitter.update!(client_id: "test_client_id", client_secret: "test_client_secret")

    get twitter_authorize_admin_crossposts_path

    assert_response :redirect
    redirect_url = response.location

    assert redirect_url.start_with?("https://twitter.com/i/oauth2/authorize")
    assert_includes redirect_url, "client_id=test_client_id"
    assert_includes redirect_url, "response_type=code"
    assert_includes redirect_url, "code_challenge_method=S256"
    assert_includes redirect_url, "scope="

    # Verify session contains PKCE data
    assert session[:twitter_oauth_state].present?
    assert session[:twitter_oauth_code_verifier].present?
  end

  test "twitter_callback fails with invalid state" do
    prepare_twitter_oauth_session

    get twitter_callback_admin_crossposts_path, params: {
      state: "wrong_state",
      code: "auth_code"
    }

    assert_redirected_to admin_crossposts_path(platform: "twitter")
    assert_match "Invalid state", flash[:alert]
  end

  test "twitter_callback fails when state is missing" do
    get twitter_callback_admin_crossposts_path, params: {
      code: "auth_code"
    }

    assert_redirected_to admin_crossposts_path(platform: "twitter")
    assert_match "Invalid state", flash[:alert]
  end

  test "twitter_callback handles error response from twitter" do
    stored_state = prepare_twitter_oauth_session

    get twitter_callback_admin_crossposts_path, params: {
      state: stored_state,
      error: "access_denied",
      error_description: "User denied access"
    }

    assert_redirected_to admin_crossposts_path(platform: "twitter")
    assert_match "User denied access", flash[:alert]
  end

  test "twitter_callback fails when callback code is missing" do
    stored_state = prepare_twitter_oauth_session

    get twitter_callback_admin_crossposts_path, params: {
      state: stored_state
    }

    assert_redirected_to admin_crossposts_path(platform: "twitter")
    assert_match "Missing OAuth session data", flash[:alert]
  end

  test "twitter_callback exchanges code for tokens successfully" do
    Crosspost.twitter.update!(
      client_id: "test_client_id",
      client_secret: "test_client_secret"
    )

    success_response = Net::HTTPSuccess.new("1.1", "200", "OK")
    success_response.instance_variable_set(:@read, true)
    success_response.instance_variable_set(:@body, {
      access_token: "new_access_token",
      refresh_token: "new_refresh_token",
      expires_in: 7200
    }.to_json)

    captured_request_body = nil
    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:request) do |req|
      captured_request_body = req.body
      success_response
    end

    # We need to set session before the request, using a controller test approach
    original_new = Net::HTTP.method(:new)
    Net::HTTP.define_singleton_method(:new) { |*args| fake_http }

    begin
      # First set up the session via authorize
      Crosspost.twitter.update!(client_id: "test_client_id")
      get twitter_authorize_admin_crossposts_path

      # Get the state from session and make callback request
      stored_state = session[:twitter_oauth_state]

      get twitter_callback_admin_crossposts_path, params: {
        state: stored_state,
        code: "authorization_code"
      }

      assert_redirected_to admin_crossposts_path(platform: "twitter")
      assert_match "authorization successful", flash[:notice]

      twitter = Crosspost.twitter.reload
      assert_equal "new_access_token", twitter.access_token
      assert_equal "new_refresh_token", twitter.refresh_token
      assert twitter.token_expires_at.present?
      assert_includes captured_request_body, "client_id=test_client_id"
    ensure
      Net::HTTP.define_singleton_method(:new, original_new)
    end
  end

  test "twitter_callback handles token exchange failure" do
    Crosspost.twitter.update!(
      client_id: "test_client_id",
      client_secret: "test_client_secret"
    )

    error_response = Net::HTTPBadRequest.new("1.1", "400", "Bad Request")
    error_response.instance_variable_set(:@read, true)
    error_response.instance_variable_set(:@body, {
      error: "invalid_grant",
      error_description: "Authorization code expired"
    }.to_json)

    fake_http = Object.new
    fake_http.define_singleton_method(:use_ssl=) { |_val| }
    fake_http.define_singleton_method(:request) { |_req| error_response }

    original_new = Net::HTTP.method(:new)
    Net::HTTP.define_singleton_method(:new) { |*args| fake_http }

    begin
      # First set up the session via authorize
      get twitter_authorize_admin_crossposts_path
      stored_state = session[:twitter_oauth_state]

      get twitter_callback_admin_crossposts_path, params: {
        state: stored_state,
        code: "expired_code"
      }

      assert_redirected_to admin_crossposts_path(platform: "twitter")
      assert_match "Authorization code expired", flash[:alert]
    ensure
      Net::HTTP.define_singleton_method(:new, original_new)
    end
  end

  test "twitter tab shows authorized only with complete oauth2 credentials" do
    Crosspost.twitter.update!(
      client_id: "client",
      client_secret: "secret",
      access_token: "access",
      refresh_token: nil
    )

    get admin_crossposts_path(platform: "twitter")
    assert_response :success
    assert_select ".authorization-status.authorized", count: 0

    Crosspost.twitter.update!(refresh_token: "refresh")

    get admin_crossposts_path(platform: "twitter")
    assert_response :success
    assert_select ".authorization-status.authorized", count: 1
  end

  private

  def with_stubbed_verify(service_class, result)
    original = service_class.instance_method(:verify)
    service_class.define_method(:verify) { |_params| result }
    yield
  ensure
    service_class.define_method(:verify, original)
  end

  def prepare_twitter_oauth_session
    Crosspost.twitter.update!(client_id: "test_client_id", client_secret: "test_client_secret")
    get twitter_authorize_admin_crossposts_path
    session[:twitter_oauth_state]
  end
end
