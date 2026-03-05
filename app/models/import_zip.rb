class ImportZip
  require "csv"
  require "fileutils"
  require "zip"
  require "nokogiri"
  require "open-uri"
  require "securerandom"
  require "stringio"

  attr_reader :error_message, :import_dir, :zip_path

  def initialize(zip_path)
    @zip_path = zip_path
    @error_message = nil
    @import_dir = Rails.root.join(
      "tmp",
      "imports",
      "import_#{Time.current.strftime('%Y%m%d_%H%M%S')}_#{SecureRandom.hex(6)}"
    )
    FileUtils.mkdir_p(@import_dir)
  end

  def import_data
    ActivityLog.log!(
      action: :started,
      target: :import,
      level: :info,
      source: "zip",
      file: @zip_path
    )
    extract_zip_file
    import_tags
    import_articles
    import_article_tags
    import_crossposts
    import_listmonks
    import_git_integrations
    import_pages
    import_settings
    import_social_media_posts
    import_comments
    import_static_files
    import_redirects
    import_newsletter_settings
    import_subscribers
    import_subscriber_tags

    ActivityLog.log!(
      action: :completed,
      target: :import,
      level: :info,
      source: "zip",
      file: @zip_path
    )
    true
  rescue StandardError => e
    @error_message = e.message
    ActivityLog.log!(
      action: :failed,
      target: :import,
      level: :error,
      source: "zip",
      file: @zip_path,
      error: e.message
    )
    false
  ensure
    FileUtils.rm_rf(@import_dir) if @import_dir && File.directory?(@import_dir)
  end

  private

  def extract_zip_file
    Rails.event.notify("import_zip.extraction_started", component: "ImportZip", zip_path: @zip_path, level: "info")
    Zip::File.open(@zip_path) do |zip_file|
      zip_file.each do |entry|
        next if entry.directory?
        extract_path = File.join(@import_dir.to_s, entry.name)
        FileUtils.mkdir_p(File.dirname(extract_path))
        begin
          File.open(extract_path, "wb") { |f| f.write(entry.get_input_stream.read) }
          Rails.event.notify("import_zip.file_extracted", component: "ImportZip", entry_name: entry.name, extract_path: extract_path, level: "info")
        rescue => e
          Rails.event.notify("import_zip.extraction_failed", component: "ImportZip", entry_name: entry.name, error: e.message, level: "error")
          raise
        end
      end
    end
    Rails.event.notify("import_zip.extraction_completed", component: "ImportZip", import_dir: @import_dir, level: "info")
  end

  # Find the directory containing CSV files
  # This handles cases where ZIP files have a subdirectory wrapper
  def find_csv_base_dir
    # First check if CSV files are in the root import directory
    return @import_dir if Dir.glob(File.join(@import_dir, "*.csv")).any?

    # Otherwise, search for the first subdirectory containing CSV files
    Dir.glob(File.join(@import_dir, "*", "*.csv")).first&.then { |path| File.dirname(path) } || @import_dir
  end

  def import_tags
    base_dir = find_csv_base_dir
    csv_path = File.join(base_dir, "tags.csv")
    return unless File.exist?(csv_path)

    Rails.event.notify("import_zip.tags_import_started", component: "ImportZip", csv_path: csv_path, level: "info")
    imported_count = 0
    skipped_count = 0
    CSV.foreach(csv_path, headers: true) do |row|
      if Tag.exists?(slug: row["slug"])
        Rails.event.notify("import_zip.tag_skipped", component: "ImportZip", slug: row["slug"], reason: "already_exists", level: "info")
        skipped_count += 1
        next
      end
      Tag.create!(
        name: row["name"],
        slug: row["slug"],
        created_at: row["created_at"],
        updated_at: row["updated_at"]
      )
      imported_count += 1
    end
    Rails.event.notify("import_zip.tags_import_completed", component: "ImportZip", imported_count: imported_count, skipped_count: skipped_count, level: "info")
  end

  def import_articles
    base_dir = find_csv_base_dir
    csv_path = File.join(base_dir, "articles.csv")
    return unless File.exist?(csv_path)

    Rails.event.notify("import_zip.articles_import_started", component: "ImportZip", csv_path: csv_path, level: "info")
    imported_count = 0
    skipped_count = 0
    CSV.foreach(csv_path, headers: true) do |row|
      if Article.exists?(slug: row["slug"])
        Rails.event.notify("import_zip.article_skipped", component: "ImportZip", slug: row["slug"], reason: "already_exists", level: "info")
        skipped_count += 1
        next
      end
      article_id = row["id"].presence || "article_#{imported_count + skipped_count}"

      raw_content = row["content"].to_s
      if raw_content.blank?
        Rails.event.notify("import_zip.article_skipped", component: "ImportZip", slug: row["slug"], reason: "content_blank", level: "warn")
        skipped_count += 1
        next
      end

      processed_content = process_imported_content(raw_content, article_id, "article")
      processed_content = fix_content_sgid_references(processed_content)

      # 获取 content_type，默认为 rich_text
      content_type = row["content_type"].presence || "rich_text"

      base_attributes = {
        title: row["title"],
        slug: row["slug"],
        description: row["description"],
        source_url: row["source_url"],
        source_author: row["source_author"],
        source_content: row["source_content"],
        meta_title: row["meta_title"],
        meta_description: row["meta_description"],
        meta_image: row["meta_image"],
        status: row["status"],
        scheduled_at: row["scheduled_at"],
        comment: cast_boolean(row["comment"], default: false),
        content_type: content_type,
        created_at: row["created_at"],
        updated_at: row["updated_at"]
      }

      begin
        # 如果是 html 类型，使用 html_content
        if content_type == "html"
          html_content = row["html_content"].presence || processed_content
          Article.create!(**base_attributes, html_content: html_content)
        else
          Article.create!(**base_attributes, content: processed_content)
        end
      rescue ActiveRecord::RecordInvalid => e
        if e.record.is_a?(Article) && e.record.errors.added?(:content, "can't be blank") && processed_content.present?
          Rails.event.notify("import_zip.article_fallback_to_html", component: "ImportZip", slug: row["slug"], reason: "rich_text_content_blank", level: "warn")
          Article.create!(**base_attributes, content_type: "html", html_content: processed_content)
        else
          raise
        end
      end
      imported_count += 1
    end
    Rails.event.notify("import_zip.articles_import_completed", component: "ImportZip", imported_count: imported_count, skipped_count: skipped_count, level: "info")
  end

  def import_article_tags
    base_dir = find_csv_base_dir
    csv_path = File.join(base_dir, "article_tags.csv")
    return unless File.exist?(csv_path)

    Rails.event.notify("import_zip.article_tags_import_started", component: "ImportZip", csv_path: csv_path, level: "info")
    imported_count = 0
    skipped_count = 0
    CSV.foreach(csv_path, headers: true) do |row|
      # 使用 article_slug 查找 article
      article_slug = row["article_slug"]
      unless article_slug.present?
        Rails.event.notify("import_zip.article_tag_skipped", component: "ImportZip", reason: "article_slug_missing", level: "warn")
        skipped_count += 1
        next
      end

      article = Article.find_by(slug: article_slug)
      unless article
        Rails.event.notify("import_zip.article_tag_skipped", component: "ImportZip", article_slug: article_slug, reason: "article_not_found", level: "info")
        skipped_count += 1
        next
      end

      # 使用 tag_slug 查找 tag
      tag_slug = row["tag_slug"]
      unless tag_slug.present?
        Rails.event.notify("import_zip.article_tag_skipped", component: "ImportZip", reason: "tag_slug_missing", level: "warn")
        skipped_count += 1
        next
      end

      tag = Tag.find_by(slug: tag_slug)
      unless tag
        Rails.event.notify("import_zip.article_tag_skipped", component: "ImportZip", tag_slug: tag_slug, reason: "tag_not_found", level: "info")
        skipped_count += 1
        next
      end

      # 检查是否已存在相同的关联
      if ArticleTag.exists?(article_id: article.id, tag_id: tag.id)
        Rails.event.notify("import_zip.article_tag_skipped", component: "ImportZip", article_id: article.id, tag_id: tag.id, reason: "already_exists", level: "info")
        skipped_count += 1
        next
      end

      ArticleTag.create!(
        article_id: article.id,
        tag_id: tag.id,
        created_at: row["created_at"],
        updated_at: row["updated_at"]
      )
      imported_count += 1
    end
    Rails.event.notify("import_zip.article_tags_import_completed", component: "ImportZip", imported_count: imported_count, skipped_count: skipped_count, level: "info")
  end

  def import_crossposts
    base_dir = find_csv_base_dir
    csv_path = File.join(base_dir, "crossposts.csv")
    return unless File.exist?(csv_path)

    Rails.event.notify("import_zip.crossposts_import_started", component: "ImportZip", csv_path: csv_path, level: "info")
    imported_count = 0
    updated_count = 0
    skipped_count = 0
    CSV.foreach(csv_path, headers: true) do |row|
      platform = row["platform"].to_s.strip.downcase
      unless platform.present?
        Rails.event.notify("import_zip.crosspost_skipped", component: "ImportZip", reason: "platform_missing", level: "warn")
        skipped_count += 1
        next
      end

      unless Crosspost::PLATFORMS.include?(platform)
        Rails.event.notify("import_zip.crosspost_skipped", component: "ImportZip", platform: platform, reason: "unsupported_platform", level: "warn")
        skipped_count += 1
        next
      end

      crosspost = Crosspost.find_or_initialize_by(platform: platform)
      is_new_record = crosspost.new_record?

      crosspost.assign_attributes(
        server_url: row["server_url"],
        client_key: row["client_key"],
        client_secret: row["client_secret"],
        client_id: row["client_id"],
        access_token: row["access_token"],
        refresh_token: row["refresh_token"],
        username: row["username"],
        app_password: row["app_password"],
        enabled: cast_boolean(row["enabled"], default: false),
        auto_fetch_comments: cast_boolean(row["auto_fetch_comments"], default: false),
        comment_fetch_schedule: row["comment_fetch_schedule"],
        max_characters: row["max_characters"],
        settings: parse_json_field(row["settings"]) || {},
        created_at: row["created_at"],
        updated_at: row["updated_at"]
      )

      begin
        crosspost.save!
      rescue ActiveRecord::RecordInvalid
        if crosspost.enabled?
          Rails.event.notify(
            "import_zip.crosspost_disabled_due_to_missing_credentials",
            component: "ImportZip",
            platform: platform,
            errors: crosspost.errors.full_messages.join(", "),
            level: "warn"
          )
          crosspost.enabled = false
          crosspost.save!
        else
          raise
        end
      end

      if is_new_record
        imported_count += 1
      else
        updated_count += 1
      end
    end
    Rails.event.notify(
      "import_zip.crossposts_import_completed",
      component: "ImportZip",
      imported_count: imported_count,
      updated_count: updated_count,
      skipped_count: skipped_count,
      level: "info"
    )
  end

  def import_git_integrations
    base_dir = find_csv_base_dir
    csv_path = File.join(base_dir, "git_integrations.csv")
    return unless File.exist?(csv_path)

    Rails.event.notify("import_zip.git_integrations_import_started", component: "ImportZip", csv_path: csv_path, level: "info")
    imported_count = 0
    updated_count = 0
    skipped_count = 0

    CSV.foreach(csv_path, headers: true) do |row|
      provider = row["provider"].to_s.strip.downcase
      unless provider.present?
        Rails.event.notify("import_zip.git_integration_skipped", component: "ImportZip", reason: "provider_missing", level: "warn")
        skipped_count += 1
        next
      end

      unless GitIntegration::PROVIDERS.include?(provider)
        Rails.event.notify("import_zip.git_integration_skipped", component: "ImportZip", provider: provider, reason: "unsupported_provider", level: "warn")
        skipped_count += 1
        next
      end

      git_integration = GitIntegration.find_or_initialize_by(provider: provider)
      is_new_record = git_integration.new_record?

      git_integration.assign_attributes(
        name: row["name"],
        server_url: row["server_url"],
        username: row["username"],
        access_token: row["access_token"],
        enabled: cast_boolean(row["enabled"], default: false),
        created_at: row["created_at"],
        updated_at: row["updated_at"]
      )

      begin
        git_integration.save!
      rescue ActiveRecord::RecordInvalid
        if git_integration.enabled? && git_integration.errors.added?(:access_token, "can't be blank")
          Rails.event.notify(
            "import_zip.git_integration_disabled_due_to_missing_token",
            component: "ImportZip",
            provider: provider,
            errors: git_integration.errors.full_messages.join(", "),
            level: "warn"
          )
          git_integration.enabled = false
          git_integration.save!
        else
          raise
        end
      end

      if is_new_record
        imported_count += 1
      else
        updated_count += 1
      end
    end

    Rails.event.notify(
      "import_zip.git_integrations_import_completed",
      component: "ImportZip",
      imported_count: imported_count,
      updated_count: updated_count,
      skipped_count: skipped_count,
      level: "info"
    )
  end

  def import_listmonks
    base_dir = find_csv_base_dir
    csv_path = File.join(base_dir, "listmonks.csv")
    return unless File.exist?(csv_path)
    Rails.event.notify("import_zip.listmonks_import_started", component: "ImportZip", csv_path: csv_path, level: "info")
    imported_count = 0
    CSV.foreach(csv_path, headers: true) do |row|
      Listmonk.find_or_create_by(
        url: row["url"],
        username: row["username"],
        api_key: row["api_key"],
        list_id: row["list_id"],
        template_id: row["template_id"],
        enabled: row["enabled"],
        created_at: row["created_at"],
        updated_at: row["updated_at"]
      )
      imported_count += 1
    end
    Rails.event.notify("import_zip.listmonks_import_completed", component: "ImportZip", imported_count: imported_count, level: "info")
  end

  def import_pages
    base_dir = find_csv_base_dir
    csv_path = File.join(base_dir, "pages.csv")
    return unless File.exist?(csv_path)
    Rails.event.notify("import_zip.pages_import_started", component: "ImportZip", csv_path: csv_path, level: "info")
    imported_count = 0
    skipped_count = 0
    CSV.foreach(csv_path, headers: true) do |row|
      if Page.exists?(slug: row["slug"])
        Rails.event.notify("import_zip.page_skipped", component: "ImportZip", slug: row["slug"], reason: "already_exists", level: "info")
        skipped_count += 1
        next
      end
      page_id = row["id"].presence || "page_#{imported_count + skipped_count}"

      raw_content = row["content"].to_s
      if raw_content.blank?
        Rails.event.notify("import_zip.page_skipped", component: "ImportZip", slug: row["slug"], reason: "content_blank", level: "warn")
        skipped_count += 1
        next
      end

      processed_content = process_imported_content(raw_content, page_id, "page")
      processed_content = fix_content_sgid_references(processed_content)

      # 获取 content_type，默认为 rich_text
      content_type = row["content_type"].presence || "rich_text"

      base_attributes = {
        title: row["title"],
        slug: row["slug"],
        status: row["status"],
        redirect_url: row["redirect_url"],
        page_order: row["page_order"],
        comment: cast_boolean(row["comment"], default: false),
        content_type: content_type,
        created_at: row["created_at"],
        updated_at: row["updated_at"]
      }

      begin
        # 如果是 html 类型，使用 html_content
        if content_type == "html"
          html_content = row["html_content"].presence || processed_content
          Page.create!(**base_attributes, html_content: html_content)
        else
          Page.create!(**base_attributes, content: processed_content)
        end
      rescue ActiveRecord::RecordInvalid => e
        if e.record.is_a?(Page) && e.record.errors.added?(:content, "can't be blank") && processed_content.present?
          Rails.event.notify("import_zip.page_fallback_to_html", component: "ImportZip", slug: row["slug"], reason: "rich_text_content_blank", level: "warn")
          Page.create!(**base_attributes, content_type: "html", html_content: processed_content)
        else
          raise
        end
      end
      imported_count += 1
    end
    Rails.event.notify("import_zip.pages_import_completed", component: "ImportZip", imported_count: imported_count, skipped_count: skipped_count, level: "info")
  end

  def import_settings
    base_dir = find_csv_base_dir
    csv_path = File.join(base_dir, "settings.csv")
    return unless File.exist?(csv_path)
    Rails.event.notify("import_zip.settings_import_started", component: "ImportZip", csv_path: csv_path, level: "info")
    imported_count = 0
    # 只允许有一个 Setting
    existing_setting = Setting.first
    if existing_setting
      csv_data = CSV.read(csv_path, headers: true).first
      return unless csv_data
      social_links = parse_json_field(csv_data["social_links"])
      static_files = parse_json_field(csv_data["static_files"])
      existing_setting.update!(
        title: csv_data["title"],
        description: csv_data["description"],
        author: csv_data["author"],
        url: csv_data["url"],
        time_zone: csv_data["time_zone"] || "UTC",
        giscus: csv_data["giscus"],
        tool_code: csv_data["tool_code"],
        head_code: csv_data["head_code"],
        custom_css: csv_data["custom_css"],
        social_links: social_links,
        static_files: static_files || {},
        auto_regenerate_triggers: parse_json_field(csv_data["auto_regenerate_triggers"]) || [],
        deploy_branch: csv_data["deploy_branch"],
        deploy_provider: csv_data["deploy_provider"],
        deploy_repo_url: csv_data["deploy_repo_url"],
        local_generation_path: csv_data["local_generation_path"],
        static_generation_destination: csv_data["static_generation_destination"],
        static_generation_delay: csv_data["static_generation_delay"],
        setup_completed: cast_boolean(csv_data["setup_completed"], default: false),
        github_backup_enabled: cast_boolean(csv_data["github_backup_enabled"], default: false),
        github_repo_url: csv_data["github_repo_url"],
        github_token: csv_data["github_token"],
        github_backup_branch: csv_data["github_backup_branch"],
        created_at: csv_data["created_at"],
        updated_at: csv_data["updated_at"]
      )
      imported_count += 1
    else
      CSV.foreach(csv_path, headers: true) do |row|
        social_links = parse_json_field(row["social_links"])
        static_files = parse_json_field(row["static_files"])
        Setting.create!(
          title: row["title"],
          description: row["description"],
          author: row["author"],
          url: row["url"],
          time_zone: row["time_zone"] || "UTC",
          giscus: row["giscus"],
          tool_code: row["tool_code"],
          head_code: row["head_code"],
          custom_css: row["custom_css"],
          social_links: social_links,
          static_files: static_files || {},
          auto_regenerate_triggers: parse_json_field(row["auto_regenerate_triggers"]) || [],
          deploy_branch: row["deploy_branch"],
          deploy_provider: row["deploy_provider"],
          deploy_repo_url: row["deploy_repo_url"],
          local_generation_path: row["local_generation_path"],
          static_generation_destination: row["static_generation_destination"],
          static_generation_delay: row["static_generation_delay"],
          setup_completed: cast_boolean(row["setup_completed"], default: false),
          github_backup_enabled: cast_boolean(row["github_backup_enabled"], default: false),
          github_repo_url: row["github_repo_url"],
          github_token: row["github_token"],
          github_backup_branch: row["github_backup_branch"],
          created_at: row["created_at"],
          updated_at: row["updated_at"]
        )
        imported_count += 1
      end
    end
    # 富文本 footer
    footer_csv_path = File.join(base_dir, "setting_footers.csv")
    if File.exist?(footer_csv_path)
      Rails.event.notify("import_zip.setting_footer_import_started", component: "ImportZip", footer_csv_path: footer_csv_path, level: "info")
      CSV.foreach(footer_csv_path, headers: true) do |row|
        setting = Setting.first
        content = row["content"].presence || ""
        if setting && content.present?
          processed_content = process_setting_footer_content(content, setting.id, "setting")
          processed_content = fix_content_sgid_references(processed_content)
          setting.update!(footer: processed_content)
          Rails.event.notify("import_zip.setting_footer_updated", component: "ImportZip", setting_id: setting.id, level: "info")
        end
      end
    end
    Rails.event.notify("import_zip.settings_import_completed", component: "ImportZip", imported_count: imported_count, level: "info")
  rescue StandardError => e
    Rails.event.notify("import_zip.settings_import_failed", component: "ImportZip", error: e.message, level: "error")
    raise
  end

  def import_social_media_posts
    base_dir = find_csv_base_dir
    csv_path = File.join(base_dir, "social_media_posts.csv")
    return unless File.exist?(csv_path)

    Rails.event.notify("import_zip.social_media_posts_import_started", component: "ImportZip", csv_path: csv_path, level: "info")
    imported_count = 0
    skipped_count = 0
    CSV.foreach(csv_path, headers: true) do |row|
      # 使用 article_slug 查找 article
      article_slug = row["article_slug"]
      unless article_slug.present?
        Rails.event.notify("import_zip.social_media_post_skipped", component: "ImportZip", reason: "article_slug_missing", level: "warn")
        skipped_count += 1
        next
      end

      article = Article.find_by(slug: article_slug)
      unless article
        Rails.event.notify("import_zip.social_media_post_skipped", component: "ImportZip", article_slug: article_slug, reason: "article_not_found", level: "info")
        skipped_count += 1
        next
      end

      # 检查是否已存在相同的记录（根据 article_id 和 platform）
      if SocialMediaPost.exists?(article_id: article.id, platform: row["platform"])
        Rails.event.notify("import_zip.social_media_post_skipped", component: "ImportZip", article_id: article.id, platform: row["platform"], reason: "already_exists", level: "info")
        skipped_count += 1
        next
      end

      SocialMediaPost.create!(
        article_id: article.id,
        platform: row["platform"],
        url: row["url"],
        created_at: row["created_at"],
        updated_at: row["updated_at"]
      )
      imported_count += 1
    end
    Rails.event.notify("import_zip.social_media_posts_import_completed", component: "ImportZip", imported_count: imported_count, skipped_count: skipped_count, level: "info")
  end

  def import_comments
    base_dir = find_csv_base_dir
    csv_path = File.join(base_dir, "comments.csv")
    return unless File.exist?(csv_path)

    Rails.event.notify("import_zip.comments_import_started", component: "ImportZip", csv_path: csv_path, level: "info")
    imported_count = 0
    skipped_count = 0

    # 使用 ID 映射来跟踪导入的评论（原始ID -> 新ID）
    comment_id_map = {}

    # 第一遍：导入所有评论（先不设置 parent_id）
    CSV.foreach(csv_path, headers: true) do |row|
      # 使用 article_slug 查找 article
      article_slug = row["article_slug"]
      unless article_slug.present?
        Rails.event.notify("import_zip.comment_skipped", component: "ImportZip", reason: "article_slug_missing", level: "warn")
        skipped_count += 1
        next
      end

      article = Article.find_by(slug: article_slug)
      unless article
        Rails.event.notify("import_zip.comment_skipped", component: "ImportZip", article_slug: article_slug, reason: "article_not_found", level: "info")
        skipped_count += 1
        next
      end

      if row["author_name"].blank?
        Rails.event.notify("import_zip.comment_skipped", component: "ImportZip", article_id: article.id, reason: "author_name_missing", level: "warn")
        skipped_count += 1
        next
      end

      if row["content"].blank?
        Rails.event.notify("import_zip.comment_skipped", component: "ImportZip", article_id: article.id, reason: "content_blank", level: "warn")
        skipped_count += 1
        next
      end

      # 检查是否已存在相同的记录
      existing_comment = nil

      # 对于外部评论，使用 article_id, platform, external_id 作为唯一标识
      if row["platform"].present? && row["external_id"].present?
        existing_comment = Comment.find_by(article_id: article.id, platform: row["platform"], external_id: row["external_id"])
        if existing_comment
          Rails.event.notify("import_zip.comment_skipped", component: "ImportZip", article_id: article.id, platform: row["platform"], external_id: row["external_id"], reason: "already_exists", level: "info")
          skipped_count += 1
          # 仍然记录到映射中，以便后续处理 parent_id
          comment_id_map[row["id"].to_i] = existing_comment.id if row["id"].present?
          next
        end
      else
        # 对于本地评论，使用 article_id + author_name + content + published_at/created_at 作为唯一标识
        # 由于时间戳可能有微小差异，我们使用时间窗口（±5秒）来匹配
        published_at = row["published_at"].present? ? Time.parse(row["published_at"]) : nil
        created_at = row["created_at"].present? ? Time.parse(row["created_at"]) : nil

        # 构建查询条件
        query = Comment.where(
          article_id: article.id,
          platform: nil,
          author_name: row["author_name"],
          content: row["content"]
        )

        # 使用时间窗口匹配（±5秒）
        if published_at
          time_window = 5.seconds
          existing_comment = query.where(
            "published_at BETWEEN ? AND ?",
            published_at - time_window,
            published_at + time_window
          ).first
        elsif created_at
          time_window = 5.seconds
          existing_comment = query.where(
            "created_at BETWEEN ? AND ?",
            created_at - time_window,
            created_at + time_window
          ).first
        end

        if existing_comment
          Rails.event.notify("import_zip.comment_skipped", component: "ImportZip", article_id: article.id, author_name: row["author_name"], reason: "already_exists", level: "info")
          skipped_count += 1
          # 仍然记录到映射中，以便后续处理 parent_id
          comment_id_map[row["id"].to_i] = existing_comment.id if row["id"].present?
          next
        end
      end

      comment = Comment.create!(
        article_id: article.id,
        commentable_type: "Article",
        commentable_id: article.id,
        parent_id: nil, # 先不设置 parent_id
        author_name: row["author_name"],
        author_url: row["author_url"],
        author_username: row["author_username"],
        author_avatar_url: row["author_avatar_url"],
        content: row["content"],
        platform: row["platform"],
        external_id: row["external_id"],
        status: row["status"] || "pending",
        published_at: row["published_at"],
        url: row["url"],
        created_at: row["created_at"],
        updated_at: row["updated_at"]
      )

      # 记录 ID 映射
      comment_id_map[row["id"].to_i] = comment.id if row["id"].present?
      imported_count += 1
    end

    # 第二遍：更新 parent_id
    CSV.foreach(csv_path, headers: true) do |row|
      next unless row["parent_id"].present?

      original_id = row["id"].to_i
      original_parent_id = row["parent_id"].to_i

      # 查找新导入的评论ID
      new_comment_id = comment_id_map[original_id]
      new_parent_id = comment_id_map[original_parent_id]

      if new_comment_id && new_parent_id
        comment = Comment.find_by(id: new_comment_id)
        if comment && comment.parent_id.nil?
          comment.update(parent_id: new_parent_id)
          Rails.event.notify("import_zip.comment_parent_updated", component: "ImportZip", comment_id: new_comment_id, parent_id: new_parent_id, level: "info")
        end
      end
    end

    Rails.event.notify("import_zip.comments_import_completed", component: "ImportZip", imported_count: imported_count, skipped_count: skipped_count, level: "info")
  end

  def import_static_files
    base_dir = find_csv_base_dir
    csv_path = File.join(base_dir, "static_files.csv")
    return unless File.exist?(csv_path)

    Rails.event.notify("import_zip.static_files_import_started", component: "ImportZip", csv_path: csv_path, level: "info")
    imported_count = 0
    skipped_count = 0
    CSV.foreach(csv_path, headers: true) do |row|
      if StaticFile.exists?(filename: row["filename"])
        Rails.event.notify("import_zip.static_file_skipped", component: "ImportZip", filename: row["filename"], reason: "already_exists", level: "info")
        skipped_count += 1
        next
      end

      # 必须要有 blob_filename 才能导入
      unless row["blob_filename"].present?
        Rails.event.notify("import_zip.static_file_skipped", component: "ImportZip", row_id: row["id"], reason: "blob_filename_missing", level: "warn")
        skipped_count += 1
        next
      end

      # 先检查文件是否存在，避免创建没有文件的记录
      static_files_dir = File.join(base_dir, "attachments", "static_files")
      file_path = File.join(static_files_dir, "#{row['id']}_#{row['blob_filename']}")

      unless File.exist?(file_path) && safe_file_path?(file_path)
        Rails.event.notify("import_zip.static_file_skipped", component: "ImportZip", file_path: file_path, row_id: row["id"], blob_filename: row["blob_filename"], reason: "file_not_found", level: "warn")
        skipped_count += 1
        next
      end

      # 创建记录并同时附加文件，避免验证错误
      static_file = StaticFile.new(
        filename: row["filename"],
        description: row["description"],
        created_at: row["created_at"],
        updated_at: row["updated_at"]
      )

      # 读取文件内容到内存，避免文件流关闭问题
      file_content = File.binread(file_path)
      static_file.file.attach(
        io: StringIO.new(file_content),
        filename: row["filename"],
        content_type: detect_content_type(file_path)
      )

      static_file.save!
      Rails.event.notify("import_zip.static_file_imported", component: "ImportZip", filename: row["filename"], blob_filename: row["blob_filename"], level: "info")
      imported_count += 1
    end
    Rails.event.notify("import_zip.static_files_import_completed", component: "ImportZip", imported_count: imported_count, skipped_count: skipped_count, level: "info")
  end

  def import_redirects
    base_dir = find_csv_base_dir
    csv_path = File.join(base_dir, "redirects.csv")
    return unless File.exist?(csv_path)

    Rails.event.notify("import_zip.redirects_import_started", component: "ImportZip", csv_path: csv_path, level: "info")
    imported_count = 0
    skipped_count = 0
    CSV.foreach(csv_path, headers: true) do |row|
      # 检查是否已存在相同的 regex
      if Redirect.exists?(regex: row["regex"])
        Rails.event.notify("import_zip.redirect_skipped", component: "ImportZip", regex: row["regex"], reason: "already_exists", level: "info")
        skipped_count += 1
        next
      end

      Redirect.create!(
        regex: row["regex"],
        replacement: row["replacement"],
        enabled: cast_boolean(row["enabled"], default: false),
        permanent: cast_boolean(row["permanent"], default: false),
        created_at: row["created_at"],
        updated_at: row["updated_at"]
      )
      imported_count += 1
    end
    Rails.event.notify("import_zip.redirects_import_completed", component: "ImportZip", imported_count: imported_count, skipped_count: skipped_count, level: "info")
  end

  def import_newsletter_settings
    base_dir = find_csv_base_dir
    csv_path = File.join(base_dir, "newsletter_settings.csv")
    return unless File.exist?(csv_path)

    Rails.event.notify("import_zip.newsletter_settings_import_started", component: "ImportZip", csv_path: csv_path, level: "info")
    imported_count = 0
    # 只允许有一个 NewsletterSetting
    existing_setting = NewsletterSetting.first
    if existing_setting
      csv_data = CSV.read(csv_path, headers: true).first
      return unless csv_data

      # 处理 footer 内容
      footer_content = csv_data["footer"].presence || ""
      processed_footer = ""
      if footer_content.present?
        processed_footer = process_newsletter_setting_footer_content(footer_content, existing_setting.id, "newsletter_setting")
        processed_footer = fix_content_sgid_references(processed_footer)
      end

      existing_setting.update!(
        provider: csv_data["provider"] || "native",
        enabled: csv_data["enabled"] != "false",
        smtp_address: csv_data["smtp_address"],
        smtp_port: csv_data["smtp_port"],
        smtp_user_name: csv_data["smtp_user_name"],
        smtp_password: csv_data["smtp_password"],
        smtp_domain: csv_data["smtp_domain"],
        smtp_authentication: csv_data["smtp_authentication"] || "plain",
        smtp_enable_starttls: csv_data["smtp_enable_starttls"] != "false",
        from_email: csv_data["from_email"],
        footer: processed_footer.present? ? processed_footer : nil,
        created_at: csv_data["created_at"],
        updated_at: csv_data["updated_at"]
      )
      imported_count += 1
    else
      CSV.foreach(csv_path, headers: true) do |row|
        # 处理 footer 内容
        footer_content = row["footer"].presence || ""
        processed_footer = ""
        if footer_content.present?
          # 对于新创建的记录，先创建再处理 footer
          newsletter_setting = NewsletterSetting.create!(
            provider: row["provider"] || "native",
            enabled: row["enabled"] != "false",
            smtp_address: row["smtp_address"],
            smtp_port: row["smtp_port"],
            smtp_user_name: row["smtp_user_name"],
            smtp_password: row["smtp_password"],
            smtp_domain: row["smtp_domain"],
            smtp_authentication: row["smtp_authentication"] || "plain",
            smtp_enable_starttls: row["smtp_enable_starttls"] != "false",
            from_email: row["from_email"],
            created_at: row["created_at"],
            updated_at: row["updated_at"]
          )

          if footer_content.present?
            processed_footer = process_newsletter_setting_footer_content(footer_content, newsletter_setting.id, "newsletter_setting")
            processed_footer = fix_content_sgid_references(processed_footer)
            newsletter_setting.update!(footer: processed_footer)
          end
        else
          NewsletterSetting.create!(
            provider: row["provider"] || "native",
            enabled: row["enabled"] != "false",
            smtp_address: row["smtp_address"],
            smtp_port: row["smtp_port"],
            smtp_user_name: row["smtp_user_name"],
            smtp_password: row["smtp_password"],
            smtp_domain: row["smtp_domain"],
            smtp_authentication: row["smtp_authentication"] || "plain",
            smtp_enable_starttls: row["smtp_enable_starttls"] != "false",
            from_email: row["from_email"],
            created_at: row["created_at"],
            updated_at: row["updated_at"]
          )
        end
        imported_count += 1
        break # 只允许有一个 NewsletterSetting，处理完第一行后退出循环
      end
    end
    Rails.event.notify("import_zip.newsletter_settings_import_completed", component: "ImportZip", imported_count: imported_count, level: "info")
  end

  def process_newsletter_setting_footer_content(content, record_id = nil, record_type = nil)
    return content if content.blank?
    record_id ||= "newsletter_setting"
    record_type ||= "newsletter_setting"
    Rails.event.notify("import_zip.newsletter_footer_processing", component: "ImportZip", record_id: record_id, level: "info")
    doc = Nokogiri::HTML.fragment(content)
    doc.css("action-text-attachment").each { |a| safe_process { process_imported_attachment_element(a, record_id, record_type) } }
    doc.css("figure[data-trix-attachment]").each { |f| safe_process { process_imported_figure_element(f, record_id, record_type) } }
    doc.css("img").each { |img| safe_process { process_imported_image_element(img, record_id, record_type) } }
    doc.to_html
  end

  def import_subscribers
    base_dir = find_csv_base_dir
    csv_path = File.join(base_dir, "subscribers.csv")
    return unless File.exist?(csv_path)

    Rails.event.notify("import_zip.subscribers_import_started", component: "ImportZip", csv_path: csv_path, level: "info")
    imported_count = 0
    skipped_count = 0
    CSV.foreach(csv_path, headers: true) do |row|
      if Subscriber.exists?(email: row["email"])
        Rails.event.notify("import_zip.subscriber_skipped", component: "ImportZip", email: row["email"], reason: "already_exists", level: "info")
        skipped_count += 1
        next
      end

      Subscriber.create!(
        email: row["email"],
        confirmation_token: row["confirmation_token"],
        confirmed_at: row["confirmed_at"],
        unsubscribe_token: row["unsubscribe_token"],
        unsubscribed_at: row["unsubscribed_at"],
        created_at: row["created_at"],
        updated_at: row["updated_at"]
      )
      imported_count += 1
    end
    Rails.event.notify("import_zip.subscribers_import_completed", component: "ImportZip", imported_count: imported_count, skipped_count: skipped_count, level: "info")
  end

  def import_subscriber_tags
    base_dir = find_csv_base_dir
    csv_path = File.join(base_dir, "subscriber_tags.csv")
    return unless File.exist?(csv_path)

    Rails.event.notify("import_zip.subscriber_tags_import_started", component: "ImportZip", csv_path: csv_path, level: "info")
    imported_count = 0
    skipped_count = 0
    CSV.foreach(csv_path, headers: true) do |row|
      # 使用 subscriber_email 查找 subscriber
      subscriber_email = row["subscriber_email"]
      unless subscriber_email.present?
        Rails.event.notify("import_zip.subscriber_tag_skipped", component: "ImportZip", reason: "subscriber_email_missing", level: "warn")
        skipped_count += 1
        next
      end

      subscriber = Subscriber.find_by(email: subscriber_email)
      unless subscriber
        Rails.event.notify("import_zip.subscriber_tag_skipped", component: "ImportZip", subscriber_email: subscriber_email, reason: "subscriber_not_found", level: "info")
        skipped_count += 1
        next
      end

      # 使用 tag_slug 查找 tag
      tag_slug = row["tag_slug"]
      unless tag_slug.present?
        Rails.event.notify("import_zip.subscriber_tag_skipped", component: "ImportZip", reason: "tag_slug_missing", level: "warn")
        skipped_count += 1
        next
      end

      tag = Tag.find_by(slug: tag_slug)
      unless tag
        Rails.event.notify("import_zip.subscriber_tag_skipped", component: "ImportZip", tag_slug: tag_slug, reason: "tag_not_found", level: "info")
        skipped_count += 1
        next
      end

      # 检查是否已存在相同的关联
      if SubscriberTag.exists?(subscriber_id: subscriber.id, tag_id: tag.id)
        Rails.event.notify("import_zip.subscriber_tag_skipped", component: "ImportZip", subscriber_id: subscriber.id, tag_id: tag.id, reason: "already_exists", level: "info")
        skipped_count += 1
        next
      end

      SubscriberTag.create!(
        subscriber_id: subscriber.id,
        tag_id: tag.id,
        created_at: row["created_at"],
        updated_at: row["updated_at"]
      )
      imported_count += 1
    end
    Rails.event.notify("import_zip.subscriber_tags_import_completed", component: "ImportZip", imported_count: imported_count, skipped_count: skipped_count, level: "info")
  end

  # ----- 内容和附件处理通用工具方法 -----
  def process_imported_content(content, record_id = nil, record_type = nil)
    return content if content.blank?
    record_id ||= "unknown"
    record_type ||= "content"
    Rails.event.notify("import_zip.content_processing", component: "ImportZip", record_id: record_id, record_type: record_type, level: "info")
    doc = Nokogiri::HTML.fragment(content)

    doc.css("action-text-attachment").each { |a| safe_process { process_imported_attachment_element(a, record_id, record_type) } }
    doc.css("figure[data-trix-attachment]").each { |f| safe_process { process_imported_figure_element(f, record_id, record_type) } }
    doc.css("img").each { |img| safe_process { process_imported_image_element(img, record_id, record_type) } }

    doc.to_html
  end

  def process_imported_attachment_element(attachment, record_id = nil, record_type = nil)
    record_id ||= "unknown"
    record_type ||= "attachment"
    return unless attachment.respond_to?(:[])
    content_type = attachment["content-type"]
    original_url = attachment["url"]
    filename = attachment["filename"]
    return unless original_url.present? && filename.present?

    if is_local_attachment?(original_url)
      attachment_path = safe_join_path(@import_dir, original_url)
      if File.exist?(attachment_path) && safe_file_path?(attachment_path)
        File.open(attachment_path) do |file|
          content_type ||= "application/octet-stream"
          blob = ActiveStorage::Blob.create_and_upload!(
            io: file, filename: filename, content_type: content_type
          )
          update_attachment_element_with_blob(attachment, blob)
        end
      else
        Rails.event.notify("import_zip.attachment_not_found", component: "ImportZip", attachment_path: attachment_path, level: "warn")
      end
    elsif is_active_storage_url?(original_url)
      blob = extract_blob_from_url(original_url)
      update_attachment_element_with_blob(attachment, blob) if blob
    end
  end

  def process_imported_figure_element(figure, record_id = nil, record_type = nil)
    record_id ||= "unknown"
    record_type ||= "figure"
    return unless figure.respond_to?(:[])
    attachment_data = JSON.parse(figure["data-trix-attachment"]) rescue nil
    return unless attachment_data
    original_url = attachment_data["url"]
    filename = attachment_data["filename"] || File.basename(original_url) if original_url.present?
    content_type = attachment_data["contentType"]
    return unless original_url.present? && filename.present?

    if is_local_attachment?(original_url)
      attachment_path = safe_join_path(@import_dir, original_url)
      if File.exist?(attachment_path) && safe_file_path?(attachment_path)
        File.open(attachment_path) do |file|
          content_type ||= "application/octet-stream"
          blob = ActiveStorage::Blob.create_and_upload!(
            io: file, filename: filename, content_type: content_type
          )
          attachment_data["url"] = Rails.application.routes.url_helpers.rails_blob_path(blob, only_path: true)
          figure["sgid"] = blob.to_sgid.to_s
          figure["data-trix-attachment"] = attachment_data.to_json
          update_img_src_in_node(figure, attachment_data["url"])
        end
      else
        Rails.event.notify("import_zip.figure_attachment_not_found", component: "ImportZip", attachment_path: attachment_path, level: "warn")
      end
    elsif is_active_storage_url?(original_url)
      blob = extract_blob_from_url(original_url)
      if blob
        attachment_data["url"] = Rails.application.routes.url_helpers.rails_blob_path(blob, only_path: true)
        figure["sgid"] = blob.to_sgid.to_s
        figure["data-trix-attachment"] = attachment_data.to_json
        update_img_src_in_node(figure, attachment_data["url"])
      end
    end
  end

  def process_imported_image_element(img, record_id = nil, record_type = nil)
    record_id ||= "unknown"
    record_type ||= "image"
    return unless img.respond_to?(:[])
    original_url = img["src"]
    return unless original_url.present?

    if is_local_attachment?(original_url)
      attachment_path = safe_join_path(@import_dir, original_url)
      if File.exist?(attachment_path) && safe_file_path?(attachment_path)
        File.open(attachment_path) do |file|
          filename = File.basename(attachment_path)
          content_type = detect_content_type(attachment_path)
          blob = ActiveStorage::Blob.create_and_upload!(
            io: file, filename: filename, content_type: content_type
          )
          new_url = Rails.application.routes.url_helpers.rails_blob_path(blob, only_path: true)
          img["src"] = new_url
        end
      else
        Rails.event.notify("import_zip.image_not_found", component: "ImportZip", attachment_path: attachment_path, level: "warn")
      end
    elsif is_active_storage_url?(original_url)
      blob = extract_blob_from_url(original_url)
      if blob
        new_url = Rails.application.routes.url_helpers.rails_blob_path(blob, only_path: true)
        img["src"] = new_url
      end
    elsif original_url.start_with?("http")
      # 处理外部图片 URL (RemoteImage)
      download_and_process_remote_image(img, original_url, record_id, record_type)
    end
  end

  def download_and_process_remote_image(img, original_url, record_id, record_type)
    # 生成唯一文件名
    ext = extract_extension_from_url(original_url) || ".jpg"
    filename = "#{SecureRandom.hex(8)}#{ext}"

    # 保存到 ActiveStorage
    begin
      URI.open(original_url) do |remote_file|
        content_type = detect_content_type_from_url(original_url) || "image/jpeg"
        blob = ActiveStorage::Blob.create_and_upload!(
          io: remote_file, filename: filename, content_type: content_type
        )
        new_url = Rails.application.routes.url_helpers.rails_blob_path(blob, only_path: true)
        img["src"] = new_url
        Rails.event.notify("import_zip.remote_image_processed", component: "ImportZip", original_url: original_url, new_url: new_url, level: "info")
      end
    rescue => e
      Rails.event.notify("import_zip.remote_image_download_failed", component: "ImportZip", url: original_url, error: e.message, level: "error")
    end
  end

  def extract_extension_from_url(url)
    uri = URI.parse(url)
    path = uri.path
    filename = File.basename(path)
    if filename.include?(".")
      File.extname(filename)
    else
      nil
    end
  rescue => e
    nil
  end

  def detect_content_type_from_url(url)
    require "net/http"
    Net::HTTP.start(URI.parse(url).host, use_ssl: true) do |http|
      response = http.head(url)
      content_type = response["Content-Type"]
      if content_type.present?
        case content_type
        when "image/jpeg" then "image/jpeg"
        when "image/png" then "image/png"
        when "image/gif" then "image/gif"
        when "image/webp" then "image/webp"
        when "image/svg+xml" then "image/svg+xml"
        when "image/bmp" then "image/bmp"
        else "image/jpeg"
        end
      end
    end
  rescue => e
    "image/jpeg"
  end

  def process_setting_footer_content(content, record_id = nil, record_type = nil)
    return content if content.blank?
    record_id ||= "setting"
    record_type ||= "setting"
    Rails.event.notify("import_zip.setting_footer_processing", component: "ImportZip", record_id: record_id, level: "info")
    doc = Nokogiri::HTML.fragment(content)
    doc.css("action-text-attachment").each { |a| safe_process { process_imported_attachment_element(a, record_id, record_type) } }
    doc.css("figure[data-trix-attachment]").each { |f| safe_process { process_imported_figure_element(f, record_id, record_type) } }
    doc.css("img").each { |img| safe_process { process_imported_image_element(img, record_id, record_type) } }
    doc.to_html
  end

  def fix_content_sgid_references(content)
    return content unless content.present?
    doc = Nokogiri::HTML(content)
    doc.css("action-text-attachment").each do |attachment|
      begin
        filename = attachment["filename"]
        next unless filename.present?
        blob = ActiveStorage::Blob.find_by(filename: filename)
        next unless blob
        current_sgid = attachment["sgid"]
        correct_sgid = blob.to_sgid.to_s
        attachment["sgid"] = correct_sgid if current_sgid != correct_sgid
        if blob && blob.filename.present?
          correct_signed_id = blob.signed_id
          correct_url = Rails.application.routes.url_helpers.rails_blob_path(
            signed_id: blob.signed_id,
            filename: blob.filename.to_s,
            only_path: true
          )
          attachment["url"] = correct_url
        end
      rescue => e
        Rails.event.notify("import_zip.attachment_fix_failed", component: "ImportZip", error: e.message, level: "error")
      end
    end

    doc.css("figure[data-trix-attachment]").each do |figure|
      begin
        attachment_data = JSON.parse(figure["data-trix-attachment"]) rescue nil
        next unless attachment_data
        filename = attachment_data["filename"]
        next unless filename.present?
        blob = ActiveStorage::Blob.find_by(filename: filename)
        next unless blob
        current_sgid = figure["sgid"]
        correct_sgid = blob.to_sgid.to_s
        figure["sgid"] = correct_sgid if current_sgid != correct_sgid
      rescue => e
        Rails.event.notify("import_zip.figure_fix_failed", component: "ImportZip", error: e.message, level: "error")
      end
    end

    doc.to_html
  rescue => e
    Rails.event.notify("import_zip.sgid_fix_failed", component: "ImportZip", error: e.message, level: "error")
    content # 返回原始内容, 如果异常
  end

  # 工具&校验方法
  def parse_json_field(json_string)
    return nil if json_string.blank?
    JSON.parse(json_string)
  rescue JSON::ParserError
    Rails.event.notify("import_zip.json_parse_failed", component: "ImportZip", reason: "invalid_format", level: "warn")
    {}
  end

  def cast_boolean(value, default: false)
    casted = ActiveModel::Type::Boolean.new.cast(value)
    casted.nil? ? default : casted
  end

  def detect_content_type(file_path)
    mime_type = `file --brief --mime-type #{file_path.shellescape}`.strip rescue nil
    mime_type.present? && !mime_type.include?("error") ? mime_type : "application/octet-stream"
  end

  def extract_blob_from_url(url)
    match = url.match(/\/rails\/active_storage\/(?:blobs|representations)\/redirect\/([^\/]+)/)
    return nil unless match
    signed_id = match[1]
    ActiveStorage::Blob.find_signed(signed_id)
  rescue => e
    Rails.event.notify("import_zip.blob_extraction_failed", component: "ImportZip", signed_id: signed_id, error: e.message, level: "error")
    nil
  end

  def safe_join_path(base_path, relative_path)
    clean_path = relative_path.to_s.gsub(/\.{2,}/, "").gsub(%r{^/}, "")
    # Use find_csv_base_dir if base_path is @import_dir
    actual_base = (base_path == @import_dir) ? find_csv_base_dir : base_path
    File.join(actual_base, clean_path)
  end

  def safe_file_path?(file_path)
    expanded_path = File.expand_path(file_path)
    expanded_import_dir = File.expand_path(@import_dir)
    expanded_path.start_with?(expanded_import_dir)
  end

  def update_attachment_element_with_blob(attachment, blob)
    url = Rails.application.routes.url_helpers.rails_blob_path(blob, only_path: true)
    attachment["url"] = url
    attachment["sgid"] = blob.to_sgid.to_s
    update_img_src_in_node(attachment, url)
  end

  def update_img_src_in_node(node, url)
    img = node.at_css("img")
    img["src"] = url if img
  end

  def is_local_attachment?(url)
    url.include?("attachments/") && !url.start_with?("http")
  end

  def is_active_storage_url?(url)
    url.include?("/rails/active_storage/blobs/") || url.include?("/rails/active_storage/representations/")
  end

  def safe_process
    yield
  rescue => e
    Rails.event.notify("import_zip.safe_process_exception", component: "ImportZip", error: e.message, level: "error")
  end
end
