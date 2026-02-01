class JekyllSyncService
  def initialize(setting: JekyllSetting.instance)
    @setting = setting
    @exporter = JekyllExport.new(setting: setting)
  end

  def sync_all
    ensure_ready!
    ensure_git_repository! if @setting.git?

    Rails.event.notify("jekyll_sync_service.sync_all_started",
      level: "info",
      component: self.class.name,
      jekyll_path: @setting.jekyll_path)
    ActivityLog.log!(action: :started, target: :jekyll_sync, level: :info, mode: "full")

    articles_count = 0
    pages_count = 0
    git_commit_sha = nil

    with_authenticated_remote do
      git_operations.pull_latest(branch: @setting.branch) if @setting.git?

      Article.order(:id).includes(:tags).find_each do |article|
        sync_article(article)
        articles_count += 1
      end

      Page.order(:id).find_each do |page|
        sync_page(page)
        pages_count += 1
      end

      export_static_files
      export_redirects
      export_comments if @setting.export_comments?

      if @setting.git?
        git_commit_sha = git_operations.commit_and_push(
          "Sync Jekyll content at #{Time.current.strftime('%Y-%m-%d %H:%M:%S')}",
          branch: @setting.branch
        )
      end
    end

    @setting.update(last_sync_at: Time.current)

    Rails.event.notify("jekyll_sync_service.sync_all_completed",
      level: "info",
      component: self.class.name,
      articles_count: articles_count,
      pages_count: pages_count,
      git_commit_sha: git_commit_sha)
    ActivityLog.log!(
      action: :completed,
      target: :jekyll_sync,
      level: :info,
      mode: "full",
      count: articles_count + pages_count,
      articles_count: articles_count,
      pages_count: pages_count,
      git_commit_sha: git_commit_sha
    )

    { articles_count: articles_count, pages_count: pages_count, git_commit_sha: git_commit_sha }
  rescue => e
    Rails.event.notify("jekyll_sync_service.sync_all_failed",
      level: "error",
      component: self.class.name,
      error: e.message)
    ActivityLog.log!(action: :failed, target: :jekyll_sync, level: :error, mode: "full", error: e.message)
    raise
  end

  def sync_article(article)
    ensure_ready!
    ensure_git_repository! if @setting.git?
    git_commit_sha = nil

    Rails.event.notify("jekyll_sync_service.sync_article_started",
      level: "info",
      component: self.class.name,
      article_id: article.id)
    ActivityLog.log!(action: :started, target: :jekyll_sync, level: :info, mode: "article", id: article.id, slug: article.slug)

    with_authenticated_remote do
      git_operations.pull_latest(branch: @setting.branch) if @setting.git?
      delete_article(article)
      @exporter.export_article(article)
      export_comments_for_article(article) if @setting.export_comments?
      if @setting.git?
        git_commit_sha = git_operations.commit_and_push(
          "Sync article #{article.slug.presence || article.id}",
          branch: @setting.branch
        )
      end
    end

    Rails.event.notify("jekyll_sync_service.sync_article_completed",
      level: "info",
      component: self.class.name,
      article_id: article.id,
      git_commit_sha: git_commit_sha)
    ActivityLog.log!(
      action: :completed,
      target: :jekyll_sync,
      level: :info,
      mode: "article",
      id: article.id,
      slug: article.slug,
      git_commit_sha: git_commit_sha
    )

    git_commit_sha
  rescue => e
    Rails.event.notify("jekyll_sync_service.sync_article_failed",
      level: "error",
      component: self.class.name,
      article_id: article.id,
      error: e.message)
    ActivityLog.log!(action: :failed, target: :jekyll_sync, level: :error, mode: "article", id: article.id, slug: article.slug, error: e.message)
    raise
  end

  def sync_page(page)
    ensure_ready!
    ensure_git_repository! if @setting.git?
    git_commit_sha = nil

    Rails.event.notify("jekyll_sync_service.sync_page_started",
      level: "info",
      component: self.class.name,
      page_id: page.id)
    ActivityLog.log!(action: :started, target: :jekyll_sync, level: :info, mode: "page", id: page.id, slug: page.slug)

    with_authenticated_remote do
      git_operations.pull_latest(branch: @setting.branch) if @setting.git?
      delete_page(page)
      @exporter.export_page(page)
      export_comments_for_page(page) if @setting.export_comments?
      if @setting.git?
        git_commit_sha = git_operations.commit_and_push(
          "Sync page #{page.slug.presence || page.id}",
          branch: @setting.branch
        )
      end
    end

    Rails.event.notify("jekyll_sync_service.sync_page_completed",
      level: "info",
      component: self.class.name,
      page_id: page.id,
      git_commit_sha: git_commit_sha)
    ActivityLog.log!(
      action: :completed,
      target: :jekyll_sync,
      level: :info,
      mode: "page",
      id: page.id,
      slug: page.slug,
      git_commit_sha: git_commit_sha
    )

    git_commit_sha
  rescue => e
    Rails.event.notify("jekyll_sync_service.sync_page_failed",
      level: "error",
      component: self.class.name,
      page_id: page.id,
      error: e.message)
    ActivityLog.log!(action: :failed, target: :jekyll_sync, level: :error, mode: "page", id: page.id, slug: page.slug, error: e.message)
    raise
  end

  def delete_article(article)
    ensure_ready!
    delete_files_with_rables_id(posts_dir, article.id)
  end

  def delete_page(page)
    ensure_ready!
    delete_files_with_rables_id(pages_dir, page.id)
  end

  private

  def ensure_ready!
    raise "Jekyll path is not configured" if @setting.jekyll_path.blank?
    raise "Jekyll path does not exist" unless File.directory?(@setting.jekyll_path)
  end

  def ensure_git_repository!
    return unless @setting.git?

    git_dir = File.join(@setting.jekyll_path.to_s, ".git")
    raise "Jekyll path is not a git repository" unless File.directory?(git_dir)
  end

  def with_authenticated_remote
    return yield unless @setting.git?
    return yield if @setting.repository_url.blank?

    integration = GitIntegration.enabled.first
    auth_url = integration ? integration.build_authenticated_url(@setting.repository_url) : @setting.repository_url
    original = git_operations.current_remote_url
    changed = false

    if auth_url.present? && original.present? && auth_url != original
      git_operations.set_remote_url("origin", auth_url)
      changed = true
    end

    yield
  ensure
    if changed && original.present?
      git_operations.set_remote_url("origin", original)
    end
  end

  def git_operations
    @git_operations ||= GitOperationsService.new(@setting.jekyll_path)
  end

  def posts_dir
    Pathname.new(@setting.jekyll_path).join(@setting.posts_directory.to_s)
  end

  def pages_dir
    Pathname.new(@setting.jekyll_path).join(@setting.pages_directory.to_s)
  end

  def delete_files_with_rables_id(directory, id)
    Dir.glob(File.join(directory, "*.md")).each do |path|
      content = File.read(path)
      next unless content.include?("rables_id: #{id}")

      File.delete(path)
    end
  end

  def export_redirects
    return if @setting.redirect_export_format.blank?

    exporter = JekyllRedirectsExporter.new(setting: @setting)
    case @setting.redirect_export_format
    when "netlify"
      exporter.export_to_netlify
    when "vercel"
      exporter.export_to_vercel
    when "htaccess"
      exporter.export_to_htaccess
    when "nginx"
      exporter.export_to_nginx
    when "jekyll-plugin"
      exporter.export_to_jekyll_plugin
    end
  end

  def export_static_files
    JekyllStaticFilesExporter.new(setting: @setting).export_all
  end

  def export_comments
    JekyllCommentsExporter.new(setting: @setting).export_all
  end

  def export_comments_for_article(article)
    JekyllCommentsExporter.new(setting: @setting).export_for_article(article)
  end

  def export_comments_for_page(page)
    JekyllCommentsExporter.new(setting: @setting).export_for_page(page)
  end
end
