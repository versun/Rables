# frozen_string_literal: true

require "test_helper"
require "zip"

class Admin::ExportsControllerTest < ActionDispatch::IntegrationTest
  setup do
    @user = users(:admin)
    @exports_dir = Rails.root.join("tmp", "exports")
    FileUtils.mkdir_p(@exports_dir)
  end

  teardown do
    FileUtils.rm_f(@exports_dir.join("test_artifact.zip"))
  end

  test "download requires authentication" do
    export = Export.create!(kind: "backup", status: :completed, filename: "test_artifact.zip")

    get download_admin_export_path(export)

    assert_redirected_to new_session_path
  end

  test "download serves the artifact" do
    sign_in(@user)
    Zip::File.open(@exports_dir.join("test_artifact.zip"), create: true) do |zipfile|
      zipfile.get_output_stream("a.txt") { |f| f.write "x" }
    end
    export = Export.create!(kind: "backup", status: :completed, filename: "test_artifact.zip", byte_size: 100)

    get download_admin_export_path(export)

    assert_response :success
    assert_equal "application/zip", response.media_type
    assert_match "attachment", response.headers["Content-Disposition"]
    assert_match "test_artifact.zip", response.headers["Content-Disposition"]
  end

  test "download redirects with an alert when the file is not available" do
    sign_in(@user)
    export = Export.create!(kind: "backup", status: :failed, error: "boom")

    get download_admin_export_path(export)

    assert_redirected_to admin_migrates_path(tab: "export")
    assert_match "not available", flash[:alert]
  end
end
