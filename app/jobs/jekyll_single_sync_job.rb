class JekyllSingleSyncJob < ApplicationJob
  queue_as :default

  retry_on StandardError, wait: :polynomially_longer, attempts: 3

  def perform(item_type, item_id, action: :sync)
    setting = JekyllSetting.instance

    unless setting.auto_sync_enabled?
      Rails.logger.info "Jekyll auto sync is disabled, skipping"
      return
    end

    service = JekyllSyncService.new(setting)

    case item_type.to_s.downcase
    when "article"
      item = Article.find_by(id: item_id)
      return unless item

      case action.to_sym
      when :sync
        service.sync_article(item)
      when :delete
        service.delete_article(item)
      end
    when "page"
      item = Page.find_by(id: item_id)
      return unless item

      case action.to_sym
      when :sync
        service.sync_page(item)
      when :delete
        service.delete_page(item)
      end
    end
  end
end
