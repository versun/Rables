require "fileutils"
require "securerandom"
require "tempfile"

class TwitterArchiveImportSubmission
  INVALID_UPLOAD_ALERT = "Please upload a valid Twitter archive ZIP file".freeze
  ACTIVE_IMPORT_ALERT = "A Twitter archive import is already in progress. Wait for it to finish before uploading another archive.".freeze
  SUCCESS_NOTICE = "Twitter archive import queued. Check the history below for progress.".freeze

  class Result
    attr_reader :notice, :alert, :import

    def initialize(success:, notice: nil, alert: nil, import: nil)
      @success = success
      @notice = notice
      @alert = alert
      @import = import
    end

    def success?
      @success
    end
  end

  def initialize(uploaded_file)
    @uploaded_file = uploaded_file
  end

  def submit
    return failure(INVALID_UPLOAD_ALERT) unless valid_zip_upload?
    return failure(ACTIVE_IMPORT_ALERT) if TwitterArchiveImport.active.exists?

    @temp_path = write_temp_zip
    import = create_import!(@temp_path)

    ActivityLog.log!(
      action: :queued,
      target: :twitter_archive,
      level: :info,
      filename: import.source_filename,
      import_id: import.id
    )

    TwitterArchiveImportJob.perform_later(import.id)

    Result.new(success: true, notice: SUCCESS_NOTICE, import: import)
  rescue ActiveRecord::RecordInvalid, ActiveRecord::RecordNotUnique => e
    if @import&.persisted?
      fail_import(e)
      failure("Twitter archive import failed: #{e.message}")
    else
      cleanup_temp_path(@temp_path)
      failure(ACTIVE_IMPORT_ALERT)
    end
  rescue StandardError => e
    fail_import(e)
    failure("Twitter archive import failed: #{e.message}")
  ensure
    cleanup_temp_path(@temp_path) if @import.blank? || !@import.persisted?
  end

  private

  def valid_zip_upload?
    @uploaded_file.present? && (
      @uploaded_file.content_type == "application/zip" ||
      @uploaded_file.original_filename.to_s.downcase.end_with?(".zip")
    )
  end

  def create_import!(source_path)
    @import = TwitterArchiveImport.create_queued!(
      source_filename: @uploaded_file.original_filename.to_s,
      source_path: source_path
    )
  end

  def write_temp_zip
    temp_dir = Rails.root.join("tmp", "twitter_archives")
    FileUtils.mkdir_p(temp_dir)
    temp_path = temp_dir.join("twitter_archive_#{Time.current.to_i}_#{SecureRandom.hex(8)}.zip")
    source = upload_source

    copy_upload(source, temp_path) unless move_uploaded_tempfile(source, temp_path)

    temp_path.to_s
  end

  def upload_source
    if @uploaded_file.respond_to?(:tempfile) && @uploaded_file.tempfile
      @uploaded_file.tempfile
    else
      @uploaded_file
    end
  end

  def move_uploaded_tempfile(source, destination)
    return false unless source.is_a?(Tempfile)

    source_path = source.path.to_s
    return false if source_path.blank? || !File.exist?(source_path)

    source.flush if source.respond_to?(:flush)
    File.rename(source_path, destination)
    true
  rescue SystemCallError
    false
  end

  def copy_upload(source, destination)
    File.open(destination, "wb") do |file|
      source.rewind if source.respond_to?(:rewind)
      IO.copy_stream(source, file)
    end
  end

  def fail_import(error)
    return unless @import&.persisted?

    @import.fail_import!(error)
    @import.cleanup_source_file!

    ActivityLog.log!(
      action: :failed,
      target: :twitter_archive,
      level: :error,
      filename: @import.source_filename,
      import_id: @import.id,
      error: error.message
    )
  rescue StandardError
    nil
  end

  def cleanup_temp_path(path)
    return if path.blank?

    File.delete(path) if File.exist?(path)
  end

  def failure(alert)
    Result.new(success: false, alert: alert)
  end
end
