# frozen_string_literal: true

require "test_helper"

class ExportTest < ActiveSupport::TestCase
  test "file_path points inside tmp/exports and strips any directory components" do
    export = Export.new(kind: "backup", filename: "backup.zip")
    assert_equal Rails.root.join("tmp", "exports", "backup.zip"), export.file_path

    export.filename = "../evil.zip"
    assert_equal Rails.root.join("tmp", "exports", "evil.zip"), export.file_path
  end

  test "file_available? requires a completed export with an existing file" do
    export = Export.new(kind: "backup", status: :pending)
    assert_not export.file_available?

    export.status = :completed
    export.filename = "definitely-missing.zip"
    assert_not export.file_available?
  end

  test "completed exports require a filename" do
    assert_not Export.new(kind: "backup", status: :completed).valid?
    assert Export.new(kind: "backup", status: :pending).valid?
  end

  test "fail_stale! fails pending/running exports older than the threshold" do
    stale_running = Export.create!(kind: "backup", status: :running)
    stale_running.update_column(:updated_at, (Export::STALE_AFTER + 1.minute).ago)
    stale_pending = Export.create!(kind: "site_export")
    stale_pending.update_column(:updated_at, (Export::STALE_AFTER + 1.minute).ago)
    fresh_running = Export.create!(kind: "backup", status: :running)
    completed = Export.create!(kind: "backup", status: :completed, filename: "done.zip")
    completed.update_column(:updated_at, (Export::STALE_AFTER + 1.minute).ago)

    assert_equal 2, Export.fail_stale!

    assert stale_running.reload.failed?
    assert_match "marked failed automatically", stale_running.error
    assert stale_pending.reload.failed?
    assert fresh_running.reload.running?
    assert completed.reload.completed?
  end
end
