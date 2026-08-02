class CleanOldExportsJob < ApplicationJob
  queue_as :default

  # 执行清理旧导出和导入文件的任务
  # @param options [Hash] 选项哈希，包含 :days 键（保留最近多少天的文件，默认7天）
  #   SolidQueue 会传递哈希作为位置参数，例如 { days: 7 }
  def perform(options = {})
    days = if options.is_a?(Hash)
             options[:days] || options["days"] || 7
    else
             7
    end
    Rails.event.notify "clean_old_exports_job.started",
      level: "info",
      component: "CleanOldExportsJob",
      days: days

    result = Export.cleanup_old_exports(days: days)

    # Also purge activity logs older than 90 days
    activity_logs_deleted = ActivityLog.where("created_at < ?", 90.days.ago).delete_all

    # 创建ActivityLog记录
    ActivityLog.log!(
      action: :completed,
      target: :export_cleanup,
      level: result[:errors] > 0 ? :warn : :info,
      error_count: result[:errors],
      activity_logs_deleted: activity_logs_deleted,
      message: result[:message]
    )

    Rails.event.notify "clean_old_exports_job.completed",
      level: "info",
      component: "CleanOldExportsJob",
      message: result[:message],
      errors: result[:errors],
      activity_logs_deleted: activity_logs_deleted
    result
  end
end
