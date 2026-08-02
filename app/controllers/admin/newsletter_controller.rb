class Admin::NewsletterController < Admin::BaseController
  def show
    @newsletter_setting = NewsletterSetting.instance
    @listmonk = Listmonk.first_or_initialize
    @active_tab = newsletter_tab(params[:tab])

    load_listmonk_options
  end

  def verify
    # Check if this is a SMTP verification or Listmonk verification
    if params[:smtp_address].present? || params[:smtp_user_name].present?
      verify_smtp
    else
      verify_listmonk
    end
  end

  def verify_listmonk
    # Create a temporary Listmonk instance with form data (not persisted)
    @listmonk = Listmonk.new(verify_params)

    unless @listmonk.configured?
      render json: {
        success: false,
        error: "Please configure all required fields first"
      }, status: :unprocessable_entity
      return
    end

    begin
      lists = @listmonk.fetch_lists
      templates = @listmonk.fetch_templates
    rescue => e
      # Surface the real error from this verification attempt
      Rails.logger.error "Listmonk verification failed: #{e.message}"
      render json: {
        success: false,
        error: e.message
      }, status: :unprocessable_entity
      return
    end

    # Check if fetch was successful by verifying we got data back
    if lists.present? && templates.present?
      render json: {
        success: true,
        lists: lists,
        templates: templates,
        current_list_id: @listmonk.list_id,
        current_template_id: @listmonk.template_id
      }
    else
      render json: {
        success: false,
        error: @listmonk.last_error.presence || "Failed to fetch lists or templates. Please check your configuration."
      }, status: :unprocessable_entity
    end
  end

  def verify_smtp
    smtp_params = verify_smtp_params
    newsletter_setting = NewsletterSetting.instance

    # 如果密码是占位符（••••••••）或为空，且数据库中有保存的密码，则使用数据库中的密码
    if (smtp_params[:smtp_password] == "••••••••" || smtp_params[:smtp_password].blank?) && newsletter_setting.smtp_password.present?
      smtp_params[:smtp_password] = newsletter_setting.smtp_password
    end

    # Validate required fields
    if smtp_params[:smtp_address].blank? || smtp_params[:smtp_port].blank? ||
       smtp_params[:smtp_user_name].blank? || smtp_params[:smtp_password].blank? ||
       smtp_params[:from_email].blank?
      render json: {
        success: false,
        error: "Please fill in all required fields"
      }, status: :unprocessable_entity
      return
    end

    begin
      # Test SMTP connection and authentication directly using Net::SMTP
      require "net/smtp"

      domain = smtp_params[:smtp_domain].presence || smtp_params[:from_email].split("@").last
      authentication = smtp_params[:smtp_authentication].presence || "plain"
      enable_starttls = smtp_params[:smtp_enable_starttls] != "0"
      port = smtp_params[:smtp_port].to_i

      # Convert authentication string to symbol
      auth_type = case authentication.to_s.downcase
      when "plain"
                    :plain
      when "login"
                    :login
      when "cram_md5"
                    :cram_md5
      else
                    :plain
      end

      # Use Net::SMTP to directly test connection and authentication
      smtp = Net::SMTP.new(smtp_params[:smtp_address], port)
      smtp.open_timeout = 5
      smtp.read_timeout = 5

      # Enable STARTTLS if needed (must be done before start)
      if enable_starttls
        smtp.enable_starttls
      end

      # Start SMTP session with authentication
      # This will raise Net::SMTPAuthenticationError if credentials are invalid
      smtp.start(domain, smtp_params[:smtp_user_name], smtp_params[:smtp_password], auth_type) do |smtp_session|
        # Connection and authentication successful if we reach here
        # We can optionally send a test email, but just connecting and authenticating is enough for verification
      end

      # If we get here, the connection and authentication were successful
      ActivityLog.log!(
        action: :verified,
        target: :newsletter,
        level: :info,
        mode: "smtp"
      )

      render json: {
        success: true,
        message: "SMTP configuration verified successfully!"
      }
    rescue Net::SMTPAuthenticationError, Net::SMTPFatalError => e
      ActivityLog.log!(
        action: :failed,
        target: :newsletter,
        level: :error,
        mode: "smtp",
        error: e.message
      )
      render json: {
        success: false,
        error: "Authentication failed: Invalid credentials. Please check your username and password."
      }, status: :unprocessable_entity
    rescue Net::SMTPError, Errno::ECONNREFUSED, Timeout::Error => e
      ActivityLog.log!(
        action: :failed,
        target: :newsletter,
        level: :error,
        mode: "smtp",
        error: e.message
      )
      error_message = if e.is_a?(Timeout::Error)
        "Connection timeout. Please check your SMTP address, port and network connection."
      elsif e.is_a?(Errno::ECONNREFUSED)
        "Connection refused. Please check your SMTP address and port settings."
      else
        "SMTP error: #{e.message}"
      end
      render json: {
        success: false,
        error: error_message
      }, status: :unprocessable_entity
    rescue => e
      ActivityLog.log!(
        action: :failed,
        target: :newsletter,
        level: :error,
        mode: "smtp",
        error: e.message
      )
      render json: {
        success: false,
        error: "Verification failed: #{e.message}"
      }, status: :unprocessable_entity
    end
  end

  def update
    @newsletter_setting = NewsletterSetting.instance
    @listmonk = Listmonk.first_or_initialize
    @active_tab = newsletter_tab(params[:tab])

    # Update newsletter setting (native email)
    if params[:newsletter_setting].present?
      setting_params = newsletter_setting_params

      # 如果密码是占位符（••••••••）或为空，且数据库中有保存的密码，则保留原有密码
      if (setting_params[:smtp_password] == "••••••••" || setting_params[:smtp_password].blank?) && @newsletter_setting.smtp_password.present?
        setting_params[:smtp_password] = @newsletter_setting.smtp_password
      end

      if @newsletter_setting.update(setting_params)
        # Clear newsletter setting cache
        CacheableSettings.refresh_newsletter_setting
        redirect_to admin_newsletter_path(tab: @active_tab), notice: "Newsletter settings updated successfully."
        return
      else
        load_listmonk_options
        flash.now[:alert] = @newsletter_setting.errors.full_messages.join(", ")
        render :show, status: :unprocessable_entity
        return
      end
    end

    # Update listmonk settings
    if @listmonk.update(listmonk_params)
      redirect_to admin_newsletter_path(tab: @active_tab), notice: "Newsletter settings updated successfully."
    else
      load_listmonk_options
      flash.now[:alert] = @listmonk.errors.full_messages.join(", ")
      render :show, status: :unprocessable_entity
    end
  end

  private

  def newsletter_setting_params
    params.require(:newsletter_setting)
      .except(:submit)
      .permit(:provider, :enabled, :smtp_address, :smtp_port, :smtp_user_name, :smtp_password, :smtp_domain, :smtp_authentication, :smtp_enable_starttls, :from_email, :footer)
  end

  def listmonk_params
    params.expect(listmonk: [ :enabled, :username, :api_key, :url, :list_id, :template_id ])
  end

  def verify_params
    params.permit(:username, :api_key, :url, :list_id, :template_id)
  end

  def verify_smtp_params
    params.permit(:smtp_address, :smtp_port, :smtp_user_name, :smtp_password, :smtp_domain, :smtp_authentication, :smtp_enable_starttls, :from_email)
  end

  def load_listmonk_options
    @lists = []
    @templates = []
    return unless @newsletter_setting.listmonk? && @listmonk.persisted? && @listmonk.configured?

    @lists = @listmonk.fetch_lists || []
    @templates = @listmonk.fetch_templates || []
  end

  def newsletter_tab(value)
    %w[general native listmonk].include?(value) ? value : "general"
  end
end
