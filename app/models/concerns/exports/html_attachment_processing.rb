module Exports
  module HtmlAttachmentProcessing
    require "fileutils"
    require "json"
    require "nokogiri"
    require "open-uri"
    require "securerandom"

    def process_html_content(html, record_id:, record_type:, record_slug: nil)
      html = html.to_s
      return "" if html.blank?

      doc = Nokogiri::HTML.fragment(html)

      doc.css("action-text-attachment").each do |attachment|
        process_attachment_element(attachment, record_id, record_type, record_slug)
      end

      doc.css("figure[data-trix-attachment]").each do |figure|
        process_figure_element(figure, record_id, record_type, record_slug)
      end

      doc.css("img").each do |img|
        process_image_element(img, record_id, record_type, record_slug)
      end

      doc.to_html
    end

    private

    def process_attachment_element(attachment, record_id, record_type, record_slug = nil)
      content_type = attachment["content-type"]
      original_url = attachment["url"]
      filename = attachment["filename"]

      return unless content_type&.start_with?("image/") && original_url.present? && filename.present?

      new_url = download_and_save_attachment(original_url, filename, record_id, record_type, record_slug)
      return unless new_url

      # Use caption attribute if available, otherwise use filename
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
      Rails.event.notify("exports.attachment_element_failed", component: self.class.name, error: e.message, level: "error")
    end

    def process_figure_element(figure, record_id, record_type, record_slug = nil)
      attachment_data = JSON.parse(figure["data-trix-attachment"]) rescue nil
      return unless attachment_data

      content_type = attachment_data["contentType"]
      original_url = attachment_data["url"]
      filename = attachment_data["filename"] || File.basename(original_url.to_s)

      return unless content_type&.start_with?("image/") && original_url.present?

      new_url = download_and_save_attachment(original_url, filename, record_id, record_type, record_slug)
      return unless new_url

      attachment_data["url"] = new_url
      figure["data-trix-attachment"] = attachment_data.to_json

      # Extract caption from trix attributes if available
      trix_attributes = JSON.parse(figure["data-trix-attributes"]) rescue {}
      alt_text = trix_attributes["caption"].presence || filename.to_s

      img = figure.at_css("img")
      if img
        img["src"] = new_url
        img["alt"] = alt_text if img["alt"].blank?
      else
        # Create img element if it doesn't exist (to_trix_html outputs empty figure)
        img_node = Nokogiri::XML::Node.new("img", figure.document)
        img_node["src"] = new_url
        img_node["alt"] = alt_text
        figure.add_child(img_node)
      end
    rescue => e
      Rails.event.notify("exports.figure_element_failed", component: self.class.name, error: e.message, level: "error")
    end

    def process_image_element(img, record_id, record_type, record_slug = nil)
      original_url = img["src"]
      return unless original_url.present?

      # 处理 ActiveStorage URL
      if original_url.include?("/rails/active_storage/blobs/") ||
         original_url.include?("/rails/active_storage/representations/")
        blob = extract_blob_from_url(original_url)
        return unless blob

        filename = blob.filename.to_s
        new_url = download_and_save_attachment(original_url, filename, record_id, record_type)
        img["src"] = new_url if new_url
      # 处理外部图片 URL (RemoteImage)
      elsif original_url.start_with?("http")
        filename = extract_filename_from_url(original_url)
        new_url = download_and_save_attachment(original_url, filename, record_id, record_type, record_slug)
        img["src"] = new_url if new_url
      end
    rescue => e
      Rails.event.notify("exports.image_element_failed", component: self.class.name, error: e.message, level: "error")
    end

    def extract_filename_from_url(url)
      # 从 URL 中提取文件名
      uri = URI.parse(url)
      path = uri.path
      filename = File.basename(path)
      # 如果没有文件名或扩展名，生成一个带扩展名的文件名
      if filename.blank? || !filename.include?(".")
        ext = detect_extension_from_content_type(url) || ".jpg"
        "#{SecureRandom.hex(8)}#{ext}"
      else
        filename
      end
    rescue => e
      "#{SecureRandom.hex(8)}.jpg"
    end

    def detect_extension_from_content_type(url)
      # 尝试通过 HEAD 请求获取 Content-Type
      require "net/http"
      Net::HTTP.start(URI.parse(url).host, use_ssl: true) do |http|
        response = http.head(url)
        content_type = response["Content-Type"]
        if content_type.present?
          case content_type
          when "image/jpeg" then ".jpg"
          when "image/png" then ".png"
          when "image/gif" then ".gif"
          when "image/webp" then ".webp"
          when "image/svg+xml" then ".svg"
          when "image/bmp" then ".bmp"
          else nil
          end
        end
      end
    rescue => e
      nil
    end

    def download_and_save_attachment(original_url, filename, record_id, record_type, record_slug = nil)
      record_attachments_dir = attachment_directory_for(record_id, record_type, record_slug)
      FileUtils.mkdir_p(record_attachments_dir)

      new_filename = "#{SecureRandom.hex(8)}_#{filename}"
      local_path = File.join(record_attachments_dir, new_filename)

      blob = extract_blob_from_url(original_url)
      if blob
        File.open(local_path, "wb") { |f| f.write(blob.download) }
      else
        full_url = build_full_url(original_url)
        URI.open(full_url) do |remote_file|
          File.open(local_path, "wb") do |local_file|
            local_file.write(remote_file.read)
          end
        end
      end

      attachment_public_prefix(record_id, record_type, record_slug, new_filename)
    rescue => e
      Rails.event.notify("exports.attachment_download_failed", component: self.class.name, url: original_url, error: e.message, level: "error")
      nil
    end

    def attachment_directory_for(record_id, record_type, record_slug)
      File.join(attachments_dir, attachment_subdir(record_id, record_type, record_slug))
    end

    def attachment_subdir(record_id, record_type, record_slug)
      "#{record_type}_#{record_id}"
    end

    def attachment_public_prefix(record_id, record_type, record_slug, filename)
      "attachments/#{attachment_subdir(record_id, record_type, record_slug)}/#{filename}"
    end

    def build_full_url(original_url)
      original_url = original_url.to_s
      return original_url if original_url.start_with?("http")

      base_url = if defined?(Setting) && Setting.respond_to?(:table_exists?) && Setting.table_exists?
                   Setting.first&.url.presence
      end
      base_url = base_url.presence || ENV["BASE_URL"].presence || "http://localhost:3000"
      base_url = base_url.chomp("/")

      original_url.start_with?("/") ? "#{base_url}#{original_url}" : "#{base_url}/#{original_url}"
    end

    def extract_blob_from_url(url)
      match = url.match(/\/rails\/active_storage\/(?:blobs|representations)\/redirect\/([^\/]+)/)
      return nil unless match

      signed_id = match[1]
      ActiveStorage::Blob.find_signed(signed_id)
    rescue => e
      Rails.event.notify("exports.blob_not_found", component: self.class.name, signed_id: signed_id, error: e.message, level: "error")
      nil
    end
  end
end
