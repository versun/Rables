class SetupController < ApplicationController
  layout "admin"
  helper SettingsHelper
  allow_unauthenticated_access

  before_action :redirect_if_setup_completed

  def show
    @user = User.new
    @setting = Setting.first_or_create
  end

  def create
    ActiveRecord::Base.transaction do
      # Re-check inside the transaction to shrink the TOCTOU window
      # (fresh query: the cached setup_incomplete? may hold a stale value)
      unless Setting.setup_incomplete_fresh?
        redirect_to admin_root_path, notice: "Setup has already been completed."
        raise ActiveRecord::Rollback
      end

      # Create admin user
      @user = User.new(user_params)
      unless @user.save
        ActivityLog.log!(
          action: :failed,
          target: :setup,
          level: :error,
          step: "create_user",
          errors: @user.errors.full_messages.join(", ")
        )
        @setting = Setting.first_or_create
        render :show, status: :unprocessable_entity
        raise ActiveRecord::Rollback
        return
      end

      # Update site settings
      @setting = Setting.first_or_create
      unless @setting.update(setting_params.merge(setup_completed: true))
        ActivityLog.log!(
          action: :failed,
          target: :setup,
          level: :error,
          step: "update_settings",
          errors: @setting.errors.full_messages.join(", ")
        )
        render :show, status: :unprocessable_entity
        raise ActiveRecord::Rollback
        return
      end

      # Refresh settings cache
      CacheableSettings.refresh_site_info

      ActivityLog.log!(
        action: :completed,
        target: :setup,
        level: :info,
        message: "setup_completed"
      )

      redirect_to new_session_path, notice: "Setup completed successfully! Please log in with your admin credentials."
    end
  end

  private

  def redirect_if_setup_completed
    unless Setting.setup_incomplete?
      redirect_to admin_root_path, notice: "Setup has already been completed."
    end
  end

  def user_params
    params.require(:user).permit(:user_name, :password, :password_confirmation)
  end

  def setting_params
    params.require(:setting).permit(:title, :description, :author, :url, :time_zone)
  end
end
