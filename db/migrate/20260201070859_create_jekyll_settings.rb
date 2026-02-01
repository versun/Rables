class CreateJekyllSettings < ActiveRecord::Migration[8.1]
  def change
    create_table :jekyll_settings do |t|
      t.string :jekyll_path
      t.string :repository_type, default: "local"
      t.string :repository_url
      t.string :branch, default: "main"
      t.string :posts_directory, default: "_posts"
      t.string :pages_directory, default: "_pages"
      t.string :assets_directory, default: "assets/images"
      t.json :front_matter_mapping
      t.boolean :auto_sync_enabled, default: false
      t.boolean :sync_on_publish, default: false
      t.datetime :last_sync_at

      t.string :redirect_export_format

      t.string :static_files_directory, default: "assets"
      t.boolean :preserve_original_paths, default: false

      t.boolean :export_comments, default: true
      t.string :comments_format, default: "yaml"
      t.boolean :include_pending_comments, default: false
      t.boolean :include_social_comments, default: true

      t.string :images_directory, default: "assets/images/posts"
      t.boolean :download_remote_images, default: true

      t.timestamps
    end
  end
end
