class ImportFromZipJob < ApplicationJob
  queue_as :default

  def perform(zip_path)
    importer = ImportZip.new(zip_path)
    success = importer.import_data

    if success
      # 创建ActivityLog记录
      ActivityLog.log!(
        action: :completed,
        target: :import,
        level: :info,
        source: "zip",
        filename: File.basename(zip_path)
      )

      Rails.event.notify "import_from_zip_job.completed",
        level: "info",
        component: "ImportFromZipJob",
        zip_path: zip_path
    else
      # 创建ActivityLog记录失败信息
      ActivityLog.log!(
        action: :failed,
        target: :import,
        level: :error,
        source: "zip",
        filename: File.basename(zip_path),
        error: importer.error_message
      )

      Rails.event.notify "import_from_zip_job.failed",
        level: "error",
        component: "ImportFromZipJob",
        error_message: importer.error_message
    end
  ensure
    # Clean up the uploaded temp file even when the import raises
    cleanup_temp_file(zip_path)
  end

  private

  def cleanup_temp_file(zip_path)
    uploads_dir = Rails.root.join("tmp", "uploads").to_s
    return unless zip_path.to_s.start_with?(uploads_dir + File::SEPARATOR)
    return unless File.exist?(zip_path)

    FileUtils.rm_f(zip_path)
    Rails.event.notify "import_from_zip_job.cleanup",
      level: "info",
      component: "ImportFromZipJob",
      zip_path: zip_path
  end
end
