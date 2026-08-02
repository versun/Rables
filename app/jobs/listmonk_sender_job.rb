require "net/http"
require "json"

class ListmonkSenderJob < ApplicationJob
  include CacheableSettings
  queue_as :default

  def perform(article_id)
    article = Article.find(article_id)
    listmonk = Listmonk.first
    return unless listmonk.present? && listmonk.list_id.present? && listmonk.template_id.present?

    ActivityLog.log!(
      action: :started,
      target: :newsletter,
      level: :info,
      title: article.title,
      slug: article.slug,
      mode: "listmonk"
    )

    listmonk.send_newsletter(article, CacheableSettings.site_info[:title])
  rescue ActiveRecord::RecordNotFound => e
    Rails.event.notify "listmonk_sender_job.article_not_found",
      level: "error",
      component: "ListmonkSenderJob",
      article_id: article_id,
      error_message: e.message
  rescue => e
    ActivityLog.log!(
      action: :failed,
      target: :newsletter,
      level: :error,
      title: article&.title,
      slug: article&.slug,
      mode: "listmonk",
      error: e.message
    )
    raise
  end
end
