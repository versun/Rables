# frozen_string_literal: true

require "fileutils"
require "pathname"
require "securerandom"

# Runs an Export (Full Backup via SiteBackup, or the Go migration package via
# SiteExport) in the background and records the resulting artifact on the
# Export row. Failures are recorded on the row instead of retried — an export
# is heavy, and Solid Queue retries would just duplicate the work.
class ExportJob < ApplicationJob
  queue_as :default

  def perform(export_id)
    export = Export.find(export_id)
    export.update!(status: :running)

    artifact = case export.kind
    when "backup"
      SiteBackup.export
    when "site_export"
      export_site_package
    else
      raise "Unknown export kind: #{export.kind}"
    end

    export.update!(status: :completed, filename: File.basename(artifact.to_s), byte_size: File.size(artifact))
    ActivityLog.log!(action: :completed, target: :export, level: :info, format: export.kind, filename: export.filename)
  rescue StandardError => e
    export&.update!(status: :failed, error: e.message)
    ActivityLog.log!(action: :failed, target: :export, level: :error, format: export&.kind, message: e.message)
    Rails.event.notify(
      "export_job.failed",
      level: "error",
      component: "ExportJob",
      export_id: export_id,
      message: e.message
    )
  end

  private

  # SiteExport produces a directory; zip it so the artifact downloads as a
  # single file like the backup ZIP.
  def export_site_package
    timestamp = Time.current.strftime("%Y%m%d_%H%M%S")
    work_dir = Rails.root.join("tmp", "exports", "site_export_#{timestamp}_#{SecureRandom.hex(4)}")

    SiteExport.export(output_dir: work_dir, logger: ->(message) { Rails.logger.info(message) })
    zip_path = Pathname.new("#{work_dir}.zip")
    ZipArchiver.zip_directory(work_dir, zip_path)
    zip_path
  ensure
    FileUtils.rm_rf(work_dir) if work_dir
  end
end
