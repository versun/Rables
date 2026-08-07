class Admin::MigratesController < Admin::BaseController
  def index
    @active_tab = migrate_tab(params[:tab])
    if @active_tab == "export"
      # Fail stuck pending/running rows right away (CleanOldExportsJob only
      # runs daily); otherwise the tab would keep auto-refreshing for hours.
      Export.fail_stale!
      @exports = Export.order(created_at: :desc).limit(10)
    end
  end

  def create
    operation_type = params[:operation_type]

    case operation_type
    when "export"
      enqueue_export(:backup)
    when "site_export"
      enqueue_export(:site_export)
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

  # Both exports run in the background (see ExportJob); the artifact shows up
  # in the list on the export tab when it completes.
  def enqueue_export(kind)
    export = Export.create!(kind: kind)
    ExportJob.perform_later(export.id)

    ActivityLog.log!(
      action: :started,
      target: :export,
      level: :info,
      format: kind
    )

    label = kind.to_sym == :backup ? "Backup" : "Go migration"
    redirect_to admin_migrates_path(tab: "export"),
      notice: "#{label} export started in the background. This page refreshes automatically while it runs."
  end

  def handle_import
    if params[:url].present?
      # RSS导入
      ImportFromRssJob.perform_later(params[:url], params[:import_images])
      redirect_to admin_migrates_path(tab: "import"), notice: "RSS Import in progress, please check the logs for details"
    elsif params[:backup_file].present?
      # 备份文件恢复
      import_from_backup
    else
      redirect_to admin_migrates_path(tab: "import"), alert: "Please provide either RSS URL or backup file for import"
    end
  end

  def import_from_backup
    uploaded_file = params[:backup_file]
    temp_file = nil

    # Validate file type
    unless uploaded_file.content_type == "application/zip" || File.extname(uploaded_file.original_filename.to_s).downcase == ".zip"
      raise SiteBackup::Error, "Only ZIP files are allowed for import"
    end

    # Generate a secure temporary filename using SecureRandom to avoid
    # any potential issues with user-provided filenames
    uploads_dir = Rails.root.join("tmp", "uploads")
    FileUtils.mkdir_p(uploads_dir)
    temp_file = uploads_dir.join("backup_#{Time.current.to_i}_#{SecureRandom.hex(8)}.zip")

    File.open(temp_file, "wb") do |f|
      source = if uploaded_file.respond_to?(:tempfile) && uploaded_file.tempfile
        uploaded_file.tempfile
      else
        uploaded_file
      end

      source.rewind if source.respond_to?(:rewind)
      IO.copy_stream(source, f)
    end

    SiteBackup.import(temp_file)

    ActivityLog.log!(
      action: :completed,
      target: :import,
      level: :info,
      source: "backup",
      filename: uploaded_file.original_filename
    )

    redirect_to admin_migrates_path(tab: "import"),
      notice: "Backup restored successfully. The database and uploaded files were replaced. Restart the server if anything looks off."
  rescue SiteBackup::Error => e
    redirect_to admin_migrates_path(tab: "import"), alert: "Backup import failed: #{e.message}"
  rescue StandardError => e
    Rails.event.notify(
      "admin.migrates_controller.backup_import_error",
      level: "error",
      component: "Admin::MigratesController",
      message: e.message,
      filename: uploaded_file&.original_filename
    )
    redirect_to admin_migrates_path(tab: "import"), alert: "Backup import failed: #{e.message}"
  ensure
    FileUtils.rm_f(temp_file) if temp_file
  end

  def migrate_tab(value)
    %w[export import].include?(value) ? value : "export"
  end
end
