class CreateTwitterArchiveConnections < ActiveRecord::Migration[8.1]
  def change
    create_table :twitter_archive_connections do |t|
      t.string :account_id, null: false
      t.string :relationship_type, null: false
      t.string :screen_name
      t.string :user_link

      t.timestamps
    end

    add_index :twitter_archive_connections, [ :relationship_type, :account_id ], unique: true, name: "index_twitter_archive_connections_on_type_and_account"
    add_index :twitter_archive_connections, :screen_name
  end
end
