class Admin::JekyllController < Admin::BaseController
  def show
    @setting = JekyllSetting.instance
    @recent_syncs = JekyllSyncRecord.order(created_at: :desc).limit(5)
  end

  def update
    @setting = JekyllSetting.instance

    if @setting.update(jekyll_setting_params)
      redirect_to admin_jekyll_path, notice: "Jekyll settings updated."
    else
      @recent_syncs = JekyllSyncRecord.order(created_at: :desc).limit(5)
      flash.now[:alert] = @setting.errors.full_messages.join(", ")
      render :show, status: :unprocessable_entity
    end
  end

  def sync
    JekyllSyncJob.perform_later
    redirect_to admin_jekyll_path, notice: "Jekyll sync has started."
  end

  def sync_article
    article = Article.find_by!(slug: params[:article_id])
    JekyllSingleSyncJob.perform_later("Article", article.id)
    redirect_to admin_jekyll_path, notice: "Article sync has started."
  end

  def verify
    setting = JekyllSetting.instance
    if setting.valid?
      render json: { success: true, message: "Jekyll path is valid." }
    else
      render json: { success: false, message: setting.errors.full_messages.join(", ") }, status: :unprocessable_entity
    end
  end

  def preview
    @setting = JekyllSetting.instance
    type = params[:type]
    id = params[:id]

    @item =
      case type
      when "article" then Article.find(id)
      when "page" then Page.find(id)
      else
        nil
      end

    if @item.nil?
      redirect_to admin_jekyll_path, alert: "Preview target not found."
      return
    end

    @markdown = JekyllExport.new(setting: @setting).preview(@item)
  rescue ActiveRecord::RecordNotFound
    redirect_to admin_jekyll_path, alert: "Preview target not found."
  end

  private

  def jekyll_setting_params
    params.require(:jekyll_setting).permit(
      :jekyll_path,
      :repository_type,
      :repository_url,
      :branch,
      :posts_directory,
      :pages_directory,
      :assets_directory,
      :images_directory,
      :front_matter_mapping_json,
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
