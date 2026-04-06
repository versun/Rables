class TwitterArchiveImportJob < ApplicationJob
  queue_as :default

  def perform(import_id)
    import = TwitterArchiveImport.find(import_id)

    import.mark_running!
    source_path = materialize_source_path(import)

    ActivityLog.log!(
      action: :started,
      target: :twitter_archive,
      level: :info,
      filename: import.source_filename,
      import_id: import.id
    )

    last_progress = import.progress
    last_message = import.status_message
    summary = TwitterArchiveImporter.new(
      source_path,
      progress_callback: lambda do |progress, message|
        next if progress == last_progress && message == last_message

        last_progress = progress
        last_message = message
        import.update_import_progress!(progress, message)
      end
    ).import!

    import.complete_import!(
      tweets: summary[:tweets],
      followers: summary[:followers],
      following: summary[:following],
      likes: summary[:likes],
      total_items: summary[:total_items]
    )

    ActivityLog.log!(
      action: :completed,
      target: :twitter_archive,
      level: :info,
      filename: import.source_filename,
      import_id: import.id,
      total_items: summary[:total_items],
      tweets: summary[:tweets],
      followers: summary[:followers],
      following: summary[:following],
      likes: summary[:likes]
    )
  rescue StandardError => e
    handle_failure(import, e) if import
  ensure
    cleanup_source_file(import) if import
  end

  private

  def handle_failure(import, error)
    import.fail_import!(error)

    ActivityLog.log!(
      action: :failed,
      target: :twitter_archive,
      level: :error,
      filename: import.source_filename,
      import_id: import.id,
      error: error.message
    )
  end

  def cleanup_source_file(import)
    import.cleanup_source_file!
  end

  def materialize_source_path(import)
    return import.source_path unless TwitterArchiveImportSubmission.direct_upload_source_path?(import.source_path)

    import.update_import_progress!(5, "Downloading uploaded archive")

    blob = TwitterArchiveImportSubmission.direct_upload_blob_from(import.source_path)
    raise ArgumentError, TwitterArchiveImportSubmission::INVALID_UPLOAD_ALERT if blob.blank?

    temp_path = Rails.root.join("tmp", "twitter_archives", "twitter_archive_#{Time.current.to_i}_#{SecureRandom.hex(8)}.zip")
    FileUtils.mkdir_p(temp_path.dirname)

    File.open(temp_path, "wb") do |file|
      blob.download { |chunk| file.write(chunk) }
    end

    import.update_column(:source_path, temp_path.to_s)
    temp_path.to_s
  ensure
    blob&.purge
  end
end
