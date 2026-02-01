class MarkdownExport
  require "fileutils"
  require "reverse_markdown"
  require "securerandom"
  require "yaml"

  include Exports::HtmlAttachmentProcessing
  include Exports::ZipPackaging

  attr_reader :zip_path, :error_message, :export_dir, :attachments_dir

  def initialize(jekyll_mode: false, front_matter_mapping: nil, file_name_format: nil, assets_prefix: nil)
    @zip_path = nil
    @error_message = nil
    @jekyll_mode = jekyll_mode
    @front_matter_mapping = front_matter_mapping || {}
    @file_name_format = file_name_format
    @assets_prefix = assets_prefix
    timestamp = Time.current.strftime("%Y%m%d_%H%M%S")
    unique_suffix = "#{Process.pid}_#{SecureRandom.hex(4)}"
    @export_dir = Rails.root.join("tmp", "exports", "markdown_export_#{timestamp}_#{unique_suffix}")
    @attachments_dir = File.join(@export_dir, jekyll_mode? ? jekyll_assets_prefix : "attachments")

    FileUtils.mkdir_p(@export_dir)
    FileUtils.mkdir_p(@attachments_dir)
  end

  def generate
    Rails.event.notify("markdown_export.generation_started", component: "MarkdownExport", export_dir: @export_dir, level: "info")

    export_articles
    export_pages
    create_zip_file

    Rails.event.notify("markdown_export.generation_completed", component: "MarkdownExport", zip_path: @zip_path, level: "info")
    true
  rescue => e
    @error_message = e.message
    Rails.event.notify("markdown_export.generation_failed", component: "MarkdownExport", error: e.message, backtrace: e.backtrace.join("\n"), level: "error")
    false
  end

  private

  def export_articles
    Rails.event.notify("markdown_export.articles_started", component: "MarkdownExport", level: "info")

    articles_dir = File.join(@export_dir, articles_dir_name)
    FileUtils.mkdir_p(articles_dir)

    Article.order(:id).includes(:tags).find_each do |article|
      html = html_for_article(article)
      record_slug = jekyll_mode? ? article.slug : nil
      markdown = ReverseMarkdown.convert(
        process_html_content(html, record_id: article.id, record_type: "article", record_slug: record_slug),
        unknown_tags: :bypass,
        github_flavored: true,
        force_encoding: true
      ).to_s
      reference = reference_markdown_for(article)
      body = [ reference, markdown ].reject(&:blank?).join("\n\n")

      front_matter = jekyll_mode? ? jekyll_front_matter_for_article(article) : {
        "type" => "article",
        "id" => article.id,
        "title" => article.title,
        "slug" => article.slug,
        "description" => article.description,
        "status" => article.status,
        "scheduled_at" => article.scheduled_at&.iso8601,
        "created_at" => article.created_at&.iso8601,
        "updated_at" => article.updated_at&.iso8601,
        "tags" => article.tags.map(&:name)
      }.compact

      write_markdown_file(
        dir: articles_dir,
        basename: article_basename(article),
        front_matter: front_matter,
        body: body
      )
    end

    Rails.event.notify("markdown_export.articles_completed", component: "MarkdownExport", count: Article.count, level: "info")
  end

  def export_pages
    Rails.event.notify("markdown_export.pages_started", component: "MarkdownExport", level: "info")

    pages_dir = File.join(@export_dir, pages_dir_name)
    FileUtils.mkdir_p(pages_dir)

    Page.order(:id).find_each do |page|
      html = html_for_page(page)
      record_slug = jekyll_mode? ? page.slug : nil
      markdown = ReverseMarkdown.convert(
        process_html_content(html, record_id: page.id, record_type: "page", record_slug: record_slug),
        unknown_tags: :bypass,
        github_flavored: true,
        force_encoding: true
      ).to_s

      front_matter = jekyll_mode? ? jekyll_front_matter_for_page(page) : {
        "type" => "page",
        "id" => page.id,
        "title" => page.title,
        "slug" => page.slug,
        "status" => page.status,
        "redirect_url" => page.redirect_url,
        "page_order" => page.page_order,
        "created_at" => page.created_at&.iso8601,
        "updated_at" => page.updated_at&.iso8601
      }.compact

      write_markdown_file(
        dir: pages_dir,
        basename: page_basename(page),
        front_matter: front_matter,
        body: markdown
      )
    end

    Rails.event.notify("markdown_export.pages_completed", component: "MarkdownExport", count: Page.count, level: "info")
  end

  def html_for_article(article)
    if article.html?
      article.html_content.to_s
    elsif article.content.present?
      article.content.to_trix_html
    else
      ""
    end
  end

  def html_for_page(page)
    if page.html?
      page.html_content.to_s
    elsif page.content.present?
      page.content.to_trix_html
    else
      ""
    end
  end

  def reference_markdown_for(article)
    return "" unless article.has_source?

    author = sanitize_source_text(article.source_author)
    content = sanitize_source_text(article.source_content, preserve_line_breaks: true)
    url = sanitize_source_url(article.source_url)

    return "" if author.blank? && content.blank? && url.blank?

    lines = [ "Reference:" ]
    lines << "Source: #{author}" if author.present?

    quote_lines = []
    if content.present?
      quote_lines.concat(content.split(/\r?\n/))
    end
    if url.present?
      quote_lines << "" if content.present?
      quote_lines << "Original: #{url}"
    end

    if quote_lines.any?
      lines << ""
      lines.concat(quote_lines.map { |line| line.present? ? "> #{line}" : ">" })
    end

    lines.join("\n").strip
  end

  def sanitize_source_text(text, preserve_line_breaks: false)
    text = text.to_s
    if preserve_line_breaks
      text = text.gsub(/<\s*br\s*\/?>/i, "\n")
      text = text.gsub(/<\/\s*p\s*>/i, "\n")
      text = text.gsub(/<\s*p[^>]*>/i, "")
    end

    sanitized = ActionView::Base.full_sanitizer.sanitize(text)
    sanitized.gsub(/\r\n?/, "\n").strip
  end

  def sanitize_source_url(url)
    sanitize_source_text(url).split(/\r?\n/).first.to_s.strip
  end

  def write_markdown_file(dir:, basename:, front_matter:, body:)
    yaml = front_matter.to_yaml(line_width: -1)
    yaml = yaml.sub(/\A---\s*\n/, "")

    File.write(
      File.join(dir, "#{basename}.md"),
      +"---\n#{yaml}---\n\n#{body.strip}\n"
    )
  end

  def jekyll_mode?
    @jekyll_mode == true
  end

  def jekyll_assets_prefix
    @assets_prefix.presence || File.join("assets", "images", "posts")
  end

  def articles_dir_name
    jekyll_mode? ? "_posts" : "articles"
  end

  def pages_dir_name
    jekyll_mode? ? "_pages" : "pages"
  end

  def article_basename(article)
    return safe_basename(article.slug.presence || "article_#{article.id}") unless jekyll_mode?

    date_prefix = (article.scheduled_at.presence || article.created_at || Time.current).strftime("%Y-%m-%d")
    slug = safe_basename(article.slug.presence || "article_#{article.id}")
    format = @file_name_format.presence || "%{date}-%{slug}"
    format % { date: date_prefix, slug: slug }
  end

  def page_basename(page)
    return safe_basename(page.slug.presence || "page_#{page.id}") unless jekyll_mode?

    format = @file_name_format.presence || "%{slug}"
    format % { slug: safe_basename(page.slug.presence || "page_#{page.id}") }
  end

  def jekyll_front_matter_for_article(article)
    base = {
      "layout" => "post",
      "title" => article.title,
      "date" => (article.scheduled_at.presence || article.created_at)&.iso8601,
      "categories" => article.tags.map(&:name),
      "tags" => article.tags.map(&:name),
      "description" => article.description,
      "rables_id" => article.id,
      "rables_type" => "article"
    }

    merge_front_matter_mapping(article, base)
  end

  def jekyll_front_matter_for_page(page)
    base = {
      "layout" => "page",
      "title" => page.title,
      "rables_id" => page.id,
      "rables_type" => "page"
    }

    merge_front_matter_mapping(page, base)
  end

  def merge_front_matter_mapping(record, base)
    @front_matter_mapping.each do |key, source|
      next if key.blank? || source.blank?
      value = record.respond_to?(source) ? record.public_send(source) : nil
      base[key.to_s] = value if value.present?
    end

    base.compact
  end

  def attachment_subdir(record_id, record_type, record_slug)
    return super unless jekyll_mode? && record_slug.present?

    record_slug
  end

  def attachment_public_prefix(record_id, record_type, record_slug, filename)
    return super unless jekyll_mode? && record_slug.present?

    File.join(jekyll_assets_prefix, record_slug, filename)
  end
  def safe_basename(value)
    value = value.to_s.encode("UTF-8", invalid: :replace, undef: :replace, replace: "_")
    value = value.strip
    value = value.gsub(/[\/\\:\*\?"<>\|\x00-\x1F]/, "_")
    value = value.gsub(/[^\p{L}\p{M}\p{N}_.\- ]+/u, "_")
    value = value.tr(" ", "_")
    value = value.gsub(/_+/, "_").gsub(/\A_+|_+\z/, "")
    value = value.gsub(/\A\.+/, "").gsub(/[. ]+\z/, "")
    value.presence || SecureRandom.hex(8)
  end
end
