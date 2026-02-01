# frozen_string_literal: true

module Admin
  class JekyllController < BaseController
    before_action :set_setting

    def show
      @sync_records = JekyllSyncRecord.recent.limit(10)
    end

    def update
      if @setting.update(setting_params)
        redirect_to admin_jekyll_path, notice: "Jekyll settings updated successfully."
      else
        @sync_records = JekyllSyncRecord.recent.limit(10)
        render :show, status: :unprocessable_entity
      end
    end

    def sync
      if @setting.configured? && @setting.jekyll_path_valid?
        JekyllSyncJob.perform_later("full", "manual")
        redirect_to admin_jekyll_path, notice: "Full sync started. Check the sync history for progress."
      else
        redirect_to admin_jekyll_path, alert: "Jekyll is not properly configured. Please check your settings."
      end
    end

    def sync_article
      article = Article.find_by(slug: params[:article_id])

      if article.nil?
        redirect_to admin_jekyll_path, alert: "Article not found."
        return
      end

      if @setting.configured? && @setting.jekyll_path_valid?
        JekyllSingleSyncJob.perform_later("Article", article.id, "manual")
        redirect_to admin_jekyll_path, notice: "Article sync started: #{article.title}"
      else
        redirect_to admin_jekyll_path, alert: "Jekyll is not properly configured."
      end
    end

    def verify
      errors = []

      if @setting.jekyll_path.blank?
        errors << "Jekyll path is not configured"
      elsif !File.directory?(@setting.jekyll_path)
        errors << "Jekyll path does not exist"
      elsif !File.writable?(@setting.jekyll_path)
        errors << "Jekyll path is not writable"
      end

      if @setting.git_repository? && @setting.repository_url.blank?
        errors << "Repository URL is required for Git repository type"
      end

      if errors.empty?
        redirect_to admin_jekyll_path, notice: "Configuration verified successfully!"
      else
        redirect_to admin_jekyll_path, alert: "Configuration errors: #{errors.join(', ')}"
      end
    end

    def preview
      @article = Article.find_by(slug: params[:article_id])

      if @article.nil?
        redirect_to admin_jekyll_path, alert: "Article not found."
        return
      end

      service = JekyllSyncService.new(@setting)
      @preview_content = service.preview_article(@article)
    end

    private

    def set_setting
      @setting = JekyllSetting.instance
    end

    def setting_params
      params.require(:jekyll_setting).permit(
        :jekyll_path,
        :repository_type,
        :repository_url,
        :branch,
        :posts_directory,
        :pages_directory,
        :assets_directory,
        :images_directory,
        :front_matter_mapping,
        :auto_sync_enabled,
        :sync_on_publish,
        :redirect_export_format,
        :static_files_directory,
        :preserve_original_paths,
        :export_comments,
        :comments_format,
        :include_pending_comments,
        :include_social_comments,
        :download_remote_images
      )
    end
  end
end
