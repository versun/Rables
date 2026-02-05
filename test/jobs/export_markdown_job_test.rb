# frozen_string_literal: true

require "test_helper"

class ExportMarkdownJobTest < ActiveJob::TestCase
  class RecordingNotifier
    attr_reader :events

    def initialize
      @events = []
    end

    def notify(name, **payload)
      @events << [ name, payload ]
    end
  end

  test "logs completion when export succeeds" do
    exporter = Object.new
    exporter.define_singleton_method(:generate) { true }
    exporter.define_singleton_method(:zip_path) { "/tmp/markdown.zip" }
    exporter.define_singleton_method(:error_message) { nil }

    notifier = RecordingNotifier.new

    MarkdownExport.stub(:new, exporter) do
      assert_difference "ActivityLog.count", 1 do
        with_event_notifier(notifier) { ExportMarkdownJob.perform_now }
      end
    end

    assert notifier.events.any? { |name, payload| name == "export_markdown_job.completed" && payload[:download_url] == "/tmp/markdown.zip" }
  end

  test "logs failure when export fails" do
    exporter = Object.new
    exporter.define_singleton_method(:generate) { false }
    exporter.define_singleton_method(:error_message) { "boom" }

    notifier = RecordingNotifier.new

    MarkdownExport.stub(:new, exporter) do
      assert_difference "ActivityLog.count", 1 do
        with_event_notifier(notifier) { ExportMarkdownJob.perform_now }
      end
    end

    assert notifier.events.any? { |name, payload| name == "export_markdown_job.failed" && payload[:error_message] == "boom" }
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
