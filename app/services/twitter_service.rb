require "x"
require "tempfile"
require "net/http"
require "uri"
require "json"

class TwitterService
  include ContentBuilder
  include HttpRedirectHandler

  def initialize
    @settings = Crosspost.twitter
  end

  def verify(settings)
    if settings[:access_token_secret].blank? || settings[:access_token].blank? || settings[:api_key].blank? || settings[:api_key_secret].blank?
      return { success: false, error: "Please fill in all information" }
    end

    begin
        client = X::Client.new(
          api_key: settings[:api_key],
          api_key_secret: settings[:api_key_secret],
          access_token: settings[:access_token],
          access_token_secret: settings[:access_token_secret]
        )

        # Try to post a test tweet to verify credentials
        test_response = client.get("users/me")
        if test_response && test_response["data"] && test_response["data"]["id"]
          { success: true }
        else
          { success: false, error: "Twitter verification failed: #{test_response}" }
        end

    rescue => e
      { success: false, error: "Twitter verification failed: #{e.message}" }
    end
  end

  def post(article)
    return unless @settings&.enabled?

    client = create_client
    max_length = @settings.effective_max_characters || 250

    quote_tweet_id = quote_tweet_id_for_article(article)
    tweet = if quote_tweet_id
      build_content(article.slug, article.title, article.plain_text_content, article.description, max_length: max_length, count_non_ascii_double: true)
    else
      build_content(article: article, max_length: max_length, count_non_ascii_double: true)
    end

    begin
      user = client.get("users/me")
      username = user["data"]["username"] if user && user["data"]

      # 获取文章所有图片（Twitter最多支持4张）
      images = article.all_image_attachments(4)
      Rails.event.notify "twitter_service.images_count",
        level: "info",
        component: "TwitterService",
        count: images.size

      media_ids = []
      images.each do |image|
        Rails.event.notify "twitter_service.upload_image_attempt",
          level: "info",
          component: "TwitterService",
          image_type: image.class.to_s
        media_id = upload_image(client, image)
        if media_id
          media_ids << media_id
          Rails.event.notify "twitter_service.image_uploaded",
            level: "info",
            component: "TwitterService",
            media_id: media_id,
            total_uploaded: media_ids.size
        else
          Rails.event.notify "twitter_service.image_upload_failed",
            level: "warn",
            component: "TwitterService"
        end
      end

      if media_ids.empty? && images.any?
        Rails.event.notify "twitter_service.no_images_uploaded",
          level: "warn",
          component: "TwitterService"
      end

      # 构建推文数据
      tweet_data = { text: tweet }
      tweet_data[:quote_tweet_id] = quote_tweet_id if quote_tweet_id
      if media_ids.any?
        tweet_data[:media] = {
          media_ids: media_ids.map(&:to_s)
        }
      end

      Rails.event.notify "twitter_service.sending_tweet",
        level: "info",
        component: "TwitterService",
        tweet_data: tweet_data.inspect
      response = client.post("tweets", tweet_data.to_json)

      if response && response["data"] && response["data"]["id"]
        id = response["data"]["id"]
        ActivityLog.log!(
          action: :posted,
          target: :crosspost,
          level: :info,
          title: article.title,
          slug: article.slug,
          platform: "twitter",
          post_id: id
        )
      else
        error_message = response&.dig("errors")&.first&.dig("message") || "Unknown error"
        Rails.event.notify "twitter_service.tweet_failed",
          level: "error",
          component: "TwitterService",
          error_message: error_message

        # 如果带媒体的推文失败，尝试发送纯文本推文
        if media_ids.any? && error_message.include?("media")
          Rails.event.notify "twitter_service.media_tweet_failed",
            level: "warn",
            component: "TwitterService"

          text_only_data = { text: tweet }
          text_only_data[:quote_tweet_id] = quote_tweet_id if quote_tweet_id
          Rails.event.notify "twitter_service.sending_text_only",
            level: "info",
            component: "TwitterService",
            tweet_data: text_only_data.inspect

          begin
            fallback_response = client.post("tweets", text_only_data.to_json)

            if fallback_response && fallback_response["data"] && fallback_response["data"]["id"]
              id = fallback_response["data"]["id"]
              Rails.event.notify "twitter_service.text_only_succeeded",
                level: "warn",
                component: "TwitterService"

              ActivityLog.log!(
                action: :posted,
                target: :crosspost,
                level: :warn,
                title: article.title,
                slug: article.slug,
                platform: "twitter",
                post_id: id,
                status: "text_only",
                error: "media_upload_failed"
              )
            else
              raise "Fallback text tweet also failed"
            end
          rescue => fallback_error
            Rails.event.notify "twitter_service.fallback_failed",
              level: "error",
              component: "TwitterService",
              error_message: fallback_error.message

            ActivityLog.log!(
              action: :failed,
              target: :crosspost,
              level: :error,
              title: article.title,
              slug: article.slug,
              platform: "twitter",
              error: "#{error_message} (fallback_failed: #{fallback_error.message})"
            )
            return nil
          end
        else
          ActivityLog.log!(
            action: :failed,
            target: :crosspost,
            level: :error,
            title: article.title,
            slug: article.slug,
            platform: "twitter",
            error: error_message
          )
          return nil
        end
      end

      "https://x.com/#{username}/status/#{id}" if username && id
    rescue => e
      Rails.event.notify "twitter_service.post_error",
        level: "error",
        component: "TwitterService",
        error_message: e.message
      ActivityLog.log!(
        action: :failed,
        target: :crosspost,
        level: :error,
        title: article.title,
        slug: article.slug,
        platform: "twitter",
        error: e.message
      )
      nil
    end
  end

  # Fetch comments (replies and quote tweets) for a Twitter/X post
  # Returns a hash with :comments array and :rate_limit info
  def fetch_comments(post_url)
    default_response = { comments: [], rate_limit: nil }
    return default_response unless @settings&.enabled?
    return default_response if post_url.blank?

    begin
      # Extract tweet ID from URL
      tweet_id = extract_tweet_id_from_url(post_url)
      return default_response unless tweet_id

      client = create_client

      # Use Twitter API v2 to get conversation thread
      # Note: Free tier has limited access, using conversation_id lookup
      response = make_rate_limited_request(client, "tweets/#{tweet_id}?expansions=author_id,referenced_tweets.id&tweet.fields=conversation_id,created_at,author_id&user.fields=username,name,profile_image_url")

      if response && response["data"]
        conversation_id = response["data"]["conversation_id"]
        comments = []
        rate_limit_info = nil

        # 1. Get direct replies to the original tweet
        replies_query = "conversation_id:#{conversation_id} is:reply"
        replies_response, rate_limit_info = make_rate_limited_request_with_retry(
          client,
          "tweets/search/recent?query=#{CGI.escape(replies_query)}&expansions=author_id,referenced_tweets.id&tweet.fields=created_at,referenced_tweets,conversation_id&user.fields=username,name,profile_image_url&max_results=100"
        )

        if replies_response
          comments.concat(process_tweets(replies_response, tweet_id))
        end

        # 2. Get quote tweets (转帖) that quote the original tweet
        # Note: Twitter API v2 free tier may have limited support for quote tweet search
        quote_tweets = []

        begin
          # Try searching for quote tweets using URL
          # This method may not work on all API tiers, so we wrap it in error handling
          quote_query = "url:#{CGI.escape(post_url)} is:quote"
          quote_response, rate_limit_info = make_rate_limited_request_with_retry(
            client,
            "tweets/search/recent?query=#{CGI.escape(quote_query)}&expansions=author_id,referenced_tweets.id&tweet.fields=created_at,referenced_tweets,conversation_id&user.fields=username,name,profile_image_url&max_results=100"
          )

          if quote_response && quote_response["data"]
            quote_tweets = process_tweets(quote_response, tweet_id)
            Rails.event.notify "twitter_service.quote_tweets_found",
              level: "info",
              component: "TwitterService",
              count: quote_tweets.length,
              tweet_id: tweet_id
          end
        rescue => e
          Rails.event.notify "twitter_service.quote_tweets_failed",
            level: "warn",
            component: "TwitterService",
            error_message: e.message
          # Continue without quote tweets - we can still fetch direct replies
        end

        comments.concat(quote_tweets)

        # 3. For each quote tweet, get its replies
        quote_tweets.each do |quote_tweet|
          quote_tweet_id = quote_tweet[:external_id]
          quote_conversation_id = quote_tweet[:conversation_id]

          if quote_conversation_id
            quote_replies_query = "conversation_id:#{quote_conversation_id} is:reply"
            quote_replies_response, rate_limit_info = make_rate_limited_request_with_retry(
              client,
              "tweets/search/recent?query=#{CGI.escape(quote_replies_query)}&expansions=author_id,referenced_tweets.id&tweet.fields=created_at,referenced_tweets,conversation_id&user.fields=username,name,profile_image_url&max_results=100"
            )

            if quote_replies_response
              quote_replies = process_tweets(quote_replies_response, quote_tweet_id)
              comments.concat(quote_replies)
            end
          end
        end

        # Use the last known rate limit info, or create default
        rate_limit_info ||= {
          limit: nil,
          remaining: nil,
          reset_at: nil
        }

        { comments: comments, rate_limit: rate_limit_info }
      else
        Rails.event.notify "twitter_service.fetch_post_failed",
          level: "error",
          component: "TwitterService",
          response: response.inspect
        default_response
      end
    rescue => e
      Rails.event.notify "twitter_service.fetch_comments_error",
        level: "error",
        component: "TwitterService",
        error_message: e.message,
        backtrace: e.backtrace.join("\n")
      default_response
    end
  end

  # Process tweets from API response and convert to comment format
  def process_tweets(search_response, parent_tweet_id)
    comments = []
    return comments unless search_response && search_response["data"]

    users_map = {}
    if search_response["includes"] && search_response["includes"]["users"]
      search_response["includes"]["users"].each do |user|
        users_map[user["id"]] = user
      end
    end

    # Build a map of referenced tweets for parent lookup
    referenced_tweets_map = {}
    if search_response["includes"] && search_response["includes"]["tweets"]
      search_response["includes"]["tweets"].each do |ref_tweet|
        referenced_tweets_map[ref_tweet["id"]] = ref_tweet
      end
    end

    search_response["data"].each do |tweet|
      author = users_map[tweet["author_id"]]
      next unless author

      # Find parent external_id from referenced_tweets
      parent_external_id = nil
      if tweet["referenced_tweets"]
        replied_to = tweet["referenced_tweets"].find { |ref| ref["type"] == "replied_to" }
        parent_external_id = replied_to["id"] if replied_to

        # If no replied_to, check if it's a quote tweet
        if parent_external_id.nil?
          quoted = tweet["referenced_tweets"].find { |ref| ref["type"] == "quoted" }
          parent_external_id = quoted["id"] if quoted
        end
      end

      # Use the provided parent_tweet_id if no parent found in referenced_tweets
      parent_external_id ||= parent_tweet_id

      comment_data = {
        external_id: tweet["id"],
        author_name: author["name"],
        author_username: author["username"],
        author_avatar_url: author["profile_image_url"],
        content: tweet["text"],
        published_at: Time.parse(tweet["created_at"]),
        url: "https://x.com/#{author["username"]}/status/#{tweet["id"]}",
        parent_external_id: parent_external_id
      }

      # Store conversation_id for quote tweets so we can fetch their replies
      if tweet["conversation_id"]
        comment_data[:conversation_id] = tweet["conversation_id"]
      end

      comments << comment_data
    end

    comments
  end

  private

  def create_client
    X::Client.new(
      api_key: @settings.api_key,
      api_key_secret: @settings.api_key_secret,
      access_token: @settings.access_token,
      access_token_secret: @settings.access_token_secret
    )
  end

  def create_v1_client
    # 为媒体上传创建 v1.1 客户端
    X::Client.new(
      api_key: @settings.api_key,
      api_key_secret: @settings.api_key_secret,
      access_token: @settings.access_token,
      access_token_secret: @settings.access_token_secret,
      base_url: "https://upload.twitter.com/1.1/"
    )
  end

  def upload_image(client, attachable)
    return nil unless attachable

    temp_file = nil
    begin
      # 创建临时文件来存储图片数据
      temp_file = create_temp_image_file(attachable)
      return nil unless temp_file

      # 使用 Twitter API v1.1 上传媒体
      upload_media_to_twitter(client, temp_file.path)
    rescue => e
      Rails.event.notify "twitter_service.upload_image_error",
        level: "error",
        component: "TwitterService",
        error_message: e.message
      nil
    ensure
      if temp_file
        temp_file.close rescue nil
        temp_file.unlink rescue nil
      end
    end
  end

  def upload_media_to_twitter(client, file_path)
    return nil unless File.exist?(file_path)

    # 使用 Twitter API v1.1 上传媒体
    # 注意：需要使用 v1.1 端点进行媒体上传
    v1_client = create_v1_client

    # 使用简单的媒体上传（非分块上传）
    begin
      # 创建 multipart/form-data 请求
      boundary = SecureRandom.hex
      upload_body = construct_upload_body(file_path, boundary)

      headers = {
        "Content-Type" => "multipart/form-data; boundary=#{boundary}",
        "Authorization" => build_oauth_header("POST", "https://upload.twitter.com/1.1/media/upload.json")
      }

      # 使用 v1.1 上传端点
      response = v1_client.post("media/upload.json", upload_body, headers: headers)

      Rails.event.notify "twitter_service.media_upload_response",
        level: "info",
        component: "TwitterService",
        response: response.inspect

      if response && (response["media_id"] || response["media_id_string"])
        media_id = response["media_id_string"] || response["media_id"].to_s
        Rails.event.notify "twitter_service.media_uploaded",
          level: "info",
          component: "TwitterService",
          media_id: media_id
        media_id
      else
        Rails.event.notify "twitter_service.media_upload_failed",
          level: "error",
          component: "TwitterService",
          response: response.inspect
        nil
      end
    rescue => e
      Rails.event.notify "twitter_service.media_upload_error",
        level: "error",
        component: "TwitterService",
        error_message: e.message,
        backtrace: e.backtrace.first(5).join("\n")
      nil
    end
  end

  def create_temp_image_file(attachable)
    return nil unless attachable

    begin
      image_data = case attachable
      when ActiveStorage::Blob
        attachable.download if attachable.content_type&.start_with?("image/")
      when ->(obj) { obj.class.name == "ActionText::Attachables::RemoteImage" }
        download_remote_image(attachable)
      else
        nil
      end

      return nil unless image_data

      # 创建临时文件
      temp_file = Tempfile.new([ "twitter_image", ".jpg" ], binmode: true)
      temp_file.write(image_data)
      temp_file.rewind
      temp_file
    rescue => e
      Rails.event.notify "twitter_service.temp_file_error",
        level: "error",
        component: "TwitterService",
        error_message: e.message
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
      component: "TwitterService",
      redirect_uri: redirect_uri.to_s
  end

  def log_download_error(error, url)
    Rails.event.notify "twitter_service.download_remote_image_error",
      level: "error",
      component: "TwitterService",
      error_message: error.message,
      url: url
  end

  # Build OAuth 1.0a authorization header for Twitter API
  def build_oauth_header(method, url, params = {})
    require "openssl"
    require "base64"
    require "cgi"

    oauth_params = {
      "oauth_consumer_key" => @settings.api_key,
      "oauth_token" => @settings.access_token,
      "oauth_signature_method" => "HMAC-SHA1",
      "oauth_timestamp" => Time.now.to_i.to_s,
      "oauth_nonce" => SecureRandom.hex(16),
      "oauth_version" => "1.0"
    }

    # Combine OAuth and request parameters
    all_params = oauth_params.merge(params)

    # Create signature base string
    sorted_params = all_params.sort.map { |k, v| "#{CGI.escape(k.to_s)}=#{CGI.escape(v.to_s)}" }.join("&")
    base_string = "#{method.upcase}&#{CGI.escape(url)}&#{CGI.escape(sorted_params)}"

    # Create signing key
    signing_key = "#{CGI.escape(@settings.api_key_secret)}&#{CGI.escape(@settings.access_token_secret)}"

    # Generate signature
    signature = Base64.strict_encode64(OpenSSL::HMAC.digest("SHA1", signing_key, base_string))
    oauth_params["oauth_signature"] = signature

    # Build header
    header_params = oauth_params.sort.map { |k, v| "#{k}=\"#{CGI.escape(v.to_s)}\"" }.join(", ")
    "OAuth #{header_params}"
  end

  def construct_upload_body(file_path, boundary)
    file_data = File.binread(file_path)
    filename = File.basename(file_path)
    media_category = "tweet_image"

    "--#{boundary}\r\n" \
      "Content-Disposition: form-data; name=\"media_category\"\r\n\r\n" \
      "#{media_category}\r\n" \
      "--#{boundary}\r\n" \
      "Content-Disposition: form-data; name=\"media\"; filename=\"#{filename}\"\r\n" \
      "Content-Type: image/jpeg\r\n\r\n" \
      "#{file_data}\r\n" \
      "--#{boundary}--\r\n"
  end

  def quote_tweet_id_for_article(article)
    source_url = article&.source_url.to_s.strip
    return nil if source_url.blank?

    host = extract_host_from_url(source_url)
    return nil unless x_dot_com_host?(host)

    extract_tweet_id_from_url(source_url)
  end

  def extract_host_from_url(url)
    normalized_url = url.match?(%r{^https?://}i) ? url : "https://#{url}"
    URI.parse(normalized_url).host&.downcase
  rescue URI::InvalidURIError
    nil
  end

  def x_dot_com_host?(host)
    return false if host.blank?

    host == "x.com" || host.end_with?(".x.com")
  end

  # Extract tweet ID from Twitter/X URL
  # Supports formats:
  # - https://twitter.com/username/status/1234567890
  # - https://x.com/username/status/1234567890
  def extract_tweet_id_from_url(url)
    return nil if url.blank?

    match = url.match(%r{(?:twitter\.com|x\.com)/(?:\w+/status|i/web/status)/(\d+)}i)
    match ? match[1] : nil
  end

  # Make a rate-limited API request with delay between requests
  # Twitter API v2 limits:
  # - GET /2/tweets/:id: 300 requests per 15 minutes (20 RPM)
  # - GET /2/tweets/search/recent: 180 requests per 15 minutes (12 RPM)
  # We use conservative 10 RPM (6 seconds between requests)
  def make_rate_limited_request(client, endpoint, max_retries: 3)
    retries = 0
    delay_between_requests = 6 # seconds - conservative 10 RPM limit

    begin
      # Add delay before request (except first request)
      if @last_request_time
        elapsed = Time.current - @last_request_time
        if elapsed < delay_between_requests
          sleep(delay_between_requests - elapsed)
        end
      end

      response = client.get(endpoint)
      @last_request_time = Time.current

      # Check for rate limit in response (X gem may include this in response)
      if response.is_a?(Hash) && response["errors"]
        error = response["errors"].first
        if error && (error["title"]&.include?("Too Many Requests") || error["detail"]&.include?("Too Many Requests"))
          raise "Rate limit exceeded: #{error["detail"] || error["title"]}"
        end
      end

      response
    rescue => e
      error_message = e.message.to_s
      if error_message.include?("Too Many Requests") || error_message.include?("Rate limit") || error_message.include?("429")
        retries += 1
        if retries <= max_retries
          wait_time = calculate_backoff_time(retries)
          Rails.event.notify "twitter_service.rate_limit_retry",
            level: "warn",
            component: "TwitterService",
            wait_time: wait_time,
            retry_count: retries,
            max_retries: max_retries
          sleep(wait_time)
          retry
        else
          Rails.event.notify "twitter_service.rate_limit_exceeded",
            level: "error",
            component: "TwitterService",
            max_retries: max_retries
          handle_rate_limit_exceeded({
            limit: 180,
            remaining: 0,
            reset_at: Time.current + 15.minutes
          })
          raise
        end
      else
        raise
      end
    end
  end

  # Make a rate-limited request with retry logic and return rate limit info
  def make_rate_limited_request_with_retry(client, endpoint, max_retries: 3)
    retries = 0
    delay_between_requests = 6 # seconds - conservative 10 RPM limit

    begin
      # Add delay before request (except first request)
      if @last_request_time
        elapsed = Time.current - @last_request_time
        if elapsed < delay_between_requests
          sleep(delay_between_requests - elapsed)
        end
      end

      response = client.get(endpoint)
      @last_request_time = Time.current

      # Check for rate limit errors
      if response.is_a?(Hash) && response["errors"]
        error = response["errors"].first
        if error && (error["title"]&.include?("Too Many Requests") || error["detail"]&.include?("Too Many Requests"))
          raise "Rate limit exceeded: #{error["detail"] || error["title"]}"
        end
      end

      # Try to extract rate limit info from response
      # Note: X gem may not expose headers directly, so we estimate based on API limits
      rate_limit_info = {
        limit: 180, # Twitter search API limit per 15 minutes
        remaining: nil, # X gem may not expose this
        reset_at: Time.current + 15.minutes # Reset window is 15 minutes
      }

      [ response, rate_limit_info ]
    rescue => e
      error_message = e.message.to_s
      if error_message.include?("Too Many Requests") || error_message.include?("Rate limit") || error_message.include?("429")
        retries += 1
        if retries <= max_retries
          wait_time = calculate_backoff_time(retries)
          Rails.event.notify "twitter_service.rate_limit_hit",
            level: "warn",
            component: "TwitterService",
            wait_time: wait_time,
            retry_count: retries,
            max_retries: max_retries

          # Log rate limit exceeded
          handle_rate_limit_exceeded({
            limit: 180,
            remaining: 0,
            reset_at: Time.current + wait_time
          })

          sleep(wait_time)
          retry
        else
          Rails.event.notify "twitter_service.rate_limit_max_retries",
            level: "error",
            component: "TwitterService",
            max_retries: max_retries
          handle_rate_limit_exceeded({
            limit: 180,
            remaining: 0,
            reset_at: Time.current + 15.minutes
          })
          [ nil, { limit: 180, remaining: 0, reset_at: Time.current + 15.minutes } ]
        end
      else
        raise
      end
    end
  end

  # Calculate exponential backoff time for retries
  def calculate_backoff_time(retry_count)
    # Exponential backoff: 15s, 30s, 60s
    base_wait = 15
    [ base_wait * (2 ** (retry_count - 1)), 300 ].min # Cap at 5 minutes
  end

  # Handle rate limit exceeded (429 response)
  def handle_rate_limit_exceeded(rate_limit_info)
    reset_time = rate_limit_info[:reset_at] || Time.current + 15.minutes
    wait_seconds = [ (reset_time - Time.current).to_i, 0 ].max

    Rails.event.notify "twitter_service.rate_limit_exceeded_event",
      level: "error",
      component: "TwitterService",
      reset_time: reset_time,
      wait_seconds: wait_seconds

    ActivityLog.log!(
      action: :rate_limited,
      target: :twitter_api,
      level: :error,
      reset_at: reset_time,
      remaining: rate_limit_info[:remaining],
      limit: rate_limit_info[:limit]
    )
  end

  # Log rate limit status for monitoring
  def log_rate_limit_status(rate_limit_info)
    return unless rate_limit_info[:remaining]

    if rate_limit_info[:remaining] < 20
      Rails.event.notify "twitter_service.rate_limit_low",
        level: "warn",
        component: "TwitterService",
        remaining: rate_limit_info[:remaining],
        limit: rate_limit_info[:limit],
        reset_at: rate_limit_info[:reset_at]

      ActivityLog.log!(
        action: :rate_limit_low,
        target: :twitter_api,
        level: :warn,
        remaining: rate_limit_info[:remaining],
        limit: rate_limit_info[:limit],
        reset_at: rate_limit_info[:reset_at]
      )
    elsif rate_limit_info[:remaining] < 50
      Rails.event.notify "twitter_service.rate_limit_status",
        level: "info",
        component: "TwitterService",
        remaining: rate_limit_info[:remaining],
        limit: rate_limit_info[:limit]
    end
  end
end
