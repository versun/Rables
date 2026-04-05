class TwitterArchiveImportJob < ApplicationJob
  queue_as :default

  def perform(import_id)
    import = TwitterArchiveImport.find(import_id)

    import.update!(
      status: "running",
      progress: 5,
      status_message: "Reading archive",
      started_at: Time.current,
      finished_at: nil,
      error_message: nil
    )

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
      import.source_path,
      progress_callback: lambda do |progress, message|
        next if progress == last_progress && message == last_message

        last_progress = progress
        last_message = message
        import.update!(progress: progress, status_message: message)
      end
    ).import!

    import.update!(
      status: "completed",
      progress: 100,
      status_message: "Import completed",
      tweets_count: summary[:tweets],
      followers_count: summary[:followers],
      following_count: summary[:following],
      likes_count: summary[:likes],
      total_items_count: summary[:total_items],
      finished_at: Time.current,
      error_message: nil
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

    TwitterArchiveHandleSyncJob.enqueue_if_needed
  rescue StandardError => e
    handle_failure(import, e) if import
  ensure
    cleanup_source_file(import) if import
  end

  private

  def handle_failure(import, error)
    import.update!(
      status: "failed",
      status_message: "Import failed",
      error_message: error.message,
      finished_at: Time.current
    )

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
    path = import.source_path.to_s
    File.delete(path) if path.present? && File.exist?(path)
    import.update_column(:source_path, nil)
  end
end
