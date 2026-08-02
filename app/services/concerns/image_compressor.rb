# 共享的图片压缩逻辑（基于 libvips），用于社交媒体上传前把超大图片压到平台限制内
module ImageCompressor
  extend ActiveSupport::Concern

  STARTING_JPEG_QUALITY = 85
  MIN_IMAGE_DIMENSION = 100

  private

  # 压缩成功返回 [ jpeg_data, "image/jpeg" ]，无法压到 max_size 以内或出错时返回 nil
  def compress_image(image_data, content_type, max_size, temp_dir:)
    original_path = nil
    compressed_path = nil

    begin
      Rails.event.notify "image_compressor.resizing_image",
        level: "info",
        component: self.class.name,
        original_size: image_data.bytesize,
        max_size: max_size

      FileUtils.mkdir_p(temp_dir)

      extension = content_type.to_s.split("/").last.to_s.downcase
      extension = "jpg" if extension.blank? || extension == "jpeg"

      original_path = temp_dir.join("original_#{SecureRandom.hex(8)}.#{extension}")
      compressed_path = temp_dir.join("compressed_#{SecureRandom.hex(8)}.jpg")
      File.binwrite(original_path, image_data)

      image = Vips::Image.new_from_file(original_path.to_s)
      current_image = normalize_image_for_jpeg(image)
      quality = STARTING_JPEG_QUALITY

      loop do
        current_image.write_to_file(compressed_path.to_s, Q: quality, strip: true)
        compressed_size = File.size(compressed_path)

        if compressed_size <= max_size
          result_data = File.binread(compressed_path)

          Rails.event.notify "image_compressor.image_resized",
            level: "info",
            component: self.class.name,
            original_size: image_data.bytesize,
            final_size: result_data.bytesize,
            quality: quality

          return [ result_data, "image/jpeg" ]
        end

        if quality > 50
          quality -= 10
          next
        end

        scale_factor = next_scale_factor(current_image, compressed_size, max_size)
        break unless scale_factor

        current_image = current_image.resize(scale_factor)
        quality = STARTING_JPEG_QUALITY
      end

      Rails.event.notify "image_compressor.image_too_large_after_resize",
        level: "warn",
        component: self.class.name,
        original_size: image_data.bytesize
      nil
    rescue => e
      Rails.event.notify "image_compressor.resize_failed",
        level: "error",
        component: self.class.name,
        error_message: e.message,
        backtrace: e.backtrace.first(5).join("\n")
      nil
    ensure
      File.delete(original_path) if original_path && File.exist?(original_path)
      File.delete(compressed_path) if compressed_path && File.exist?(compressed_path)
    end
  end

  def next_scale_factor(image, compressed_size, max_size)
    scale_factor = Math.sqrt(max_size.to_f / compressed_size) * 0.95
    scale_factor = [ scale_factor, 0.9 ].min
    scale_factor = [ scale_factor, 0.5 ].max

    new_width = (image.width * scale_factor).to_i
    new_height = (image.height * scale_factor).to_i

    return nil if new_width < MIN_IMAGE_DIMENSION || new_height < MIN_IMAGE_DIMENSION

    scale_factor
  end

  def normalize_image_for_jpeg(image)
    normalized_image = image.autorot

    return normalized_image unless normalized_image.has_alpha?

    normalized_image.flatten(background: jpeg_background_for(normalized_image))
  end

  def jpeg_background_for(image)
    Array.new([ image.bands - 1, 1 ].max, 255)
  end
end
