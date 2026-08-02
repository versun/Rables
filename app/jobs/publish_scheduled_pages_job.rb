class PublishScheduledPagesJob < ApplicationJob
  queue_as :urgent
  queue_with_priority 10

  def perform(page_id)
    page = Page.find(page_id)

    # Skip stale jobs whose page is no longer awaiting scheduled publication
    if page.scheduled_at.nil?
      Rails.event.notify "publish_scheduled_pages_job.skipped",
        level: "info",
        component: "PublishScheduledPagesJob",
        page_id: page_id,
        reason: "scheduled_at_missing"
      return
    end

    Rails.event.notify "publish_scheduled_pages_job.publishing",
      level: "info",
      component: "PublishScheduledPagesJob",
      page_id: page_id,
      current_time: Time.current
    page.publish_scheduled
  rescue ActiveRecord::RecordNotFound => e
    Rails.event.notify "publish_scheduled_pages_job.page_not_found",
      level: "error",
      component: "PublishScheduledPagesJob",
      page_id: page_id,
      error_message: e.message
  end

  def self.schedule_at(page)
    return unless page.schedule? && page.scheduled_at.present?

    # The scheduled_at value is already in UTC in the database
    scheduled_time = page.scheduled_at

    set(wait_until: scheduled_time).perform_later(page.id)

    Rails.event.notify "publish_scheduled_pages_job.scheduled",
      level: "info",
      component: "PublishScheduledPagesJob",
      page_id: page.id,
      scheduled_time: scheduled_time
  end
end
