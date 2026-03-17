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
    @rate_limiter = TwitterApi::RateLimiter.new
    @media_uploader = TwitterApi::MediaUploader.new(@settings)
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
      username = fetch_username(client)

      images = tweet_images_for_article(article, quote_tweet_id)
      Rails.event.notify "twitter_service.images_count",
        level: "info",
        component: "TwitterService",
        count: images.size

      media_ids = upload_article_images(client, images)

      tweet_data = build_tweet_data(tweet, quote_tweet_id, media_ids)

      Rails.event.notify "twitter_service.sending_tweet",
        level: "info",
        component: "TwitterService",
        tweet_data: tweet_data.inspect

      response = create_tweet_with_retry(client, tweet_data)

      handle_tweet_response(response, article, tweet, quote_tweet_id, media_ids, client, username)
    rescue => e
      log_post_error(e, article)
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
      tweet_id = extract_tweet_id_from_url(post_url)
      return default_response unless tweet_id

      client = create_client

      response = @rate_limiter.make_request(client, "tweets/#{tweet_id}?expansions=author_id,referenced_tweets.id&tweet.fields=conversation_id,created_at,author_id&user.fields=username,name,profile_image_url")

      if response && response["data"]
        fetch_conversation_comments(client, response, tweet_id, post_url)
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

    users_map = build_users_map(search_response)
    referenced_tweets_map = build_referenced_tweets_map(search_response)

    search_response["data"].each do |tweet|
      author = users_map[tweet["author_id"]]
      next unless author

      parent_external_id = find_parent_external_id(tweet, parent_tweet_id)

      comment_data = build_comment_data(tweet, author, parent_external_id)
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

  def fetch_username(client)
    user = client.get("users/me")
    user&.dig("data", "username")
  rescue => e
    Rails.event.notify "twitter_service.fetch_username_failed",
      level: "warn",
      component: "TwitterService",
      error_message: e.message
    nil
  end

  def upload_article_images(client, images)
    media_ids = []
    images.each do |image|
      Rails.event.notify "twitter_service.upload_image_attempt",
        level: "info",
        component: "TwitterService",
        image_type: image.class.to_s
      media_id = @media_uploader.upload(client, image)
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

    media_ids
  end

  def build_tweet_data(tweet, quote_tweet_id, media_ids)
    tweet_data = { text: tweet }
    tweet_data[:quote_tweet_id] = quote_tweet_id if quote_tweet_id
    if media_ids.any?
      tweet_data[:media] = { media_ids: media_ids.map(&:to_s) }
    end
    tweet_data
  end

  def create_tweet_with_retry(client, tweet_data)
    @rate_limiter.with_retry do
      client.post("tweets", tweet_data.to_json)
    end
  end

  def handle_tweet_response(response, article, tweet, quote_tweet_id, media_ids, client, username = nil)
    if successful_tweet_response?(response)
      id = response["data"]["id"]
      log_successful_post(article, id)
      build_tweet_url(id, username)
    else
      error_message = extract_error_message(response)
      Rails.event.notify "twitter_service.tweet_failed",
        level: "error",
        component: "TwitterService",
        error_message: error_message

      if media_ids.any? && media_error?(error_message)
        try_text_only_tweet(client, tweet, quote_tweet_id, article, error_message, username)
      else
        log_failed_post(article, error_message)
        nil
      end
    end
  end

  def try_text_only_tweet(client, tweet, quote_tweet_id, article, original_error, username = nil)
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
      fallback_response = create_tweet_with_retry(client, text_only_data)

      if successful_tweet_response?(fallback_response)
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
        build_tweet_url(id, username)
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
        error: "#{original_error} (fallback_failed: #{fallback_error.message})"
      )
      nil
    end
  end

  def log_successful_post(article, post_id)
    ActivityLog.log!(
      action: :posted,
      target: :crosspost,
      level: :info,
      title: article.title,
      slug: article.slug,
      platform: "twitter",
      post_id: post_id
    )
  end

  def log_failed_post(article, error_message)
    ActivityLog.log!(
      action: :failed,
      target: :crosspost,
      level: :error,
      title: article.title,
      slug: article.slug,
      platform: "twitter",
      error: error_message
    )
  end

  def log_post_error(error, article)
    Rails.event.notify "twitter_service.post_error",
      level: "error",
      component: "TwitterService",
      error_message: error.message
    ActivityLog.log!(
      action: :failed,
      target: :crosspost,
      level: :error,
      title: article.title,
      slug: article.slug,
      platform: "twitter",
      error: error.message
    )
  end

  def successful_tweet_response?(response)
    response && response["data"] && response["data"]["id"]
  end

  def extract_error_message(response)
    response&.dig("errors")&.first&.dig("message") || "Unknown error"
  end

  def media_error?(error_message)
    error_message.to_s.downcase.include?("media")
  end

  def fetch_conversation_comments(client, response, tweet_id, post_url)
    conversation_id = response["data"]["conversation_id"]
    comments = []
    rate_limit_info = nil

    # 1. Get direct replies
    replies_query = "conversation_id:#{conversation_id} is:reply"
    replies_response, rate_limit_info = @rate_limiter.make_request_with_info(
      client,
      "tweets/search/recent?query=#{CGI.escape(replies_query)}&expansions=author_id,referenced_tweets.id&tweet.fields=created_at,referenced_tweets,conversation_id&user.fields=username,name,profile_image_url&max_results=100"
    )

    comments.concat(process_tweets(replies_response, tweet_id)) if replies_response

    # 2. Get quote tweets
    quote_tweets = fetch_quote_tweets(client, post_url, tweet_id, rate_limit_info)
    comments.concat(quote_tweets[:tweets])
    rate_limit_info = quote_tweets[:rate_limit_info] || rate_limit_info

    # 3. Get replies to quote tweets
    quote_tweets[:tweets].each do |quote_tweet|
      quote_replies = fetch_quote_tweet_replies(client, quote_tweet)
      comments.concat(quote_replies[:tweets])
      rate_limit_info = quote_replies[:rate_limit_info] || rate_limit_info
    end

    rate_limit_info ||= { limit: nil, remaining: nil, reset_at: nil }

    { comments: comments, rate_limit: rate_limit_info }
  end

  def fetch_quote_tweets(client, post_url, tweet_id, rate_limit_info)
    begin
      quote_query = "url:#{CGI.escape(post_url)} is:quote"
      quote_response, rate_limit_info = @rate_limiter.make_request_with_info(
        client,
        "tweets/search/recent?query=#{CGI.escape(quote_query)}&expansions=author_id,referenced_tweets.id&tweet.fields=created_at,referenced_tweets,conversation_id&user.fields=username,name,profile_image_url&max_results=100"
      )

      tweets = []
      if quote_response && quote_response["data"]
        tweets = process_tweets(quote_response, tweet_id)
        Rails.event.notify "twitter_service.quote_tweets_found",
          level: "info",
          component: "TwitterService",
          count: tweets.length,
          tweet_id: tweet_id
      end
      { tweets: tweets, rate_limit_info: rate_limit_info }
    rescue => e
      Rails.event.notify "twitter_service.quote_tweets_failed",
        level: "warn",
        component: "TwitterService",
        error_message: e.message
      { tweets: [], rate_limit_info: rate_limit_info }
    end
  end

  def fetch_quote_tweet_replies(client, quote_tweet)
    quote_tweet_id = quote_tweet[:external_id]
    quote_conversation_id = quote_tweet[:conversation_id]

    return { tweets: [], rate_limit_info: nil } unless quote_conversation_id

    quote_replies_query = "conversation_id:#{quote_conversation_id} is:reply"
    quote_replies_response, rate_limit_info = @rate_limiter.make_request_with_info(
      client,
      "tweets/search/recent?query=#{CGI.escape(quote_replies_query)}&expansions=author_id,referenced_tweets.id&tweet.fields=created_at,referenced_tweets,conversation_id&user.fields=username,name,profile_image_url&max_results=100"
    )

    tweets = quote_replies_response ? process_tweets(quote_replies_response, quote_tweet_id) : []
    { tweets: tweets, rate_limit_info: rate_limit_info }
  end

  def build_users_map(search_response)
    users_map = {}
    if search_response["includes"] && search_response["includes"]["users"]
      search_response["includes"]["users"].each do |user|
        users_map[user["id"]] = user
      end
    end
    users_map
  end

  def build_referenced_tweets_map(search_response)
    referenced_tweets_map = {}
    if search_response["includes"] && search_response["includes"]["tweets"]
      search_response["includes"]["tweets"].each do |ref_tweet|
        referenced_tweets_map[ref_tweet["id"]] = ref_tweet
      end
    end
    referenced_tweets_map
  end

  def find_parent_external_id(tweet, default_parent_id)
    return default_parent_id unless tweet["referenced_tweets"]

    replied_to = tweet["referenced_tweets"].find { |ref| ref["type"] == "replied_to" }
    return replied_to["id"] if replied_to

    quoted = tweet["referenced_tweets"].find { |ref| ref["type"] == "quoted" }
    return quoted["id"] if quoted

    default_parent_id
  end

  def build_comment_data(tweet, author, parent_external_id)
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

    comment_data[:conversation_id] = tweet["conversation_id"] if tweet["conversation_id"]
    comment_data
  end

  def quote_tweet_id_for_article(article)
    source_url = article&.source_url.to_s.strip
    return nil if source_url.blank?

    parsed_url = parse_twitter_status_url(source_url)
    return nil unless parsed_url

    extract_tweet_id_from_path(parsed_url.path)
  end

  def normalize_url(url)
    normalized_url = url.match?(%r{^https?://}i) ? url : "https://#{url}"
    URI.parse(normalized_url)
  rescue URI::InvalidURIError
    nil
  end

  def extract_host_from_url(url)
    normalize_url(url)&.host&.downcase
  end

  def parse_twitter_status_url(url)
    normalized_uri = normalize_url(url)
    return nil unless normalized_uri

    host = normalized_uri.host&.downcase
    return nil unless twitter_host?(host)
    return nil unless extract_tweet_id_from_path(normalized_uri.path)

    normalized_uri
  end

  def twitter_host?(host)
    return false if host.blank?

    [ "x.com", "twitter.com" ].any? do |domain|
      host == domain || host.end_with?(".#{domain}")
    end
  end

  def extract_tweet_id_from_path(path)
    return nil if path.blank?

    match = path.match(%r{\A/(?:i/(?:web/)?status|[^/]+/status|statuses)/(\d+)(?:/.*)?\z}i)
    match ? match[1] : nil
  end

  def tweet_images_for_article(article, quote_tweet_id)
    if quote_tweet_id.present?
      Rails.event.notify "twitter_service.skipping_media_for_quote_tweet",
        level: "info",
        component: "TwitterService",
        article_id: article.id,
        quote_tweet_id: quote_tweet_id
      []
    else
      limit_twitter_media_attachments(article.all_image_attachments(4))
    end
  end

  def limit_twitter_media_attachments(images)
    animated_gif = images.find { |image| animated_gif_attachable?(image) }
    return images unless animated_gif

    Rails.event.notify "twitter_service.using_single_gif_attachment",
      level: "info",
      component: "TwitterService"

    [ animated_gif ]
  end

  def animated_gif_attachable?(attachable)
    case attachable
    when ActiveStorage::Blob
      attachable.content_type == "image/gif"
    when ->(obj) { obj.class.name == "ActionText::Attachables::RemoteImage" }
      remote_gif_attachable_url?(attachable.try(:url))
    else
      false
    end
  end

  def remote_gif_attachable_url?(url)
    return false if url.blank?

    uri = URI.parse(url)
    File.extname(uri.path).downcase == ".gif"
  rescue URI::InvalidURIError
    false
  end

  def extract_tweet_id_from_url(url)
    normalized_uri = normalize_url(url)
    return nil unless normalized_uri

    extract_tweet_id_from_path(normalized_uri.path)
  end

  def build_tweet_url(tweet_id, username = nil)
    return if tweet_id.blank?

    if username.present?
      "https://x.com/#{username}/status/#{tweet_id}"
    else
      "https://x.com/i/web/status/#{tweet_id}"
    end
  end

  def calculate_backoff_time(retry_count)
    @rate_limiter.calculate_backoff_time(retry_count)
  end

  # HttpRedirectHandler callbacks
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
end
