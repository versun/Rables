# frozen_string_literal: true

require "test_helper"
require "minitest/mock"

class Admin::MigratesControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    @uploads_dir = Rails.root.join("tmp", "uploads")
    FileUtils.mkdir_p(@uploads_dir)
  end

  def teardown
    FileUtils.rm_rf(@uploads_dir)
  end

  test "should require authentication for index" do
    get admin_migrates_path
    assert_redirected_to new_session_path
  end

  test "should show index when authenticated" do
    sign_in(@user)
    get admin_migrates_path
    assert_response :success
    assert_select ".status-tab.active", text: "Export"

    get admin_migrates_path(tab: "import")
    assert_response :success
    assert_select ".status-tab.active", text: "Import"
  end

  test "should require authentication for create" do
    post admin_migrates_path, params: { operation_type: "export" }
    assert_redirected_to new_session_path
  end

  test "should enqueue a backup export" do
    sign_in(@user)

    assert_difference -> { Export.count }, 1 do
      assert_enqueued_with(job: ExportJob) do
        post admin_migrates_path, params: { operation_type: "export" }
      end
    end

    export = Export.last
    assert_equal "backup", export.kind
    assert export.pending?
    assert_redirected_to admin_migrates_path(tab: "export")
    assert_match "background", flash[:notice]
  end

  test "should enqueue a go migration export" do
    sign_in(@user)

    assert_enqueued_with(job: ExportJob) do
      post admin_migrates_path, params: { operation_type: "site_export" }
    end

    assert_equal "site_export", Export.last.kind
    assert_redirected_to admin_migrates_path(tab: "export")
  end

  test "export tab lists recent exports" do
    sign_in(@user)
    Export.create!(kind: "backup", status: :completed, filename: "done.zip", byte_size: 1024)

    get admin_migrates_path(tab: "export")

    assert_response :success
    assert_select "td", text: "Full Backup"
    assert_select "td", text: /completed/
  end

  test "export tab fails stuck pending/running exports on visit" do
    sign_in(@user)
    stuck = Export.create!(kind: "backup", status: :running)
    stuck.update_column(:updated_at, (Export::STALE_AFTER + 1.minute).ago)
    fresh = Export.create!(kind: "backup", status: :running)

    get admin_migrates_path(tab: "export")

    assert_response :success
    assert stuck.reload.failed?
    assert fresh.reload.running?
  end

  test "should restore from a backup file" do
    sign_in(@user)
    file = fixture_file_upload(create_temp_zip_file, "application/zip")
    captured_path = nil
    fake_import = ->(path, **_kwargs) { captured_path = path; true }

    SiteBackup.stub(:import, fake_import) do
      post admin_migrates_path, params: {
        operation_type: "import",
        backup_file: file
      }
    end

    assert_redirected_to admin_migrates_path(tab: "import")
    assert_match "restored", flash[:notice]
    assert_equal @uploads_dir.to_s, File.dirname(captured_path.to_s)
    assert_match(/\.zip\z/, captured_path.to_s)
  end

  test "backup import failure shows an alert" do
    sign_in(@user)
    file = fixture_file_upload(create_temp_zip_file, "application/zip")
    failing_import = ->(*_args) { raise SiteBackup::Error, "corrupt backup" }

    SiteBackup.stub(:import, failing_import) do
      post admin_migrates_path, params: {
        operation_type: "import",
        backup_file: file
      }
    end

    assert_redirected_to admin_migrates_path(tab: "import")
    assert_match "corrupt backup", flash[:alert]
  end

  test "backup import unexpected error shows an alert and notifies" do
    sign_in(@user)
    file = fixture_file_upload(create_temp_zip_file, "application/zip")
    failing_import = ->(*_args) { raise Errno::EIO, "disk exploded" }

    notifier = RecordingNotifier.new
    original_event = Rails.event
    Rails.define_singleton_method(:event) { notifier }
    begin
      SiteBackup.stub(:import, failing_import) do
        post admin_migrates_path, params: {
          operation_type: "import",
          backup_file: file
        }
      end
    ensure
      Rails.define_singleton_method(:event) { original_event }
    end

    assert_redirected_to admin_migrates_path(tab: "import")
    assert_match "disk exploded", flash[:alert]
    event = notifier.events.find { |name, _| name == "admin.migrates_controller.backup_import_error" }
    assert_not_nil event
    assert_match "disk exploded", event[1][:message]
  end

  test "should reject non-zip backup files" do
    sign_in(@user)
    file = fixture_file_upload(
      create_temp_file("test.txt", "test content"),
      "text/plain"
    )

    post admin_migrates_path, params: {
      operation_type: "import",
      backup_file: file
    }

    assert_redirected_to admin_migrates_path(tab: "import")
    assert_match "ZIP", flash[:alert]
  end

  test "should handle RSS import with URL" do
    sign_in(@user)

    assert_enqueued_with(job: ImportFromRssJob, args: [ "https://example.com/feed.xml", nil ]) do
      post admin_migrates_path, params: {
        operation_type: "import",
        url: "https://example.com/feed.xml"
      }
    end

    assert_redirected_to admin_migrates_path(tab: "import")
    assert_match "RSS Import in progress", flash[:notice]
  end

  test "should reject unsupported operation type" do
    sign_in(@user)

    post admin_migrates_path, params: { operation_type: "invalid" }

    assert_redirected_to admin_migrates_path(tab: "export")
    assert_match "Unsupported operation type", flash[:alert]
  end

  test "should reject import without URL or backup file" do
    sign_in(@user)

    post admin_migrates_path, params: { operation_type: "import" }

    assert_redirected_to admin_migrates_path(tab: "import")
    assert_match "RSS URL or backup file", flash[:alert]
  end

  private

  def create_temp_file(filename, content)
    temp_path = @uploads_dir.join(filename)
    File.write(temp_path, content)
    temp_path
  end

  def create_temp_zip_file
    require "zip"
    temp_path = @uploads_dir.join("test_backup.zip")

    Zip::File.open(temp_path, create: true) do |zipfile|
      zipfile.get_output_stream("test.txt") { |f| f.write "test content" }
    end

    temp_path
  end
end
