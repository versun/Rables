class JekyllAttachmentProcessor
  attr_reader :setting

  def initialize(setting = nil)
    @setting = setting || JekyllSetting.instance
  end

  # Process all attachments for an article or page
  def process_attachments(item)
    return [] unless item
    return [] unless setting.jekyll_path_valid?

    attachments = []

    # Get content to process
    content = if item.respond_to?(:html_content) && item.html?
                item.html_content.to_s
    else
                item.content.to_s
    end

    # Parse HTML for attachments
    doc = Nokogiri::HTML::DocumentFragment.parse(content)

    # Process images
    doc.css("img").each do |img|
      attachment = process_image(img, item)
      attachments << attachment if attachment
    end

    # Process file links
    doc.css("a[href]").each do |link|
      attachment = process_file_link(link, item)
      attachments << attachment if attachment
    end

    attachments.compact
  end

  # Copy attachments to Jekyll assets directory
  def copy_to_jekyll(attachments)
    return unless setting.jekyll_path_valid?

    copied = []

    attachments.each do |attachment|
      next unless attachment[:blob]

      begin
        target_path = File.join(setting.jekyll_path, attachment[:target_path])
        FileUtils.mkdir_p(File.dirname(target_path))

        # Download and save file
        attachment[:blob].download do |temp_file|
          FileUtils.cp(temp_file.path, target_path)
        end

        copied << attachment
      rescue => e
        Rails.logger.error "Failed to copy attachment: #{e.message}"
      end
    end

    copied
  end

  # Convert content image paths to Jekyll format
  def convert_content_paths(content, item_slug)
    return content unless content.present?

    doc = Nokogiri::HTML::DocumentFragment.parse(content)

    doc.css("img").each do |img|
      src = img["src"]
      next unless src.present?

      # Convert ActiveStorage URLs to Jekyll paths
      if src.include?("/rails/active_storage/")
        filename = extract_filename_from_src(src)
        jekyll_path = "/#{setting.images_directory}/#{item_slug}/#{filename}"
        img["src"] = jekyll_path
      end
    end

    doc.to_html
  end

  # Get attachment target directory for an item
  def attachment_directory(item)
    return nil unless setting.jekyll_path_valid?

    File.join(setting.images_full_path, item.slug)
  end

  private

  def process_image(img, item)
    src = img["src"]
    return nil unless src.present?

    # Handle ActiveStorage attachments
    if src.include?("/rails/active_storage/")
      blob = find_blob_from_src(src)
      return nil unless blob

      filename = blob.filename.to_s
      target_path = File.join(setting.images_directory, item.slug, filename)

      {
        type: :image,
        original_src: src,
        filename: filename,
        alt: img["alt"],
        blob: blob,
        target_path: target_path,
        jekyll_url: "/#{target_path}"
      }
    end
  end

  def process_file_link(link, item)
    href = link["href"]
    return nil unless href.present?

    # Handle ActiveStorage file attachments
    if href.include?("/rails/active_storage/")
      blob = find_blob_from_src(href)
      return nil unless blob

      filename = blob.filename.to_s
      target_path = File.join(setting.assets_directory, "files", item.slug, filename)

      {
        type: :file,
        original_href: href,
        filename: filename,
        blob: blob,
        target_path: target_path,
        jekyll_url: "/#{target_path}"
      }
    end
  end

  def find_blob_from_src(src)
    # Extract blob key or ID from URL
    # URL format: /rails/active_storage/blobs/:signed_id/:filename
    match = src.match(/\/rails\/active_storage\/blobs\/([^\/]+)\//)
    return nil unless match

    signed_id = match[1]
    ActiveStorage::Blob.find_signed(signed_id)
  rescue => e
    Rails.logger.error "Failed to find blob: #{e.message}"
    nil
  end

  def extract_filename_from_src(src)
    # Extract filename from URL
    URI.decode_www_form_component(File.basename(src))
  rescue
    "unknown"
  end
end
