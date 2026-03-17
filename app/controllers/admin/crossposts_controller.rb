class Admin::CrosspostsController < Admin::BaseController
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
        TwitterService.new.verify(crosspost)
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

  private

  def crosspost_params
    params.expect(crosspost: [
      :platform, :server_url, :enabled, :access_token, :access_token_secret, :client_key, :api_key, :api_key_secret, :app_password, :username, :auto_fetch_comments, :comment_fetch_schedule, :max_characters ]
    )
  end
end
