# frozen_string_literal: true

require "test_helper"
require "zip"

class TwitterArchiveImporterTest < ActiveSupport::TestCase
  setup do
    TwitterArchiveTweet.destroy_all
    TwitterArchiveConnection.delete_all
    TwitterArchiveLike.delete_all
  end

  test "imports deduplicated archive tweets with tab classification" do
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
        },
        {
          tweet: tweet_payload(
            id: "101",
            created_at: "Thu Oct 11 20:19:24 +0000 2018",
            full_text: "@friend Reply tweet",
            in_reply_to_status_id_str: "50"
          )
        },
        {
          tweet: tweet_payload(
            id: "102",
            created_at: "Fri Oct 12 20:19:24 +0000 2018",
            full_text: "Quoted tweet",
            quoted_status_id_str: "88"
          )
        },
        {
          tweet: tweet_payload(
            id: "100",
            created_at: "Wed Oct 10 20:19:24 +0000 2018",
            full_text: "Original tweet duplicate"
          )
        }
      ])
    )

    assert_difference("TwitterArchiveTweet.count", 3) do
      TwitterArchiveImporter.new(zip_path).import!
    end

    assert_equal [ "102", "101", "100" ], TwitterArchiveTweet.chronological_desc.pluck(:tweet_id)
    assert_equal [ "retweet_quote", "reply", "tweet" ], TwitterArchiveTweet.chronological_desc.pluck(:entry_type)
    assert_equal [ "archive_owner" ], TwitterArchiveTweet.distinct.pluck(:screen_name)
    assert_equal "Original tweet duplicate", TwitterArchiveTweet.find_by!(tweet_id: "100").full_text
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "merges duplicate tweet rows so longer text and media are both preserved" do
    zip_path = build_zip(
      "data/account.js" => js_payload("account", [
        {
          account: {
            username: "archive_owner"
          }
        }
      ]),
      "data/tweets_01.js" => js_payload("tweets", [
        {
          tweet: tweet_payload(
            id: "100",
            created_at: "Wed Oct 10 20:19:24 +0000 2018",
            full_text: "Short text"
          )
        }
      ]),
      "data/tweets_02.js" => js_payload("tweets", [
        {
          tweet: {
            "id" => "100",
            "id_str" => "100",
            "created_at" => "Wed Oct 10 20:19:24 +0000 2018",
            "full_text" => "This duplicate row carries the longer text version"
          }
        }
      ]),
      "data/tweets_03.js" => js_payload("tweets", [
        {
          tweet: {
            "id" => "100",
            "id_str" => "100",
            "created_at" => "Wed Oct 10 20:19:24 +0000 2018",
            "full_text" => "Short text",
            "extended_entities" => {
              "media" => [
                {
                  "media_url_https" => "https://pbs.twimg.com/media/100-photo.jpg"
                }
              ]
            }
          }
        }
      ]),
      "data/tweets_media/100-photo.jpg" => file_fixture_bytes("test_image.jpg")
    )

    TwitterArchiveImporter.new(zip_path).import!

    imported_tweet = TwitterArchiveTweet.find_by!(tweet_id: "100")

    assert_equal "This duplicate row carries the longer text version", imported_tweet.full_text
    assert_predicate imported_tweet.media, :attached?
    assert_equal [ "100-photo.jpg" ], imported_tweet.media.map { |attachment| attachment.blob.filename.to_s }
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "merges media references across duplicate tweet rows" do
    zip_path = build_zip(
      "data/account.js" => js_payload("account", [
        {
          account: {
            username: "archive_owner"
          }
        }
      ]),
      "data/tweets_01.js" => js_payload("tweets", [
        {
          tweet: {
            "id" => "100",
            "id_str" => "100",
            "created_at" => "Wed Oct 10 20:19:24 +0000 2018",
            "full_text" => "Tweet with split media references",
            "extended_entities" => {
              "media" => [
                {
                  "media_url_https" => "https://pbs.twimg.com/media/100-photo-a.jpg"
                }
              ]
            }
          }
        }
      ]),
      "data/tweets_02.js" => js_payload("tweets", [
        {
          tweet: {
            "id" => "100",
            "id_str" => "100",
            "created_at" => "Wed Oct 10 20:19:24 +0000 2018",
            "full_text" => "Tweet with split media references",
            "extended_entities" => {
              "media" => [
                {
                  "media_url_https" => "https://pbs.twimg.com/media/100-photo-b.jpg"
                }
              ]
            }
          }
        }
      ]),
      "data/tweets_media/100-photo-a.jpg" => file_fixture_bytes("test_image.jpg"),
      "data/tweets_media/100-photo-b.jpg" => file_fixture_bytes("test_image.jpg")
    )

    TwitterArchiveImporter.new(zip_path).import!

    imported_tweet = TwitterArchiveTweet.find_by!(tweet_id: "100")

    assert_predicate imported_tweet.media, :attached?
    assert_equal %w[100-photo-a.jpg 100-photo-b.jpg], imported_tweet.media.map { |attachment| attachment.blob.filename.to_s }.sort
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "uses archive owner screen name even when tweets appear before account data" do
    zip_path = build_zip(
      "data/tweets.js" => js_payload("tweets", [
        {
          tweet: {
            "id" => "100",
            "id_str" => "100",
            "created_at" => "Wed Oct 10 20:19:24 +0000 2018",
            "full_text" => "Original tweet"
          }
        }
      ]),
      "data/account.js" => js_payload("account", [
        {
          account: {
            username: "archive_owner"
          }
        }
      ])
    )

    TwitterArchiveImporter.new(zip_path).import!

    assert_equal [ "archive_owner" ], TwitterArchiveTweet.distinct.pluck(:screen_name)
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "parses iso createdAt timestamps" do
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
          tweet: {
            "id" => "100",
            "id_str" => "100",
            "createdAt" => "2018-10-10T20:19:24.000Z",
            "full_text" => "Original tweet"
          }
        }
      ])
    )

    TwitterArchiveImporter.new(zip_path).import!

    assert_equal Time.iso8601("2018-10-10T20:19:24Z"), TwitterArchiveTweet.find_by!(tweet_id: "100").tweeted_at
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "imports tweets with legacy.created_at timestamps" do
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
          tweet: {
            "id" => "100",
            "id_str" => "100",
            "legacy" => {
              "created_at" => "Wed Oct 10 20:19:24 +0000 2018",
              "full_text" => "Legacy tweet"
            }
          }
        }
      ])
    )

    TwitterArchiveImporter.new(zip_path).import!

    assert_equal Time.zone.parse("2018-10-10 20:19:24 UTC"), TwitterArchiveTweet.find_by!(tweet_id: "100").tweeted_at
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "imports media-only tweets" do
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
          tweet: {
            "id" => "100",
            "id_str" => "100",
            "legacy" => {
              "created_at" => "Wed Oct 10 20:19:24 +0000 2018",
              "extended_entities" => {
                "media" => [
                  {
                    "media_url_https" => "https://pbs.twimg.com/media/100-photo.jpg"
                  }
                ]
              }
            }
          }
        }
      ]),
      "data/tweets_media/100-photo.jpg" => file_fixture_bytes("test_image.jpg")
    )

    TwitterArchiveImporter.new(zip_path).import!

    imported_tweet = TwitterArchiveTweet.find_by!(tweet_id: "100")

    assert_equal "", imported_tweet.full_text
    assert_predicate imported_tweet.media, :attached?
    assert_equal [ "100-photo.jpg" ], imported_tweet.media.map { |attachment| attachment.blob.filename.to_s }
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "imports followers following and likes alongside tweets" do
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

    summary = TwitterArchiveImporter.new(zip_path).import!

    assert_equal(
      {
        tweets: 1,
        followers: 1,
        following: 1,
        likes: 1,
        total_items: 4
      },
      summary
    )
    assert_equal [ "100" ], TwitterArchiveTweet.pluck(:tweet_id)
    assert_equal [ [ "900", "follower", "https://twitter.com/follower_one" ], [ "901", "following", "https://twitter.com/following_one" ] ],
      TwitterArchiveConnection.order(:relationship_type, :account_id).pluck(:account_id, :relationship_type, :user_link)
    assert_equal [ [ "777", "Liked tweet text", "https://twitter.com/someone/status/777" ] ],
      TwitterArchiveLike.pluck(:tweet_id, :full_text, :expanded_url)
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "imports tweet-like payloads from nonstandard archive entries" do
    zip_path = build_zip(
      "data/account.js" => js_payload("account", [
        {
          account: {
            username: "archive_owner"
          }
        }
      ]),
      "data/other.js" => js_payload("other", [
        tweet_payload(
          id: "150",
          created_at: "Sat Oct 13 20:19:24 +0000 2018",
          full_text: "Tweet from another archive entry"
        )
      ])
    )

    summary = TwitterArchiveImporter.new(zip_path).import!

    assert_equal 1, summary[:tweets]
    assert_equal [ "150" ], TwitterArchiveTweet.pluck(:tweet_id)
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "imports raw json archive entries that contain equals signs in text" do
    zip_path = build_zip(
      "data/account.json" => JSON.generate([
        {
          account: {
            username: "archive_owner"
          }
        }
      ]),
      "data/tweets.json" => JSON.generate([
        {
          tweet: {
            "id" => "160",
            "id_str" => "160",
            "created_at" => "Sat Oct 13 20:19:24 +0000 2018",
            "full_text" => "Archive link https://example.com/?token=a=b"
          }
        }
      ])
    )

    summary = TwitterArchiveImporter.new(zip_path).import!

    assert_equal 1, summary[:tweets]
    assert_equal "Archive link https://example.com/?token=a=b", TwitterArchiveTweet.find_by!(tweet_id: "160").full_text
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "reports staged progress milestones during import" do
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
      ])
    )
    progress_events = []

    TwitterArchiveImporter.new(
      zip_path,
      progress_callback: ->(progress, message) { progress_events << [ progress, message ] }
    ).import!

    assert_equal(
      [
        [ 5, "Validating archive" ],
        [ 25, "Scanning archive" ],
        [ 55, "Archive parsed" ],
        [ 80, "Replacing stored archive" ],
        [ 95, "Cleaning up media" ],
        [ 100, "Import completed" ]
      ],
      progress_events
    )
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "does not replace existing archive when import fails" do
    TwitterArchiveTweet.create!(
      tweet_id: "existing-1",
      entry_type: "tweet",
      screen_name: "archive_owner",
      full_text: "Existing archive entry",
      tweeted_at: Time.zone.parse("2024-01-01 10:00:00 UTC")
    )

    zip_path = build_zip(
      "data/tweets.js" => "window.YTD.tweets.part0 = not-valid-json"
    )

    assert_raises(TwitterArchiveImporter::ImportError) do
      TwitterArchiveImporter.new(zip_path).import!
    end

    assert_equal [ "existing-1" ], TwitterArchiveTweet.pluck(:tweet_id)
    assert_equal "Existing archive entry", TwitterArchiveTweet.first.full_text
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "imports tweet media files into active storage" do
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
            full_text: "Tweet with media"
          )
        }
      ]),
      "data/tweets_media/100-photo.jpg" => file_fixture_bytes("test_image.jpg"),
      "data/tweets_media/100-clip.mp4" => "fake-mp4-data"
    )

    assert_difference("ActiveStorage::Blob.count", 2) do
      TwitterArchiveImporter.new(zip_path).import!
    end

    imported_tweet = TwitterArchiveTweet.find_by!(tweet_id: "100")

    assert_respond_to imported_tweet, :media
    assert_predicate imported_tweet.media, :attached?
    assert_equal %w[100-clip.mp4 100-photo.jpg], imported_tweet.media.map { |attachment| attachment.blob.filename.to_s }.sort
    assert_equal %w[image/jpeg video/mp4], imported_tweet.media.map { |attachment| attachment.blob.content_type }.sort
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "does not attach media to tweets whose ids only appear later in the filename" do
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
            id: "12345",
            created_at: "Wed Oct 10 20:19:24 +0000 2018",
            full_text: "Tweet with inferred media"
          )
        },
        {
          tweet: tweet_payload(
            id: "2024",
            created_at: "Thu Oct 11 20:19:24 +0000 2018",
            full_text: "Tweet without media"
          )
        },
        {
          tweet: tweet_payload(
            id: "4",
            created_at: "Fri Oct 12 20:19:24 +0000 2018",
            full_text: "Another tweet without media"
          )
        }
      ]),
      "data/tweets_media/12345-clip-2024.mp4" => "fake-mp4-data"
    )

    TwitterArchiveImporter.new(zip_path).import!

    assert_equal [ "12345-clip-2024.mp4" ],
      TwitterArchiveTweet.find_by!(tweet_id: "12345").media.map { |attachment| attachment.blob.filename.to_s }
    assert_empty TwitterArchiveTweet.find_by!(tweet_id: "2024").media.attachments
    assert_empty TwitterArchiveTweet.find_by!(tweet_id: "4").media.attachments
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  test "purges media blobs from replaced archive tweets" do
    existing_tweet = TwitterArchiveTweet.create!(
      tweet_id: "existing-1",
      entry_type: "tweet",
      screen_name: "archive_owner",
      full_text: "Existing archive entry",
      tweeted_at: Time.zone.parse("2024-01-01 10:00:00 UTC")
    )
    existing_tweet.media.attach(
      io: StringIO.new("old-image"),
      filename: "existing-image.png",
      content_type: "image/png"
    )
    existing_blob_id = existing_tweet.media.first.blob.id

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
            id: "200",
            created_at: "Wed Oct 10 20:19:24 +0000 2018",
            full_text: "Replacement tweet"
          )
        }
      ])
    )

    TwitterArchiveImporter.new(zip_path).import!

    assert_equal [ "200" ], TwitterArchiveTweet.pluck(:tweet_id)
    assert_not ActiveStorage::Blob.exists?(existing_blob_id)
  ensure
    File.delete(zip_path) if zip_path.present? && File.exist?(zip_path)
  end

  private

  def build_zip(files)
    zip_path = Rails.root.join("tmp", "twitter_archive_test_#{SecureRandom.hex(6)}.zip")

    Zip::File.open(zip_path, create: true) do |zip|
      files.each do |name, content|
        zip.get_output_stream(name) { |stream| stream.write(content) }
      end
    end

    zip_path
  end

  def js_payload(key, records)
    "window.YTD.#{key}.part0 = #{JSON.generate(records)}"
  end

  def file_fixture_bytes(name)
    File.binread(Rails.root.join("test/fixtures/files", name))
  end

  def tweet_payload(id:, created_at:, full_text:, in_reply_to_status_id_str: nil, quoted_status_id_str: nil, retweeted_status_id_str: nil)
    {
      id: id,
      id_str: id,
      created_at: created_at,
      full_text: full_text,
      in_reply_to_status_id_str: in_reply_to_status_id_str,
      quoted_status_id_str: quoted_status_id_str,
      retweeted_status_id_str: retweeted_status_id_str
    }.compact.stringify_keys
  end
end
