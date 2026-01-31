# frozen_string_literal: true

require "test_helper"

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
end
