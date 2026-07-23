class CreateTwitterSyncs < ActiveRecord::Migration[8.1]
  def change
    create_table :twitter_syncs do |t|
      t.boolean :enabled, default: false, null: false
      t.string :username
      t.string :user_id
      t.string :since_id
      t.datetime :last_synced_at
      t.string :last_error

      t.timestamps
    end
  end
end
