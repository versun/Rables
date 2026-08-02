# frozen_string_literal: true

require "test_helper"

class Admin::EditorImagesControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    sign_in(@user)
  end

  test "rejects non-image files" do
    text_file = fixture_file_upload("sample.txt", "text/plain")

    assert_no_difference "ActiveStorage::Blob.count" do
      post admin_editor_images_path, params: { file: text_file }
    end

    assert_response :unsupported_media_type
    json_response = JSON.parse(response.body)
    assert_equal "Unsupported file type", json_response["error"]
  end

  test "returns error when file is missing" do
    post admin_editor_images_path

    assert_response :bad_request
    json_response = JSON.parse(response.body)
    assert_equal "No file provided", json_response["error"]
  end

  test "rejects SVG uploads" do
    svg_file = fixture_file_upload("sample.txt", "image/svg+xml")

    assert_no_difference "ActiveStorage::Blob.count" do
      post admin_editor_images_path, params: { file: svg_file }
    end

    assert_response :unsupported_media_type
  end

  test "continues to accept image files" do
    image_file = fixture_file_upload("test_image.png", "image/png")

    assert_difference "ActiveStorage::Blob.count", 1 do
      post admin_editor_images_path, params: { file: image_file }
    end

    assert_response :success
  end

  test "handles upload failure gracefully" do
    image_file = fixture_file_upload("test_image.png", "image/png")

    # Simulate an upload failure by stubbing ActiveStorage
    original_method = ActiveStorage::Blob.method(:create_and_upload!)
    ActiveStorage::Blob.define_singleton_method(:create_and_upload!) do |*_args|
      raise ActiveStorage::Error, "Storage service unavailable"
    end

    post admin_editor_images_path, params: { file: image_file }

    assert_response :internal_server_error
    json_response = JSON.parse(response.body)
    assert_match /Upload failed/, json_response["error"]
  ensure
    ActiveStorage::Blob.define_singleton_method(:create_and_upload!, original_method)
  end

  test "accepts webp images" do
    webp_file = fixture_file_upload("test_image.webp", "image/webp")

    assert_difference "ActiveStorage::Blob.count", 1 do
      post admin_editor_images_path, params: { file: webp_file }
    end

    assert_response :success
  end

  test "accepts gif images" do
    gif_file = fixture_file_upload("test_image.gif", "image/gif")

    assert_difference "ActiveStorage::Blob.count", 1 do
      post admin_editor_images_path, params: { file: gif_file }
    end

    assert_response :success
  end

  test "accepts jpeg images" do
    jpeg_file = fixture_file_upload("test_image.jpg", "image/jpeg")

    assert_difference "ActiveStorage::Blob.count", 1 do
      post admin_editor_images_path, params: { file: jpeg_file }
    end

    assert_response :success
  end
end
