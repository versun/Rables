class FetchSocialCommentsJob < ApplicationJob
  queue_as :default

  def perform
    fetch_mastodon_comments if mastodon_enabled?
    fetch_bluesky_comments if bluesky_enabled?
  end

  private

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
                      .includes(:comments)
                      .where(social_media_posts: { platform: "mastodon" })
                      .where.not(social_media_posts: { url: nil })
                      .distinct

    process_platform_comments(articles, "mastodon", MastodonService.new, rate_limit_thresholds: { stop: 5, delay: 20 })
  end

  def fetch_bluesky_comments
    Rails.event.notify "fetch_social_comments_job.bluesky_started",
      level: "info",
      component: "FetchSocialCommentsJob",
      platform: "bluesky"

    articles = Article.published
                      .joins(:social_media_posts)
                      .includes(:comments)
                      .where(social_media_posts: { platform: "bluesky" })
                      .where.not(social_media_posts: { url: nil })
                      .distinct

    process_platform_comments(articles, "bluesky", BlueskyService.new, rate_limit_thresholds: { stop: 50, delay: 200 })
  end

  def process_platform_comments(articles, platform, service, rate_limit_thresholds:)
    success_count = 0
    error_count = 0
    total_comments = 0
    stopped_due_to_rate_limit = false

    articles.each do |article|
      begin
        post = article.social_media_posts.find_by(platform: platform)
        next unless post&.url

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
          comment = article.comments.find_or_initialize_by(
            platform: platform,
            external_id: comment_data[:external_id]
          )

          comment.assign_attributes(
            author_name: comment_data[:author_name],
            author_username: comment_data[:author_username],
            author_avatar_url: comment_data[:author_avatar_url],
            content: comment_data[:content],
            published_at: comment_data[:published_at],
            url: comment_data[:url]
          )

          if comment.new_record?
            comment.save!
            total_comments += 1
            Rails.event.notify "fetch_social_comments_job.comment_created",
              level: "info",
              component: "FetchSocialCommentsJob",
              platform: platform,
              article_slug: article.slug
          elsif comment.changed?
            comment.save!
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
