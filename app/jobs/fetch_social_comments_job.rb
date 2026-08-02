class FetchSocialCommentsJob < ApplicationJob
  queue_as :default

  # When platform is given, only fetch comments for that platform so each
  # platform can follow its own schedule. Nil fetches all enabled platforms.
  def perform(platform = nil)
    fetch_mastodon_comments if fetch_platform?(platform, "mastodon") && mastodon_enabled?
    fetch_bluesky_comments if fetch_platform?(platform, "bluesky") && bluesky_enabled?
  end

  private

  def fetch_platform?(requested, platform)
    requested.nil? || requested == platform
  end

  def mastodon_enabled?
    mastodon_settings = Crosspost.find_by(platform: "mastodon")
    mastodon_settings&.enabled? && mastodon_settings&.auto_fetch_comments
  end

  def bluesky_enabled?
    bluesky_settings = Crosspost.find_by(platform: "bluesky")
    bluesky_settings&.enabled? && bluesky_settings&.auto_fetch_comments
  end

  def fetch_mastodon_comments
    Rails.event.notify "fetch_social_comments_job.mastodon_started",
      level: "info",
      component: "FetchSocialCommentsJob",
      platform: "mastodon"

    articles = Article.published
                      .joins(:social_media_posts)
                      .where(social_media_posts: { platform: "mastodon" })
                      .where.not(social_media_posts: { url: nil })
                      .distinct
                      .select(:id, :slug)

    process_platform_comments(articles, "mastodon", MastodonService.new, rate_limit_thresholds: { stop: 5, delay: 20 })
  end

  def fetch_bluesky_comments
    Rails.event.notify "fetch_social_comments_job.bluesky_started",
      level: "info",
      component: "FetchSocialCommentsJob",
      platform: "bluesky"

    articles = Article.published
                      .joins(:social_media_posts)
                      .where(social_media_posts: { platform: "bluesky" })
                      .where.not(social_media_posts: { url: nil })
                      .distinct
                      .select(:id, :slug)

    process_platform_comments(articles, "bluesky", BlueskyService.new, rate_limit_thresholds: { stop: 50, delay: 200 })
  end

  def process_platform_comments(articles, platform, service, rate_limit_thresholds:)
    success_count = 0
    error_count = 0
    total_comments = 0
    stopped_due_to_rate_limit = false

    # Load the posts for all articles in one query, indexed by
    # [ article_id, platform ], instead of a find_by per article.
    posts_by_article_and_platform = SocialMediaPost.where(article_id: articles.map(&:id))
                                                   .index_by { |post| [ post.article_id, post.platform ] }

    articles.each do |article|
      begin
        post = posts_by_article_and_platform[[ article.id, platform ]]
        next unless post&.url.presence

        # Fetch comments from platform
        result = service.fetch_comments(post.url)

        # Handle rate limit info
        if result[:rate_limit]
          rate_limit = result[:rate_limit]

          # Stop processing if rate limit is critically low
          if rate_limit[:remaining] && rate_limit[:remaining] < rate_limit_thresholds[:stop]
            Rails.event.notify "fetch_social_comments_job.rate_limit_stop",
              level: "warn",
              component: "FetchSocialCommentsJob",
              platform: platform,
              remaining: rate_limit[:remaining]
            ActivityLog.log!(
              action: :paused,
              target: :fetch_comments,
              level: :warn,
              platform: platform,
              remaining: rate_limit[:remaining],
              limit: rate_limit[:limit],
              reset_at: rate_limit[:reset_at]
            )
            stopped_due_to_rate_limit = true
            break
          end

          # Add delay if rate limit is getting low
          if rate_limit[:remaining] && rate_limit[:remaining] < rate_limit_thresholds[:delay]
            sleep_time = 2
            Rails.event.notify "fetch_social_comments_job.rate_limit_delay",
              level: "info",
              component: "FetchSocialCommentsJob",
              platform: platform,
              remaining: rate_limit[:remaining],
              sleep_time: sleep_time
            sleep(sleep_time)
          end
        end

        comments_data = result[:comments]

        # Create or update comments with deduplication
        comments_data.each do |comment_data|
          _comment, upsert_result = Comment.upsert_from_external(article, platform, comment_data)

          case upsert_result
          when :created
            total_comments += 1
            Rails.event.notify "fetch_social_comments_job.comment_created",
              level: "info",
              component: "FetchSocialCommentsJob",
              platform: platform,
              article_slug: article.slug
          when :updated
            Rails.event.notify "fetch_social_comments_job.comment_updated",
              level: "info",
              component: "FetchSocialCommentsJob",
              platform: platform,
              article_slug: article.slug
          end
        end

        success_count += 1
      rescue => e
        error_count += 1
        Rails.event.notify "fetch_social_comments_job.article_failed",
          level: "error",
          component: "FetchSocialCommentsJob",
          platform: platform,
          article_slug: article.slug,
          error_message: e.message
        Rails.event.notify "fetch_social_comments_job.error_backtrace",
          level: "error",
          component: "FetchSocialCommentsJob",
          backtrace: e.backtrace.join("\n")

        ActivityLog.log!(
          action: :failed,
          target: :fetch_comments,
          level: :error,
          platform: platform,
          slug: article.slug,
          error: e.message
        )
      end
    end

    ActivityLog.log!(
      action: :completed,
      target: :fetch_comments,
      level: :info,
      platform: platform,
      success_count: success_count,
      total_comments: total_comments,
      error_count: error_count,
      stopped: stopped_due_to_rate_limit ? true : nil
    )

    Rails.event.notify "fetch_social_comments_job.platform_completed",
      level: "info",
      component: "FetchSocialCommentsJob",
      platform: platform,
      success_count: success_count,
      error_count: error_count,
      total_comments: total_comments,
      stopped_due_to_rate_limit: stopped_due_to_rate_limit
  end
end
