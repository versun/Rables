class Admin::MigratesController < Admin::BaseController
  include ActiveStorage::SetCurrent

  def index
    @active_tab = migrate_tab(params[:tab])
  end

  def create
    operation_type = params[:operation_type]

    case operation_type
    when "export"
      handle_export
    when "import"
      handle_import
    else
      redirect_to admin_migrates_path(tab: migrate_tab(nil)), alert: "Unsupported operation type"
    end
  rescue StandardError => e
    Rails.event.notify(
      "admin.migrates_controller.operation_error",
      level: "error",
      component: "Admin::MigratesController",
      operation_type: params[:operation_type],
      message: e.message
    )
    redirect_to admin_migrates_path(tab: migrate_tab(params[:operation_type])), alert: "An unexpected error occurred: #{e.message}"
  end

  private

  def handle_export
    export_type = (params[:export_type].presence || "default").to_s

    flash_message = {
      "default" => "Export Initiated",
      "markdown" => "Markdown Export Initiated"
    }.fetch(export_type, nil)

    unless flash_message
      redirect_to admin_migrates_path(tab: "export"), alert: "Unsupported export type" and return
    end

    ExportDataJob.perform_later(export_type)
    ActivityLog.log!(
      action: :queued,
      target: :export,
      level: :info,
      format: export_type
    )
    flash[:notice] = flash_message

    redirect_to admin_migrates_path(tab: "export")
  end

  def handle_import
    if params[:url].present?
      # RSS导入
      ImportFromRssJob.perform_later(params[:url], params[:import_images])
      redirect_to admin_migrates_path(tab: "import"), notice: "RSS Import in progress, please check the logs for details"
    elsif params[:zip_file].present?
      # ZIP文件导入
      import_from_zip
    else
      redirect_to admin_migrates_path(tab: "import"), alert: "Please provide either RSS URL or ZIP file for import"
    end
  end

  def import_from_zip
    uploaded_file = params[:zip_file]
    temp_file = nil

    # Validate file type
    unless uploaded_file.content_type == "application/zip" || uploaded_file.original_filename.to_s.end_with?(".zip")
      raise "Only ZIP files are allowed for import"
    end

    # Generate a secure temporary filename using SecureRandom to avoid
    # any potential issues with user-provided filenames
    secure_filename = "import_#{Time.current.to_i}_#{SecureRandom.hex(8)}.zip"
    uploads_dir = Rails.root.join("tmp", "uploads")
    FileUtils.mkdir_p(uploads_dir)

    temp_file = uploads_dir.join(secure_filename)

    File.open(temp_file, "wb") do |f|
      source = if uploaded_file.respond_to?(:tempfile) && uploaded_file.tempfile
        uploaded_file.tempfile
      else
        uploaded_file
      end

      source.rewind if source.respond_to?(:rewind)
      IO.copy_stream(source, f)
    end

    # Execute import job; on success the job deletes the temp file itself
    ImportFromZipJob.perform_later(temp_file.to_s)

    redirect_to admin_migrates_path(tab: "import"), notice: "ZIP Import in progress, please check the logs for details"
  rescue StandardError => e
    # Remove the uploaded zip when the job never took ownership of it
    FileUtils.rm_f(temp_file) if temp_file
    Rails.event.notify(
      "admin.migrates_controller.zip_import_error",
      level: "error",
      component: "Admin::MigratesController",
      message: e.message,
      filename: uploaded_file&.original_filename
    )
    redirect_to admin_migrates_path(tab: "import"), alert: "ZIP import failed: #{e.message}"
  end

  def migrate_tab(value)
    %w[export import].include?(value) ? value : "export"
  end
end
