class JekyllAttachmentProcessor
  require "fileutils"
  require "json"
  require "nokogiri"
  require "open-uri"
  require "securerandom"

  def initialize(setting:)
    @setting = setting
    @jekyll_path = Pathname.new(@setting.jekyll_path.to_s)
  end

  def process_article_attachments(article, html)
    process_html(html, article.slug.presence || "article-#{article.id}")
  end

  def process_page_attachments(page, html)
    process_html(html, page.slug.presence || "page-#{page.id}")
  end

  def convert_image_paths(content, slug)
    process_html(content, slug)
  end

  def extract_images_from_html(html)
    doc = Nokogiri::HTML.fragment(html.to_s)
    doc.css("img").map { |img| img["src"] }.compact
  end

  private

  def process_html(html, slug)
    return "" if html.blank?

    doc = Nokogiri::HTML.fragment(html.to_s)

    doc.css("action-text-attachment").each do |attachment|
      process_attachment_element(attachment, slug)
    end

    doc.css("figure[data-trix-attachment]").each do |figure|
      process_figure_element(figure, slug)
    end

    doc.css("img").each do |img|
      process_image_element(img, slug)
    end

    doc.to_html
  end

  def process_attachment_element(attachment, slug)
    content_type = attachment["content-type"]
    original_url = attachment["url"]
    filename = attachment["filename"]

    return unless content_type&.start_with?("image/") && original_url.present? && filename.present?

    new_url = copy_to_jekyll_assets(original_url, filename, slug)
    return unless new_url

    alt_text = attachment["caption"].presence || filename.to_s

    img = attachment.at_css("img")
    if img
      attachment["url"] = new_url
      img["src"] = new_url
      img["alt"] = alt_text if img["alt"].blank?
    else
      attachment.replace(%(<img src="#{new_url}" alt="#{alt_text}">))
    end
  rescue => e
    Rails.event.notify("jekyll_attachment_processor.attachment_failed",
      component: self.class.name,
      error: e.message,
      level: "error")
  end

  def process_figure_element(figure, slug)
    attachment_data = JSON.parse(figure["data-trix-attachment"]) rescue nil
    return unless attachment_data

    content_type = attachment_data["contentType"]
    original_url = attachment_data["url"]
    filename = attachment_data["filename"] || File.basename(original_url.to_s)

    return unless content_type&.start_with?("image/") && original_url.present?

    new_url = copy_to_jekyll_assets(original_url, filename, slug)
    return unless new_url

    attachment_data["url"] = new_url
    figure["data-trix-attachment"] = attachment_data.to_json

    trix_attributes = JSON.parse(figure["data-trix-attributes"]) rescue {}
    alt_text = trix_attributes["caption"].presence || filename.to_s

    img = figure.at_css("img")
    if img
      img["src"] = new_url
      img["alt"] = alt_text if img["alt"].blank?
    else
      img_node = Nokogiri::XML::Node.new("img", figure.document)
      img_node["src"] = new_url
      img_node["alt"] = alt_text
      figure.add_child(img_node)
    end
  rescue => e
    Rails.event.notify("jekyll_attachment_processor.figure_failed",
      component: self.class.name,
      error: e.message,
      level: "error")
  end

  def process_image_element(img, slug)
    original_url = img["src"]
    return unless original_url.present?

    if active_storage_url?(original_url)
      blob = extract_blob_from_url(original_url)
      return unless blob

      new_url = copy_blob_to_assets(blob, slug)
      img["src"] = new_url if new_url
    elsif original_url.start_with?("http")
      return unless @setting.download_remote_images

      filename = File.basename(URI.parse(original_url).path.presence || "")
      filename = generate_filename(original_url) if filename.blank? || !filename.include?(".")
      new_url = copy_to_jekyll_assets(original_url, filename, slug)
      img["src"] = new_url if new_url
    end
  rescue => e
    Rails.event.notify("jekyll_attachment_processor.image_failed",
      component: self.class.name,
      error: e.message,
      level: "error")
  end

  def active_storage_url?(url)
    url.include?("/rails/active_storage/blobs/") ||
      url.include?("/rails/active_storage/representations/")
  end

  def copy_blob_to_assets(blob, slug)
    filename = blob.filename.to_s
    target_dir = assets_target_dir(slug)
    FileUtils.mkdir_p(target_dir)

    sanitized = sanitize_filename(filename)
    local_path = File.join(target_dir, sanitized)
    File.open(local_path, "wb") { |f| f.write(blob.download) }

    public_asset_path(slug, sanitized)
  end

  def copy_to_jekyll_assets(original_url, filename, slug)
    target_dir = assets_target_dir(slug)
    FileUtils.mkdir_p(target_dir)

    sanitized = sanitize_filename(filename)
    local_path = File.join(target_dir, sanitized)

    if active_storage_url?(original_url)
      blob = extract_blob_from_url(original_url)
      return nil unless blob
      File.open(local_path, "wb") { |f| f.write(blob.download) }
    else
      URI.open(original_url) do |remote_file|
        File.open(local_path, "wb") { |local| local.write(remote_file.read) }
      end
    end

    public_asset_path(slug, sanitized)
  rescue => e
    Rails.event.notify("jekyll_attachment_processor.copy_failed",
      component: self.class.name,
      url: original_url,
      error: e.message,
      level: "error")
    nil
  end

  def assets_target_dir(slug)
    images_dir = @setting.images_directory.to_s.strip
    images_dir = images_dir.delete_prefix("/")
    @jekyll_path.join(images_dir, slug)
  end

  def public_asset_path(slug, filename)
    images_dir = @setting.images_directory.to_s.strip
    images_dir = images_dir.delete_prefix("/")
    "/#{images_dir}/#{slug}/#{filename}"
  end

  def sanitize_filename(filename)
    filename.to_s.encode("UTF-8", invalid: :replace, undef: :replace, replace: "_")
            .gsub(/[\/\\:\*\?"<>\|\x00-\x1F]/, "_")
  end

  def generate_filename(url)
    ext = File.extname(URI.parse(url).path.to_s)
    ext = ".jpg" if ext.blank?
    "#{SecureRandom.hex(8)}#{ext}"
  rescue URI::InvalidURIError
    "#{SecureRandom.hex(8)}.jpg"
  end

  def extract_blob_from_url(url)
    match = url.match(/\/rails\/active_storage\/(?:blobs|representations)\/redirect\/([^\/]+)/)
    return nil unless match

    signed_id = match[1]
    ActiveStorage::Blob.find_signed(signed_id)
  rescue => e
    Rails.event.notify("jekyll_attachment_processor.blob_not_found",
      component: self.class.name,
      signed_id: signed_id,
      error: e.message,
      level: "error")
    nil
  end
end
