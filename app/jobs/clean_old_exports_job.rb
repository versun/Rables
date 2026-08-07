class CleanOldExportsJob < ApplicationJob
  queue_as :default

  DEFAULT_DIRS = %w[exports imports uploads].freeze

  # 清理 tmp 下旧的导出/导入/上传临时文件
  # @param options [Hash] 选项哈希：
  #   :days 保留最近多少天的文件（默认7天）
  #   :dirs 要清理的目录（默认 tmp/exports、tmp/imports、tmp/uploads，主要供测试注入）
  #   SolidQueue 会传递哈希作为位置参数，例如 { days: 7 }
  def perform(options = {})
    days = option_value(options, :days) || 7
    dirs = Array(option_value(options, :dirs)).presence || default_dirs

    Rails.event.notify "clean_old_exports_job.started",
      level: "info",
      component: "CleanOldExportsJob",
      days: days

    result = cleanup_old_files(dirs, days: days)

    # Fail Export rows stuck in pending/running (worker died mid-export)
    stale_exports_failed = Export.fail_stale!

    # Also purge activity logs older than 90 days
    activity_logs_deleted = ActivityLog.where("created_at < ?", 90.days.ago).delete_all

    # 创建ActivityLog记录
    ActivityLog.log!(
      action: :completed,
      target: :export_cleanup,
      level: result[:errors] > 0 ? :warn : :info,
      error_count: result[:errors],
      stale_exports_failed: stale_exports_failed,
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

  private

  def option_value(options, key)
    return nil unless options.is_a?(Hash)

    options[key] || options[key.to_s]
  end

  def default_dirs
    DEFAULT_DIRS.map { |dir| Rails.root.join("tmp", dir) }
  end

  def cleanup_old_files(dirs, days:)
    cutoff_time = days.days.ago
    deleted_count = 0
    error_count = 0

    dirs.each do |dir|
      base = Pathname.new(dir)
      next unless base.directory?

      children = begin
        base.children
      rescue StandardError => e
        error_count += 1
        Rails.event.notify "clean_old_exports_job.deletion_failed",
          level: "error",
          component: "CleanOldExportsJob",
          path: base.to_s,
          error: e.message
        next
      end

      children.each do |entry|
        if entry.mtime < cutoff_time
          FileUtils.rm_rf(entry)
          deleted_count += 1
        end
      rescue StandardError => e
        error_count += 1
        Rails.event.notify "clean_old_exports_job.deletion_failed",
          level: "error",
          component: "CleanOldExportsJob",
          path: entry.to_s,
          error: e.message
      end
    end

    {
      deleted: deleted_count,
      errors: error_count,
      message: "Cleaned up #{deleted_count} old export/import file(s) older than #{days} days"
    }
  end
end
