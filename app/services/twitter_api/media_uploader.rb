require "x/media_uploader"
require "fileutils"
require "securerandom"
require "tempfile"

module TwitterApi
  # Handles media upload to Twitter/X API
  class MediaUploader
    include HttpRedirectHandler
    include ImageCompressor
    include TransientNetworkErrors

    MAX_IMAGE_SIZE = 5.megabytes
    MAX_GIF_SIZE = 15.megabytes
    IMAGE_CHUNK_SIZE_MB = 5
    GIF_CHUNK_SIZE_MB = 15
    CONTENT_TYPE_EXTENSIONS = {
      "image/bmp" => "bmp",
      "image/gif" => "gif",
      "image/jpeg" => "jpg",
      "image/pjpeg" => "jpg",
      "image/png" => "png",
      "image/tiff" => "tiff",
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
        raise if transient_network_error?(e)

        nil
      ensure
        cleanup_temp_file(temp_file)
      end
    end

    private

    def upload_to_twitter(client, file_path)
      return nil unless File.exist?(file_path)

      begin
        media_category = media_category_for_file(file_path)
        media_type = media_type_for_file(file_path, media_category)
        response = X::MediaUploader.chunked_upload(
          client: client,
          file_path: file_path,
          media_category: media_category,
          media_type: media_type,
          chunk_size_mb: chunk_size_mb_for(media_category)
        )
        response = await_processing_if_needed(client, response)

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
        error_message = permission_error?(e) ? normalize_upload_error_message(e.message) : e.message

        Rails.event.notify "twitter_service.media_upload_error",
          level: "error",
          component: "Twitter::MediaUploader",
          error_message: error_message,
          backtrace: e.backtrace.first(5).join("\n")
        raise if transient_network_error?(e)

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
      when ActionText::Attachables::RemoteImage, RemoteImageWrapper
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
      return [ image_data, normalized_content_type ] if image_data.bytesize <= max_upload_size_for(normalized_content_type)

      if gif_content_type?(normalized_content_type)
        Rails.event.notify "twitter_service.gif_too_large",
          level: "warn",
          component: "Twitter::MediaUploader",
          original_size: image_data.bytesize,
          max_size: MAX_GIF_SIZE
        return nil
      end

      compress_image(image_data, normalized_content_type, MAX_IMAGE_SIZE, temp_dir: Rails.root.join("tmp", "twitter_uploads"))
    end

    def await_processing_if_needed(client, response)
      state = response&.dig("processing_info", "state")
      return response if state.blank? || state == "succeeded"

      Rails.event.notify "twitter_service.awaiting_media_processing",
        level: "info",
        component: "Twitter::MediaUploader",
        media_id: response["id"],
        processing_state: state

      X::MediaUploader.await_processing!(client: client, media: response) || response
    end

    def media_category_for_file(file_path)
      File.extname(file_path).downcase == ".gif" ? "tweet_gif" : "tweet_image"
    end

    def media_type_for_file(file_path, media_category)
      case File.extname(file_path).downcase
      when ".gif"
        "image/gif"
      when ".jpg", ".jpeg"
        "image/jpeg"
      when ".png"
        "image/png"
      when ".webp"
        "image/webp"
      when ".bmp"
        "image/bmp"
      when ".tif", ".tiff"
        "image/tiff"
      else
        X::MediaUploader.infer_media_type(file_path, media_category)
      end
    end

    def chunk_size_mb_for(media_category)
      media_category == "tweet_gif" ? GIF_CHUNK_SIZE_MB : IMAGE_CHUNK_SIZE_MB
    end

    def max_upload_size_for(content_type)
      gif_content_type?(content_type) ? MAX_GIF_SIZE : MAX_IMAGE_SIZE
    end

    def gif_content_type?(content_type)
      normalize_content_type(content_type) == "image/gif"
    end

    def normalize_upload_error_message(message)
      return message unless missing_media_scope?(message)

      "#{message}. Re-authorize with X to grant the media.write scope, then try uploading images again."
    end

    def permission_error?(error)
      (defined?(X::Forbidden) && error.is_a?(X::Forbidden)) ||
        (defined?(X::Unauthorized) && error.is_a?(X::Unauthorized)) ||
        missing_media_scope?(error.message)
    end

    def missing_media_scope?(message)
      normalized = message.to_s.downcase
      normalized.include?("media.write") ||
        (normalized.include?("scope") && normalized.include?("media")) ||
        (normalized.include?("permission") && normalized.include?("media")) ||
        (normalized.include?("not authorized") && normalized.include?("media"))
    end

    def cleanup_temp_file(temp_file)
      return unless temp_file

      temp_file.close rescue nil
      temp_file.unlink rescue nil
    end
  end
end
