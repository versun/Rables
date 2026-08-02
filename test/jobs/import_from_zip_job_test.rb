# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class ImportFromZipJobTest < ActiveJob::TestCase
  test "logs completion and cleans up temp file" do
    zip_dir = Rails.root.join("tmp", "uploads")
    FileUtils.mkdir_p(zip_dir)
    zip_path = zip_dir.join("import_test.zip").to_s
    File.write(zip_path, "dummy")

    importer = Object.new
    importer.define_singleton_method(:import_data) { true }
    importer.define_singleton_method(:error_message) { nil }

    notifier = RecordingNotifier.new

    ImportZip.stub(:new, importer) do
      assert_difference "ActivityLog.count", 1 do
        with_event_notifier(notifier) { ImportFromZipJob.perform_now(zip_path) }
      end
    end

    assert_not File.exist?(zip_path), "expected temp zip to be removed"
    assert notifier.events.any? { |name, _| name == "import_from_zip_job.completed" }
    assert notifier.events.any? { |name, _| name == "import_from_zip_job.cleanup" }
  end

  test "logs failure when import fails" do
    zip_path = "/tmp/import_failure.zip"

    importer = Object.new
    importer.define_singleton_method(:import_data) { false }
    importer.define_singleton_method(:error_message) { "bad zip" }

    notifier = RecordingNotifier.new

    ImportZip.stub(:new, importer) do
      assert_difference "ActivityLog.count", 1 do
        with_event_notifier(notifier) { ImportFromZipJob.perform_now(zip_path) }
      end
    end

    assert notifier.events.any? { |name, payload| name == "import_from_zip_job.failed" && payload[:error_message] == "bad zip" }
  end

  test "cleans up temp file when import raises" do
    zip_dir = Rails.root.join("tmp", "uploads")
    FileUtils.mkdir_p(zip_dir)
    zip_path = zip_dir.join("import_raise_test.zip").to_s
    File.write(zip_path, "dummy")

    importer = Object.new
    importer.define_singleton_method(:import_data) { raise StandardError, "explode" }

    ImportZip.stub(:new, importer) do
      assert_raises(StandardError) { ImportFromZipJob.perform_now(zip_path) }
    end

    assert_not File.exist?(zip_path), "expected temp zip to be removed even after an exception"
  end

  test "does not remove files outside the uploads dir" do
    zip_path = "/tmp/import_outside_uploads.zip"
    File.write(zip_path, "dummy")

    importer = Object.new
    importer.define_singleton_method(:import_data) { true }
    importer.define_singleton_method(:error_message) { nil }

    ImportZip.stub(:new, importer) do
      ImportFromZipJob.perform_now(zip_path)
    end

    assert File.exist?(zip_path), "expected file outside tmp/uploads to be kept"
  ensure
    FileUtils.rm_f(zip_path)
  end

  private

  def with_event_notifier(notifier)
    original_event = Rails.event
    Rails.define_singleton_method(:event) { notifier }
    yield
  ensure
    Rails.define_singleton_method(:event) { original_event }
  end
end
