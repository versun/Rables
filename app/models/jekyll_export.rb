class JekyllExport
  require "fileutils"
  require "reverse_markdown"
  require "yaml"

  attr_reader :setting, :export_path

  def initialize(setting: JekyllSetting.instance)
    @setting = setting
    @export_path = Pathname.new(setting.jekyll_path.to_s)
    @attachment_processor = JekyllAttachmentProcessor.new(setting: setting)
    @static_files_exporter = JekyllStaticFilesExporter.new(setting: setting)
  end

  def generate
    ensure_directories!
    Article.order(:id).includes(:tags).find_each { |article| export_article(article) }
    Page.order(:id).find_each { |page| export_page(page) }
    true
  end

  def export_article(article)
    ensure_directories!

    html = html_for_article(article)
    html = @attachment_processor.process_article_attachments(article, html)
    markdown = ReverseMarkdown.convert(html, unknown_tags: :bypass, github_flavored: true, force_encoding: true).to_s
    markdown = @static_files_exporter.update_references_in_content(markdown)

    front_matter = build_front_matter(article)
    filename = article_filename(article)

    write_markdown_file(posts_dir, filename, front_matter, markdown)
  end

  def export_page(page)
    ensure_directories!

    html = html_for_page(page)
    html = @attachment_processor.process_page_attachments(page, html)
    markdown = ReverseMarkdown.convert(html, unknown_tags: :bypass, github_flavored: true, force_encoding: true).to_s
    markdown = @static_files_exporter.update_references_in_content(markdown)

    front_matter = build_front_matter(page)
    filename = page_filename(page)

    write_markdown_file(pages_dir, filename, front_matter, markdown)
  end

  def preview(item)
    html = item.is_a?(Page) ? html_for_page(item) : html_for_article(item)
    html = item.is_a?(Page) ? @attachment_processor.process_page_attachments(item, html) : @attachment_processor.process_article_attachments(item, html)
    ReverseMarkdown.convert(html, unknown_tags: :bypass, github_flavored: true, force_encoding: true).to_s
  end

  def build_front_matter(item)
    base = {
      "layout" => item.is_a?(Page) ? "page" : "post",
      "title" => item.title.to_s,
      "rables_id" => item.id,
      "rables_type" => item.class.name.underscore
    }

    if item.is_a?(Article)
      base["date"] = article_date(item)&.iso8601
      base["tags"] = item.tags.map(&:name)
      base["categories"] = item.tags.map(&:name)
      base["description"] = item.description.presence
      base["author"] = Setting.first&.author.presence
    end

    mapping = setting.front_matter_mapping_hash
    if item.is_a?(Article) && mapping["permalink"].blank?
      base["permalink"] = article_permalink(item) if article_route_prefix_present?
    end

    mapping.each do |key, source|
      next if key.blank? || source.blank?
      value = extract_mapping_value(item, source)
      base[key.to_s] = value if value.present?
    end

    base.compact
  end

  private

  def ensure_directories!
    raise "Jekyll path is not configured" if export_path.blank?
    raise "Jekyll path does not exist" unless export_path.directory?

    FileUtils.mkdir_p(posts_dir)
    FileUtils.mkdir_p(pages_dir)
  end

  def posts_dir
    export_path.join(setting.posts_directory.to_s)
  end

  def pages_dir
    export_path.join(setting.pages_directory.to_s)
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

  def article_date(article)
    article.scheduled_at.presence || article.created_at
  end

  def article_filename(article)
    date_prefix = article_date(article)&.strftime("%Y-%m-%d") || Time.current.strftime("%Y-%m-%d")
    slug = safe_basename(article.slug.presence || "article_#{article.id}")
    "#{date_prefix}-#{slug}-#{article.id}.md"
  end

  def page_filename(page)
    slug = safe_basename(page.slug.presence || "page_#{page.id}")
    "#{slug}.md"
  end

  def write_markdown_file(directory, filename, front_matter, body)
    yaml = front_matter.to_yaml(line_width: -1)
    yaml = yaml.sub(/\A---\s*\n/, "")

    File.write(
      File.join(directory, filename),
      +"---\n#{yaml}---\n\n#{body.strip}\n"
    )
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

  def extract_mapping_value(item, source)
    case source.to_s
    when "permalink"
      return item.is_a?(Article) ? article_permalink(item) : nil
    end
    return item.public_send(source) if item.respond_to?(source)
    return item.public_send(source.to_s) if item.respond_to?(source.to_s)
    nil
  end

  def article_permalink(article)
    ApplicationController.helpers.public_article_path(article)
  end

  def article_route_prefix_present?
    Rails.application.config.x.article_route_prefix.to_s.strip.present?
  end
end
