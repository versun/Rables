class Admin::ArticlesController < Admin::BaseController
  before_action :set_article, only: [ :show, :edit, :update, :destroy, :publish, :unpublish, :fetch_comments ]

  def index
    @scope = Article.all
    @articles = fetch_articles(@scope)
    @path = admin_articles_path
  end

  def show
  end

  def new
    @article = Article.new(comment: true)
  end

  def edit
  end

  def create
    @article = Article.new(article_params)

    respond_to do |format|
      if @article.save
        ActivityLog.log!(
          action: :created,
          target: :article,
          level: :info,
          title: @article.title,
          slug: @article.slug
        )
        if params[:create_and_add_another].present?
          format.html { redirect_to new_admin_article_path, notice: "Article was successfully created." }
        else
          format.html { redirect_to admin_articles_path, notice: "Article was successfully created." }
        end
        format.json { render :show, status: :created, location: @article }
      else
        ActivityLog.log!(
          action: :failed,
          target: :article,
          level: :error,
          title: @article.title,
          slug: @article.slug,
          errors: @article.errors.full_messages.join(", ")
        )
        format.html { render :new }
        format.json { render json: @article.errors, status: :unprocessable_entity }
      end
    end
  end

  def update
    respond_to do |format|
      if @article.update(article_params)
        ActivityLog.log!(
          action: :updated,
          target: :article,
          level: :info,
          title: @article.title,
          slug: @article.slug
        )
        format.html { redirect_to admin_articles_path, notice: "Article was successfully updated." }
        format.json { render :show, status: :ok, location: @article }
      else
        ActivityLog.log!(
          action: :failed,
          target: :article,
          level: :error,
          title: @article.title,
          slug: @article.slug,
          errors: @article.errors.full_messages.join(", ")
        )
        format.html { render :edit }
        format.json { render json: @article.errors, status: :unprocessable_entity }
      end
    end
  end

  def destroy
    article_title = @article.title
    if @article.status != "trash"
      @article.update(status: "trash")
      ActivityLog.log!(
        action: :trashed,
        target: :article,
        level: :info,
        title: article_title,
        slug: @article.slug
      )
      notice_message = "Article was successfully moved to trash."
    else
      @article.destroy!
      ActivityLog.log!(
        action: :deleted,
        target: :article,
        level: :info,
        title: article_title,
        slug: @article.slug
      )
      notice_message = "Article was successfully deleted."
    end

    respond_to do |format|
      format.html { redirect_to admin_articles_path, status: :see_other, notice: notice_message }
      format.json { head :no_content }
    end
  end

  def drafts
    @scope = Article.draft
    @articles = fetch_articles(@scope)
    @path = drafts_admin_articles_path
    render :index
  end

  def scheduled
    @scope = Article.scheduled
    @articles = fetch_articles(@scope)
    @path = scheduled_admin_articles_path
    render :index
  end

  def publish
    if @article.update(status: :publish)
      ActivityLog.log!(
        action: :published,
        target: :article,
        level: :info,
        title: @article.title,
        slug: @article.slug
      )

      # Trigger Jekyll sync if enabled
      trigger_jekyll_sync(@article)

      redirect_to admin_articles_path, notice: "Article was successfully published."
    else
      ActivityLog.log!(
        action: :failed,
        target: :article,
        level: :error,
        title: @article.title,
        slug: @article.slug,
        errors: @article.errors.full_messages.join(", ")
      )
      redirect_to admin_articles_path, alert: "Failed to publish article."
    end
  end

  def unpublish
    if @article.update(status: :draft)
      ActivityLog.log!(
        action: :unpublished,
        target: :article,
        level: :info,
        title: @article.title,
        slug: @article.slug
      )
      redirect_to admin_articles_path, notice: "Article was successfully unpublished."
    else
      ActivityLog.log!(
        action: :failed,
        target: :article,
        level: :error,
        title: @article.title,
        slug: @article.slug,
        errors: @article.errors.full_messages.join(", ")
      )
      redirect_to admin_articles_path, alert: "Failed to unpublish article."
    end
  end

  def batch_add_tags
    ids = params[:ids] || []
    tag_names = params[:tag_names] || ""

    if ids.empty?
      redirect_to admin_articles_path, alert: "请至少选择一个文章。"
      return
    end

    if tag_names.blank?
      redirect_to admin_articles_path, alert: "请输入至少一个标签。"
      return
    end

    count = 0
    errors = []

    ids.each do |id|
      article = Article.find_by(slug: id)
      next unless article

      begin
        # 创建或查找新标签
        new_tags = Tag.find_or_create_by_names(tag_names).compact

        if new_tags.empty?
          errors << "#{article.title || article.slug || 'Unknown'}: 无法创建标签"
          next
        end

        # 获取现有标签的ID
        existing_tag_ids = article.tags.pluck(:id)

        # 添加新标签（只添加不存在的标签）
        new_tags.each do |tag|
          next unless tag&.id # 确保tag和id都存在
          unless existing_tag_ids.include?(tag.id)
            ArticleTag.find_or_create_by(article_id: article.id, tag_id: tag.id)
          end
        end

        count += 1
      rescue => e
        errors << "#{article.title || article.slug || 'Unknown'}: #{e.message}"
      end
    end

    if errors.any?
      ActivityLog.log!(
        action: :updated,
        target: :article,
        level: :warn,
        count: count,
        error_count: errors.size,
        tags: tag_names,
        errors: errors.join("; ")
      )
      redirect_to admin_articles_path, alert: "成功添加标签到 #{count} 篇文章。错误: #{errors.join('; ')}"
    else
      ActivityLog.log!(
        action: :updated,
        target: :article,
        level: :info,
        count: count,
        tags: tag_names
      )
      redirect_to admin_articles_path, notice: "成功添加标签到 #{count} 篇文章。"
    end
  end

  def batch_crosspost
    ids = params[:ids] || []
    platforms = params[:platforms] || []

    if ids.empty?
      redirect_to admin_articles_path, alert: "请至少选择一个文章。"
      return
    end

    if platforms.empty?
      redirect_to admin_articles_path, alert: "请至少选择一个平台。"
      return
    end

    count = 0
    errors = []

    ids.each do |id|
      article = Article.find_by(slug: id)
      next unless article
      unless article.publish?
        errors << "#{article.title}: 文章未发布，无法进行跨平台发布"
        next
      end

      begin
        jobs_queued = false
        platforms.each do |platform|
          # 检查平台是否启用
          crosspost = Crosspost.find_by(platform: platform)
          next unless crosspost&.enabled?

          CrosspostArticleJob.perform_later(article.id, platform)
          jobs_queued = true
        end
        count += 1 if jobs_queued
      rescue => e
        errors << "#{article.title}: #{e.message}"
      end
    end

    if errors.any?
      ActivityLog.log!(
        action: :queued,
        target: :crosspost,
        level: :warn,
        count: count,
        platforms: platforms,
        error_count: errors.size,
        errors: errors.join("; ")
      )
      redirect_to admin_articles_path, alert: "成功提交 #{count} 篇文章进行跨平台发布。错误: #{errors.join('; ')}"
    else
      ActivityLog.log!(
        action: :queued,
        target: :crosspost,
        level: :info,
        count: count,
        platforms: platforms
      )
      redirect_to admin_articles_path, notice: "成功提交 #{count} 篇文章进行跨平台发布。"
    end
  end

  def batch_newsletter
    ids = params[:ids] || []

    if ids.empty?
      redirect_to admin_articles_path, alert: "请至少选择一个文章。"
      return
    end

    count = 0
    errors = []

    ids.each do |id|
      article = Article.find_by(slug: id)
      next unless article
      unless article.publish?
        errors << "#{article.title}: 文章未发布，无法发送邮件"
        next
      end

      begin
        # 检查newsletter配置
        newsletter_setting = NewsletterSetting.instance
        if newsletter_setting.enabled? && newsletter_setting.configured?
          if newsletter_setting.native?
            NativeNewsletterSenderJob.perform_later(article.id)
          elsif newsletter_setting.listmonk?
            ListmonkSenderJob.perform_later(article.id)
          end
          count += 1
        else
          errors << "#{article.title}: Newsletter未配置或未启用"
        end
      rescue => e
        errors << "#{article.title}: #{e.message}"
      end
    end

    if errors.any?
      ActivityLog.log!(
        action: :queued,
        target: :newsletter,
        level: :warn,
        count: count,
        error_count: errors.size,
        errors: errors.join("; ")
      )
      redirect_to admin_articles_path, alert: "成功提交 #{count} 篇文章发送邮件。错误: #{errors.join('; ')}"
    else
      ActivityLog.log!(
        action: :queued,
        target: :newsletter,
        level: :info,
        count: count
      )
      redirect_to admin_articles_path, notice: "成功提交 #{count} 篇文章发送邮件。"
    end
  end

  def batch_destroy
    ids = params[:ids] || []

    if ids.empty?
      redirect_to admin_articles_path, alert: "请至少选择一个文章。"
      return
    end

    trashed_count = 0
    deleted_count = 0
    errors = []

    ids.each do |id|
      article = Article.find_by(slug: id)
      next unless article

      begin
        if article.status != "trash"
          article.update(status: "trash")
          trashed_count += 1
        else
          article.destroy!
          deleted_count += 1
        end
      rescue => e
        errors << "#{article.title || article.slug || 'Unknown'}: #{e.message}"
      end
    end

    messages = []
    messages << "成功将 #{trashed_count} 篇文章移动到垃圾箱。" if trashed_count > 0
    messages << "成功删除 #{deleted_count} 篇文章。" if deleted_count > 0

    if errors.any?
      ActivityLog.log!(
        action: :deleted,
        target: :article,
        level: :warn,
        trashed_count: trashed_count,
        deleted_count: deleted_count,
        error_count: errors.size,
        errors: errors.join("; ")
      )
      redirect_to admin_articles_path, alert: "#{messages.join(' ')}错误: #{errors.join('; ')}"
    else
      ActivityLog.log!(
        action: :deleted,
        target: :article,
        level: :info,
        trashed_count: trashed_count,
        deleted_count: deleted_count
      )
      redirect_to admin_articles_path, notice: messages.join(" ")
    end
  end

  def fetch_comments
    # Get all social media posts for this article
    social_posts = @article.social_media_posts.where.not(url: nil)

    if params[:platform].present?
      social_posts = social_posts.where(platform: params[:platform])
    end

    if social_posts.empty?
      render json: { success: false, message: "No social media posts found for this article" }, status: :unprocessable_entity
      return
    end

    total_fetched = 0
    results = []
    errors = []

    social_posts.each do |post|
      begin
        service = case post.platform
        when "mastodon"
          MastodonService.new
        when "bluesky"
          BlueskyService.new
        when "twitter"
          TwitterService.new
        else
          next
        end

        result = service.fetch_comments(post.url)
        comments_data = result[:comments] || []

        # Create or update comments
        # First pass: create/update all comments and build external_id -> comment mapping
        platform_count = 0
        external_id_to_comment = {}

        comments_data.each do |comment_data|
          # Skip comments without content (required field)
          next if comment_data[:content].blank?

          comment = @article.comments.find_or_initialize_by(
            platform: post.platform,
            external_id: comment_data[:external_id]
          )

          comment.assign_attributes(
            author_name: comment_data[:author_name],
            author_username: comment_data[:author_username],
            author_avatar_url: comment_data[:author_avatar_url],
            content: comment_data[:content],
            published_at: comment_data[:published_at],
            url: comment_data[:url],
            status: :approved  # Auto-approve external comments
          )

          if comment.new_record?
            comment.save!
            platform_count += 1
          elsif comment.changed?
            comment.save!
          end

          # Store mapping for parent lookup
          external_id_to_comment[comment_data[:external_id]] = comment
        end

        # Second pass: set parent relationships based on parent_external_id
        comments_data.each do |comment_data|
          # Skip comments without content to match first pass behavior
          next if comment_data[:content].blank?
          next unless comment_data[:parent_external_id]

          comment = external_id_to_comment[comment_data[:external_id]]
          parent_comment = external_id_to_comment[comment_data[:parent_external_id]]

          # Only set parent if both comment and parent exist and are from the same platform
          if comment && parent_comment && comment.platform == parent_comment.platform
            comment.update(parent_id: parent_comment.id) if comment.parent_id != parent_comment.id
          end
        end

        total_fetched += platform_count
        results << { platform: post.platform.titleize, fetched: platform_count }

        # Log activity
        ActivityLog.log!(
          action: :fetched,
          target: :fetch_comments,
          level: :info,
          title: @article.title,
          slug: @article.slug,
          platform: post.platform,
          count: platform_count
        )
      rescue => e
        error_msg = "Failed to fetch #{post.platform} comments: #{e.message}"
        errors << error_msg
        Rails.event.notify(
          "admin.articles_controller.fetch_comments_error",
          level: "error",
          component: "Admin::ArticlesController",
          platform: post.platform,
          message: e.message,
          backtrace: e.backtrace,
          article_id: @article.id,
          article_title: @article.title
        )

        ActivityLog.log!(
          action: :failed,
          target: :fetch_comments,
          level: :error,
          title: @article.title,
          slug: @article.slug,
          platform: post.platform,
          error: e.message
        )
      end
    end

    if errors.any?
      render json: {
        success: false,
        message: "Completed with errors. Fetched #{total_fetched} total comments.",
        results: results,
        errors: errors
      }, status: :partial_content
    else
      render json: {
        success: true,
        message: "Successfully fetched #{total_fetched} total comments from #{results.size} platform(s)",
        results: results
      }
    end
  end

  private

  def set_article
    @article = Article.find_by!(slug: params[:id])
  end

    def article_params
      params.require(:article).permit(:title, :content, :excerpt, :slug, :status, :published_at, :meta_title, :meta_description, :meta_image, :tags, :description, :created_at, :scheduled_at, :send_newsletter, :crosspost_mastodon, :crosspost_twitter, :crosspost_bluesky, :crosspost_xiaohongshu, :tag_list, :comment, :content_type, :html_content, :source_url, :source_author, :source_content, social_media_posts_attributes: [ :id, :platform, :url, :_destroy ])
    end

    def trigger_jekyll_sync(article)
      setting = JekyllSetting.instance
      return unless setting.configured? && setting.sync_on_publish?

      JekyllSingleSyncJob.perform_later("Article", article.id, "publish")
    end
end
