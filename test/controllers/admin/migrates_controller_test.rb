# frozen_string_literal: true

require "test_helper"

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

  test "should handle export operation" do
    sign_in(@user)

    assert_enqueued_with(job: ExportDataJob) do
      post admin_migrates_path, params: { operation_type: "export", export_type: "default" }
    end

    assert_redirected_to admin_migrates_path(tab: "export")
    assert_match "Export Initiated", flash[:notice]
  end

  test "should handle markdown export operation" do
    sign_in(@user)

    assert_enqueued_with(job: ExportMarkdownJob) do
      post admin_migrates_path, params: { operation_type: "export", export_type: "markdown" }
    end

    assert_redirected_to admin_migrates_path(tab: "export")
    assert_match "Markdown Export Initiated", flash[:notice]
  end

  test "should reject non-zip files" do
    sign_in(@user)

    # Create a mock uploaded file with wrong content type
    file = fixture_file_upload(
      create_temp_file("test.txt", "test content"),
      "text/plain"
    )

    post admin_migrates_path, params: {
      operation_type: "import",
      zip_file: file
    }

    assert_redirected_to admin_migrates_path(tab: "import")
    assert_match "ZIP", flash[:alert]
  end

  test "should accept zip files for import" do
    sign_in(@user)

    # Create a minimal valid zip file
    temp_zip = create_temp_zip_file

    file = fixture_file_upload(temp_zip, "application/zip")

    assert_enqueued_with(job: ImportFromZipJob) do
      post admin_migrates_path, params: {
        operation_type: "import",
        zip_file: file
      }
    end

    assert_redirected_to admin_migrates_path(tab: "import")
    assert_match "ZIP Import in progress", flash[:notice]
  end

  test "file basename sanitizes filename for import" do
    # File.basename should strip path traversal attempts with forward slashes
    assert_equal "malicious.zip", File.basename("../../../malicious.zip")
    assert_equal "malicious.zip", File.basename("/etc/malicious.zip")
    # Note: File.basename behavior with backslashes is platform-dependent
    # On Unix, backslashes are valid filename characters
    # The controller code uses forward slash path separators
  end

  test "should handle RSS import with URL" do
    sign_in(@user)

    assert_enqueued_with(job: ImportFromRssJob) do
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

  private

  def create_temp_file(filename, content)
    temp_path = Rails.root.join("tmp", filename)
    File.write(temp_path, content)
    temp_path
  end

  def create_temp_zip_file
    require "zip"
    temp_path = Rails.root.join("tmp", "test_import.zip")

    Zip::File.open(temp_path, create: true) do |zipfile|
      zipfile.get_output_stream("test.txt") { |f| f.write "test content" }
    end

    temp_path
  end
end
