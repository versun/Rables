# frozen_string_literal: true

require "test_helper"

class JekyllSyncRecordTest < ActiveSupport::TestCase
  def setup
    @record = JekyllSyncRecord.create!(
      sync_type: "full",
      status: "pending"
    )
  end

  test "validates sync_type inclusion" do
    @record.sync_type = "invalid"
    assert_not @record.valid?
    assert_includes @record.errors[:sync_type], "is not included in the list"
  end

  test "validates status inclusion" do
    @record.status = "invalid"
    assert_not @record.valid?
    assert_includes @record.errors[:status], "is not included in the list"
  end

  test "mark_started! updates status and started_at" do
    freeze_time do
      @record.mark_started!
      assert_equal "in_progress", @record.status
      assert_equal Time.current, @record.started_at
    end
  end

  test "mark_completed! updates status and completed_at" do
    freeze_time do
      @record.mark_completed!(commit_sha: "abc123")
      assert_equal "completed", @record.status
      assert_equal Time.current, @record.completed_at
      assert_equal "abc123", @record.git_commit_sha
    end
  end

  test "mark_failed! updates status and error_message" do
    freeze_time do
      @record.mark_failed!("Something went wrong")
      assert_equal "failed", @record.status
      assert_equal Time.current, @record.completed_at
      assert_equal "Something went wrong", @record.error_message
    end
  end

  test "successful? returns true for completed status" do
    @record.update(status: "completed")
    assert @record.successful?
  end

  test "successful? returns false for other statuses" do
    @record.update(status: "failed")
    assert_not @record.successful?
  end

  test "duration calculates correctly" do
    @record.update(started_at: 10.seconds.ago, completed_at: 5.seconds.ago)
    assert_in_delta 5.0, @record.duration, 0.1
  end

  test "duration returns nil without started_at" do
    @record.update(started_at: nil, completed_at: Time.current)
    assert_nil @record.duration
  end

  test "duration returns nil without completed_at" do
    @record.update(started_at: Time.current, completed_at: nil)
    assert_nil @record.duration
  end

  test "summary returns formatted string with counts" do
    @record.update(articles_count: 5, pages_count: 3)
    assert_equal "5 articles, 3 pages", @record.summary
  end

  test "summary returns only articles when pages is zero" do
    @record.update(articles_count: 5, pages_count: 0)
    assert_equal "5 articles", @record.summary
  end

  test "summary returns message when no items" do
    @record.update(articles_count: 0, pages_count: 0)
    assert_equal "No items", @record.summary
  end

  test "recent scope returns records ordered by created_at desc" do
    old_record = JekyllSyncRecord.create!(sync_type: "full", status: "completed", created_at: 1.day.ago)
    assert_equal [@record, old_record], JekyllSyncRecord.recent.to_a
  end

  test "completed scope filters by status" do
    completed = JekyllSyncRecord.create!(sync_type: "full", status: "completed")
    assert_includes JekyllSyncRecord.completed, completed
    assert_not_includes JekyllSyncRecord.completed, @record
  end
end
