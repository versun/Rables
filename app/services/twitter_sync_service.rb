require "x"
require "cgi"
require "net/http"
require "open-uri"

# Archives original tweets (and quote tweets) from a configured Twitter/X
# account as published Articles. Replies and pure retweets are excluded.
#
# Media (photos, videos, GIFs) from the tweets and their quoted tweets is
# downloaded and uploaded to ActiveStorage, then embedded in the article
# content. Uses the API credentials from
# Crosspost.for("twitter") and the shared TwitterApi::RateLimiter.
class TwitterSyncService
  FIRST_RUN_LIMIT = 10 # On the first run only backfill the latest 10 tweets
  MAX_PAGES_PER_SYNC = 10 # Safety cap for timeline pagination in a single run
  QUOTED_CONTENT_LIMIT = 250 # Max length of a quoted tweet stored as source_content

  def initialize(sync = TwitterSync.instance)
    @sync = sync
    @settings = Crosspost.for("twitter")
    @rate_limiter = TwitterApi::RateLimiter.new
  end

  def perform(force: false)
    return unless @sync.enabled? && @sync.username.present? && @settings&.enabled?
    return unless force || @sync.due_to_sync?

    client = create_client
    user_id = resolve_user_id(client)
    unless user_id
      record_failure("user not found: #{@sync.username}")
      return
    end

    tweets, includes = fetch_new_tweets(client, user_id)
    tweets = tweets.uniq { |tweet| tweet["id"] }.sort_by { |tweet| tweet["id"].to_i }
    tweets = tweets.last(FIRST_RUN_LIMIT) if @sync.since_id.blank? && @sync.start_date.blank?

    # Archive tweet by tweet: a single broken ("poison") tweet is logged and
    # skipped instead of aborting the whole run. since_id below is computed
    # from every fetched tweet, so it advances regardless of per-tweet
    # failures and the poison tweet is never retried.
    tweets.each do |tweet|
      archive_tweet(tweet, includes)
    rescue => e
      log_tweet_failure(tweet, e)
    end

    latest_id = tweets.map { |tweet| tweet["id"].to_i }.max
    @sync.update!(
      since_id: latest_id ? latest_id.to_s : @sync.since_id,
      last_synced_at: Time.current,
      last_error: nil
    )
  rescue => e
    record_failure(e.message)
  end

  private

  def record_failure(message)
    Rails.event.notify "twitter_sync_service.sync_failed",
      level: "error",
      component: "TwitterSyncService",
      error_message: message
    @sync.update_columns(last_error: message)
    ActivityLog.log!(
      action: :failed,
      target: :twitter_sync,
      level: :error,
      error: message
    )
  end

  def log_tweet_failure(tweet, error)
    message = "tweet #{tweet['id']}: #{error.message}"
    Rails.event.notify "twitter_sync_service.tweet_archive_failed",
      level: "error",
      component: "TwitterSyncService",
      tweet_id: tweet["id"],
      error_message: error.message
    ActivityLog.log!(
      action: :failed,
      target: :twitter_sync,
      level: :error,
      error: message
    )
  end

  def create_client
    X::Client.new(
      api_key: @settings.api_key,
      api_key_secret: @settings.api_key_secret,
      access_token: @settings.access_token,
      access_token_secret: @settings.access_token_secret
    )
  end

  def resolve_user_id(client)
    return @sync.user_id if @sync.user_id.present?

    response = @rate_limiter.make_request(client, "users/by/username/#{CGI.escape(@sync.username)}")
    user_id = response&.dig("data", "id")
    @sync.update!(user_id: user_id) if user_id.present?
    user_id
  end

  # Fetches the timeline, following pagination whenever the API returns a
  # next_token (both for the initial backfill and for incremental syncs, so
  # a burst of >100 tweets between runs leaves no permanent gap). Returns
  # [tweets, includes] where includes holds lookup hashes for media,
  # referenced (quoted) tweets, and their authors.
  def fetch_new_tweets(client, user_id)
    tweets = []
    includes = { media: {}, tweets: {}, users: {} }
    pagination_token = nil
    pages = 0

    loop do
      response = fetch_timeline(client, user_id, pagination_token)
      raise api_error_message(response) if api_error_response?(response)

      tweets.concat(Array(response&.dig("data")))
      merge_includes!(includes, response)

      pagination_token = response&.dig("meta", "next_token")
      pages += 1
      break unless pagination_token.present? && pages < MAX_PAGES_PER_SYNC
    end

    [ tweets, includes ]
  end

  # A successful timeline page always carries "data" (possibly empty) or a
  # meta result_count (when there are no new tweets the API omits "data").
  # Anything else is an error payload and must fail the sync instead of
  # being recorded as a successful run.
  def api_error_response?(response)
    return true if response.nil?
    return false if response.key?("data")

    response.dig("meta", "result_count").nil?
  end

  def api_error_message(response)
    return "Twitter API returned no response" if response.nil?

    messages = Array(response["errors"]).filter_map do |error|
      error["message"] || error["detail"] || error["title"]
    end
    messages.presence&.join(", ") || response["title"].presence || "Twitter API returned an unexpected response"
  end

  def fetch_timeline(client, user_id, pagination_token = nil)
    endpoint = "users/#{user_id}/tweets" \
      "?exclude=retweets,replies" \
      "&max_results=100" \
      "&tweet.fields=created_at,attachments,referenced_tweets,note_tweet,entities,author_id" \
      "&expansions=attachments.media_keys,referenced_tweets.id,referenced_tweets.id.author_id,referenced_tweets.id.attachments.media_keys" \
      "&media.fields=url,preview_image_url,type,variants,alt_text" \
      "&user.fields=name,username"
    endpoint += "&since_id=#{@sync.since_id}" if @sync.since_id.present?
    if @sync.start_date.present?
      endpoint += "&start_time=#{CGI.escape(@sync.start_date.in_time_zone.beginning_of_day.iso8601)}"
    end
    endpoint += "&pagination_token=#{pagination_token}" if pagination_token.present?

    @rate_limiter.make_request(client, endpoint)
  end

  def merge_includes!(includes, response)
    data = response&.dig("includes") || {}
    includes[:media].merge!(Array(data["media"]).index_by { |media| media["media_key"] })
    includes[:tweets].merge!(Array(data["tweets"]).index_by { |tweet| tweet["id"] })
    includes[:users].merge!(Array(data["users"]).index_by { |user| user["id"] })
  end

  def archive_tweet(tweet, includes)
    # Defensive filter: exclude retweets/replies even if the API returned them
    referenced_tweets = tweet["referenced_tweets"] || []
    return if referenced_tweets.any? { |ref| %w[retweeted replied_to].include?(ref["type"]) }
    return if article_announcement?(tweet)
    return if before_start_date?(tweet)

    slug = "tweet-#{tweet['id']}"
    return if Article.exists?(slug: slug)

    full_text = tweet.dig("note_tweet", "text").presence || tweet["text"].to_s
    quoted_id = referenced_tweets.find { |ref| ref["type"] == "quoted" }&.dig("id")
    full_text = resolve_tco_links(full_text, tweet, quoted_id: quoted_id)
    source_url = quoted_id.present? ? "https://x.com/i/web/status/#{quoted_id}" : nil
    source_author, source_content = quoted_source_reference(quoted_id, includes)
    blobs = build_media_attachments(tweet, includes[:media])
    quoted_tweet = includes[:tweets][quoted_id] if quoted_id.present?
    blobs += build_media_attachments(quoted_tweet, includes[:media]) if quoted_tweet

    article = Article.create!(
      title: nil,
      slug: slug,
      status: :publish,
      created_at: tweet["created_at"].present? ? Time.parse(tweet["created_at"]) : Time.current,
      source_url: source_url,
      source_author: source_author,
      source_content: source_content,
      content: build_tweet_content(full_text, blobs)
    )
    # The tweet's own URL belongs to the article's crosspost section, not Source Reference
    article.social_media_posts.create!(
      platform: "twitter",
      url: "https://x.com/#{@sync.username}/status/#{tweet['id']}"
    )

    ActivityLog.log!(
      action: :posted,
      target: :twitter_sync,
      level: :info,
      slug: article.slug,
      url: article.source_url
    )
    article
  end

  # Fills the Source Reference (author + quote) of a quoted tweet from the
  # timeline's expanded includes. Missing data (deleted/protected tweets) is
  # non-fatal: the article is archived with just the source URL.
  def quoted_source_reference(quoted_id, includes)
    return [ nil, nil ] if quoted_id.blank?

    quoted = includes[:tweets][quoted_id]
    return [ nil, nil ] unless quoted

    author = includes[:users][quoted["author_id"]]&.dig("name")
    content = quoted.dig("note_tweet", "text").presence || quoted["text"].to_s
    content = resolve_tco_links(content, quoted)
    content = content[0, QUOTED_CONTENT_LIMIT] if content.length > QUOTED_CONTENT_LIMIT

    [ author, content ]
  end

  # X Articles are not exposed by the API (only a t.co wrapper in the text),
  # so announcement tweets linking to x.com/i/article are skipped entirely.
  def article_announcement?(tweet)
    Array(tweet.dig("entities", "urls")).any? do |url|
      url["expanded_url"].to_s.match?(%r{\Ahttps?://(www\.)?(x\.com|twitter\.com)/i/article/}i)
    end
  end

  def before_start_date?(tweet)
    return false if @sync.start_date.blank? || tweet["created_at"].blank?

    Time.parse(tweet["created_at"]) < @sync.start_date.in_time_zone.beginning_of_day
  end

  # Resolves t.co short links to their real destinations, using the API's
  # url entities first and falling back to following the HTTP redirect.
  # Links that are redundant with the article are removed instead: the
  # tweet's own media attachments (downloaded into the content) and the
  # quoted tweet (stored as Source Reference).
  def resolve_tco_links(text, tweet, quoted_id: nil)
    entities = Array(tweet.dig("entities", "urls")) + Array(tweet.dig("note_tweet", "entities", "urls"))
    replacements = {}
    removable = []
    entities.each do |entity|
      short = entity["url"].presence
      next unless short

      expanded = entity["expanded_url"].presence
      if expanded && redundant_link?(expanded, tweet["id"], quoted_id)
        removable << short
      elsif expanded && !tco_link?(expanded)
        replacements[short] = expanded
      end
    end

    text.gsub(%r{https://t\.co/\w+}) do |short|
      if removable.include?(short)
        ""
      else
        resolved = replacements[short] || follow_redirect(short)
        resolved && redundant_link?(resolved, tweet["id"], quoted_id) ? "" : (resolved || short)
      end
    end.gsub(/[ \t]+$/, "") # removing a link can leave trailing whitespace
  end

  # A link is redundant when it points at the tweet's own media attachments
  # (embedded in the content) or at the quoted tweet (kept in Source Reference).
  def redundant_link?(url, tweet_id, quoted_id)
    return true if tweet_id.present? && url.match?(%r{/status/#{tweet_id}/(photo|video)/\d+}i)
    return true if quoted_id.present? && url.match?(%r{/(x\.com|twitter\.com)/[^/]+/status/#{quoted_id}/?\z}i)

    false
  end

  def tco_link?(url)
    url.match?(%r{\Ahttps?://t\.co/}i)
  end

  # Follows redirects (HEAD) up to a limit and returns the final URL,
  # or nil when resolution fails so the caller keeps the original text.
  def follow_redirect(url, limit = 5)
    location = redirect_location(url)
    return url if location.blank?
    return url if limit <= 1

    follow_redirect(URI.join(url, location).to_s, limit - 1)
  rescue => e
    Rails.event.notify "twitter_sync_service.link_resolution_failed",
      level: "warn",
      component: "TwitterSyncService",
      url: url,
      error_message: e.message
    nil
  end

  def redirect_location(url)
    uri = URI.parse(url)
    response = Net::HTTP.start(uri.host, uri.port,
      use_ssl: uri.scheme == "https", open_timeout: 5, read_timeout: 5) do |http|
      http.head(uri.request_uri.presence || "/")
    end
    response.is_a?(Net::HTTPRedirection) ? response["location"] : nil
  end

  def build_tweet_content(full_text, blobs)
    parts = full_text.split("\n").map(&:strip).reject(&:blank?).map do |line|
      "<p>#{CGI.escapeHTML(line)}</p>"
    end
    parts << "<p></p>" if parts.empty?

    blobs.each do |blob|
      parts << ActionText::Attachment.from_attachable(blob).node.to_html
    end

    ActionText::Content.new(parts.join)
  end

  def build_media_attachments(tweet, media_by_key)
    media_keys = tweet.dig("attachments", "media_keys") || []
    media_keys.filter_map do |key|
      media = media_by_key[key]
      next unless media

      url, content_type = media_download_url(media)
      next unless url

      begin
        download_media(url, content_type, tweet["id"])
      rescue => e
        Rails.event.notify "twitter_sync_service.media_download_failed",
          level: "warn",
          component: "TwitterSyncService",
          tweet_id: tweet["id"],
          url: url,
          error_message: e.message
        nil
      end
    end
  end

  def media_download_url(media)
    case media["type"]
    when "photo"
      url = media["url"]
      return nil if url.blank?

      [ url, Marcel::MimeType.for(name: File.basename(URI.parse(url).path)) ]
    when "video", "animated_gif"
      variant = Array(media["variants"])
        .select { |v| v["content_type"] == "video/mp4" }
        .max_by { |v| v["bitrate"].to_i }
      variant ? [ variant["url"], "video/mp4" ] : nil
    end
  end

  def download_media(url, content_type, tweet_id)
    extension = File.extname(URI.parse(url).path)
    URI.open(url) do |io|
      ActiveStorage::Blob.create_and_upload!(
        io: io,
        filename: "tweet-#{tweet_id}-#{SecureRandom.hex(4)}#{extension}",
        content_type: content_type
      )
    end
  end
end
