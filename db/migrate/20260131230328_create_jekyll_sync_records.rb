# frozen_string_literal: true

class CreateJekyllSyncRecords < ActiveRecord::Migration[8.1]
  def change
    create_table :jekyll_sync_records do |t|
      t.string :sync_type, null: false # full, incremental, single
      t.string :status, default: "pending" # pending, in_progress, completed, failed
      t.integer :articles_count, default: 0
      t.integer :pages_count, default: 0
      t.integer :attachments_count, default: 0
      t.text :error_message
      t.text :details # JSON for additional sync details
      t.datetime :started_at
      t.datetime :completed_at
      t.string :git_commit_sha
      t.string :triggered_by # manual, auto, publish

      t.timestamps
    end

    add_index :jekyll_sync_records, :status
    add_index :jekyll_sync_records, :sync_type
    add_index :jekyll_sync_records, :created_at
  end
end
