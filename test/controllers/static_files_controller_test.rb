# frozen_string_literal: true

require "test_helper"

class StaticFilesControllerTest < ActionDispatch::IntegrationTest
  test "show redirects to blob when file exists and 404s otherwise" do
    uploaded = fixture_file_upload("sample.txt", "text/plain")
    static_file = StaticFile.create!(
      file: uploaded,
      filename: "sample.txt",
      description: "Sample"
    )

    get static_file_path(filename: static_file.filename)
    assert_response :redirect
    assert_includes response.headers["Location"], "/rails/active_storage"

    get static_file_path(filename: "missing.txt")
    assert_response :not_found
  end
end
