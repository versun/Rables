# frozen_string_literal: true

require "test_helper"

class Admin::TwitterArchiveDirectUploadsControllerTest < ActionDispatch::IntegrationTest
  def teardown
    ActiveStorage::Blob.unattached.find_each(&:purge)
  end

  test "create returns signed id and direct upload details for valid blob params" do
    sign_in(users(:admin))

    content = "twitter archive content"
    post admin_twitter_archive_direct_uploads_path, params: {
      blob: {
        filename: "twitter-archive.zip",
        byte_size: content.bytesize,
        checksum: Digest::MD5.base64digest(content),
        content_type: "application/zip"
      }
    }

    assert_response :success

    json = response.parsed_body
    blob = ActiveStorage::Blob.order(:created_at).last
    assert_equal "twitter-archive.zip", json["filename"]
    assert_equal TwitterArchiveImportSubmission.direct_upload_token_for(blob), json["signed_id"]
    assert json["direct_upload"]["url"].present?
    assert json["direct_upload"]["headers"].present?
  end

  test "create returns bad request when blob params are missing" do
    sign_in(users(:admin))

    assert_no_difference("ActiveStorage::Blob.count") do
      post admin_twitter_archive_direct_uploads_path, params: {}
    end

    assert_response :bad_request
  end

  test "create redirects to sign in when not authenticated" do
    content = "twitter archive content"

    assert_no_difference("ActiveStorage::Blob.count") do
      post admin_twitter_archive_direct_uploads_path, params: {
        blob: {
          filename: "twitter-archive.zip",
          byte_size: content.bytesize,
          checksum: Digest::MD5.base64digest(content),
          content_type: "application/zip"
        }
      }
    end

    assert_redirected_to new_session_path
  end
end
