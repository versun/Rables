class ApplicationController < ActionController::Base
  include CacheableSettings
  before_action :set_time_zone
  before_action :redirect_to_setup_if_needed

  include Authentication
  # Only allow modern browsers supporting webp images, web push, badges, import maps, CSS nesting, and CSS :has.
  # allow_browser versions: :modern
  protect_from_forgery with: :exception

  private

  def set_time_zone
    Time.zone = CacheableSettings.site_info[:time_zone] || "UTC"
  end

  def refresh_settings
    CacheableSettings.refresh_site_info
  end

  def redirect_to_setup_if_needed
    # Skip if we're already on the setup page or dealing with assets
    return if controller_name == "setup" || request.path.start_with?("/assets", "/rails/active_storage")

    # Redirect to setup if setup is incomplete
    if Setting.setup_incomplete?
      redirect_to setup_path unless request.path == setup_path
    end
  end
end
