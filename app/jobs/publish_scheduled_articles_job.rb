class PublishScheduledArticlesJob < ApplicationJob
  queue_as :urgent
  queue_with_priority 10

  def self.cancel_old_jobs(article_id)
    Rails.event.notify "publish_scheduled_articles_job.checking_old_jobs",
      level: "info",
      component: "PublishScheduledArticlesJob",
      article_id: article_id

    # Skip job cancellation in test environment where ActiveJob::Base.jobs is not available
    return if Rails.env.test?

    ActiveJob::Base.jobs.scheduled.where(job_class_name: "PublishScheduledArticlesJob").each do |job|
      if job.arguments[0]["arguments"] == [ article_id ]
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
    Rails.event.notify "publish_scheduled_articles_job.publishing",
      level: "info",
      component: "PublishScheduledArticlesJob",
      article_id: article_id,
      current_time: Time.current
    article.publish_scheduled

    if article.publish? && JekyllSetting.respond_to?(:table_exists?) && JekyllSetting.table_exists?
      setting = JekyllSetting.instance
      if setting.auto_sync_enabled? && setting.sync_on_publish?
        JekyllSingleSyncJob.perform_later("Article", article.id)
      end
    end
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

  # private

  # def self.scheduled_job_for(article)
  #   ActiveJob::Base.queue_adapter.enqueued_jobs.find do |job|
  #     job[:job] == self && job[:args].first == article.id
  #   end
  # end
end
