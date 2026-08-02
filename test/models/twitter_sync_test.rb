# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class TwitterSyncTest < ActiveSupport::TestCase
  test "instance returns a singleton record" do
    first = TwitterSync.instance
    second = TwitterSync.instance

    assert first.persisted?
    assert_equal first.id, second.id
    assert_equal 1, TwitterSync.count
  end

  test "instance falls back to the winning record on RecordNotUnique" do
    sync = TwitterSync.instance

    TwitterSync.stub(:first_or_create, -> { raise ActiveRecord::RecordNotUnique }) do
      assert_equal sync, TwitterSync.instance
    end
  end

  test "instance defaults to disabled" do
    assert_not TwitterSync.instance.enabled?
  end

  test "username is required when enabled" do
    sync = TwitterSync.instance
    sync.enabled = true
    sync.username = nil

    assert_not sync.valid?
    assert_includes sync.errors[:username], "can't be blank"
  end

  test "username is optional when disabled" do
    sync = TwitterSync.instance
    sync.enabled = false
    sync.username = nil

    assert sync.valid?
  end

  test "strips leading @ from username" do
    sync = TwitterSync.instance
    sync.update!(username: "@versun")

    assert_equal "versun", sync.username
  end

  test "blank username is normalized to nil" do
    sync = TwitterSync.instance
    sync.update!(username: "  ")

    assert_nil sync.username
  end

  test "sync_schedule must be a known schedule" do
    sync = TwitterSync.instance
    sync.sync_schedule = "every_5_seconds"

    assert_not sync.valid?
    assert sync.errors[:sync_schedule].present?
  end

  test "due_to_sync? is true when never synced" do
    sync = TwitterSync.instance
    sync.update_columns(last_synced_at: nil)

    assert sync.due_to_sync?
  end

  test "due_to_sync? respects the configured schedule" do
    sync = TwitterSync.instance

    sync.update!(sync_schedule: "hourly")
    sync.update_columns(last_synced_at: 30.minutes.ago)
    assert_not sync.due_to_sync?

    sync.update_columns(last_synced_at: 2.hours.ago)
    assert sync.due_to_sync?
  end

  test "due_to_sync? falls back to the shortest interval for unknown schedules" do
    sync = TwitterSync.instance
    sync.update_columns(sync_schedule: "bogus", last_synced_at: 10.minutes.ago)

    assert_not sync.due_to_sync?

    sync.update_columns(last_synced_at: 20.minutes.ago)
    assert sync.due_to_sync?
  end
end
