# This file is auto-generated from the current state of the database. Instead
# of editing this file, please use the migrations feature of Active Record to
# incrementally modify your database, and then regenerate this schema definition.
#
# This file is the source Rails uses to define your schema when running `bin/rails
# db:schema:load`. When creating a new database, `bin/rails db:schema:load` tends to
# be faster and is potentially less error prone than running all of your
# migrations from scratch. Old migrations may fail to apply correctly if those
# migrations use external dependencies or application code.
#
# It's strongly recommended that you check this file into your version control system.

ActiveRecord::Schema[8.1].define(version: 2026_04_03_000120) do
  create_table "action_text_rich_texts", force: :cascade do |t|
    t.text "body"
    t.datetime "created_at", null: false
    t.string "name", null: false
    t.bigint "record_id", null: false
    t.string "record_type", null: false
    t.datetime "updated_at", null: false
    t.index ["record_type", "record_id", "name"], name: "index_action_text_rich_texts_uniqueness", unique: true
  end

  create_table "active_storage_attachments", force: :cascade do |t|
    t.bigint "blob_id", null: false
    t.datetime "created_at", null: false
    t.string "name", null: false
    t.bigint "record_id", null: false
    t.string "record_type", null: false
    t.index ["blob_id"], name: "index_active_storage_attachments_on_blob_id"
    t.index ["record_type", "record_id", "name", "blob_id"], name: "index_active_storage_attachments_uniqueness", unique: true
  end

  create_table "active_storage_blobs", force: :cascade do |t|
    t.bigint "byte_size", null: false
    t.string "checksum"
    t.string "content_type"
    t.datetime "created_at", null: false
    t.string "filename", null: false
    t.string "key", null: false
    t.text "metadata"
    t.string "service_name", null: false
    t.index ["key"], name: "index_active_storage_blobs_on_key", unique: true
  end

  create_table "active_storage_variant_records", force: :cascade do |t|
    t.bigint "blob_id", null: false
    t.string "variation_digest", null: false
    t.index ["blob_id", "variation_digest"], name: "index_active_storage_variant_records_uniqueness", unique: true
  end

  create_table "activity_logs", force: :cascade do |t|
    t.string "action"
    t.datetime "created_at", null: false
    t.text "description"
    t.integer "level", default: 0
    t.string "target"
    t.datetime "updated_at", null: false
  end

  create_table "article_tags", force: :cascade do |t|
    t.integer "article_id", null: false
    t.datetime "created_at", null: false
    t.integer "tag_id", null: false
    t.datetime "updated_at", null: false
    t.index ["article_id", "tag_id"], name: "index_article_tags_on_article_id_and_tag_id", unique: true
    t.index ["article_id"], name: "index_article_tags_on_article_id"
    t.index ["tag_id"], name: "index_article_tags_on_tag_id"
  end

  create_table "articles", force: :cascade do |t|
    t.boolean "comment", default: false, null: false
    t.string "content_type", default: "rich_text", null: false
    t.datetime "created_at", null: false
    t.string "description"
    t.text "excerpt"
    t.text "html_content"
    t.text "meta_description"
    t.string "meta_image"
    t.string "meta_title"
    t.datetime "scheduled_at"
    t.text "scheduled_crosspost_platforms", default: "[]", null: false
    t.boolean "scheduled_send_newsletter", default: false, null: false
    t.string "slug"
    t.string "source_author"
    t.text "source_content"
    t.string "source_url"
    t.integer "status", default: 0, null: false
    t.string "title"
    t.datetime "updated_at", null: false
    t.index ["slug"], name: "index_articles_on_slug", unique: true
    t.index ["status", "created_at"], name: "index_articles_on_status_and_created_at"
    t.index ["status"], name: "index_articles_on_status"
  end

  create_table "comments", force: :cascade do |t|
    t.integer "article_id"
    t.string "author_avatar_url"
    t.string "author_email"
    t.string "author_name", null: false
    t.string "author_url"
    t.string "author_username"
    t.integer "commentable_id"
    t.string "commentable_type"
    t.text "content", null: false
    t.datetime "created_at", null: false
    t.string "external_id"
    t.integer "parent_id"
    t.string "platform"
    t.datetime "published_at"
    t.integer "status", default: 0, null: false
    t.datetime "updated_at", null: false
    t.string "url"
    t.index ["article_id", "platform", "external_id"], name: "index_comments_on_article_platform_external_id", unique: true, where: "platform IS NOT NULL AND external_id IS NOT NULL"
    t.index ["article_id"], name: "index_comments_on_article_id"
    t.index ["commentable_type", "commentable_id"], name: "index_comments_on_commentable"
    t.index ["parent_id"], name: "index_comments_on_parent_id"
  end

  create_table "crossposts", force: :cascade do |t|
    t.string "access_token"
    t.string "access_token_secret"
    t.string "api_key"
    t.string "api_key_secret"
    t.string "app_password"
    t.boolean "auto_fetch_comments", default: false, null: false
    t.string "client_id"
    t.string "client_key"
    t.string "client_secret"
    t.string "comment_fetch_schedule"
    t.datetime "created_at", null: false
    t.boolean "enabled", default: false, null: false
    t.integer "max_characters"
    t.string "platform", null: false
    t.string "refresh_token"
    t.string "server_url"
    t.text "settings"
    t.datetime "token_expires_at"
    t.datetime "updated_at", null: false
    t.string "username"
    t.index ["platform"], name: "index_crossposts_on_platform", unique: true
  end

  create_table "git_integrations", force: :cascade do |t|
    t.string "access_token"
    t.datetime "created_at", null: false
    t.boolean "enabled", default: false, null: false
    t.string "name", null: false
    t.string "provider", null: false
    t.string "server_url"
    t.datetime "updated_at", null: false
    t.string "username"
    t.index ["provider"], name: "index_git_integrations_on_provider", unique: true
  end

  create_table "listmonks", force: :cascade do |t|
    t.string "api_key"
    t.datetime "created_at", null: false
    t.boolean "enabled", default: false, null: false
    t.integer "list_id"
    t.integer "template_id"
    t.datetime "updated_at", null: false
    t.string "url"
    t.string "username"
  end

  create_table "newsletter_settings", force: :cascade do |t|
    t.datetime "created_at", null: false
    t.boolean "enabled", default: false, null: false
    t.string "from_email"
    t.string "provider", default: "native", null: false
    t.string "smtp_address"
    t.string "smtp_authentication", default: "plain"
    t.string "smtp_domain"
    t.boolean "smtp_enable_starttls", default: true
    t.string "smtp_password"
    t.integer "smtp_port", default: 587
    t.string "smtp_user_name"
    t.datetime "updated_at", null: false
  end

  create_table "pages", force: :cascade do |t|
    t.boolean "comment", default: false, null: false
    t.string "content_type", default: "rich_text", null: false
    t.datetime "created_at", null: false
    t.text "html_content"
    t.integer "page_order", default: 0, null: false
    t.string "redirect_url"
    t.string "slug"
    t.integer "status", default: 0, null: false
    t.string "title"
    t.datetime "updated_at", null: false
    t.index ["slug"], name: "index_pages_on_slug", unique: true
    t.index ["status"], name: "index_pages_on_status"
  end

  create_table "redirects", force: :cascade do |t|
    t.datetime "created_at", null: false
    t.boolean "enabled", default: true
    t.boolean "permanent", default: false
    t.string "regex", null: false
    t.string "replacement", null: false
    t.datetime "updated_at", null: false
    t.index ["enabled"], name: "index_redirects_on_enabled"
  end

  create_table "sessions", force: :cascade do |t|
    t.datetime "created_at", null: false
    t.string "ip_address"
    t.datetime "updated_at", null: false
    t.string "user_agent"
    t.integer "user_id", null: false
    t.index ["user_id"], name: "index_sessions_on_user_id"
  end

  create_table "settings", force: :cascade do |t|
    t.string "author"
    t.json "auto_regenerate_triggers", default: []
    t.datetime "created_at", null: false
    t.text "custom_css"
    t.string "deploy_branch", default: "main"
    t.string "deploy_provider"
    t.string "deploy_repo_url"
    t.text "description"
    t.text "giscus"
    t.string "github_backup_branch", default: "main"
    t.boolean "github_backup_enabled", default: false
    t.string "github_repo_url"
    t.string "github_token"
    t.text "head_code"
    t.string "local_generation_path"
    t.boolean "setup_completed", default: false
    t.json "social_links"
    t.json "static_files", default: {}
    t.string "static_generation_delay"
    t.string "static_generation_destination", default: "local"
    t.string "time_zone", default: "UTC"
    t.string "title"
    t.text "tool_code"
    t.datetime "updated_at", null: false
    t.string "url"
  end

  create_table "social_media_posts", force: :cascade do |t|
    t.integer "article_id", null: false
    t.datetime "created_at", null: false
    t.string "platform", null: false
    t.datetime "updated_at", null: false
    t.string "url", null: false
    t.index ["article_id", "platform"], name: "index_social_media_posts_on_article_id_and_platform", unique: true
    t.index ["article_id"], name: "index_social_media_posts_on_article_id"
  end

  create_table "static_files", force: :cascade do |t|
    t.datetime "created_at", null: false
    t.text "description"
    t.text "filename"
    t.datetime "updated_at", null: false
    t.index ["filename"], name: "index_static_files_on_filename", unique: true
  end

  create_table "subscriber_tags", force: :cascade do |t|
    t.datetime "created_at", null: false
    t.integer "subscriber_id", null: false
    t.integer "tag_id", null: false
    t.datetime "updated_at", null: false
    t.index ["subscriber_id", "tag_id"], name: "index_subscriber_tags_on_subscriber_id_and_tag_id", unique: true
    t.index ["subscriber_id"], name: "index_subscriber_tags_on_subscriber_id"
    t.index ["tag_id"], name: "index_subscriber_tags_on_tag_id"
  end

  create_table "subscribers", force: :cascade do |t|
    t.string "confirmation_token"
    t.datetime "confirmed_at"
    t.datetime "created_at", null: false
    t.string "email", null: false
    t.string "unsubscribe_token"
    t.datetime "unsubscribed_at"
    t.datetime "updated_at", null: false
    t.index ["confirmation_token"], name: "index_subscribers_on_confirmation_token", unique: true
    t.index ["email"], name: "index_subscribers_on_email", unique: true
    t.index ["unsubscribe_token"], name: "index_subscribers_on_unsubscribe_token", unique: true
  end

  create_table "tags", force: :cascade do |t|
    t.datetime "created_at", null: false
    t.string "name", null: false
    t.string "slug", null: false
    t.datetime "updated_at", null: false
    t.index ["name"], name: "index_tags_on_name", unique: true
    t.index ["slug"], name: "index_tags_on_slug", unique: true
  end

  create_table "twitter_archive_connections", force: :cascade do |t|
    t.string "account_id", null: false
    t.datetime "created_at", null: false
    t.string "relationship_type", null: false
    t.string "screen_name"
    t.datetime "updated_at", null: false
    t.string "user_link"
    t.index ["relationship_type", "account_id"], name: "index_twitter_archive_connections_on_type_and_account", unique: true
    t.index ["screen_name"], name: "index_twitter_archive_connections_on_screen_name"
  end

  create_table "twitter_archive_imports", force: :cascade do |t|
    t.integer "active_slot"
    t.datetime "created_at", null: false
    t.text "error_message"
    t.datetime "finished_at"
    t.integer "followers_count", default: 0, null: false
    t.integer "following_count", default: 0, null: false
    t.integer "likes_count", default: 0, null: false
    t.integer "progress", default: 0, null: false
    t.datetime "queued_at", null: false
    t.string "source_filename", null: false
    t.string "source_path"
    t.datetime "started_at"
    t.string "status", default: "queued", null: false
    t.string "status_message"
    t.integer "total_items_count", default: 0, null: false
    t.integer "tweets_count", default: 0, null: false
    t.datetime "updated_at", null: false
    t.index ["active_slot"], name: "index_twitter_archive_imports_on_active_slot", unique: true, where: "active_slot IS NOT NULL"
    t.index ["created_at"], name: "index_twitter_archive_imports_on_created_at"
    t.index ["status"], name: "index_twitter_archive_imports_on_status"
  end

  create_table "twitter_archive_likes", force: :cascade do |t|
    t.datetime "created_at", null: false
    t.string "expanded_url"
    t.text "full_text"
    t.string "tweet_id", null: false
    t.datetime "updated_at", null: false
    t.index ["tweet_id"], name: "index_twitter_archive_likes_on_tweet_id", unique: true
  end

  create_table "twitter_archive_tweets", force: :cascade do |t|
    t.datetime "created_at", null: false
    t.string "entry_type", null: false
    t.text "full_text", null: false
    t.string "screen_name", null: false
    t.string "tweet_id", null: false
    t.datetime "tweeted_at", null: false
    t.datetime "updated_at", null: false
    t.index ["entry_type", "tweeted_at"], name: "index_twitter_archive_tweets_on_entry_type_and_tweeted_at"
    t.index ["tweet_id"], name: "index_twitter_archive_tweets_on_tweet_id", unique: true
  end

  create_table "users", force: :cascade do |t|
    t.datetime "created_at", null: false
    t.string "password_digest", null: false
    t.datetime "updated_at", null: false
    t.string "user_name", null: false
    t.index ["user_name"], name: "index_users_on_user_name", unique: true
  end

  add_foreign_key "active_storage_attachments", "active_storage_blobs", column: "blob_id"
  add_foreign_key "active_storage_variant_records", "active_storage_blobs", column: "blob_id"
  add_foreign_key "article_tags", "articles"
  add_foreign_key "article_tags", "tags"
  add_foreign_key "comments", "articles"
  add_foreign_key "comments", "comments", column: "parent_id", on_delete: :cascade
  add_foreign_key "sessions", "users"
  add_foreign_key "social_media_posts", "articles"
  add_foreign_key "subscriber_tags", "subscribers"
  add_foreign_key "subscriber_tags", "tags"
end
