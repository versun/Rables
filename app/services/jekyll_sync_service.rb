# frozen_string_literal: true

require "fileutils"
require "reverse_markdown"
require "yaml"
require "ipaddr"
require "resolv"

class JekyllSyncService
  include Exports::HtmlAttachmentProcessing

  # Whitelist of allowed fields for front matter mapping
  ARTICLE_ALLOWED_FIELDS = %w[
    title slug description status created_at updated_at scheduled_at
    source_url source_author source_content meta_description
  ].freeze

  PAGE_ALLOWED_FIELDS = %w[
    title slug status created_at updated_at
    redirect_url page_order
  ].freeze

  # Private IP ranges for SSRF protection
  PRIVATE_IP_RANGES = [
    IPAddr.new("10.0.0.0/8"),
    IPAddr.new("172.16.0.0/12"),
    IPAddr.new("192.168.0.0/16"),
    IPAddr.new("127.0.0.0/8"),
    IPAddr.new("169.254.0.0/16"),
    IPAddr.new("0.0.0.0/8"),
    IPAddr.new("::1/128"),
    IPAddr.new("fc00::/7"),
    IPAddr.new("fe80::/10")
  ].freeze

  HTTP_TIMEOUT = 10 # seconds

  attr_reader :setting, :sync_record, :error_message, :stats

  def initialize(setting = nil)
    @setting = setting || JekyllSetting.instance
    @sync_record = nil
    @error_message = nil
    @stats = { articles: 0, pages: 0, attachments: 0, comments: 0, redirects: 0, static_files: 0 }
  end

  # Full sync - export all published content
  def sync_all(triggered_by: "manual")
    return failure("Jekyll is not configured") unless @setting.configured?
    return failure("Jekyll path is not valid") unless @setting.jekyll_path_valid?

    @sync_record = JekyllSyncRecord.create!(
      sync_type: :full,
      status: :in_progress,
      triggered_by: triggered_by,
      started_at: Time.current
    )

    begin
      ensure_directories
      sync_articles(Article.where(status: :publish))
      sync_pages(Page.where(status: :publish))

      # Export comments if enabled
      export_comments if @setting.export_comments?

      # Export redirects
      export_redirects

      # Export static files
      export_static_files

      commit_and_push if @setting.git_repository?

      @sync_record.mark_completed!(
        articles: @stats[:articles],
        pages: @stats[:pages],
        attachments: @stats[:attachments],
        git_sha: @git_commit_sha
      )

      @setting.update!(last_sync_at: Time.current)

      log_activity(:completed, "Full sync completed: #{@stats[:articles]} articles, #{@stats[:pages]} pages, #{@stats[:comments]} comments")
      true
    rescue => e
      @error_message = e.message
      @sync_record&.mark_failed!(e.message)
      log_activity(:failed, "Full sync failed: #{e.message}")
      Rails.event.notify("jekyll_sync.failed", component: "JekyllSyncService", error: e.message, backtrace: e.backtrace.join("\n"), level: "error")
      false
    end
  end

  # Single article sync
  def sync_article(article, triggered_by: "publish")
    return failure("Jekyll is not configured") unless @setting.configured?
    return failure("Jekyll path is not valid") unless @setting.jekyll_path_valid?

    @sync_record = JekyllSyncRecord.create!(
      sync_type: :single,
      status: :in_progress,
      triggered_by: triggered_by,
      started_at: Time.current
    )

    begin
      ensure_directories
      export_article(article)

      commit_and_push("Sync article: #{article.title}") if @setting.git_repository?

      @sync_record.mark_completed!(
        articles: 1,
        pages: 0,
        attachments: @stats[:attachments],
        git_sha: @git_commit_sha
      )

      log_activity(:completed, "Article synced: #{article.title}")
      true
    rescue => e
      @error_message = e.message
      @sync_record&.mark_failed!(e.message)
      log_activity(:failed, "Article sync failed: #{e.message}")
      false
    end
  end

  # Single page sync
  def sync_page(page, triggered_by: "publish")
    return failure("Jekyll is not configured") unless @setting.configured?
    return failure("Jekyll path is not valid") unless @setting.jekyll_path_valid?

    @sync_record = JekyllSyncRecord.create!(
      sync_type: :single,
      status: :in_progress,
      triggered_by: triggered_by,
      started_at: Time.current
    )

    begin
      ensure_directories
      export_page(page)

      commit_and_push("Sync page: #{page.title}") if @setting.git_repository?

      @sync_record.mark_completed!(
        articles: 0,
        pages: 1,
        attachments: @stats[:attachments],
        git_sha: @git_commit_sha
      )

      log_activity(:completed, "Page synced: #{page.title}")
      true
    rescue => e
      @error_message = e.message
      @sync_record&.mark_failed!(e.message)
      log_activity(:failed, "Page sync failed: #{e.message}")
      false
    end
  end

  # Delete article file
  def delete_article(article)
    return unless @setting.configured? && @setting.jekyll_path_valid?

    filename = article_filename(article)
    filepath = File.join(@setting.full_posts_path, filename)

    if File.exist?(filepath)
      FileUtils.rm(filepath)
      log_activity(:deleted, "Deleted article file: #{filename}")
    end
  end

  # Delete page file
  def delete_page(page)
    return unless @setting.configured? && @setting.jekyll_path_valid?

    filename = "#{page.slug}.md"
    filepath = File.join(@setting.full_pages_path, filename)

    if File.exist?(filepath)
      FileUtils.rm(filepath)
      log_activity(:deleted, "Deleted page file: #{filename}")
    end
  end

  # Preview export without writing
  def preview_article(article)
    build_article_content(article)
  end

  def preview_page(page)
    build_page_content(page)
  end

  private

  def failure(message)
    @error_message = message
    false
  end

  def ensure_directories
    FileUtils.mkdir_p(@setting.full_posts_path)
    FileUtils.mkdir_p(@setting.full_pages_path)
    FileUtils.mkdir_p(@setting.full_images_path)
  end

  def sync_articles(articles)
    articles.find_each do |article|
      export_article(article)
      @stats[:articles] += 1
    end
  end

  def sync_pages(pages)
    pages.find_each do |page|
      export_page(page)
      @stats[:pages] += 1
    end
  end

  def export_article(article)
    content = build_article_content(article)
    filename = article_filename(article)
    filepath = File.join(@setting.full_posts_path, filename)

    File.write(filepath, content)
    copy_article_attachments(article)
  end

  def export_page(page)
    content = build_page_content(page)
    filename = "#{page.slug}.md"
    filepath = File.join(@setting.full_pages_path, filename)

    File.write(filepath, content)
    copy_page_attachments(page)
  end

  def build_article_content(article)
    front_matter = build_article_front_matter(article)
    body = build_article_body(article)

    "---\n#{front_matter.to_yaml.sub(/\A---\s*\n/, "")}---\n\n#{body.strip}\n"
  end

  def build_page_content(page)
    front_matter = build_page_front_matter(page)
    body = build_page_body(page)

    "---\n#{front_matter.to_yaml.sub(/\A---\s*\n/, "")}---\n\n#{body.strip}\n"
  end

  def build_article_front_matter(article)
    # Determine the date to use
    date = article.scheduled_at || article.created_at || Time.current

    fm = {
      "layout" => "post",
      "title" => article.title,
      "date" => date.strftime("%Y-%m-%d %H:%M:%S %z"),
      "categories" => article.tags.map(&:name),
      "tags" => article.tags.map(&:name)
    }

    fm["description"] = article.description if article.description.present?
    fm["author"] = CacheableSettings.site_info[:author] if CacheableSettings.site_info[:author].present?

    # Add source reference if present
    if article.has_source?
      fm["source_url"] = article.source_url if article.source_url.present?
      fm["source_author"] = article.source_author if article.source_author.present?
    end

    # Apply custom front matter mapping
    apply_front_matter_mapping(fm, article)

    fm.compact
  end

  def build_page_front_matter(page)
    fm = {
      "layout" => "page",
      "title" => page.title,
      "permalink" => "/#{page.slug}/"
    }

    fm["redirect_to"] = page.redirect_url if page.redirect_url.present?
    fm["order"] = page.page_order if page.page_order.present?

    apply_front_matter_mapping(fm, page)

    fm.compact
  end

  def apply_front_matter_mapping(front_matter, record)
    mapping = @setting.front_matter_mapping_hash
    return front_matter if mapping.empty?

    # Determine allowed fields based on record type
    allowed_fields = case record
    when Article
      ARTICLE_ALLOWED_FIELDS
    when Page
      PAGE_ALLOWED_FIELDS
    else
      []
    end

    mapping.each do |source_field, target_field|
      # Only allow whitelisted fields to prevent arbitrary method invocation
      next unless allowed_fields.include?(source_field.to_s)
      next unless target_field.is_a?(String) && target_field.match?(/\A[a-z_][a-z0-9_]*\z/i)

      if record.respond_to?(source_field) && record.send(source_field).present?
        front_matter[target_field] = record.send(source_field)
      end
    end

    front_matter
  end

  def build_article_body(article)
    html = article_html(article)
    html = process_images_in_html(html, article)
    markdown = html_to_markdown(html)

    # Add source reference block
    if article.has_source?
      reference = build_source_reference(article)
      markdown = "#{reference}\n\n#{markdown}" if reference.present?
    end

    markdown
  end

  def build_page_body(page)
    html = page_html(page)
    html = process_images_in_html(html, page)
    html_to_markdown(html)
  end

  def article_html(article)
    if article.html?
      article.html_content.to_s
    elsif article.content.present?
      article.content.to_trix_html
    else
      ""
    end
  end

  def page_html(page)
    if page.html?
      page.html_content.to_s
    elsif page.content.present?
      page.content.to_trix_html
    else
      ""
    end
  end

  def html_to_markdown(html)
    ReverseMarkdown.convert(html, unknown_tags: :bypass, github_flavored: true, force_encoding: true).to_s
  end

  def build_source_reference(article)
    return "" unless article.has_source?

    lines = []
    lines << "> **Source Reference**"
    lines << ">" if article.source_author.present? || article.source_content.present?
    lines << "> *#{article.source_author}*" if article.source_author.present?

    if article.source_content.present?
      article.source_content.split(/\r?\n/).each do |line|
        lines << "> #{line}"
      end
    end

    lines << ">" if article.source_url.present?
    lines << "> [Original source](#{article.source_url})" if article.source_url.present?

    lines.join("\n")
  end

  def process_images_in_html(html, record)
    return html if html.blank?

    doc = Nokogiri::HTML.fragment(html)
    doc.css("img").each do |img|
      src = img["src"]
      next if src.blank?

      # Convert to Jekyll asset path
      new_path = convert_image_path(src, record)
      img["src"] = new_path if new_path
    end

    doc.to_html
  end

  def convert_image_path(src, record)
    # Handle ActiveStorage URLs
    if src.include?("/rails/active_storage/")
      # Extract filename and copy to Jekyll assets
      return copy_active_storage_image(src, record)
    end

    # Handle relative paths
    if src.start_with?("/")
      return src # Keep as-is for now
    end

    # Handle remote URLs
    if src.start_with?("http")
      return download_and_copy_remote_image(src, record) if @setting.download_remote_images?

      return src
    end

    src
  end

  def copy_active_storage_image(src, record)
    # Parse the blob from the URL
    begin
      # Match ActiveStorage blob URLs
      if src =~ /\/rails\/active_storage\/blobs\/redirect\/([^\/]+)\/(.+)/
        signed_id = $1
        filename = $2
        blob = ActiveStorage::Blob.find_signed(signed_id)

        if blob
          slug = record.respond_to?(:slug) ? record.slug : "item_#{record.id}"
          target_dir = File.join(@setting.full_images_path, slug)
          FileUtils.mkdir_p(target_dir)

          target_path = File.join(target_dir, filename)
          blob.download do |chunk|
            File.open(target_path, "ab") { |f| f.write(chunk) }
          end

          @stats[:attachments] += 1
          return "/#{@setting.images_directory}/#{slug}/#{filename}"
        end
      end
    rescue => e
      Rails.event.notify("jekyll_sync.image_copy_failed", component: "JekyllSyncService", src: src, error: e.message, level: "warn")
    end

    src
  end

  def download_and_copy_remote_image(url, record)
    begin
      uri = URI.parse(url)

      # SSRF protection: only allow http/https schemes
      unless %w[http https].include?(uri.scheme&.downcase)
        Rails.event.notify("jekyll_sync.remote_image_blocked",
          component: "JekyllSyncService", url: url, reason: "invalid_scheme", level: "warn")
        return url
      end

      # SSRF protection: block private/internal IPs
      if private_ip?(uri.host)
        Rails.event.notify("jekyll_sync.remote_image_blocked",
          component: "JekyllSyncService", url: url, reason: "private_ip", level: "warn")
        return url
      end

      filename = File.basename(uri.path)
      filename = "image_#{SecureRandom.hex(4)}#{File.extname(uri.path)}" if filename.blank?
      # Sanitize filename to prevent path traversal
      filename = sanitize_filename(filename)

      slug = record.respond_to?(:slug) ? record.slug : "item_#{record.id}"
      target_dir = File.join(@setting.full_images_path, slug)
      FileUtils.mkdir_p(target_dir)

      target_path = File.join(target_dir, filename)

      # Use timeout for HTTP request
      http = Net::HTTP.new(uri.host, uri.port)
      http.use_ssl = (uri.scheme == "https")
      http.open_timeout = HTTP_TIMEOUT
      http.read_timeout = HTTP_TIMEOUT

      request = Net::HTTP::Get.new(uri.request_uri)
      response = http.request(request)

      if response.is_a?(Net::HTTPSuccess)
        File.binwrite(target_path, response.body)
        @stats[:attachments] += 1
        return "/#{@setting.images_directory}/#{slug}/#{filename}"
      end
    rescue => e
      Rails.event.notify("jekyll_sync.remote_image_download_failed", component: "JekyllSyncService", url: url, error: e.message, level: "warn")
    end

    url
  end

  def private_ip?(host)
    return true if host.nil? || host.empty?

    # Block localhost variations
    return true if host.downcase == "localhost"
    return true if host.match?(/\A127\.\d+\.\d+\.\d+\z/)

    begin
      ip = IPAddr.new(host)
      PRIVATE_IP_RANGES.any? { |range| range.include?(ip) }
    rescue IPAddr::InvalidAddressError
      # If it's a hostname, resolve it and check
      begin
        resolved = Resolv.getaddress(host)
        ip = IPAddr.new(resolved)
        PRIVATE_IP_RANGES.any? { |range| range.include?(ip) }
      rescue Resolv::ResolvError, IPAddr::InvalidAddressError
        false
      end
    end
  end

  def sanitize_filename(filename)
    # Remove path traversal attempts and invalid characters
    filename.gsub(/\.\./, "").gsub(%r{[/\\]}, "").gsub(/[^a-zA-Z0-9._-]/, "_")
  end

  def copy_article_attachments(article)
    # Additional attachment processing can be added here
  end

  def copy_page_attachments(page)
    # Additional attachment processing can be added here
  end

  def article_filename(article)
    date = article.scheduled_at || article.created_at || Time.current
    date_prefix = date.strftime("%Y-%m-%d")
    "#{date_prefix}-#{article.slug}.md"
  end

  def commit_and_push(message = nil)
    return unless @setting.git_repository?

    message ||= "Jekyll sync: #{Time.current.strftime('%Y-%m-%d %H:%M:%S')}"

    Dir.chdir(@setting.jekyll_path) do
      # Add all changes
      system("git", "add", "-A")

      # Check if there are changes to commit
      status = `git status --porcelain`.strip
      return if status.empty?

      # Commit
      system("git", "commit", "-m", message)

      # Get commit SHA
      @git_commit_sha = `git rev-parse HEAD`.strip

      # Push if repository URL is configured
      if @setting.repository_url.present?
        system("git", "push", "origin", @setting.branch)
      end
    end
  end

  def export_comments
    exporter = JekyllCommentsExporter.new(@setting)
    result = exporter.export_all
    return unless result

    @stats[:comments] = exporter.stats[:comments]
    Rails.event.notify("jekyll_sync.comments_exported",
      component: "JekyllSyncService",
      articles: exporter.stats[:articles],
      comments: exporter.stats[:comments],
      level: "info")
  end

  def export_redirects
    exporter = JekyllRedirectsExporter.new(@setting)
    result = exporter.export_all
    return unless result

    @stats[:redirects] = exporter.stats[:exported]
    Rails.event.notify("jekyll_sync.redirects_exported",
      component: "JekyllSyncService",
      exported: exporter.stats[:exported],
      level: "info")
  end

  def export_static_files
    exporter = JekyllStaticFilesExporter.new(@setting)
    result = exporter.export_all
    return unless result

    @stats[:static_files] = exporter.stats[:exported]
    Rails.event.notify("jekyll_sync.static_files_exported",
      component: "JekyllSyncService",
      exported: exporter.stats[:exported],
      errors: exporter.stats[:errors],
      level: "info")
  end

  def log_activity(action, message)
    ActivityLog.log!(
      action: action,
      target: :jekyll_sync,
      level: action == :failed ? :error : :info,
      message: message
    )
  end
end
