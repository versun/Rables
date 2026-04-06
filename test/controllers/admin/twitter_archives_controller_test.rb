# frozen_string_literal: true

require "test_helper"
require "zip"

class Admin::TwitterArchivesControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    TwitterArchiveImport.delete_all
    TwitterArchiveTweet.delete_all
    TwitterArchiveConnection.delete_all
    TwitterArchiveLike.delete_all
    sign_in(@user)
  end

  def teardown
    ActiveStorage::Blob.unattached.find_each(&:purge)
  end

  public

  test "index shows archive summary and upload form" do
    TwitterArchiveTweet.create!(
      tweet_id: "existing-archive",
      entry_type: "tweet",
      screen_name: "archive_owner",
      full_text: "Existing archive entry",
      tweeted_at: Time.zone.parse("2024-01-01 10:00:00 UTC")
    )
    TwitterArchiveImport.create!(
      source_filename: "twitter-archive.zip",
      source_path: "/tmp/twitter-archive.zip",
      status: "running",
      progress: 45,
      queued_at: Time.zone.parse("2026-04-03 08:00:00 UTC"),
      started_at: Time.zone.parse("2026-04-03 08:01:00 UTC")
    )

    get admin_twitter_archives_path

    assert_response :success
    assert_select "h1", text: "Twitter Archive"
    assert_select "form[action='#{admin_twitter_archives_path}'][enctype='multipart/form-data'][data-turbo='false']" do
      assert_select "input[type='file'][data-direct-upload-url='/admin/twitter_archives/direct_uploads']"
    end
    assert_select "a[href=?]", twitter_archive_path, text: "Open Public Archive"
    assert_match "Total archived items:", response.body
    assert_match "Last imported:", response.body
    assert_match "object storage", response.body
    assert_match "Import History", response.body
    assert_match "twitter-archive.zip", response.body
    assert_match "Running", response.body
    assert_match "45%", response.body
  end

  test "index uses the model's unified last imported time" do
    import_time = Time.zone.parse("2026-04-04 12:34:56 UTC")
    original_last_imported_at = TwitterArchiveImport.method(:last_imported_at)
    TwitterArchiveImport.define_singleton_method(:last_imported_at) { import_time }

    get admin_twitter_archives_path

    assert_response :success
    assert_match import_time.to_fs(:long), response.body
  ensure
    TwitterArchiveImport.define_singleton_method(:last_imported_at, original_last_imported_at)
  end

  test "index does not enqueue archive handle sync jobs" do
    Crosspost.twitter.update!(
      enabled: true,
      api_key: "api_key",
      api_key_secret: "api_key_secret",
      access_token: "access_token",
      access_token_secret: "access_token_secret"
    )
    TwitterArchiveConnection.create!(
      account_id: "900",
      relationship_type: "follower",
      user_link: "https://twitter.com/intent/user?user_id=900"
    )

    assert_no_enqueued_jobs do
      get admin_twitter_archives_path
    end
  end

  test "create queues twitter archive import and redirects back to archive page" do
    zip_path = build_zip(
      "data/account.js" => "window.YTD.account.part0 = #{JSON.generate([ { account: { username: 'archive_owner' } } ])}",
      "data/tweets.js" => "window.YTD.tweets.part0 = #{JSON.generate([
        { tweet: { id: '200', id_str: '200', created_at: 'Wed Oct 10 20:19:24 +0000 2018', full_text: 'Original tweet' } },
        { tweet: { id: '201', id_str: '201', created_at: 'Thu Oct 11 20:19:24 +0000 2018', full_text: '@friend Reply tweet', in_reply_to_status_id_str: '1' } }
      ])}"
    )

    uploaded = fixture_file_upload(zip_path, "application/zip")

    assert_difference("TwitterArchiveImport.count", 1) do
      assert_enqueued_with(job: TwitterArchiveImportJob) do
        post admin_twitter_archives_path, params: { twitter_archive: { file: uploaded } }
      end
    end

    assert_redirected_to admin_twitter_archives_path
    assert_match "queued", flash[:notice].downcase
    import = TwitterArchiveImport.order(:created_at).last
    assert_equal "queued", import.status
    assert_equal File.basename(zip_path), import.source_filename
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "create queues twitter archive import from a direct uploaded blob" do
    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new(build_zip_bytes(
        "data/account.js" => "window.YTD.account.part0 = #{JSON.generate([ { account: { username: 'archive_owner' } } ])}"
      )),
      filename: "twitter-archive.zip",
      content_type: "application/zip"
    )
    result_import = nil

    assert_difference("TwitterArchiveImport.count", 1) do
      assert_enqueued_with(job: TwitterArchiveImportJob) do
        post admin_twitter_archives_path, params: { twitter_archive: { file: twitter_archive_direct_upload_token(blob) } }
      end
    end

    assert_redirected_to admin_twitter_archives_path
    assert_match "queued", flash[:notice].downcase

    result_import = TwitterArchiveImport.order(:created_at).last
    assert_equal "queued", result_import.status
    assert_equal "twitter-archive.zip", result_import.source_filename
  ensure
    result_import&.cleanup_source_file!
    blob&.purge
  end

  test "create rejects unscoped direct upload blob ids" do
    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new(build_zip_bytes(
        "data/account.js" => "window.YTD.account.part0 = #{JSON.generate([ { account: { username: 'archive_owner' } } ])}"
      )),
      filename: "twitter-archive.zip",
      content_type: "application/zip"
    )

    assert_no_difference("TwitterArchiveImport.count") do
      assert_no_enqueued_jobs only: TwitterArchiveImportJob do
        post admin_twitter_archives_path, params: { twitter_archive: { file: blob.signed_id } }
      end
    end

    assert_redirected_to admin_twitter_archives_path
    assert_equal TwitterArchiveImportSubmission::INVALID_UPLOAD_ALERT, flash[:alert]
    assert ActiveStorage::Blob.exists?(blob.id)
  ensure
    blob&.purge
  end

  test "create refuses to queue a new import while another import is active" do
    TwitterArchiveImport.create!(
      source_filename: "existing-twitter-archive.zip",
      source_path: "/tmp/existing-twitter-archive.zip",
      status: "running",
      progress: 40,
      status_message: "Reading archive",
      queued_at: 5.minutes.ago,
      started_at: 4.minutes.ago
    )

    zip_path = build_zip(
      "data/account.js" => "window.YTD.account.part0 = #{JSON.generate([ { account: { username: 'archive_owner' } } ])}"
    )
    uploaded = fixture_file_upload(zip_path, "application/zip")

    assert_no_difference("TwitterArchiveImport.count") do
      assert_no_enqueued_jobs only: TwitterArchiveImportJob do
        post admin_twitter_archives_path, params: { twitter_archive: { file: uploaded } }
      end
    end

    assert_redirected_to admin_twitter_archives_path
    assert_match "already", flash[:alert].downcase
    assert_match "progress", flash[:alert].downcase
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "create keeps existing archive when uploaded file is invalid" do
    TwitterArchiveTweet.create!(
      tweet_id: "existing-archive",
      entry_type: "tweet",
      screen_name: "archive_owner",
      full_text: "Existing archive entry",
      tweeted_at: Time.zone.parse("2024-01-01 10:00:00 UTC")
    )
    existing_tweet_ids = TwitterArchiveTweet.order(:tweet_id).pluck(:tweet_id)

    uploaded = fixture_file_upload("sample.txt", "text/plain")

    assert_no_difference("TwitterArchiveImport.count") do
      post admin_twitter_archives_path, params: { twitter_archive: { file: uploaded } }
    end

    assert_redirected_to admin_twitter_archives_path
    assert_equal existing_tweet_ids, TwitterArchiveTweet.order(:tweet_id).pluck(:tweet_id)
  end

  private

  def build_zip(files)
    zip_path = Rails.root.join("tmp", "twitter_archive_upload_test_#{SecureRandom.hex(6)}.zip")

    Zip::File.open(zip_path, create: true) do |zip|
      files.each do |name, content|
        zip.get_output_stream(name) { |stream| stream.write(content) }
      end
    end

    zip_path
  end

  def build_zip_bytes(files)
    Zip::OutputStream.write_buffer do |zip|
      files.each do |name, content|
        zip.put_next_entry(name)
        zip.write(content)
      end
    end.string
  end

  def twitter_archive_direct_upload_token(blob)
    blob.signed_id(purpose: :twitter_archive_import)
  end
end
