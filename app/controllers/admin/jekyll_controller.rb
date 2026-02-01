module Admin
  class JekyllController < BaseController
    before_action :set_setting

    def show
      @sync_records = JekyllSyncRecord.recent.limit(10)
      @verification_errors = @setting.jekyll_path_valid? ? [] : [ "Jekyll path is not configured or invalid" ]
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
      # Trigger full sync in background
      JekyllSyncJob.perform_later(sync_type: "full")

      redirect_to admin_jekyll_path, notice: "Full sync started. This may take a few minutes."
    end

    def sync_article
      article = Article.find_by(slug: params[:article_slug])

      if article
        JekyllSingleSyncJob.perform_later("article", article.id, action: :sync)
        redirect_to admin_jekyll_path, notice: "Article sync queued for '#{article.title}'."
      else
        redirect_to admin_jekyll_path, alert: "Article not found."
      end
    end

    def verify
      service = JekyllSyncService.new(@setting)
      @verification_errors = service.verify

      if @verification_errors.empty?
        redirect_to admin_jekyll_path, notice: "Jekyll configuration verified successfully."
      else
        flash.now[:alert] = "Verification failed: #{@verification_errors.join(', ')}"
        @sync_records = JekyllSyncRecord.recent.limit(10)
        render :show, status: :unprocessable_entity
      end
    end

    def preview
      @article = Article.find_by(slug: params[:article_slug])
      @page = Page.find_by(slug: params[:page_slug])

      exporter = JekyllExport.new(@setting)

      if @article
        @content = exporter.article_to_markdown(@article)
        @filename = exporter.article_filename(@article)
      elsif @page
        @content = exporter.page_to_markdown(@page)
        @filename = exporter.page_filename(@page)
      else
        redirect_to admin_jekyll_path, alert: "Please specify an article or page to preview."
        return
      end

      render :preview
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
        :images_directory,
        :download_remote_images
      )
    end
  end
end
