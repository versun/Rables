# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

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

  test "logs completion when markdown export succeeds" do
    exporter = Object.new
    exporter.define_singleton_method(:generate) { true }
    exporter.define_singleton_method(:zip_path) { "/tmp/markdown.zip" }
    exporter.define_singleton_method(:error_message) { nil }

    notifier = RecordingNotifier.new

    MarkdownExport.stub(:new, exporter) do
      assert_difference "ActivityLog.count", 1 do
        Rails.stub(:event, notifier) { ExportDataJob.perform_now("markdown") }
      end
    end

    log = ActivityLog.last
    assert_equal "completed", log.action
    assert_includes log.description, 'format="markdown"'

    assert notifier.events.any? { |name, payload| name == "export_data_job.completed" && payload[:format] == "markdown" && payload[:download_url] == "/tmp/markdown.zip" }
  end

  test "logs failure when markdown export fails" do
    exporter = Object.new
    exporter.define_singleton_method(:generate) { false }
    exporter.define_singleton_method(:error_message) { "boom" }

    notifier = RecordingNotifier.new

    MarkdownExport.stub(:new, exporter) do
      assert_difference "ActivityLog.count", 1 do
        Rails.stub(:event, notifier) { ExportDataJob.perform_now("markdown") }
      end
    end

    log = ActivityLog.last
    assert_equal "failed", log.action
    assert_includes log.description, 'format="markdown"'

    assert notifier.events.any? { |name, payload| name == "export_data_job.failed" && payload[:error_message] == "boom" }
  end
end
