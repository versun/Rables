# frozen_string_literal: true

require "test_helper"

class JekyllSyncRecordTest < ActiveSupport::TestCase
  test "validates sync_type presence" do
    record = JekyllSyncRecord.new(status: :pending)
    assert_not record.valid?
    assert_includes record.errors[:sync_type], "can't be blank"
  end

  test "validates status presence" do
    record = JekyllSyncRecord.new(sync_type: :full, status: nil)
    # Status has a default value, so we need to explicitly set it to nil
    record.status = nil
    assert_not record.valid?
    assert_includes record.errors[:status], "can't be blank"
  end

  test "sync_type enum values" do
    record = JekyllSyncRecord.new(sync_type: :full, status: :pending)
    assert record.sync_type_full?

    record.sync_type = :incremental
    assert record.sync_type_incremental?

    record.sync_type = :single
    assert record.sync_type_single?
  end

  test "status enum values" do
    record = JekyllSyncRecord.new(sync_type: :full, status: :pending)
    assert record.status_pending?

    record.status = :in_progress
    assert record.status_in_progress?

    record.status = :completed
    assert record.status_completed?

    record.status = :failed
    assert record.status_failed?
  end

  test "triggered_by enum values" do
    record = JekyllSyncRecord.new(sync_type: :full, status: :pending, triggered_by: :manual)
    assert record.triggered_by_manual?

    record.triggered_by = :auto
    assert record.triggered_by_auto?

    record.triggered_by = :publish
    assert record.triggered_by_publish?
  end

  test "recent scope orders by created_at desc" do
    old_record = JekyllSyncRecord.create!(sync_type: :full, status: :completed, created_at: 2.days.ago)
    new_record = JekyllSyncRecord.create!(sync_type: :full, status: :completed, created_at: 1.day.ago)

    records = JekyllSyncRecord.recent
    assert_equal new_record.id, records.first.id
    assert_equal old_record.id, records.second.id
  end

  test "successful scope returns completed records" do
    completed = JekyllSyncRecord.create!(sync_type: :full, status: :completed)
    JekyllSyncRecord.create!(sync_type: :full, status: :failed)
    JekyllSyncRecord.create!(sync_type: :full, status: :pending)

    successful = JekyllSyncRecord.successful
    assert_includes successful, completed
    assert_equal 1, successful.count
  end

  test "failed_records scope returns failed records" do
    JekyllSyncRecord.create!(sync_type: :full, status: :completed)
    failed = JekyllSyncRecord.create!(sync_type: :full, status: :failed)

    failed_records = JekyllSyncRecord.failed_records
    assert_includes failed_records, failed
    assert_equal 1, failed_records.count
  end

  test "mark_in_progress! updates status and started_at" do
    record = JekyllSyncRecord.create!(sync_type: :full, status: :pending)
    record.mark_in_progress!

    assert record.status_in_progress?
    assert_not_nil record.started_at
  end

  test "mark_completed! updates status and counts" do
    record = JekyllSyncRecord.create!(sync_type: :full, status: :in_progress, started_at: Time.current)
    record.mark_completed!(articles: 5, pages: 2, attachments: 10, git_sha: "abc123")

    assert record.status_completed?
    assert_not_nil record.completed_at
    assert_equal 5, record.articles_count
    assert_equal 2, record.pages_count
    assert_equal 10, record.attachments_count
    assert_equal "abc123", record.git_commit_sha
  end

  test "mark_failed! updates status and error_message" do
    record = JekyllSyncRecord.create!(sync_type: :full, status: :in_progress, started_at: Time.current)
    record.mark_failed!("Something went wrong")

    assert record.status_failed?
    assert_not_nil record.completed_at
    assert_equal "Something went wrong", record.error_message
  end

  test "duration calculates time difference" do
    record = JekyllSyncRecord.new(
      sync_type: :full,
      status: :completed,
      started_at: 10.seconds.ago,
      completed_at: Time.current
    )

    assert_in_delta 10, record.duration, 1
  end

  test "duration returns nil without started_at" do
    record = JekyllSyncRecord.new(sync_type: :full, status: :pending)
    assert_nil record.duration
  end

  test "duration_in_words formats seconds" do
    record = JekyllSyncRecord.new(
      sync_type: :full,
      status: :completed,
      started_at: 30.seconds.ago,
      completed_at: Time.current
    )

    assert_match(/seconds/, record.duration_in_words)
  end

  test "duration_in_words formats minutes" do
    record = JekyllSyncRecord.new(
      sync_type: :full,
      status: :completed,
      started_at: 5.minutes.ago,
      completed_at: Time.current
    )

    assert_match(/minutes/, record.duration_in_words)
  end

  test "duration_in_words formats hours" do
    record = JekyllSyncRecord.new(
      sync_type: :full,
      status: :completed,
      started_at: 2.hours.ago,
      completed_at: Time.current
    )

    assert_match(/hours/, record.duration_in_words)
  end

  test "details_hash parses JSON correctly" do
    record = JekyllSyncRecord.new(
      sync_type: :full,
      status: :completed,
      details: '{"synced_files": ["file1.md", "file2.md"]}'
    )

    expected = { "synced_files" => [ "file1.md", "file2.md" ] }
    assert_equal expected, record.details_hash
  end

  test "details_hash returns empty hash for invalid JSON" do
    record = JekyllSyncRecord.new(
      sync_type: :full,
      status: :completed,
      details: "{invalid"
    )

    assert_equal({}, record.details_hash)
  end

  test "details_hash= sets JSON string" do
    record = JekyllSyncRecord.new(sync_type: :full, status: :pending)
    record.details_hash = { "key" => "value" }

    assert_equal '{"key":"value"}', record.details
  end

  test "add_detail merges into existing details" do
    record = JekyllSyncRecord.new(
      sync_type: :full,
      status: :pending,
      details: '{"existing": "value"}'
    )

    record.add_detail(:new_key, "new_value")

    expected = { "existing" => "value", "new_key" => "new_value" }
    assert_equal expected, record.details_hash
  end

  test "set_started_at callback sets started_at for in_progress status" do
    record = JekyllSyncRecord.create!(sync_type: :full, status: :in_progress)
    assert_not_nil record.started_at
  end

  test "set_started_at callback does not set started_at for pending status" do
    record = JekyllSyncRecord.create!(sync_type: :full, status: :pending)
    assert_nil record.started_at
  end
end
