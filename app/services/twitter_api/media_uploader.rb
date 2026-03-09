require "x/media_uploader"
require "fileutils"
require "securerandom"
require "tempfile"

module TwitterApi
  # Handles media upload to Twitter/X API
  class MediaUploader
    include HttpRedirectHandler

    MAX_IMAGE_SIZE = 5.megabytes
    STARTING_JPEG_QUALITY = 85
    MIN_JPEG_QUALITY = 50
    JPEG_QUALITY_STEP = 10
    MAX_RESIZE_SCALE = 0.9
    MIN_RESIZE_SCALE = 0.5
    MIN_IMAGE_DIMENSION = 100
    JPEG_BACKGROUND_VALUE = 255
    CONTENT_TYPE_EXTENSIONS = {
      "image/gif" => "gif",
      "image/jpeg" => "jpg",
      "image/png" => "png",
      "image/webp" => "webp"
    }.freeze

    def initialize(settings)
      @settings = settings
    end

    # Upload an image attachment and return media ID
    def upload(client, attachable)
      return nil unless attachable

      temp_file = nil
      begin
        temp_file = create_temp_file(attachable)
        return nil unless temp_file

        upload_to_twitter(client, temp_file.path)
      rescue => e
        Rails.event.notify "twitter_service.upload_image_error",
          level: "error",
          component: "Twitter::MediaUploader",
          error_message: e.message
        nil
      ensure
        cleanup_temp_file(temp_file)
      end
    end

    private

    def upload_to_twitter(client, file_path)
      return nil unless File.exist?(file_path)

      begin
        response = X::MediaUploader.upload(
          client: client,
          file_path: file_path,
          media_category: "tweet_image"
        )

        Rails.event.notify "twitter_service.media_upload_response",
          level: "info",
          component: "Twitter::MediaUploader",
          response: response.inspect

        if response && response["id"].present?
          media_id = response["id"].to_s
          Rails.event.notify "twitter_service.media_uploaded",
            level: "info",
            component: "Twitter::MediaUploader",
            media_id: media_id
          media_id
        else
          Rails.event.notify "twitter_service.media_upload_failed",
            level: "error",
            component: "Twitter::MediaUploader",
            response: response.inspect
          nil
        end
      rescue => e
        Rails.event.notify "twitter_service.media_upload_error",
          level: "error",
          component: "Twitter::MediaUploader",
          error_message: e.message,
          backtrace: e.backtrace.first(5).join("\n")
        nil
      end
    end

    def create_temp_file(attachable)
      image_data, content_type = extract_image_data(attachable)
      return nil unless image_data

      image_data, content_type = resize_image_if_needed(image_data, content_type)
      return nil unless image_data

      temp_dir = Rails.root.join("tmp", "twitter_uploads")
      FileUtils.mkdir_p(temp_dir)

      temp_file = Tempfile.new([ "twitter_image", ".#{extension_for_content_type(content_type)}" ], temp_dir)
      temp_file.binmode
      temp_file.write(image_data)
      temp_file.rewind
      temp_file
    rescue => e
      Rails.event.notify "twitter_service.temp_file_error",
        level: "error",
        component: "Twitter::MediaUploader",
        error_message: e.message
      nil
    end

    def extract_image_data(attachable)
      case attachable
      when ActiveStorage::Blob
        [ attachable.download, normalize_content_type(attachable.content_type) ] if attachable.content_type&.start_with?("image/")
      when ->(obj) { obj.class.name == "ActionText::Attachables::RemoteImage" }
        download_remote_image(attachable)
      else
        nil
      end
    end

    def download_remote_image(remote_image)
      return nil unless remote_image.respond_to?(:url)

      image_url = remote_image.url
      return nil unless image_url.present?

      result = download_remote_image_with_redirect(image_url)
      return nil unless result

      image_data, content_type = result
      [ image_data, normalize_content_type(content_type) ]
    end

    def log_redirect(redirect_uri)
      Rails.event.notify "twitter_service.following_redirect",
        level: "info",
        component: "Twitter::MediaUploader",
        redirect_uri: redirect_uri.to_s
    end

    def log_download_error(error, url)
      Rails.event.notify "twitter_service.download_remote_image_error",
        level: "error",
        component: "Twitter::MediaUploader",
        error_message: error.message,
        url: url
    end

    def normalize_content_type(content_type)
      normalized = content_type.to_s.split(";").first.to_s.strip.downcase
      normalized.present? ? normalized : "image/jpeg"
    end

    def extension_for_content_type(content_type)
      CONTENT_TYPE_EXTENSIONS[normalize_content_type(content_type)] || "jpg"
    end

    def resize_image_if_needed(image_data, content_type)
      normalized_content_type = normalize_content_type(content_type)
      return [ image_data, normalized_content_type ] if image_data.bytesize <= MAX_IMAGE_SIZE

      original_path = nil
      compressed_path = nil

      begin
        Rails.event.notify "twitter_service.resizing_image",
          level: "info",
          component: "Twitter::MediaUploader",
          original_size: image_data.bytesize,
          max_size: MAX_IMAGE_SIZE

        temp_dir = Rails.root.join("tmp", "twitter_uploads")
        FileUtils.mkdir_p(temp_dir)

        original_path = temp_dir.join("original_#{SecureRandom.hex(8)}.#{extension_for_content_type(normalized_content_type)}")
        compressed_path = temp_dir.join("compressed_#{SecureRandom.hex(8)}.jpg")
        File.binwrite(original_path, image_data)

        image = Vips::Image.new_from_file(original_path.to_s)
        current_image = normalize_image_for_jpeg(image)
        quality = STARTING_JPEG_QUALITY

        loop do
          current_image.write_to_file(compressed_path.to_s, Q: quality, strip: true)
          compressed_size = File.size(compressed_path)

          if compressed_size <= MAX_IMAGE_SIZE
            result_data = File.binread(compressed_path)

            Rails.event.notify "twitter_service.image_resized",
              level: "info",
              component: "Twitter::MediaUploader",
              original_size: image_data.bytesize,
              final_size: result_data.bytesize,
              quality: quality

            return [ result_data, "image/jpeg" ]
          end

          if quality > MIN_JPEG_QUALITY
            quality -= JPEG_QUALITY_STEP
            next
          end

          scale_factor = next_scale_factor(current_image, compressed_size)
          break unless scale_factor

          current_image = current_image.resize(scale_factor)
          quality = STARTING_JPEG_QUALITY
        end

        Rails.event.notify "twitter_service.image_too_large_after_resize",
          level: "warn",
          component: "Twitter::MediaUploader",
          original_size: image_data.bytesize
        nil
      rescue => e
        Rails.event.notify "twitter_service.resize_failed",
          level: "error",
          component: "Twitter::MediaUploader",
          error_message: e.message,
          backtrace: e.backtrace.first(5).join("\n")
        nil
      ensure
        File.delete(original_path) if original_path && File.exist?(original_path)
        File.delete(compressed_path) if compressed_path && File.exist?(compressed_path)
      end
    end

    def next_scale_factor(image, compressed_size)
      return nil unless image.respond_to?(:width) && image.respond_to?(:height)

      scale_factor = Math.sqrt(MAX_IMAGE_SIZE.to_f / compressed_size) * 0.95
      scale_factor = [ scale_factor, MAX_RESIZE_SCALE ].min
      scale_factor = [ scale_factor, MIN_RESIZE_SCALE ].max

      new_width = (image.width * scale_factor).to_i
      new_height = (image.height * scale_factor).to_i

      return nil if new_width < MIN_IMAGE_DIMENSION || new_height < MIN_IMAGE_DIMENSION

      scale_factor
    end

    def normalize_image_for_jpeg(image)
      normalized_image = image.respond_to?(:autorot) ? image.autorot : image

      return normalized_image unless normalized_image.respond_to?(:has_alpha?) && normalized_image.has_alpha?

      normalized_image.flatten(background: jpeg_background_for(normalized_image))
    end

    def jpeg_background_for(image)
      Array.new([ image.bands - 1, 1 ].max, JPEG_BACKGROUND_VALUE)
    end

    def cleanup_temp_file(temp_file)
      return unless temp_file

      temp_file.close rescue nil
      temp_file.unlink rescue nil
    end
  end
end
