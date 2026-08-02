class AddQueryPerformanceIndexes < ActiveRecord::Migration[8.1]
  def change
    add_index :activity_logs, :created_at
    add_index :comments, :status
    add_index :twitter_archive_tweets, :created_at
    add_index :twitter_archive_likes, [ :created_at, :tweet_id ]
    add_index :active_storage_blobs, :filename
  end
end
