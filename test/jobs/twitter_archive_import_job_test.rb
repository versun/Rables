# frozen_string_literal: true

require "test_helper"
require "zip"

class TwitterArchiveImportJobTest < ActiveJob::TestCase
  test "marks import as completed and stores category counts" do
    zip_path = build_zip(
      "data/account.js" => js_payload("account", [
        {
          account: {
            username: "archive_owner"
          }
        }
      ]),
      "data/tweets.js" => js_payload("tweets", [
        {
          tweet: tweet_payload(
            id: "100",
            created_at: "Wed Oct 10 20:19:24 +0000 2018",
            full_text: "Original tweet"
          )
        }
      ]),
      "data/follower.js" => js_payload("follower", [
        {
          follower: {
            accountId: "900",
            userLink: "https://twitter.com/follower_one"
          }
        }
      ]),
      "data/following.js" => js_payload("following", [
        {
          following: {
            accountId: "901",
            userLink: "https://twitter.com/following_one"
          }
        }
      ]),
      "data/like.js" => js_payload("like", [
        {
          like: {
            tweetId: "777",
            fullText: "Liked tweet text",
            expandedUrl: "https://twitter.com/someone/status/777"
          }
        }
      ])
    )
    import = TwitterArchiveImport.create!(
      source_filename: "twitter-archive.zip",
      source_path: zip_path,
      status: "queued",
      progress: 0,
      queued_at: Time.current
    )

    assert_no_enqueued_jobs do
      TwitterArchiveImportJob.perform_now(import.id)
    end

    import.reload

    assert_equal "completed", import.status
    assert_equal 100, import.progress
    assert_equal 1, import.tweets_count
    assert_equal 1, import.followers_count
    assert_equal 1, import.following_count
    assert_equal 1, import.likes_count
    assert_equal 4, import.total_items_count
    assert_not_nil import.started_at
    assert_not_nil import.finished_at
    assert_nil import.error_message
    assert_equal [ "100" ], TwitterArchiveTweet.pluck(:tweet_id)
    assert_equal [ [ "900", "follower" ], [ "901", "following" ] ],
      TwitterArchiveConnection.order(:relationship_type, :account_id).pluck(:account_id, :relationship_type)
    assert_equal [ "777" ], TwitterArchiveLike.pluck(:tweet_id)
    assert_not File.exist?(zip_path), "expected temp zip to be removed"
  end

  test "marks import as failed when archive import raises" do
    zip_path = build_zip(
      "data/tweets.js" => "window.YTD.tweets.part0 = not-valid-json"
    )
    import = TwitterArchiveImport.create!(
      source_filename: "twitter-archive.zip",
      source_path: zip_path,
      status: "queued",
      progress: 0,
      queued_at: Time.current
    )

    TwitterArchiveImportJob.perform_now(import.id)

    import.reload

    assert_equal "failed", import.status
    assert_not_nil import.started_at
    assert_not_nil import.finished_at
    assert_operator import.progress, :<, 100
    assert_match "unexpected token", import.error_message.downcase
    assert_not File.exist?(zip_path), "expected temp zip to be removed"
  end

  test "downloads a scoped direct upload inside the job" do
    archive_bytes = build_zip_bytes(
      "data/account.js" => js_payload("account", [
        {
          account: {
            username: "archive_owner"
          }
        }
      ]),
      "data/tweets.js" => js_payload("tweets", [
        {
          tweet: tweet_payload(
            id: "100",
            created_at: "Wed Oct 10 20:19:24 +0000 2018",
            full_text: "Original tweet"
          )
        }
      ])
    )
    blob = ActiveStorage::Blob.create_and_upload!(
      io: StringIO.new(archive_bytes),
      filename: "twitter-archive.zip",
      content_type: "application/zip"
    )
    import = TwitterArchiveImport.create!(
      source_filename: "twitter-archive.zip",
      source_path: "direct-upload://#{blob.signed_id(purpose: :twitter_archive_import)}",
      status: "queued",
      progress: 0,
      queued_at: Time.current
    )

    TwitterArchiveImportJob.perform_now(import.id)

    import.reload

    assert_equal "completed", import.status
    assert_equal [ "100" ], TwitterArchiveTweet.pluck(:tweet_id)
    assert_not ActiveStorage::Blob.exists?(blob.id)
    assert_nil import.source_path
  end

  private

  def build_zip(files)
    zip_path = Rails.root.join("tmp", "twitter_archive_job_test_#{SecureRandom.hex(6)}.zip")

    Zip::File.open(zip_path, create: true) do |zip|
      files.each do |name, content|
        zip.get_output_stream(name) { |stream| stream.write(content) }
      end
    end

    zip_path.to_s
  end

  def js_payload(key, records)
    "window.YTD.#{key}.part0 = #{JSON.generate(records)}"
  end

  def build_zip_bytes(files)
    Zip::OutputStream.write_buffer do |zip|
      files.each do |name, content|
        zip.put_next_entry(name)
        zip.write(content)
      end
    end.string
  end

  def tweet_payload(id:, created_at:, full_text:)
    {
      id: id,
      id_str: id,
      created_at: created_at,
      full_text: full_text
    }.stringify_keys
  end
end
