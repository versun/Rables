class CreateTwitterArchiveImports < ActiveRecord::Migration[8.1]
  def change
    create_table :twitter_archive_imports do |t|
      t.integer :active_slot
      t.string :source_filename, null: false
      t.string :source_path
      t.string :status, null: false, default: "queued"
      t.integer :progress, null: false, default: 0
      t.string :status_message
      t.text :error_message
      t.integer :tweets_count, null: false, default: 0
      t.integer :followers_count, null: false, default: 0
      t.integer :following_count, null: false, default: 0
      t.integer :likes_count, null: false, default: 0
      t.integer :total_items_count, null: false, default: 0
      t.datetime :queued_at, null: false
      t.datetime :started_at
      t.datetime :finished_at

      t.timestamps
    end

    add_index :twitter_archive_imports, :status
    add_index :twitter_archive_imports, :created_at
    add_index :twitter_archive_imports, :active_slot, unique: true, where: "active_slot IS NOT NULL", name: "index_twitter_archive_imports_on_active_slot"
  end
end
