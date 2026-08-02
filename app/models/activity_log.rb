class ActivityLog < ApplicationRecord
  enum :level, { info: 0, warn: 1, error: 2 }

  LEVEL_ALIASES = {
    "warning" => "warn"
  }.freeze

  DESCRIPTION_KEY_ORDER = %i[
    title name slug id email author commentable
    platform platforms provider
    source format mode operation
    status
    count success_count error_count subscriber_count total_subscribers
    total_comments total_errors
    tags
    regex replacement
    position
    trashed_count deleted_count
    campaign_id post_id
    filename file path url
    reset_at remaining limit
    errors error reason message
    stopped
  ].freeze

  def self.track_activity(target)
    ActivityLog.where(target: normalize_token(target)).order(created_at: :desc).limit(10)
  end

  def self.log!(action:, target:, level: :info, **context)
    create!(
      action: normalize_token(action),
      target: normalize_token(target),
      level: normalize_level(level),
      description: format_description(context)
    )
  rescue StandardError => e
    # Activity logging must never break the main flow
    Rails.logger.warn("ActivityLog.log! failed: #{e.class}: #{e.message}")
    nil
  end

  def self.normalize_token(value)
    token = value.to_s.strip
    return "unknown" if token.blank?

    token.downcase.gsub(/[-\s]+/, "_")
  end

  def self.normalize_level(value)
    normalized = normalize_token(value)
    normalized = LEVEL_ALIASES.fetch(normalized, normalized)
    return normalized if levels.key?(normalized)
    return "info" if normalized == "unknown"

    warn_unknown_level(value)
    "warn"
  end

  def self.format_description(context)
    context = context.compact
    return nil if context.empty?

    ordered = []
    DESCRIPTION_KEY_ORDER.each do |key|
      ordered << [ key, context.delete(key) ] if context.key?(key)
    end
    context.sort_by { |key, _| key.to_s }.each { |pair| ordered << pair }

    ordered.map { |key, value| "#{key}=#{format_value(value)}" }.join(" ")
  end

  def self.format_value(value)
    case value
    when Time, DateTime, Date
      quote_string(value.iso8601)
    when Array
      "[#{value.compact.map { |item| format_value(item) }.join(",")}]"
    when Numeric, TrueClass, FalseClass
      value.to_s
    else
      quote_string(value.to_s)
    end
  end

  def self.quote_string(text)
    sanitized = text.to_s.strip.gsub(/\s+/, " ")
    escaped = sanitized.gsub("\\", "\\\\").gsub("\"", "\\\"")
    "\"#{escaped}\""
  end

  def self.warn_unknown_level(value)
    return unless defined?(Rails) && Rails.respond_to?(:logger) && Rails.logger

    Rails.logger.warn("ActivityLog: unknown level #{value.inspect}; defaulting to warn")
  end
end
