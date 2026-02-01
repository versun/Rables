# frozen_string_literal: true

class JekyllSingleSyncJob < ApplicationJob
  queue_as :default

  def perform(record_type, record_id, triggered_by = "publish")
    setting = JekyllSetting.instance

    unless setting.configured? && setting.sync_on_publish?
      return
    end

    service = JekyllSyncService.new(setting)

    case record_type.to_s
    when "Article"
      article = Article.find_by(id: record_id)
      return unless article

      if article.publish?
        service.sync_article(article, triggered_by: triggered_by)
      else
        service.delete_article(article)
      end
    when "Page"
      page = Page.find_by(id: record_id)
      return unless page

      if page.publish?
        service.sync_page(page, triggered_by: triggered_by)
      else
        service.delete_page(page)
      end
    else
      Rails.event.notify("jekyll_single_sync_job.unknown_record_type", component: "JekyllSingleSyncJob", record_type: record_type, level: "error")
    end
  end
end
