require "x"

module TwitterApi
  # Handles OAuth 2.0 token management for Twitter/X API.
  # Delegates token refresh to the x gem's built-in OAuth2Authenticator
  # and persists updated tokens back to the database.
  class OauthService
    TOKEN_REFRESH_BUFFER = 300 # 5 minutes

    def initialize(settings)
      @settings = settings
    end

    def credentials_complete?(settings = @settings)
      %i[client_id client_secret access_token refresh_token].all? do |field|
        setting_value(settings, field).present?
      end
    end

    def token_needs_refresh?
      return false unless credentials_complete?

      expires_at = @settings.token_expires_at
      return true if expires_at.nil?

      Time.current >= expires_at - TOKEN_REFRESH_BUFFER
    end

    def refresh_token!
      Rails.event.notify "twitter_service.refreshing_token",
        level: "info",
        component: "Twitter::OAuthService"

      client = build_client
      authenticator = client.authenticator
      authenticator.refresh_token!

      update_attrs = { access_token: authenticator.access_token }
      update_attrs[:refresh_token] = authenticator.refresh_token if authenticator.refresh_token
      update_attrs[:token_expires_at] = authenticator.expires_at if authenticator.expires_at

      @settings.update!(update_attrs)

      Rails.event.notify "twitter_service.token_refreshed",
        level: "info",
        component: "Twitter::OAuthService"
    rescue X::Error => e
      Rails.event.notify "twitter_service.token_refresh_failed",
        level: "error",
        component: "Twitter::OAuthService",
        error: e.message
      raise "OAuth 2.0 token refresh failed: #{e.message}"
    end

    def refresh_if_needed!
      return unless @settings
      return unless token_needs_refresh?

      refresh_token!
    end

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
