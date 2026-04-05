class CreateTwitterArchiveTweets < ActiveRecord::Migration[8.1]
  def change
    create_table :twitter_archive_tweets do |t|
      t.string :tweet_id, null: false
      t.string :entry_type, null: false
      t.string :screen_name, null: false
      t.text :full_text, null: false
      t.datetime :tweeted_at, null: false

      t.timestamps
    end

    add_index :twitter_archive_tweets, :tweet_id, unique: true
    add_index :twitter_archive_tweets, [ :entry_type, :tweeted_at ]
  end
end
