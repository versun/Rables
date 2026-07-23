class Admin::TwitterSyncController < Admin::BaseController
  def show
    @sync = TwitterSync.instance
  end

  def update
    @sync = TwitterSync.instance
    attrs = twitter_sync_params

    # Switching accounts restarts archiving from scratch
    if attrs[:username].to_s.strip.sub(/\A@/, "") != @sync.username.to_s
      attrs = attrs.merge(user_id: nil, since_id: nil)
    elsif attrs[:start_date].to_s != @sync.start_date.to_s
      # A changed start date re-backfills from the new date; existing
      # articles are kept and not duplicated (slug uniqueness)
      attrs = attrs.merge(since_id: nil)
    end

    if @sync.update(attrs)
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
    SyncTwitterJob.perform_later(force: true)
    redirect_to admin_twitter_sync_path, notice: "Twitter sync has been queued."
  end

  private

  def twitter_sync_params
    params.expect(twitter_sync: [ :enabled, :username, :sync_schedule, :start_date ])
  end
end
