# frozen_string_literal: true

require "test_helper"

class ExportDataJobTest < ActiveJob::TestCase
  test "logs activity on successful export" do
    mock_exporter = Minitest::Mock.new
    mock_exporter.expect :generate, true
    mock_exporter.expect :zip_path, "/tmp/export.zip"

    Export.stub :new, mock_exporter do
      assert_difference "ActivityLog.count", 1 do
        ExportDataJob.perform_now
      end
    end

    log = ActivityLog.last
    assert_equal "completed", log.action
    assert_equal "export", log.target
    assert_equal "info", log.level

    mock_exporter.verify
  end

  test "logs activity on failed export" do
    mock_exporter = Minitest::Mock.new
    mock_exporter.expect :generate, false
    mock_exporter.expect :error_message, "Export failed"
    mock_exporter.expect :error_message, "Export failed"  # Called twice in the job

    Export.stub :new, mock_exporter do
      assert_difference "ActivityLog.count", 1 do
        ExportDataJob.perform_now
      end
    end

    log = ActivityLog.last
    assert_equal "failed", log.action
    assert_equal "export", log.target
    assert_equal "error", log.level

    mock_exporter.verify
  end
end
