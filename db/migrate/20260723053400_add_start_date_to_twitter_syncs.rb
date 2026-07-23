class AddStartDateToTwitterSyncs < ActiveRecord::Migration[8.1]
  def change
    add_column :twitter_syncs, :start_date, :date
  end
end
