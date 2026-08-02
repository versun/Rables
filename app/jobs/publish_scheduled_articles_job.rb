class PublishScheduledArticlesJob < ApplicationJob
  queue_as :urgent
  queue_with_priority 10

  def self.cancel_old_jobs(article_id)
    Rails.event.notify "publish_scheduled_articles_job.checking_old_jobs",
      level: "info",
      component: "PublishScheduledArticlesJob",
      article_id: article_id

    # Only adapters with mission_control-jobs querying support (e.g. solid_queue)
    # can list scheduled jobs; other adapters (like the test adapter) have
    # nothing queryable to cancel.
    return unless ActiveJob::Base.queue_adapter.respond_to?(:fetch_jobs)

    ActiveJob::Base.jobs.scheduled.where(job_class_name: "PublishScheduledArticlesJob").each do |job|
      # ActiveJob::Base.jobs yields deserialized job proxies, so arguments is
      # the plain positional arguments array, e.g. [ article_id ].
      if job.arguments == [ article_id ]
        Rails.event.notify "publish_scheduled_articles_job.cancelling_old_job",
          level: "info",
          component: "PublishScheduledArticlesJob",
          article_id: article_id
        job.discard
      end
    end
  end

  def perform(article_id)
    article = Article.find(article_id)

    # Skip stale jobs whose article is no longer awaiting scheduled
    # publication; publish_scheduled would otherwise compare nil <= Time.
    if article.scheduled_at.nil?
      Rails.event.notify "publish_scheduled_articles_job.skipped",
        level: "info",
        component: "PublishScheduledArticlesJob",
        article_id: article_id,
        reason: "scheduled_at_missing"
      return
    end

    Rails.event.notify "publish_scheduled_articles_job.publishing",
      level: "info",
      component: "PublishScheduledArticlesJob",
      article_id: article_id,
      current_time: Time.current
    article.publish_scheduled
  rescue ActiveRecord::RecordNotFound => e
    Rails.event.notify "publish_scheduled_articles_job.article_not_found",
      level: "error",
      component: "PublishScheduledArticlesJob",
      article_id: article_id,
      error_message: e.message
  end

  def self.schedule_at(article)
    return unless article.schedule? && article.scheduled_at.present?
    cancel_old_jobs(article.id)

    # The scheduled_at value is already in UTC in the database
    # We only need to convert it to the application time zone for display/job scheduling
    scheduled_time = article.scheduled_at # UTC

    set(wait_until: scheduled_time).perform_later(article.id)

    Rails.event.notify "publish_scheduled_articles_job.scheduled",
      level: "info",
      component: "PublishScheduledArticlesJob",
      article_id: article.id,
      scheduled_time: scheduled_time
  end
end
