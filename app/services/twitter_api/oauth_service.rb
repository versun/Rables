require "net/http"
require "uri"
require "json"

module TwitterApi
  # Handles OAuth 2.0 token management for Twitter/X API
  class OauthService
    # Buffer time in seconds before token expiration to trigger refresh
    TOKEN_REFRESH_BUFFER = 300 # 5 minutes

    def initialize(settings)
      @settings = settings
    end

    # Check if OAuth 2.0 credentials are complete
    def credentials_complete?(settings = @settings)
      %i[client_id client_secret access_token refresh_token].all? do |field|
        setting_value(settings, field).present?
      end
    end

    # Check if the OAuth 2.0 token needs to be refreshed
    def token_needs_refresh?
      return false unless credentials_complete?

      expires_at = @settings.token_expires_at
      return true if expires_at.nil? # Unknown expiration, try to refresh

      Time.current >= expires_at - TOKEN_REFRESH_BUFFER
    end

    # Refresh the OAuth 2.0 access token using the refresh token
    def refresh_token!
      Rails.event.notify "twitter_service.refreshing_token",
        level: "info",
        component: "Twitter::OAuthService"

      uri = URI("https://api.x.com/2/oauth2/token")
      http = Net::HTTP.new(uri.host, uri.port)
      http.use_ssl = true

      request = Net::HTTP::Post.new(uri)
      request["Content-Type"] = "application/x-www-form-urlencoded"
      request["Authorization"] = "Basic #{Base64.strict_encode64("#{@settings.client_id}:#{@settings.client_secret}")}"
      request.body = URI.encode_www_form(
        grant_type: "refresh_token",
        refresh_token: @settings.refresh_token
      )

      response = http.request(request)
      body = JSON.parse(response.body)

      unless response.is_a?(Net::HTTPSuccess)
        error_msg = body["error_description"] || body["error"] || "Token refresh failed"
        Rails.event.notify "twitter_service.token_refresh_failed",
          level: "error",
          component: "Twitter::OAuthService",
          error: error_msg
        raise "OAuth 2.0 token refresh failed: #{error_msg}"
      end

      # Update tokens in database
      update_attrs = { access_token: body["access_token"] }
      update_attrs[:refresh_token] = body["refresh_token"] if body["refresh_token"]
      update_attrs[:token_expires_at] = Time.current + body["expires_in"].to_i if body["expires_in"]

      @settings.update!(update_attrs)

      Rails.event.notify "twitter_service.token_refreshed",
        level: "info",
        component: "Twitter::OAuthService",
        expires_in: body["expires_in"]
    rescue JSON::ParserError => e
      Rails.event.notify "twitter_service.token_refresh_parse_error",
        level: "error",
        component: "Twitter::OAuthService",
        error: e.message
      raise "OAuth 2.0 token refresh failed: Invalid response"
    end

    # Refresh token if needed
    def refresh_if_needed!
      return unless @settings
      return unless token_needs_refresh?

      refresh_token!
    end

    # Build X::Client with current credentials
    def build_client(settings = @settings)
      unless credentials_complete?(settings)
        raise "Twitter OAuth 2.0 credentials are incomplete"
      end

      X::Client.new(
        client_id: setting_value(settings, :client_id),
        client_secret: setting_value(settings, :client_secret),
        access_token: setting_value(settings, :access_token),
        refresh_token: setting_value(settings, :refresh_token)
      )
    end

    private

    def setting_value(settings, field)
      value = nil
      if settings.respond_to?(:[])
        value = settings[field]
        value = settings[field.to_s] if value.nil?
      end
      value = settings.public_send(field) if value.nil? && settings.respond_to?(field)
      value
    end
  end
end
