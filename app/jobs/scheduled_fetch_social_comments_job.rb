# Scheduled job that runs hourly (via config/recurring.yml) and checks
# if it's time to fetch social comments based on crosspost settings.
#
# This approach ensures the job persists after service restart since it's
# defined as a static recurring task, not dynamically created.
class ScheduledFetchSocialCommentsJob < ApplicationJob
  queue_as :default

  CACHE_KEY = "social_comments_last_fetch_at".freeze

  def perform
    enabled_platforms = Crosspost.where(enabled: true)
                                 .where(auto_fetch_comments: true)
                                 .where.not(comment_fetch_schedule: [ nil, "" ])

    if enabled_platforms.empty?
      Rails.event.notify "scheduled_fetch_social_comments_job.skipped",
        level: "debug",
        component: "ScheduledFetchSocialCommentsJob",
        reason: "no_enabled_platforms"
      return
    end

    # Each platform follows its own schedule and its own last-fetch timestamp
    due_platforms = enabled_platforms.select do |platform|
      last_fetch_at = Rails.cache.read(cache_key_for(platform.platform))
      should_fetch_now?(platform.comment_fetch_schedule, last_fetch_at)
    end

    if due_platforms.empty?
      Rails.event.notify "scheduled_fetch_social_comments_job.skipped",
        level: "debug",
        component: "ScheduledFetchSocialCommentsJob",
        reason: "not_time_yet",
        platforms: enabled_platforms.map(&:platform)
      return
    end

    # Update last fetch time before triggering to avoid duplicate runs
    due_platforms.each do |platform|
      Rails.cache.write(cache_key_for(platform.platform), Time.current, expires_in: 2.months)
    end

    Rails.event.notify "scheduled_fetch_social_comments_job.triggering",
      level: "info",
      component: "ScheduledFetchSocialCommentsJob",
      platforms: due_platforms.map(&:platform)

    # Trigger the actual fetch job for each due platform
    due_platforms.each do |platform|
      FetchSocialCommentsJob.perform_later(platform.platform)
    end
  end

  private

  def cache_key_for(platform)
    "#{CACHE_KEY}:#{platform}"
  end

  def should_fetch_now?(schedule, last_fetch_at)
    # If never fetched before, fetch now
    return true if last_fetch_at.nil?

    case schedule
    when "daily"
      last_fetch_at < 1.day.ago
    when "weekly"
      last_fetch_at < 1.week.ago
    when "monthly"
      last_fetch_at < 1.month.ago
    else
      Rails.event.notify "scheduled_fetch_social_comments_job.unknown_schedule",
        level: "warn",
        component: "ScheduledFetchSocialCommentsJob",
        schedule: schedule
      false
    end
  end
end
