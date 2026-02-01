class JekyllSyncJob < ApplicationJob
  queue_as :default

  retry_on StandardError, wait: :polynomially_longer, attempts: 3

  def perform(sync_type: "full")
    # Create sync record
    sync_record = JekyllSyncRecord.create!(
      sync_type: sync_type,
      status: "pending"
    )

    # Get setting
    setting = JekyllSetting.instance

    unless setting.auto_sync_enabled?
      Rails.logger.info "Jekyll auto sync is disabled, skipping"
      sync_record.update(status: "completed", completed_at: Time.current)
      return
    end

    # Perform sync
    service = JekyllSyncService.new(setting, sync_record)

    case sync_type
    when "full"
      service.sync_all
    when "articles"
      service.sync_all(pages: Page.none)
    when "pages"
      service.sync_all(articles: Article.none)
    else
      service.sync_all
    end
  end
end
