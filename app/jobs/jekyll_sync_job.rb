# frozen_string_literal: true

class JekyllSyncJob < ApplicationJob
  queue_as :default

  def perform(sync_type = "full", triggered_by = "manual")
    setting = JekyllSetting.instance

    unless setting.configured?
      Rails.event.notify("jekyll_sync_job.not_configured", component: "JekyllSyncJob", level: "warn")
      return
    end

    service = JekyllSyncService.new(setting)

    case sync_type.to_s
    when "full"
      service.sync_all(triggered_by: triggered_by)
    else
      Rails.event.notify("jekyll_sync_job.unknown_sync_type", component: "JekyllSyncJob", sync_type: sync_type, level: "error")
    end
  end
end
