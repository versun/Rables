class CreateJekyllSyncRecords < ActiveRecord::Migration[8.1]
  def change
    create_table :jekyll_sync_records do |t|
      t.string :sync_type
      t.string :status
      t.integer :articles_count, default: 0, null: false
      t.integer :pages_count, default: 0, null: false
      t.text :error_message
      t.datetime :started_at
      t.datetime :completed_at
      t.string :git_commit_sha

      t.timestamps
    end
  end
end
