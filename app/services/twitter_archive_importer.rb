require "json"
require "stringio"
require "uri"
require "zip"

class TwitterArchiveImporter
  class ImportError < StandardError; end
  MEDIA_DIRECTORIES = %w[data/tweets_media/ data/tweet_media/].freeze

  attr_reader :source, :progress_callback

  def initialize(source, progress_callback: nil)
    @source = source
    @progress_callback = progress_callback
    @last_progress = nil
    @last_message = nil
  end

  def import!
    report_progress(5, "Validating archive")
    report_progress(25, "Scanning archive")
    archive_data = parse_archive_data
    summary = build_summary(archive_data)

    raise ImportError, "No supported archive items found in archive" if summary[:total_items].zero?

    previous_blob_ids = existing_archive_media_blob_ids
    report_progress(55, "Archive parsed")
    report_progress(80, "Replacing stored archive")

    Zip::File.open(source_path) do |zip|
      ActiveRecord::Base.transaction do
        TwitterArchiveTweet.destroy_all
        TwitterArchiveConnection.destroy_all
        TwitterArchiveLike.destroy_all

        archive_data[:tweets].each do |row|
          media_entry_names = row.delete(:media_entry_names)
          tweet = TwitterArchiveTweet.create!(row)
          attach_media_files(tweet, zip, media_entry_names)
        end

        archive_data[:followers].each do |row|
          TwitterArchiveConnection.create!(row)
        end

        archive_data[:following].each do |row|
          TwitterArchiveConnection.create!(row)
        end

        archive_data[:likes].each do |row|
          TwitterArchiveLike.create!(row)
        end
      end
    end

    report_progress(95, "Cleaning up media")
    purge_replaced_media_blobs(previous_blob_ids)
    report_progress(100, "Import completed")

    summary
  end

  private

  def parse_archive_data
    ensure_zip_file!

    tweets_by_id = {}
    seen_connections = {}
    seen_likes = {}
    account_name = nil
    followers = []
    following = []
    likes = []

    Zip::File.open(source_path) do |zip|
      media_entries_by_tweet_id = build_media_index(zip)

      zip.each do |entry|
        next if entry.directory?
        next unless archive_data_entry?(entry)

        content = entry.get_input_stream.read.to_s
        payload = parse_js_payload(content)
        next if payload.nil?

        account_name ||= extract_account_name(payload)

        extract_tweets(payload).each do |tweet|
          tweet_id = extract_tweet_id(tweet)
          next if tweet_id.blank?

          row = build_tweet_row(tweet, tweet_id, account_name, media_entries_for(tweet, tweet_id, media_entries_by_tweet_id))
          has_media = row[:media_entry_names].present?
          next if row[:tweeted_at].blank?
          next if row[:full_text].blank? && !has_media

          tweets_by_id[tweet_id] = merge_tweet_rows(tweets_by_id[tweet_id], row)
        end

        case archive_entry_type(entry)
        when "follower"
          extract_connection_rows(payload, "follower").each do |row|
            key = "#{row[:relationship_type]}:#{row[:account_id]}"
            next if seen_connections.key?(key)

            seen_connections[key] = true
            followers << row
          end
        when "following"
          extract_connection_rows(payload, "following").each do |row|
            key = "#{row[:relationship_type]}:#{row[:account_id]}"
            next if seen_connections.key?(key)

            seen_connections[key] = true
            following << row
          end
        when "like"
          extract_like_rows(payload).each do |row|
            next if seen_likes.key?(row[:tweet_id])

            seen_likes[row[:tweet_id]] = true
            likes << row
          end
        end
      end
    end

    tweets_by_id.each_value do |row|
      row[:screen_name] = account_name if row[:screen_name] == "archive" && account_name.present?
    end

    {
      tweets: tweets_by_id.values,
      followers: followers,
      following: following,
      likes: likes
    }
  rescue Zip::Error, JSON::ParserError, ArgumentError => e
    raise ImportError, e.message
  end

  def ensure_zip_file!
    raise ImportError, "Archive file not found" unless source_path.present? && File.exist?(source_path)
    raise ImportError, "Archive file must be a zip" unless File.extname(source_path.to_s).downcase == ".zip"
  end

  def parse_js_payload(content)
    return nil if content.blank?

    json = content.strip
    return JSON.parse(json) if json.start_with?("{", "[")

    json = json.sub(/\A.*?=\s*/m, "").strip
    json = json.sub(/;\s*\z/m, "")
    return nil if json.blank?

    JSON.parse(json)
  end

  def archive_data_entry?(entry)
    entry.name.start_with?("data/") && entry.name.end_with?(".js", ".json")
  end

  def archive_media_entry?(entry)
    MEDIA_DIRECTORIES.any? { |directory| entry.name.start_with?(directory) }
  end

  def archive_entry_type(entry)
    File.basename(entry.name.to_s, File.extname(entry.name.to_s))
  end

  def build_media_index(zip)
    zip.each_with_object(Hash.new { |hash, key| hash[key] = [] }) do |entry, index|
      next if entry.directory?
      next unless archive_media_entry?(entry)

      entry_name = entry.name.to_s
      extract_media_tweet_ids(entry_name).each do |tweet_id|
        index[tweet_id] << entry_name
      end
    end.transform_values { |entry_names| entry_names.uniq.sort }
  end

  def extract_media_tweet_ids(entry_name)
    basename = File.basename(entry_name.to_s, File.extname(entry_name.to_s))
    tweet_id = basename[/\A\d+/]
    tweet_id.present? ? [ tweet_id ] : []
  end

  def extract_account_name(payload)
    items = payload.is_a?(Array) ? payload : [ payload ]

    items.each do |item|
      account = item["account"] || item[:account] if item.respond_to?(:[])
      username = account&.dig("username") || account&.dig(:username)
      return username if username.present?
    end

    nil
  end

  def extract_tweets(payload)
    items = payload.is_a?(Array) ? payload : [ payload ]

    items.filter_map do |item|
      tweet = item["tweet"] || item[:tweet] if item.respond_to?(:[])
      tweet ||= item if tweet_like_hash?(item)
      tweet.is_a?(Hash) ? tweet : nil
    end
  end

  def extract_tweet_id(tweet)
    tweet["id_str"].presence || tweet["id"].presence || tweet["tweet_id"].presence
  end

  def extract_tweeted_at(tweet)
    value = tweet["created_at"].presence || tweet["createdAt"].presence
    value ||= tweet.dig("legacy", "created_at").presence || tweet.dig(:legacy, :created_at).presence
    return Time.zone.parse(value) if value.present?

    value = tweet["timestamp_ms"].presence
    return Time.zone.at(value.to_f / 1000.0) if value.present?

    nil
  end

  def extract_screen_name(tweet, account_name)
    account_name.presence ||
      tweet.dig("user", "screen_name").presence ||
      tweet.dig("legacy", "user", "screen_name").presence ||
      "archive"
  end

  def extract_entry_type(tweet)
    return "retweet_quote" if tweet["retweeted_status_id_str"].present? ||
                              tweet["retweeted_status"].present? ||
                              tweet["quoted_status_id_str"].present? ||
                              tweet["quoted_status"].present? ||
                              extract_full_text(tweet).start_with?("RT @")
    return "reply" if tweet["in_reply_to_status_id_str"].present? || tweet["in_reply_to_user_id_str"].present? || tweet["in_reply_to_screen_name"].present?

    "tweet"
  end

  def extract_full_text(tweet)
    tweet["full_text"].presence ||
      tweet["text"].presence ||
      tweet.dig("legacy", "full_text").presence ||
      tweet.dig("legacy", "text").presence ||
      tweet.dig("note_tweet", "text").presence ||
      ""
  end

  def build_tweet_row(tweet, tweet_id, account_name, media_entry_names)
    {
      tweet_id: tweet_id.to_s,
      entry_type: extract_entry_type(tweet),
      screen_name: extract_screen_name(tweet, account_name),
      full_text: extract_full_text(tweet).to_s,
      tweeted_at: extract_tweeted_at(tweet),
      media_entry_names: media_entry_names
    }
  end

  def media_entries_for(tweet, tweet_id, media_entries_by_tweet_id)
    entry_names = media_entries_by_tweet_id.fetch(tweet_id.to_s, [])
    referenced_basenames = extract_referenced_media_basenames(tweet)
    return entry_names if referenced_basenames.blank?

    filtered_entry_names = entry_names.select do |entry_name|
      filename = File.basename(entry_name)
      referenced_basenames.any? do |basename|
        filename == basename || filename.end_with?("-#{basename}")
      end
    end

    filtered_entry_names.presence || entry_names
  end

  def merge_tweet_rows(existing_row, candidate_row)
    return candidate_row if existing_row.blank?

    {
      tweet_id: existing_row[:tweet_id],
      entry_type: preferred_entry_type(existing_row[:entry_type], candidate_row[:entry_type]),
      screen_name: preferred_screen_name(existing_row[:screen_name], candidate_row[:screen_name]),
      full_text: preferred_full_text(existing_row[:full_text], candidate_row[:full_text]),
      tweeted_at: existing_row[:tweeted_at] || candidate_row[:tweeted_at],
      media_entry_names: merge_media_entry_names(existing_row[:media_entry_names], candidate_row[:media_entry_names])
    }
  end

  def preferred_entry_type(existing_entry_type, candidate_entry_type)
    [ existing_entry_type, candidate_entry_type ].max_by do |entry_type|
      case entry_type
      when "retweet_quote" then 2
      when "reply" then 1
      else 0
      end
    end
  end

  def preferred_screen_name(existing_screen_name, candidate_screen_name)
    [ existing_screen_name, candidate_screen_name ].find do |screen_name|
      screen_name.present? && screen_name != "archive"
    end || existing_screen_name.presence || candidate_screen_name.presence || "archive"
  end

  def preferred_full_text(existing_full_text, candidate_full_text)
    [ existing_full_text.to_s, candidate_full_text.to_s ].max_by(&:length)
  end

  def merge_media_entry_names(existing_media_entry_names, candidate_media_entry_names)
    (Array(existing_media_entry_names) + Array(candidate_media_entry_names)).uniq.sort
  end

  def extract_connection_rows(payload, relationship_type)
    items = payload.is_a?(Array) ? payload : [ payload ]

    items.filter_map do |item|
      connection = item[relationship_type] || item[relationship_type.to_sym] if item.respond_to?(:[])
      next unless connection.is_a?(Hash)

      account_id = connection["accountId"].presence || connection[:accountId].presence || connection["account_id"].presence || connection[:account_id].presence
      user_link = connection["userLink"].presence || connection[:userLink].presence || connection["user_link"].presence || connection[:user_link].presence
      next if account_id.blank?

      {
        account_id: account_id.to_s,
        relationship_type: relationship_type,
        user_link: user_link.to_s.presence
      }
    end
  end

  def extract_like_rows(payload)
    items = payload.is_a?(Array) ? payload : [ payload ]

    items.filter_map do |item|
      like = item["like"] || item[:like] if item.respond_to?(:[])
      next unless like.is_a?(Hash)

      tweet_id = like["tweetId"].presence || like[:tweetId].presence || like["tweet_id"].presence || like[:tweet_id].presence
      next if tweet_id.blank?

      {
        tweet_id: tweet_id.to_s,
        full_text: like["fullText"].presence || like[:fullText].presence || like["full_text"].presence || like[:full_text].presence,
        expanded_url: like["expandedUrl"].presence || like[:expandedUrl].presence || like["expanded_url"].presence || like[:expanded_url].presence
      }
    end
  end

  def attach_media_files(tweet, zip, media_entry_names)
    Array(media_entry_names).each do |entry_name|
      entry = zip.find_entry(entry_name)

      unless entry
        Rails.logger.warn("Twitter archive media not found in zip: #{entry_name}")
        next
      end

      media_bytes = entry.get_input_stream.read
      next if media_bytes.blank?

      filename = File.basename(entry.name)
      io = StringIO.new(media_bytes)
      content_type = Marcel::MimeType.for(io, name: filename) || "application/octet-stream"
      io.rewind

      tweet.media.attach(
        io: io,
        filename: filename,
        content_type: content_type
      )
    end
  end

  def existing_archive_media_blob_ids
    ActiveStorage::Attachment.where(record_type: "TwitterArchiveTweet", name: "media").distinct.pluck(:blob_id)
  end

  def purge_replaced_media_blobs(blob_ids)
    return if blob_ids.blank?

    ActiveStorage::Blob.unattached.where(id: blob_ids).find_each(&:purge)
  end

  def extract_referenced_media_basenames(tweet)
    media_entities_for(tweet).filter_map do |media|
      extract_media_basename(media["media_url_https"]) ||
        extract_media_basename(media["media_url"]) ||
        extract_video_variant_basename(media)
    end.uniq
  end

  def media_entities_for(tweet)
    [
      tweet.dig("extended_entities", "media"),
      tweet.dig("entities", "media"),
      tweet.dig("legacy", "extended_entities", "media"),
      tweet.dig("legacy", "entities", "media")
    ].compact.find(&:present?) || []
  end

  def extract_video_variant_basename(media)
    variants = media.dig("video_info", "variants")
    return nil unless variants.is_a?(Array)

    variants.filter_map { |variant| extract_media_basename(variant["url"]) }.first
  end

  def extract_media_basename(url)
    return nil if url.blank?

    File.basename(URI.parse(url).path)
  rescue URI::InvalidURIError
    nil
  end

  def tweet_like_hash?(value)
    return false unless value.is_a?(Hash)

    value.key?("id") ||
      value.key?(:id) ||
      value.key?("id_str") ||
      value.key?(:id_str) ||
      value.key?("full_text") ||
      value.key?(:full_text) ||
      value.key?("created_at") ||
      value.key?(:created_at) ||
      value.key?("text") ||
      value.key?(:text)
  end

  def source_path
    if source.respond_to?(:tempfile) && source.tempfile.present?
      source.tempfile.path
    elsif source.respond_to?(:path)
      source.path
    else
      source.to_s
    end
  end

  def build_summary(archive_data)
    {
      tweets: archive_data[:tweets].count,
      followers: archive_data[:followers].count,
      following: archive_data[:following].count,
      likes: archive_data[:likes].count,
      total_items: archive_data.values.sum(&:count)
    }
  end

  def report_progress(progress, message)
    return unless progress_callback

    normalized_progress = progress.to_i.clamp(0, 100)
    return if normalized_progress == @last_progress && message == @last_message

    @last_progress = normalized_progress
    @last_message = message
    progress_callback.call(normalized_progress, message)
  end
end
