class Export
  require "csv"
  require "fileutils"
  require "nokogiri"
  require "open-uri"
  require "securerandom"
  require "zip"

  include Exports::HtmlAttachmentProcessing
  include Exports::ZipPackaging

  attr_reader :zip_path, :error_message, :export_dir, :attachments_dir

  def initialize
    @zip_path = nil
    @error_message = nil
    timestamp = Time.current.strftime("%Y%m%d_%H%M%S")
    unique_suffix = "#{Process.pid}_#{SecureRandom.hex(4)}"
    @export_dir = Rails.root.join("tmp", "exports", "export_#{timestamp}_#{unique_suffix}")
    @attachments_dir = File.join(@export_dir, "attachments")

    # 创建导出目录
    FileUtils.mkdir_p(@export_dir)
    FileUtils.mkdir_p(@attachments_dir)

    # 检查数据库连接
    check_database_connection
  end

  def check_database_connection
    begin
      # 尝试执行一个简单的查询来检查数据库连接
      ActiveRecord::Base.connection.execute("SELECT 1")
      true
    rescue => e
      @error_message = "Database connection failed: #{e.message}"
      Rails.event.notify("export.database_connection_failed", component: "Export", error: e.message, level: "error")
      false
    end
  end

  def generate
    begin
      Rails.event.notify("export.generation_started", component: "Export", export_dir: @export_dir, level: "info")

      # export_activity_logs
      export_articles
      export_crossposts
      export_listmonks
      export_git_integrations
      export_pages
      export_settings
      export_social_media_posts
      export_tags
      export_comments
      export_static_files
      export_redirects
      export_newsletter_settings
      export_subscribers
      export_article_tags
      export_subscriber_tags
      # export_users

      # 创建ZIP文件
      create_zip_file

      Rails.event.notify("export.generation_completed", component: "Export", level: "info")
      true
    rescue => e
      @error_message = e.message
      Rails.event.notify("export.generation_failed", component: "Export", error: e.message, backtrace: e.backtrace.join("\n"), level: "error")
      false
    end
  end

  private

  def export_activity_logs
    Rails.event.notify("export.activity_logs_started", component: "Export", level: "info")

    CSV.open(File.join(@export_dir, "activity_logs.csv"), "w", write_headers: true, headers: %w[id action target level description created_at updated_at]) do |csv|
      ActivityLog.order(:id).find_each do |log|
        csv << [
          log.id,
          log.action,
          log.target,
          log.level,
          log.description,
          log.created_at,
          log.updated_at
        ]
      end
    end

    Rails.event.notify("export.activity_logs_completed", component: "Export", count: ActivityLog.count, level: "info")
  end

  def export_articles
    Rails.event.notify("export.articles_started", component: "Export", level: "info")

    CSV.open(
      File.join(@export_dir, "articles.csv"),
      "w",
      write_headers: true,
      headers: %w[
        id title slug description content status scheduled_at
        scheduled_crosspost_platforms
        source_url source_author source_content
        meta_title meta_description meta_image
        content_type html_content comment
        created_at updated_at
      ]
    ) do |csv|
      Article.includes(:rich_text_content).order(:id).find_each do |article|
        # 处理文章内容和附件
        processed_content = process_article_content(article)

        csv << [
          article.id,
          article.title,
          article.slug,
          article.description,
          processed_content,
          article.status,
          article.scheduled_at,
          article.scheduled_crosspost_platforms.to_json,
          article.source_url,
          article.source_author,
          article.source_content,
          article.meta_title,
          article.meta_description,
          article.meta_image,
          article.content_type,
          article.html_content,
          article.comment,
          article.created_at,
          article.updated_at
        ]
      end
    end

    Rails.event.notify("export.articles_completed", component: "Export", count: Article.count, level: "info")
  end

  def process_article_content(article)
    # 根据 content_type 获取内容
    if article.html?
      content_html = article.html_content || ""
    else
      return "" unless article.content.present?
      content_html = article.content.to_trix_html
    end

    process_html_content(content_html, record_id: article.id, record_type: "article")
  end

  def export_crossposts
    Rails.event.notify("export.crossposts_started", component: "Export", level: "info")

    CSV.open(
      File.join(@export_dir, "crossposts.csv"),
      "w",
      write_headers: true,
      headers: %w[
        id platform server_url client_key client_secret
        access_token access_token_secret api_key api_key_secret
        username app_password enabled auto_fetch_comments
        comment_fetch_schedule max_characters settings
        created_at updated_at
      ]
    ) do |csv|
      Crosspost.order(:id).find_each do |crosspost|
        csv << [
          crosspost.id,
          crosspost.platform,
          crosspost.server_url,
          crosspost.client_key,
          crosspost.client_secret,
          crosspost.access_token,
          crosspost.access_token_secret,
          crosspost.api_key,
          crosspost.api_key_secret,
          crosspost.username,
          crosspost.app_password,
          crosspost.enabled,
          crosspost.auto_fetch_comments,
          crosspost.comment_fetch_schedule,
          crosspost.max_characters,
          crosspost.settings&.to_json,
          crosspost.created_at,
          crosspost.updated_at
        ]
      end
    end

    Rails.event.notify("export.crossposts_completed", component: "Export", count: Crosspost.count, level: "info")
  end

  def export_listmonks
    Rails.event.notify("export.listmonks_started", component: "Export", level: "info")

    CSV.open(File.join(@export_dir, "listmonks.csv"), "w", write_headers: true, headers: %w[id url username api_key list_id template_id enabled created_at updated_at]) do |csv|
      Listmonk.order(:id).find_each do |listmonk|
        csv << [
          listmonk.id,
          listmonk.url,
          listmonk.username,
          listmonk.api_key,
          listmonk.list_id,
          listmonk.template_id,
          listmonk.enabled,
          listmonk.created_at,
          listmonk.updated_at
        ]
      end
    end

    Rails.event.notify("export.listmonks_completed", component: "Export", count: Listmonk.count, level: "info")
  end

  def export_git_integrations
    Rails.event.notify("export.git_integrations_started", component: "Export", level: "info")

    CSV.open(
      File.join(@export_dir, "git_integrations.csv"),
      "w",
      write_headers: true,
      headers: %w[id provider name server_url username access_token enabled created_at updated_at]
    ) do |csv|
      GitIntegration.order(:id).find_each do |integration|
        csv << [
          integration.id,
          integration.provider,
          integration.name,
          integration.server_url,
          integration.username,
          integration.access_token,
          integration.enabled,
          integration.created_at,
          integration.updated_at
        ]
      end
    end

    Rails.event.notify("export.git_integrations_completed", component: "Export", count: GitIntegration.count, level: "info")
  end

  def export_pages
    Rails.event.notify("export.pages_started", component: "Export", level: "info")

    CSV.open(File.join(@export_dir, "pages.csv"), "w", write_headers: true, headers: %w[id title slug content status redirect_url page_order content_type html_content comment created_at updated_at]) do |csv|
      Page.includes(:rich_text_content).order(:id).find_each do |page|
        # 处理页面内容（如果有富文本内容的话）
        content = page.content.present? ? process_page_content(page) : ""

        csv << [
          page.id,
          page.title,
          page.slug,
          content,
          page.status,
          page.redirect_url,
          page.page_order,
          page.content_type,
          page.html_content,
          page.comment,
          page.created_at,
          page.updated_at
        ]
      end
    end

    Rails.event.notify("export.pages_completed", component: "Export", count: Page.count, level: "info")
  end

  def process_page_content(page)
    return "" unless page.content.present?

    content_html = page.content.to_trix_html
    process_html_content(content_html, record_id: page.id, record_type: "page")
  end

  def export_settings
    Rails.event.notify("export.settings_started", component: "Export", level: "info")

    CSV.open(
      File.join(@export_dir, "settings.csv"),
      "w",
      write_headers: true,
      headers: %w[
        id title description author url time_zone head_code custom_css
        social_links footer tool_code giscus static_files
        auto_regenerate_triggers deploy_branch deploy_provider deploy_repo_url
        local_generation_path static_generation_destination static_generation_delay
        setup_completed github_backup_enabled github_repo_url github_token
        github_backup_branch created_at updated_at
      ]
    ) do |csv|
      Setting.includes(:rich_text_footer).order(:id).find_each do |setting|
        # 处理footer内容（如果有富文本内容的话）
        footer_content = setting.footer.present? ? process_setting_footer(setting) : ""

        csv << [
          setting.id,
          setting.title,
          setting.description,
          setting.author,
          setting.url,
          setting.time_zone,
          setting.head_code,
          setting.custom_css,
          setting.social_links&.to_json,
          footer_content,
          setting.tool_code,
          setting.giscus,
          setting.static_files&.to_json,
          setting.auto_regenerate_triggers&.to_json,
          setting.deploy_branch,
          setting.deploy_provider,
          setting.deploy_repo_url,
          setting.local_generation_path,
          setting.static_generation_destination,
          setting.static_generation_delay,
          setting.setup_completed,
          setting.github_backup_enabled,
          setting.github_repo_url,
          setting.github_token,
          setting.github_backup_branch,
          setting.created_at,
          setting.updated_at
        ]
      end
    end

    Rails.event.notify("export.settings_completed", component: "Export", count: Setting.count, level: "info")
  end

  def process_setting_footer(setting)
    return "" unless setting.footer.present?

    footer_html = setting.footer.to_trix_html
    process_html_content(footer_html, record_id: setting.id, record_type: "setting")
  end

  def export_social_media_posts
    Rails.event.notify("export.social_media_posts_started", component: "Export", level: "info")

    CSV.open(File.join(@export_dir, "social_media_posts.csv"), "w", write_headers: true, headers: %w[id article_id article_slug platform url created_at updated_at]) do |csv|
      SocialMediaPost.includes(:article).order(:id).find_each do |post|
        csv << [
          post.id,
          post.article_id,
          post.article&.slug,
          post.platform,
          post.url,
          post.created_at,
          post.updated_at
        ]
      end
    end

    Rails.event.notify("export.social_media_posts_completed", component: "Export", count: SocialMediaPost.count, level: "info")
  end

  def export_tags
    Rails.event.notify("export.tags_started", component: "Export", level: "info")

    CSV.open(File.join(@export_dir, "tags.csv"), "w", write_headers: true, headers: %w[id name slug created_at updated_at]) do |csv|
      Tag.order(:id).find_each do |tag|
        csv << [
          tag.id,
          tag.name,
          tag.slug,
          tag.created_at,
          tag.updated_at
        ]
      end
    end

    Rails.event.notify("export.tags_completed", component: "Export", count: Tag.count, level: "info")
  end

  def export_comments
    Rails.event.notify("export.comments_started", component: "Export", level: "info")

    CSV.open(File.join(@export_dir, "comments.csv"), "w", write_headers: true, headers: %w[id article_id article_slug parent_id author_name author_url author_username author_avatar_url content platform external_id status published_at url created_at updated_at]) do |csv|
      Comment.includes(:article).order(:id).find_each do |comment|
        csv << [
          comment.id,
          comment.article_id,
          comment.article&.slug,
          comment.parent_id,
          comment.author_name,
          comment.author_url,
          comment.author_username,
          comment.author_avatar_url,
          comment.content,
          comment.platform,
          comment.external_id,
          comment.status,
          comment.published_at,
          comment.url,
          comment.created_at,
          comment.updated_at
        ]
      end
    end

    Rails.event.notify("export.comments_completed", component: "Export", count: Comment.count, level: "info")
  end

  def export_static_files
    Rails.event.notify("export.static_files_started", component: "Export", level: "info")

    CSV.open(File.join(@export_dir, "static_files.csv"), "w", write_headers: true, headers: %w[id filename blob_filename description created_at updated_at]) do |csv|
      StaticFile.includes(file_attachment: :blob).order(:id).find_each do |static_file|
        unless static_file.file.attached?
          Rails.event.notify("export.static_file_skipped", component: "Export", static_file_id: static_file.id, reason: "no_attached_file", level: "warn")
          next
        end

        blob = static_file.file.blob
        blob_filename = blob.filename.to_s

        csv << [
          static_file.id,
          static_file.filename,
          blob_filename,
          static_file.description,
          static_file.created_at,
          static_file.updated_at
        ]

        # 导出静态文件的实际文件内容，使用 blob_filename
        begin
          file_path = File.join(@attachments_dir, "static_files", "#{static_file.id}_#{blob_filename}")
          FileUtils.mkdir_p(File.dirname(file_path))
          File.open(file_path, "wb") do |f|
            f.write(blob.download)
          end
          Rails.event.notify("export.static_file_exported", component: "Export", blob_filename: blob_filename, level: "info")
        rescue => e
          Rails.event.notify("export.static_file_export_failed", component: "Export", static_file_id: static_file.id, error: e.message, level: "error")
        end
      end
    end

    Rails.event.notify("export.static_files_completed", component: "Export", count: StaticFile.count, level: "info")
  end

  def export_redirects
    Rails.event.notify("export.redirects_started", component: "Export", level: "info")

    CSV.open(File.join(@export_dir, "redirects.csv"), "w", write_headers: true, headers: %w[id regex replacement enabled permanent created_at updated_at]) do |csv|
      Redirect.order(:id).find_each do |redirect|
        csv << [
          redirect.id,
          redirect.regex,
          redirect.replacement,
          redirect.enabled,
          redirect.permanent,
          redirect.created_at,
          redirect.updated_at
        ]
      end
    end

    Rails.event.notify("export.redirects_completed", component: "Export", count: Redirect.count, level: "info")
  end

  def export_newsletter_settings
    Rails.event.notify("export.newsletter_settings_started", component: "Export", level: "info")

    CSV.open(File.join(@export_dir, "newsletter_settings.csv"), "w", write_headers: true, headers: %w[id provider enabled smtp_address smtp_port smtp_user_name smtp_password smtp_domain smtp_authentication smtp_enable_starttls from_email footer created_at updated_at]) do |csv|
      NewsletterSetting.includes(:rich_text_footer).order(:id).find_each do |setting|
        # 处理footer内容（如果有富文本内容的话）
        footer_content = setting.footer.present? ? process_newsletter_setting_footer(setting) : ""

        csv << [
          setting.id,
          setting.provider,
          setting.enabled,
          setting.smtp_address,
          setting.smtp_port,
          setting.smtp_user_name,
          setting.smtp_password,
          setting.smtp_domain,
          setting.smtp_authentication,
          setting.smtp_enable_starttls,
          setting.from_email,
          footer_content,
          setting.created_at,
          setting.updated_at
        ]
      end
    end

    Rails.event.notify("export.newsletter_settings_completed", component: "Export", count: NewsletterSetting.count, level: "info")
  end

  def process_newsletter_setting_footer(setting)
    return "" unless setting.footer.present?

    footer_html = setting.footer.to_trix_html
    process_html_content(footer_html, record_id: setting.id, record_type: "newsletter_setting")
  end

  def export_subscribers
    Rails.event.notify("export.subscribers_started", component: "Export", level: "info")

    CSV.open(File.join(@export_dir, "subscribers.csv"), "w", write_headers: true, headers: %w[id email confirmation_token confirmed_at unsubscribe_token unsubscribed_at created_at updated_at]) do |csv|
      Subscriber.order(:id).find_each do |subscriber|
        csv << [
          subscriber.id,
          subscriber.email,
          subscriber.confirmation_token,
          subscriber.confirmed_at,
          subscriber.unsubscribe_token,
          subscriber.unsubscribed_at,
          subscriber.created_at,
          subscriber.updated_at
        ]
      end
    end

    Rails.event.notify("export.subscribers_completed", component: "Export", count: Subscriber.count, level: "info")
  end

  def export_article_tags
    Rails.event.notify("export.article_tags_started", component: "Export", level: "info")

    CSV.open(File.join(@export_dir, "article_tags.csv"), "w", write_headers: true, headers: %w[id article_id article_slug tag_id tag_name tag_slug created_at updated_at]) do |csv|
      ArticleTag.includes(:article, :tag).order(:id).find_each do |article_tag|
        csv << [
          article_tag.id,
          article_tag.article_id,
          article_tag.article&.slug,
          article_tag.tag_id,
          article_tag.tag&.name,
          article_tag.tag&.slug,
          article_tag.created_at,
          article_tag.updated_at
        ]
      end
    end

    Rails.event.notify("export.article_tags_completed", component: "Export", count: ArticleTag.count, level: "info")
  end

  def export_subscriber_tags
    Rails.event.notify("export.subscriber_tags_started", component: "Export", level: "info")

    CSV.open(File.join(@export_dir, "subscriber_tags.csv"), "w", write_headers: true, headers: %w[id subscriber_id subscriber_email tag_id tag_name tag_slug created_at updated_at]) do |csv|
      SubscriberTag.includes(:subscriber, :tag).order(:id).find_each do |subscriber_tag|
        csv << [
          subscriber_tag.id,
          subscriber_tag.subscriber_id,
          subscriber_tag.subscriber&.email,
          subscriber_tag.tag_id,
          subscriber_tag.tag&.name,
          subscriber_tag.tag&.slug,
          subscriber_tag.created_at,
          subscriber_tag.updated_at
        ]
      end
    end

    Rails.event.notify("export.subscriber_tags_completed", component: "Export", count: SubscriberTag.count, level: "info")
  end

  def export_users
    Rails.event.notify("export.users_started", component: "Export", level: "info")

    CSV.open(File.join(@export_dir, "users.csv"), "w", write_headers: true, headers: %w[id user_name created_at updated_at]) do |csv|
      User.order(:id).find_each do |user|
        csv << [
          user.id,
          user.user_name,
          user.created_at,
          user.updated_at
        ]
      end
    end

    Rails.event.notify("export.users_completed", component: "Export", count: User.count, level: "info")
  end

  def create_zip_file
    super
    Rails.event.notify("export.zip_created", component: "Export", zip_path: @zip_path, level: "info")
  end

  # 清理旧的导出和导入文件
  # @param days [Integer] 保留最近多少天的文件，默认7天
  # @return [Hash] 返回清理统计信息
  def self.cleanup_old_exports(days: 7)
    exports_dir = Rails.root.join("tmp", "exports")
    uploads_dir = Rails.root.join("tmp", "uploads")

    deleted_count = 0
    error_count = 0
    cutoff_time = Time.current - days.days

    begin
      # 清理导出zip文件
      if Dir.exist?(exports_dir)
        zip_files = Dir.glob(File.join(exports_dir, "{export_,markdown_export_}*.zip"))
        zip_files.each do |zip_file|
          begin
            file_mtime = File.mtime(zip_file)
            if file_mtime < cutoff_time
              File.delete(zip_file)
              deleted_count += 1
              Rails.event.notify("export.old_file_deleted", component: "Export", file_type: "export", filename: File.basename(zip_file), age_days: (Time.current - file_mtime).to_i / 86400, level: "info")
            end
          rescue => e
            error_count += 1
            Rails.event.notify("export.file_deletion_failed", component: "Export", file_type: "export", zip_file: zip_file, error: e.message, level: "error")
          end
        end
      end

      # 清理导入zip文件
      if Dir.exist?(uploads_dir)
        import_files = Dir.glob(File.join(uploads_dir, "import_*.zip"))
        import_files.each do |import_file|
          begin
            file_mtime = File.mtime(import_file)
            if file_mtime < cutoff_time
              File.delete(import_file)
              deleted_count += 1
              Rails.event.notify("export.old_file_deleted", component: "Export", file_type: "import", filename: File.basename(import_file), age_days: (Time.current - file_mtime).to_i / 86400, level: "info")
            end
          rescue => e
            error_count += 1
            Rails.event.notify("export.file_deletion_failed", component: "Export", file_type: "import", import_file: import_file, error: e.message, level: "error")
          end
        end
      end

      message = "Cleaned up #{deleted_count} old export/import file(s) older than #{days} days"
      Rails.event.notify("export.cleanup_completed", component: "Export", deleted_count: deleted_count, error_count: error_count, days: days, level: "info")

      {
        deleted: deleted_count,
        errors: error_count,
        message: message
      }
    rescue => e
      error_message = "Error during export/import cleanup: #{e.message}"
      Rails.event.notify("export.cleanup_failed", component: "Export", error: e.message, backtrace: e.backtrace.join("\n"), level: "error")

      {
        deleted: deleted_count,
        errors: error_count + 1,
        message: error_message
      }
    end
  end
end
