# frozen_string_literal: true

require "test_helper"

class TwitterArchiveImportTest < ActiveSupport::TestCase
  test "create_queued! initializes a queued import" do
    import = TwitterArchiveImport.create_queued!(
      source_filename: "twitter-archive.zip",
      source_path: "/tmp/twitter-archive.zip"
    )

    assert_equal "queued", import.status
    assert_equal 0, import.progress
    assert_equal "Queued", import.status_message
    assert_not_nil import.queued_at
  end

  test "remains valid when active_slot support is unavailable" do
    import = TwitterArchiveImport.new(
      source_filename: "twitter-archive.zip",
      status: "queued",
      progress: 0,
      queued_at: Time.current
    )

    import.define_singleton_method(:has_attribute?) do |name|
      return false if name.to_s == "active_slot"

      super(name)
    end
    import.define_singleton_method(:active_slot=) do |_value|
      raise NoMethodError, "undefined method 'active_slot='"
    end

    assert_predicate import, :valid?
  end

  test "marks itself running and clears previous completion data" do
    import = twitter_archive_import
    import.update!(
      status: "completed",
      status_message: "Import completed",
      finished_at: 10.minutes.ago,
      error_message: "boom"
    )

    import.mark_running!

    assert_equal "running", import.status
    assert_equal 5, import.progress
    assert_equal "Reading archive", import.status_message
    assert_not_nil import.started_at
    assert_nil import.finished_at
    assert_nil import.error_message
  end

  test "applies progress updates and completes with summary data" do
    import = twitter_archive_import
    import.mark_running!

    import.update_import_progress!(55, "Archive parsed")
    assert_equal 55, import.progress
    assert_equal "Archive parsed", import.status_message

    import.complete_import!(
      tweets: 2,
      followers: 1,
      following: 3,
      likes: 4,
      total_items: 10
    )

    assert_equal "completed", import.status
    assert_equal 100, import.progress
    assert_equal "Import completed", import.status_message
    assert_equal 2, import.tweets_count
    assert_equal 1, import.followers_count
    assert_equal 3, import.following_count
    assert_equal 4, import.likes_count
    assert_equal 10, import.total_items_count
    assert_not_nil import.finished_at
    assert_nil import.error_message
  end

  test "marks failure and removes source path" do
    path = Rails.root.join("tmp", "twitter_archive_import_test_#{SecureRandom.hex(6)}.zip").to_s
    File.write(path, "archive-bytes")
    import = twitter_archive_import(source_path: path)

    import.fail_import!(StandardError.new("boom"))
    import.cleanup_source_file!

    assert_equal "failed", import.status
    assert_equal "Import failed", import.status_message
    assert_equal "boom", import.error_message
    assert_not_nil import.finished_at
    assert_nil import.source_path
    assert_not File.exist?(path)
  end

  test "cleans source file without raising when it is already missing" do
    import = twitter_archive_import(source_path: Rails.root.join("tmp", "missing-twitter-archive.zip").to_s)

    assert_nothing_raised do
      import.cleanup_source_file!
    end

    assert_nil import.source_path
  end

  test "last_imported_at returns the latest completed import or archive tweet time" do
    past_time = 2.days.ago
    completed_time = 1.day.ago

    TwitterArchiveImport.delete_all
    TwitterArchiveTweet.delete_all

    TwitterArchiveTweet.create!(
      tweet_id: "fallback",
      entry_type: "tweet",
      screen_name: "archive_owner",
      full_text: "Fallback tweet",
      tweeted_at: past_time,
      created_at: past_time,
      updated_at: past_time
    )

    assert_equal past_time.to_i, TwitterArchiveImport.last_imported_at.to_i

    TwitterArchiveImport.create!(
      source_filename: "twitter-archive.zip",
      source_path: "/tmp/twitter-archive.zip",
      status: "completed",
      progress: 100,
      queued_at: 3.days.ago,
      started_at: 3.days.ago,
      finished_at: completed_time
    )

    assert_equal completed_time.to_i, TwitterArchiveImport.last_imported_at.to_i
  end

  private

  def twitter_archive_import(attributes = {})
    TwitterArchiveImport.new({
      source_filename: "twitter-archive.zip",
      status: "queued",
      progress: 0,
      queued_at: Time.current
    }.merge(attributes))
  end
end
