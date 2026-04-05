class Admin::TwitterArchivesController < Admin::BaseController
  ACTIVE_IMPORT_ALERT = "A Twitter archive import is already in progress. Wait for it to finish before uploading another archive.".freeze

  def index
    @twitter_archive_counts = TwitterArchiveTweet.group(:entry_type).count
    @twitter_archive_connection_counts = TwitterArchiveConnection.group(:relationship_type).count
    @twitter_archive_likes_count = TwitterArchiveLike.count
    @twitter_archive_total = TwitterArchiveTweet.count + TwitterArchiveConnection.count + @twitter_archive_likes_count
    @twitter_archive_updated_at = TwitterArchiveImport.completed.maximum(:finished_at) || TwitterArchiveTweet.maximum(:created_at)
    @twitter_archive_imports = TwitterArchiveImport.recent_first.limit(10)

    TwitterArchiveHandleSyncJob.enqueue_if_needed
  end

  def create
    uploaded_file = params.dig(:twitter_archive, :file)

    unless uploaded_file.present? && valid_zip_upload?(uploaded_file)
      redirect_to admin_twitter_archives_path, alert: "Please upload a valid Twitter archive ZIP file"
      return
    end

    if TwitterArchiveImport.active.exists?
      redirect_to admin_twitter_archives_path, alert: ACTIVE_IMPORT_ALERT
      return
    end

    temp_path = write_temp_zip(uploaded_file)
    import = TwitterArchiveImport.create!(
      source_filename: uploaded_file.original_filename.to_s,
      source_path: temp_path,
      status: "queued",
      progress: 0,
      status_message: "Queued",
      queued_at: Time.current
    )

    TwitterArchiveImportJob.perform_later(import.id)

    ActivityLog.log!(
      action: :queued,
      target: :twitter_archive,
      level: :info,
      filename: import.source_filename,
      import_id: import.id
    )

    redirect_to admin_twitter_archives_path, notice: "Twitter archive import queued. Check the history below for progress."
  rescue ActiveRecord::RecordInvalid, ActiveRecord::RecordNotUnique
    redirect_to admin_twitter_archives_path, alert: ACTIVE_IMPORT_ALERT
  rescue StandardError => e
    if defined?(import) && import&.persisted?
      File.delete(import.source_path) if import.source_path.present? && File.exist?(import.source_path)
      import.update(
        status: "failed",
        status_message: "Import failed",
        error_message: e.message,
        source_path: nil,
        finished_at: Time.current
      )
    end

    ActivityLog.log!(
      action: :failed,
      target: :twitter_archive,
      level: :error,
      filename: uploaded_file&.original_filename,
      errors: e.message
    )
    redirect_to admin_twitter_archives_path, alert: "Twitter archive import failed: #{e.message}"
  ensure
    should_cleanup = (!defined?(import) || import.blank? || !import.persisted?)
    File.delete(temp_path) if should_cleanup && defined?(temp_path) && temp_path.present? && File.exist?(temp_path)
  end

  private

  def valid_zip_upload?(uploaded_file)
    uploaded_file.content_type == "application/zip" || uploaded_file.original_filename.to_s.downcase.end_with?(".zip")
  end

  def write_temp_zip(uploaded_file)
    temp_dir = Rails.root.join("tmp", "twitter_archives")
    FileUtils.mkdir_p(temp_dir)
    temp_path = temp_dir.join("twitter_archive_#{Time.current.to_i}_#{SecureRandom.hex(8)}.zip")

    File.open(temp_path, "wb") do |file|
      source = if uploaded_file.respond_to?(:tempfile) && uploaded_file.tempfile
        uploaded_file.tempfile
      else
        uploaded_file
      end

      source.rewind if source.respond_to?(:rewind)
      IO.copy_stream(source, file)
    end

    temp_path.to_s
  end
end
