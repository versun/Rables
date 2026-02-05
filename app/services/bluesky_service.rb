# COPY from: https://t27duck.com/posts/17-a-bluesky-at-proto-api-example-in-ruby
class BlueskyService
  include ContentBuilder

  TOKEN_CACHE_KEY = :bluesky_token_data

  def initialize
    @settings = Crosspost.bluesky
    return unless @settings.present?

    @username = @settings.username
    @password = @settings.app_password
    @server_url = @settings.server_url.presence || "https://bsky.social/xrpc"

    if (token_data = Rails.cache.read(TOKEN_CACHE_KEY))
      process_tokens(token_data)
    end
  end

  def verify(settings)
    if settings[:username].blank? || settings[:app_password].blank?
      return { success: false, error: "App Password and username are required" }
    end

    # Temporarily store the current credentials
    original_username = @username
    original_password = @password
    original_server_url = @server_url

    begin
      @username = settings[:username]
      @password = settings[:app_password]
      @server_url = settings[:server_url]

      # Clear any existing token data
      @token = nil
      @token_expires_at = nil
      Rails.cache.delete(TOKEN_CACHE_KEY)

      # Attempt to generate new tokens with the provided credentials
      verify_tokens
      { success: true }
    rescue => e
      { success: false, error: "Bluesky verification failed: #{e.message}" }
    ensure
      # Restore the original credentials
      @username = original_username
      @password = original_password
      @server_url = original_server_url
    end
  end

  def post(article)
    return unless @settings&.enabled?

    max_length = @settings.effective_max_characters || 300
    content = build_content(article: article, max_length: max_length)

    # 获取文章所有图片（Bluesky最多支持4张）
    images = article.all_image_attachments(4)
    Rails.event.notify "bluesky_service.images_count",
      level: "info",
      component: "BlueskyService",
      count: images.size

    embed = nil

    if images.any?
      Rails.event.notify "bluesky_service.upload_images_attempt",
        level: "info",
        component: "BlueskyService",
        images_count: images.size
      embed = upload_images_embed(images)
      Rails.event.notify "bluesky_service.upload_images_result",
        level: "info",
        component: "BlueskyService",
        result: embed.present? ? "success" : "failed"
    else
      Rails.event.notify "bluesky_service.no_images",
        level: "info",
        component: "BlueskyService"
    end

    begin
      posted_url = skeet(content, embed)
      ActivityLog.log!(
        action: :posted,
        target: :crosspost,
        level: :info,
        title: article.title,
        slug: article.slug,
        platform: "bluesky",
        url: posted_url
      )

      posted_url
    rescue => e
      ActivityLog.log!(
        action: :failed,
        target: :crosspost,
        level: :error,
        title: article.title,
        slug: article.slug,
        platform: "bluesky",
        error: e.message
      )
      nil
    end
  end

  # Posts a new message (skeet) to the account and return the direct URL.
  def skeet(message, embed = nil)
    # Generate, refresh, or use an active token.
    verify_tokens

    # URLs and tags are not automatically parsed. Instead we have to manually
    # parse and set facets for each.
    # See: https://docs.bsky.app/docs/advanced-guides/post-richtext
    facets = link_facets(message)
    # facets += tag_facets(message)

    # Build the record - only include embed if it's present
    record = {
      text: message,
      createdAt: Time.now.iso8601,
      langs: [ "en" ],
      facets: facets
    }

    # Only add embed if it's not nil (Bluesky requires $type if embed is present)
    record[:embed] = embed if embed.present?

    body = {
      repo: @user_did,
      collection: "app.bsky.feed.post",
      record: record
    }
    response_body = post_request("#{@server_url}/com.atproto.repo.createRecord", body: body)

    # This is the full atproto URI
    # Ex: "at://did:plc:axbcdefg12345/app.bsky.feed.post/abcdefg12345"
    if response_body["uri"].present?
      "https://bsky.app/profile/#{@settings.username}/post/#{response_body["uri"].split('/').last}"
    end
  end

  def unskeet(skeet_uri)
    # Generate, refresh, or use an active token.
    verify_tokens

    did, nsid, record_key = skeet_uri.delete_prefix("at://").split("/")
    body = { repo: did, collection: nsid, rkey: record_key }
    post_request("#{@server_url}/com.atproto.repo.deleteRecord", body: body)
  end

  # Fetch comments (replies) for a Bluesky post
  # Returns a hash with :comments array and :rate_limit info
  def fetch_comments(post_url)
    default_response = { comments: [], rate_limit: nil }
    return default_response unless @settings&.enabled?
    return default_response if post_url.blank?

    begin
      # Extract AT-URI from Bluesky URL
      at_uri = extract_post_uri_from_url(post_url)
      return default_response unless at_uri

      # Ensure we have a valid token
      verify_tokens

      # Call Bluesky API to get thread with replies
      # Using public API for better rate limits
      api_url = "https://public.api.bsky.app/xrpc/app.bsky.feed.getPostThread"
      uri = URI(api_url)
      uri.query = URI.encode_www_form(uri: at_uri, depth: 10)

      http = Net::HTTP.new(uri.host, uri.port)
      http.use_ssl = true
      http.open_timeout = 5
      http.read_timeout = 5

      request = Net::HTTP::Get.new(uri)
      # Optional: Add auth for better limits, but public API works
      # request["Authorization"] = "Bearer #{@token}" if @token

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
        thread_data = JSON.parse(response.body)

        # Get the original post URI to identify top-level replies
        original_post_uri = thread_data.dig("thread", "post", "uri")
        original_post_rkey = original_post_uri&.split("/")&.last

        # Get replies from thread (nested structure)
        replies = thread_data.dig("thread", "replies") || []

        # Flatten nested replies into comment list, preserving parent relationships
        # Top-level replies should have parent_external_id = nil, not the original post ID
        comments = flatten_thread_replies(replies, nil)

        { comments: comments, rate_limit: rate_limit_info }
      else
        Rails.event.notify "bluesky_service.fetch_comments_failed",
          level: "error",
          component: "BlueskyService",
          response_code: response.code,
          response_body: response.body[0..200]
        { comments: [], rate_limit: rate_limit_info }
      end
    rescue => e
      Rails.event.notify "bluesky_service.fetch_comments_error",
        level: "error",
        component: "BlueskyService",
        error_message: e.message,
        backtrace: e.backtrace.join("\n")
      default_response
    end
  end

  private



  def retrieve_server_token
    token_data = Rails.cache.read(TOKEN_CACHE_KEY)
    if token_data.present?
      process_tokens(token_data)
      @token
    else
      verify_tokens
      @token
    end
  end

  def link_facets(message)
    [].tap do |facets|
      matches = []
      message.scan(URI::RFC2396_PARSER.make_regexp([ "http", "https" ])) { matches << Regexp.last_match }
      matches.each do |match|
        url = match[0]

        # 验证 URL 格式是否正确
        begin
          uri = URI.parse(url)
          # 确保 URL 有 scheme 和 host
          next unless uri.scheme && uri.host
        rescue URI::InvalidURIError
          Rails.event.notify "bluesky_service.invalid_url_in_facets",
            level: "warn",
            component: "BlueskyService",
            url: url
          next
        end

        start, stop = match.byteoffset(0)
        facets << {
          "index" => { "byteStart" => start, "byteEnd" => stop },
          "features" => [ { "uri" => url, "$type" => "app.bsky.richtext.facet#link" } ]
        }
      end
    end
  end

  def tag_facets(message)
    [].tap do |facets|
      matches = []
      message.scan(/(^|[^\w])(#[\w\-]+)/) { matches << Regexp.last_match }
      matches.each do |match|
        start, stop = match.byteoffset(2)
        facets << {
          "index" => { "byteStart" => start, "byteEnd" => stop },
          "features" => [ { "tag" => match[2].delete_prefix("#"), "$type" => "app.bsky.richtext.facet#tag" } ]
        }
      end
    end
  end

  # Makes a POST request to the API.
  def post_request(url, body: {}, auth_token: true, content_type: "application/json")
    uri = URI.parse(url)
    Rails.event.notify "bluesky_service.post_request",
      level: "info",
      component: "BlueskyService",
      url: url,
      uri: uri.to_s
    http = Net::HTTP.new(uri.host, uri.port)
    http.use_ssl = (uri.scheme == "https")
    http.open_timeout = 4
    http.read_timeout = 4
    http.write_timeout = 4
    request = Net::HTTP::Post.new(uri.path)
    request["content-type"] = content_type

    # This allows the authorization token to:
    #   - Be sent using the currently stored token (true).
    #   - Not send when providing the username/password to generate the token (false).
    #   - Use a different token - like the refresh token (string).
    if auth_token
      token = auth_token.is_a?(String) ? auth_token : @token
      request["Authorization"] = "Bearer #{token}"
    end
    request.body = body.is_a?(Hash) ? body.to_json : body if body.present?
    response = http.request(request)
  raise "#{response.code} response - #{response.body}" unless response.code.to_s.start_with?("2")

    response.content_type == "application/json" ? JSON.parse(response.body) : response.body
  end

  # Generate tokens given an account identifier and app password.
  def generate_tokens
    body = { identifier: @username, password: @password }
    response_body = post_request("#{@server_url}/com.atproto.server.createSession", body: body, auth_token: false)

    process_tokens(response_body)
    store_token_data(response_body)
  end

  # Regenerates expired tokens with the refresh token.
  def perform_token_refresh
    response_body = post_request("#{@server_url}/com.atproto.server.refreshSession", auth_token: @renewal_token)

    process_tokens(response_body)
    store_token_data(response_body)
  end

  # Makes sure a token is set and the token has not expired.
  # If this is the first request, we'll generate the token.
  # If the token expired, we'll refresh it.
  def verify_tokens
    if @token.nil?
      generate_tokens
    elsif @token_expires_at < Time.now.utc + 60
      perform_token_refresh
    end
  end

  # Given the response body of generating or refreshing token, this pulls out
  # and stores the bits of information we care about.
  def process_tokens(response_body)
    @token = response_body["accessJwt"]
    @renewal_token = response_body["refreshJwt"]
    @user_did = response_body["did"]
    @token_expires_at = Time.at(JSON.parse(Base64.decode64(response_body["accessJwt"].split(".")[1]))["exp"]).utc
  end

  # Stores the token info for use later, else we'll have to generate the token
  # for every instance of this class.
  # Assumes the cached info is stored in the Rails cache store.
  def store_token_data(data)
    Rails.cache.write(TOKEN_CACHE_KEY, data)
  end

  def upload_image_embed(attachable)
    upload_images_embed([ attachable ])
  end

  def upload_images_embed(attachables)
    Rails.event.notify "bluesky_service.upload_images_embed_started",
      level: "info",
      component: "BlueskyService",
      count: attachables&.size || 0
    return nil unless attachables.present?

    begin
      images_data = []

      attachables.each_with_index do |attachable, index|
        blob_data = nil
        filename = "image.jpg"

        # Handle ActiveStorage::Blob
        if attachable.is_a?(ActiveStorage::Blob) && attachable.content_type&.start_with?("image/")
          Rails.event.notify "bluesky_service.processing_blob",
            level: "info",
            component: "BlueskyService",
            index: index,
            storage_type: "ActiveStorage::Blob"
          blob_data = upload_blob(attachable)
          filename = attachable.filename.to_s if attachable.respond_to?(:filename)
        # Handle RemoteImage
        elsif attachable.class.name == "ActionText::Attachables::RemoteImage"
          Rails.event.notify "bluesky_service.processing_remote_image",
            level: "info",
            component: "BlueskyService",
            index: index,
            storage_type: "RemoteImage"
          image_url = attachable.try(:url)

          if image_url.present?
            blob_data = upload_remote_image(image_url)
            begin
              filename = File.basename(URI.parse(image_url).path)
              filename = "image.jpg" if filename.blank? || filename == "/"
            rescue URI::InvalidURIError => e
              Rails.event.notify "bluesky_service.invalid_url_for_filename",
                level: "warn",
                component: "BlueskyService",
                image_url: image_url,
                error_message: e.message
              filename = "image.jpg"
            end
          else
            Rails.event.notify "bluesky_service.remote_image_no_url",
              level: "warn",
              component: "BlueskyService",
              index: index
            next
          end
        else
          Rails.event.notify "bluesky_service.unknown_attachable_type",
            level: "warn",
            component: "BlueskyService",
            index: index,
            attachable_type: attachable.class.to_s
          next
        end

        if blob_data
          images_data << { blob: blob_data, filename: filename }
          Rails.event.notify "bluesky_service.image_uploaded",
            level: "info",
            component: "BlueskyService",
            index: index,
            total_uploaded: images_data.size
        else
          Rails.event.notify "bluesky_service.image_upload_failed",
            level: "warn",
            component: "BlueskyService",
            index: index
        end
      end

      return nil if images_data.empty?

      # 创建多图片embed
      embed_result = {
        "$type" => "app.bsky.embed.images",
        "images" => images_data.map do |img_data|
          {
            "alt" => img_data[:filename],
            "image" => img_data[:blob]
          }
        end
      }
      Rails.event.notify "bluesky_service.embed_created",
        level: "info",
        component: "BlueskyService",
        images_count: images_data.size
      embed_result
    rescue => e
      Rails.event.notify "bluesky_service.embed_creation_error",
        level: "error",
        component: "BlueskyService",
        error_message: e.message,
        backtrace: e.backtrace.join("\n")
      nil
    end
  end

  # Bluesky 最大图片大小限制 (略低于 976.56KB 以留有余地)
  MAX_IMAGE_SIZE = 950.kilobytes

  def upload_blob(blob)
    return nil unless blob

    begin
      # 确保token有效
      verify_tokens

      # 下载图片数据
      image_data = blob.download

      # 如果图片太大，进行压缩
      image_data, content_type = resize_image_if_needed(image_data, blob.content_type)

      uri = URI("#{@server_url}/com.atproto.repo.uploadBlob")
      request = Net::HTTP::Post.new(uri)
      request["Content-Type"] = content_type
      request["Authorization"] = "Bearer #{@token}"
      request.body = image_data

      response = Net::HTTP.start(uri.hostname, uri.port, use_ssl: uri.scheme == "https") do |http|
        http.request(request)
      end

      if response.is_a?(Net::HTTPSuccess)
        result = JSON.parse(response.body)
        result["blob"]
      else
        Rails.event.notify "bluesky_service.blob_upload_failed",
          level: "error",
          component: "BlueskyService",
          response_code: response.code,
          response_body: response.body[0..200]
        nil
      end
    rescue => e
      Rails.event.notify "bluesky_service.blob_upload_error",
        level: "error",
        component: "BlueskyService",
        error_message: e.message
      nil
    end
  end

  def upload_remote_image(image_url)
    return nil unless image_url.present?

    begin
      # 确保token有效
      verify_tokens

      # 将相对 URL 转换为绝对 URL
      if image_url.start_with?("/")
        site_url = Setting.first&.url.presence || "http://localhost:3000"
        image_url = "#{site_url}#{image_url}"
      end

      # 下载远程图片，支持重定向（ActiveStorage redirect URLs)
      uri = URI.parse(image_url)
      image_response = fetch_with_redirect(uri)

      unless image_response.is_a?(Net::HTTPSuccess)
        Rails.event.notify "bluesky_service.remote_image_download_failed",
          level: "error",
          component: "BlueskyService",
          response_code: image_response.code
        return nil
      end

      image_data = image_response.body
      content_type = image_response["content-type"] || "image/jpeg"

      # 如果图片太大，进行压缩
      image_data, content_type = resize_image_if_needed(image_data, content_type)

      # 上传到 Bluesky
      upload_uri = URI("#{@server_url}/com.atproto.repo.uploadBlob")
      request = Net::HTTP::Post.new(upload_uri)
      request["Content-Type"] = content_type
      request["Authorization"] = "Bearer #{@token}"
      request.body = image_data

      response = Net::HTTP.start(upload_uri.hostname, upload_uri.port, use_ssl: upload_uri.scheme == "https") do |http|
        http.request(request)
      end

      if response.is_a?(Net::HTTPSuccess)
        result = JSON.parse(response.body)
        Rails.event.notify "bluesky_service.remote_image_uploaded",
          level: "info",
          component: "BlueskyService"
        result["blob"]
      else
        Rails.event.notify "bluesky_service.remote_image_upload_failed",
          level: "error",
          component: "BlueskyService",
          response_code: response.code,
          response_body: response.body[0..200]
        nil
      end
    rescue => e
      Rails.event.notify "bluesky_service.remote_image_upload_error",
        level: "error",
        component: "BlueskyService",
        error_message: e.message,
        backtrace: e.backtrace.join("\n")
      nil
    end
  end

  # 如果图片太大，使用 ruby-vips 压缩
  def resize_image_if_needed(image_data, content_type)
    return [ image_data, content_type ] if image_data.bytesize <= MAX_IMAGE_SIZE

    original_path = nil
    compressed_path = nil

    begin
      Rails.event.notify "bluesky_service.resizing_image",
        level: "info",
        component: "BlueskyService",
        original_size: image_data.bytesize,
        max_size: MAX_IMAGE_SIZE

      # 创建临时文件保存原始图片
      extension = content_type.to_s.split("/").last
      extension = "jpg" if extension.blank? || extension == "jpeg"
      extension = extension.downcase

      temp_dir = Rails.root.join("tmp", "bluesky_uploads")
      FileUtils.mkdir_p(temp_dir)

      original_path = temp_dir.join("original_#{SecureRandom.hex(8)}.#{extension}")
      File.binwrite(original_path, image_data)

      # 使用 Vips 处理图片
      image = Vips::Image.new_from_file(original_path.to_s)

      # 计算缩放比例
      # 首先尝试降低质量压缩
      quality = 85
      compressed_path = temp_dir.join("compressed_#{SecureRandom.hex(8)}.jpg")

      loop do
        image.write_to_file(compressed_path.to_s, Q: quality, strip: true)
        compressed_size = File.size(compressed_path)

        if compressed_size <= MAX_IMAGE_SIZE || quality <= 50
          break
        end

        # 如果还是太大，继续降低质量
        quality -= 10
        File.delete(compressed_path) if File.exist?(compressed_path)
      end

      # 如果质量降到50还是太大，尝试缩放尺寸
      if File.size(compressed_path) > MAX_IMAGE_SIZE
        File.delete(compressed_path) if File.exist?(compressed_path)

        # 计算需要缩小的比例
        scale_factor = Math.sqrt(MAX_IMAGE_SIZE.to_f / image_data.bytesize) * 0.9
        new_width = (image.width * scale_factor).to_i
        new_height = (image.height * scale_factor).to_i

        # 确保不小于最小尺寸
        new_width = [ new_width, 100 ].max
        new_height = [ new_height, 100 ].max

        resized = image.resize(scale_factor)
        resized.write_to_file(compressed_path.to_s, Q: 85, strip: true)
      end

      result_data = File.binread(compressed_path)
      result_type = "image/jpeg"

      Rails.event.notify "bluesky_service.image_resized",
        level: "info",
        component: "BlueskyService",
        original_size: image_data.bytesize,
        final_size: result_data.bytesize,
        quality: quality

      [ result_data, result_type ]
    rescue => e
      Rails.event.notify "bluesky_service.resize_failed",
        level: "error",
        component: "BlueskyService",
        error_message: e.message,
        backtrace: e.backtrace.join("\n")

      # 压缩失败时返回原图，让 Bluesky 决定是否接受
      [ image_data, content_type ]
    ensure
      File.delete(original_path) if original_path && File.exist?(original_path)
      File.delete(compressed_path) if compressed_path && File.exist?(compressed_path)
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
      Rails.event.notify "bluesky_service.following_redirect",
        level: "info",
        component: "BlueskyService",
        redirect_uri: redirect_uri.to_s
      fetch_with_redirect(redirect_uri, limit - 1)
    else
      response
    end
  end

  # Extract AT-URI from Bluesky URL
  # URL format: https://bsky.app/profile/{handle}/post/{rkey}
  # AT-URI format: at://{did}/app.bsky.feed.post/{rkey}
  def extract_post_uri_from_url(url)
    # Extract handle and rkey from URL
    match = url.match(%r{bsky\.app/profile/([^/]+)/post/(\w+)})
    return nil unless match

    handle = match[1]
    rkey = match[2]

    # Resolve handle to DID
    begin
      resolve_uri = URI("https://public.api.bsky.app/xrpc/com.atproto.identity.resolveHandle")
      resolve_uri.query = URI.encode_www_form(handle: handle)

      response = Net::HTTP.get_response(resolve_uri)
      if response.is_a?(Net::HTTPSuccess)
        result = JSON.parse(response.body)
        did = result["did"]
        "at://#{did}/app.bsky.feed.post/#{rkey}"
      else
        Rails.event.notify "bluesky_service.handle_resolution_failed",
          level: "error",
          component: "BlueskyService",
          handle: handle
        nil
      end
    rescue => e
      Rails.event.notify "bluesky_service.handle_resolution_error",
        level: "error",
        component: "BlueskyService",
        error_message: e.message
      nil
    end
  end

  # Flatten nested thread replies into a flat comment list, preserving parent relationships
  def flatten_thread_replies(replies, parent_external_id = nil)
    [].tap do |comments|
      replies.each do |reply_item|
        next unless reply_item["post"]  # Skip non-post items

        post = reply_item["post"]
        current_external_id = post["uri"].split("/").last  # Extract rkey from AT-URI

        # Add this reply as a comment with parent information
        comments << {
          external_id: current_external_id,
          author_name: post["author"]["displayName"].presence || post["author"]["handle"],
          author_username: post["author"]["handle"],
          author_avatar_url: post["author"]["avatar"],
          content: post["record"]["text"],
          published_at: Time.parse(post["record"]["createdAt"]),
          url: "https://bsky.app/profile/#{post["author"]["handle"]}/post/#{current_external_id}",
          parent_external_id: parent_external_id
        }

        # Recursively process nested replies, passing current reply as parent
        if reply_item["replies"]&.any?
          nested_comments = flatten_thread_replies(reply_item["replies"], current_external_id)
          comments.concat(nested_comments)
        end
      end
    end
  end

  # Parse rate limit headers from Bluesky API response
  def parse_rate_limit_headers(response)
    {
      limit: response["RateLimit-Limit"]&.to_i,
      remaining: response["RateLimit-Remaining"]&.to_i,
      reset_at: response["RateLimit-Reset"] ? Time.at(response["RateLimit-Reset"].to_i) : nil
    }
  end

  # Log rate limit status for monitoring
  def log_rate_limit_status(rate_limit_info)
    return unless rate_limit_info[:remaining]

    # Bluesky has higher limits (3000/5min vs Mastodon 300/5min)
    # So we use different thresholds
    if rate_limit_info[:remaining] < 100
      Rails.event.notify "bluesky_service.rate_limit_low",
        level: "warn",
        component: "BlueskyService",
        remaining: rate_limit_info[:remaining],
        limit: rate_limit_info[:limit],
        reset_at: rate_limit_info[:reset_at]

      ActivityLog.log!(
        action: :rate_limit_low,
        target: :bluesky_api,
        level: :warn,
        remaining: rate_limit_info[:remaining],
        limit: rate_limit_info[:limit],
        reset_at: rate_limit_info[:reset_at]
      )
    elsif rate_limit_info[:remaining] < 500
      Rails.event.notify "bluesky_service.rate_limit_status",
        level: "info",
        component: "BlueskyService",
        remaining: rate_limit_info[:remaining],
        limit: rate_limit_info[:limit]
    end
  end

  # Handle rate limit exceeded (429 response)
  def handle_rate_limit_exceeded(rate_limit_info)
    reset_time = rate_limit_info[:reset_at] || Time.current + 5.minutes
    wait_seconds = [ (reset_time - Time.current).to_i, 0 ].max

    Rails.event.notify "bluesky_service.rate_limit_exceeded",
      level: "error",
      component: "BlueskyService",
      reset_time: reset_time,
      wait_seconds: wait_seconds

    ActivityLog.log!(
      action: :rate_limited,
      target: :bluesky_api,
      level: :error,
      reset_at: reset_time,
      remaining: rate_limit_info[:remaining],
      limit: rate_limit_info[:limit]
    )
  end
end
