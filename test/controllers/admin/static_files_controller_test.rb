# frozen_string_literal: true

require "test_helper"

class Admin::StaticFilesControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    sign_in(@user)
  end

  test "index create update and destroy static files" do
    get admin_static_files_path
    assert_response :success

    post admin_static_files_path, params: { static_file: {} }
    assert_response :success

    uploaded = fixture_file_upload("sample.txt", "text/plain")
    assert_difference "StaticFile.count", 1 do
      post admin_static_files_path, params: {
        static_file: { file: uploaded, description: "Sample file" }
      }
    end
    assert_redirected_to admin_static_files_path

    static_file = StaticFile.last

    updated_upload = fixture_file_upload("sample.txt", "text/plain")
    assert_no_difference "StaticFile.count" do
      post admin_static_files_path, params: {
        static_file: { file: updated_upload, description: "Updated description" }
      }
    end
    assert_redirected_to admin_static_files_path
    assert_equal "Updated description", static_file.reload.description

    assert_difference "StaticFile.count", -1 do
      delete admin_static_file_path(static_file)
    end
    assert_redirected_to admin_static_files_path
  end

  test "renders index when update fails for existing file" do
    uploaded = fixture_file_upload("sample.txt", "text/plain")
    static_file = StaticFile.create!(
      file: uploaded,
      filename: "sample.txt",
      description: "Sample"
    )

    original_find_by = StaticFile.method(:find_by)
    original_update = static_file.method(:update)
    StaticFile.define_singleton_method(:find_by) { |_args| static_file }
    static_file.define_singleton_method(:update) { |_params| false }

    post admin_static_files_path, params: {
      static_file: { file: uploaded, description: "Updated" }
    }
  ensure
    StaticFile.define_singleton_method(:find_by, original_find_by)
    static_file.define_singleton_method(:update, original_update)

    assert_response :success
    assert_match "文件上传失败", response.body
  end

  test "renders index when new upload fails" do
    uploaded = fixture_file_upload("sample.txt", "text/plain")
    failed = StaticFile.new(file: uploaded, filename: "sample.txt", description: "Fail")
    failed.define_singleton_method(:save) { false }

    original_new = StaticFile.method(:new)
    StaticFile.define_singleton_method(:new) { |*_args| failed }

    post admin_static_files_path, params: {
      static_file: { file: uploaded, description: "Fail" }
    }

    assert_response :success
    assert_match "文件上传失败", response.body
  ensure
    StaticFile.define_singleton_method(:new, original_new)
  end
end
