class CreateTwitterArchiveLikes < ActiveRecord::Migration[8.1]
  def change
    create_table :twitter_archive_likes do |t|
      t.string :tweet_id, null: false
      t.text :full_text
      t.string :expanded_url

      t.timestamps
    end

    add_index :twitter_archive_likes, :tweet_id, unique: true
  end
end
