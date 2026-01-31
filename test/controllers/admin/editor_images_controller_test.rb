# frozen_string_literal: true

require "test_helper"

class Admin::EditorImagesControllerTest < ActionDispatch::IntegrationTest
  def setup
    @user = users(:admin)
    sign_in(@user)
  end

  test "successfully uploads a valid image file" do
    image_file = fixture_file_upload("test_image.png", "image/png")

    assert_difference "ActiveStorage::Blob.count", 1 do
      post admin_editor_images_path, params: { file: image_file }
    end

    assert_response :success
    json_response = JSON.parse(response.body)
    assert json_response["location"].present?
    # URL is Active Storage blob URL
    assert_match %r{/rails/active_storage/}, json_response["location"]
  end

  test "returns error when file is missing" do
    post admin_editor_images_path

    assert_response :bad_request
    json_response = JSON.parse(response.body)
    assert_equal "No file provided", json_response["error"]
  end

  test "returns error for invalid file type" do
    text_file = fixture_file_upload("sample.txt", "text/plain")

    assert_no_difference "ActiveStorage::Blob.count" do
      post admin_editor_images_path, params: { file: text_file }
    end

    assert_response :bad_request
    json_response = JSON.parse(response.body)
    assert_match /Invalid file type/, json_response["error"]
  end

  test "accepts file based on declared content type, not extension" do
    # The controller validates content_type header, not file extension or content
    # A text file uploaded with image/png content-type will be accepted
    text_file_as_png = fixture_file_upload("sample.txt", "image/png")

    assert_difference "ActiveStorage::Blob.count", 1 do
      post admin_editor_images_path, params: { file: text_file_as_png }
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
