class JekyllSyncService
  include Rails.application.routes.url_helpers

  attr_reader :setting, :export, :sync_record

  def initialize(setting = nil, sync_record = nil)
    @setting = setting || JekyllSetting.instance
    @export = JekyllExport.new(@setting)
    @sync_record = sync_record
    @errors = []
  end

  # Sync all content
  def sync_all(articles: Article.publish, pages: Page.publish)
    return false unless validate_setting

    sync_record&.mark_started!

    articles_count = 0
    pages_count = 0

    begin
      # Ensure directories exist
      ensure_directories

      # Sync articles
      articles.find_each do |article|
        if sync_article(article)
          articles_count += 1
        end
      end

      # Sync pages
      pages.find_each do |page|
        if sync_page(page)
          pages_count += 1
        end
      end

      # Clean up deleted content
      cleanup_deleted_content(articles, pages)

      # Update sync record
      if sync_record
        sync_record.update(
          articles_count: articles_count,
          pages_count: pages_count
        )
      end

      # Commit and push if Git is enabled
      if setting.git_enabled?
        commit_sha = commit_and_push("Sync content: #{articles_count} articles, #{pages_count} pages")
        sync_record&.mark_completed!(commit_sha: commit_sha)
      else
        sync_record&.mark_completed!
      end

      log_success(articles_count, pages_count)
      true
    rescue => e
      handle_error(e)
      false
    end
  end

  # Sync single article
  def sync_article(article)
    return false unless validate_setting
    return false unless article

    begin
      exported = export.export_article(article)
      return false unless exported

      # Write file
      file_path = File.join(setting.posts_full_path, exported[:filename])
      File.write(file_path, exported[:content])

      # Copy attachments
      copy_attachments(article, exported[:attachments])

      log_sync(:article, article.slug, :success)
      true
    rescue => e
      log_sync(:article, article.slug, :failed, e.message)
      false
    end
  end

  # Sync single page
  def sync_page(page)
    return false unless validate_setting
    return false unless page

    begin
      exported = export.export_page(page)
      return false unless exported

      # Write file
      file_path = File.join(setting.pages_full_path, exported[:filename])
      File.write(file_path, exported[:content])

      # Copy attachments
      copy_attachments(page, exported[:attachments])

      log_sync(:page, page.slug, :success)
      true
    rescue => e
      log_sync(:page, page.slug, :failed, e.message)
      false
    end
  end

  # Delete article file
  def delete_article(article)
    return false unless validate_setting
    return false unless article

    begin
      filename = export.article_filename(article)
      file_path = File.join(setting.posts_full_path, filename)

      if File.exist?(file_path)
        File.delete(file_path)
        log_sync(:article, article.slug, :deleted)
      end

      true
    rescue => e
      log_sync(:article, article.slug, :failed, e.message)
      false
    end
  end

  # Delete page file
  def delete_page(page)
    return false unless validate_setting
    return false unless page

    begin
      filename = export.page_filename(page)
      file_path = File.join(setting.pages_full_path, filename)

      if File.exist?(file_path)
        File.delete(file_path)
        log_sync(:page, page.slug, :deleted)
      end

      true
    rescue => e
      log_sync(:page, page.slug, :failed, e.message)
      false
    end
  end

  # Verify Jekyll configuration
  def verify
    errors = []

    if setting.jekyll_path.blank?
      errors << "Jekyll path is not configured"
    elsif !Dir.exist?(setting.jekyll_path)
      errors << "Jekyll path does not exist: #{setting.jekyll_path}"
    elsif !File.writable?(setting.jekyll_path)
      errors << "Jekyll path is not writable: #{setting.jekyll_path}"
    end

    # Check if posts directory exists or can be created
    if setting.posts_full_path && !Dir.exist?(setting.posts_full_path)
      begin
        FileUtils.mkdir_p(setting.posts_full_path)
      rescue => e
        errors << "Cannot create posts directory: #{e.message}"
      end
    end

    # Check if pages directory exists or can be created
    if setting.pages_full_path && !Dir.exist?(setting.pages_full_path)
      begin
        FileUtils.mkdir_p(setting.pages_full_path)
      rescue => e
        errors << "Cannot create pages directory: #{e.message}"
      end
    end

    # Verify Git if enabled
    if setting.git_enabled?
      git_errors = verify_git
      errors.concat(git_errors)
    end

    errors
  end

  private

  def validate_setting
    unless setting.jekyll_path_valid?
      @errors << "Jekyll path is not valid"
      return false
    end
    true
  end

  def ensure_directories
    FileUtils.mkdir_p(setting.posts_full_path) if setting.posts_full_path
    FileUtils.mkdir_p(setting.pages_full_path) if setting.pages_full_path
    FileUtils.mkdir_p(setting.images_full_path) if setting.images_full_path
  end

  def copy_attachments(item, attachments)
    return if attachments.blank?

    # Get target directory for this item's attachments
    target_dir = File.join(setting.images_full_path, item.slug)
    FileUtils.mkdir_p(target_dir)

    # Process each attachment
    attachments.each do |attachment|
      # This is a placeholder - actual implementation would copy files from ActiveStorage
      # to the Jekyll assets directory
    end
  end

  def cleanup_deleted_content(articles, pages)
    # Remove files for articles that no longer exist or are not published
    return unless setting.posts_full_path

    existing_slugs = articles.pluck(:slug)

    Dir.glob(File.join(setting.posts_full_path, "*.md")).each do |file|
      filename = File.basename(file)
      # Extract slug from filename (YYYY-MM-DD-slug.md)
      slug = filename.sub(/^\d{4}-\d{2}-\d{2}-/, "").sub(/\.md$/, "")

      unless existing_slugs.include?(slug)
        File.delete(file)
        log_sync(:article, slug, :cleaned)
      end
    end

    # Remove files for pages that no longer exist or are not published
    return unless setting.pages_full_path

    existing_page_slugs = pages.pluck(:slug)

    Dir.glob(File.join(setting.pages_full_path, "*.md")).each do |file|
      slug = File.basename(file, ".md")

      unless existing_page_slugs.include?(slug)
        File.delete(file)
        log_sync(:page, slug, :cleaned)
      end
    end
  end

  def commit_and_push(message)
    return nil unless setting.git_enabled?

    begin
      # Add all changes
      git_command("add", ".")

      # Commit
      git_command("commit", "-m", message)

      # Get commit SHA
      sha = git_command("rev-parse", "HEAD").strip

      # Push to remote
      git_command("push", "origin", setting.branch)

      sha
    rescue => e
      Rails.logger.error "Git operation failed: #{e.message}"
      nil
    end
  end

  def verify_git
    errors = []

    # Check if git is installed
    begin
      `git --version`
    rescue
      errors << "Git is not installed"
      return errors
    end

    # Check if jekyll_path is a git repository
    git_dir = File.join(setting.jekyll_path, ".git")
    unless Dir.exist?(git_dir)
      errors << "Jekyll path is not a Git repository"
    end

    errors
  end

  def git_command(*args)
    Dir.chdir(setting.jekyll_path) do
      result = `git #{args.join(" ")} 2>&1`
      raise "Git command failed: #{result}" unless $?.success?
      result
    end
  end

  def log_sync(type, identifier, status, message = nil)
    ActivityLog.log!(
      action: :jekyll_sync,
      target: type,
      level: status == :success ? :info : :error,
      identifier: identifier,
      status: status,
      message: message
    )
  end

  def log_success(articles_count, pages_count)
    Rails.event.notify(
      "jekyll.sync.completed",
      level: "info",
      component: "JekyllSyncService",
      articles_count: articles_count,
      pages_count: pages_count
    )
  end

  def handle_error(error)
    @errors << error.message
    sync_record&.mark_failed!(error.message)

    Rails.event.notify(
      "jekyll.sync.failed",
      level: "error",
      component: "JekyllSyncService",
      error: error.message,
      backtrace: error.backtrace&.first(5)
    )
  end
end
