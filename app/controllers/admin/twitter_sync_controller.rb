class Admin::TwitterSyncController < Admin::BaseController
  def show
    @sync = TwitterSync.instance
  end

  def update
    @sync = TwitterSync.instance
    @sync.assign_attributes(twitter_sync_params)

    # Switching accounts restarts archiving from scratch; a changed start
    # date re-backfills from the new date (existing articles are kept,
    # not duplicated thanks to slug uniqueness). The sync cursor, last
    # success time, and last error no longer apply to the new configuration.
    cursor_reset = @sync.will_save_change_to_username? || @sync.will_save_change_to_start_date?
    @sync.user_id = nil if @sync.will_save_change_to_username?
    if cursor_reset
      @sync.since_id = nil
      @sync.last_synced_at = nil
      @sync.last_error = nil
    end

    if @sync.save
      ActivityLog.log!(
        action: :updated,
        target: :twitter_sync,
        level: :info
      )
      redirect_to admin_twitter_sync_path, notice: "Twitter sync settings updated successfully."
    else
      redirect_to admin_twitter_sync_path, alert: @sync.errors.full_messages.join(", ")
    end
  end

  def sync_now
    @sync = TwitterSync.instance

    unless @sync.enabled? && @sync.username.present? && Crosspost.for("twitter")&.enabled?
      return redirect_to admin_twitter_sync_path,
        alert: "Twitter sync is not enabled or the X/Twitter credentials are missing. Enable it in the settings before syncing."
    end

    SyncTwitterJob.perform_later(force: true)
    redirect_to admin_twitter_sync_path, notice: "Twitter sync has been queued."
  end

  private

  def twitter_sync_params
    params.expect(twitter_sync: [ :enabled, :username, :sync_schedule, :start_date ])
  end
end
