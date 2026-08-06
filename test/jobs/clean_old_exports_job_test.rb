# frozen_string_literal: true

require "test_helper"

class CleanOldExportsJobTest < ActiveJob::TestCase
  def setup
    @dirs = [ Dir.mktmpdir("exports"), Dir.mktmpdir("uploads") ]
  end

  def teardown
    @dirs.each { |dir| FileUtils.rm_rf(dir) }
  end

  test "removes files older than the given days and keeps recent ones" do
    old_file = write_file(@dirs.first, "old.zip")
    new_file = write_file(@dirs.first, "new.zip")
    FileUtils.touch(old_file, mtime: Time.now - 10.days)

    CleanOldExportsJob.perform_now(days: 7, dirs: @dirs)

    assert_not File.exist?(old_file)
    assert File.exist?(new_file)
  end

  test "removes stale working directories" do
    stale_dir = File.join(@dirs.first, "backup_20240101_abc")
    FileUtils.mkdir_p(stale_dir)
    File.write(File.join(stale_dir, "database.sqlite3"), "db")
    FileUtils.touch(stale_dir, mtime: Time.now - 10.days)

    CleanOldExportsJob.perform_now(days: 7, dirs: @dirs)

    assert_not Dir.exist?(stale_dir)
  end

  test "respects the custom days option" do
    file = write_file(@dirs.first, "recent.zip")
    FileUtils.touch(file, mtime: Time.now - 10.days)

    CleanOldExportsJob.perform_now(days: 14, dirs: @dirs)

    assert File.exist?(file)
  end

  test "handles string keys from SolidQueue" do
    file = write_file(@dirs.first, "old.zip")
    FileUtils.touch(file, mtime: Time.now - 10.days)

    CleanOldExportsJob.perform_now({ "days" => 7, "dirs" => @dirs })

    assert_not File.exist?(file)
  end

  test "logs a completed activity entry" do
    assert_difference "ActivityLog.count", 1 do
      CleanOldExportsJob.perform_now(days: 7, dirs: @dirs)
    end

    log = ActivityLog.last
    assert_equal "completed", log.action
    assert_equal "export_cleanup", log.target
    assert_equal "info", log.level
  end

  test "deletes activity logs older than 90 days" do
    old_log = ActivityLog.log!(action: :completed, target: :export_cleanup, message: "ancient")
    old_log.update_column(:created_at, 91.days.ago)

    CleanOldExportsJob.perform_now(days: 7, dirs: @dirs)

    assert_not ActivityLog.exists?(old_log.id)
  end

  test "keeps activity logs newer than 90 days" do
    recent_log = ActivityLog.log!(action: :completed, target: :export_cleanup, message: "recent")

    CleanOldExportsJob.perform_now(days: 7, dirs: @dirs)

    assert ActivityLog.exists?(recent_log.id)
  end

  test "reports the number of deleted activity logs" do
    2.times do |i|
      log = ActivityLog.log!(action: :completed, target: :export_cleanup, message: "ancient #{i}")
      log.update_column(:created_at, 100.days.ago)
    end

    notifier = RecordingNotifier.new
    original_event = Rails.event
    Rails.define_singleton_method(:event) { notifier }
    begin
      CleanOldExportsJob.perform_now(days: 7, dirs: @dirs)
    ensure
      Rails.define_singleton_method(:event) { original_event }
    end

    event = notifier.events.find { |name, _| name == "clean_old_exports_job.completed" }
    assert_not_nil event
    assert_equal 2, event[1][:activity_logs_deleted]
  end

  private

  def write_file(dir, name)
    path = File.join(dir, name)
    File.write(path, "content")
    path
  end
end
