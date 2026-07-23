class AddSyncScheduleToTwitterSyncs < ActiveRecord::Migration[8.1]
  def change
    add_column :twitter_syncs, :sync_schedule, :string, null: false, default: "every_15_minutes"
  end
end
