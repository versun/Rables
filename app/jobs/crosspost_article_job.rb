class CrosspostArticleJob < ApplicationJob
  queue_as :default

  RETRIED_ERRORS = TransientNetworkErrors::TRANSIENT_ERRORS
  DISCARDED_ERRORS = [ ActiveRecord::RecordInvalid, ActiveRecord::RecordNotSaved, ActiveRecord::RecordNotUnique ].freeze

  # Only transient network failures are worth retrying; the final failure is
  # logged once when retries are exhausted instead of on every attempt.
  retry_on(*RETRIED_ERRORS, wait: ->(executions) { 2 ** executions }, attempts: 5) do |job, error|
    job.log_failure(error)
  end

  # Permanent record errors can never succeed on retry: discard and log once.
  discard_on(*DISCARDED_ERRORS) do |job, error|
    job.log_failure(error)
  end

  def perform(article_id, platform, requested_at = nil)
    article = Article.find_by(id: article_id)
    return unless article

    # On retry, skip if the URL was already recorded in a previous execution
    # to avoid publishing duplicate posts. Only URLs recorded after this
    # crosspost was requested count: an older URL comes from a previous
    # crosspost and must not block a re-crosspost. Legacy jobs enqueued
    # without requested_at conservatively skip on any recorded URL.
    return if executions > 1 && url_recorded_since?(article, platform, requested_at)

    social_media_posts = {}

    case platform
    when "mastodon"
        mastodon_url = MastodonService.new.post(article)
        if mastodon_url
          social_media_posts["mastodon"] = mastodon_url
        end
    when "twitter"
        twitter_url = TwitterService.new.post(article)
        if twitter_url
          social_media_posts["twitter"] = twitter_url
        end
    when "bluesky"
        bluesky_url = BlueskyService.new.post(article)
        if bluesky_url
          social_media_posts["bluesky"] = bluesky_url
        end
    end

    # Update article with all crosspost URLs at once
    social_media_posts.each do |platform, url|
      article.social_media_posts.find_or_initialize_by(platform: platform).update!(url: url)
    end

    # Log successful crosspost
    if social_media_posts.any?
      ActivityLog.log!(
        action: :posted,
        target: :crosspost,
        level: :info,
        title: article.title,
        slug: article.slug,
        platforms: social_media_posts.keys
      )
    end

  rescue => e
    # Errors covered by retry_on/discard_on are logged once by those blocks;
    # any other error executes only once, so log it here before re-raising.
    log_failure(e, article: article) unless retried_error?(e) || discarded_error?(e)
    raise
  end

  def log_failure(error, article: nil)
    article ||= Article.find_by(id: arguments.first)
    ActivityLog.log!(
      action: :failed,
      target: :crosspost,
      level: :error,
      title: article&.title,
      slug: article&.slug,
      platform: arguments.second,
      error: error.message
    )
  end

  private

  def url_recorded_since?(article, platform, requested_at)
    scope = article.social_media_posts.where(platform: platform).where.not(url: [ nil, "" ])
    scope = scope.where(updated_at: requested_at..) if requested_at
    scope.exists?
  end

  def retried_error?(error)
    RETRIED_ERRORS.any? { |klass| error.is_a?(klass) }
  end

  def discarded_error?(error)
    DISCARDED_ERRORS.any? { |klass| error.is_a?(klass) }
  end
end
