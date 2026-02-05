require "net/http"
require "uri"

class MastodonService
  include ContentBuilder

  def initialize
    @settings = Crosspost.mastodon
  end

  def verify(settings)
    if settings[:access_token].blank?
      return { success: false, error: "Access token are required" }
    end

    begin
      uri = mastodon_api_uri("/api/v1/accounts/verify_credentials", settings[:server_url])
      return { success: false, error: "Server URL must be a valid http(s) URL" } unless uri

      http = Net::HTTP.new(uri.host, uri.port)
      http.use_ssl = uri.scheme == "https"

      request = Net::HTTP::Get.new(uri)
      request["Authorization"] = "Bearer #{settings[:access_token]}"

      response = http.request(request)

      response.is_a?(Net::HTTPSuccess) ?
        { success: true } :
        { success: false, error: "Verification failed: #{response.code} #{response.message}" }
    rescue => e
      { success: false, error: "Mastodon verification failed: #{e}" }
    end
  end


  def post(article)
    return unless @settings&.enabled?
    max_length = @settings.effective_max_characters || 500
    status_text = build_content(article: article, max_length: max_length)

    # 获取文章所有图片（Mastodon最多支持4张）
    images = article.all_image_attachments(4)
    Rails.event.notify "mastodon_service.images_count",
      level: "info",
      component: "MastodonService",
      count: images.size

    media_ids = []
    images.each do |image|
      media_id = upload_image(image)
      if media_id
        media_ids << media_id
        Rails.event.notify "mastodon_service.image_uploaded",
          level: "info",
          component: "MastodonService",
          media_id: media_id,
          total_uploaded: media_ids.size
      else
        Rails.event.notify "mastodon_service.image_upload_failed",
          level: "warn",
          component: "MastodonService"
      end
    end

    uri = mastodon_api_uri("/api/v1/statuses")
    return nil unless uri

    begin
      http = Net::HTTP.new(uri.host, uri.port)
      http.use_ssl = uri.scheme == "https"

      request = Net::HTTP::Post.new(uri)
      form_data = [
        [ "status", status_text ],
        [ "visibility", "public" ]
      ]

      # 添加所有媒体ID
      media_ids.each do |media_id|
        form_data << [ "media_ids[]", media_id ]
      end

      request.set_form_data(form_data)
      request["Authorization"] = "Bearer #{@settings.access_token}"

      response = http.request(request)

      if response.is_a?(Net::HTTPSuccess)
        json_response = JSON.parse(response.body)
        post_url = json_response["url"]
        ActivityLog.log!(
          action: :posted,
          target: :crosspost,
          level: :info,
          title: article.title,
          slug: article.slug,
          platform: "mastodon",
          url: post_url
        )

        post_url
      else
        error_message = "#{response.code} #{response.message}".strip
        ActivityLog.log!(
          action: :failed,
          target: :crosspost,
          level: :error,
          title: article.title,
          slug: article.slug,
          platform: "mastodon",
          error: error_message
        )
        nil
      end
    rescue => e
      ActivityLog.log!(
        action: :failed,
        target: :crosspost,
        level: :error,
        title: article.title,
        slug: article.slug,
        platform: "mastodon",
        error: e.message
      )
      nil
    end
  end

  # Fetch comments (replies) for a Mastodon status
  # Returns a hash with :comments array and :rate_limit info
  def fetch_comments(status_url)
    default_response = { comments: [], rate_limit: nil }
    return default_response unless @settings&.enabled?
    return default_response if status_url.blank?

    begin
      # Extract status ID from URL (e.g., https://mastodon.social/@username/123456789)
      status_id = extract_status_id_from_url(status_url)
      return default_response unless status_id

      # Call Mastodon API to get context (ancestors and descendants/replies)
      uri = mastodon_api_uri("/api/v1/statuses/#{status_id}/context")
      return default_response unless uri

      http = Net::HTTP.new(uri.host, uri.port)
      http.use_ssl = uri.scheme == "https"

      request = Net::HTTP::Get.new(uri)
      request["Authorization"] = "Bearer #{@settings.access_token}"

      response = http.request(request)

      # Parse rate limit headers
      rate_limit_info = parse_rate_limit_headers(response)
      log_rate_limit_status(rate_limit_info)

      # Handle rate limit exceeded
      if response.code == "429"
        handle_rate_limit_exceeded(rate_limit_info)
        return { comments: [], rate_limit: rate_limit_info }
      end

      if response.is_a?(Net::HTTPSuccess)
        context_data = JSON.parse(response.body)
        descendants = context_data["descendants"] || []
        original_status_id = status_id.to_s

        # Parse descendants into comment data
        comments = descendants.map do |reply|
          {
            external_id: reply["id"],
            author_name: reply["account"]["display_name"].presence || reply["account"]["username"],
            author_username: reply["account"]["acct"],
            author_avatar_url: reply["account"]["avatar"],
            content: reply["content"],
            published_at: Time.parse(reply["created_at"]),
            url: reply["url"],
            # Extract parent external_id: if in_reply_to_id exists and is not the original post, use it
            parent_external_id: reply["in_reply_to_id"].present? && reply["in_reply_to_id"] != original_status_id ? reply["in_reply_to_id"] : nil
          }
        end

        { comments: comments, rate_limit: rate_limit_info }
      else
        Rails.event.notify "mastodon_service.fetch_comments_failed",
          level: "error",
          component: "MastodonService",
          response_code: response.code,
          response_body: response.body[0..200]
        { comments: [], rate_limit: rate_limit_info }
      end
    rescue => e
      Rails.event.notify "mastodon_service.fetch_comments_error",
        level: "error",
        component: "MastodonService",
        error_message: e.message,
        backtrace: e.backtrace.join("\n")
      default_response
    end
  end

  private

  # def create_client
  #   Mastodon::REST::Client.new(
  #     base_url: @settings[:server_url],
  #     bearer_token: @settings.access_token
  #   )
  # end

  def upload_image(attachable)
    Rails.event.notify "mastodon_service.upload_image_started",
      level: "info",
      component: "MastodonService",
      attachable_type: attachable.class.to_s
    return nil unless attachable

    begin
      image_data = nil
      filename = "image.jpg"
      content_type = "image/jpeg"

      # Handle ActiveStorage::Blob
      if attachable.is_a?(ActiveStorage::Blob) && attachable.content_type&.start_with?("image/")
        Rails.event.notify "mastodon_service.processing_blob",
          level: "info",
          component: "MastodonService",
          storage_type: "ActiveStorage::Blob"
        image_data = attachable.download
        filename = attachable.filename.to_s if attachable.respond_to?(:filename)
        content_type = attachable.content_type
      # Handle RemoteImage
      elsif attachable.class.name == "ActionText::Attachables::RemoteImage"
        Rails.event.notify "mastodon_service.processing_remote_image",
          level: "info",
          component: "MastodonService",
          storage_type: "RemoteImage"
        image_url = attachable.try(:url)
        Rails.event.notify "mastodon_service.remote_image_url",
          level: "info",
          component: "MastodonService",
          image_url: image_url

        if image_url.present?
          # Download remote image
          image_data, content_type = download_remote_image(image_url)
          # Safely extract filename from URL
          begin
            filename = File.basename(URI.parse(image_url).path)
            # Ensure we have a valid filename
            filename = "image.jpg" if filename.blank? || filename == "/"
          rescue URI::InvalidURIError => e
            Rails.event.notify "mastodon_service.invalid_url",
              level: "warn",
              component: "MastodonService",
              image_url: image_url,
              error_message: e.message
            filename = "image.jpg"
          end
        else
          Rails.event.notify "mastodon_service.remote_image_no_url",
            level: "warn",
            component: "MastodonService"
          return nil
        end
      else
        Rails.event.notify "mastodon_service.unknown_attachable_type",
          level: "warn",
          component: "MastodonService",
          attachable_type: attachable.class.to_s
        return nil
      end

      return nil unless image_data

      # Upload to Mastodon
      uri = mastodon_api_uri("/api/v2/media")
      return nil unless uri

      http = Net::HTTP.new(uri.host, uri.port)
      http.use_ssl = uri.scheme == "https"

      request = Net::HTTP::Post.new(uri)
      request["Authorization"] = "Bearer #{@settings.access_token}"

      # 创建multipart表单数据
      boundary = "----WebKitFormBoundary#{SecureRandom.hex(16)}"
      request["Content-Type"] = "multipart/form-data; boundary=#{boundary}"

      # 构建 multipart 表单数据，确保正确的编码
      # 所有部分都需要使用 ASCII-8BIT 编码以兼容二进制数据
      body_parts = []
      body_parts << "--#{boundary}\r\n"
      body_parts << "Content-Disposition: form-data; name=\"file\"; filename=\"#{filename}\"\r\n"
      body_parts << "Content-Type: #{content_type}\r\n\r\n"
      body_parts << image_data
      body_parts << "\r\n--#{boundary}--\r\n"

      # 将所有部分强制转换为 ASCII-8BIT 编码并连接
      request.body = body_parts.map { |part| part.force_encoding("ASCII-8BIT") }.join

      response = http.request(request)

      if response.is_a?(Net::HTTPSuccess)
        media_data = JSON.parse(response.body)
        Rails.event.notify "mastodon_service.image_uploaded",
          level: "info",
          component: "MastodonService",
          media_id: media_data["id"]
        media_data["id"]
      else
        Rails.event.notify "mastodon_service.image_upload_failed",
          level: "error",
          component: "MastodonService",
          response_code: response.code,
          response_body: response.body[0..200]
        nil
      end
    rescue => e
      Rails.event.notify "mastodon_service.image_upload_error",
        level: "error",
        component: "MastodonService",
        error_message: e.message,
        backtrace: e.backtrace.join("\n")
      nil
    end
  end

  # Parse rate limit headers from Mastodon API response
  def parse_rate_limit_headers(response)
    {
      limit: response["X-RateLimit-Limit"]&.to_i,
      remaining: response["X-RateLimit-Remaining"]&.to_i,
      reset_at: response["X-RateLimit-Reset"] ? Time.at(response["X-RateLimit-Reset"].to_i) : nil
    }
  end

  # Log rate limit status for monitoring
  def log_rate_limit_status(rate_limit_info)
    return unless rate_limit_info[:remaining]

    if rate_limit_info[:remaining] < 10
      Rails.event.notify "mastodon_service.rate_limit_low",
        level: "warn",
        component: "MastodonService",
        remaining: rate_limit_info[:remaining],
        limit: rate_limit_info[:limit],
        reset_at: rate_limit_info[:reset_at]

      ActivityLog.log!(
        action: :rate_limit_low,
        target: :mastodon_api,
        level: :warn,
        remaining: rate_limit_info[:remaining],
        limit: rate_limit_info[:limit],
        reset_at: rate_limit_info[:reset_at]
      )
    elsif rate_limit_info[:remaining] < 50
      Rails.event.notify "mastodon_service.rate_limit_status",
        level: "info",
        component: "MastodonService",
        remaining: rate_limit_info[:remaining],
        limit: rate_limit_info[:limit]
    end
  end

  # Handle rate limit exceeded (429 response)
  def handle_rate_limit_exceeded(rate_limit_info)
    reset_time = rate_limit_info[:reset_at] || Time.current + 5.minutes
    wait_seconds = [ (reset_time - Time.current).to_i, 0 ].max

    Rails.event.notify "mastodon_service.rate_limit_exceeded",
      level: "error",
      component: "MastodonService",
      reset_time: reset_time,
      wait_seconds: wait_seconds

    ActivityLog.log!(
      action: :rate_limited,
      target: :mastodon_api,
      level: :error,
      reset_at: reset_time,
      remaining: rate_limit_info[:remaining],
      limit: rate_limit_info[:limit]
    )
  end

  def mastodon_api_uri(path, server_url = @settings[:server_url])
    base_uri = normalized_server_uri(server_url)
    unless base_uri
      notify_invalid_server_url(server_url, path)
      return nil
    end

    relative_path = path.to_s.sub(%r{\A/}, "")
    URI.join(base_uri.to_s, relative_path)
  end

  def notify_invalid_server_url(server_url, path)
    info = server_url_info(server_url)
    Rails.event.notify "mastodon_service.invalid_server_url",
      level: "error",
      component: "MastodonService",
      path: path.to_s,
      server_url_present: info[:present],
      server_url_scheme: info[:scheme],
      server_url_host: info[:host]
  end

  def server_url_info(server_url)
    raw_url = server_url.to_s.strip
    return { present: false, scheme: nil, host: nil } if raw_url.blank?

    uri = URI.parse(raw_url)
    { present: true, scheme: uri.scheme, host: uri.host }
  rescue URI::InvalidURIError
    { present: true, scheme: nil, host: nil }
  end

  def normalized_server_uri(server_url)
    raw_url = server_url.to_s.strip
    return nil if raw_url.blank?

    uri = URI.parse(raw_url)
    return nil unless uri.is_a?(URI::HTTP)
    return nil if uri.host.blank? || uri.userinfo.present?

    normalized_path = uri.path.presence || "/"
    normalized_path = "/#{normalized_path}" unless normalized_path.start_with?("/")
    normalized_path = "#{normalized_path}/" unless normalized_path.end_with?("/")

    normalized = "#{uri.scheme}://#{uri.host}"
    normalized += ":#{uri.port}" if uri.port && uri.port != uri.default_port
    normalized += normalized_path
    URI.parse(normalized)
  rescue URI::InvalidURIError
    nil
  end

  def extract_status_id_from_url(url)
    # Mastodon URLs are typically: https://mastodon.social/@username/123456789
    # or https://mastodon.social/users/username/statuses/123456789
    match = url.match(%r{/(?:@\w+|users/\w+/statuses)/(\d+)})
    match ? match[1] : nil
  end

  # Download remote image with redirect support
  def download_remote_image(image_url)
    return nil unless image_url.present?

    begin
      # 将相对 URL 转换为绝对 URL
      if image_url.start_with?("/")
        site_url = Setting.first&.url.presence || "http://localhost:3000"
        image_url = "#{site_url}#{image_url}"
      end

      # 下载远程图片，支持重定向（ActiveStorage redirect URLs)
      uri = URI.parse(image_url)
      image_response = fetch_with_redirect(uri)

      unless image_response.is_a?(Net::HTTPSuccess)
        Rails.event.notify "mastodon_service.remote_image_download_failed",
          level: "error",
          component: "MastodonService",
          response_code: image_response.code
        return nil
      end

      image_data = image_response.body
      content_type = image_response["content-type"] || "image/jpeg"

      [ image_data, content_type ]
    rescue => e
      Rails.event.notify "mastodon_service.remote_image_download_error",
        level: "error",
        component: "MastodonService",
        error_message: e.message,
        backtrace: e.backtrace.join("\n")
      nil
    end
  end

  # 跟随HTTP重定向获取图片
  def fetch_with_redirect(uri, limit = 5)
    raise "Too many HTTP redirects" if limit == 0

    http = Net::HTTP.new(uri.host, uri.port)
    http.use_ssl = (uri.scheme == "https")
    http.open_timeout = 10
    http.read_timeout = 10

    request = Net::HTTP::Get.new(uri.path + (uri.query ? "?#{uri.query}" : ""))
    response = http.request(request)

    case response
    when Net::HTTPSuccess
      response
    when Net::HTTPRedirection
      redirect_uri = URI.parse(response["location"])
      # 如果是相对URL，补全域名
      if redirect_uri.relative?
        redirect_uri = URI.join("#{uri.scheme}://#{uri.host}:#{uri.port}", response["location"])
      end
      Rails.event.notify "mastodon_service.following_redirect",
        level: "info",
        component: "MastodonService",
        redirect_uri: redirect_uri.to_s
      fetch_with_redirect(redirect_uri, limit - 1)
    else
      response
    end
  end
end
