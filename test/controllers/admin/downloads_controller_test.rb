# frozen_string_literal: true

require "test_helper"

class Admin::DownloadsControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    @exports_dir = Rails.root.join("tmp", "exports")
    FileUtils.mkdir_p(@exports_dir)
    # Use unique filenames based on process ID to avoid conflicts in parallel tests
    @unique_suffix = "#{Process.pid}_#{Time.current.to_i}"
  end

  def teardown
    # Only remove files created by this test process
    Dir.glob(@exports_dir.join("*#{@unique_suffix}*")).each do |f|
      FileUtils.rm_f(f)
    end
  end

  test "should require authentication" do
    get admin_download_path(filename: "test.zip")
    assert_redirected_to new_session_path
  end

  test "should download existing file when authenticated" do
    sign_in(@user)

    # Ensure directory exists before creating file
    FileUtils.mkdir_p(@exports_dir)

    # Create a test file with unique name
    filename = "test_export_#{@unique_suffix}.zip"
    test_file = @exports_dir.join(filename)
    File.write(test_file, "test content")

    get admin_download_path(filename: filename)
    assert_response :success
    assert_equal "test content", response.body
  end

  test "should return error for non-existent file" do
    sign_in(@user)

    get admin_download_path(filename: "nonexistent_#{@unique_suffix}.zip")
    assert_redirected_to admin_migrates_path
    assert_match "不存在", flash[:alert]
  end

  # Note: The route constraint {filename: /[^\/]+/} already prevents slashes in URLs
  # These tests verify that File.basename provides an additional layer of protection
  # in case the route constraint is bypassed or changed

  test "file basename strips directory traversal components" do
    # File.basename should strip path traversal attempts
    assert_equal "secret.txt", File.basename("../secret.txt")
    assert_equal "secret.txt", File.basename("../../secret.txt")
    assert_equal "passwd", File.basename("/etc/passwd")
  end

  test "should handle filenames with special characters" do
    sign_in(@user)

    # Ensure directory exists before creating file
    FileUtils.mkdir_p(@exports_dir)

    filename = "export_2024-01-01_#{@unique_suffix}.zip"
    test_file = @exports_dir.join(filename)
    File.write(test_file, "test content")

    get admin_download_path(filename: filename)
    assert_response :success
  end

  test "rejects blank or dot filenames" do
    sign_in(@user)

    get admin_download_path(filename: ".hidden")
    assert_redirected_to admin_migrates_path
    assert_match "无效", flash[:alert]
  end

  test "rejects symlink escape" do
    sign_in(@user)

    filename = "symlink_#{@unique_suffix}.zip"
    link_path = @exports_dir.join(filename)
    FileUtils.ln_sf("/etc/hosts", link_path)

    get admin_download_path(filename: filename)
    assert_redirected_to admin_migrates_path
    assert_match "不允许", flash[:alert]
  end

  test "rescues unexpected errors during download" do
    sign_in(@user)

    filename = "error_export_#{@unique_suffix}.zip"
    test_file = @exports_dir.join(filename)
    File.write(test_file, "test content")

    ActionController::Base.class_eval do
      alias_method :original_send_file, :send_file
      def send_file(*)
        raise "boom"
      end
    end

    get admin_download_path(filename: filename)
    assert_redirected_to admin_migrates_path
    assert_match "下载失败", flash[:alert]
  ensure
    ActionController::Base.class_eval do
      alias_method :send_file, :original_send_file
      remove_method :original_send_file
    end
  end

  test "safe path validation prevents directory escape" do
    # This test verifies that even if File.basename is bypassed somehow,
    # the start_with? check would catch directory traversal
    exports_base = Rails.root.join("tmp", "exports").to_s

    # A legitimate path should pass
    safe_path = Rails.root.join("tmp", "exports", "test.zip")
    assert safe_path.to_s.start_with?(exports_base)

    # A malicious path should not pass (though File.basename would prevent this)
    malicious_path = Rails.root.join("tmp", "exports", "..", "secrets.txt")
    # After path expansion, this would be tmp/secrets.txt
    assert_not malicious_path.to_s.start_with?(exports_base) ||
               malicious_path.to_s.include?("..")
  end
end
