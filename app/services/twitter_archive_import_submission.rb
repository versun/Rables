require "fileutils"
require "securerandom"
require "tempfile"

class TwitterArchiveImportSubmission
  INVALID_UPLOAD_ALERT = "Please upload a valid Twitter archive ZIP file".freeze
  ACTIVE_IMPORT_ALERT = "A Twitter archive import is already in progress. Wait for it to finish before uploading another archive.".freeze
  SUCCESS_NOTICE = "Twitter archive import queued. Check the history below for progress.".freeze
  DIRECT_UPLOAD_PURPOSE = :twitter_archive_import
  DIRECT_UPLOAD_SOURCE_PREFIX = "direct-upload://".freeze

  class InvalidUploadError < StandardError; end

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

  class << self
    def direct_upload_token_for(blob)
      blob.signed_id(purpose: DIRECT_UPLOAD_PURPOSE)
    end

    def direct_upload_source_path(token)
      "#{DIRECT_UPLOAD_SOURCE_PREFIX}#{token}"
    end

    def direct_upload_source_path?(source_path)
      source_path.to_s.start_with?(DIRECT_UPLOAD_SOURCE_PREFIX)
    end

    def direct_upload_token_from(source_path)
      return unless direct_upload_source_path?(source_path)

      source_path.delete_prefix(DIRECT_UPLOAD_SOURCE_PREFIX)
    end

    def direct_upload_blob_from(source)
      token = direct_upload_token_from(source) || source
      ActiveStorage::Blob.find_signed(token, purpose: DIRECT_UPLOAD_PURPOSE)
    end
  end

  def initialize(source)
    @source = source
  end

  def submit
    return failure(INVALID_UPLOAD_ALERT) if @source.blank?
    resolve_source!

    if TwitterArchiveImport.active.exists?
      purge_direct_upload_blob
      return failure(ACTIVE_IMPORT_ALERT)
    end

    @source_path = prepare_source_path
    import = create_import!(@source_path)

    ActivityLog.log!(
      action: :queued,
      target: :twitter_archive,
      level: :info,
      filename: import.source_filename,
      import_id: import.id
    )

    TwitterArchiveImportJob.perform_later(import.id)

    Result.new(success: true, notice: SUCCESS_NOTICE, import: import)
  rescue InvalidUploadError => e
    purge_direct_upload_blob
    failure(e.message)
  rescue ActiveRecord::RecordInvalid, ActiveRecord::RecordNotUnique => e
    if @import&.persisted?
      fail_import(e)
      failure("Twitter archive import failed: #{e.message}")
    else
      failure(ACTIVE_IMPORT_ALERT)
    end
  rescue StandardError => e
    fail_import(e)
    failure("Twitter archive import failed: #{e.message}")
  ensure
    cleanup_unpersisted_source if @import.blank? || !@import.persisted?
  end

  private

  def resolve_source!
    if direct_upload_blob_id?
      @direct_upload_blob = self.class.direct_upload_blob_from(@source)
      failure!(INVALID_UPLOAD_ALERT) unless zip_blob?(@direct_upload_blob)
      @source_filename = @direct_upload_blob.filename.to_s
    else
      failure!(INVALID_UPLOAD_ALERT) unless valid_zip_upload?
      @source_filename = @source.original_filename.to_s
    end
  end

  def prepare_source_path
    if direct_upload_blob_id?
      self.class.direct_upload_source_path(@source)
    else
      write_uploaded_zip
    end
  end

  def failure!(alert)
    raise InvalidUploadError, alert
  end

  def direct_upload_blob_id?
    @source.is_a?(String)
  end

  def valid_zip_upload?
    @source.present? && zip_file?(@source.content_type, @source.original_filename)
  end

  def create_import!(source_path)
    @import = TwitterArchiveImport.create_queued!(
      source_filename: @source_filename,
      source_path: source_path
    )
  end

  def write_uploaded_zip
    temp_dir = Rails.root.join("tmp", "twitter_archives")
    FileUtils.mkdir_p(temp_dir)
    temp_path = temp_dir.join("twitter_archive_#{Time.current.to_i}_#{SecureRandom.hex(8)}.zip")
    source = upload_source

    copy_upload(source, temp_path) unless move_uploaded_tempfile(source, temp_path)

    temp_path.to_s
  end

  def upload_source
    if @source.respond_to?(:tempfile) && @source.tempfile
      @source.tempfile
    else
      @source
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

  def cleanup_unpersisted_source
    cleanup_temp_path(@source_path)
    purge_direct_upload_blob
  end

  def purge_direct_upload_blob
    @direct_upload_blob&.purge
  end

  def zip_blob?(blob)
    blob.present? && zip_file?(blob.content_type, blob.filename)
  end

  def zip_file?(content_type, filename)
    content_type == "application/zip" || filename.to_s.downcase.end_with?(".zip")
  end

  def failure(alert)
    Result.new(success: false, alert: alert)
  end
end
