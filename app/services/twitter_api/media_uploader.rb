require "x/media_uploader"
require "tempfile"

module TwitterApi
  # Handles media upload to Twitter/X API
  class MediaUploader
    include HttpRedirectHandler

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
      image_data = extract_image_data(attachable)
      return nil unless image_data

      temp_file = Tempfile.new([ "twitter_image", ".jpg" ], binmode: true)
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
        attachable.download if attachable.content_type&.start_with?("image/")
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

      result.first # Return just the image data
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

    def cleanup_temp_file(temp_file)
      return unless temp_file

      temp_file.close rescue nil
      temp_file.unlink rescue nil
    end
  end
end
