class Admin::TwitterArchivesController < Admin::BaseController
  def index
    @twitter_archive_counts = TwitterArchiveTweet.group(:entry_type).count
    @twitter_archive_connection_counts = TwitterArchiveConnection.group(:relationship_type).count
    @twitter_archive_likes_count = TwitterArchiveLike.count
    @twitter_archive_total = TwitterArchiveTweet.count + TwitterArchiveConnection.count + @twitter_archive_likes_count
    @twitter_archive_updated_at = TwitterArchiveImport.last_imported_at
    @twitter_archive_imports = TwitterArchiveImport.recent_first.limit(10)

    TwitterArchiveHandleSyncJob.enqueue_if_needed
  end

  def create
    result = TwitterArchiveImportSubmission.new(params.dig(:twitter_archive, :file)).submit

    if result.success?
      redirect_to admin_twitter_archives_path, notice: result.notice
    else
      redirect_to admin_twitter_archives_path, alert: result.alert
    end
  end
end
