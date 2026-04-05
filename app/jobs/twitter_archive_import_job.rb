class TwitterArchiveImportJob < ApplicationJob
  queue_as :default

  def perform(import_id)
    import = TwitterArchiveImport.find(import_id)

    import.mark_running!

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

    TwitterArchiveHandleSyncJob.enqueue_if_needed
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
end
