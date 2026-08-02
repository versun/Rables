class ExportDataJob < ApplicationJob
  queue_as :default

  def perform(format = "default")
    exporter = (format == "markdown" ? MarkdownExport : Export).new
    success = exporter.generate

    if success
      # 创建下载URL（使用Rails的静态文件服务）
      download_url = exporter.zip_path

      # 创建ActivityLog记录
      ActivityLog.log!(
        action: :completed,
        target: :export,
        level: :info,
        format: format,
        file: download_url
      )

      Rails.event.notify "export_data_job.completed",
        level: "info",
        component: "ExportDataJob",
        format: format,
        download_url: download_url
    else
      # 创建ActivityLog记录失败信息
      ActivityLog.log!(
        action: :failed,
        target: :export,
        level: :error,
        format: format,
        error: exporter.error_message
      )

      Rails.event.notify "export_data_job.failed",
        level: "error",
        component: "ExportDataJob",
        format: format,
        error_message: exporter.error_message
    end
  end
end
