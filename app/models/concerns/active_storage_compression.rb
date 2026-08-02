# frozen_string_literal: true

# 自动压缩上传到 Action Text 富文本编辑器中的图片
module ActiveStorageCompression
  extend ActiveSupport::Concern

  included do
    # Action Text 图片上传后压缩
    after_commit :compress_trix_image, on: :create, if: -> { image_attachment? }
  end

  private

  def image_attachment?
    blob.present? && blob.content_type&.start_with?("image/")
  end

  def compress_trix_image
    return unless image_attachment?

    # 只处理富文本编辑器的图片 (embeds)
    return unless name == "embeds"

    # 使用 ruby-vips 压缩图片
    begin
      compress_image
    rescue => e
      Rails.logger.error "压缩图片失败: #{e.message}"
    end
  end

  def compress_image
    # 获取原始文件
    original_path = blob.service.path_for(blob.key)
    return unless File.exist?(original_path)

    # 使用 ruby-vips 压缩
    image = Vips::Image.new_from_file(original_path)

    # Skip animated images: vips would flatten them to a static first frame
    return if animated_image?(image)

    # Bake EXIF orientation into the pixels so the compressed copy renders the same
    image = image.autorot

    # vips infers the output format from the file extension; keep the original one
    extension = blob.filename.extension_with_delimiter.presence || Rack::Mime::MIME_TYPES.invert[blob.content_type]
    return if extension.blank?

    # 压缩质量设置
    quality = 80

    # 保存压缩后的图片
    compressed_path = "#{original_path}.compressed#{extension}"

    begin
      image.write_to_file(compressed_path, Q: quality)

      original_size = File.size(original_path)
      new_size = File.size(compressed_path)

      # Only replace the original when compression actually made the file smaller
      return unless new_size < original_size

      FileUtils.mv(compressed_path, original_path)

      # The stored content changed: refresh checksum (same MD5 base64 as
      # ActiveStorage::Blob#compute_checksum_in_chunks) and byte_size, otherwise
      # blob.open integrity verification fails and variant generation 500s.
      # Then re-analyze so metadata (width/height) matches the new content.
      blob.update!(byte_size: new_size, checksum: compute_checksum(original_path))
      blob.analyze

      Rails.logger.info "图片压缩完成: #{blob.filename} (#{original_size} -> #{new_size} bytes)"
    ensure
      FileUtils.rm_f(compressed_path)
    end
  end

  # vips flattens animated images to their first frame, so leave them untouched
  def animated_image?(image)
    return true if blob.content_type == "image/gif"

    image.get_typeof("n-pages") != 0 && image.get("n-pages") > 1
  end

  def compute_checksum(path)
    File.open(path, "rb") { |io| blob.send(:compute_checksum_in_chunks, io) }
  end
end
