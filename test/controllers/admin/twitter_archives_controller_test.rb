# frozen_string_literal: true

require "test_helper"
require "zip"

class Admin::TwitterArchivesControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    sign_in(@user)
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
    assert_select "input[type='file']"
    assert_select "a[href=?]", twitter_archive_path, text: "Open Public Archive"
    assert_match "Total archived items:", response.body
    assert_match "Last imported:", response.body
    assert_match "Import History", response.body
    assert_match "twitter-archive.zip", response.body
    assert_match "Running", response.body
    assert_match "45%", response.body
  end

  test "index enqueues catch up sync when unresolved rows exist and credentials are available" do
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

    assert_enqueued_with(job: TwitterArchiveHandleSyncJob) do
      get admin_twitter_archives_path
    end
  end

  test "index does not enqueue catch up sync when no unresolved rows exist" do
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
      screen_name: "resolved_handle",
      user_link: "https://twitter.com/intent/user?user_id=900"
    )

    assert_no_enqueued_jobs only: TwitterArchiveHandleSyncJob do
      get admin_twitter_archives_path
    end
  end

  test "index does not enqueue catch up sync when twitter credentials are unavailable" do
    Crosspost.twitter.update!(
      enabled: false,
      api_key: nil,
      api_key_secret: nil,
      access_token: nil,
      access_token_secret: nil
    )
    TwitterArchiveConnection.create!(
      account_id: "900",
      relationship_type: "follower",
      user_link: "https://twitter.com/intent/user?user_id=900"
    )

    assert_no_enqueued_jobs only: TwitterArchiveHandleSyncJob do
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

    uploaded = fixture_file_upload("sample.txt", "text/plain")

    assert_no_difference("TwitterArchiveImport.count") do
      post admin_twitter_archives_path, params: { twitter_archive: { file: uploaded } }
    end

    assert_redirected_to admin_twitter_archives_path
    assert_equal [ "existing-archive" ], TwitterArchiveTweet.pluck(:tweet_id)
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
end
