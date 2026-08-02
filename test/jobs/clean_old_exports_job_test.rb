# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class CleanOldExportsJobTest < ActiveJob::TestCase
  test "cleans old exports with default days" do
    mock_result = { errors: 0, message: "Cleaned 5 files" }

    Export.stub :cleanup_old_exports, mock_result do
      assert_difference "ActivityLog.count", 1 do
        CleanOldExportsJob.perform_now
      end
    end

    log = ActivityLog.last
    assert_equal "completed", log.action
    assert_equal "export_cleanup", log.target
    assert_equal "info", log.level
  end

  test "cleans old exports with custom days" do
    mock_result = { errors: 0, message: "Cleaned 3 files" }

    Export.stub :cleanup_old_exports, mock_result do
      CleanOldExportsJob.perform_now(days: 14)
    end

    log = ActivityLog.last
    assert_equal "completed", log.action
  end

  test "logs warning level when errors occur" do
    mock_result = { errors: 2, message: "Cleaned with errors" }

    Export.stub :cleanup_old_exports, mock_result do
      CleanOldExportsJob.perform_now
    end

    log = ActivityLog.last
    assert_equal "warn", log.level
  end

  test "handles hash options from SolidQueue" do
    mock_result = { errors: 0, message: "Cleaned" }

    Export.stub :cleanup_old_exports, mock_result do
      CleanOldExportsJob.perform_now({ "days" => 10 })
    end

    log = ActivityLog.last
    assert_equal "completed", log.action
  end

  test "deletes activity logs older than 90 days" do
    old_log = ActivityLog.log!(action: :completed, target: :export_cleanup, message: "ancient")
    old_log.update_column(:created_at, 91.days.ago)

    mock_result = { errors: 0, message: "Cleaned" }

    Export.stub :cleanup_old_exports, mock_result do
      CleanOldExportsJob.perform_now
    end

    assert_not ActivityLog.exists?(old_log.id)
  end

  test "keeps activity logs newer than 90 days" do
    recent_log = ActivityLog.log!(action: :completed, target: :export_cleanup, message: "recent")

    mock_result = { errors: 0, message: "Cleaned" }

    Export.stub :cleanup_old_exports, mock_result do
      CleanOldExportsJob.perform_now
    end

    assert ActivityLog.exists?(recent_log.id)
  end

  test "reports the number of deleted activity logs" do
    2.times do |i|
      log = ActivityLog.log!(action: :completed, target: :export_cleanup, message: "ancient #{i}")
      log.update_column(:created_at, 100.days.ago)
    end

    mock_result = { errors: 0, message: "Cleaned" }

    notifier = RecordingNotifier.new

    Export.stub :cleanup_old_exports, mock_result do
      original_event = Rails.event
      Rails.define_singleton_method(:event) { notifier }
      begin
        CleanOldExportsJob.perform_now
      ensure
        Rails.define_singleton_method(:event) { original_event }
      end
    end

    event = notifier.events.find { |name, _| name == "clean_old_exports_job.completed" }
    assert_not_nil event
    assert_equal 2, event[1][:activity_logs_deleted]
  end
end
