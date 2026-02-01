# frozen_string_literal: true

class CreateJekyllSettings < ActiveRecord::Migration[8.1]
  def change
    create_table :jekyll_settings do |t|
      # Basic configuration
      t.string :jekyll_path
      t.string :repository_type, default: "local" # local or git
      t.string :repository_url
      t.string :branch, default: "main"

      # Directory configuration
      t.string :posts_directory, default: "_posts"
      t.string :pages_directory, default: "_pages"
      t.string :assets_directory, default: "assets/images"
      t.string :images_directory, default: "assets/images/posts"

      # Front matter mapping (JSON)
      t.text :front_matter_mapping

      # Sync settings
      t.boolean :auto_sync_enabled, default: false
      t.boolean :sync_on_publish, default: true
      t.datetime :last_sync_at

      # Redirect export settings
      t.string :redirect_export_format, default: "netlify" # netlify, vercel, htaccess, nginx, jekyll-plugin

      # Static files settings
      t.string :static_files_directory, default: "assets"
      t.boolean :preserve_original_paths, default: false

      # Comments export settings
      t.boolean :export_comments, default: true
      t.string :comments_format, default: "yaml" # yaml or json
      t.boolean :include_pending_comments, default: false
      t.boolean :include_social_comments, default: true

      # Image settings
      t.boolean :download_remote_images, default: true

      t.timestamps
    end
  end
end
