class ConsolidateSchemaChanges < ActiveRecord::Migration[8.1]
  def change
    # Remove the discontinued Git integration feature.
    drop_table :git_integrations do |t|
      t.string :provider, null: false
      t.string :name, null: false
      t.string :server_url
      t.string :username
      t.string :access_token
      t.boolean :enabled, default: false, null: false

      t.timestamps

      t.index :provider, unique: true
    end

    # Support scheduled publishing for pages.
    add_column :pages, :scheduled_at, :datetime

    # Deduplicate external comments at the DB level for the polymorphic commentable.
    # Partial index: local comments (external_id IS NULL) are not affected.
    add_index :comments, [ :commentable_type, :commentable_id, :platform, :external_id ],
      unique: true,
      where: "external_id IS NOT NULL",
      name: "index_comments_on_commentable_platform_external_id"
  end
end
