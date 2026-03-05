class Admin::CrosspostsController < Admin::BaseController
  # Required OAuth 2.0 scopes for Twitter/X API
  TWITTER_SCOPES = %w[
    tweet.read
    tweet.write
    users.read
    offline.access
  ].freeze

  def index
    @mastodon = Crosspost.mastodon
    @twitter = Crosspost.twitter
    @bluesky = Crosspost.bluesky
    @xiaohongshu = Crosspost.xiaohongshu
    @active_platform = Crosspost::PLATFORMS.include?(params[:platform]) ? params[:platform] : "mastodon"
  end

  def update
    @settings = Crosspost.find_or_create_by(platform: params[:id])
    # Rails.logger.info "Updating Crosspost: #{params[:id]}"
    # Rails.logger.info "Params: #{params.inspect}"

    if @settings.update(crosspost_params)
      ActivityLog.log!(
        action: :updated,
        target: :crosspost,
        level: :info,
        platform: params[:id]
      )
      # Rails.logger.info "Successfully updated Crosspost"
      redirect_to admin_crossposts_path(platform: params[:id]), notice: "CrossPost settings updated successfully."
    else
      ActivityLog.log!(
        action: :failed,
        target: :crosspost,
        level: :error,
        platform: params[:id],
        errors: @settings.errors.full_messages.join(", ")
      )
      # Rails.logger.error "Failed to update Crosspost: #{@settings.errors.full_messages}"
      redirect_to admin_crossposts_path(platform: params[:id]), alert: @settings.errors.full_messages.join(", ")
    end
  end

  def verify
    # Rails.logger.info "Verifying #{params[:id]} platform"
    # Rails.logger.info "Params: #{params.inspect}"

    @platform = params[:id]
    @message = ""
    @status = ""

    begin
      crosspost = params[:crosspost] || {}
      crosspost = crosspost.to_unsafe_h if crosspost.respond_to?(:to_unsafe_h)
      crosspost = crosspost.with_indifferent_access if crosspost.respond_to?(:with_indifferent_access)

      # 如果 crosspost[:platform] 为空，尝试从 params[:id] 获取
      if crosspost[:platform].blank?
        crosspost[:platform] = params[:id]
      end

      unless crosspost[:platform] == params[:id]
        raise "Platform mismatch: #{crosspost[:platform].inspect} != #{params[:id].inspect}"
      end

      results = case crosspost[:platform]
      when "mastodon"
        crosspost[:server_url] = "https://mastodon.social" if crosspost[:server_url].blank?
        MastodonService.new.verify(crosspost)
      when "twitter"
        TwitterService.new.verify(Crosspost.twitter.attributes.symbolize_keys)
      when "bluesky"
        # Set default server_url if not provided
        crosspost[:server_url] = "https://bsky.social/xrpc" if crosspost[:server_url].blank?
        BlueskyService.new.verify(crosspost)
      else
        raise "Unknown platform: #{crosspost[:platform]}"
      end

      if results[:success]
        @status = "success"
        @message = "Verified Successfully!"
      else
        @status = "error"
        @message = results[:error]
      end
    rescue => e
      # Rails.logger.error "Verification error for #{params[:id]}: #{e.message}"
      # Rails.logger.error e.backtrace.join("\n")
      @status = "error"
      @message = "Error: #{e.message}"
    end

    respond_to do |format|
      format.turbo_stream
      format.json { render json: { status: @status, message: @message } }
    end
  end

  # Initiate OAuth 2.0 authorization flow for Twitter/X
  def twitter_authorize
    @twitter = Crosspost.twitter

    if @twitter.client_id.blank? || @twitter.client_secret.blank?
      redirect_to admin_crossposts_path(platform: "twitter"),
        alert: "Please enter Client ID and Client Secret first before authorizing."
      return
    end

    # Generate PKCE code verifier and challenge
    code_verifier = SecureRandom.urlsafe_base64(32)
    code_challenge = Base64.urlsafe_encode64(
      Digest::SHA256.digest(code_verifier),
      padding: false
    )

    # Generate state for CSRF protection
    state = SecureRandom.urlsafe_base64(16)

    # Store in session for callback verification
    session[:twitter_oauth_state] = state
    session[:twitter_oauth_code_verifier] = code_verifier

    # Build authorization URL
    auth_params = {
      response_type: "code",
      client_id: @twitter.client_id,
      redirect_uri: twitter_callback_admin_crossposts_url,
      scope: TWITTER_SCOPES.join(" "),
      state: state,
      code_challenge: code_challenge,
      code_challenge_method: "S256"
    }

    auth_url = "https://x.com/i/oauth2/authorize?#{auth_params.to_query}"

    redirect_to auth_url, allow_other_host: true
  end

  # Handle OAuth 2.0 callback from Twitter/X
  def twitter_callback
    @twitter = Crosspost.twitter

    callback_state = params[:state]
    session_state = session[:twitter_oauth_state]

    # Verify state to prevent CSRF
    unless callback_state.present? && session_state.present? &&
           ActiveSupport::SecurityUtils.secure_compare(callback_state, session_state)
      clear_twitter_oauth_session!
      redirect_to admin_crossposts_path(platform: "twitter"),
        alert: "Authorization failed: Invalid state parameter (possible CSRF attack)."
      return
    end

    # Check for error response
    if params[:error].present?
      clear_twitter_oauth_session!
      error_desc = params[:error_description] || params[:error]
      redirect_to admin_crossposts_path(platform: "twitter"),
        alert: "Authorization denied: #{error_desc}"
      return
    end

    # Exchange authorization code for tokens
    code_verifier = session[:twitter_oauth_code_verifier]
    authorization_code = params[:code]

    if code_verifier.blank? || authorization_code.blank?
      clear_twitter_oauth_session!
      redirect_to admin_crossposts_path(platform: "twitter"),
        alert: "Authorization failed: Missing OAuth session data. Please try authorizing again."
      return
    end

    if @twitter.client_id.blank? || @twitter.client_secret.blank?
      clear_twitter_oauth_session!
      redirect_to admin_crossposts_path(platform: "twitter"),
        alert: "Authorization failed: Missing OAuth client credentials. Please save Client ID and Client Secret first."
      return
    end

    begin
      tokens = exchange_twitter_code_for_tokens(authorization_code, code_verifier)

      # Update crosspost settings with new tokens
      @twitter.update!(
        access_token: tokens[:access_token],
        refresh_token: tokens[:refresh_token],
        token_expires_at: tokens[:expires_at]
      )

      ActivityLog.log!(
        action: :authorized,
        target: :crosspost,
        level: :info,
        platform: "twitter"
      )

      redirect_to admin_crossposts_path(platform: "twitter"),
        notice: "Twitter/X authorization successful! You can now enable crossposting."
    rescue => e
      Rails.logger.error "Twitter OAuth callback error: #{e.message}"
      redirect_to admin_crossposts_path(platform: "twitter"),
        alert: "Authorization failed: #{e.message}"
    ensure
      clear_twitter_oauth_session!
    end
  end

  private

  def clear_twitter_oauth_session!
    session.delete(:twitter_oauth_state)
    session.delete(:twitter_oauth_code_verifier)
  end

  def crosspost_params
    params.expect(crosspost: [
      :platform, :server_url, :enabled, :access_token, :refresh_token, :client_id, :client_secret, :client_key, :app_password, :username, :auto_fetch_comments, :comment_fetch_schedule, :max_characters ]
    )
  end

  # Exchange authorization code for access and refresh tokens
  def exchange_twitter_code_for_tokens(code, code_verifier)
    @twitter = Crosspost.twitter

    uri = URI("https://api.x.com/2/oauth2/token")
    http = Net::HTTP.new(uri.host, uri.port)
    http.use_ssl = true
    http.open_timeout = 10
    http.read_timeout = 15

    request = Net::HTTP::Post.new(uri)
    request["Content-Type"] = "application/x-www-form-urlencoded"

    # Use Basic Auth with client credentials
    if @twitter.client_secret.present?
      request["Authorization"] = "Basic #{Base64.strict_encode64("#{@twitter.client_id}:#{@twitter.client_secret}")}"
    end

    request.body = URI.encode_www_form(
      grant_type: "authorization_code",
      code: code,
      client_id: @twitter.client_id,
      redirect_uri: twitter_callback_admin_crossposts_url,
      code_verifier: code_verifier
    )

    response = http.request(request)
    body = JSON.parse(response.body)

    unless response.is_a?(Net::HTTPSuccess)
      error_msg = body["error_description"] || body["error"] || "Token exchange failed"
      raise "Token exchange failed: #{error_msg}"
    end

    {
      access_token: body["access_token"],
      refresh_token: body["refresh_token"],
      expires_at: body["expires_in"] ? Time.current + body["expires_in"].to_i : nil
    }
  end
end
