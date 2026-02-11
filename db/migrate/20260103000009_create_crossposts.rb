class CreateCrossposts < ActiveRecord::Migration[8.1]
  def change
    create_table :crossposts do |t|
      t.string :platform, null: false
      t.string :server_url
      t.string :client_key
      t.string :client_secret
      t.string :access_token
      t.string :refresh_token
      t.string :username
      t.string :app_password
      t.boolean :enabled, default: false, null: false
      t.text :settings
      t.boolean :auto_fetch_comments, default: false, null: false
      t.string :comment_fetch_schedule
      t.integer :max_characters

      t.timestamps
    end

    add_index :crossposts, :platform, unique: true
  end
end
