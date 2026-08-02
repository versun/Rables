class Admin::TwitterArchivesController < Admin::BaseController
  def index
    @twitter_archive_counts = TwitterArchiveTweet.group(:entry_type).count
    @twitter_archive_connection_counts = TwitterArchiveConnection.group(:relationship_type).count
    @twitter_archive_likes_count = TwitterArchiveLike.count
    @twitter_archive_total = TwitterArchiveTweet.count + TwitterArchiveConnection.count + @twitter_archive_likes_count
    @twitter_archive_updated_at = TwitterArchiveImport.last_imported_at
    @twitter_archive_imports = TwitterArchiveImport.recent_first.limit(10)
  end

  def create
    # Malformed requests (e.g. twitter_archive posted as a plain string)
    # fall through to the submission's invalid-upload alert instead of 500.
    archive_params = params[:twitter_archive]
    file = archive_params.is_a?(ActionController::Parameters) ? archive_params[:file] : nil

    result = TwitterArchiveImportSubmission.new(file).submit

    if result.success?
      redirect_to admin_twitter_archives_path, notice: result.notice
    else
      redirect_to admin_twitter_archives_path, alert: result.alert
    end
  end
end
